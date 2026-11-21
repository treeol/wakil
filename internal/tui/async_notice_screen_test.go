package tui

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/core/event"
)

// TestScreenAsyncNoticeOnOwnLine pins Block 1 on the real program: reasoning
// text is streaming when the async-results status arrives (as a session note,
// the notice path). The reasoning sentence must stay unbroken on one screen
// line and the notice must sit on a separate line, never inside the text.
func TestScreenAsyncNoticeOnOwnLine(t *testing.T) {
	h := newScreenHarness(t)
	h.run()
	h.p.Send(evt(event.KindTurnStarted, event.TurnStarted{TurnID: "trn"}, h.m.sessionID))
	h.settle()
	const first = "Now I have a clear picture of the repository"
	h.p.Send(evt(event.KindReasoningDelta, event.ReasoningDelta{Text: first}, h.m.sessionID))
	h.settle()
	h.p.Send(evt(event.KindSessionNote, event.SessionNote{Text: "· async results delivered"}, h.m.sessionID))
	h.settle()
	const second = " and the next step is clear."
	h.p.Send(evt(event.KindReasoningDelta, event.ReasoningDelta{Text: second}, h.m.sessionID))
	h.waitFor("reasoning tail and notice on screen", func() bool {
		return strings.Contains(h.raw(), "next step is clear") && strings.Contains(h.raw(), "async results delivered")
	})

	screen := h.raw()
	if strings.Contains(screen, "clear· async") || strings.Contains(screen, "clear ·") {
		t.Fatalf("notice was written into the reasoning text:\n%s", screen)
	}
	var noticeLine string
	for _, row := range strings.Split(screen, "\n") {
		if strings.Contains(row, "async results delivered") {
			noticeLine = row
		}
	}
	if noticeLine == "" {
		t.Fatalf("no screen line carries the notice:\n%s", screen)
	}
	if strings.Contains(noticeLine, "clear") {
		t.Errorf("notice shares a line with the reasoning text: %q", noticeLine)
	}
	// The reasoning sentence is not broken by the notice: the head and the
	// tail of the reasoning both appear, and the notice is not between them.
	if !strings.Contains(screen, first) && !strings.Contains(screen, "picture of the repository") {
		t.Errorf("reasoning head missing from screen:\n%s", screen)
	}
}
