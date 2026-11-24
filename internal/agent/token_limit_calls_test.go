package agent

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

	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/trace"
)

// wireMsg is the subset of an outgoing chat message the pairing checks read.
type wireMsg struct {
	Role       string `json:"role"`
	ToolCallID string `json:"tool_call_id"`
	ToolCalls  []struct {
		ID string `json:"id"`
	} `json:"tool_calls"`
}

// recordingSSEServer serves framesPerCall in order (the last repeats) and
// records every request body's messages.
type recordingSSEServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests [][]wireMsg
}

func newRecordingSSEServer(t *testing.T, framesPerCall ...[]string) *recordingSSEServer {
	t.Helper()
	rs := &recordingSSEServer{}
	call := 0
	rs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []wireMsg `json:"messages"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("recordingSSEServer: bad request body: %v", err)
		}
		rs.mu.Lock()
		rs.requests = append(rs.requests, req.Messages)
		frames := framesPerCall[len(framesPerCall)-1]
		if call < len(framesPerCall) {
			frames = framesPerCall[call]
		}
		call++
		rs.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fl.Flush()
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	return rs
}

func (rs *recordingSSEServer) all() [][]wireMsg {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([][]wireMsg(nil), rs.requests...)
}

// pairingErrors returns one line per assistant tool_call id that does not have
// exactly one tool-result message in msgs (and per tool message with no call).
func pairingErrors(msgs []wireMsg) []string {
	calls := map[string]int{}
	results := map[string]int{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			calls[tc.ID]++
		}
		if m.Role == "tool" {
			results[m.ToolCallID]++
		}
	}
	var errs []string
	for id := range calls {
		if results[id] != 1 {
			errs = append(errs, fmt.Sprintf("tool_call %q has %d results, want 1", id, results[id]))
		}
	}
	for id := range results {
		if calls[id] == 0 {
			errs = append(errs, fmt.Sprintf("tool result %q has no tool_call", id))
		}
	}
	return errs
}

// convPairingErrors is pairingErrors over the stored transcript.
func convPairingErrors(conv []proxy.Message) []string {
	var w []wireMsg
	for _, m := range conv {
		wm := wireMsg{Role: m.Role, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			wm.ToolCalls = append(wm.ToolCalls, struct {
				ID string `json:"id"`
			}{ID: tc.ID})
		}
		w = append(w, wm)
	}
	return pairingErrors(w)
}

func toolFrame(index int, id, name, args string) string {
	b, _ := json.Marshal(args)
	return fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":%d,"id":%q,"type":"function","function":{"name":%q,"arguments":%s}}]}}]}`, index, id, name, b)
}

const completeWrite = `{"path":"ok.go","content":"package ok\n"}`
const cutWrite = `{"path":"evil.go","content":"partial`

// A complete call plus a cut call, finish_reason=length. The complete call
// runs once through the normal path, the cut call never runs, and the
// continuation request carries a tool result for the kept call id.
func TestStreamLengthKeptCallExecutedBeforeContinuation(t *testing.T) {
	srv := newRecordingSSEServer(t,
		[]string{toolFrame(0, "c1", "write_file", completeWrite), toolFrame(1, "c2", "write_file", cutWrite), lengthFinish},
		[]string{contentChunk("continued")},
	)
	defer srv.Close()

	exe := newFakeExecutor()
	app := newTestApp(srv.URL, exe, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.MaxToolIterations = 0

	var tt []trace.ToolTrace
	final, _, err := app.streamTurn(context.Background(), "hi", nil, &tt)
	if err != nil {
		t.Fatalf("streamTurn: %v", err)
	}
	if final != "continued" {
		t.Fatalf("final = %q", final)
	}
	if got := exe.writeCalls["ok.go"]; got != "package ok\n" {
		t.Errorf("complete call ran %q, want it written once", got)
	}
	if _, bad := exe.writeCalls["evil.go"]; bad {
		t.Error("cut call was executed")
	}
	reqs := srv.all()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want exactly 2", len(reqs))
	}
	if errs := pairingErrors(reqs[1]); len(errs) > 0 {
		t.Errorf("second request is not well formed: %v", errs)
	}
	// Order: assistant(tool_calls) → tool result → continuation user message.
	var order []string
	for _, m := range reqs[1] {
		order = append(order, m.Role)
	}
	joined := strings.Join(order, ",")
	if !strings.HasSuffix(joined, "assistant,tool,user") {
		t.Errorf("request roles = %s, want ...assistant,tool,user (continuation after the results)", joined)
	}
}

