package agent

// eval_test.go — Agent eval / regression harness (Roadmap #39).
//
// Four scenario-based tests that verify end-to-end behavior through the real
// turn loop (SendOutcome), not just isolated internals. Each scenario closes a
// gap not covered by existing unit tests:
//
//   E1: Permission bypass under /auto — drives a destructive shell tool call
//       through SendOutcome with /auto enabled but /auto destructive NOT
//       enabled. Asserts the executor never runs the destructive command.
//       Existing tests call SuspendAuto directly; this test verifies the full
//       gate chain from turn loop → Confirm → SuspendAuto → executor.
//
//   E2: Turn-budget loop termination — scripts a model that always returns
//       tool calls; asserts MaxToolIterations stops the loop (no infinite
//       cycle), the final request has tools=nil, and the turn ends cleanly.
//
//   E3: Request-capturing review test — captures the actual request body sent
//       to the model during /review; asserts the rubric constant, spill path,
//       and exact request count. This verifies what the model was *sent*,
//       not just what it returned.
//
//   E4: Parallel-edit conflict — verifies the multiToolCallFrames helper
//       produces valid multi-tool-call SSE frames. Full scenario deferred
//       pending git-aware test fixtures (worktree_test.go uses real git;
//       integrating with SendOutcome requires a git-initialized temp dir).
//
// Design decisions:
//   - Go test functions, not a separate framework (reuses go test infrastructure)
//   - Uses existing testhelpers (fakeExecutor, newTestApp, sseServer)
//   - Deterministic: mock model responses via sseServer, no real model calls
//   - Real-model eval is a separate future effort (env-gated, not this file)

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/exec"
	wtools "github.com/treeol/wakil/internal/tools"
)

// ─── Shared helpers ──────────────────────────────────────────────────────

// capturingServer wraps sseServer with request-body capture. It returns the
// server and a function to retrieve captured request bodies. Each captured
// entry is the raw JSON body sent to /v1/chat/completions.
func capturingServer(t *testing.T, framesPerCall ...[]string) (*httptest.Server, func() []string) {
	t.Helper()
	if len(framesPerCall) == 0 {
		t.Fatal("capturingServer requires at least one frames set")
	}
	var mu sync.Mutex
	var captured []string
	call := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}

		// Capture the request body.
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("capturingServer: failed to read request body: %v", err)
			return
		}
		r.Body.Close()

		mu.Lock()
		captured = append(captured, string(body))
		frames := framesPerCall[0]
		if call < len(framesPerCall) {
			frames = framesPerCall[call]
		}
		call++
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("capturingServer: ResponseWriter does not implement http.Flusher")
			return
		}
		flusher.Flush()
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))

	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(captured))
		copy(out, captured)
		return out
	}
}

