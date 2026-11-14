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
//     (AutoApprove or IsHeadless), and also in interactive sessions (Group 4):
//     each attempt prints one line naming the reason and the wait, and the
//     exhausted message names the last status, the attempts, and one next
//     step.
//
// Retry loop: up to cfg.BackendMaxRetries attempts with exponential backoff
// (1s/2s/4s base + jitter) — rate-limited and overloaded failures wait longer
// (15 s default, Retry-After when present, never past 60 s). A configuration
// that explicitly sets BackendMaxRetries to 0 means "no retries" and the
// error surfaces after the one failed attempt.
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
	// Retries run in interactive sessions too (previously the error was
	// surfaced immediately there with no attempt at all, which the Group 4
	// change removes): each attempt prints one line naming the reason and
	// the wait, and the exhausted message names the last status, the
	// attempts, and one next step.

	maxRetries := app.Cfg.BackendMaxRetries
	// The field's contract: 0 disables retrying (DefaultConfig sets 3; a
	// nil/negative means unset and defaults to 3).
	if maxRetries < 0 {
		maxRetries = 3
	}
	if maxRetries == 0 {
		return err
	}

	allStreamErrors := true // false if any retry produces a non-stream error
	// The initial error is the first failure being amortised over: the
	// first attempt's line and its wait are computed from it (a 429 with no
	// Retry-After still waits the 15 s minimum), not from a nil slot.
	lastErr := err
	for attempt := 1; attempt <= maxRetries; attempt++ {
		suffix := ""
		if app.NearContextLimit() {
			suffix = " (near context limit)"
		}
		// Per-attempt line naming the reason and the wait: rate-limited and
		// overloaded (429/529) back off longer than other transient errors
		// and honour Retry-After (see retryWait).
		delay := retryWait(attempt, lastErr, app)
		backendNote(app, fmt.Sprintf("⚠ %s — retrying in %s, attempt %d of %d%s", retryReason(lastErr), delay, attempt, maxRetries, suffix))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}

		app.WorkflowStepTrace = nil
		_, rerr := app.Send(ctx, streamRetryHint)
		if rerr == nil {
			return nil // recovered
		}
		if !errors.Is(rerr, proxy.ErrBackendStream) {
			// Fatal (4xx) or non-stream error reached during a retry: stop
			// looping (e.g. a 500 followed by a 400) and surface it; the
			// exhausted message below still names the last status.
			allStreamErrors = false
			lastErr = rerr
			break
		}
		lastErr = rerr
	}

	// All retries exhausted — annotate based on failure pattern. The final
	// message names the last status, the attempt count and one next step.
	status := "unknown status"
	if lastErr != nil {
		status = lastErr.Error()
	}
	if len(status) > 80 {
		status = status[:80]
	}
	if allStreamErrors && app.NearContextLimit() {
		backendNote(app, fmt.Sprintf("⚠ gave up after %d attempts — last status: %s. Next step: /compact (near the context limit)", maxRetries, status))
	} else if allStreamErrors {
		backendNote(app, fmt.Sprintf("⚠ gave up after %d attempts — persistent: last status: %s. Next step: wait a moment, then resend", maxRetries, status))
	} else {
		backendNote(app, fmt.Sprintf("⚠ backend unreachable after %d attempts — last status: %s. Next step: check the backend route, then resend", maxRetries, status))
	}
	return lastErr
}

