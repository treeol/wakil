package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// findingsRange is a mixed range: two reads of one file (overlapping and
// separate), a whole-file read, a write, an edit, a failed and a passed
// shell command, and the agent's own text.
func findingsRange() []proxy.Message {
	asst := func(text string, calls ...proxy.ToolCall) proxy.Message {
		m := proxy.Message{Role: "assistant", ToolCalls: calls}
		if text != "" {
			m.Content = StrPtr(text)
		}
		return m
	}
	call := func(id, name, args string) proxy.ToolCall {
		return proxy.ToolCall{ID: id, Type: "function", Function: proxy.FunctionCall{Name: name, Arguments: args}}
	}
	return []proxy.Message{
		asst("First I read the parser.", call("r1", "read_file", `{"path":"b.go","offset":10,"limit":50}`)),
		trMsg("r1", "read_file", "RESULT-BODY-ONE"),
		asst("", call("r2", "read_file", `{"path":"b.go","offset":55,"limit":20}`)),
		trMsg("r2", "read_file", "RESULT-BODY-TWO"),
		asst("", call("r3", "read_file", `{"path":"a.go","offset":200}`)),
		trMsg("r3", "read_file", "RESULT-BODY-THREE"),
		asst("", call("r4", "read_file_full", `{"path":"c.go"}`)),
		trMsg("r4", "read_file_full", "RESULT-BODY-FOUR"),
		asst("The bug is in the tokenizer, not the parser.", call("w1", "write_file", `{"path":"d.go","content":"SECRET-ARG-CONTENT-NOT-A-PATH"}`)),
		trMsg("w1", "write_file", "wrote d.go"),
		asst("", call("e1", "edit_file", `{"path":"e.go","old_string":"x","new_string":"y"}`)),
		trMsg("e1", "edit_file", "edited e.go"),
		asst("", call("s1", "run_shell", `{"command":"cd sub && go test ./... -run X --token=hunter2"}`)),
		trMsg("s1", "run_shell", "ERROR: exit status 2\nFAIL TOKEN-IN-OUTPUT-MARKER"),
		asst("", call("s2", "run_shell", `{"command":"FOO=1 make build"}`)),
		trMsg("s2", "run_shell", "ERROR: exit status 7\nboom"),
		asst("", call("s3", "run_shell", `{"command":"ls"}`)),
		trMsg("s3", "run_shell", "all good TOKEN-IN-OUTPUT-MARKER"),
		asst("Next I will fix the tokenizer."),
	}
}

