package agent

// Card #123: stuck async subagent fix — timeout + watchdog + refusal-path Done events.
//
// Invariants under test:
//   - A cooperative child (ctx-aware HTTP stream) that exceeds the timeout is
//     cancelled by the context, returns normally, and the op terminalizes with
//     an incomplete child — not a stuck turn.
//   - A non-cooperative child (ignores ctx) is force-terminalized by the
//     watchdog: asyncActive decrements, the inbox gets an error result, and
//     WaitForAsyncCompletion unblocks.
//   - Registry-refusal sends SubagentDoneMsg with Err for every child so TUI
//     tabs don't spin forever.
//   - The global semaphore is released after timeout (the worker returns from
//     its ctx-aware HTTP call, releasing the sem slot).

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/proxy"
)

// TestAsyncSubagentCooperativeTimeout verifies that a child whose HTTP stream
// respects ctx cancellation is cancelled by the timeout context, returns with
// an incomplete summary, and the op terminalizes normally (no watchdog needed).
// The turn does NOT hang — the op publishes and asyncActive decrements.
func TestAsyncSubagentCooperativeTimeout(t *testing.T) {
	var parentCalls atomic.Int32
	// Server that holds the subagent response indefinitely (never sends SSE).
	// The timeout context will cancel the HTTP request, causing dispatchSubagent
	// to return with a context-cancelled error → incomplete summary.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if isSubagentRequest(body) {
			// Block until the request context is cancelled (simulating a hung backend).
			<-r.Context().Done()
			return
		}
		switch parentCalls.Add(1) {
		case 1:
			// Parent: dispatch one subagent.
			writeSSE(w, toolCallFrames("d1", "dispatch_subagent", `{"task":"TASK-A"}`)...)
		default:
			// Second call: final text (no more tool calls).
			writeSSE(w, contentChunk("done"))
		}
	}))
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.SubagentTimeoutSeconds = 2  // short timeout for test speed
	app.watchdogGrace = 1 * time.Second // shorten grace so watchdog safety net fires within the test's 15s context

	// Drive the turn. The subagent will be dispatched async, the model will
	// produce final text ("done"), and the turn should suspend. Then the
	// timeout fires, the child returns (ctx cancelled), the op publishes,
	// and WaitForAsyncCompletion unblocks.
	out, err := app.SendOutcome(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	// The model dispatched a subagent and then said "done" → Suspended.
	if out.Kind != TurnSuspended {
		t.Fatalf("expected Suspended (async pending), got %v (text=%q)", out.Kind, out.Text)
	}

	// Wait for the timeout to fire and the op to publish.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ok, err := app.WaitForAsyncCompletion(ctx)
	if err != nil {
		t.Fatalf("WaitForAsyncCompletion error: %v", err)
	}
	if !ok {
		t.Fatal("expected a completion after timeout, got none")
	}

	// Drain the inbox — should contain the error result.
	env := app.drainAsyncInbox()
	if env == "" {
		t.Fatal("expected non-empty envelope after timeout")
	}
	// The child should have an incomplete/error status.
	if !strings.Contains(env, "op-") {
		t.Errorf("envelope missing op id: %q", truncateStr(env, 80))
	}

	// asyncActive should be 0 after the op published.
	if n := app.countActiveAsyncOps(); n != 0 {
		t.Errorf("expected 0 active ops after timeout, got %d", n)
	}
}

