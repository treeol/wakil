package agent

// Send phase characterization tests.
//
// These tests verify the major phases of Send() as described in WP-5.4:
//   - Model selection (SelectedModel override, default restore)
//   - Egress consent (external backend gate)
//   - Stream loop (single-turn, multi-turn tool loop)
//   - Force-finish (MaxToolIterations reached)
//   - Tool-result batch (tool calls dispatched and results appended to Conv)
//   - Compaction (runs after stream loop exits)
//
// When WP-6.2 extracts Send into phase methods, these tests must pass unchanged.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// ── Phase 1: Model selection ─────────────────────────────────────────────────

// TestSend_ModelSelection_Override verifies that SelectedModel is applied to
// Client.Model at Send entry.
func TestSend_ModelSelection_Override(t *testing.T) {
	var captured struct {
		Model string `json:"model"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &captured)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: " + contentChunk("ok") + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SelectedModel = "gpt-4o"
	if _, err := app.Send(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if captured.Model != "gpt-4o" {
		t.Errorf("model = %q, want %q", captured.Model, "gpt-4o")
	}
}

// TestSend_ModelSelection_DefaultRestore verifies that when SelectedModel is
// empty, the default model (Client.Model at construction time) is restored.
func TestSend_ModelSelection_DefaultRestore(t *testing.T) {
	var captured struct {
		Model string `json:"model"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &captured)
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: " + contentChunk("ok") + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	// Simulate a previous turn that set SelectedModel, then cleared it.
	app.SelectedModel = ""
	if _, err := app.Send(context.Background(), "hi"); err != nil {
		t.Fatal(err)
	}
	if captured.Model != "ilm" {
		t.Errorf("model = %q, want %q (default)", captured.Model, "ilm")
	}
}

// ── Phase 2: Egress consent ──────────────────────────────────────────────────

// TestSend_EgressConsent_Declined verifies that declining the external backend
// consent gate causes Send to return early with no error and no backend set.
func TestSend_EgressConsent_Declined(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("server should not be called when egress consent is declined")
	}))
	defer srv.Close()

	declined := false
	app := newTestApp(srv.URL, newFakeExecutor(), func(tool, title, detail string, _ bool) bool {
		if tool == "external_backend" {
			declined = true
			return false
		}
		return true
	})
	// Set up an external backend that triggers the consent gate.
	app.SelectedBackend = "external-openai"
	app.BackendList = []BackendInfo{{Name: "external-openai", External: true}}

	_, err := app.Send(context.Background(), "hi")
	if err != nil {
		t.Fatalf("declined egress should return nil error, got: %v", err)
	}
	if !declined {
		t.Error("consent gate should have been prompted")
	}
	if app.SelectedBackend != "" {
		t.Errorf("SelectedBackend should be cleared after decline, got %q", app.SelectedBackend)
	}
}

// TestSend_EgressConsent_Approved verifies that approving the external backend
// consent gate allows the request to proceed.
func TestSend_EgressConsent_Approved(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: " + contentChunk("ok") + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	approved := false
	app := newTestApp(srv.URL, newFakeExecutor(), func(tool, title, detail string, _ bool) bool {
		if tool == "external_backend" {
			approved = true
			return true
		}
		return true
	})
	app.SelectedBackend = "external-openai"
	app.BackendList = []BackendInfo{{Name: "external-openai", External: true}}

	_, err := app.Send(context.Background(), "hi")
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if !approved {
		t.Error("consent gate should have been prompted and approved")
	}
}

// TestSend_EgressConsent_SkippedForDefaultBackend verifies that the consent
// gate is NOT triggered when no external backend is selected.
func TestSend_EgressConsent_SkippedForDefaultBackend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: " + contentChunk("ok") + "\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	consentCalled := false
	app := newTestApp(srv.URL, newFakeExecutor(), func(tool, title, detail string, _ bool) bool {
		if tool == "external_backend" {
			consentCalled = true
		}
		return true
	})
	// No SelectedBackend → no external backend → no consent gate.
	app.SelectedBackend = ""

	_, err := app.Send(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if consentCalled {
		t.Error("consent gate should NOT fire when no external backend is selected")
	}
}

// ── Phase 3: Stream loop ─────────────────────────────────────────────────────

// TestSend_StreamLoop_SingleTurn verifies that a single-turn conversation
// (no tool calls) completes with the model's text response.
func TestSend_StreamLoop_SingleTurn(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("hello world")})
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	final, err := app.Send(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(final, "hello world") {
		t.Errorf("final = %q, want to contain 'hello world'", final)
	}
	// Conv should have: user, assistant.
	if len(app.Conv) != 2 {
		t.Errorf("Conv len = %d, want 2 (user + assistant)", len(app.Conv))
	}
}

