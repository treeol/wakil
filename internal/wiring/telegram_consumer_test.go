package wiring

import (
	"testing"
)

func TestIsTelegramChoice(t *testing.T) {
	tests := []struct {
		s    string
		want bool
	}{
		{"approve", true},
		{"decline", true},
		{"allow_reads", true},
		{"grant", true},
		{"timeout", true},
		{"cancelled", true},
		{"", false},
		{"unknown", false},
		{"approved", false}, // past tense — not a valid choice
		{"APPROVE", false},  // case sensitive
	}
	for _, tt := range tests {
		t.Run(tt.s, func(t *testing.T) {
			if got := isTelegramChoice(tt.s); got != tt.want {
				t.Errorf("isTelegramChoice(%q) = %v, want %v", tt.s, got, tt.want)
			}
		})
	}
}

func TestParseTelegramResult(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		// Valid JSON choices
		{"json approve", `{"choice":"approve"}`, "approve"},
		{"json decline", `{"choice":"decline"}`, "decline"},
		{"json allow_reads", `{"choice":"allow_reads"}`, "allow_reads"},
		{"json grant", `{"choice":"grant"}`, "grant"},
		{"json timeout", `{"choice":"timeout"}`, "timeout"},
		{"json cancelled", `{"choice":"cancelled"}`, "cancelled"},
		{"json with whitespace", `  {"choice":"approve"}  `, "approve"},

		// Valid plain-string choices
		{"plain approve", "approve", "approve"},
		{"plain decline", "decline", "decline"},
		{"plain with whitespace", "  approve  ", "approve"},

		// Invalid inputs
		{"empty", "", ""},
		{"no output", "(no output)", ""},
		{"error prefix", "ERROR: MCP call failed", ""},
		{"invalid json", `{not json}`, ""},
		{"json missing choice", `{"foo":"bar"}`, ""},
		{"json empty choice", `{"choice":""}`, ""},
		{"json unknown choice", `{"choice":"unknown"}`, ""},
		{"unknown plain string", "unknown", ""},
		{"json string literal", `"approve"`, ""}, // not an object, not a raw token
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseTelegramResult(tt.input)
			if got != tt.want {
				t.Errorf("parseTelegramResult(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestDetectTelegramBridge_NilMCP(t *testing.T) {
	// A nil MCPManager must return false, not panic.
	if detectTelegramBridge(nil) {
		t.Error("detectTelegramBridge(nil) should return false")
	}
}