func TestBuildFindingsMixedRange(t *testing.T) {
	got := buildFindings(findingsRange(), "/cache/turn-1-part-1.txt")
	// Content: merged sorted read ranges, writes, failures newest first, notes
	// newest first, the archive hint.
	wantLines := []string{
		"Findings so far:",
		"read (requested ranges): a.go:200-; b.go:10-74; c.go",
		"written or edited: d.go; e.go",
		"failed commands (newest first): make (exit 7); go (exit 2)",
		"my last notes (newest first): Next I will fix the tokenizer. | The bug is in the tokenizer, not the parser. | First I read the parser.",
		`archive: /cache/turn-1-part-1.txt — search it with search_files {"pattern":"<term>","path":"/cache/turn-1-part-1.txt"}`,
	}
	if want := strings.Join(wantLines, "\n") + "\n"; got != want {
		t.Fatalf("findings mismatch\n got:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildFindingsDeterministic(t *testing.T) {
	a := buildFindings(findingsRange(), "/cache/x.txt")
	for i := 0; i < 20; i++ {
		if b := buildFindings(findingsRange(), "/cache/x.txt"); b != a {
			t.Fatalf("run %d differs:\n%s\n---\n%s", i, a, b)
		}
	}
}

// TestBuildFindingsNoToolResultBytes: a marker that only appears in tool
// results, and a secret that only appears in call arguments (a file's content
// and a command's arguments), never reaches the part.
func TestBuildFindingsNoToolResultBytes(t *testing.T) {
	got := buildFindings(findingsRange(), "/cache/x.txt")
	for _, bad := range []string{"RESULT-BODY", "TOKEN-IN-OUTPUT-MARKER", "boom", "FAIL", "hunter2", "SECRET-ARG-CONTENT", "--token", "./..."} {
		if strings.Contains(got, bad) {
			t.Errorf("findings contain %q:\n%s", bad, got)
		}
	}
}

func TestBuildFindingsEmptyRange(t *testing.T) {
	if got := buildFindings(nil, "/cache/x.txt"); got != "" {
		t.Errorf("empty range: %q", got)
	}
	// Only unusable calls: a failed read, a declined write, a passed command.
	w := []proxy.Message{
		tcMsg("a", "read_file", `{"path":"x.go"}`), trMsg("a", "read_file", "ERROR: no such file"),
		tcMsg("b", "write_file", `{"path":"y.go"}`), trMsg("b", "write_file", "[declined by user]"),
		tcMsg("c", "run_shell", `{"command":"ls"}`), trMsg("c", "run_shell", "ok"),
	}
	if got := buildFindings(w, "/cache/x.txt"); got != "" {
		t.Errorf("nothing extractable: %q", got)
	}
}

// TestBuildFindingsByteLimitAndMarker: many entries are cut from the oldest,
// with a visible "(n more)", under maxFindingsBytes; the archive line stays.
func TestBuildFindingsByteLimitAndMarker(t *testing.T) {
	var w []proxy.Message
	for i := 0; i < 80; i++ {
		id := fmt.Sprintf("r%d", i)
		w = append(w, tcMsg(id, "read_file", fmt.Sprintf(`{"path":"pkg/sub/file_%03d.go","offset":1,"limit":40}`, i)), trMsg(id, "read_file", "x"))
	}
	got := buildFindings(w, "/cache/turn-1-part-9.txt")
	if len(got) > maxFindingsBytes {
		t.Fatalf("part is %d bytes, limit %d:\n%s", len(got), maxFindingsBytes, got)
	}
	if !strings.Contains(got, " more)") {
		t.Errorf("no (n more) marker:\n%s", got)
	}
	if !strings.Contains(got, "archive: /cache/turn-1-part-9.txt") {
		t.Errorf("archive line missing:\n%s", got)
	}
	// The newest entries survive, the oldest are the ones cut.
	if !strings.Contains(got, "file_079.go") || strings.Contains(got, "file_000.go") {
		t.Errorf("not cut from the oldest:\n%s", got)
	}
}

func TestBuildFindingsLaterWriteDropsStaleRead(t *testing.T) {
	w := []proxy.Message{
		tcMsg("a", "read_file", `{"path":"x.go"}`), trMsg("a", "read_file", "x"),
		tcMsg("b", "edit_file", `{"path":"x.go","old_string":"a","new_string":"b"}`), trMsg("b", "edit_file", "ok"),
	}
	got := buildFindings(w, "/c/x.txt")
	if strings.Contains(got, "read (requested ranges)") || !strings.Contains(got, "written or edited: x.go") {
		t.Errorf("read before a write must not be listed as a known state:\n%s", got)
	}
}

func TestCommandWord(t *testing.T) {
	for in, want := range map[string]string{
		"go test ./...":                    "go",
		"cd sub && go test":                "go",
		"FOO=1 BAR=2 make build":           "make",
		"/usr/bin/env python3 x.py":        "env",
		`"quoted cmd" x`:                   "?",
		"":                                 "?",
		"$(evil) x":                        "?",
		strings.Repeat("a", 41) + " x":     "?",
		"cd /tmp":                          "?",
		"API_KEY=sk-secret-value curl url": "curl",
	} {
		if got := commandWord(in); got != want {
			t.Errorf("commandWord(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMergeRanges(t *testing.T) {
	got := formatRanges(mergeRanges([]readRange{{20, 30}, {1, 5}, {6, 10}, {25, 40}, {100, 1<<63 - 1}}))
	if want := ":1-10,20-40,100-"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := formatRanges(mergeRanges([]readRange{{5, 9}, {1, 1<<63 - 1}})); got != "" {
		t.Errorf("a whole-file read swallows the others and prints no range, got %q", got)
	}
}

// findingsApp is a long-session app whose conversation is one user task, a
// mixed working set built from findingsRange(), padded so a collapse is
// smaller than what it replaces, and a final assistant round.
func findingsApp(t *testing.T) *App {
	t.Helper()
	isolateToolCache(t)
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextCollapseMinBytes = 1
	app.Cfg.ContextCollapseCeilingTokens = 1000
	app.Out = new(strings.Builder)
	app.curTurn, app.turnSeq = 1, 1
	app.appendConv(proxy.Message{Role: "user", Content: StrPtr("fix the tokenizer")})
	pad := strings.Repeat("padding line of tool output\n", 400)
	for _, m := range findingsRange() {
		if m.Role == "tool" {
			c := *m.Content + "\n" + pad
			m.Content = &c
		}
		app.appendConv(m)
	}
	return app
}

func recFindings(t *testing.T, app *App) string {
	t.Helper()
	app.stateMu.RLock()
	defer app.stateMu.RUnlock()
	rec, ok := app.collapsed[1]
	if !ok {
		t.Fatal("no collapse record for turn 1")
	}
	return rec.Findings
}

// TestFindingsAllThreePaths: the checkpoint the model sees after each of the
// three collapse paths carries the same findings part, and none of the
// tool-result marker text.
func TestFindingsAllThreePaths(t *testing.T) {
	check := func(t *testing.T, name, block, findings string) {
		t.Helper()
		if !strings.Contains(findings, "b.go:10-74") || !strings.Contains(findings, "make (exit 7)") {
			t.Errorf("%s: findings lack the read range or the failed command:\n%s", name, findings)
		}
		if !strings.Contains(block, findingsHeader) || strings.Count(block, findingsHeader) != 1 {
			t.Errorf("%s: checkpoint must hold exactly one findings header:\n%s", name, block)
		}
		for _, bad := range []string{"TOKEN-IN-OUTPUT-MARKER", "RESULT-BODY", "padding line of tool output", "hunter2"} {
			if strings.Contains(block, bad) {
				t.Errorf("%s: checkpoint holds tool-result or argument bytes %q", name, bad)
			}
		}
	}

	t.Run("collapse_context tool", func(t *testing.T) {
		app := findingsApp(t)
		call := proxy.ToolCall{ID: "cc", Type: "function", Function: proxy.FunctionCall{Name: "collapse_context", Arguments: `{"summary":"s"}`}}
		app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{call}})
		res := app.handleCollapseContext(call)
		if !strings.Contains(res, "collapsed request 1") {
			t.Fatalf("collapse did not happen: %s", res)
		}
		app.stateMu.RLock()
		rec := app.collapsed[1]
		app.stateMu.RUnlock()
		check(t, "tool", buildPartialCheckpointBlock(rec), rec.Findings)
	})

	t.Run("mid-turn automatic", func(t *testing.T) {
		app := findingsApp(t)
		app.Cfg.ContextCollapseCeilingTokens = 1000
		app.collapseMidTurnPrefix()
		app.stateMu.RLock()
		rec, ok := app.collapsed[1]
		app.stateMu.RUnlock()
		if !ok {
			t.Fatal("mid-turn collapse did not happen")
		}
		check(t, "mid-turn", buildPartialCheckpointBlock(rec), rec.Findings)
	})

	t.Run("turn end", func(t *testing.T) {
		app := findingsApp(t)
		app.appendConv(proxy.Message{Role: "assistant", Content: StrPtr("All done: the tokenizer is fixed.")})
		app.collapseFinishedTurns()
		app.stateMu.RLock()
		rec, ok := app.collapsed[1]
		app.stateMu.RUnlock()
		if !ok || rec.Partial {
			t.Fatalf("turn-end collapse did not happen: %+v", rec)
		}
		var proj string
		for _, m := range app.contextProjection() {
			if m.Role == "assistant" && m.Content != nil && strings.Contains(*m.Content, "All done") {
				proj = *m.Content
			}
		}
		check(t, "turn end", proj, rec.Findings)
	})
}

// TestFindingsTwoCollapsesInARow: a second collapse recomputes the part from
// the cumulative range: still one header, entries of both ranges present.
func TestFindingsTwoCollapsesInARow(t *testing.T) {
	app := findingsApp(t)
	call1 := proxy.ToolCall{ID: "cc1", Type: "function", Function: proxy.FunctionCall{Name: "collapse_context", Arguments: `{"summary":"one"}`}}
	app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{call1}})
	if res := app.handleCollapseContext(call1); !strings.Contains(res, "collapsed request 1") {
		t.Fatalf("first collapse: %s", res)
	}
	app.appendConv(trMsg("cc1", "collapse_context", "collapsed"))
	pad := strings.Repeat("later padding line\n", 400)
	app.appendConv(tcMsg("z1", "read_file", `{"path":"later.go","offset":1,"limit":30}`))
	app.appendConv(trMsg("z1", "read_file", pad))
	app.appendConv(tcMsg("z2", "run_shell", `{"command":"npm test"}`))
	app.appendConv(trMsg("z2", "run_shell", "ERROR: exit status 1\n"+pad))
	call2 := proxy.ToolCall{ID: "cc2", Type: "function", Function: proxy.FunctionCall{Name: "collapse_context", Arguments: `{"summary":"two"}`}}
	app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{call2}})
	if res := app.handleCollapseContext(call2); !strings.Contains(res, "collapsed request 1") {
		t.Fatalf("second collapse: %s", res)
	}
	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	block := buildPartialCheckpointBlock(rec)
	if n := strings.Count(block, findingsHeader); n != 1 {
		t.Fatalf("findings header appears %d times (nested):\n%s", n, block)
	}
	for _, want := range []string{"b.go:10-74", "later.go:1-30", "npm (exit 1)", "make (exit 7)"} {
		if !strings.Contains(block, want) {
			t.Errorf("merged findings lack %q:\n%s", want, block)
		}
	}
}

