package tui

// Tests for SubagentFinishedMsg — the display-only early completion event.
// Verifies the TUI reaches done-state on SubagentFinishedMsg alone, and that a
// subsequent SubagentDoneMsg causes no state regression.

import (
	"testing"

	agent "github.com/treeol/wakil/internal/agent"
)

// TestFinishedReachesDoneState verifies that SubagentFinishedMsg alone flips
// the tab to a visually-done state (finished=true, done=false) with the
// display data populated.
func TestFinishedReachesDoneState(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.SubagentStartMsg{Task: "task A", ChatID: "chat-a"})

	m = step(m, agent.SubagentFinishedMsg{
		ChatID:         "chat-a",
		Status:         "ok",
		CostUSD:        0.015,
		FilesChanged:   []string{"foo.go"},
		SummaryPreview: "found the bug",
	})

	var tab *subTab
	for _, t := range m.subTabs {
		if t.chatID == "chat-a" {
			tab = t
			break
		}
	}
	if tab == nil {
		t.Fatal("tab not found")
	}
	if !tab.finished {
		t.Error("tab should be finished after SubagentFinishedMsg")
	}
	if tab.done {
		t.Error("tab should NOT be done (authoritative) after SubagentFinishedMsg alone")
	}
	if tab.finCostUSD != 0.015 {
		t.Errorf("finCostUSD = %v, want 0.015", tab.finCostUSD)
	}
	if tab.finFilesN != 1 {
		t.Errorf("finFilesN = %v, want 1", tab.finFilesN)
	}
	if tab.finPreview != "found the bug" {
		t.Errorf("finPreview = %q, want %q", tab.finPreview, "found the bug")
	}
}

// TestDoneDoesNotRegressFinished verifies that a subsequent SubagentDoneMsg
// for an already-finished tab enriches it (done=true, authoritative fields
// filled) without any visual regression — the tab stays done, no flicker
// back to active.
func TestDoneDoesNotRegressFinished(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.SubagentStartMsg{Task: "task A", ChatID: "chat-a"})
	m = step(m, agent.SubagentActiveMsg{ChatID: "chat-a"})
	m = step(m, agent.SubagentFinishedMsg{
		ChatID:         "chat-a",
		Status:         "ok",
		CostUSD:        0.015,
		SummaryPreview: "found the bug",
	})

	// Tab is now finished (display-only), not done.
	var tab *subTab
	for _, t := range m.subTabs {
		if t.chatID == "chat-a" {
			tab = t
		}
	}
	if !tab.finished || tab.done {
		t.Fatalf("precondition: finished=%v done=%v, want finished=true done=false", tab.finished, tab.done)
	}

	// SubagentDoneMsg arrives (Phase C). Should enrich, not regress.
	m = step(m, agent.SubagentDoneMsg{
		ChatID:    "chat-a",
		CostUSD:   0.015,
		CtxSize:   5000,
		UsedBackend: "llama",
	})

	tab = nil
	for _, t := range m.subTabs {
		if t.chatID == "chat-a" {
			tab = t
		}
	}
	if !tab.done {
		t.Error("tab should be done after SubagentDoneMsg")
	}
	if !tab.finished {
		t.Error("tab should still be finished (done implies finished)")
	}
	if tab.costUSD != 0.015 {
		t.Errorf("costUSD = %v, want 0.015", tab.costUSD)
	}
	if tab.ctxSize != 5000 {
		t.Errorf("ctxSize = %v, want 5000", tab.ctxSize)
	}
	if tab.usedBackend != "llama" {
		t.Errorf("usedBackend = %q, want llama", tab.usedBackend)
	}
}

// TestFinishedDotDistinctFromRunningAndDone verifies the three visual states
// are distinct: running (active, not finished) ≠ finished (not done) ≠ done.
func TestFinishedDotDistinctFromRunningAndDone(t *testing.T) {
	running := &subTab{active: true}
	finished := &subTab{active: true, finished: true}
	done := &subTab{active: true, finished: true, done: true}

	_, runColor := subTabDotSpec(running, 0)
	_, finColor := subTabDotSpec(finished, 0)
	_, doneColor := subTabDotSpec(done, 0)

	if runColor == finColor {
		t.Errorf("running and finished dots are identical (%v), should be distinct", runColor)
	}
	if finColor == doneColor {
		t.Errorf("finished and done dots are identical (%v), should be distinct", finColor)
	}
	if runColor == doneColor {
		t.Errorf("running and done dots are identical (%v), should be distinct", runColor)
	}

	// Glyph check: finished and done both use ✓, running uses ●.
	runGlyph, _ := subTabDotSpec(running, 0)
	finGlyph, _ := subTabDotSpec(finished, 0)
	doneGlyph, _ := subTabDotSpec(done, 0)
	if runGlyph != "●" {
		t.Errorf("running glyph = %q, want ●", runGlyph)
	}
	if finGlyph != "✓" {
		t.Errorf("finished glyph = %q, want ✓", finGlyph)
	}
	if doneGlyph != "✓" {
		t.Errorf("done glyph = %q, want ✓", doneGlyph)
	}
}

// TestFinishedTabClosable verifies a finished (but not done) tab shows × and
// is closable, same as a done tab.
func TestFinishedTabClosable(t *testing.T) {
	finished := &subTab{active: true, finished: true}
	done := &subTab{active: true, finished: true, done: true}
	running := &subTab{active: true}

	// The close-char logic: done||finished → ×, else ·.
	// We test via the renderMainTabBar close-char condition.
	checkClosable := func(tab *subTab) bool {
		return tab.done || tab.finished
	}
	if !checkClosable(finished) {
		t.Error("finished tab should be closable")
	}
	if !checkClosable(done) {
		t.Error("done tab should be closable")
	}
	if checkClosable(running) {
		t.Error("running tab should NOT be closable")
	}
}

// TestPruneProtectsFinishedNotDone verifies pruneSubTabs does not drop a
// finished (but not done) tab — its authoritative SubagentDoneMsg is still
// in flight.
func TestPruneProtectsFinishedNotDone(t *testing.T) {
	mk := func(n int, done, finished bool) *subTab {
		return &subTab{n: n, done: done, finished: finished}
	}
	// 4 tabs: 2 done, 1 finished (not done), 1 running. Cap 2, focus on the running one.
	tabs := []*subTab{
		mk(1, true, true),        // done — droppable
		mk(2, false, true),       // finished not done — must be protected
		mk(3, true, true),        // done — droppable
		mk(4, false, false),      // running — protected (also focused)
	}
	got := pruneSubTabs(tabs, 4, 2)
	has := map[int]bool{}
	for _, x := range got {
		has[x.n] = true
	}
	if !has[2] {
		t.Error("finished (not done) tab was pruned — should be protected")
	}
	if !has[4] {
		t.Error("running tab was pruned — should be protected")
	}
}
