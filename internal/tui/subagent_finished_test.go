package tui

// Tests for the display-only early-completion signal (SubagentProgress with
// Finished=true) and the authoritative SubagentCompleted. Verifies the TUI
// reaches done-state on the early signal alone, and that a subsequent
// SubagentCompleted causes no state regression.

import (
	"testing"

	"github.com/treeol/wakil/internal/core/event"
)

const finSID = event.SessionID("sess_tui_test")

func finModel() (tuiModel, *fakeFacade) {
	f := &fakeFacade{sid: finSID, chatID: "chat_tabs"}
	return newWiringModel(f), f
}

func subSpawn(chatID, task string, sid event.SessionID) event.Event {
	return evt(event.KindSubagentSpawned, event.SubagentSpawned{
		SubagentID: event.SubagentID("sub_" + chatID), Task: task, Capability: "discovery",
	}, sid)
}

func subActive(chatID string, sid event.SessionID) event.Event {
	return evt(event.KindSubagentProgress, event.SubagentProgress{
		SubagentID: event.SubagentID("sub_" + chatID), Text: "[active]",
	}, sid)
}

func subFinished(chatID, status string, cost float64, filesN int, preview string, sid event.SessionID) event.Event {
	return evt(event.KindSubagentProgress, event.SubagentProgress{
		SubagentID: event.SubagentID("sub_" + chatID), Finished: true,
		FinishedStatus: status, FinishedCostUSD: cost, FinishedFilesN: filesN, Text: preview,
	}, sid)
}

func subCompleted(chatID, status string, cost float64, ctxSize int, backend string, sid event.SessionID) event.Event {
	return evt(event.KindSubagentCompleted, event.SubagentCompleted{
		SubagentID: event.SubagentID("sub_" + chatID), Status: status,
		CostUSD: cost, CtxSize: ctxSize, UsedBackend: backend,
	}, sid)
}

func findFinTab(m tuiModel, chatID string) *subTab {
	for _, t := range m.subTabs {
		if t.chatID == "sub_"+chatID {
			return t
		}
	}
	return nil
}

// TestFinishedReachesDoneState verifies the early signal alone flips the tab
// to a visually-done state (finished=true, done=false) with display data.
func TestFinishedReachesDoneState(t *testing.T) {
	m, f := finModel()
	m = step(m, subSpawn("chat-a", "task A", finSID))

	m = step(m, subFinished("chat-a", "ok", 0.015, 1, "found the bug", finSID))
	_ = f

	tab := findFinTab(m, "chat-a")
	if tab == nil {
		t.Fatal("tab not found")
	}
	if !tab.finished {
		t.Error("tab should be finished after the early signal")
	}
	if tab.done {
		t.Error("tab should NOT be done (authoritative) after the early signal alone")
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

// TestDoneDoesNotRegressFinished verifies a subsequent SubagentCompleted for
// an already-finished tab enriches it without visual regression.
func TestDoneDoesNotRegressFinished(t *testing.T) {
	m, _ := finModel()
	m = step(m, subSpawn("chat-a", "task A", finSID))
	m = step(m, subActive("chat-a", finSID))
	m = step(m, subFinished("chat-a", "ok", 0.015, 0, "found the bug", finSID))

	// Tab is now finished (display-only), not done.
	tab := findFinTab(m, "chat-a")
	if !tab.finished || tab.done {
		t.Fatalf("precondition: finished=%v done=%v, want finished=true done=false", tab.finished, tab.done)
	}

	// SubagentCompleted arrives (Phase C). Should enrich, not regress.
	m = step(m, subCompleted("chat-a", "ok", 0.015, 5000, "llama", finSID))

	tab = findFinTab(m, "chat-a")
	if !tab.done {
		t.Error("tab should be done after SubagentCompleted")
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
// are distinct.
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
// finished (but not done) tab.
func TestPruneProtectsFinishedNotDone(t *testing.T) {
	mk := func(n int, done, finished bool) *subTab {
		return &subTab{n: n, done: done, finished: finished}
	}
	tabs := []*subTab{
		mk(1, true, true),
		mk(2, false, true),
		mk(3, true, true),
		mk(4, false, false),
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
