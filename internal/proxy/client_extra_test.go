package proxy

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWrapStreamErrIsBackendStream(t *testing.T) {
	err := wrapStreamErr(io.ErrUnexpectedEOF)
	if !errors.Is(err, ErrBackendStream) {
		t.Errorf("wrapStreamErr should match ErrBackendStream: %v", err)
	}
	if err.Error() == ErrBackendStream.Error() {
		t.Errorf("wrapped error should include cause detail, got bare sentinel: %q", err.Error())
	}
}

// TestStreamErrorClassification verifies the three classification buckets:
//   - 4xx → ErrBackendFatal (never retry)
//   - 5xx → ErrBackendStream (retry)
//   - transport error (pre-response) → ErrBackendStream (retry)
func TestStreamErrorClassification(t *testing.T) {
	sseOK := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n"))
	}

	cases := []struct {
		name       string
		handler    http.HandlerFunc
		wantFatal  bool // ErrBackendFatal
		wantStream bool // ErrBackendStream
	}{
		{
			name:    "200 OK",
			handler: sseOK,
		},
		{
			name: "500 Internal Server Error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "backend down", http.StatusInternalServerError)
			},
			wantStream: true,
		},
		{
			name: "503 Service Unavailable",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "overloaded", http.StatusServiceUnavailable)
			},
			wantStream: true,
		},
		{
			name: "400 Bad Request",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "bad request body", http.StatusBadRequest)
			},
			wantFatal: true,
		},
		{
			name: "401 Unauthorized",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "invalid api key", http.StatusUnauthorized)
			},
			wantFatal: true,
		},
		{
			name: "422 Unprocessable",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "context too long", http.StatusUnprocessableEntity)
			},
			wantFatal: true,
		},
		{
			name: "mid-stream body truncation",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				// Write a partial chunk then close without [DONE].
				w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"he"))
				// Hijack and close the connection abruptly.
				if h, ok := w.(http.Hijacker); ok {
					conn, _, _ := h.Hijack()
					conn.Close()
				}
			},
			wantStream: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()

			c := &Client{
				BaseURL: srv.URL,
				Model:   "ilm",
				ChatID:  "test",
				HTTP:    http.DefaultClient,
			}
			_, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil)

			switch {
			case tc.wantFatal:
				if !errors.Is(err, ErrBackendFatal) {
					t.Errorf("want ErrBackendFatal, got: %v", err)
				}
				if errors.Is(err, ErrBackendStream) {
					t.Errorf("fatal error must NOT match ErrBackendStream")
				}
			case tc.wantStream:
				if !errors.Is(err, ErrBackendStream) {
					t.Errorf("want ErrBackendStream, got: %v", err)
				}
				if errors.Is(err, ErrBackendFatal) {
					t.Errorf("stream error must NOT match ErrBackendFatal")
				}
			default:
				if err != nil {
					t.Errorf("want nil error for 200 OK, got: %v", err)
				}
			}
		})
	}
}

// TestFatalErrMessageContainsStatus verifies the 4xx error message includes
// the HTTP status and body excerpt so the user can diagnose it.
func TestFatalErrMessageContainsStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid api key", http.StatusUnauthorized)
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Model: "ilm", ChatID: "test", HTTP: http.DefaultClient}
	_, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "401") {
		t.Errorf("error message should contain status code 401; got %q", msg)
	}
	if !strings.Contains(msg, "invalid api key") {
		t.Errorf("error message should contain body excerpt; got %q", msg)
	}
}

func strPtr(s string) *string { return &s }

// ── P42: pre-send byte-size guard ─────────────────────────────────────────────

