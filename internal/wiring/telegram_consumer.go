package wiring

// telegram_consumer.go: the Telegram approval consumer for the TUI (async)
// approval path.
//
// When the `telegram-bridge` MCP server is configured, this consumer runs
// alongside the TUI event pump. It subscribes to the session's event stream
// (a second subscription, independent of the TUI's), watches for
// ApprovalRequested events, and for each one calls the
// `telegram-bridge__send_confirmation` MCP tool. The MCP tool sends a Telegram
// message with inline buttons and blocks until the user taps a button (or
// times out). When the tool returns, the consumer calls
// facade.RespondToApproval — the same method the TUI's keypress handler uses.
//
// This is the "first response wins" pattern: both the TUI keypress and the
// Telegram button press call RespondToApproval with the same ApprovalID.
// RespondToApproval is idempotent for matching outcomes, and the TUI's
// KindApprovalResolved handler clears the prompt when the matching approval
// is resolved. No Confirmer wrapping, no agent changes, no TUI changes.
//
// The consumer runs in its own goroutine. Each ApprovalRequested event is
// handled in a SEPARATE goroutine so the subscription reader is never blocked
// by a pending Telegram call — the agent loop is sequential within a turn
// (one approval at a time), but a late Telegram response for an
// already-TUI-answered approval must not prevent the consumer from seeing
// the next ApprovalRequested. A cancel context (per-consumer) stops the
// consumer goroutine and all in-flight handlers on Close or session rotation.
//
// Diagnostics go to the session's ephemeral note stream (note), NOT to
// os.Stderr: in TUI mode stderr is the live alt-screen terminal, so writing
// to it corrupts Bubble Tea's frame until a resize.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/diag"
)

// telegramBridgeServerName is the MCP server name we look for to activate
// the Telegram approval consumer. The MCP tool is namespaced as
// "{server}__send_confirmation".
const telegramBridgeServerName = "telegram-bridge"

// telegramToolName is the MCP tool name exposed by the telegram-bridge server.
const telegramToolName = "send_confirmation"

// namespacedTelegramTool is the full MCP tool name the consumer calls.
const namespacedTelegramTool = telegramBridgeServerName + "__" + telegramToolName

// consumerStopTimeout is the maximum time Stop() waits for the consumer
// goroutine and in-flight handlers to exit. The MCP call (Telegram
// long-polling) should unblock promptly when its context is cancelled;
// this timeout is a safety bound so a hung transport cannot block Close
// or rotation indefinitely.
const consumerStopTimeout = 15 * time.Second

// detectTelegramBridge checks whether the telegram-bridge MCP server is
// configured and connected. Returns true if the MCP manager has a connected
// server named "telegram-bridge".
func detectTelegramBridge(mcp *agent.MCPManager) bool {
	if mcp == nil {
		return false
	}
	for _, srv := range mcp.Servers() {
		if srv.Cfg.Name == telegramBridgeServerName && srv.Status == "connected" {
			return true
		}
	}
	return false
}

// telegramApprovalConsumer subscribes to the session event stream and forwards
// ApprovalRequested events to the Telegram bridge MCP tool. When the tool
// returns (user tapped a button or timed out), it calls RespondToApproval.
//
// Lifecycle: Start launches a goroutine that runs until ctx is cancelled
// (via Stop). The consumer holds its own event subscription, independent of
// the TUI's pump. Each ApprovalRequested fires a blocking MCP call in a
// separate handler goroutine so the subscription reader is never blocked.
type telegramApprovalConsumer struct {
	facade    *wiringFacade
	mcp       *agent.MCPManager
	principal core.Principal
	sessionID event.SessionID // captured once at Start; immutable thereafter

	cancel context.CancelFunc
	wg     sync.WaitGroup // tracks the reader goroutine + all in-flight handlers
}

// note reports a diagnostic as an ephemeral session note so it renders in the
// TUI's item stream instead of corrupting the alt-screen.
//
// os.Stderr is unusable here: in TUI mode it IS the live alt-screen terminal
// (restoreStderr runs at cmd/wakil/main.go:235 before the real model is
// installed), so writing to it injects bytes into the middle of Bubble Tea's
// frame. The corruption persists until a resize forces a full repaint — the
// exact symptom this replaced.
//
// The fallback is diag.Printf, NOT os.Stderr: diag is redirected to a per-session
// log file at startup and back to os.Stderr only after the TUI exits
// (cmd/wakil/main.go:286-300), so it never writes into a live alt-screen. A
// note that cannot be delivered falls back to a file-backed sink rather than
// the terminal — a note failing to publish does NOT establish that the
// terminal is ours to write to (the common cause of failure, a cancelled ctx
// during rotation, is precisely when the alt-screen is still active).
func (tc *telegramApprovalConsumer) note(ctx context.Context, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if err := tc.facade.host.NotifySessionNote(ctx, tc.principal, tc.sessionID, msg); err != nil {
		diag.Printf("telegram consumer: %s (note undeliverable: %v)", msg, err)
	}
}

