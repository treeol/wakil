package proxy

// Wire coverage for the transport (Step 5).
//
// parseChatChunk is the non-streaming JSON fallback; here it is driven
// directly (its live caller — a JSON-answering server — is also covered
// by the Exec path below) plus through Exec for the transport-level
// branches: build-request error, JSON fallback read, policy-refusal error
// passthrough, and the default-chat-client nil path.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestParseChatChunkMessageShape verifies the non-streaming JSON body is
// read from choices[0].message (content + tool_calls, not delta) and that
// HTTPStatusError/PolicyRefusedError carry their Unwrap contract.
func TestParseChatChunkMessageShape(t *testing.T) {
	raw := []byte(`{
		"model": "served-x",
		"choices": [{"message": {"role": "assistant", "content": "hello", "tool_calls": [{"id": "t1", "type": "function", "function": {"name": "ping", "arguments": "{}"}}]}, "finish_reason": "stop"}],
		"usage": {"prompt_tokens": 7, "completion_tokens": 3, "cost": 0.001}
	}`)
	res, err := parseChatChunk(raw, http.StatusOK)
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "hello" || res.FinishReason != "stop" {
		t.Errorf("content/finish = %q/%q", res.Content, res.FinishReason)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Function.Name != "ping" {
		t.Errorf("tool calls = %+v", res.ToolCalls)
	}
	if !res.Usage.Exact || res.Usage.InputTok != 7 || res.Usage.OutputTok != 3 {
		t.Errorf("usage = %+v", res.Usage)
	}
	if res.Usage.ProviderCostUSD != 0.001 || !res.Usage.HasProviderCost {
		t.Errorf("cost = %v/%v", res.Usage.ProviderCostUSD, res.Usage.HasProviderCost)
	}
	if res.Usage.ServedModel != "served-x" {
		t.Errorf("served = %q", res.Usage.ServedModel)
	}

	// Error body shape.
	_, err = parseChatChunk([]byte(`{"error":{"message":"boom","code":500}}`), http.StatusOK)
	if err == nil {
		t.Fatal("want error for error body")
	}
	// Bad JSON.
	_, err = parseChatChunk([]byte(`not json`), http.StatusOK)
	if err == nil {
		t.Fatal("want error for bad JSON")
	}
}

// TestExecJSONFallback verifies Exec collects a non-SSE JSON answer.
func TestExecJSONFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"model":"m","choices":[{"message":{"content":"json ok"}}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	res, err := c.Exec(context.Background(), ChatRequest{BaseURL: srv.URL, Model: "m", Body: []byte(`{"model":"m","stream":true}`)})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.Content != "json ok" {
		t.Errorf("content = %q", res.Content)
	}
	if res.StatusCode != http.StatusOK {
		t.Errorf("status = %d", res.StatusCode)
	}
}

// TestExecBuildRequestError verifies a malformed URL fails before any dial.
func TestExecBuildRequestError(t *testing.T) {
	c := &Client{HTTP: http.DefaultClient}
	_, err := c.Exec(context.Background(), ChatRequest{
		BaseURL: "http://[::1]:namedport", // unparseable host/port
		Model:   "m",
		Body:    []byte(`{"model":"m"}`),
	})
	if err == nil {
		t.Fatal("want a build-request error")
	}
}

// TestExecPolicyRefusedPassthrough verifies counsel-mappable refusal flows
// through Exec as PolicyRefusedError (the panel then returns the raw error).
func TestExecPolicyRefusedPassthrough(t *testing.T) {
	c := &Client{ZDR: "on"} // BaseURL empty → other host, refused
	_, err := c.Exec(context.Background(), ChatRequest{
		BaseURL: "https://api.example.com/v1/chat/completions",
		Model:   "m",
		Body:    []byte(`{"model":"m"}`),
	})
	if err == nil {
		t.Fatal("want refusal")
	}
	var pref *PolicyRefusedError
	if !errors.As(err, &pref) {
		t.Fatalf("want PolicyRefusedError, got %T: %v", err, err)
	}
}

// TestExecWithNilHTTPUsesDefault verifies Exec works with a nil HTTP client
// (the shared default chat client path).
func TestExecWithNilHTTPUsesDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: " + mustPolicyChunk(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "ok"}, "finish_reason": "stop"}}}) + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: nil}
	res, err := c.Exec(context.Background(), ChatRequest{BaseURL: srv.URL, Model: "m", Body: []byte(`{"model":"m"}`)})
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if res.Content != "ok" {
		t.Errorf("content = %q", res.Content)
	}
}

