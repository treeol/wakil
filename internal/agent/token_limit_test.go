package agent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/treeol/wakil/internal/trace"
)

// countingAndForwardingServer mirrors the inner SSE server while counting
// requests, so a test can assert the exact request count.
func countingAndForwardingServer(t *testing.T, inner *httptest.Server, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// Forward to the inner server with the same path and body.
		req, err := http.NewRequestWithContext(r.Context(), r.Method, inner.URL+r.URL.Path, io.NopCloser(bytes.NewReader(body)))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for k, vs := range r.Header {
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
	}))
}

// lengthChunk emits a content delta with finish_reason=null.
func lengthChunk(text string) string {
	return `{"choices":[{"delta":{"content":"` + text + `"},"finish_reason":null}]}`
}

// lengthFinish is the finish_reason=length delta.
const lengthFinish = `{"choices":[{"delta":{},"finish_reason":"length"}]}`

// newTokenLimitApp builds the app used by the token-limit tests: an endpoint
// name and a max_tokens value, so the second-cut line can name both.
func newTokenLimitApp(url string) *App {
	app := newTestApp(url, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	mt := 4096
	app.Client.MaxTokens = &mt
	app.Cfg.EndpointName = "test-ep"
	app.Cfg.MaxToolIterations = 0
	return app
}

// Cut text, then an ordinary finish: the kept text is followed by the
// continuation in Conv and the turn completes.
func TestStreamLengthRecoveryContinues(t *testing.T) {
	srv := sseServer(t,
		[]string{lengthChunk("half of the answer"), lengthFinish},
		[]string{contentChunk(" — completed.")},
	)
	defer srv.Close()

	app := newTokenLimitApp(srv.URL)

	var traceToolCalls []trace.ToolTrace
	final, _, err := app.streamTurn(context.Background(), "hi", nil, &traceToolCalls)
	if err != nil {
		t.Fatalf("streamTurn error: %v", err)
	}
	if final != " — completed." {
		t.Fatalf("final = %q, want the continuation only", final)
	}
	// Conv holds the cut text (kept) followed by the continuation prompt
	// and then the completed text.
	var assistantTexts []string
	continuationSeen := false
	for _, m := range app.Conv {
		if m.Role == "assistant" && m.Content != nil {
			assistantTexts = append(assistantTexts, *m.Content)
		}
		if m.Role == "user" && m.Content != nil && strings.Contains(*m.Content, "Continue from exactly where") {
			continuationSeen = true
		}
	}
	if !continuationSeen {
		t.Fatal("continuation prompt not in Conv")
	}
	if len(assistantTexts) < 2 || assistantTexts[0] != "half of the answer" {
		t.Fatalf("kept text not in Conv: %v", assistantTexts)
	}
}

// Cut, cut: the turn stops with one line naming the endpoint, the model,
// the current max_tokens value and how to raise it. Exactly two requests.
func TestStreamLengthSecondCutStops(t *testing.T) {
	inner := sseServer(t,
		[]string{lengthChunk("first half"), lengthFinish},
		[]string{lengthChunk("second half"), lengthFinish},
	)
	defer inner.Close()

	var calls atomic.Int32
	// Wrap the SSE server to count requests: sseServer itself serves the
	// frames; counting via a second full handler would duplicate it, so we
	// count through a counting front server that mirrors the shape.
	srv := countingAndForwardingServer(t, inner, &calls)
	defer srv.Close()

	app := newTokenLimitApp(srv.URL)

	var traceToolCalls []trace.ToolTrace
	_, _, err := app.streamTurn(context.Background(), "hi", nil, &traceToolCalls)
	if err == nil {
		t.Fatal("second cut: want TokenLimitError, got nil")
	}
	var tle *TokenLimitError
	if !errors.As(err, &tle) {
		t.Fatalf("second cut: want *TokenLimitError, got %T: %v", err, err)
	}
	line := tle.Error()
	for _, want := range []string{"test-ep", "ilm", "4096", "endpoints.test-ep.max_tokens"} {
		if !strings.Contains(line, want) {
			t.Fatalf("second-cut line %q missing %q", line, want)
		}
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("requests = %d, want exactly 2 (cut + one continuation)", got)
	}
	// The first partial text is in Conv even though the turn ended.
	var kept string
	for _, m := range app.Conv {
		if m.Role == "assistant" && m.Content != nil && *m.Content == "first half" {
			kept = *m.Content
		}
	}
	if kept == "" {
		t.Fatal("first partial text was not kept in Conv")
	}
}

// The cut tool call is dropped by the proxy and never executed.
func TestStreamLengthCutCallNeverExecutes(t *testing.T) {
	srv := sseServer(t,
		[]string{
			`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"evil.go\",\"content\":\"partial"}}]}}]}`,
			lengthFinish,
		},
		[]string{contentChunk("done")},
	)
	defer srv.Close()

	exe := newFakeExecutor()
	app := newTestApp(srv.URL, exe, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.MaxToolIterations = 0

	var traceToolCalls []trace.ToolTrace
	final, _, err := app.streamTurn(context.Background(), "hi", nil, &traceToolCalls)
	if err != nil {
		t.Fatalf("streamTurn error: %v", err)
	}
	if final != "done" {
		t.Fatalf("final = %q", final)
	}
	if len(exe.writeCalls) != 0 {
		t.Fatalf("cut call was executed: writes %v", exe.writeCalls)
	}
}
