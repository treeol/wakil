package agent

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// TestAsyncResultsNoticeThroughNoticePath pins the fix for the status line
// "· async results delivered" that landed inside the streamed answer text: the
// line must go through the notice path (SysNoteMsg), never a.Out, when the
// TUI is attached.
func TestAsyncResultsNoticeThroughNoticePath(t *testing.T) {
	app := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	var out strings.Builder
	app.Out = &out

	var notes []SysNoteMsg
	app.EventSink = func(msg interface{}) {
		if sn, ok := msg.(SysNoteMsg); ok {
			notes = append(notes, sn)
		}
	}

	enqueueTerminalOp(t, app, app.chatID(), []string{"m"}, "panel result")
	if env := app.drainAsyncInbox(); env == "" {
		t.Fatal("test premise: drain should deliver the terminal op")
	}
	app.emitNotice("· async results delivered")

	if len(notes) != 1 || notes[0].Text != "· async results delivered" {
		t.Fatalf("want one SysNoteMsg with the async notice, got %+v", notes)
	}
	if strings.Contains(out.String(), "async results delivered") {
		t.Errorf("async notice leaked to a.Out (assistant stream): %q", out.String())
	}
}

// TestCollapseContextToolNoticeOneLine pins Block 2: one model-called
// collapse_context produces exactly one collapse notice, routed through the
// notice path, and nothing in the assistant stream (a.Out).
func TestCollapseContextToolNoticeOneLine(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextCollapseMinBytes = 0
	app.MemoryStore = testStore(t)

	var out strings.Builder
	app.Out = &out
	var notes []SysNoteMsg
	app.EventSink = func(msg interface{}) {
		if sn, ok := msg.(SysNoteMsg); ok {
			notes = append(notes, sn)
		}
	}

	call := proxy.ToolCall{ID: "ccB2", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"summary of the work so far"}`}}
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("do it"), Turn: 1},
		tcMsg("tcB2", "read_file", `{"path":"a.go"}`),
		trMsg("tcB2", "read_file", strings.Repeat("payload line\n", 400)),
		{Role: "assistant", ToolCalls: []proxy.ToolCall{call}, Turn: 1},
	}
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq, app.curTurn = 1, 1

	res := app.handleCollapseContext(call)
	if strings.HasPrefix(res, "ERROR") || strings.Contains(res, "skipped") {
		t.Fatalf("collapse did not run: %s", res)
	}
	var collapseNotes []SysNoteMsg
	for _, n := range notes {
		if strings.Contains(n.Text, "collapsed request") {
			collapseNotes = append(collapseNotes, n)
		}
	}
	if len(collapseNotes) != 1 {
		t.Fatalf("want exactly one collapse notice, got %d: %+v", len(collapseNotes), notes)
	}
	if strings.Contains(collapseNotes[0].Text, "\n") {
		t.Errorf("notice is not one line: %q", collapseNotes[0].Text)
	}
	if strings.Contains(out.String(), "collapsed request") {
		t.Errorf("collapse notice leaked to a.Out (assistant stream): %q", out.String())
	}
}
