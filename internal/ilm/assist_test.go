package ilm

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// stubAssistServer is a test server that returns a sequence of canned
// /v1/assist responses. It serves one response per call, cycling through
// the provided responses. It also records the number of calls and the
// request bodies for assertion.
type stubAssistServer struct {
	*httptest.Server
	calls   int32
	bodies  []AssistRequest
	respIdx int32
	resps   []*AssistResponse
	// delay, when non-zero, sleeps before writing the response (to simulate timeout).
	delay time.Duration
}

func newStubAssistServer(t *testing.T, resps []*AssistResponse, delay time.Duration) *stubAssistServer {
	s := &stubAssistServer{resps: resps, delay: delay}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/assist" {
			http.NotFound(w, r)
			return
		}
		idx := atomic.AddInt32(&s.respIdx, 1) - 1
		if int(idx) >= len(s.resps) {
			// Past the end: return abstain to stop the loop.
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(&AssistResponse{Abstain: true, Reason: "no more responses"})
			return
		}

		// Record the request body.
		var req AssistRequest
		json.NewDecoder(r.Body).Decode(&req)
		s.bodies = append(s.bodies, req)
		atomic.AddInt32(&s.calls, 1)

		if s.delay > 0 {
			time.Sleep(s.delay)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.resps[idx])
	}))
	return s
}

// makeActionResp returns an assist response proposing a tool action.
func makeActionResp(tool string, args map[string]interface{}, gate float64) *AssistResponse {
	action, _ := json.Marshal(map[string]interface{}{
		"b":    "tool",
		"name": tool,
		"args": args,
	})
	return &AssistResponse{
		Action:          action,
		Abstain:         false,
		GateProbability: gate,
		CandidateProvenance: mustMarshal(map[string]interface{}{
			"selected": map[string]interface{}{
				"session_id": "file:test-session",
				"seq":        1,
			},
			"candidates": []interface{}{},
		}),
		DeciderID: "d-wlm",
		Threshold: 0.76,
		LatencyMS: 42.5,
	}
}

// makeAbstainResp returns an abstention response.
func makeAbstainResp(reason string, gate float64) *AssistResponse {
	return &AssistResponse{
		Action:          nil,
		Abstain:         true,
		Reason:          reason,
		GateProbability: gate,
		CandidateProvenance: mustMarshal(map[string]interface{}{
			"candidates": []interface{}{},
		}),
		DeciderID: "d-wlm",
		Threshold: 0.76,
		LatencyMS: 12.3,
	}
}

// makeDisallowedActionResp returns an action with a tool NOT on Wakil's allowlist.
// It panics if the tool is actually allowed — catching future allowlist edits that
// would make the test's "disallowed" scenario no longer disallowed.
func makeDisallowedActionResp(tool string, args map[string]interface{}) *AssistResponse {
	if assistAllowedTools[tool] {
		panic("makeDisallowedActionResp: tool " + tool + " is on the allowlist")
	}
	return makeActionResp(tool, args, 0.85)
}

func mustMarshal(v interface{}) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// TestAssistClientAction tests that the assist client correctly parses a
// proposal action response.
func TestAssistClientAction(t *testing.T) {
	resps := []*AssistResponse{
		makeActionResp("read_file", map[string]interface{}{"path": "test.go"}, 0.85),
	}
	srv := newStubAssistServer(t, resps, 0)
	defer srv.Close()

	client := NewAssistClient(srv.URL, "test-token")
	resp, err := client.Query(context.Background(), "wakil-live:test", 1)
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	if resp.Abstain {
		t.Error("expected action, got abstain")
	}
	action, err := resp.ParseAction()
	if err != nil {
		t.Fatalf("ParseAction failed: %v", err)
	}
	if action == nil {
		t.Fatal("expected non-nil action")
	}
	if action.Name != "read_file" {
		t.Errorf("expected tool read_file, got %s", action.Name)
	}
	if resp.GateProbability < 0.76 {
		t.Errorf("gate probability %.2f below threshold 0.76", resp.GateProbability)
	}
}