// TestPresendGuardTrimsLargestToolResult verifies that when the request
// exceeds MaxRequestBytes the largest tool result is stubbed and the
// request is sent successfully instead of getting a 400.
func TestPresendGuardTrimsLargestToolResult(t *testing.T) {
	received := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received = string(body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()

	bigContent := strings.Repeat("A", 5000)
	msgs := []Message{
		{Role: "user", Content: strPtr("hello")},
		{Role: "tool", ToolCallID: "tc1", Name: "read_file", Content: strPtr(bigContent)},
	}

	c := &Client{
		BaseURL:         srv.URL,
		Model:           "ilm",
		ChatID:          "test",
		HTTP:            http.DefaultClient,
		MaxRequestBytes: 1000, // tiny limit forces trimming
	}
	_, err := c.Stream(t.Context(), msgs, nil, nil, nil)
	if err != nil {
		t.Fatalf("expected successful send after trimming, got: %v", err)
	}
	// The sent body must be under the limit.
	if len(received) > 1000 {
		t.Errorf("sent body %d bytes, expected ≤ 1000 after trim", len(received))
	}
	// The large content must be replaced with a stub note.
	if strings.Contains(received, bigContent[:100]) {
		t.Error("large content must have been trimmed from sent body")
	}
	if !strings.Contains(received, "pre-send trim") {
		t.Error("stub note 'pre-send trim' must appear in sent body")
	}
}

// TestPresendGuardNoToolsReturnsError verifies that when the request
// exceeds MaxRequestBytes but there are no large tool results to trim,
// Stream returns a fatal error rather than sending an oversized request.
func TestPresendGuardNoToolsReturnsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Should not be reached.
		http.Error(w, "should not receive request", http.StatusBadRequest)
	}))
	defer srv.Close()

	msgs := []Message{
		{Role: "user", Content: strPtr(strings.Repeat("X", 2000))},
	}

	c := &Client{
		BaseURL:         srv.URL,
		Model:           "ilm",
		ChatID:          "test",
		HTTP:            http.DefaultClient,
		MaxRequestBytes: 100, // impossibly small
	}
	_, err := c.Stream(t.Context(), msgs, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error when request cannot be trimmed")
	}
	if !errors.Is(err, ErrBackendFatal) {
		t.Errorf("want ErrBackendFatal, got: %v", err)
	}
}

// TestTrimToolResults verifies the trimmer replaces large messages largest-first.
func TestTrimToolResults(t *testing.T) {
	small := strings.Repeat("s", 300)
	large := strings.Repeat("L", 3000)

	msgs := []Message{
		{Role: "user", Content: strPtr("question")},
		{Role: "tool", Name: "read_file", Content: strPtr(small)},
		{Role: "tool", Name: "search", Content: strPtr(large)},
	}

	// currentSize set large enough to need trimming; maxBytes small enough that
	// only the large result needs to be replaced.
	trimmed := trimToolResults(msgs, 5000, 2500)
	if trimmed == nil {
		t.Fatal("trimToolResults returned nil unexpectedly")
	}
	// The large result should be stubbed.
	if trimmed[2].Content == nil || !strings.Contains(*trimmed[2].Content, "pre-send trim") {
		t.Errorf("large tool result not stubbed: %v", trimmed[2].Content)
	}
	// The small result should be unchanged (trimming the large one was enough).
	if trimmed[1].Content == nil || *trimmed[1].Content != small {
		t.Errorf("small tool result should be unchanged")
	}
	// User message must be untouched.
	if trimmed[0].Content == nil || *trimmed[0].Content != "question" {
		t.Errorf("user message must not be modified")
	}
}

