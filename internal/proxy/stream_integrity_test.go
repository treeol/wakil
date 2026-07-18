package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// streamClient builds a Client against srv for Stream-level tests.
func streamClient(srv *httptest.Server) *Client {
	return &Client{
		BaseURL: srv.URL,
		Model:   "ilm",
		ChatID:  "test",
		HTTP:    http.DefaultClient,
	}
}

func streamWith(t *testing.T, body string) (Message, error) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, body)
	}))
	defer srv.Close()
	return streamClient(srv).Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil)
}

// Malformed JSON on a data: line mid-stream must abort the stream as
// retryable. Previously the chunk was silently dropped, leaving a hole in the
// accumulated tool-call arguments that surfaced downstream as
// "could not parse arguments: unexpected end of JSON input".
func TestStreamMalformedChunkFailsStream(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"run_shell\",\"arguments\":\"{\\\"command\\\":\"}}]}}]}\n\n" +
		"data: {THIS IS NOT JSON\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"ls\\\"}\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	_, err := streamWith(t, body)
	if !errors.Is(err, ErrBackendStream) {
		t.Fatalf("malformed chunk: want ErrBackendStream, got %v", err)
	}
	if errors.Is(err, ErrBackendFatal) {
		t.Fatalf("malformed chunk must not be fatal: %v", err)
	}
}

// Losing the first chunk of a tool call loses the function name (it travels
// only there). The malformed chunk aborts before assembly even starts.
func TestStreamMalformedFirstChunkFailsStream(t *testing.T) {
	body := "data: GARBAGE\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{}\"}}]}}]}\n\n" +
		"data: [DONE]\n\n"
	_, err := streamWith(t, body)
	if !errors.Is(err, ErrBackendStream) {
		t.Fatalf("malformed first chunk: want ErrBackendStream, got %v", err)
	}
}

// A stream that ends on clean EOF without [DONE] was truncated — the
// accumulated message is partial and must not be returned as success.
func TestStreamEOFWithoutDoneFails(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"run_shell\",\"arguments\":\"{\\\"command\\\":\\\"ls\"}}]}}]}\n\n"
	// no [DONE] — handler just returns, closing the body cleanly
	_, err := streamWith(t, body)
	if !errors.Is(err, ErrBackendStream) {
		t.Fatalf("EOF without [DONE]: want ErrBackendStream, got %v", err)
	}
	if !strings.Contains(err.Error(), "[DONE]") {
		t.Fatalf("error should mention the missing [DONE] marker: %v", err)
	}
}

// Benign SSE noise (comments, blank lines, event:/id:/retry: fields) must not
// affect the stream.
func TestStreamBenignSSENoiseIgnored(t *testing.T) {
	body := ": keepalive\n\n" +
		"event: completion\nid: 1\nretry: 1000\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n" +
		": OPENROUTER PROCESSING\n\n" +
		"data: [DONE]\n\n"
	msg, err := streamWith(t, body)
	if err != nil {
		t.Fatalf("benign SSE noise must not fail the stream: %v", err)
	}
	if msg.Content == nil || *msg.Content != "ok" {
		t.Fatalf("want content %q, got %v", "ok", msg.Content)
	}
}

// finish_reason=length mid-tool-call leaves truncated arguments; the call
// cannot succeed and retrying identically is pointless → fatal, not stream.
func TestStreamLengthFinishWithToolCallIsFatal(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"write_file\",\"arguments\":\"{\\\"path\\\":\\\"a.go\\\",\\\"content\\\":\\\"package\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n" +
		"data: [DONE]\n\n"
	_, err := streamWith(t, body)
	if !errors.Is(err, ErrBackendFatal) {
		t.Fatalf("length finish with tool call: want ErrBackendFatal, got %v", err)
	}
	if errors.Is(err, ErrBackendStream) {
		t.Fatalf("length truncation must not be retried: %v", err)
	}
	if !strings.Contains(err.Error(), "write_file") {
		t.Fatalf("error should name the truncated tool: %v", err)
	}
}

// Defense in depth: a well-formed stream (with [DONE]) whose accumulated
// arguments are not valid JSON must fail as retryable, not reach handlers.
func TestStreamIntegrityCheckCatchesInvalidArgs(t *testing.T) {
	// Two argument fragments that concatenate to invalid JSON (missing close).
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"run_shell\",\"arguments\":\"{\\\"command\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"ls\\\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	_, err := streamWith(t, body)
	if !errors.Is(err, ErrBackendStream) {
		t.Fatalf("invalid accumulated args: want ErrBackendStream, got %v", err)
	}
	if !strings.Contains(err.Error(), "run_shell") {
		t.Fatalf("error should name the tool with invalid args: %v", err)
	}
}