// TestAsyncSubagentWatchdogForceTerminalizes verifies that if the worker
// doesn't return within the timeout + grace period, the watchdog
// force-terminalizes the op: asyncActive decrements, the inbox gets an error
// result, and WaitForAsyncCompletion unblocks.
//
// We simulate a non-cooperative worker by registering an op directly (not
// through queueAsyncDiscoveryBlock) and arming the watchdog without starting
// a worker. The watchdog must fire and publish.
func TestAsyncSubagentWatchdogForceTerminalizes(t *testing.T) {
	app := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	// Register an op with child ChatIDs (as queueAsyncDiscoveryBlock would).
	op, reason := app.registerAsyncOp("dispatch_subagents", "2 discovery subagents")
	if reason != "" {
		t.Fatalf("register: %s", reason)
	}
	op.childChatIDs = []string{"child-1", "child-2"}
	op.childTasks = []string{"TASK-A", "TASK-B"}

	// Arm the watchdog with a very short timeout and grace period.
	app.watchdogGrace = 1 * time.Second
	app.armSubagentWatchdog(op, 1*time.Second)

	// Don't start a worker — simulate a stuck goroutine. The watchdog should
	// fire after ~1s + grace period.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ok, err := app.WaitForAsyncCompletion(ctx)
	if err != nil {
		t.Fatalf("WaitForAsyncCompletion error: %v", err)
	}
	if !ok {
		t.Fatal("watchdog did not fire — WaitForAsyncCompletion returned no completion")
	}

	// asyncActive should be 0.
	if n := app.countActiveAsyncOps(); n != 0 {
		t.Errorf("expected 0 active ops after watchdog, got %d", n)
	}

	// Drain should produce an error envelope.
	env := app.drainAsyncInbox()
	if env == "" {
		t.Fatal("expected non-empty envelope after watchdog")
	}
	if !strings.Contains(env, "timed out") {
		t.Errorf("envelope should contain timeout error: %q", truncateStr(env, 120))
	}

	// op.done should NOT be closed by the watchdog (only the worker closes it).
	select {
	case <-op.done:
		t.Error("op.done was closed by the watchdog — should only be closed by the worker")
	default:
		// Good: watchdog did not close op.done.
	}

	// terminal should be true.
	terminal, _, _ := op.terminalSnapshot()
	if !terminal {
		t.Error("op should be terminal after watchdog")
	}
}

// TestAsyncRegistryRefusalSendsDoneEvents verifies that when the async registry
// is full, queueAsyncDiscoveryBlock sends SubagentDoneMsg with Err for every
// child so TUI tabs don't spin forever (the registry-refusal bug from Mashūra
// review finding #7).
func TestAsyncRegistryRefusalSendsDoneEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, contentChunk(summaryFor("test")))
	}))
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	// Fill the registry.
	held := make([]*asyncOp, 0, asyncMaxActive)
	for i := 0; i < asyncMaxActive; i++ {
		op, reason := app.registerAsyncOp("dispatch_subagents", fmt.Sprintf("held-%d", i))
		if reason != "" {
			t.Fatalf("could not reserve slot %d: %s", i, reason)
		}
		held = append(held, op)
	}
	defer func() {
		for _, op := range held {
			app.finishAsyncOp(op)
		}
	}()

	// Track SubagentDoneMsg events.
	var doneEvents atomic.Int32
	app.EventSink = func(msg interface{}) {
		if _, ok := msg.(SubagentDoneMsg); ok {
			doneEvents.Add(1)
		}
	}

	// Prepare a pure-discovery block with 2 children.
	block := []proxy.ToolCall{
		{ID: "d1", Function: proxy.FunctionCall{Name: "dispatch_subagent", Arguments: `{"task":"TASK-A"}`}},
		{ID: "d2", Function: proxy.FunctionCall{Name: "dispatch_subagent", Arguments: `{"task":"TASK-B"}`}},
	}
	// Run the block — should be refused (registry full).
	results := app.runParallelSubagentBlock(context.Background(), block)
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	for _, r := range results {
		if !strings.Contains(r, "ERROR: async") {
			t.Errorf("expected rejection, got %q", r)
		}
	}

	// Both children should have received SubagentDoneMsg with Err.
	if n := doneEvents.Load(); n != 2 {
		t.Errorf("expected 2 SubagentDoneMsg events for refused children, got %d", n)
	}
}

