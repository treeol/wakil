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
	// The raw capture is the line-diff stream, not a final screen: a line
	// whose bytes are identical across two consecutive paints is emitted
	// twice (renderer redraw), so the cost text legitimately appears on
	// two captured "lines" while being drawn once. The invariant that
	// matters (the bug shape): the cost line must sit on its own screen
	// line, never inside a streamed text line. We check: every captured
	// line carrying it carries ONLY it (plus padding), and the tool
	// output text stays intact on its own lines.
	var costLines []string
	for _, row := range strings.Split(screen, "\n") {
		if strings.Contains(row, "mashūra panel:") {
			costLines = append(costLines, row)
		}
	}
	if len(costLines) == 0 {
		t.Fatalf("no screen line carries the cost line:\n%s", screen)
	}
	costLine := costLines[0]
	for _, overlap := range []string{"op-9", "oracle suggests", "toolcache", "final answer"} {
		if strings.Contains(costLine, overlap) {
			t.Errorf("cost line shares a screen line with %q: %q", overlap, costLine)
		}
	}
	// Every drawn instance of the cost line is just the cost line (plus
	// box padding): a joined instance would carry streamed text.
	if len(costLines) > 1 {
		t.Logf("cost line drawn %d times in the raw capture (%d line-diff paints); all are padding-only rows", len(costLines), len(costLines))
	}
	for i, row := range costLines {
		// Strip the box border characters the frame draws around the line.
		trimmed := strings.Trim(row, " \t│\r")
		if trimmed != "mashūra panel: $1.4447 total (3 calls)" {
			t.Errorf("cost line #%d carries more than the cost text: %q (trimmed %q)", i, row, trimmed)
		}
	}
	// The expanded tool output is intact, line for line.
	for _, frag := range []string{"op-9 — the panel is running", "oracle suggests the plan is sound", "full content at"} {
		if !strings.Contains(screen, frag) {
			t.Errorf("tool output fragment missing after the cost line: %q\n%s", frag, screen)
		}
	}
}
