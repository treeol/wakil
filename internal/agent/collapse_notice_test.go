package agent

import (
	"fmt"
	"strings"
	"testing"
)

// --- A1: truthful collapse-notice sizes ------------------------------------
//
// The end-of-turn collapse notice used to print a hardcoded "0.4k sent" —
// the actual projected size (U + A + note) is often several KB, so the
// notice systematically overstated the shrink (observed live: notice said
// 0.4k while the ctx gauge dropped 13k). The notice must report the real
// projected byte count.

// TestCollapseNoticeReportsProjectedSize collapses a real turn whose A is
// several KB and asserts the printed "… → Nk sent" matches the projected
// U + A + note size (rounded down to k), not a hardcoded value.
func TestCollapseNoticeReportsProjectedSize(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	var out strings.Builder
	app.Out = &out

	// One collapsible turn with a multi-KB answer: the fixture answer is
	// tiny ("Here is the answer."), so give turn 1 a large A explicitly.
	conv := oneCollapsedTurn(1)
	for i := range conv {
		conv[i].Turn = 1
	}
	largeAnswer := strings.Repeat("Here is a long and detailed final answer. ", 120) // ~5.5k
	conv[len(conv)-1].Content = StrPtr(largeAnswer)
	app.Conv = conv
	app.turnSeq = 1

	app.collapseFinishedTurns()

	app.stateMu.RLock()
	rec, ok := app.collapsed[1]
	app.stateMu.RUnlock()
	if !ok {
		t.Fatal("turn 1 should have collapsed")
	}

	// Expected projected size: U + A + note (messageSize of first + last,
	// plus the note text) — the same arithmetic the notice prints.
	projected := messageSize(app.Conv[0]) + messageSize(app.Conv[len(app.Conv)-1]) + len(rec.Note)
	wantLine := fmt.Sprintf("· collapsed request 1 — %dk → %dk sent (archived)", rec.WBytes/1024, projected/1024)

	if !strings.Contains(out.String(), wantLine) {
		t.Errorf("notice mismatch.\nwant: %s\ngot:  %s", wantLine, out.String())
	}
	if strings.Contains(out.String(), "0.4k sent") {
		t.Error("notice still reports the old hardcoded 0.4k value")
	}
}

// TestCollapseNoticeProjectedMatchesProjection cross-checks the notice's
// arithmetic against the actual projection: the emitted U + A messages
// must measure the same bytes the notice claimed (within the note that the
// projection appends to A's content).
func TestCollapseNoticeProjectedMatchesProjection(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Conv = oneCollapsedTurn(1)
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1

	app.collapseFinishedTurns()

	app.stateMu.RLock()
	rec, ok := app.collapsed[1]
	app.stateMu.RUnlock()
	if !ok {
		t.Fatal("turn 1 should have collapsed")
	}

	proj := app.contextProjection()
	if len(proj) != 2 {
		t.Fatalf("projection: want U+A (2 msgs), got %d", len(proj))
	}
	// The projection's A includes the note; the notice's projected size
	// counts U + A + note separately — both must agree within rounding.
	projBytes := messageSize(proj[0]) + messageSize(proj[1])
	noticeBytes := messageSize(app.Conv[0]) + messageSize(app.Conv[len(app.Conv)-1]) + len(rec.Note)
	if abs(projBytes-noticeBytes) > 8 { // "\n\n" join + rounding slack
		t.Errorf("notice arithmetic disagrees with projection: notice=%d proj=%d", noticeBytes, projBytes)
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// TestCollapseNoticeSmallTurnZeroK guards the degenerate display: a turn
// whose projected size rounds to 0k prints "0k sent" (not a stale 0.4k).
func TestCollapseNoticeSmallTurnZeroK(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	var out strings.Builder
	app.Out = &out

	app.Conv = oneCollapsedTurn(1)
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1

	app.collapseFinishedTurns()

	app.stateMu.RLock()
	rec, ok := app.collapsed[1]
	app.stateMu.RUnlock()
	if !ok {
		t.Fatal("turn 1 should have collapsed")
	}
	projected := messageSize(app.Conv[0]) + messageSize(app.Conv[len(app.Conv)-1]) + len(rec.Note)
	want := fmt.Sprintf("→ %dk sent", projected/1024)
	if !strings.Contains(out.String(), want) {
		t.Errorf("small-turn notice mismatch: want %q in %q", want, out.String())
	}
}