// "null" unmarshals into a map without error — the integrity check must
// reject it via the opening-brace requirement, not via Unmarshal alone.
func TestStreamIntegrityCheckRejectsNullArgs(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"run_shell\",\"arguments\":\"null\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	_, err := streamWith(t, body)
	if !errors.Is(err, ErrBackendStream) {
		t.Fatalf("null args: want ErrBackendStream, got %v", err)
	}
}

// Scalar/array arguments are not object-shaped — rejected before they reach
// a handler's field-level unmarshal confusion.
func TestStreamIntegrityCheckRejectsNonObjectArgs(t *testing.T) {
	for _, args := range []string{`"just a string"`, `[1,2,3]`, `42`} {
		body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"type\":\"function\",\"function\":{\"name\":\"run_shell\",\"arguments\":" + fmt.Sprintf("%q", args) + "}}]}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
			"data: [DONE]\n\n"
		if _, err := streamWith(t, body); !errors.Is(err, ErrBackendStream) {
			t.Errorf("args %s: want ErrBackendStream, got %v", args, err)
		}
	}
}

// finish_reason=length with an already-complete, valid tool call: the call
// itself is intact but the turn ended by budget exhaustion — still fatal,
// and the error must name the tool.
func TestStreamLengthFinishWithCompleteToolCall(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"type\":\"function\",\"function\":{\"name\":\"run_shell\",\"arguments\":\"{\\\"command\\\":\\\"ls\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n" +
		"data: [DONE]\n\n"
	_, err := streamWith(t, body)
	if !errors.Is(err, ErrBackendFatal) {
		t.Fatalf("length after complete tool call: want ErrBackendFatal, got %v", err)
	}
	if !strings.Contains(err.Error(), "run_shell") {
		t.Fatalf("error should name the tool: %v", err)
	}
}

// OpenRouter-style mid-stream error chunk must surface its own message
// rather than fizzling into a generic truncation error.
func TestStreamMidStreamErrorChunkSurfaced(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"Working on it\"}}]}\n\n" +
		"data: {\"error\":{\"message\":\"Provider returned error\",\"code\":429}}\n\n"
	_, err := streamWith(t, body)
	if !errors.Is(err, ErrBackendStream) {
		t.Fatalf("error chunk: want ErrBackendStream, got %v", err)
	}
	if !strings.Contains(err.Error(), "Provider returned error") {
		t.Fatalf("error should carry the backend's message: %v", err)
	}
}

// CRLF line endings are legal SSE and must parse identically.
func TestStreamCRLFLineEndings(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\r\n\r\n" +
		"data: [DONE]\r\n\r\n"
	msg, err := streamWith(t, body)
	if err != nil {
		t.Fatalf("CRLF stream: %v", err)
	}
	if msg.Content == nil || *msg.Content != "ok" {
		t.Fatalf("want content %q, got %v", "ok", msg.Content)
	}
}

// Zero-argument tools: some models emit empty arguments — must be tolerated.
// Whitespace-padded object args must also pass (the object check trims first).
func TestStreamEmptyAndWhitespaceArgsTolerated(t *testing.T) {
	for _, args := range []string{"", "   {\\\"command\\\":\\\"ls\\\"}  "} {
		body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"type\":\"function\",\"function\":{\"name\":\"run_shell\",\"arguments\":\"" + args + "\"}}]}}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
			"data: [DONE]\n\n"
		msg, err := streamWith(t, body)
		if err != nil {
			t.Errorf("args %q should be tolerated: %v", args, err)
			continue
		}
		if len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "run_shell" {
			t.Errorf("args %q: unexpected tool calls %+v", args, msg.ToolCalls)
		}
	}
}

// Happy path: fragmented arguments across many chunks assemble correctly.
func TestStreamFragmentedToolCallHappyPath(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"run_shell\",\"arguments\":\"{\\\"com\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"mand\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"ls -la\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	msg, err := streamWith(t, body)
	if err != nil {
		t.Fatalf("happy path: %v", err)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %d", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.Function.Name != "run_shell" {
		t.Errorf("name = %q", tc.Function.Name)
	}
	if tc.Function.Arguments != `{"command":"ls -la"}` {
		t.Errorf("arguments = %q", tc.Function.Arguments)
	}
}
