package agent

import (
	"fmt"
	"strings"
	"testing"
)

// --- A1: truthful collapse-notice sizes ------------------------------------
//
// The end-of-turn collapse notice used to print a hardcoded "0.4k sent" —
// the actual projected size (U + A + note, answer subject to the
// context_tail_answer_bytes cap) is often several KB, so the notice
// systematically overstated the shrink (observed live: notice said 0.4k
// while the ctx gauge dropped 13k). The notice must report the real
// projected byte count, computed by the SAME rendering the projection uses
// (projectTailAnswerContent) so the two cannot drift.

// TestCollapseNoticeReportsProjectedSize collapses a real turn whose A is
// several KB — exceeding the default 500-byte tail-answer cap, so the cap,
// the note join, and the notice all interact — and asserts the printed
// "… → Nk sent" matches the notice arithmetic AND the actual projection.
func TestCollapseNoticeReportsProjectedSize(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	var out strings.Builder
	app.Out = &out
	if app.Cfg.ContextTailAnswerBytes != 500 {
		t.Fatalf("test premise: default cap should be 500, got %d", app.Cfg.ContextTailAnswerBytes)
	}

	// One collapsible turn with a multi-KB answer (cap fires).
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

	// Notice arithmetic: U + projected A content (capped answer + note),
	// via the shared helper — identical to what the projection emits.
	projectedContent := projectTailAnswerContent(app.Conv[len(app.Conv)-1].Content, rec, app.Cfg.ContextTailAnswerBytes)
	projected := messageSize(app.Conv[0]) + len(*projectedContent)
	wantLine := fmt.Sprintf("· collapsed request 1 — %dk → %dk sent (archived)", rec.WBytes/1024, projected/1024)

	if !strings.Contains(out.String(), wantLine) {
		t.Errorf("notice mismatch.\nwant: %s\ngot:  %s", wantLine, out.String())
	}
	if strings.Contains(out.String(), "0.4k sent") {
		t.Error("notice still reports the old hardcoded 0.4k value")
	}

	// Non-vacuous cross-check: the notice's number matches the ACTUAL
	// projection bytes for the turn (the cap fired — the projected A is
	// ~500 bytes, not 5.5k).
	proj := app.contextProjection()
	if len(proj) != 2 {
		t.Fatalf("projection: want U+A (2 msgs), got %d", len(proj))
	}
	projBytes := messageSize(proj[0]) + messageSize(proj[1])
	if abs(projBytes-projected) > 2 { // "\n\n" join already counted by both
		t.Errorf("notice disagrees with projection: notice=%d proj=%d (cap should make both ~%d)",
			projected, projBytes, messageSize(app.Conv[0])+500+len(rec.Note))
	}
	if projBytes >= len(largeAnswer) {
		t.Errorf("cap did not fire: projection %d bytes >= original answer %d", projBytes, len(largeAnswer))
	}
}

// TestCollapseNoticeSummaryWrapperCounted: upgrading from a partial record
// (which carries a Summary) must count the "[checkpoint summary: …]"
// wrapper in the notice — the wrapper is part of what the projection sends.
func TestCollapseNoticeSummaryWrapperCounted(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	var out strings.Builder
	app.Out = &out

	conv := oneCollapsedTurn(1)
	for i := range conv {
		conv[i].Turn = 1
	}
	app.Conv = conv
	app.turnSeq = 1

	// Pre-seed a partial record with a Summary — the sweep upgrades it to
	// a full collapse, preserving Summary.
	app.stateMu.Lock()
	app.collapsed = map[int]CollapseRecord{1: {
		Turn: 1, Partial: true, Summary: "phase one wrapped up cleanly",
		BoundaryCallID: "cc1",
	}}
	app.stateMu.Unlock()

	app.collapseFinishedTurns()

	app.stateMu.RLock()
	rec, ok := app.collapsed[1]
	app.stateMu.RUnlock()
	if !ok || rec.Partial {
		t.Fatal("precondition: partial should have upgraded to full")
	}
	if rec.Summary == "" {
		t.Fatal("precondition: summary should be preserved on upgrade")
	}

	projectedContent := projectTailAnswerContent(app.Conv[len(app.Conv)-1].Content, rec, app.Cfg.ContextTailAnswerBytes)
	if !strings.Contains(*projectedContent, "[checkpoint summary: phase one wrapped up cleanly]") {
		t.Fatalf("projected content missing summary wrapper: %q", *projectedContent)
	}
	projected := messageSize(app.Conv[0]) + len(*projectedContent)
	wantLine := fmt.Sprintf("→ %dk sent", projected/1024)
	if !strings.Contains(out.String(), wantLine) {
		t.Errorf("notice with summary wrapper mismatch: want %q in %q", wantLine, out.String())
	}
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
	projectedContent := projectTailAnswerContent(app.Conv[len(app.Conv)-1].Content, rec, app.Cfg.ContextTailAnswerBytes)
	projected := messageSize(app.Conv[0]) + len(*projectedContent)
	want := fmt.Sprintf("→ %dk sent", projected/1024)
	if !strings.Contains(out.String(), want) {
		t.Errorf("small-turn notice mismatch: want %q in %q", want, out.String())
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
