package agent

// Block 2 (0.8.11) follow-ups from the diff review: emitNotice nil-safety
// (printMashuraPanelOutcome dropped its a.Out==nil guard) and the styled-
// vs-plain contract through the sink.

import (
	"strings"
	"testing"
)

// TestEmitNoticeNilSafety: a nil App and an App with a nil Out must not
// panic and must not touch the sink.
func TestEmitNoticeNilSafety(t *testing.T) {
	var nilApp *App
	nilApp.emitNotice("x") // must not panic

	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Out = nil
	var notes int
	app.EventSink = func(interface{}) { notes++ }
	app.emitNotice("x")
	if notes != 1 {
		t.Errorf("sink should have received the nil-Out notice (sink wins over Out), got %d", notes)
	}

	// Nil Out + no sink → drop, no panic.
	app.EventSink = nil
	app.emitNotice("x")
}

// TestEmitNoticeStyledThroughSink: a pre-styled notice that goes through
// the sink path arrives verbatim (the TUI renders it); a later unstyled
// notice arrives unstyled — emitNotice never (double-)styles.
func TestEmitNoticeStyledThroughSink(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Out = &strings.Builder{}
	var got []string
	app.EventSink = func(msg interface{}) {
		if sn, ok := msg.(SysNoteMsg); ok {
			got = append(got, sn.Text)
		}
	}
	styled := Yellow("⚠ styled warning")
	app.emitNotice(styled)
	app.emitNotice("plain cost line")
	if len(got) != 2 {
		t.Fatalf("want 2 sink notices, got %d", len(got))
	}
	if got[0] != styled {
		t.Errorf("styled notice must arrive verbatim: got %q want %q", got[0], styled)
	}
	if got[1] != "plain cost line" {
		t.Errorf("plain notice must arrive unstyled: got %q", got[1])
	}
}
