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
	got := toolLine(tcall("read_file", `{"path":"main.go"}`), okResult(big))
	want := "· read_file main.go → 412 lines · 12.5KB"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestToolLineShortScalarShownVerbatim(t *testing.T) {
	got := toolLine(tcall("run_shell", `{"command":"pwd"}`), okResult("/root\n"))
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
		if got := resultSummary(stringToToolResult(c.in)); got != c.want {
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

func TestToolPrimaryArgMoveFileSrc(t *testing.T) {
	// move_file uses "src"/"dst", not "path" — the key list must include "src"
	// so the one-line display shows what file is being moved.
	got := toolPrimaryArg(tcall("move_file", `{"src":"a.go","dst":"b.go"}`))
	if want := "a.go"; got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestToolAbbrev(t *testing.T) {
	cases := []struct{ name, want string }{
		{"run_shell", "shell"},
		{"read_file", "read"},
		{"read_file_full", "rfull"},
		{"write_file", "write"},
		{"edit_file", "edit"},
		{"find_files", "find"},
		{"search_files", "search"},
		{"list_dir", "list"},
		{"delete_file", "delete"},
		{"move_file", "move"},
		{"dispatch_subagent", "subagent"},
		{"dispatch_subagents", "subagents"},
		{"unknown_tool", "unknown_"}, // >8 runes: truncated to 8
		{"short", "short"},           // ≤8 runes: returned as-is
	}
	for _, c := range cases {
		if got := toolAbbrev(c.name); got != c.want {
			t.Errorf("toolAbbrev(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestMakeTraceEntryMoveFile(t *testing.T) {
	e := MakeTraceEntry(tcall("move_file", `{"src":"old.go","dst":"new.go"}`), okResult("ok"))
	if e.Abbrev != "move" {
		t.Errorf("Abbrev = %q, want \"move\"", e.Abbrev)
	}
	if e.Command != "old.go → new.go" {
		t.Errorf("Command = %q, want \"old.go → new.go\"", e.Command)
	}
}

func TestMakeTraceEntryMoveFileMalformedArgs(t *testing.T) {
	// Empty src and dst should produce empty Command, not " → ".
	e := MakeTraceEntry(tcall("move_file", `{"src":"","dst":""}`), okResult("ok"))
	if e.Command != "" {
		t.Errorf("Command = %q, want \"\" for empty args", e.Command)
	}
}

func TestMakeTraceEntryDeleteFile(t *testing.T) {
	// delete_file uses "path" as its param name — the default branch extracts it.
	e := MakeTraceEntry(tcall("delete_file", `{"path":"old.go"}`), okResult("ok"))
	if e.Abbrev != "delete" {
		t.Errorf("Abbrev = %q, want \"delete\"", e.Abbrev)
	}
	if e.Command != "old.go" {
		t.Errorf("Command = %q, want \"old.go\"", e.Command)
	}
}
