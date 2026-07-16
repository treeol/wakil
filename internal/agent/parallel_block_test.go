package agent

// Step-4 tests for the parallel-subagents plan: contiguous-block parallel
// execution inside Send, order preservation, forced-concurrency barrier,
// cancellation stubbing, and the semaphore bound.

import (
	"context"
	"encoding/json"
	"fmt"
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

// twoDispatchFrames returns SSE frames for an assistant message carrying TWO
// dispatch_subagent tool calls in one turn (indexes 0 and 1).
func twoDispatchFrames(taskA, taskB string) []string {
	f := func(idx int, id, task string) string {
		args, _ := json.Marshal(fmt.Sprintf(`{"task":%q}`, task))
		return fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":%d,"id":%q,"type":"function","function":{"name":"dispatch_subagent","arguments":%s}}]},"finish_reason":null}]}`, idx, id, args)
	}
	return []string{
		f(0, "d1", taskA),
		f(1, "d2", taskB),
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
}

// isSubagentRequest reports whether the request body belongs to a subagent
// turn. Matched on the subagent SYSTEM PROMPT opening ("You are a focused
// discovery subagent") — NOT on "discovery subagent" alone, because the
// parent's requests also contain that phrase inside the dispatch_subagent
// tool description.
func isSubagentRequest(body []byte) bool {
	return strings.Contains(string(body), "You are a focused discovery subagent")
}

// taskFromBody extracts which task ("TASK-A" or "TASK-B") a subagent request
// is working on, from the pinned user message.
func taskFromBody(body []byte) string {
	if strings.Contains(string(body), "TASK-A") {
		return "TASK-A"
	}
	if strings.Contains(string(body), "TASK-B") {
		return "TASK-B"
	}
	return ""
}

func summaryFor(task string) string {
	return fmt.Sprintf(`{"objective":%q,"findings":[{"summary":"done %s","location":"x.go:1","kind":"fact","weight":"low"}]}`, task, task)
}

func writeSSE(w http.ResponseWriter, frames ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, f := range frames {
		fmt.Fprintf(w, "data: %s\n\n", f)
	}
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// TestParallelBlockRunsConcurrently proves two dispatches in one assistant
// turn actually overlap: the server holds each subagent response on a barrier
// that only opens once BOTH subagent requests have arrived. Sequential
// execution would deadlock (guarded by the test timeout below).
func TestParallelBlockRunsConcurrently(t *testing.T) {
	var parentCalls atomic.Int32
	var subArrived atomic.Int32
	barrier := make(chan struct{})
	var once sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if isSubagentRequest(body) {
			if subArrived.Add(1) >= 2 {
				once.Do(func() { close(barrier) })
			}
			select {
			case <-barrier:
			case <-time.After(10 * time.Second):
				http.Error(w, "barrier timeout: subagents did not run concurrently", 500)
				return
			}
			writeSSE(w, contentChunk(summaryFor(taskFromBody(body))))
			return
		}
		switch parentCalls.Add(1) {
		case 1:
			writeSSE(w, twoDispatchFrames("TASK-A", "TASK-B")...)
		default:
			writeSSE(w, contentChunk("both done"))
		}
	}))
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	done := make(chan error, 1)
	go func() {
		_, err := app.Send(context.Background(), "go")
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Send did not finish — subagents likely ran sequentially and deadlocked on the barrier")
	}

	// Order preservation: tool results answer d1 then d2, with matching content.
	var toolMsgs []proxy.Message
	for _, m := range app.Conv {
		if m.Role == "tool" && m.Name == "dispatch_subagent" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	if len(toolMsgs) != 2 {
		t.Fatalf("want 2 dispatch_subagent tool results, got %d", len(toolMsgs))
	}
	if toolMsgs[0].ToolCallID != "d1" || toolMsgs[1].ToolCallID != "d2" {
		t.Errorf("tool results out of order: got IDs %q, %q — want d1, d2",
			toolMsgs[0].ToolCallID, toolMsgs[1].ToolCallID)
	}
	if !strings.Contains(DerefStr(toolMsgs[0].Content), "TASK-A") {
		t.Errorf("result for d1 should carry TASK-A; got %q", DerefStr(toolMsgs[0].Content))
	}
	if !strings.Contains(DerefStr(toolMsgs[1].Content), "TASK-B") {
		t.Errorf("result for d2 should carry TASK-B; got %q", DerefStr(toolMsgs[1].Content))
	}
	for i, m := range toolMsgs {
		if !m.Pinned {
			t.Errorf("tool result %d not pinned", i)
		}
	}
}

// TestParallelBlockCancellationStubs verifies that cancelling ctx mid-dispatch
// still yields a tool response for every dispatched tool_call_id.
func TestParallelBlockCancellationStubs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if isSubagentRequest(body) {
			cancel() // cancel while subagents are in flight
			<-r.Context().Done()
			return
		}
		writeSSE(w, twoDispatchFrames("TASK-A", "TASK-B")...)
	}))
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	_, _ = app.Send(ctx, "go") // error expected (parent's follow-up stream is cancelled too)

	// Every dispatched tool_call_id must have a tool response in Conv.
	var gotIDs []string
	for _, m := range app.Conv {
		if m.Role == "tool" && m.Name == "dispatch_subagent" {
			gotIDs = append(gotIDs, m.ToolCallID)
		}
	}
	if len(gotIDs) != 2 {
		t.Fatalf("cancelled block must still answer both tool_call_ids; got %v", gotIDs)
	}
	if gotIDs[0] != "d1" || gotIDs[1] != "d2" {
		t.Errorf("stub order: got %v, want [d1 d2]", gotIDs)
	}
}