// DriveTurnWithResilience runs one user turn to its FINAL outcome with the
// agent's resilience policies applied, and returns the authoritative
// TurnOutcome. It is the single entry point for callers that need the final
// assistant text after retry/empty-recovery — the host adapter (chunk
// 7) and, later, the TUI re-route.
//
// Sequence (mirrors the headless driver's driveHeadlessTurn + recovery, but
// returns the final text instead of dropping it):
//  1. send + suspend/wait/resume to Final (runTurnToFinal);
//  2. on a transient stream error, retry via HandleStreamError — which re-sends
//     a fresh user turn internally and returns only error, so the recovered
//     turn's text is read back from the conversation below;
//  3. on success, apply HandleEmptyResponse (empty-completion recovery).
//
// It deliberately does NOT install Out/Confirm/OnReasoning/OnTokRate callbacks
// and does NOT emit AgentDoneMsg — the caller owns the output sink and the TUI
// owns done signalling. Fatal (non-retryable) errors are returned for the caller
// to classify (the host adapter distinguishes backend_failure from
// request_error).
func DriveTurnWithResilience(ctx context.Context, app *App, userText string) (TurnOutcome, error) {
	out, err := runTurnToFinal(ctx, app, userText)
	if err != nil {
		if err = HandleStreamError(ctx, app, err); err != nil {
			if out.Kind == TurnFinal || out.Kind == TurnSuspended || out.Cause == StopCauseNone {
				out = abortedTurnOutcome(ctx, err)
			}
			return out, err
		}
		// Recovery succeeded. The next assistant text is authoritative, but the
		// outcome is only clean if recovery itself did not record an abnormal cause.
		out = TurnOutcome{Kind: TurnFinal, Text: workflow.LastAssistantText(app.Conv), Cause: currentTurnStopCause(app)}
		out = turnOutcome(out.Text, false, out.Cause)
	}
	// Runs after a clean send AND after a recovered retry (matches the legacy
	// runSingleTaskHeadless, which applied HandleEmptyResponse on both paths).
	HandleEmptyResponse(ctx, app)
	// The last assistant message is authoritative after any retry or recovery:
	// HandleStreamError's retry (app.Send(streamRetryHint)) and
	// HandleEmptyResponse's recovery (app.Send(retryHint)) each append a fresh
	// assistant message, so the pre-recovery TurnOutcome would be stale.
	app.convMu.RLock()
	text := workflow.LastAssistantText(app.Conv)
	app.convMu.RUnlock()
	out = turnOutcome(text, false, currentTurnStopCause(app))
	return out, nil
}

// RetryAfterCap is the ceiling honoured for a Retry-After header: the
// header is never waited out beyond 60 seconds.
const RetryAfterCap = 60 * time.Second

// RateLimitDefaultWait is the default wait before a retry when the failure
// was rate limiting or overload (429/529) and no Retry-After applies —
// deliberately longer than the plain exponential backoff for other errors.
const RateLimitDefaultWait = 15 * time.Second

// retryWait returns the pause before retry attempt n. A Retry-After header
// the proxy saw is honoured up to RetryAfterCap (60 s) whenever it is
// present; 429/529 failures wait at least RateLimitDefaultWait (15 s);
// everything else uses the exponential backoff schedule. An explicit
// RetryDelay override (tests) still shortens the wait — noDelay(n) 0s
// beats the 429 minimum so test-time retries never block on a real second.
func retryWait(n int, lastErr error, app *App) time.Duration {
	if d := app.Client.RetryAfter(); d > 0 {
		if d > RetryAfterCap {
			return RetryAfterCap
		}
		return d
	}
	if IsRateLimitOrOverload(lastErr) {
		if app.RetryDelay != nil {
			return retryBackoff(n-1, app.RetryDelay)
		}
		if d := retryBackoff(n-1, app.RetryDelay); d > RateLimitDefaultWait {
			return d
		}
		return RateLimitDefaultWait
	}
	return retryBackoff(n-1, app.RetryDelay)
}

// retryReason names the failure reason for the per-attempt line, from the
// status carried by the error. Plain prose a stranger can understand.
func retryReason(err error) string {
	if err == nil {
		return "backend error"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "429") || strings.Contains(msg, "Too Many Requests"):
		return "rate limited"
	case strings.Contains(msg, "529") || strings.Contains(msg, "overloaded"):
		return "provider overloaded"
	case strings.Contains(msg, "503"):
		return "provider unavailable"
	default:
		return "backend error"
	}
}

// IsRateLimitOrOverload reports whether err carries a 429 (rate limited) or
// 529 (provider overloaded) status.
func IsRateLimitOrOverload(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "429") || strings.Contains(msg, "Too Many Requests") || strings.Contains(msg, "529") || strings.Contains(msg, "overloaded")
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