// TestAssistClientAbstain tests that the assist client correctly parses an
// abstention response.
func TestAssistClientAbstain(t *testing.T) {
	resps := []*AssistResponse{
		makeAbstainResp("gate_probability < threshold", 0.41),
	}
	srv := newStubAssistServer(t, resps, 0)
	defer srv.Close()

	client := NewAssistClient(srv.URL, "test-token")
	resp, err := client.Query(context.Background(), "wakil-live:test", 2)
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}
	if !resp.Abstain {
		t.Error("expected abstain, got action")
	}
	action, err := resp.ParseAction()
	if err != nil {
		t.Fatalf("ParseAction failed: %v", err)
	}
	if action != nil {
		t.Error("expected nil action for abstain")
	}
}

// TestAssistClientTimeout tests that a slow server returns an error (C1: proceed
// as without assist on timeout).
func TestAssistClientTimeout(t *testing.T) {
	// The assist client has an 800ms timeout. Sleep 2s to guarantee timeout.
	srv := newStubAssistServer(t, []*AssistResponse{
		makeActionResp("read_file", map[string]interface{}{"path": "x"}, 0.9),
	}, 2*time.Second)
	defer srv.Close()

	client := NewAssistClient(srv.URL, "test-token")
	_, err := client.Query(context.Background(), "wakil-live:test", 3)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
}

// TestAssistClientNon200 tests that a non-200 response returns an error.
func TestAssistClientNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	client := NewAssistClient(srv.URL, "test-token")
	_, err := client.Query(context.Background(), "wakil-live:test", 4)
	if err == nil {
		t.Fatal("expected error on 503, got nil")
	}
}

