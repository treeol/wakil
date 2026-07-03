package agent

import (
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

func tcall(name, args string) proxy.ToolCall {
	return proxy.ToolCall{Function: proxy.FunctionCall{Name: name, Arguments: args}}
}

func TestToolLineCollapsesLargeOutput(t *testing.T) {
	big := ""
	for i := 0; i < 412; i++ {
		big += "some line of file content here\n"
	}
	got := toolLine(tcall("read_file", `{"path":"main.go"}`), big)
	want := "· read_file main.go → 412 lines · 12.5KB"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestToolLineShortScalarShownVerbatim(t *testing.T) {
	got := toolLine(tcall("run_shell", `{"command":"pwd"}`), "/root\n")
	if want := "· run_shell pwd → /root"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestResultSummaryEdges(t *testing.T) {
	cases := []struct{ in, want string }{
		{"[declined by user]", "declined"},
		{"(no output)", "ok"},
		{"", "ok"},
		{"ERROR: no such file or directory", "✗ no such file or directory"},
		{"one line only", "one line only"},
	}
	for _, c := range cases {
		if got := resultSummary(c.in); got != c.want {
			t.Errorf("resultSummary(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestToolPrimaryArgFlattensAndTruncates(t *testing.T) {
	// Multi-line / multi-space command flattens to a single spaced line.
	got := toolPrimaryArg(tcall("run_shell", `{"command":"grep -rn foo\n   bar baz"}`))
	if want := "grep -rn foo bar baz"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