// TestFindingsFirstRangeCanBeEvicted pins the honest semantics: the part is
// recomputed over the cumulative range under one cap, so a first range that
// fills the cap is partly cut by the next one; the marker says so.
func TestFindingsFirstRangeCanBeEvicted(t *testing.T) {
	var w []proxy.Message
	for i := 0; i < 60; i++ {
		id := fmt.Sprintf("a%d", i)
		w = append(w, tcMsg(id, "read_file", fmt.Sprintf(`{"path":"first/range/file_%03d.go"}`, i)), trMsg(id, "read_file", "x"))
	}
	before := buildFindings(w, "/c/a.txt")
	for i := 0; i < 30; i++ {
		id := fmt.Sprintf("b%d", i)
		w = append(w, tcMsg(id, "read_file", fmt.Sprintf(`{"path":"second/range/file_%03d.go"}`, i)), trMsg(id, "read_file", "x"))
	}
	after := buildFindings(w, "/c/a.txt")
	if len(after) > maxFindingsBytes || len(before) > maxFindingsBytes {
		t.Fatalf("over the limit: %d / %d", len(before), len(after))
	}
	if !strings.Contains(after, "second/range/file_029.go") || !strings.Contains(after, " more)") {
		t.Errorf("newest entries must stay and the cut must be visible:\n%s", after)
	}
	if strings.Contains(after, "first/range/file_000.go") {
		t.Errorf("oldest first-range entry should have been cut:\n%s", after)
	}
}

