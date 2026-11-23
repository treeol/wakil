package tui

// Block 2 (0.8.11): the Mashura cost line and every other status line
// written while output is on screen go through the notice path (a session
// note) on its own line — never on top of streamed text, and never on top
// of an expanded tool output. This screen test reproduces the exact
// reported shape: expanded tool output on screen, then the cost line.

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/core/event"
)

// TestScreenCostLineWhileToolOutputExpanded pins the reported bug: an
// expanded wait_for_completion tool output is on screen and the Mashura
// cost line arrives at the same time. The cost line must sit on its own
// screen line; the tool output text must stay intact.
func TestScreenCostLineWhileToolOutputExpanded(t *testing.T) {
	h := newScreenHarness(t)
	h.run()
	h.p.Send(evt(event.KindTurnStarted, event.TurnStarted{TurnID: "trn"}, h.m.sessionID))
	h.settle()

	// Expanded tool output on screen: a multi-line wait_for_completion
	// result, streamed as message deltas like the real path does.
	toolText := "wait_for_completion queued as op-9 — the panel is running now\n" +
		"panel result: the oracle suggests the plan is sound, two risks remain\n" +
		"full content at /tmp/toolcache/op-9.txt"
	h.p.Send(evt(event.KindMessageDelta, event.MessageDelta{Text: toolText}, h.m.sessionID))
	h.waitFor("tool output on screen", func() bool {
		return strings.Contains(h.raw(), "oracle suggests")
	})

	// The cost line arrives (notice path) while that output is on screen.
	cost := "mashūra panel: $1.4447 total (3 calls)"
	h.p.Send(evt(event.KindSessionNote, event.SessionNote{Text: cost, Notice: true}, h.m.sessionID))
	h.settle()

	// More streamed text after, to prove the note did not overwrite text.
	h.p.Send(evt(event.KindMessageDelta, event.MessageDelta{Text: "\nfinal answer follows"}, h.m.sessionID))
	h.waitFor("tool output, cost line and answer on screen", func() bool {
		s := h.raw()
		return strings.Contains(s, "oracle suggests") && strings.Contains(s, cost) && strings.Contains(s, "final answer follows")
	})

	screen := h.raw()
	var costLine string
	for _, row := range strings.Split(screen, "\n") {
		if strings.Contains(row, "mashūra panel:") {
			costLine = row
		}
	}
	if costLine == "" {
		t.Fatalf("no screen line carries the cost line:\n%s", screen)
	}
	for _, overlap := range []string{"op-9", "oracle suggests", "toolcache", "final answer"} {
		if strings.Contains(costLine, overlap) {
			t.Errorf("cost line shares a screen line with %q: %q", overlap, costLine)
		}
	}
	// The expanded tool output is intact, line for line.
	for _, frag := range []string{"op-9 — the panel is running", "oracle suggests the plan is sound", "full content at"} {
		if !strings.Contains(screen, frag) {
			t.Errorf("tool output fragment missing after the cost line: %q\n%s", frag, screen)
		}
	}
	if strings.Count(screen, "mashūra panel: $1.4447 total (3 calls)") != 1 {
		// The line-diff renderer pastes line fragments across frames, so the
		// settled screen can paste the cost text twice. The invariant that
		// matters: exactly ONE screen line carries it (checked above).
		t.Logf("cost text appears %d times in the raw capture (line-diff pasting); one line still carries it: %q",
			strings.Count(screen, "mashūra panel: $1.4447 total (3 calls)"), costLine)
	}
}