// newTelegramApprovalConsumer creates a consumer for the given facade. It does
// NOT start it — call Start.
func newTelegramApprovalConsumer(f *wiringFacade, mcp *agent.MCPManager, principal core.Principal) *telegramApprovalConsumer {
	return &telegramApprovalConsumer{
		facade:    f,
		mcp:       mcp,
		principal: principal,
	}
}

// Start launches the consumer goroutine. It subscribes to the session's event
// stream and processes ApprovalRequested events. The subscription begins at
// the current durable head (live-only — no replay of past approvals).
func (tc *telegramApprovalConsumer) Start(ctx context.Context) error {
	f := tc.facade
	head := event.Seq(0)
	if snap, err := f.host.SessionSnapshot(ctx, tc.principal, f.sessionID); err == nil {
		head = snap.LastSeq
	}
	// Capture the session ID once so the consumer is bound to this session
	// for its entire lifetime. Rotation builds a fresh facade (and thus a
	// fresh consumer); this consumer never outlives the session it was
	// created for.
	tc.sessionID = f.sessionID

	sub, err := f.host.Subscribe(ctx, tc.principal, tc.sessionID, head)
	if err != nil {
		return fmt.Errorf("telegram consumer: subscribe: %w", err)
	}

	// Detach from the caller's context on purpose. Start receives the
	// bootstrap context, which is cancelled once the TUI finishes starting
	// up; a consumer bound to it would have its subscription reader die at
	// run()'s sub.Next(ctx) error return, leaving it permanently deaf while
	// still looking alive (no log, no symptom). The consumer's lifetime is
	// owned by Stop() via tc.cancel, called from facade.Close().
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	tc.cancel = cancel
	tc.wg.Add(1)
	go tc.run(ctx, sub)
	return nil
}

// Stop cancels the consumer goroutine and waits for it (and any in-flight
// handlers) to exit, bounded by consumerStopTimeout. Safe to call multiple
// times (subsequent calls are no-ops after the first cancel).
func (tc *telegramApprovalConsumer) Stop() {
	if tc.cancel != nil {
		tc.cancel()
	}

	// Bounded wait: a hung MCP transport should not block Close or rotation
	// indefinitely. After the timeout, the goroutine(s) may still be running
	// (leaked), but the facade is being torn down anyway — the host session
	// close will cancel the turn ctx, which unblocks any ParkApproval.
	done := make(chan struct{})
	go func() {
		tc.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(consumerStopTimeout):
		tc.note(context.Background(), "≫ telegram consumer: Stop timed out after %v", consumerStopTimeout)
	}
}

// run is the consumer loop. It reads events from the subscription and handles
// ApprovalRequested by calling the Telegram MCP tool in a separate goroutine.
// All other events are ignored (the TUI's pump handles them).
func (tc *telegramApprovalConsumer) run(ctx context.Context, sub core.EventSubscription) {
	defer tc.wg.Done()
	defer sub.Close()

	for {
		ev, err := sub.Next(ctx)
		if err != nil {
			// A silent return here is what made this bug invisible: the
			// consumer goes deaf with no symptom in the TUI. Report the
			// reason, distinguishing an expected Stop() from a real fault.
			if ctx.Err() != nil {
				return // Stop() or session close — expected
			}
			tc.note(ctx, "≫ telegram approval consumer stopped: %v", err)
			return
		}
		if ev.Kind != event.KindApprovalRequested {
			continue
		}
		p, ok := ev.Payload.(event.ApprovalRequested)
		if !ok {
			continue
		}
		// Handle in a separate goroutine so a blocking MCP call (Telegram
		// long-polling) does not block the subscription reader. If the TUI
		// resolves the approval before Telegram responds, the in-flight
		// call's result is a no-op (RespondToApproval is idempotent for
		// matching outcomes, and returns ErrApprovalAlreadyResolved for
		// conflicting ones — both are safe to discard).
		tc.wg.Add(1)
		go tc.handleApproval(ctx, p)
	}
}