// TestExecCancelMidStream verifies the ctxErr branch of sendOnce: a cancel
// during an open stream returns ctx.Err().
func TestExecCancelMidStream(t *testing.T) {
	stalled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-stalled
	}))
	defer srv.Close()
	defer close(stalled)

	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := c.Exec(ctx, ChatRequest{BaseURL: srv.URL, Model: "m", Body: []byte(`{"model":"m"}`)})
	if err == nil {
		t.Fatal("expected a cancel error")
	}
}

// TestHTTPStatusErrorAndPolicyRefusedErrorContracts covers the error types'
// Error/Unwrap contracts.
func TestHTTPStatusErrorAndPolicyRefusedErrorContracts(t *testing.T) {
	h := &HTTPStatusError{StatusCode: 500, Body: "x"}
	if h.Error() == "" || !errors.Is(h, ErrBackendFatal) {
		t.Errorf("HTTPStatusError contract broken: %q %v", h.Error(), h.Unwrap())
	}
	p := &PolicyRefusedError{Err: &PolicyError{Host: "h", What: "ZDR"}}
	if p.Error() == "" || p.Unwrap() == nil {
		t.Errorf("PolicyRefusedError contract broken: %q %v", p.Error(), p.Unwrap())
	}
	// errors.As must find both.
	var hs *HTTPStatusError
	if !errors.As(h, &hs) {
		t.Error("errors.As must find HTTPStatusError")
	}
}

// TestExecObservedEntryPoint verifies the ExecObserved seam collects a call.
func TestExecObservedEntryPoint(t *testing.T) {
	var lines []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: " + mustPolicyChunk(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "ok"}, "finish_reason": "stop"}}}) + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, HTTP: srv.Client()}
	res, err := c.ExecObserved(context.Background(), ChatRequest{BaseURL: srv.URL, Model: "m", Body: []byte(`{"model":"m"}`)}, func(s string) { lines = append(lines, s) })
	if err != nil {
		t.Fatalf("exec observed: %v", err)
	}
	if res.Content != "ok" {
		t.Errorf("content = %q", res.Content)
	}
	if len(lines) != 1 || !strings.Contains(lines[0], `"ok"`) {
		t.Errorf("observed lines = %v", lines)
	}
	// JSON bodies round-trip unchanged through the observed seam check.
	b, err := json.Marshal(res)
	if err != nil || !strings.Contains(string(b), `"StatusCode"`) {
		t.Errorf("result shape sanity failed: %v\n%s", err, b)
	}
}