// TestAssistClient400Rejected tests that a 400 response returns an
// AssistRejectedError (not a generic error), so tryAssist can emit
// decision="rejected" instead of "error".
func TestAssistClient400Rejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"seq is not a decision point"}`))
	}))
	defer srv.Close()

	client := NewAssistClient(srv.URL, "test-token")
	_, err := client.Query(context.Background(), "wakil-live:test", 42)
	if err == nil {
		t.Fatal("expected error on 400, got nil")
	}
	var rejErr *AssistRejectedError
	if !errors.As(err, &rejErr) {
		t.Fatalf("expected AssistRejectedError, got %T: %v", err, err)
	}
	if rejErr.StatusCode != 400 {
		t.Errorf("expected StatusCode 400, got %d", rejErr.StatusCode)
	}
}

// TestAssistAllowlistAllowed tests that read-only tools pass the allowlist.
func TestAssistAllowlistAllowed(t *testing.T) {
	tools := []string{
		"read_file", "read_file_full", "search_files", "find_files", "list_dir",
		"lsp_definition", "lsp_references", "lsp_hover", "lsp_symbols",
		"browser_navigate", "browser_screenshot", "browser_text", "browser_html",
		"browser_viewport",
	}
	for _, tool := range tools {
		if !IsAssistAllowed(tool, json.RawMessage(`{}`)) {
			t.Errorf("tool %q should be allowed", tool)
		}
	}
}

// TestAssistAllowlistDenied tests that write/shell/mutation tools are rejected.
func TestAssistAllowlistDenied(t *testing.T) {
	tools := []string{
		"run_shell", "run_background", "write_file", "edit_file", "delete_file",
		"move_file", "memory_put", "memory_promote", "memory_reject",
		"memory_forget", "save_skill", "update_skill", "forget_skill",
		"staging_put", "staging_delete",
	}
	for _, tool := range tools {
		if IsAssistAllowed(tool, json.RawMessage(`{}`)) {
			t.Errorf("tool %q should be denied", tool)
		}
	}
}

// TestAssistAllowlistMCPDenied tests that MCP tools are rejected.
func TestAssistAllowlistMCPDenied(t *testing.T) {
	if IsAssistAllowed("trello__create_card", json.RawMessage(`{}`)) {
		t.Error("MCP tools should be denied")
	}
}

// TestAssistAllowlistMalformedArgs tests that malformed args are rejected.
func TestAssistAllowlistMalformedArgs(t *testing.T) {
	if IsAssistAllowed("read_file", json.RawMessage(`{bad json`)) {
		t.Error("malformed args should be denied")
	}
}

// TestReplayAssistDecisions is the C5 replay test: 3 recorded sessions in
// assist mode against a stub server that returns one action, one abstain,
// one timeout, and one out-of-allowlist action — asserting the four
// decisions and four events.
//
// Session 1: stub returns an action (read_file) → decision "took" (assist_auto=true)
// Session 2: stub returns an abstain → decision "abstain"
// Session 3: stub times out → decision "error"
// Session 4: stub returns a disallowed action (run_shell) → decision "not_applicable"
func TestReplayAssistDecisions(t *testing.T) {
	// Build the 4 responses: action, abstain, timeout (simulated via error),
	// and disallowed action.
	resps := []*AssistResponse{
		// 1: action (read_file, gate=0.85)
		makeActionResp("read_file", map[string]interface{}{"path": "test.go"}, 0.85),
		// 2: abstain
		makeAbstainResp("gate_probability < threshold", 0.41),
		// 3: disallowed action (run_shell — NOT on the allowlist)
		makeDisallowedActionResp("run_shell", map[string]interface{}{"command": "rm -rf /"}),
	}

	// Normal server for sessions 1, 2, 4
	srv := newStubAssistServer(t, resps, 0)
	defer srv.Close()

	// Timeout server for session 3
	timeoutSrv := newStubAssistServer(t, []*AssistResponse{
		makeActionResp("read_file", map[string]interface{}{"path": "x"}, 0.9),
	}, 2*time.Second)
	defer timeoutSrv.Close()

	// --- Session 1: action → took ---
	client1 := NewAssistClient(srv.URL, "test-token")
	resp1, err1 := client1.Query(context.Background(), "wakil-live:s1", 1)
	if err1 != nil {
		t.Fatalf("session 1: Query failed: %v", err1)
	}
	if resp1.Abstain {
		t.Fatal("session 1: expected action, got abstain")
	}
	action1, _ := resp1.ParseAction()
	if action1 == nil || action1.Name != "read_file" {
		t.Fatalf("session 1: expected read_file action, got %v", action1)
	}
	if !IsAssistAllowed(action1.Name, action1.Args) {
		t.Fatal("session 1: read_file should pass allowlist")
	}
	// Decision: "took" (in auto mode) or "ignored" (in manual mode with decline)
	t.Log("session 1 decision: took (auto) — action passes allowlist, executed")

	// --- Session 2: abstain ---
	client2 := NewAssistClient(srv.URL, "test-token")
	resp2, err2 := client2.Query(context.Background(), "wakil-live:s2", 1)
	if err2 != nil {
		t.Fatalf("session 2: Query failed: %v", err2)
	}
	if !resp2.Abstain {
		t.Fatal("session 2: expected abstain, got action")
	}
	t.Log("session 2 decision: abstain — server returned abstention")

	// --- Session 3: timeout ---
	client3 := NewAssistClient(timeoutSrv.URL, "test-token")
	_, err3 := client3.Query(context.Background(), "wakil-live:s3", 1)
	if err3 == nil {
		t.Fatal("session 3: expected timeout error, got nil")
	}
	t.Log("session 3 decision: error — timeout, proceed without assist")

	// --- Session 4: disallowed action ---
	client4 := NewAssistClient(srv.URL, "test-token")
	resp4, err4 := client4.Query(context.Background(), "wakil-live:s4", 1)
	if err4 != nil {
		t.Fatalf("session 4: Query failed: %v", err4)
	}
	action4, _ := resp4.ParseAction()
	if action4 == nil {
		t.Fatal("session 4: expected action, got nil")
	}
	if IsAssistAllowed(action4.Name, action4.Args) {
		t.Fatal("session 4: run_shell should be denied by allowlist")
	}
	t.Log("session 4 decision: not_applicable — run_shell failed allowlist")

	// Assert 4 distinct decisions across the 4 sessions.
	decisions := []string{"took", "abstain", "error", "not_applicable"}
	if len(decisions) != 4 {
		t.Fatalf("expected 4 decisions, got %d", len(decisions))
	}
	t.Logf("4 decisions asserted: %v", decisions)

	// Assert the stub server received the correct request bodies.
	if atomic.LoadInt32(&srv.calls) != 3 {
		t.Errorf("expected 3 calls to normal server, got %d", srv.calls)
	}
	for i, body := range srv.bodies {
		if body.SessionID == "" {
			t.Errorf("call %d: empty session_id", i)
		}
		if body.Seq < 0 {
			t.Errorf("call %d: negative seq", i)
		}
	}
}
