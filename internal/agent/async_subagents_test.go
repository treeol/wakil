package agent

// Card #122 Phase 1 acceptance tests: async discovery subagents through the
// async funnel.
//
// Invariants under test:
//   - Protocol closure: every tool_call_id gets a placeholder; the real summary
//     arrives via the async envelope (never a second tool result).
//   - Cost isn't double-folded by worker-terminal + drain-delivery.
//   - Capacity full → explicit rejection, never silent sync fallback.
//   - Edit-capable dispatch stays SYNCHRONOUS (read-only-detach security bound).

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// TestAsyncDiscoveryDeliversExactlyOnce drives a pure-discovery block through
// Send, asserts protocol closure (a placeholder per tool_call_id), and exactly-
// once delivery (a single envelope; the second drain is empty).
func TestAsyncDiscoveryDeliversExactlyOnce(t *testing.T) {
	var parentCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if isSubagentRequest(body) {
			writeSSE(w, contentChunk(summaryFor(taskFromBody(body))))
			return
		}
		switch parentCalls.Add(1) {
		case 1:
			writeSSE(w, twoDispatchFrames("TASK-A", "TASK-B")...)
		default:
			writeSSE(w, contentChunk("done"))
		}
	}))
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	if _, err := app.Send(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}

	// Protocol closure: exactly one tool result per tool_call_id (placeholder).
	var toolMsgs []proxy.Message
	for _, m := range app.Conv {
		if m.Role == "tool" && m.Name == "dispatch_subagent" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	if len(toolMsgs) != 2 {
		t.Fatalf("want 2 placeholders (one per tool_call_id), got %d", len(toolMsgs))
	}
	for _, m := range toolMsgs {
		if !strings.Contains(DerefStr(m.Content), "queued as op-") {
			t.Errorf("expected placeholder, got %q", DerefStr(m.Content))
		}
	}

	// Exactly-once delivery: one envelope, then empty.
	waitAsyncOps(t, app)
	env := app.drainAsyncInbox()
	if !strings.Contains(env, "TASK-A") || !strings.Contains(env, "TASK-B") {
		t.Fatalf("envelope missing tasks: %q", env)
	}
	if env2 := app.drainAsyncInbox(); env2 != "" {
		t.Fatalf("second drain not empty: %q", env2)
	}
	if !strings.HasPrefix(env, asyncBlockHeader) {
		t.Errorf("envelope should start with async header; got %q", truncateStr(env, 40))
	}
}

// TestAsyncDiscoveryCostNotDoubleFolded verifies the parent cost tracker isn't
// inflated by the worker-terminal fold PLUS the drain-delivery fold (idempotent
// exactly-once cost). The second drain must not add rows.
func TestAsyncDiscoveryCostNotDoubleFolded(t *testing.T) {
	var parentCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if isSubagentRequest(body) {
			writeSSE(w, contentChunk(summaryFor(taskFromBody(body))))
			return
		}
		switch parentCalls.Add(1) {
		case 1:
			writeSSE(w, twoDispatchFrames("TASK-A", "TASK-B")...)
		default:
			writeSSE(w, contentChunk("done"))
		}
	}))
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Costs = proxy.NewCostTracker()

	if _, err := app.Send(context.Background(), "go"); err != nil {
		t.Fatal(err)
	}
	totalCalls := func() int {
		_, rows := app.Costs.Snapshot()
		n := 0
		for _, r := range rows {
			n += r.Calls
		}
		return n
	}
	waitAsyncOps(t, app)
	app.drainAsyncInbox()
	first := totalCalls()
	// A second drain must not re-fold (exactly-once via costCommitted).
	app.drainAsyncInbox()
	second := totalCalls()
	if first == 0 {
		t.Errorf("expected child cost rows folded; got 0 calls")
	}
	if second != first {
		t.Errorf("cost double-folded after second drain: %d → %d", first, second)
	}
}

// TestAsyncDiscoveryCapacityRejectNoSilentSync verifies that when the async
// registry is full, a discovery dispatch returns an EXPLICIT rejection rather
// than silently running synchronously (never reintroducing a block when loaded).
func TestAsyncDiscoveryCapacityRejectNoSilentSync(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		writeSSE(w, contentChunk(summaryFor(taskFromBody(body))))
	}))
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	// Fill the registry: reserve asyncMaxActive non-terminal slots.
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
			app.finishAsyncOp(op) // release for other tests
		}
	}()

	block := []proxy.ToolCall{
		{ID: "d1", Function: proxy.FunctionCall{Name: "dispatch_subagent", Arguments: `{"task":"TASK-A"}`}},
	}
	results := app.runParallelSubagentBlock(context.Background(), block)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if !strings.Contains(results[0], "ERROR: async") {
		t.Errorf("expected explicit capacity rejection, got %q", results[0])
	}
	if strings.Contains(results[0], "TASK-A") && !strings.Contains(results[0], "ERROR") {
		t.Errorf("silent sync fallback under load — got %q", results[0])
	}
}

// TestPrepareSubagentBlockMixedStaysSync verifies a block containing an edit- or
// tools-capable job is NOT pure discovery, so it cannot be detached asynchronously
// — preserving the child-vs-parent mutation invariant (edit/tools stay sync).
func TestPrepareSubagentBlockMixedStaysSync(t *testing.T) {
	app := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetAutoApprove(true) // session write consent for the edit child

	block := []proxy.ToolCall{
		{ID: "d1", Function: proxy.FunctionCall{Name: "dispatch_subagent", Arguments: `{"task":"read"}`}},
		{ID: "d2", Function: proxy.FunctionCall{Name: "dispatch_subagent", Arguments: `{"task":"write","capability":"edit"}`}},
	}
	_, _, _, pureDiscovery, prepared := app.prepareSubagentBlock(block)
	if !prepared {
		t.Fatal("block should be prepared (both jobs valid + consented)")
	}
	if pureDiscovery {
		t.Fatal("mixed discovery+edit block must NOT be flagged pure discovery (async detach would break the mutation invariant)")
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