// TestFindingsKeepTailUnderHardMax: with findings in the checkpoint, the kept
// tail plus the checkpoint still fits under the hardMax clamp (30% of it),
// apart from the one round that is always kept.
func TestFindingsKeepTailUnderHardMax(t *testing.T) {
	isolateToolCache(t)
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.curTurn, app.turnSeq = 1, 1
	app.Cfg.ContextCollapseCeilingTokens = 200000
	app.Cfg.ContextCollapseMinBytes = 1
	app.Cfg.HardMaxBytes = 100000
	app.Out = new(strings.Builder)
	app.appendConv(proxy.Message{Role: "user", Content: StrPtr(strings.Repeat("task ", 5000))})
	const roundSize = 10000
	for i := 1; i <= 12; i++ {
		id := fmt.Sprintf("c%d", i)
		app.appendConv(tcMsg(id, "read_file", fmt.Sprintf(`{"path":"pkg/file_%02d.go","offset":1,"limit":100}`, i)))
		app.appendConv(trMsg(id, "read_file", strings.Repeat("x", roundSize)))
	}
	app.collapseMidTurnPrefix()
	app.stateMu.RLock()
	rec, ok := app.collapsed[1]
	app.stateMu.RUnlock()
	if !ok || rec.Findings == "" {
		t.Fatalf("expected a partial collapse with findings: ok=%v findings=%q", ok, rec.Findings)
	}
	idx := findBoundaryAssistantIdx(app.Conv, turnSpansFromSlice(app.Conv)[1], rec.BoundaryCallID)
	tail := 0
	for i := idx; i < len(app.Conv); i++ {
		tail += messageSize(app.Conv[i])
	}
	hardCap := app.Cfg.HardMaxBytes * 30 / 100
	if got := tail + len(rec.Findings); got > hardCap+roundSize+100 {
		t.Errorf("tail %d + findings %d = %d exceeds the clamp %d (+ the one always-kept round %d)", tail, len(rec.Findings), got, hardCap, roundSize)
	}
	assertNoOrphanedToolCalls(t, app.contextProjection())
}

