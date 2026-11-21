package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/trace"
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

// reasoningChunk is one SSE frame carrying a reasoning_content delta.
func reasoningChunk(s string) string {
	return fmt.Sprintf(`{"choices":[{"delta":{"reasoning_content":%q},"finish_reason":null}]}`, s)
}

// TestStreamTurnAsyncNoticeNotInStream drives the real streamTurn loop against
// a fake backend. An async result arrives while round 1 is streaming
// reasoning; round 2's loop top drains it and prints "async results
// delivered" while reasoning and answer text stream again. The notice must go
// through the notice sink exactly once and appear in neither the answer
// stream (a.Out) nor the reasoning stream.
func TestStreamTurnAsyncNoticeNotInStream(t *testing.T) {
	var app *App
	var mu sync.Mutex
	call := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n := call
		call++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		send := func(f string) { fmt.Fprintf(w, "data: %s\n\n", f); fl.Flush() }
		fl.Flush()
		switch n {
		case 0:
			send(reasoningChunk("Now I have a clear picture of the repository"))
			// The async result lands while this reasoning is still streaming.
			enqueueTerminalOp(t, app, app.chatID(), []string{"m"}, "panel result")
			send(reasoningChunk(" and the next step is clear."))
			for _, f := range toolCallFrames("c1", "run_shell", `{"command":"echo hi"}`) {
				send(f)
			}
		default:
			send(reasoningChunk("The panel agrees, so I can answer."))
			send(contentChunk("final answer text"))
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	defer srv.Close()

	app = newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.MaxToolIterations = 0
	var answer strings.Builder
	app.Out = &answer
	var notes []SysNoteMsg
	app.EventSink = func(msg any) {
		if sn, ok := msg.(SysNoteMsg); ok {
			notes = append(notes, sn)
		}
	}
	var reasoning strings.Builder
	rsink := proxy.Sink(func(s string) { reasoning.WriteString(s) })

	var tr []trace.ToolTrace
	final, _, err := app.streamTurn(context.Background(), "go", rsink, &tr)
	if err != nil {
		t.Fatalf("streamTurn: %v", err)
	}
	if final != "final answer text" {
		t.Fatalf("final = %q (the loop did not reach round 2)", final)
	}
	if !strings.Contains(reasoning.String(), "next step is clear.") || !strings.Contains(reasoning.String(), "panel agrees") {
		t.Fatalf("test premise: reasoning did not stream in both rounds: %q", reasoning.String())
	}
	for name, s := range map[string]string{"answer stream": answer.String(), "reasoning stream": reasoning.String()} {
		if strings.Contains(s, "async results delivered") {
			t.Errorf("notice text inside the %s: %q", name, s)
		}
	}
	var got []string
	for _, n := range notes {
		if strings.Contains(n.Text, "async results delivered") {
			got = append(got, n.Text)
		}
	}
	if len(got) != 1 {
		t.Fatalf("notice sink got %d async notices %v, want exactly 1 (all notes: %+v)", len(got), got, notes)
	}
}

// noticeFallbackOut runs the two notices that use the fallback (the async one
// and a collapse notice) with no event sink attached and returns what reached
// a.Out.
func noticeFallbackOut(t *testing.T, out io.Writer) string {
	t.Helper()
	isolateToolCache(t)
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextCollapseMinBytes = 1
	app.Out = out
	app.emitNotice("· async results delivered")
	conv := oneCollapsedTurn(1)
	for i := range conv {
		conv[i].Turn = 1
	}
	app.Conv = conv
	app.turnSeq = 1
	app.collapseFinishedTurns()
	return fmt.Sprint(out)
}

// TestNoticeFallbackNoEscapeWithoutTerminal: with no terminal (a buffer, a
// pipe, a file) the collapse and async notices carry no escape sequence, with
// or without NO_COLOR.
func TestNoticeFallbackNoEscapeWithoutTerminal(t *testing.T) {
	for _, noColor := range []string{"", "1"} {
		t.Setenv("NO_COLOR", noColor)
		var buf strings.Builder
		got := noticeFallbackOut(t, &buf)
		if !strings.Contains(got, "async results delivered") || !strings.Contains(got, "collapsed request 1:") {
			t.Fatalf("NO_COLOR=%q: notices missing: %q", noColor, got)
		}
		if strings.Contains(got, "\x1b") {
			t.Errorf("NO_COLOR=%q: escape sequence without a terminal: %q", noColor, got)
		}
	}
	// A real file that is not a terminal.
	f, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	noticeFallbackOut(t, f)
	b, _ := os.ReadFile(f.Name())
	if len(b) == 0 || strings.Contains(string(b), "\x1b") {
		t.Errorf("file output empty or styled: %q", b)
	}
}

// TestNoticeFallbackTerminalStyling: on a terminal the line is dimmed, unless
// NO_COLOR is set to a non-empty value (an empty NO_COLOR does not disable
// colour, per no-color.org).
func TestNoticeFallbackTerminalStyling(t *testing.T) {
	old := isTerminalFile
	isTerminalFile = func(*os.File) bool { return true }
	t.Cleanup(func() { isTerminalFile = old })
	for _, tc := range []struct {
		noColor string
		styled  bool
	}{{"", true}, {"1", false}, {"0", false}} {
		t.Setenv("NO_COLOR", tc.noColor)
		f, err := os.CreateTemp(t.TempDir(), "tty")
		if err != nil {
			t.Fatal(err)
		}
		app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
		app.Out = f
		app.emitNotice("· async results delivered")
		f.Close()
		b, _ := os.ReadFile(f.Name())
		if got := strings.Contains(string(b), "\x1b[2m"); got != tc.styled {
			t.Errorf("NO_COLOR=%q: styled=%v, want %v: %q", tc.noColor, got, tc.styled, b)
		}
	}
}