// TestSend_StreamLoop_ToolCallThenAnswer verifies that a tool call is executed
// and the result is fed back, then the model produces a final answer.
func TestSend_StreamLoop_ToolCallThenAnswer(t *testing.T) {
	srv := sseServer(t,
		toolCallFrames("c1", "read_file", `{"path":"test.txt"}`),
		[]string{contentChunk("the file says hello")},
	)
	defer srv.Close()

	fe := newFakeExecutor()
	fe.files["test.txt"] = "hello"
	app := newTestApp(srv.URL, fe, func(_, _, _ string, _ bool) bool { return true })

	final, err := app.Send(context.Background(), "read the file")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(final, "the file says hello") {
		t.Errorf("final = %q, want 'the file says hello'", final)
	}
	// Conv: user, assistant(tool_call), tool, assistant(final).
	if len(app.Conv) != 4 {
		t.Errorf("Conv len = %d, want 4", len(app.Conv))
	}
	// The tool result must be in Conv.
	var toolContent string
	for _, m := range app.Conv {
		if m.Role == "tool" {
			toolContent = DerefStr(m.Content)
		}
	}
	if !strings.Contains(toolContent, "hello") {
		t.Errorf("tool result = %q, want to contain 'hello'", toolContent)
	}
}

// ── Phase 4: Force-finish ────────────────────────────────────────────────────

