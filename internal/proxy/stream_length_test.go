package proxy

import (
	"errors"
	"strings"
	"testing"
)

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// finish_reason=length with partial text: the text is kept, the marker rides
// with the error, and the call is NOT fatal.
func TestStreamLengthKeepsText(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"half of the answer\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n" +
		"data: [DONE]\n\n"
	msg, err := streamWith(t, body)
	if err == nil {
		t.Fatalf("length finish with text: want the marker, got no error (text %q)", derefStr(msg.Content))
	}
	if !errors.Is(err, ErrTokenLimitPartial) {
		t.Fatalf("length finish with text: want ErrTokenLimitPartial, got %v", err)
	}
	if errors.Is(err, ErrBackendFatal) {
		t.Fatalf("length finish with text must not be fatal: %v", err)
	}
	if msg.Content == nil || *msg.Content != "half of the answer" {
		t.Fatalf("kept text = %q, want %q", derefStr(msg.Content), "half of the answer")
	}
}

// finish_reason=length with one complete and one cut tool call: the complete
// call is kept, the cut one is dropped (never executed), and the marker is
// present.
func TestStreamLengthDropsCutToolCall(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"a.go\\\"}\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":1,\"id\":\"c2\",\"type\":\"function\",\"function\":{\"name\":\"write_file\",\"arguments\":\"{\\\"path\\\":\\\"b.go\\\",\\\"content\\\":\\\"partial\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n" +
		"data: [DONE]\n\n"
	msg, err := streamWith(t, body)
	if !errors.Is(err, ErrTokenLimitPartial) {
		t.Fatalf("length finish with calls: want ErrTokenLimitPartial, got %v", err)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("kept %d tool calls, want 1 (the cut one dropped)", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].Function.Name != "read_file" {
		t.Fatalf("kept call = %q, want read_file", msg.ToolCalls[0].Function.Name)
	}
	if !strings.Contains(msg.ToolCalls[0].Function.Arguments, "a.go") {
		t.Fatalf("kept arguments = %q", msg.ToolCalls[0].Function.Arguments)
	}
}

// finish_reason=length with only a cut tool call and no text: the marker is
// present, no tool call survives, and the turn can still continue.
func TestStreamLengthOnlyCutCall(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"write_file\",\"arguments\":\"{\\\"path\\\":\\\"a.go\\\",\\\"content\\\":\\\"package\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n" +
		"data: [DONE]\n\n"
	msg, err := streamWith(t, body)
	if !errors.Is(err, ErrTokenLimitPartial) {
		t.Fatalf("length finish with cut call only: want ErrTokenLimitPartial, got %v", err)
	}
	if errors.Is(err, ErrBackendFatal) {
		t.Fatalf("length finish with cut call only must not be fatal: %v", err)
	}
	if len(msg.ToolCalls) != 0 {
		t.Fatalf("cut call survived: %v", msg.ToolCalls)
	}
}

// A call whose arguments never started (header chunk only, empty argument
// string) is cut, not a zero-argument call: under finish_reason=length it must
// be dropped, never kept and executed.
func TestStreamLengthDropsEmptyArgumentCall(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"write_file\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}]}\n\n" +
		"data: [DONE]\n\n"
	msg, err := streamWith(t, body)
	if !errors.Is(err, ErrTokenLimitPartial) && !errors.Is(err, ErrBackendFatal) {
		t.Fatalf("want the token-limit marker or the empty-output fatal, got %v", err)
	}
	if len(msg.ToolCalls) != 0 {
		t.Fatalf("empty-argument call kept under length: %+v", msg.ToolCalls)
	}
}

// Outside a length cut an empty argument string is still a valid zero-argument
// call (some models emit it that way).
func TestStreamEmptyArgumentCallKeptWhenNotCut(t *testing.T) {
	body := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"check_pending\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"
	msg, err := streamWith(t, body)
	if err != nil {
		t.Fatalf("zero-argument call: %v", err)
	}
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("kept %d calls, want 1", len(msg.ToolCalls))
	}
}