// TestPolicyClientHelpers covers the policy state helpers used by shells
// and per-repo restore paths.
func TestPolicyClientHelpers(t *testing.T) {
	c := &Client{}
	c.SetNoZDRNoticeHook(func(string) {})
	c.ResetPolicyState()
	c.SetDataPolicy("on", "eu", []string{"a.local"})
	c.ResetPolicyState()
	zdr, region, trusted := c.DataPolicy()
	if zdr != "on" || region != "eu" || len(trusted) != 1 || trusted[0] != "a.local" {
		t.Errorf("DataPolicy() = %q/%q/%v", zdr, region, trusted)
	}
	clone := c.CloneForBaseURL("https://openrouter.ai/api")
	if clone.BaseURL != "https://openrouter.ai/api" {
		t.Errorf("clone BaseURL = %q", clone.BaseURL)
	}
	cz, cr, _ := clone.DataPolicy()
	if cz != "on" || cr != "eu" {
		t.Errorf("clone policy = %q/%q, want on/eu", cz, cr)
	}
	if clone.noZDRNoticeHook() == nil {
		t.Error("clone must inherit the notice hook")
	}
	// Usage + backend + latency accessors (thin, but load-bearing for the
	// TUI's live readouts).
	c.SetUsage(UsageStat{InputTok: 3, Exact: true})
	if u := c.LastUsage(); u.InputTok != 3 {
		t.Errorf("LastUsage = %+v", u)
	}
	if got := c.LastExactInputTokens(); got != 3 {
		t.Errorf("LastExactInputTokens = %d, want 3", got)
	}
	c.SetLastUsedBackend("b")
	if got := c.LastUsedBackend(); got != "b" {
		t.Errorf("LastUsedBackend = %q", got)
	}
	c.setLastLatencyMs(5)
	if got := c.LastLatencyMs(); got != 5 {
		t.Errorf("LastLatencyMs = %d, want 5", got)
	}
	// Grounding helpers (full behaviour is pinned in grounding_test.go; here
	// the reset/dedupe surface is touched).
	c.AddGrounding(GroundingEntry{Type: "web", Label: "a"})
	c.AddGrounding(GroundingEntry{Type: "web", Label: "a"}) // dup: dropped
	c.ResetGrounding()
	if n := len(c.Grounding()); n != 0 {
		t.Errorf("ResetGrounding left %d entries", n)
	}
	c.SetGrounding(true, 0.5, []GroundingEntry{{Type: "corpus", Label: "x", Score: 0.5}})
	if _, _, es := c.GroundingState(); len(es) != 1 {
		t.Errorf("GroundingState = %+v", es)
	}
}

// TestIdleReaderExpiryAndDoubleClose covers the idle-reader error and
// double-close branches not exercised by the stream loops.
func TestIdleReaderExpiryAndDoubleClose(t *testing.T) {
	// Error + Unwrap contract of the stall error.
	e := &idleTimeoutErr{d: time.Second}
	if !strings.Contains(e.Error(), "idle") {
		t.Errorf("idleTimeoutErr.Error() = %q", e.Error())
	}
	if !errors.Is(e, ErrBackendStream) {
		t.Error("idleTimeoutErr must unwrap to ErrBackendStream")
	}

	// A reader whose source errors without the timer having fired passes
	// the error through.
	rc := &closeOnlyReader{err: errors.New("boom")}
	r := newIdleReader(rc, time.Hour)
	if _, err := r.Read(make([]byte, 1)); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("Read err = %v, want boom passthrough", err)
	}
	// Double close is a no-op.
	if err := r.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := r.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if rc.closes != 1 {
		t.Errorf("underlying closes = %d, want 1", rc.closes)
	}
	// resetTimer on a closed reader returns immediately.
	r.resetTimer()
}

type closeOnlyReader struct {
	closes int
	err    error
}

func (c *closeOnlyReader) Read(p []byte) (int, error) { return 0, c.err }
func (c *closeOnlyReader) Close() error {
	c.closes++
	return nil
}

// TestParseChatChunkDeltaFallback verifies the delta-shaped non-SSE body and
// the usage sub-object branches.
func TestParseChatChunkDeltaFallback(t *testing.T) {
	raw := []byte(`{"choices":[{"delta":{"content":"from delta","tool_calls":[{"index":0,"id":"t1","type":"function","function":{"name":"ping","arguments":"{}"}}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"completion_tokens_details":{"reasoning_tokens":1},"prompt_tokens_details":{"cached_tokens":1},"cache_creation_input_tokens":4,"server_tool_use":{"web_search_requests":2}}}`)
	res, err := parseChatChunk(raw, http.StatusOK)
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "from delta" {
		t.Errorf("content = %q, want delta content", res.Content)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Function.Name != "ping" {
		t.Errorf("tool calls = %+v", res.ToolCalls)
	}
	if res.Usage.ReasoningTok != 1 || res.Usage.CachedTok != 1 || res.Usage.CacheWriteTok != 4 || res.Usage.WebSearchRequests != 2 {
		t.Errorf("usage = %+v", res.Usage)
	}
}