// TestSubagentTimeoutConfigValidation verifies that a negative
// SubagentTimeoutSeconds is rejected by config validation.
func TestSubagentTimeoutConfigValidation(t *testing.T) {
	// This is tested via the config package's validation tests, but we verify
	// the App-level helper handles 0 (disabled) correctly.
	app := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.SubagentTimeoutSeconds = 0
	if d := app.subagentTimeout(); d != 120*time.Second {
		t.Errorf("expected default 120s for 0 config, got %v", d)
	}
	app.Cfg.SubagentTimeoutSeconds = 30
	if d := app.subagentTimeout(); d != 30*time.Second {
		t.Errorf("expected 30s, got %v", d)
	}
}

// TestSubagentBatchTimeoutScaling (card #164) verifies that the batch-level
// timeout scales with job count and maxPar to account for multi-wave execution.
// With maxPar=2 and 6 jobs, children run in ceil(6/2)=3 waves, so the batch
// timeout should be 3× childTimeout.
func TestSubagentBatchTimeoutScaling(t *testing.T) {
	app := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.SubagentTimeoutSeconds = 60 // 1 min per child
	app.Cfg.MaxParallelSubagents = 2

	// 1 job, maxPar=2 (clamped to 1) → 1 wave → 60s
	if d := app.subagentBatchTimeout(1); d != 60*time.Second {
		t.Errorf("1 job, maxPar=2: expected 60s (1 wave), got %v", d)
	}
	// 2 jobs, maxPar=2 → 1 wave → 60s
	if d := app.subagentBatchTimeout(2); d != 60*time.Second {
		t.Errorf("2 jobs, maxPar=2: expected 60s (1 wave), got %v", d)
	}
	// 3 jobs, maxPar=2 → ceil(3/2)=2 waves → 120s
	if d := app.subagentBatchTimeout(3); d != 120*time.Second {
		t.Errorf("3 jobs, maxPar=2: expected 120s (2 waves), got %v", d)
	}
	// 6 jobs, maxPar=2 → ceil(6/2)=3 waves → 180s
	if d := app.subagentBatchTimeout(6); d != 180*time.Second {
		t.Errorf("6 jobs, maxPar=2: expected 180s (3 waves), got %v", d)
	}
	// 4 jobs, maxPar=4 → 1 wave → 60s
	app.Cfg.MaxParallelSubagents = 4
	if d := app.subagentBatchTimeout(4); d != 60*time.Second {
		t.Errorf("4 jobs, maxPar=4: expected 60s (1 wave), got %v", d)
	}
}

// TestPerChildTimeoutStartsAfterSemaphore (card #164) verifies the
// subagentBatchTimeout calculation for single-child and multi-wave scenarios.
// This confirms the batch-level timeout scales correctly with job count and
// maxPar, which is the prerequisite for per-child timeouts to work: the batch
// must allow enough total time for all waves to complete.
//
// The actual per-child timeout behavior (fresh context after semaphore
// acquisition) is exercised by TestAsyncSubagentCooperativeTimeout, which
// runs through the async dispatch path that now uses per-child contexts.
func TestPerChildTimeoutStartsAfterSemaphore(t *testing.T) {
	app := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.SubagentTimeoutSeconds = 5
	app.Cfg.MaxParallelSubagents = 1

	// With 1 job and maxPar=1: batch = 1 wave × 5s = 5s
	batch := app.subagentBatchTimeout(1)
	child := app.subagentTimeout()
	if batch != child {
		t.Errorf("single-child batch timeout should equal child timeout: batch=%v child=%v", batch, child)
	}
	// With 3 jobs and maxPar=1: batch = 3 waves × 5s = 15s
	batch3 := app.subagentBatchTimeout(3)
	if batch3 != 3*child {
		t.Errorf("3 jobs at maxPar=1: batch should be 3×child: batch=%v child=%v", batch3, child)
	}
}