// TestFindingsNoticeOneLine: a collapse with findings still writes exactly
// one notice line: on a.Out without a sink, as one SysNoteMsg with a sink.
func TestFindingsNoticeOneLine(t *testing.T) {
	collapse := func(app *App) {
		call := proxy.ToolCall{ID: "cc", Type: "function", Function: proxy.FunctionCall{Name: "collapse_context", Arguments: `{"summary":"s"}`}}
		app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{call}})
		if res := app.handleCollapseContext(call); !strings.Contains(res, "collapsed request 1") {
			t.Fatalf("collapse: %s", res)
		}
	}
	t.Run("no sink (headless fallback)", func(t *testing.T) {
		app := findingsApp(t)
		var out strings.Builder
		app.Out = &out
		collapse(app)
		if n := strings.Count(out.String(), "collapsed request 1:"); n != 1 {
			t.Errorf("notice lines on a.Out = %d, want 1: %q", n, out.String())
		}
		if strings.Contains(out.String(), findingsHeader) {
			t.Errorf("findings leaked into the notice: %q", out.String())
		}
	})
	t.Run("sink (TUI path)", func(t *testing.T) {
		app := findingsApp(t)
		var out strings.Builder
		app.Out = &out
		var notes []string
		app.EventSink = func(msg any) {
			if sn, ok := msg.(SysNoteMsg); ok {
				notes = append(notes, sn.Text)
			}
		}
		collapse(app)
		n := 0
		for _, s := range notes {
			if strings.Contains(s, "collapsed request 1:") {
				n++
				if strings.Contains(s, "\n") || strings.Contains(s, findingsHeader) {
					t.Errorf("notice is not one plain line: %q", s)
				}
			}
		}
		if n != 1 || strings.Contains(out.String(), "collapsed request 1:") {
			t.Errorf("sink notices = %d (want 1), a.Out = %q (want none)", n, out.String())
		}
	})
}

// TestFindingsSearchHintWorksOnOwnArchive: the search hint in the part is
// usable: search_files on this session's archive path finds a term.
func TestFindingsSearchHintWorksOnOwnArchive(t *testing.T) {
	app := findingsApp(t)
	call := proxy.ToolCall{ID: "cc", Type: "function", Function: proxy.FunctionCall{Name: "collapse_context", Arguments: `{"summary":"s"}`}}
	app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{call}})
	app.handleCollapseContext(call)
	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	if rec.Archive == "" {
		t.Fatal("no archive")
	}
	res := app.handleSearchFiles(context.Background(), proxy.ToolCall{ID: "s", Function: proxy.FunctionCall{
		Name: "search_files", Arguments: fmt.Sprintf(`{"pattern":"RESULT-BODY-ONE","path":%q}`, rec.Archive)}})
	if !strings.Contains(res, "RESULT-BODY-ONE") {
		t.Errorf("search_files on the archive path did not find the term: %q", res)
	}
}
