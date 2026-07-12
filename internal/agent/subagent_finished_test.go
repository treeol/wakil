package agent

// Tests for SubagentFinishedMsg — the display-only early completion event
// emitted from the worker goroutine the moment a child returns, before the
// Phase C cost fold and before SubagentDoneMsg.

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/proxy"
)

// collectEvents captures agent events in arrival order, thread-safe.
type collectEvents struct {
	mu     sync.Mutex
	events []interface{}
}

func (c *collectEvents) sink(msg interface{}) {
	c.mu.Lock()
	c.events = append(c.events, msg)
	c.mu.Unlock()
}

func (c *collectEvents) snapshot() []interface{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]interface{}, len(c.events))
	copy(cp, c.events)
	return cp
}

// eventOrder returns the ordered list of subagent event types (by ChatID) for
// assertion. Only SubagentFinishedMsg and SubagentDoneMsg are tracked.
func eventOrder(events []interface{}, chatID string) []string {
	var order []string
	for _, e := range events {
		switch m := e.(type) {
		case SubagentFinishedMsg:
			if m.ChatID == chatID {
				order = append(order, "finished")
			}
		case SubagentDoneMsg:
			if m.ChatID == chatID {
				order = append(order, "done")
			}
		}
	}
	return order
}

// TestWorkerEmitsFinishedBeforeDone verifies that the parallel worker emits
// SubagentFinishedMsg at actual completion time, and that SubagentDoneMsg
// (Phase C) follows for the same child. Order is asserted: finished before
// done.
func TestWorkerEmitsFinishedBeforeDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		writeSSE(w, contentChunk(summaryFor(taskFromBody(body))))
	}))
	defer srv.Close()

	collector := &collectEvents{}
	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.EventSink = collector.sink

	block := []proxy.ToolCall{
		{ID: "d1", Function: proxy.FunctionCall{Name: "dispatch_subagent", Arguments: `{"task":"TASK-A"}`}},
	}
	// runParallelSubagentBlock runs all three phases: Start (A), workers (B),
	// and Done (C). SubagentFinishedMsg fires in Phase B; SubagentDoneMsg in C.
	// We need to find the ChatID from the Start event.
	app.runParallelSubagentBlock(context.Background(), block)

	// Find the ChatID from the Start event.
	var chatID string
	for _, e := range collector.snapshot() {
		if m, ok := e.(SubagentStartMsg); ok {
			chatID = m.ChatID
			break
		}
	}
	if chatID == "" {
		t.Fatal("no SubagentStartMsg found")
	}

	order := eventOrder(collector.snapshot(), chatID)
	if len(order) != 2 {
		t.Fatalf("want [finished done] for %s, got %v (%d events)", chatID, order, len(order))
	}
	if order[0] != "finished" || order[1] != "done" {
		t.Errorf("event order = %v, want [finished done]", order)
	}
}

// TestFinishedCarriesDisplayData verifies SubagentFinishedMsg carries the
// child's own cost, files changed, and summary preview — display data only.
func TestFinishedCarriesDisplayData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		writeSSE(w, contentChunk(summaryFor(taskFromBody(body))))
	}))
	defer srv.Close()

	collector := &collectEvents{}
	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.EventSink = collector.sink

	block := []proxy.ToolCall{
		{ID: "d1", Function: proxy.FunctionCall{Name: "dispatch_subagent", Arguments: `{"task":"TASK-A"}`}},
	}
	app.runParallelSubagentBlock(context.Background(), block)

	var finMsg *SubagentFinishedMsg
	for _, e := range collector.snapshot() {
		if m, ok := e.(SubagentFinishedMsg); ok {
			finMsg = &m
			break
		}
	}
	if finMsg == nil {
		t.Fatal("no SubagentFinishedMsg received")
	}
	if finMsg.SummaryPreview == "" {
		t.Error("SummaryPreview should be non-empty")
	}
	if !strings.Contains(finMsg.SummaryPreview, "TASK-A") {
		t.Errorf("SummaryPreview = %q, want it to contain TASK-A", finMsg.SummaryPreview)
	}
	if finMsg.FinishedAt.IsZero() {
		t.Error("FinishedAt should be set")
	}
}