// Every assistant tool_call id in any outgoing request, and in the stored
// transcript, has exactly one tool result — also after a second cut that
// carries kept calls, and under a forced finish.
func TestStreamLengthRequestWellFormed(t *testing.T) {
	t.Run("kept call then normal finish", func(t *testing.T) {
		srv := newRecordingSSEServer(t,
			[]string{toolFrame(0, "c1", "write_file", completeWrite), lengthFinish},
			[]string{contentChunk("done")},
		)
		defer srv.Close()
		app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
		app.Cfg.MaxToolIterations = 0
		var tt []trace.ToolTrace
		if _, _, err := app.streamTurn(context.Background(), "hi", nil, &tt); err != nil {
			t.Fatal(err)
		}
		for i, r := range srv.all() {
			if errs := pairingErrors(r); len(errs) > 0 {
				t.Errorf("request %d: %v", i, errs)
			}
		}
		if errs := convPairingErrors(app.Conv); len(errs) > 0 {
			t.Errorf("stored transcript: %v", errs)
		}
	})

	t.Run("second cut with a kept call and text", func(t *testing.T) {
		srv := newRecordingSSEServer(t,
			[]string{lengthChunk("first half"), lengthFinish},
			[]string{lengthChunk("second half"), toolFrame(0, "c9", "write_file", completeWrite), lengthFinish},
		)
		defer srv.Close()
		exe := newFakeExecutor()
		app := newTestApp(srv.URL, exe, func(_, _, _ string, _ bool) bool { return true })
		app.Cfg.MaxToolIterations = 0
		var tt []trace.ToolTrace
		_, _, err := app.streamTurn(context.Background(), "hi", nil, &tt)
		if err == nil {
			t.Fatal("second cut: want TokenLimitError")
		}
		if len(exe.writeCalls) != 0 {
			t.Errorf("a call kept on the second cut was executed: %v", exe.writeCalls)
		}
		if errs := convPairingErrors(app.Conv); len(errs) > 0 {
			t.Errorf("stored transcript after the second cut: %v", errs)
		}
	})

	t.Run("second cut with a kept call and no text", func(t *testing.T) {
		srv := newRecordingSSEServer(t,
			[]string{lengthChunk("first half"), lengthFinish},
			[]string{toolFrame(0, "c9", "write_file", completeWrite), lengthFinish},
		)
		defer srv.Close()
		app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
		app.Cfg.MaxToolIterations = 0
		var tt []trace.ToolTrace
		if _, _, err := app.streamTurn(context.Background(), "hi", nil, &tt); err == nil {
			t.Fatal("second cut: want TokenLimitError")
		}
		if errs := convPairingErrors(app.Conv); len(errs) > 0 {
			t.Errorf("stored transcript: %v", errs)
		}
		for i, m := range app.Conv {
			if m.Role == "assistant" && DerefStr(m.Content) == "" && len(m.ToolCalls) == 0 {
				t.Errorf("empty assistant message stored at %d", i)
			}
		}
	})
}

// A kept mutating call goes through the normal gate: declined, it does not run
// and still gets a tool result.
func TestStreamLengthKeptCallGoesThroughGate(t *testing.T) {
	srv := newRecordingSSEServer(t,
		[]string{toolFrame(0, "c1", "write_file", completeWrite), lengthFinish},
		[]string{contentChunk("done")},
	)
	defer srv.Close()
	exe := newFakeExecutor()
	app := newTestApp(srv.URL, exe, func(_, _, _ string, _ bool) bool { return false })
	app.Cfg.MaxToolIterations = 0
	var tt []trace.ToolTrace
	if _, _, err := app.streamTurn(context.Background(), "hi", nil, &tt); err != nil {
		t.Fatal(err)
	}
	if len(exe.writeCalls) != 0 {
		t.Errorf("declined kept call ran: %v", exe.writeCalls)
	}
	reqs := srv.all()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if errs := pairingErrors(reqs[1]); len(errs) > 0 {
		t.Errorf("second request: %v", errs)
	}
}

// A v0.8.10 session saved after a second cut ends in an assistant message with
// tool_calls and no results. Resuming repairs it in memory so the next request
// is well formed; the saved file is not touched.
func TestResumeRepairsDanglingToolCalls(t *testing.T) {
	dangling := []proxy.Message{
		{Role: "user", Content: StrPtr("do it")},
		{Role: "assistant", Content: StrPtr("half"), ToolCalls: []proxy.ToolCall{{ID: "c1", Type: "function", Function: proxy.FunctionCall{Name: "write_file", Arguments: completeWrite}}}},
	}
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.InstallSession(&Session{ChatID: "dangling-0001", Conv: dangling})
	if errs := convPairingErrors(app.Conv); len(errs) > 0 {
		t.Errorf("resumed transcript still dangling: %v", errs)
	}
	if len(dangling) != 2 {
		t.Errorf("the loaded session slice was modified (len %d)", len(dangling))
	}
}

// A reused id answered earlier must not hide a dangling call; an empty id is
// never repaired; nil stays nil.
func TestRepairDanglingEdgeCases(t *testing.T) {
	call := func(id string) proxy.ToolCall {
		return proxy.ToolCall{ID: id, Type: "function", Function: proxy.FunctionCall{Name: "read_file", Arguments: `{}`}}
	}
	res := func(id string) proxy.Message {
		return proxy.Message{Role: "tool", ToolCallID: id, Content: StrPtr("r")}
	}
	conv := []proxy.Message{
		{Role: "user", Content: StrPtr("go")},
		{Role: "assistant", ToolCalls: []proxy.ToolCall{call("dup")}},
		res("dup"),
		{Role: "assistant", Content: StrPtr("mid")},
		{Role: "user", Content: StrPtr("again")},
		{Role: "assistant", ToolCalls: []proxy.ToolCall{call("dup"), call("")}}, // dangling reuse + empty id
	}
	got := repairDanglingToolCalls(conv)
	if len(conv) != 6 {
		t.Fatal("input modified")
	}
	if len(got) != 7 || got[6].Role != "tool" || got[6].ToolCallID != "dup" {
		t.Fatalf("want one synthetic result for the reused id at the end, got %d messages: %+v", len(got), got[len(got)-1])
	}
	for _, m := range got {
		if m.Role == "tool" && m.ToolCallID == "" {
			t.Error("a result was added for an empty-id call")
		}
	}
	if repairDanglingToolCalls(nil) != nil || repairDanglingToolCalls([]proxy.Message{}) != nil {
		t.Error("nil/empty conv must stay nil")
	}
}