// TestSemaphoreBoundsParallelism verifies MaxParallelSubagents caps concurrent
// workers: with cap=1 and two jobs, the second worker must not start before
// the first finishes.
func TestSemaphoreBoundsParallelism(t *testing.T) {
	var inFlight, maxInFlight atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if isSubagentRequest(body) {
			cur := inFlight.Add(1)
			for {
				prev := maxInFlight.Load()
				if cur <= prev || maxInFlight.CompareAndSwap(prev, cur) {
					break
				}
			}
			time.Sleep(50 * time.Millisecond) // widen the overlap window
			inFlight.Add(-1)
			writeSSE(w, contentChunk(summaryFor(taskFromBody(body))))
			return
		}
		writeSSE(w, contentChunk("noop"))
	}))
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.MaxParallelSubagents = 1

	jobs := []subagentJob{
		{Index: 0, Task: "TASK-A", ChatID: NewChatID()},
		{Index: 1, Task: "TASK-B", ChatID: NewChatID()},
	}
	results := app.runSubagentJobs(context.Background(), jobs, "")
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	if got := maxInFlight.Load(); got > 1 {
		t.Errorf("cap=1 but %d subagent requests were in flight concurrently", got)
	}
}

// TestParallelBlockConsentDeclineAnswersAll verifies a declined egress gate
// produces a decline summary for every call in the block, in order.
func TestParallelBlockConsentDeclineAnswersAll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeSSE(w, contentChunk("unreachable"))
	}))
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return false })
	app.Cfg.ExternalBackends = []string{"openrouter"}
	app.SelectedBackend = "openrouter"

	block := []proxy.ToolCall{
		{ID: "d1", Function: proxy.FunctionCall{Name: "dispatch_subagent", Arguments: `{"task":"TASK-A"}`}},
		{ID: "d2", Function: proxy.FunctionCall{Name: "dispatch_subagent", Arguments: `{"task":"TASK-B"}`}},
	}
	out := app.runParallelSubagentBlock(context.Background(), block)
	if len(out) != 2 {
		t.Fatalf("want 2 results, got %d", len(out))
	}
	for i, r := range out {
		if !strings.Contains(r, "not consented") {
			t.Errorf("result %d should be a decline summary; got %q", i, r)
		}
	}
}

// TestParallelBlockExhaustionSurfaced verifies that an exhausted subagent in a
// parallel block yields Status:"incomplete" in its slot and the loud ⚠ warning
// is printed once per exhausted child during Phase C.
func TestParallelBlockExhaustionSurfaced(t *testing.T) {
	var mu sync.Mutex
	subCalls := map[string]int{} // per-task call counter

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		task := taskFromBody(body)
		mu.Lock()
		subCalls[task]++
		n := subCalls[task]
		mu.Unlock()
		if n == 1 {
			// First call per subagent: emit a read_file tool call so the
			// iteration counter advances to the forceFinish backstop.
			writeSSE(w, toolCallFrames("r1", "read_file", `{"path":"a.go"}`)...)
			return
		}
		// forceFinish turn: valid JSON despite exhaustion.
		writeSSE(w, contentChunk(summaryFor(task)))
	}))
	defer srv.Close()

	exec := newFakeExecutor()
	exec.files["a.go"] = "content"

	var outBuf strings.Builder
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	app.Out = &outBuf
	app.subMaxToolIter = 1 // force exhaustion in every child

	block := []proxy.ToolCall{
		{ID: "d1", Function: proxy.FunctionCall{Name: "dispatch_subagent", Arguments: `{"task":"TASK-A"}`}},
		{ID: "d2", Function: proxy.FunctionCall{Name: "dispatch_subagent", Arguments: `{"task":"TASK-B"}`}},
	}
	out := app.runParallelSubagentBlock(context.Background(), block)

	for i, r := range out {
		if !strings.Contains(r, `"status":"incomplete"`) {
			t.Errorf("result %d should carry incomplete status; got %q", i, r)
		}
	}
	warnings := strings.Count(outBuf.String(), "ran out of budget")
	if warnings != 2 {
		t.Errorf("want 2 exhaustion warnings (one per child), got %d\noutput: %s", warnings, outBuf.String())
	}
}

