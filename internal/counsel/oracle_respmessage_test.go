package counsel

import (
	"encoding/json"
	"testing"
)

func TestOrRespMessageUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		role    string
		content string
		wantErr bool
	}{
		{"string content", `{"role":"assistant","content":"hello"}`, "assistant", "hello", false},
		{"null content", `{"role":"assistant","content":null}`, "assistant", "", false},
		{"missing content", `{"role":"assistant"}`, "assistant", "", false},
		{"array of text parts", `{"role":"assistant","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}`, "assistant", "a\nb", false},
		{"array skips non-text and malformed parts", `{"role":"assistant","content":[{"type":"image","text":"x"},"bogus",{"type":"text","text":"ok"}]}`, "assistant", "ok", false},
		{"empty array", `{"role":"assistant","content":[]}`, "assistant", "", false},
		{"unknown content shape", `{"role":"assistant","content":42}`, "assistant", "", false},
		{"invalid json", `{"role":`, "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var m orRespMessage
			err := json.Unmarshal([]byte(tc.in), &m)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if tc.wantErr {
				return
			}
			if m.Role != tc.role || m.Content != tc.content {
				t.Fatalf("got role=%q content=%q, want role=%q content=%q", m.Role, m.Content, tc.role, tc.content)
			}
		})
	}
}