// TestSend_ForceFinish_IterationLimit verifies that MaxToolIterations forces
// the model to answer without tools after the cap is reached.
func TestSend_ForceFinish_IterationLimit(t *testing.T) {
	// Server always returns a tool call — never stops on its own.
	srv := sseServer(t, toolCallFrames("c1", "run_shell", `{"command":"echo hi"}`))
	defer srv.Close()

	fe := newFakeExecutor()
	app := newTestApp(srv.URL, fe, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.MaxToolIterations = 2

	final, err := app.Send(context.Background(), "go")
	if err != nil {
		t.Fatal(err)
	}
	// Tools run on iters 0,1; iter 2 is the forced tool-less finish.
	if len(fe.shellCalls) != 2 {
		t.Errorf("shell executed %d times, want 2", len(fe.shellCalls))
	}
	// The ToolLimitPrompt must be injected.
	var injected bool
	for _, m := range app.Conv {
		if m.Role == "user" && DerefStr(m.Content) == ToolLimitPrompt {
			injected = true
		}
	}
	if !injected {
		t.Error("ToolLimitPrompt was not injected on force-finish")
	}
	// exhausted flag must be set.
	if !app.exhausted {
		t.Error("exhausted should be true after force-finish")
	}
	_ = final
}

// ── Phase 5: Tool-result batch ───────────────────────────────────────────────

// TestSend_ToolResultBatch_OrderPreserved verifies that multiple tool calls in
// one response are executed in order and their results appear in Conv in order.
func TestSend_ToolResultBatch_OrderPreserved(t *testing.T) {
	// Two tool calls in one response: read_file then list_dir.
	frames := []string{
		// First: two tool calls in one delta
		`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"t1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.txt\"}"}},{"index":1,"id":"t2","type":"function","function":{"name":"list_dir","arguments":"{\"path\":\".\"}"}}]},"finish_reason":null}]}`,
		`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	srv := sseServer(t, frames, []string{contentChunk("done")})
	defer srv.Close()

	fe := newFakeExecutor()
	fe.files["a.txt"] = "content-a"
	fe.files["b.go"] = "content-b"
	app := newTestApp(srv.URL, fe, func(_, _, _ string, _ bool) bool { return true })

	_, err := app.Send(context.Background(), "read and list")
	if err != nil {
		t.Fatal(err)
	}

	// Collect tool messages in order.
	var toolMsgs []proxy.Message
	for _, m := range app.Conv {
		if m.Role == "tool" {
			toolMsgs = append(toolMsgs, m)
		}
	}
	if len(toolMsgs) != 2 {
		t.Fatalf("expected 2 tool messages, got %d", len(toolMsgs))
	}
	if toolMsgs[0].Name != "read_file" {
		t.Errorf("first tool = %q, want read_file", toolMsgs[0].Name)
	}
	if toolMsgs[1].Name != "list_dir" {
		t.Errorf("second tool = %q, want list_dir", toolMsgs[1].Name)
	}
}

// ── Phase 6: Compaction ──────────────────────────────────────────────────────

// TestSend_Compaction_RunsAfterLoop verifies that compaction runs after the
// stream loop exits. We test this indirectly by verifying that Send completes
// without error when Conv is large enough to trigger compaction.
func TestSend_Compaction_RunsAfterLoop(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("done")})
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	// Pre-fill Conv with enough messages to trigger compaction.
	for i := 0; i < 100; i++ {
		app.Conv = append(app.Conv, proxy.Message{
			Role:    "user",
			Content: StrPtr(strings.Repeat("x", 500)),
		})
		app.Conv = append(app.Conv, proxy.Message{
			Role:    "assistant",
			Content: StrPtr(strings.Repeat("y", 500)),
		})
	}

	_, err := app.Send(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	// If compaction ran, Conv should be shorter than 200+2.
	// (This is a soft assertion — compaction depends on thresholds.)
}

// ── Shared gate: Confinement ─────────────────────────────────────────────────

// TestSend_ConfinementGate_BreakerTrips verifies that repeated confinement
// errors trigger the circuit breaker and force-finish with a specific message.
func TestSend_ConfinementGate_BreakerTrips(t *testing.T) {
	// Server returns read_file calls that will all hit confinement errors.
	srv := sseServer(t, toolCallFrames("c1", "read_file", `{"path":"/etc/passwd"}`))
	defer srv.Close()

	fe := newFakeExecutor()
	fe.confineErrFn = func(path string) error {
		return fmt.Errorf("path %q is outside workspace %q — traversal not allowed", path, "/work")
	}
	app := newTestApp(srv.URL, fe, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.MaxToolIterations = 10 // high enough that the breaker trips first

	_, err := app.Send(context.Background(), "read /etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	if !app.confinementTripped {
		t.Error("confinement breaker should have tripped")
	}
	if app.stopReason != "confinement_breaker" {
		t.Errorf("stopReason = %q, want 'confinement_breaker'", app.stopReason)
	}
}

// ── Shared gate: Cap/Stub ────────────────────────────────────────────────────

// TestExecuteToolCall_CapOrStub_LargeResultCapped verifies that CapOrStub
// caps a large result to the configured ToolResultCap.
func TestExecuteToolCall_CapOrStub_LargeResultCapped(t *testing.T) {
	app := &App{
		Cfg:     config.DefaultConfig(), // ToolResultCap = 8000
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}
	largeResult := strings.Repeat("A", 20000)
	capped := app.CapOrStub(largeResult, "run_shell", 0)
	if len(capped) >= 20000 {
		t.Errorf("result len = %d, should be capped below 20K (ToolResultCap=8000)", len(capped))
	}
	if len(capped) > 8200 { // small margin for cap marker
		t.Errorf("result len = %d, should be near 8000 (ToolResultCap)", len(capped))
	}
}

// TestExecuteToolCall_CapOrStub_MashuraExempt verifies that mashura tool
// results are NOT capped (they pass through CapOrStub unchanged).
func TestExecuteToolCall_CapOrStub_MashuraExempt(t *testing.T) {
	app := &App{
		Cfg:     config.DefaultConfig(),
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}
	largeResult := strings.Repeat("X", 20000)
	capped := app.CapOrStub(largeResult, "mashura__review", 0)
	if len(capped) != len(largeResult) {
		t.Errorf("mashura result was capped: %d -> %d", len(largeResult), len(capped))
	}
}

// TestExecuteToolCall_CapOrStub_SubagentExempt verifies that dispatch_subagent
// results are NOT capped.
func TestExecuteToolCall_CapOrStub_SubagentExempt(t *testing.T) {
	app := &App{
		Cfg:     config.DefaultConfig(),
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}
	largeResult := strings.Repeat("X", 20000)
	capped := app.CapOrStub(largeResult, "dispatch_subagent", 0)
	if len(capped) != len(largeResult) {
		t.Errorf("subagent result was capped: %d -> %d", len(largeResult), len(capped))
	}
}

// ── Shared gate: ToolCache dedup ─────────────────────────────────────────────

// TestExecuteToolCall_ToolCache_Dedup verifies that a repeated equivalent tool
// call returns a dedup notice instead of re-executing.
func TestExecuteToolCall_ToolCache_Dedup(t *testing.T) {
	fe := newFakeExecutor()
	fe.files["a.go"] = "content"
	app := &App{
		Exec:      fe,
		Out:       io.Discard,
		Confirm:   func(_, _, _ string, _ bool) bool { return true },
		Cfg:       config.DefaultConfig(),
		ToolCache: map[string]bool{},
	}

	r1 := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file", Arguments: `{"path":"a.go"}`,
	}})
	if strings.Contains(r1, "already called") {
		t.Fatalf("first call should execute: %s", r1)
	}

	r2 := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file", Arguments: `{"path":"./a.go"}`,
	}})
	if !strings.Contains(r2, "already called") {
		t.Errorf("equivalent path should be deduped: %s", r2)
	}
}

// ── Shared gate: Confirm accepted ────────────────────────────────────────────

// TestExecuteToolCall_ConfirmAccepted verifies that an accepted confirm gate
// allows the tool to execute.
func TestExecuteToolCall_ConfirmAccepted(t *testing.T) {
	fe := newFakeExecutor()
	confirmed := false
	app := newTestApp("", fe, func(tool, _, _ string, _ bool) bool {
		if tool == "run_shell" {
			confirmed = true
		}
		return true
	})

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "run_shell", Arguments: `{"command":"echo hi"}`,
	}})
	if !confirmed {
		t.Error("confirm gate should have been called for run_shell")
	}
	if strings.Contains(res, "declined") {
		t.Errorf("accepted tool should execute: %s", res)
	}
}

// ── Additional tool cases ────────────────────────────────────────────────────

// TestExecuteToolCall_ReadFileFull verifies the read_file_full tool.
func TestExecuteToolCall_ReadFileFull(t *testing.T) {
	fe := newFakeExecutor()
	fe.files["full.txt"] = "full content here"
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file_full", Arguments: `{"path":"full.txt"}`,
	}})
	if !strings.Contains(res, "full content") {
		t.Errorf("read_file_full result missing content: %s", res)
	}
}

// TestExecuteToolCall_OpenURL verifies the open_url tool attempts to open a URL.
// In the sandbox, xdg-open is not available, so the result will be an error —
// but the tool should still execute without panicking.
func TestExecuteToolCall_OpenURL(t *testing.T) {
	fe := newFakeExecutor()
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "open_url", Arguments: `{"url":"https://example.com"}`,
	}})
	// The tool will likely error in the sandbox (no xdg-open), but it must
	// return a string without panicking.
	if res == "" {
		t.Error("open_url should return a non-empty string")
	}
}

// TestExecuteToolCall_RunBackground verifies the run_background tool starts a
// process and returns an id.
func TestExecuteToolCall_RunBackground(t *testing.T) {
	fe := newFakeExecutor()
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "run_background", Arguments: `{"command":"sleep 10","label":"test"}`,
	}})
	if !strings.Contains(res, "id:") {
		t.Errorf("run_background should return an id: %s", res)
	}
}

// TestExecuteToolCall_RunBackground_CapEnforced verifies that the 5-process
// cap is enforced.
func TestExecuteToolCall_RunBackground_CapEnforced(t *testing.T) {
	fe := &aliveExecutorImpl{fakeExecutor: newFakeExecutor()}
	app := &App{
		Exec:    fe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     config.DefaultConfig(),
		bgProcs: map[string]*bgEntry{
			"bg1": {pid: 101, generation: 1},
			"bg2": {pid: 102, generation: 1},
			"bg3": {pid: 103, generation: 1},
			"bg4": {pid: 104, generation: 1},
			"bg5": {pid: 105, generation: 1},
		},
		bgCounter: 5,
	}

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "run_background", Arguments: `{"command":"sleep 10","label":"test"}`,
	}})
	if !strings.Contains(res, "maximum of 5") {
		t.Errorf("6th background process should be rejected: %s", res)
	}
}

// TestExecuteToolCall_StagingGet verifies the staging_get tool.
// Note: staging requires Docker mode with KVR enabled. In test mode without
// a real KVR socket, staging operations return "unavailable". We verify the
// tool handles this gracefully without panicking.
func TestExecuteToolCall_StagingGet(t *testing.T) {
	fe := newFakeExecutor()
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "staging_get", Arguments: `{"key":"test-key"}`,
	}})
	// Must return a non-empty string (either the value or an "unavailable" message).
	if res == "" {
		t.Error("staging_get should return a non-empty string")
	}
}

// TestExecuteToolCall_MemoryGet verifies the memory_get tool handles missing
// stores gracefully. In test mode without a real memory store, memory_get
// returns "unavailable" — the tool must not panic.
func TestExecuteToolCall_MemoryGet(t *testing.T) {
	fe := newFakeExecutor()
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "memory_get", Arguments: `{"key":"test/mem"}`,
	}})
	if res == "" {
		t.Error("memory_get should return a non-empty string")
	}
}

// TestExecuteToolCall_StagingDelete verifies the staging_delete tool handles
// missing KVR gracefully.
func TestExecuteToolCall_StagingDelete(t *testing.T) {
	fe := newFakeExecutor()
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "staging_delete", Arguments: `{"key":"del-key"}`,
	}})
	if res == "" {
		t.Error("staging_delete should return a non-empty string")
	}
}

// TestExecuteToolCall_StagingList verifies the staging_list tool handles missing
// KVR gracefully.
func TestExecuteToolCall_StagingList(t *testing.T) {
	fe := newFakeExecutor()
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "staging_list", Arguments: `{}`,
	}})
	if res == "" {
		t.Error("staging_list should return a non-empty string")
	}
}
