package proxy

import (
	"encoding/json"
	"testing"
)

// TestTurnFieldNeverOnWire proves that the Turn field on Message never reaches
// the wire body. marshalWireMessages builds wireMessage field by field (Role,
// Content, ToolCalls, ToolCallID, Name) and explicitly excludes Turn. This is
// the golden guarantee for long-session mode: Turn is local bookkeeping only.
func TestTurnFieldNeverOnWire(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: StrPtr("hello"), Turn: 1},
		{Role: "assistant", Content: StrPtr("hi"), Turn: 1},
		{Role: "user", Content: StrPtr("second"), Turn: 2},
	}

	// marshalWireMessages with no marked indices = byte-identical to json.Marshal
	// of []wireMessage (which has no Turn field).
	wire, err := marshalWireMessages(msgs, nil)
	if err != nil {
		t.Fatalf("marshalWireMessages: %v", err)
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	// The wire body must NOT contain "wakil_turn".
	if s := string(raw); containsField(s, "wakil_turn") {
		t.Errorf("wire body contains 'wakil_turn' — Turn must never reach the wire:\n%s", s)
	}

	// But json.Marshal of []Message DOES include wakil_turn (it's a real json tag).
	msgRaw, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("json.Marshal(msgs): %v", err)
	}
	if s := string(msgRaw); !containsField(s, "wakil_turn") {
		t.Errorf("session JSON should contain 'wakil_turn' (persisted), got:\n%s", s)
	}
}

// TestTurnFieldWithCacheControl proves Turn is excluded from the wire even when
// cache_control decoration is applied (marked messages use parts-shaped content).
func TestTurnFieldWithCacheControl(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: StrPtr("hello"), Turn: 1},
		{Role: "assistant", Content: StrPtr("hi"), Turn: 1},
	}
	marked := map[int]bool{0: true, 1: true}

	wire, err := marshalWireMessages(msgs, marked)
	if err != nil {
		t.Fatalf("marshalWireMessages: %v", err)
	}
	raw, err := json.Marshal(wire)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if s := string(raw); containsField(s, "wakil_turn") {
		t.Errorf("wire body with cache_control contains 'wakil_turn' — Turn must never reach the wire:\n%s", s)
	}
}

func containsField(s, field string) bool {
	return jsonHasField(s, field)
}

// jsonHasField does a quick substring check for a JSON field name. Not a full
// parser, but sufficient for checking whether a field name appears in the
// serialized output.
func jsonHasField(s, field string) bool {
	needle := "\"" + field + "\""
	for i := 0; i < len(s); i++ {
		if s[i] == '"' && i+len(needle) <= len(s) && s[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// StrPtr is a test helper (mirrors the one in the agent package).
func StrPtr(s string) *string { return &s }
