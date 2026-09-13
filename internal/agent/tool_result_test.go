package agent

import (
	"strings"
	"testing"
)

func TestStringToToolResult(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool // ok
	}{
		{"plain text", "hello world", true},
		{"no output marker", "(no output)", true},
		{"output with ERROR: line in middle", "line1\nERROR: something\nline3", true},
		{"output starting with ERROR:", "ERROR: something", false},
		{"declined by user", "[declined by user]", false},
		{"declined with extra text", "[declined by user] because reasons", false},
		{"log file with ERROR lines", "2024-01-01 INFO: starting\n2024-01-01 ERROR: connection lost\n2024-01-01 INFO: retrying", true},
		{"empty string", "", true},
		{"output ending with ERROR: line", "some output\nERROR: at end", true},
		{"multi-line output no ERROR", "line1\nline2\nline3", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stringToToolResult(tt.in)
			if got.ok != tt.want {
				t.Errorf("stringToToolResult(%q).ok = %v, want %v", tt.in, got.ok, tt.want)
			}
		})
	}
}

func TestFormatResult(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		err     error
		wantOk  bool
		wantHas string // substring that must be present
	}{
		{"success with output", "hello", nil, true, "hello"},
		{"success no output", "", nil, true, "(no output)"},
		{"error no output", "", errFake{}, false, "ERROR: "},
		{"error with output", "some output", errFake{}, false, "ERROR: "},
		{"error with output preserves stdout", "some output", errFake{}, false, "some output"},
		{"output trailing newline trimmed", "hello\n\n", nil, true, "hello"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := formatResult(tt.out, tt.err)
			r := stringToToolResult(s)
			if r.ok != tt.wantOk {
				t.Errorf("formatResult(%q, %v).ok = %v, want %v (result: %q)", tt.out, tt.err, r.ok, tt.wantOk, s)
			}
			if tt.wantHas != "" && !strings.Contains(s, tt.wantHas) {
				t.Errorf("formatResult(%q, %v) = %q, want substring %q", tt.out, tt.err, s, tt.wantHas)
			}
		})
	}
}

type errFake struct{}

func (errFake) Error() string { return "fake error" }
