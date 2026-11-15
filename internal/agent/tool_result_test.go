package agent

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
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

// TestMaskedExitEcho pins the run_shell masked-exit exception: a compound
// command ending in an echo of $? whose last line prints a non-zero value
// is ok=false (the trace records it as failed); exit=0 stays ok; and a
// command printing "exit=1" without echoing $? is not guessed at.
func TestMaskedExitEcho(t *testing.T) {
	tests := []struct {
		name    string
		command string
		out     string
		wantOk  bool
	}{
		{"echo non-zero last line", `./check.sh; echo "exit=$?"`, "exit=1", false},
		{"echo zero last line", `./check.sh; echo "exit=$?"`, "exit=0", true},
		{"plain print, no echo of $?", "grep -r x .", "exit=1", true},
		{"echo with quoted $? and preceding output", `go test ./...; echo "exit=$?"`, "ok foo\nFAIL\n exit=1", false},
		{"echo $? without exit= prefix is not guessed", `make check; echo $?`, "1", true},
		{"no echo at all, failure via ERROR", `make check`, "ERROR: exit status 1", false},
		{"error prefix still wins", `./check.sh; echo "exit=$?"`, "ERROR: exit status 1\nexit=0", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stringToToolResultFor("run_shell", tt.command, tt.out)
			if got.ok != tt.wantOk {
				t.Errorf("run_shell + command %q + out %q: ok = %v, want %v", tt.command, tt.out, got.ok, tt.wantOk)
			}
		})
	}
	// A non-run_shell tool is never affected by the exception.
	if r := stringToToolResultFor("read_file", `echo "exit=$?"`, "exit=1"); !r.ok {
		t.Error("non-run_shell tool must stay ok for exit=1 output")
	}
}

// TestMaskedExitEchoTrace: the trace entry for a masked exit carries
// ExitErr, so the struggle trace records the call as failed.
func TestMaskedExitEchoTrace(t *testing.T) {
	tc := proxy.ToolCall{Function: proxy.FunctionCall{
		Name:      "run_shell",
		Arguments: `{"command":"./check.sh; echo \"exit=$?\""}`,
	}}
	r := stringToToolResultFor("run_shell", `./check.sh; echo "exit=$?"`, "exit=1")
	e := MakeTraceEntry(tc, r)
	if !e.ExitErr {
		t.Fatal("masked exit=1 must record ExitErr in the trace")
	}
	r0 := stringToToolResultFor("run_shell", `./check.sh; echo "exit=$?"`, "exit=0")
	if MakeTraceEntry(tc, r0).ExitErr {
		t.Fatal("exit=0 must not record ExitErr")
	}
}