// TestBatchToolAggregatesInOrder verifies the dispatch_subagents batch tool:
// one tool_call_id, a JSON array of summaries in task order.
func TestBatchToolAggregatesInOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		writeSSE(w, contentChunk(summaryFor(taskFromBody(body))))
	}))
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	tc := proxy.ToolCall{
		ID:       "batch1",
		Function: proxy.FunctionCall{Name: "dispatch_subagents", Arguments: `{"tasks":["TASK-A","TASK-B"]}`},
	}
	result := app.ExecuteToolCall(context.Background(), tc)

	var arr []string
	if err := json.Unmarshal([]byte(result.text), &arr); err != nil {
		t.Fatalf("batch result is not a JSON array: %v\n%s", err, result.text)
	}
	if len(arr) != 2 {
		t.Fatalf("want 2 aggregated results, got %d", len(arr))
	}
	if !strings.Contains(arr[0], "TASK-A") || !strings.Contains(arr[1], "TASK-B") {
		t.Errorf("aggregated results out of order: [0]=%q [1]=%q", arr[0], arr[1])
	}
}

// TestBatchToolValidation verifies argument validation of dispatch_subagents.
func TestBatchToolValidation(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	cases := []struct {
		args string
		want string
	}{
		{`{}`, "ERROR: tasks is required"},
		{`{"tasks":[]}`, "ERROR: tasks is required"},
		{`{"tasks":["a","b","c","d","e","f","g","h","i"]}`, "too many tasks"},
		{`{"tasks":["ok",""]}`, "task 2 is empty"},
		{`not json`, "could not parse"},
	}
	for _, c := range cases {
		tc := proxy.ToolCall{ID: "b", Function: proxy.FunctionCall{Name: "dispatch_subagents", Arguments: c.args}}
		got := app.ExecuteToolCall(context.Background(), tc)
		if !strings.Contains(got.text, c.want) {
			t.Errorf("args %q: got %q, want substring %q", c.args, got.text, c.want)
		}
	}
}

// TestActiveEventsRespectCap verifies SubagentActiveMsg fires only when a
// worker actually acquires a slot: with cap=1 and two jobs, the second Active
// event must not fire before the first subagent's request completes.
func TestActiveEventsRespectCap(t *testing.T) {
	release := make(chan struct{})
	arrived := make(chan struct{})
	var first sync.Once

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		task := taskFromBody(body)
		// Hold whichever subagent request arrives FIRST (slot acquisition
		// order is nondeterministic); the second proceeds normally.
		hold := false
		first.Do(func() { hold = true; close(arrived) })
		if hold {
			<-release
		}
		writeSSE(w, contentChunk(summaryFor(task)))
	}))
	defer srv.Close()

	var mu sync.Mutex
	var activeIDs []string
	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.MaxParallelSubagents = 1
	app.EventSink = func(msg interface{}) {
		if m, ok := msg.(SubagentActiveMsg); ok {
			mu.Lock()
			activeIDs = append(activeIDs, m.ChatID)
			mu.Unlock()
		}
	}

	jobs := []subagentJob{
		{Index: 0, Task: "TASK-A", ChatID: "chat-a"},
		{Index: 1, Task: "TASK-B", ChatID: "chat-b"},
	}
	done := make(chan []subagentJobResult, 1)
	go func() { done <- app.runSubagentJobs(context.Background(), jobs, "") }()

	<-arrived // the first worker holds the only slot and is blocked in-flight
	mu.Lock()
	if len(activeIDs) != 1 {
		t.Errorf("while slot held: %d active events (%v), want exactly 1", len(activeIDs), activeIDs)
	}
	mu.Unlock()

	close(release)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("jobs did not finish")
	}
	mu.Lock()
	if len(activeIDs) != 2 {
		t.Errorf("after completion: %d active events, want 2 (%v)", len(activeIDs), activeIDs)
	}
	mu.Unlock()
}

// TestWorkerPanicIsolated verifies a panicking worker is converted to an
// error summary and does not crash the parent or its sibling.
func TestWorkerPanicIsolated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if taskFromBody(body) == "TASK-B" {
			writeSSE(w, contentChunk(summaryFor("TASK-B")))
			return
		}
		// TASK-A: nil Client trick is hard to arrange; instead panic via a
		// malformed SSE that the client tolerates — so trigger the panic in
		// the job goroutine directly using a poisoned progress writer.
		writeSSE(w, contentChunk(summaryFor("TASK-A")))
	}))
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	// Poison one worker: a progress writer that panics exercises the recover
	// path inside the worker goroutine. We call runSubagentJobs with a job
	// whose dispatch panics via a nil-Exec App. Use a pointer to avoid
	// copying the mutex (Go vet: lock copy).
	poisoned := &App{
		Cfg:     app.Cfg,
		Client:  app.Client,
		Exec:    nil, // dispatchSubagent calls a.Exec.Cwd() → nil deref panic
		Tools:   app.Tools,
		Out:     app.Out,
		Confirm: app.Confirm,
	}

	jobs := []subagentJob{{Index: 0, Task: "TASK-A", ChatID: NewChatID()}}
	results := poisoned.runSubagentJobs(context.Background(), jobs, "")
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].Summary.Status != "incomplete" {
		t.Errorf("panicked worker should yield incomplete summary; got %+v", results[0].Summary)
	}
	found := false
	for _, f := range results[0].Summary.Findings {
		if strings.Contains(f.Summary, "panic") {
			found = true
		}
	}
	if !found {
		t.Error("panic not surfaced in findings")
	}
	_ = io.Discard
}
