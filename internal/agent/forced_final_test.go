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
	"time"
)

// recordedRequest is the subset of a chat-completions body these tests inspect.
type recordedRequest struct {
	Tools      json.RawMessage `json:"tools"`
	ToolChoice json.RawMessage `json:"tool_choice"`
	Messages   []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// contentFrames is a plain text answer: one content chunk then stop.
func contentFrames(text string) []string {
	return []string{
		contentChunk(text),
		`{"choices":[{"delta":{},"finish_reason":"stop"}]}`,
	}
}

// lastUserText returns the content of the final user message, or "".
func (r recordedRequest) lastUserText() string {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role != "user" {
			continue
		}
		var s string
		if json.Unmarshal(r.Messages[i].Content, &s) == nil {
			return s
		}
		return string(r.Messages[i].Content)
	}
	return ""
}

// recordingSSE serves one scripted frame set per call and records each
// request body. Calls beyond the script reuse the last frame set.
func recordingSSE(t *testing.T, script ...[]string) (*httptest.Server, func() []recordedRequest) {
	t.Helper()
	var mu sync.Mutex
	var reqs []recordedRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var rec recordedRequest
		if err := json.Unmarshal(body, &rec); err != nil {
			t.Errorf("recordingSSE: bad request body: %v", err)
		}
		mu.Lock()
		idx := len(reqs)
		reqs = append(reqs, rec)
		if idx >= len(script) {
			idx = len(script) - 1
		}
		frames := script[idx]
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		flusher.Flush()
		for _, f := range frames {
			fmt.Fprintf(w, "data: %s\n\n", f)
			flusher.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	get := func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), reqs...)
	}
	return srv, get
}

func runSendWithin(t *testing.T, app *App, timeout time.Duration) string {
	t.Helper()
	type result struct {
		out string
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := app.Send(context.Background(), "go")
		done <- result{out, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Send: %v", r.err)
		}
		return r.out
	case <-time.After(timeout):
		t.Fatal("Send did not terminate")
	}
	return ""
}

// TestForcedFinal_KeepsToolsOnWire: the forced request keeps the tool list and
// the injected forced-final message is the last user message.
func TestForcedFinal_KeepsToolsOnWire(t *testing.T) {
	srv, reqs := recordingSSE(t,
		toolCallFrames("c1", "run_shell", `{"command":"echo hi"}`),
		contentFrames("final answer"),
	)
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.MaxToolIterations = 1
	runSendWithin(t, app, 5*time.Second)

	got := reqs()
	if len(got) != 2 {
		t.Fatalf("requests = %d, want 2 (tool iteration + forced final)", len(got))
	}
	forced := got[1]
	if len(forced.Tools) == 0 || string(forced.Tools) == "null" {
		t.Fatal("forced final request carries no tools; prompt cache prefix would be invalidated")
	}
	if len(forced.ToolChoice) != 0 && string(forced.ToolChoice) == "null" {
		t.Errorf("tool_choice should not be forced to null on the forced request")
	}
	if got := forced.lastUserText(); got != ForcedFinalPrompt {
		t.Errorf("last user message = %q, want ForcedFinalPrompt", got)
	}
}

// TestForcedFinal_TextAnswerEndsTurn: a text answer on the forced request ends
// the turn with no fallback request.
func TestForcedFinal_TextAnswerEndsTurn(t *testing.T) {
	srv, reqs := recordingSSE(t,
		toolCallFrames("c1", "run_shell", `{"command":"echo hi"}`),
		contentFrames("the answer"),
	)
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.MaxToolIterations = 1
	out := runSendWithin(t, app, 5*time.Second)

	if n := len(reqs()); n != 2 {
		t.Errorf("requests = %d, want 2 (no fallback after a text answer)", n)
	}
	if !strings.Contains(out, "the answer") {
		t.Errorf("final = %q, want the forced text answer", out)
	}
}

// TestForcedFinal_ToolCallAnswerSingleFallback: a tool-call answer on the forced
// request is discarded, exactly one tools-off fallback is sent, and the turn
// ends even if the fallback itself is still asked for tools.
func TestForcedFinal_ToolCallAnswerSingleFallback(t *testing.T) {
	srv, reqs := recordingSSE(t,
		toolCallFrames("c1", "run_shell", `{"command":"echo hi"}`),
		toolCallFrames("c2", "run_shell", `{"command":"echo forced"}`),
		contentFrames("fallback answer"),
	)
	defer srv.Close()

	exec := newFakeExecutor()
	app := newTestApp(srv.URL, exec, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.MaxToolIterations = 1
	out := runSendWithin(t, app, 5*time.Second)

	got := reqs()
	if len(got) != 3 {
		t.Fatalf("requests = %d, want 3 (tool iteration, forced, one fallback)", len(got))
	}
	if len(got[2].Tools) != 0 && string(got[2].Tools) != "null" {
		t.Errorf("fallback request must be tools-off, got tools=%s", got[2].Tools)
	}
	if len(exec.shellCalls) != 1 {
		t.Errorf("shell executed %d times, want 1 (forced tool calls must not run)", len(exec.shellCalls))
	}
	if !strings.Contains(out, "fallback answer") {
		t.Errorf("final = %q, want the fallback answer", out)
	}
}
