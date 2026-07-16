package agent

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/workflow"
)

// isEmptyTurn reports whether the last assistant message in conv has no text
// content and no tool calls — an empty completion that typically indicates the
// model hit a token limit rather than producing a deliberate empty reply.
func IsEmptyTurn(conv []proxy.Message) bool {
	for i := len(conv) - 1; i >= 0; i-- {
		if conv[i].Role == "assistant" {
			return strings.TrimSpace(DerefStr(conv[i].Content)) == "" &&
				len(conv[i].ToolCalls) == 0
		}
	}
	return false
}

// handleEmptyResponse detects an empty-completion turn and, in IMPLEMENT phases,
// retries exactly once with a directive noting the truncation. A second empty
// response surfaces the condition to the user without touching workflow state
// (no phase or step transition fires from an empty response).
func HandleEmptyResponse(ctx context.Context, app *App) {
	if !IsEmptyTurn(app.Conv) {
		return
	}
	wfProgNote(app, "⚠ empty response (likely token-limit truncation)")

	if app.Workflow == nil || app.Workflow.Phase != workflow.WFImplement {
		return
	}

	// Single automatic retry: reset the step-evidence trace so it reflects
	// the retry turn, not the empty one.
	app.WorkflowStepTrace = nil
	const retryHint = "The previous response was empty — likely hit the token limit. " +
		"Please resume and complete the current implementation step."
	_, err := app.Send(ctx, retryHint)
	if err != nil {
		wfProgNote(app, "⚠ retry failed: "+err.Error())
	}

	if IsEmptyTurn(app.Conv) {
		// Retry also empty — surface without advancing state.
		wfProgNote(app, "⚠ retry also returned empty — "+
			"check token budget; workflow state unchanged")
	}
}

// streamRetryHint is sent as the user turn on each automatic retry after a
// backend error, so the model can resume the interrupted work.
const streamRetryHint = "The previous response was interrupted by a backend error. " +
	"Please resume and complete the current task."

// HandleStreamError handles errors from app.Send with retry logic for transient
// backend failures.
//
// Classification:
//   - nil / non-backend error → returned unchanged.
//   - ErrBackendFatal (4xx) → returned immediately, never retried.
//   - ErrBackendStream (5xx, reset, timeout) → retried in unattended runs
//     (AutoApprove or IsHeadless); passed through immediately in interactive
//     non-auto sessions so the human can decide to re-send.
//
// Retry loop: up to cfg.BackendMaxRetries attempts with exponential backoff
// (1s/2s/4s base + jitter). Each attempt is logged. When all retries fail with
// connection-reset symptoms, a "possibly deterministic" note is added — this
// distinguishes a transient infrastructure outage from a request that can never
// succeed (e.g. context-overflow resetting the connection every time).
func HandleStreamError(ctx context.Context, app *App, err error) error {
	if err == nil {
		return nil
	}
	// Fatal (4xx): bad request, auth — retrying is pointless.
	if errors.Is(err, proxy.ErrBackendFatal) {
		return err
	}
	// Non-stream errors pass through unchanged.
	if !errors.Is(err, proxy.ErrBackendStream) {
		return err
	}
	// Interactive non-auto: surface immediately; a human is present to re-send.
	if !app.AutoApprove && !app.IsHeadless {
		return err
	}

	maxRetries := app.Cfg.BackendMaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}

	allStreamErrors := true // false if any retry produces a non-stream error
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		suffix := ""
		if app.NearContextLimit() {
			suffix = " (near context limit)"
		}
		backendNote(app, fmt.Sprintf("⚠ backend error%s — retry %d/%d", suffix, attempt, maxRetries))

		delay := retryBackoff(attempt-1, app.RetryDelay)
		if delay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		app.WorkflowStepTrace = nil
		_, rerr := app.Send(ctx, streamRetryHint)
		if rerr == nil {
			return nil // recovered
		}
		if !errors.Is(rerr, proxy.ErrBackendStream) {
			allStreamErrors = false
		}
		lastErr = rerr
	}

	// All retries exhausted — annotate based on failure pattern.
	if allStreamErrors && app.NearContextLimit() {
		backendNote(app, "⚠ persistent stream errors near context limit — possible request-size issue; try /compact")
	} else if allStreamErrors {
		backendNote(app, "⚠ persistent stream errors — possible deterministic backend failure (context overflow?)")
	} else {
		backendNote(app, fmt.Sprintf("⚠ backend unreachable after %d retries", maxRetries))
	}
	return lastErr
}

// retryBackoff returns the wait duration before retry attempt n (0-based).
// The override function (App.RetryDelay) is used when set (tests); otherwise
// the standard 1s·2^n + jitter schedule.
func retryBackoff(n int, override func(int) time.Duration) time.Duration {
	if override != nil {
		return override(n)
	}
	base := time.Duration(1<<uint(n)) * time.Second
	jitter := time.Duration(rand.Int63n(int64(base / 2)))
	return base + jitter
}

// backendNote logs a backend-resilience message to the appropriate sink.
// In a workflow it uses the workflow progress channel; otherwise it writes to
// app.Out so headless and free-chat sessions see the line.
func backendNote(app *App, text string) {
	if app.Workflow != nil {
		wfProgNote(app, text)
		return
	}
	fmt.Fprintln(app.Out, text)
}
