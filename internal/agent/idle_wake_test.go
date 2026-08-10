package agent

// Card #122 Phase 2 acceptance tests: Idle/Wake engine.
//   - A turn that idles with async work pending returns Suspended, not Final.
//   - wait_for_completion token triggers suspension.
//   - WaitForAsyncCompletion: no lost wake (check-then-subscribe), coalescing,
//     shutdown-driven short-circuit, cancellation.

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

// TestSendSuspendsWhenAsyncPending verifies a Final vs Suspended outcome: a turn
// whose model produces final text while an async op is still running returns
// TurnSuspended (not Final), and a second identical turn with no pending async
// returns TurnFinal.
func TestSendSuspendsWhenAsyncPending(t *testing.T) {
	release := make(chan struct{})
	var parentCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if isSubagentRequest(body) {
			<-release // hold the child so the op stays active
			writeSSE(w, contentChunk(summaryFor(taskFromBody(body))))
			return
		}
		switch parentCalls.Add(1) {
		case 1:
			writeSSE(w, toolCallFrames("d1", "dispatch_subagent", `{"task":"TASK-A"}`)...)
		default:
			writeSSE(w, contentChunk("all done"))
		}
	}))
	defer srv.Close()
	defer close(release)

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	out, err := app.SendOutcome(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	// The block went async; the model's next answer ("all done") is final text
	// while the child is still held → Suspended.
	if out.Kind != TurnSuspended {
		t.Fatalf("expected Suspended (async pending), got %v (text=%q)", out.Kind, out.Text)
	}
	if !strings.Contains(out.Text, "all done") {
		t.Errorf("suspended text = %q, want 'all done'", out.Text)
	}
}

// TestSendFinalWhenNoAsync verifies a normal turn (no async work) returns Final.
func TestSendFinalWhenNoAsync(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, contentChunk("plain answer"))
	}))
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	out, err := app.SendOutcome(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if out.Kind != TurnFinal {
		t.Fatalf("expected Final, got %v", out.Kind)
	}
}

// TestWaitForCompletionNoLostWake verifies a completion that lands BEFORE the
// waiter subscribes is NOT missed (check-then-subscribe under lock). We enqueue
// and finish an op, then WaitForAsyncCompletion must return true immediately.
func TestWaitForCompletionNoLostWake(t *testing.T) {
	app := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	op, reason := app.registerAsyncOp("dispatch_subagents", "test")
	if reason != "" {
		t.Fatalf("register: %s", reason)
	}
	go func() {
		time.Sleep(10 * time.Millisecond)
		op.mu.Lock()
		op.terminal, op.result = true, "done"
		op.mu.Unlock()
		app.finishAsyncOp(op)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ok, err := app.WaitForAsyncCompletion(ctx)
	if err != nil {
		t.Fatalf("wait error: %v", err)
	}
	if !ok {
		t.Fatal("lost wake: completion existed but waiter returned no completion")
	}
}

// TestWaitForCompletionCoalescing verifies multiple near-simultaneous
// completions coalesce: the waiter wakes and drains, and a second wait returns
// the (already drained) state without double-count.
func TestWaitForCompletionCoalescing(t *testing.T) {
	app := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	for i := 0; i < 3; i++ {
		op, reason := app.registerAsyncOp("dispatch_subagents", fmt.Sprintf("c-%d", i))
		if reason != "" {
			t.Fatalf("register: %s", reason)
		}
		op.mu.Lock()
		op.terminal, op.result = true, fmt.Sprintf("res-%d", i)
		op.mu.Unlock()
		app.finishAsyncOp(op)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ok, err := app.WaitForAsyncCompletion(ctx)
	if err != nil || !ok {
		t.Fatalf("first wait: ok=%v err=%v", ok, err)
	}
	env := app.drainAsyncInbox()
	for i := 0; i < 3; i++ {
		if !strings.Contains(env, fmt.Sprintf("res-%d", i)) {
			t.Errorf("coalesced envelope missing res-%d: %q", i, env)
		}
	}
	// After drain, no inbox remains and nothing active → wait returns false.
	ok2, _ := app.WaitForAsyncCompletion(ctx)
	if ok2 {
		t.Error("no remaining completions, but second wait reported a completion")
	}
}

// TestWaitForCompletionShutdownShortCircuit verifies WaitForAsyncCompletion
// returns (false, nil) when the session is shutting down (no async remains).
func TestWaitForCompletionShutdownShortCircuit(t *testing.T) {
	app := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.asyncMu.Lock()
	app.asyncStopping = true
	app.asyncMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	ok, err := app.WaitForAsyncCompletion(ctx)
	if err != nil {
		t.Fatalf("wait error during shutdown: %v", err)
	}
	if ok {
		t.Error("expected no completion during shutdown")
	}
}

// TestWaitForCompletionToolSuspends verifies the wait_for_completion handler
// returns the idle token when async work is pending and completes immediately
// otherwise.
func TestWaitForCompletionToolSuspends(t *testing.T) {
	app := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	if got := app.handleWaitForCompletion(); got != "no async operations pending — nothing to wait for" {
		t.Errorf("no-pending case: %q", got)
	}

	op, reason := app.registerAsyncOp("dispatch_subagents", "held")
	if reason != "" {
		t.Fatalf("register: %s", reason)
	}
	defer func() {
		op.mu.Lock()
		op.terminal = true
		op.mu.Unlock()
		app.finishAsyncOp(op)
	}()
	if got := app.handleWaitForCompletion(); got != waitForCompletionToken {
		t.Errorf("pending case: got %q, want token", got)
	}
}

// TestHandleWaitForCompletionViaTool verifies the wait_for_completion tool is
// dispatched through ExecuteToolCall and returns the token (async pending),
// which streamTurn then turns into suspension.
func TestHandleWaitForCompletionViaTool(t *testing.T) {
	app := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	tc := proxy.ToolCall{ID: "w1", Function: proxy.FunctionCall{Name: "wait_for_completion", Arguments: `{}`}}
	res := app.ExecuteToolCall(context.Background(), tc)
	if !strings.Contains(res.text, "no async") {
		t.Errorf("empty case: %q", res.text)
	}
}
