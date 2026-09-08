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

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
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

	ctx, cancel := context.WithCancel(ctx)
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
		fmt.Fprintln(os.Stderr, "telegram consumer: Stop timed out after", consumerStopTimeout)
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
			return // ctx cancelled or subscription closed
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
//   - "timeout"/"cancelled"           → resolve as deny (the bridge reported
//     an explicit terminal state; the user is unreachable)
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

	// Call the MCP tool with a confirmer that always approves. The
	// send_confirmation tool is an infrastructure call — it must not trigger
	// the agent's interactive confirmation gate (which would recurse back
	// into the approval system). Passing always-true bypasses the gate in
	// MCPManager.CallTool (which calls confirm only for non-read tools when
	// allowReads is false; we pass allowReads=true so the gate is skipped
	// entirely, but the confirmer is a belt-and-suspenders safeguard).
	alwaysApprove := agent.Confirmer(func(string, string, string, bool) bool {
		return true
	})

	result := tc.mcp.CallTool(ctx, namespacedTelegramTool, string(args), alwaysApprove, true)

	outcome := parseTelegramResult(result)
	if outcome == "" {
		// MCP call failed or returned an unparseable result. Do not
		// resolve the approval — the TUI keypress path is still live
		// and can answer it.
		fmt.Fprintf(os.Stderr, "telegram consumer: unparseable result for approval %s: %q\n",
			p.ApprovalID, result)
		return
	}

	// Map the Telegram result to a core.ApprovalOutcome.
	var coreOutcome core.ApprovalOutcome
	switch outcome {
	case "approve":
		coreOutcome = core.ApprovalAllowOnce
	case "allow_reads":
		coreOutcome = core.ApprovalAllowReadsOnce
	case "grant":
		coreOutcome = core.ApprovalGrantTool
	default:
		// "decline", "timeout", "cancelled" all map to deny.
		coreOutcome = core.ApprovalDeny
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
		fmt.Fprintf(os.Stderr, "telegram consumer: RespondToApproval for %s: %v\n",
			p.ApprovalID, err)
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