// handleApproval calls the Telegram bridge MCP tool with the approval details
// and forwards the result to RespondToApproval. It blocks until the MCP tool
// returns (user responds or tool times out). If the MCP call fails or returns
// an unparseable result, it does NOT call RespondToApproval — the TUI
// keypress path is still live and can answer the approval.
//
// Failure policy:
//   - "approve"/"allow_reads"/"grant" → resolve with the matching outcome
//   - "decline"                       → resolve as deny
//   - "timeout"/"cancelled"           → do NOT resolve; the bridge reported a
//     non-decision (the user never answered), so the approval stays parked and
//     the TUI confirm gate remains the live fallback
//   - MCP transport error / unparseable → do NOT resolve (the bridge is
//     broken; the TUI keypress path remains the fallback)
func (tc *telegramApprovalConsumer) handleApproval(ctx context.Context, p event.ApprovalRequested) {
	defer tc.wg.Done()

	args, _ := json.Marshal(map[string]any{
		"tool_name":   p.ToolName,
		"headline":    p.Headline,
		"detail":      p.Detail,
		"read_action": p.ReadAction,
	})

	// Call the MCP tool with a confirmer that always approves.
	//
	// The always-true confirmer is LOAD-BEARING, not belt-and-suspenders:
	// send_confirmation is a write tool (it sends a Telegram message), so
	// CallTool's gate at internal/agent/mcp_manager.go:288 evaluates
	// !(isRead && allowReads) → true and calls this confirmer. An
	// always-false confirmer would make the consumer decline its own
	// infrastructure call. Passing allowReads=true does NOT skip the gate
	// (the "send" prefix marks it non-read); it only prevents the gate from
	// being additionally gated by the session's read-consent state.
	alwaysApprove := agent.Confirmer(func(string, string, string, bool) bool {
		return true
	})

	result := tc.mcp.CallTool(ctx, namespacedTelegramTool, string(args), alwaysApprove, true)

	outcome := parseTelegramResult(result)
	if outcome == "" {
		// MCP call failed or returned an unparseable result. Do not
		// resolve the approval — the TUI keypress path is still live
		// and can answer it.
		tc.note(ctx, "≫ telegram bridge failed (approval %s): %s — answer in the TUI to proceed",
			p.ApprovalID, strings.TrimSpace(result))
		return
	}

	// Map the Telegram result to a core.ApprovalOutcome. ok is false for a
	// non-decision outcome — see telegramOutcomeToCore.
	coreOutcome, ok := telegramOutcomeToCore(outcome)
	if !ok {
		tc.note(ctx, "≫ telegram: %s — answer in the TUI to proceed", outcome)
		return
	}

	err := tc.facade.host.RespondToApproval(ctx, tc.principal, core.ApprovalDecision{
		SessionID:  tc.sessionID,
		ApprovalID: p.ApprovalID,
		Outcome:    coreOutcome,
	})
	if err != nil {
		// The most common error is ErrApprovalAlreadyResolved — the TUI
		// keypress beat Telegram to it. That is expected and harmless.
		// Other errors (session closed, not found) are also safe to
		// log and discard — the turn's ParkApproval will be released by
		// the host session lifecycle.
		tc.note(ctx, "≫ telegram: approval %s not delivered: %v", p.ApprovalID, err)
	}
}

// telegramOutcomeToCore maps a telegram-bridge choice to a core.ApprovalOutcome.
//
// ok is FALSE for the non-decision outcomes "timeout" and "cancelled" (and any
// unrecognized choice): the caller must NOT resolve the approval. The bridge
// reaching its TELEGRAM_APPROVAL_TIMEOUT means "the user did not answer", not
// "the user declined" — mapping it to deny silently cancelled the user's tool
// call with no visible cause. Leaving the approval parked keeps the TUI
// confirm gate live as the documented fallback, so the user can still answer
// with y/a/g/n.
func telegramOutcomeToCore(outcome string) (core.ApprovalOutcome, bool) {
	switch outcome {
	case "approve":
		return core.ApprovalAllowOnce, true
	case "allow_reads":
		return core.ApprovalAllowReadsOnce, true
	case "grant":
		return core.ApprovalGrantTool, true
	case "decline":
		return core.ApprovalDeny, true
	default:
		// "timeout", "cancelled" — non-decisions.
		return "", false
	}
}

// parseTelegramResult extracts the approval outcome from the MCP tool result
// string. The telegram-bridge returns one of: "approve", "decline",
// "allow_reads", "grant", "timeout", "cancelled". Returns "" if the result
// cannot be parsed (error, malformed, etc.) — in that case the caller does NOT
// resolve the approval, leaving the TUI keypress path as the fallback.
func parseTelegramResult(result string) string {
	result = strings.TrimSpace(result)

	// Error from MCP layer — do not resolve.
	if strings.HasPrefix(result, "ERROR:") {
		return ""
	}
	if result == "(no output)" {
		return ""
	}

	// The telegram-bridge returns a JSON object with a "choice" field, or
	// a plain string. Try JSON first, then fall back to plain string matching.
	var parsed struct {
		Choice string `json:"choice"`
	}
	if json.Unmarshal([]byte(result), &parsed) == nil && parsed.Choice != "" {
		if isTelegramChoice(parsed.Choice) {
			return parsed.Choice
		}
	}

	// Plain string fallback.
	if isTelegramChoice(result) {
		return result
	}

	return ""
}

// isTelegramChoice reports whether s is a recognized telegram-bridge outcome.
func isTelegramChoice(s string) bool {
	switch s {
	case "approve", "decline", "allow_reads", "grant", "timeout", "cancelled":
		return true
	}
	return false
}
