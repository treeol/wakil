package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"wakil/internal/trace"
)

// traceServer returns an httptest.Server that serves two SSE responses:
//
//	call 1: reasoning chunk + read_file tool call + usage
//	call 2: final text "done" + usage
//
// X-Ilm-Backend-Used is set to "testbackend" on every response so the backend
// metadata field is exercised. The server is the authoritative source of truth
// for what the trace must contain — changing the server frames must be paired
// with updating the assertions below.
func traceServer(t *testing.T) *httptest.Server {
	t.Helper()
	call := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}
		call++
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Ilm-Backend-Used", "testbackend")

		var frames []string
		if call == 1 {
			frames = []string{
				// Reasoning content — should be accumulated in ReasoningChars.
				`{"choices":[{"delta":{"reasoning_content":"i should read the file"},"finish_reason":null}]}`,
				// Tool call: read_file.
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"tc1","type":"function","function":{"name":"read_file","arguments":""}}]},"finish_reason":null}]}`,
				`{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"/work/data.txt\"}"}}]},"finish_reason":null}]}`,
				`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
				// Usage chunk — gives exact token counts.
				`{"usage":{"prompt_tokens":120,"completion_tokens":15,"completion_tokens_details":{"reasoning_tokens":8}}}`,
			}
		} else {
			frames = []string{
				`{"choices":[{"delta":{"content":"done reading"},"finish_reason":null}]}`,
				`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
				`{"usage":{"prompt_tokens":200,"completion_tokens":3}}`,
			}
		}
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// TestTraceWriteToDisk is the P38 end-to-end integration test. It asserts:
//
//	(a) the trace directory is created by Open
//	(b) a per-turn rich record is written to disk
//	(c) the record contains all P38 fields: pre_cap tool bytes, reasoning chars,
//	    turn_type, sft_eligible:false, store_header at line 0, backend, outcome
func TestTraceWriteToDisk(t *testing.T) {
	dir := t.TempDir()
	srv := traceServer(t)
	defer srv.Close()

	exe := newFakeExecutor()
	// File content is 300 bytes — under the default 8 000-char cap so
	// PreCapBytes == PostCapBytes and Capped == false in this test.
	exe.files["/work/data.txt"] = strings.Repeat("a", 300)

	app := newTestApp(srv.URL, exe, func(_, _, _ string, _ bool) bool { return true })

	// Open the trace store using the same chat_id the App will use in records.
	sessionID := app.Client.ChatID // "test" from newTestClient
	ts, err := trace.Open(dir, sessionID, "test-model", "/workspace")
	if err != nil {
		t.Fatal("trace.Open:", err)
	}
	app.Trace = ts

	_, err = app.Send(context.Background(), "read the data file")
	if err != nil {
		t.Fatal("Send:", err)
	}

	ts.Close()

	// (a) Trace directory was created.
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("trace dir missing:", err)
	}

	// (b) Per-turn file was written.
	path := filepath.Join(dir, sessionID+".jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("trace file not written: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected ≥ 2 JSONL records, got %d:\n%s", len(lines), string(raw))
	}

	// (c.i) Line 0 must be store_header with sft_eligible:false.
	var hdr trace.Record
	if err := json.Unmarshal([]byte(lines[0]), &hdr); err != nil {
		t.Fatalf("line 0 parse error: %v", err)
	}
	if hdr.Type != "store_header" {
		t.Errorf("line[0].type = %q, want store_header", hdr.Type)
	}
	if hdr.SftEligible {
		t.Error("store_header: sft_eligible must be false")
	}
	if hdr.SessionID != sessionID {
		t.Errorf("store_header.session_id = %q, want %q", hdr.SessionID, sessionID)
	}
	if hdr.Model != "test-model" {
		t.Errorf("store_header.model = %q, want test-model", hdr.Model)
	}
	if hdr.Workspace != "/workspace" {
		t.Errorf("store_header.workspace = %q, want /workspace", hdr.Workspace)
	}
	if hdr.Ts == "" {
		t.Error("store_header.ts is empty")
	}

	// (c.ii) Line 1 is the turn record with all P38 rich fields.
	var turn trace.Record
	if err := json.Unmarshal([]byte(lines[1]), &turn); err != nil {
		t.Fatalf("line 1 parse error: %v", err)
	}
	if turn.Type != "turn" {
		t.Errorf("line[1].type = %q, want turn", turn.Type)
	}

	// sft_eligible must be false on every record.
	if turn.SftEligible {
		t.Error("turn: sft_eligible must be false")
	}

	// turn_type: tool call was made → "tool_loop".
	if turn.TurnType != "tool_loop" {
		t.Errorf("turn_type = %q, want tool_loop", turn.TurnType)
	}

	// pre-cap tool result: tool output is 300+ bytes, so pre_cap_bytes > 0.
	if len(turn.ToolCalls) == 0 {
		t.Fatal("turn.tool_calls is empty — pre_cap tap did not fire")
	}
	tc0 := turn.ToolCalls[0]
	if tc0.Name != "read_file" {
		t.Errorf("tool_calls[0].name = %q, want read_file", tc0.Name)
	}
	if tc0.PreCapBytes <= 0 {
		t.Errorf("tool_calls[0].pre_cap_bytes = %d, want > 0", tc0.PreCapBytes)
	}
	// Under the cap: post == pre and Capped == false.
	if tc0.PostCapBytes != tc0.PreCapBytes {
		t.Errorf("post_cap_bytes (%d) != pre_cap_bytes (%d) for uncapped result",
			tc0.PostCapBytes, tc0.PreCapBytes)
	}
	if tc0.Capped {
		t.Error("Capped = true for a result under the cap")
	}

	// reasoning_chars: server sent "i should read the file" (22 chars).
	if turn.ReasoningChars <= 0 {
		t.Errorf("reasoning_chars = %d, want > 0", turn.ReasoningChars)
	}

	// routing/backend metadata: X-Ilm-Backend-Used = "testbackend".
	if turn.Backend != "testbackend" {
		t.Errorf("backend = %q, want testbackend", turn.Backend)
	}

	// outcome: normal completion.
	if turn.Outcome != "complete" {
		t.Errorf("outcome = %q, want complete", turn.Outcome)
	}

	// turn_index: this is the first user message → 1.
	if turn.TurnIndex != 1 {
		t.Errorf("turn_index = %d, want 1", turn.TurnIndex)
	}

	// token counts: second call usage (200 + 3 = final iteration).
	if turn.InputTokens <= 0 {
		t.Errorf("input_tokens = %d, want > 0", turn.InputTokens)
	}
	if turn.OutputTokens <= 0 {
		t.Errorf("output_tokens = %d, want > 0", turn.OutputTokens)
	}

	// session_id must match the client's chat_id.
	if turn.SessionID != sessionID {
		t.Errorf("turn.session_id = %q, want %q", turn.SessionID, sessionID)
	}
}

// TestTraceNilSafe verifies that nil *trace.Store never panics — a Store-less
// App must behave identically to a pre-P38 App.
func TestTraceNilSafe(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("hello")})
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	// app.Trace is nil by default.

	resp, err := app.Send(context.Background(), "hi")
	if err != nil {
		t.Fatal(err)
	}
	if resp == "" {
		t.Error("expected non-empty response")
	}
}

// TestTraceCappedResult verifies that pre_cap_bytes > post_cap_bytes and
// Capped == true when a tool result exceeds ToolResultCap.
func TestTraceCappedResult(t *testing.T) {
	dir := t.TempDir()

	// Server: returns a single tool call + final text.
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}
		call++
		w.Header().Set("Content-Type", "text/event-stream")
		var frames []string
		if call == 1 {
			frames = toolCallFrames("tc2", "read_file", `{"path":"/work/big.txt"}`)
		} else {
			frames = []string{contentChunk("ok")}
		}
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	exe := newFakeExecutor()
	// 20 000 bytes exceeds the default ToolResultCap of 8 000.
	exe.files["/work/big.txt"] = strings.Repeat("b", 20000)

	app := newTestApp(srv.URL, exe, func(_, _, _ string, _ bool) bool { return true })

	ts, err := trace.Open(dir, "sess-cap", "m", "")
	if err != nil {
		t.Fatal(err)
	}
	app.Trace = ts
	_, err = app.Send(context.Background(), "read big file")
	if err != nil {
		t.Fatal(err)
	}
	ts.Close()

	raw, err := os.ReadFile(filepath.Join(dir, "sess-cap.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected ≥ 2 records, got %d", len(lines))
	}
	var turn trace.Record
	if err := json.Unmarshal([]byte(lines[1]), &turn); err != nil {
		t.Fatal(err)
	}
	if len(turn.ToolCalls) == 0 {
		t.Fatal("no tool calls in turn")
	}
	tc := turn.ToolCalls[0]
	if !tc.Capped {
		t.Error("Capped = false for a result that exceeded the cap")
	}
	if tc.PreCapBytes <= tc.PostCapBytes {
		t.Errorf("pre_cap_bytes (%d) should be > post_cap_bytes (%d)", tc.PreCapBytes, tc.PostCapBytes)
	}
}