// TestFastChildFinishedWhileSlowRuns verifies that with two children (fast and
// slow), the fast child's SubagentFinishedMsg arrives while the slow child is
// still running. Uses synchronized mocks (a release channel), not sleeps —
// per the lock-test hardening pattern.
func TestFastChildFinishedWhileSlowRuns(t *testing.T) {
	releaseSlow := make(chan struct{})
	var slowArrived atomic.Int32
	var fastFinished atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		task := taskFromBody(body)
		if task == "TASK-B" {
			// Slow child: hold until released.
			slowArrived.Add(1)
			<-releaseSlow
			writeSSE(w, contentChunk(summaryFor(task)))
			return
		}
		// Fast child: return immediately.
		writeSSE(w, contentChunk(summaryFor(task)))
	}))
	defer srv.Close()

	collector := &collectEvents{}
	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.EventSink = func(msg interface{}) {
		if _, ok := msg.(SubagentFinishedMsg); ok {
			// The fast child (TASK-A) is the one that finishes first. We can't
			// know the ChatID ahead of time (runParallelSubagentBlock mints it
			// in Phase A), so we track by checking if the slow child has
			// finished yet. If not, this must be the fast child.
			fastFinished.Add(1)
		}
		collector.sink(msg)
	}

	block := []proxy.ToolCall{
		{ID: "d1", Function: proxy.FunctionCall{Name: "dispatch_subagent", Arguments: `{"task":"TASK-A"}`}},
		{ID: "d2", Function: proxy.FunctionCall{Name: "dispatch_subagent", Arguments: `{"task":"TASK-B"}`}},
	}
	done := make(chan struct{})
	go func() {
		app.runParallelSubagentBlock(context.Background(), block)
		close(done)
	}()

	// Wait for the slow child to arrive and be blocked, and the fast child to
	// have finished. We poll: the fast child's SubagentFinishedMsg should
	// arrive while the slow child is still running (before releaseSlow is closed).
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if slowArrived.Load() > 0 && fastFinished.Load() > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	if slowArrived.Load() == 0 {
		t.Fatal("slow child did not arrive in time")
	}
	if fastFinished.Load() == 0 {
		t.Error("fast child's SubagentFinishedMsg should have arrived while slow child is still running")
	}

	// Release the slow child so the test can finish.
	close(releaseSlow)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("block did not finish after releasing slow child")
	}

	// Both children should have [finished, done] in order. Find their ChatIDs
	// from the Start events.
	events := collector.snapshot()
	var chatIDs []string
	for _, e := range events {
		if m, ok := e.(SubagentStartMsg); ok {
			chatIDs = append(chatIDs, m.ChatID)
		}
	}
	if len(chatIDs) != 2 {
		t.Fatalf("want 2 Start events, got %d", len(chatIDs))
	}
	for _, chatID := range chatIDs {
		order := eventOrder(events, chatID)
		if len(order) != 2 || order[0] != "finished" || order[1] != "done" {
			t.Errorf("chat %s: event order = %v, want [finished done]", chatID, order)
		}
	}
}

// TestSequentialPathEmitsFinishedBeforeDone verifies the sequential
// (single-dispatch) path emits both events in the same order: finished before
// done. This keeps the two paths symmetric.
func TestSequentialPathEmitsFinishedBeforeDone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if isSubagentRequest(body) {
			writeSSE(w, contentChunk(summaryFor(taskFromBody(body))))
			return
		}
		// Parent: emit one dispatch_subagent call, then a final answer.
		writeSSE(w, toolCallFrames("d1", "dispatch_subagent", `{"task":"TASK-A"}`)...)
	}))
	defer srv.Close()

	collector := &collectEvents{}
	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.EventSink = collector.sink

	// Run through ExecuteToolCall so the sequential dispatch_subagent handler runs.
	tc := proxy.ToolCall{
		ID:       "d1",
		Function: proxy.FunctionCall{Name: "dispatch_subagent", Arguments: `{"task":"TASK-A"}`},
	}
	app.ExecuteToolCall(context.Background(), tc)

	// Find the ChatID from the Start event.
	var chatID string
	for _, e := range collector.snapshot() {
		if m, ok := e.(SubagentStartMsg); ok {
			chatID = m.ChatID
			break
		}
	}
	if chatID == "" {
		t.Fatal("no SubagentStartMsg found")
	}

	order := eventOrder(collector.snapshot(), chatID)
	if len(order) != 2 || order[0] != "finished" || order[1] != "done" {
		t.Errorf("sequential path event order = %v, want [finished done]", order)
	}
}