// TestTrimToolResultsPreservesSpillPath verifies that when a tool result
// contains an embedded spill path (from CapToolResult, StubToolResult, or
// SpillFullResult), the pre-send trim preserves it in the stub so the model
// can recover the full content.
func TestTrimToolResultsPreservesSpillPath(t *testing.T) {
	large := strings.Repeat("L", 3000) + "\n[full content at: /cache/read_file_full-abc.txt]"

	msgs := []Message{
		{Role: "user", Content: strPtr("question")},
		{Role: "tool", Name: "read_file_full", Content: strPtr(large)},
	}

	trimmed := trimToolResults(msgs, 5000, 1000)
	if trimmed == nil {
		t.Fatal("trimToolResults returned nil unexpectedly")
	}
	if trimmed[1].Content == nil {
		t.Fatal("tool result content is nil")
	}
	stub := *trimmed[1].Content
	// Must preserve the spill path.
	if !strings.Contains(stub, "/cache/read_file_full-abc.txt") {
		t.Errorf("pre-send trim stub must preserve spill path, got: %q", stub)
	}
	// Must NOT say "retrieve with read_file" when a spill path is available.
	if strings.Contains(stub, "retrieve with read_file") {
		t.Errorf("stub should use spill path, not 'retrieve with read_file', got: %q", stub)
	}
}

// TestTrimToolResultsNoSpillPathUsesFallback verifies that when a tool result
// has no embedded spill path, the trim stub falls back to the original wording.
func TestTrimToolResultsNoSpillPathUsesFallback(t *testing.T) {
	large := strings.Repeat("L", 3000)

	msgs := []Message{
		{Role: "user", Content: strPtr("question")},
		{Role: "tool", Name: "read_file", Content: strPtr(large)},
	}

	trimmed := trimToolResults(msgs, 5000, 1000)
	if trimmed == nil {
		t.Fatal("trimToolResults returned nil unexpectedly")
	}
	stub := *trimmed[1].Content
	if !strings.Contains(stub, "retrieve with read_file") {
		t.Errorf("fallback stub should say 'retrieve with read_file', got: %q", stub)
	}
}

// TestUsageCachedTokensParsed verifies prompt_tokens_details.cached_tokens on
// the trailing usage chunk lands in UsageStat.CachedTok.
func TestUsageCachedTokensParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":20,\"prompt_tokens_details\":{\"cached_tokens\":250}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Kind: KindOpenAI, ConfiguredModel: "m", Model: "m", HTTP: http.DefaultClient}
	if _, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	u := c.LastUsage()
	if u.CachedTok != 250 {
		t.Errorf("CachedTok = %d, want 250", u.CachedTok)
	}
	if u.InputTok != 1000 || u.OutputTok != 20 {
		t.Errorf("InputTok/OutputTok = %d/%d, want 1000/20", u.InputTok, u.OutputTok)
	}
	if !u.Exact {
		t.Error("usage chunk present — Exact should be true")
	}
}

// TestUsageWithoutPromptTokensDetailsCachedTokZero verifies a usage chunk
// missing prompt_tokens_details entirely (proxy, older servers) decodes to
// CachedTok 0 with no nil-dereference.
func TestUsageWithoutPromptTokensDetailsCachedTokZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":500,\"completion_tokens\":10}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Kind: KindOpenAI, ConfiguredModel: "m", Model: "m", HTTP: http.DefaultClient}
	if _, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	u := c.LastUsage()
	if u.CachedTok != 0 {
		t.Errorf("CachedTok = %d, want 0 when prompt_tokens_details absent", u.CachedTok)
	}
}

// TestSSEReader_RejectsOversizedLine verifies that a malformed SSE stream
// sending a line larger than the scanner's max buffer is rejected with an
// error instead of causing unbounded memory growth.
func TestSSEReader_RejectsOversizedLine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Send a "data:" line that exceeds the 10 MB cap.
		w.Write([]byte("data: "))
		huge := strings.Repeat("x", 11*1024*1024) // 11 MB > 10 MB cap
		w.Write([]byte(huge))
		w.Write([]byte("\n\n"))
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Kind: KindOpenAI, ConfiguredModel: "m", Model: "m", HTTP: http.DefaultClient}
	_, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil)
	if err == nil {
		t.Fatal("expected error for oversized SSE line, got nil")
	}
	if !errors.Is(err, ErrBackendStream) {
		t.Errorf("expected ErrBackendStream for oversized line, got: %v", err)
	}
}