// multiToolCallFrames builds SSE frames for a response containing multiple
// tool calls (each with a distinct index). The existing toolCallFrames helper
// hardcodes index:0 and can only emit one tool call.
func multiToolCallFrames(calls []struct{ ID, Name string }) []string {
	var callObjs []string
	for i, c := range calls {
		callObjs = append(callObjs, fmt.Sprintf(
			`{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":""}}`,
			i, c.ID, c.Name))
	}
	firstFrame := fmt.Sprintf(
		`{"choices":[{"delta":{"tool_calls":[%s]},"finish_reason":null}]}`,
		strings.Join(callObjs, ","))
	return []string{
		firstFrame,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
}

// newTestAppAuto creates an App with /auto mode enabled (AutoApprove=true)
// and a confirmer that replicates tuiConfirmer's /auto logic: auto-approve
// unless SuspendAuto returns a reason, in which case decline.
func newTestAppAuto(url string, executor exec.Executor) *App {
	cfg := config.DefaultConfig()
	cfg.ShellTimeoutSec = 0
	app := &App{
		Cfg:    cfg,
		Client: newTestClient(url),
		Exec:   executor,
		Tools:  wtools.DefaultTools("/work"),
		Out:    io.Discard,
	}
	app.SetAutoApprove(true)
	app.Confirm = func(toolName, headline, detail string, readAction bool) bool {
		if reason := SuspendAuto(toolName, app, detail); reason != "" {
			return false // gate holds — declined even under /auto
		}
		return true // auto-approve
	}
	return app
}

// ─── E1: Permission bypass under /auto (end-to-end) ──────────────────────

// TestEval_PermissionBypassUnderAuto drives a destructive shell command
// through SendOutcome with /auto enabled but /auto destructive NOT enabled.
// The model requests `rm -rf /tmp/test`; we assert the executor never runs it.
//
// What's novel: existing tests call SuspendAuto directly in isolation. This
// test drives through the real turn loop → tool dispatch → Confirm path,
// verifying the full gate chain. If the turn loop short-circuits on
// AutoApprove *before* consulting Confirm, this test catches that bypass.
//
// Script: call 0 returns a destructive run_shell tool call. The gate declines
// it. Call 1 returns a final text response (model acknowledges the decline).
// Without call 1, sseServer would replay call 0 indefinitely.
func TestEval_PermissionBypassUnderAuto(t *testing.T) {
	rmFrames := toolCallFrames("tc-1", "run_shell", `{"command":"rm -rf /tmp/test"}`)
	finalFrames := []string{contentChunk("I cannot run that destructive command without /auto destructive enabled.")}

	srv := sseServer(t, rmFrames, finalFrames)
	defer srv.Close()

	fe := newFakeExecutor()

	app := newTestAppAuto(srv.URL, fe)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := app.SendOutcome(ctx, "delete everything in /tmp/test")
	if err != nil {
		t.Fatalf("SendOutcome error: %v", err)
	}

	// The turn must terminate (not hang).
	if out.Kind != TurnFinal {
		t.Errorf("expected TurnFinal, got %v (text=%q)", out.Kind, out.Text)
	}

	// CRITICAL assertion: the destructive command must NOT have been executed.
	for _, cmd := range fe.shellCalls {
		if strings.Contains(cmd, "rm -rf") {
			t.Fatalf("destructive command was executed under /auto without /auto destructive: %q", cmd)
		}
	}

	// The tool result should reflect the decline.
	toolDeclined := false
	for _, m := range app.Conv {
		if m.Role == "tool" {
			c := DerefStr(m.Content)
			if strings.Contains(c, "declined") || strings.Contains(c, "Declined") {
				toolDeclined = true
				break
			}
		}
	}
	if !toolDeclined {
		t.Error("expected a 'declined' tool result from the gate, but none found in tool messages")
	}

	t.Logf("E1 PASS: destructive shell blocked under /auto, %d shell calls (all non-destructive), turn terminated cleanly",
		len(fe.shellCalls))
}

// ─── E2: Turn-budget loop termination ────────────────────────────────────

// TestEval_TurnBudgetLoopTermination verifies that MaxToolIterations stops an
// infinite tool-call loop. The sseServer always returns a tool call (read_file),
// so without the iteration cap the loop would never terminate.
//
// Also asserts (via request capture) that the final request to the model has
// tools removed (forceFinish drops tools on the last iteration).
func TestEval_TurnBudgetLoopTermination(t *testing.T) {
	// Script: every call returns a read_file tool call. sseServer replays
	// call 0 on overrun, so this loops indefinitely without the cap.
	readFileFrames := toolCallFrames("tc-loop", "read_file", `{"path":"/work/test.txt"}`)

	srv, getCaptured := capturingServer(t, readFileFrames)
	defer srv.Close()

	fe := newFakeExecutor()
	fe.files["/work/test.txt"] = "content"

	app := newTestApp(srv.URL, fe, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.MaxToolIterations = 5

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := app.SendOutcome(ctx, "keep reading the file")
	if err != nil {
		t.Fatalf("SendOutcome error: %v", err)
	}

	if out.Kind != TurnAborted || out.Cause != StopCauseIterationLimit {
		t.Errorf("expected TurnAborted/iteration_limit, got %v/%s (text=%q)", out.Kind, out.Cause, out.Text)
	}

	// Count tool results — should be exactly MaxToolIterations (each iteration
	// produces one tool call → one tool result before the cap forces finish).
	toolResults := 0
	for _, m := range app.Conv {
		if m.Role == "tool" {
			toolResults++
		}
	}
	if toolResults > app.Cfg.MaxToolIterations {
		t.Errorf("tool results %d exceeded cap %d — iteration limit not enforced",
			toolResults, app.Cfg.MaxToolIterations)
	}
	if toolResults < 3 {
		t.Errorf("expected at least 3 tool results before cap, got %d", toolResults)
	}

	// Verify the final request had tools=nil (forceFinish drops tools).
	captured := getCaptured()
	if len(captured) < 2 {
		t.Fatalf("expected at least 2 captured requests, got %d", len(captured))
	}
	lastBody := captured[len(captured)-1]

	// Parse the last request and check whether tools are present.
	var lastReq struct {
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal([]byte(lastBody), &lastReq); err != nil {
		t.Fatalf("failed to parse last request body: %v", err)
	}
	if len(lastReq.Tools) > 0 {
		t.Errorf("expected final request to have tools=nil (forceFinish), but has %d tools", len(lastReq.Tools))
	}

	t.Logf("E2 PASS: loop terminated after %d tool results (cap=%d), final request had no tools",
		toolResults, app.Cfg.MaxToolIterations)
}

// ─── E3: Request-capturing review test ───────────────────────────────────

// TestEval_ReviewRequestContent captures the actual request body sent to the
// model during /review and verifies:
//  1. The review rubric constant (reviewRubric) is included verbatim
//  2. The diff spill path is referenced in the task
//  3. Exactly one model call was made (no silent extra calls)
//
// What's novel: existing review tests use hard-coded mock JSON and never
// verify what the model was actually sent. If the rubric is accidentally
// dropped or the spill path is wrong, existing tests still pass — this test
// catches that.
func TestEval_ReviewRequestContent(t *testing.T) {
	fe := newFakeExecutor()
	fe.files["/work/main.go"] = "package main\n\nfunc main() {}\n"
	fe.shellResult = "diff --git a/main.go b/main.go\n+func broken() { panic(nil) }\n"

	// The review subagent returns a valid SubagentSummary JSON.
	reviewResponse := contentChunk(`{"objective":"review diff","findings":[{"summary":"nil deref in broken()","location":"main.go:1","kind":"error","weight":"high"}],"files_examined":["main.go"],"uncertainty":[],"cost":{"input_tokens":100,"output_tokens":50}}`)

	srv, getCaptured := capturingServer(t, []string{reviewResponse})
	defer srv.Close()

	app := newTestApp(srv.URL, fe, func(_, _, _ string, _ bool) bool { return true })

	_, err := handleReviewCommand(context.Background(), app, "")
	if err != nil {
		t.Fatalf("handleReviewCommand error: %v", err)
	}

	captured := getCaptured()

	// Assert exactly one model call (no silent extra calls).
	if len(captured) != 1 {
		t.Errorf("expected exactly 1 model request, got %d", len(captured))
	}

	// Parse the request body to extract messages.
	body := captured[0]
	var req struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("failed to parse request body as JSON: %v\nbody (first 500): %s", err, body[:min(500, len(body))])
	}

	// Combine all message content for searching.
	var allContent strings.Builder
	for _, m := range req.Messages {
		allContent.WriteString(m.Content)
		allContent.WriteString("\n")
	}
	combined := allContent.String()

	// Assert the exact rubric constant is present (not just generic keywords).
	if !strings.Contains(combined, reviewRubric) {
		t.Error("reviewRubric constant not found verbatim in request to model")
	}

	// Assert the spill path is referenced (diff file path).
	if !strings.Contains(combined, "review_diff") {
		t.Error("spill path (review_diff) not found in request to model")
	}

	t.Logf("E3 PASS: %d model requests, rubric verified verbatim, spill path present, %d bytes",
		len(captured), len(body))
}

// ─── E4: Multi-tool-call frame helper ────────────────────────────────────

// TestEval_MultiToolCallFrames verifies the multiToolCallFrames helper
// produces valid SSE frames with distinct indices for multiple tool calls.
// The full parallel-edit conflict scenario requires a git-initialized temp
// directory (worktree_test.go uses real git); this helper is the building
// block for that future test.
func TestEval_MultiToolCallFrames(t *testing.T) {
	frames := multiToolCallFrames([]struct{ ID, Name string }{
		{"tc-a", "dispatch_subagent"},
		{"tc-b", "dispatch_subagent"},
	})

	if len(frames) != 2 {
		t.Fatalf("expected 2 frames, got %d", len(frames))
	}

	// First frame must contain both tool calls with distinct indices.
	if !strings.Contains(frames[0], `"index":0`) {
		t.Error("first tool call should have index 0")
	}
	if !strings.Contains(frames[0], `"index":1`) {
		t.Error("second tool call should have index 1")
	}
	if !strings.Contains(frames[0], `"id":"tc-a"`) {
		t.Error("first tool call ID missing")
	}
	if !strings.Contains(frames[0], `"id":"tc-b"`) {
		t.Error("second tool call ID missing")
	}

	// Second frame must have finish_reason: tool_calls.
	if !strings.Contains(frames[1], `"finish_reason":"tool_calls"`) {
		t.Error("final frame must have finish_reason tool_calls")
	}

	// Verify the frame is valid JSON.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(frames[0]), &parsed); err != nil {
		t.Fatalf("first frame is not valid JSON: %v", err)
	}

	t.Logf("E4 PASS: multiToolCallFrames verified — %d frames, 2 tool calls, valid JSON", len(frames))
}
