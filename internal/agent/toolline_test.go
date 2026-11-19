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

func TestToolResultStatus(t *testing.T) {
	cases := []struct {
		in   string
		ok   bool
		want string
	}{
		{"[declined by user]", false, "declined"},
		{"file1\nfile2", true, "ok"},
		{"(no output)", true, "ok"},
		{"ERROR: boom", false, "error"},
	}
	for _, c := range cases {
		r := stringToToolResult(c.in)
		r.ok = c.ok
		if got := toolResultStatus(r); got != c.want {
			t.Errorf("toolResultStatus(%q, ok=%v) = %q, want %q", c.in, c.ok, got, c.want)
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

// Two searches of the same directory with different patterns must NOT collapse to
// the same identity — the struggle detector keys on Command equality and would
// otherwise flag them as a no-progress re-run (false positive).
func TestMakeTraceEntrySearchIdentityIncludesPattern(t *testing.T) {
	a := MakeTraceEntry(tcall("search_files", `{"pattern":"struggle detected","path":"internal/tui"}`), okResult("x"))
	b := MakeTraceEntry(tcall("search_files", `{"pattern":"no progress","path":"internal/tui"}`), okResult("y"))
	if a.Command == b.Command {
		t.Errorf("different patterns in same dir must differ; both = %q", a.Command)
	}
	if a.Command != "struggle detected @ internal/tui" {
		t.Errorf("Command = %q, want pattern @ path", a.Command)
	}
	// Identical re-run (same pattern AND path) must still match so the detector fires.
	c := MakeTraceEntry(tcall("search_files", `{"pattern":"struggle detected","path":"internal/tui"}`), okResult("z"))
	if a.Command != c.Command {
		t.Errorf("identical re-run should match: %q vs %q", a.Command, c.Command)
	}
	// find_files gets the same treatment.
	f := MakeTraceEntry(tcall("find_files", `{"pattern":"*.go","path":"internal"}`), okResult("z"))
	if f.Command != "*.go @ internal" {
		t.Errorf("find_files Command = %q, want \"*.go @ internal\"", f.Command)
	}
}

func TestMakeTraceEntryReadFileSliceAndReadOnlyFlag(t *testing.T) {
	r1 := MakeTraceEntry(tcall("read_file", `{"path":"a.go","offset":1,"limit":100}`), okResult("x"))
	r2 := MakeTraceEntry(tcall("read_file", `{"path":"a.go","offset":101,"limit":100}`), okResult("y"))
	if r1.Command == r2.Command {
		t.Errorf("paginated read_file slices must have distinct Command; both = %q", r1.Command)
	}
	if r1.Command != "a.go:1+100" || r2.Command != "a.go:101+100" {
		t.Errorf("unexpected Commands: %q, %q", r1.Command, r2.Command)
	}
	if !r1.ReadOnly || !r2.ReadOnly {
		t.Errorf("read_file trace entries must be tagged ReadOnly=true")
	}
	plain := MakeTraceEntry(tcall("read_file", `{"path":"a.go"}`), okResult("x"))
	if plain.Command != "a.go" || !plain.ReadOnly {
		t.Errorf("plain read_file: Command=%q ReadOnly=%v", plain.Command, plain.ReadOnly)
	}
	sh := MakeTraceEntry(tcall("run_shell", `{"command":"go test ./..."}`), errResult("FAIL"))
	if sh.ReadOnly {
		t.Errorf("run_shell must not be tagged ReadOnly")
	}
	ed := MakeTraceEntry(tcall("edit_file", `{"path":"a.go"}`), okResult("ok"))
	if ed.ReadOnly {
		t.Errorf("edit_file must not be tagged ReadOnly")
	}
}

// lsp_* trace entries: identity is path:line plus the symbol, so two lookups of
// different symbols on one line are distinct work and an identical re-run
// matches. All four are read-only exploration tools.
func TestMakeTraceEntryLSP(t *testing.T) {
	cases := []struct {
		name, args, wantCmd, wantAbbrev string
	}{
		{"lsp_definition", `{"path":"a.go","line":12,"symbol":"Foo"}`, "a.go:12 Foo", "lsp_defi"},
		{"lsp_references", `{"path":"a.go","line":12,"symbol":"Foo"}`, "a.go:12 Foo", "lsp_refe"},
		{"lsp_hover", `{"path":"pkg/b.go","line":3,"symbol":"Bar"}`, "pkg/b.go:3 Bar", "lsp_hove"},
		{"lsp_definition", `{"path":"a.go","symbol":"Foo"}`, "a.go Foo", "lsp_defi"}, // no line
		{"lsp_definition", `{"path":"a.go","line":7}`, "a.go:7", "lsp_defi"},         // no symbol
		{"lsp_symbols", `{"query":"Handler"}`, "", "lsp_symb"},                       // default branch: no path/pattern
	}
	for _, c := range cases {
		e := MakeTraceEntry(tcall(c.name, c.args), okResult("x"))
		if e.Command != c.wantCmd || e.Abbrev != c.wantAbbrev {
			t.Errorf("%s %s: (Abbrev, Command) = (%q, %q), want (%q, %q)", c.name, c.args, e.Abbrev, e.Command, c.wantAbbrev, c.wantCmd)
		}
		if !e.ReadOnly || e.DedupRefusal || e.ExitErr {
			t.Errorf("%s: ReadOnly=%v DedupRefusal=%v ExitErr=%v, want true/false/false", c.name, e.ReadOnly, e.DedupRefusal, e.ExitErr)
		}
	}
	a := MakeTraceEntry(tcall("lsp_references", `{"path":"a.go","line":12,"symbol":"Foo"}`), okResult("x"))
	b := MakeTraceEntry(tcall("lsp_references", `{"path":"a.go","line":12,"symbol":"Bar"}`), okResult("x"))
	c := MakeTraceEntry(tcall("lsp_references", `{"path":"a.go","line":12,"symbol":"Foo"}`), okResult("y"))
	if a.Command == b.Command {
		t.Errorf("different symbols on one line must differ; both = %q", a.Command)
	}
	if a.Command != c.Command {
		t.Errorf("identical re-run must match: %q vs %q", a.Command, c.Command)
	}
}

// git_* trace entries: identity is "<rev> <path>" (either part optional).
func TestMakeTraceEntryGit(t *testing.T) {
	cases := []struct {
		name, args, wantCmd, wantAbbrev string
	}{
		{"git_diff", `{"rev":"HEAD~1","path":"a.go"}`, "HEAD~1 a.go", "git_diff"},
		{"git_diff", `{"path":"a.go"}`, "a.go", "git_diff"},
		{"git_diff", `{"rev":"main...feature"}`, "main...feature", "git_diff"},
		{"git_diff", `{"staged":true}`, "", "git_diff"},
		{"git_log", `{"rev":"A..B","path":"x/y.go","count":5}`, "A..B x/y.go", "git_log"},
		{"git_show", `{"rev":"abc123"}`, "abc123", "git_show"},
		{"git_blame", `{"path":"a.go","rev":"v1.0"}`, "v1.0 a.go", "git_blam"},
		{"git_status", `{}`, "", "git_stat"}, // default branch
	}
	for _, c := range cases {
		e := MakeTraceEntry(tcall(c.name, c.args), okResult("x"))
		if e.Command != c.wantCmd || e.Abbrev != c.wantAbbrev {
			t.Errorf("%s %s: (Abbrev, Command) = (%q, %q), want (%q, %q)", c.name, c.args, e.Abbrev, e.Command, c.wantAbbrev, c.wantCmd)
		}
		if !e.ReadOnly || e.DedupRefusal {
			t.Errorf("%s: ReadOnly=%v DedupRefusal=%v, want true/false", c.name, e.ReadOnly, e.DedupRefusal)
		}
	}
	// Same path at two revisions is different work.
	a := MakeTraceEntry(tcall("git_show", `{"rev":"A"}`), okResult("x"))
	b := MakeTraceEntry(tcall("git_show", `{"rev":"B"}`), okResult("x"))
	if a.Command == b.Command {
		t.Errorf("different revs must differ; both = %q", a.Command)
	}
}

// The dedup refusal is carried by a field. Text that merely LOOKS like the
// refusal (a file whose first line is "[already called ...") must not be read
// as one, and a real refusal must be recognised whatever its wording.
func TestMakeTraceEntryDedupRefusalIsAField(t *testing.T) {
	real := toolResult{ok: false, text: "any wording at all", dedupRefusal: true}
	if e := MakeTraceEntry(tcall("read_file", `{"path":"a.go"}`), real); !e.DedupRefusal || !isDedupRefusal(e) {
		t.Errorf("a refusal result must set DedupRefusal: %+v", e)
	}
	lookalike := errResult("[already called read_file with equivalent arguments — pretend]")
	if e := MakeTraceEntry(tcall("read_file", `{"path":"a.go"}`), lookalike); e.DedupRefusal || isDedupRefusal(e) {
		t.Errorf("matching text alone must not make a dedup refusal: %+v", e)
	}
	// The flag alone is not enough without the error status (defensive).
	if isDedupRefusal(ToolTraceEntry{DedupRefusal: true, ExitErr: false}) {
		t.Error("a non-error entry is never a refusal")
	}
}
