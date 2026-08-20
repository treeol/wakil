package tui

// Tests for subTabCloseMsg: auto-close of done subagent tabs after 30s.

import (
	"testing"

	"github.com/treeol/wakil/internal/core/event"
)

// TestSubagentDoneClearsActive verifies card #134: a done subagent tab is no
// longer active (the active flag is cleared).
func TestSubagentDoneClearsActive(t *testing.T) {
	m := newTabModel()
	m = step(m, evt(event.KindSubagentSpawned, event.SubagentSpawned{SubagentID: "sub_chat-a", Task: "task A", Capability: "discovery"}, tabSID))
	m = step(m, evt(event.KindSubagentProgress, event.SubagentProgress{SubagentID: "sub_chat-a", Text: "[active]"}, tabSID)) // queued → running
	tab := findSubTab(m, "sub_chat-a")
	if tab == nil {
		t.Fatal("subagent tab not created")
	}
	if !tab.active {
		t.Fatal("precondition: tab should be active after SubagentActiveMsg")
	}

	m = step(m, evt(event.KindSubagentCompleted, event.SubagentCompleted{SubagentID: "sub_chat-a", Status: "ok"}, tabSID))
	tab = findSubTab(m, "sub_chat-a")
	if tab == nil {
		t.Fatal("tab missing after Done")
	}
	if tab.active {
		t.Error("done subagent tab still active=true (card #134)")
	}
	if !tab.done || !tab.finished {
		t.Errorf("tab done=%v finished=%v, want both true", tab.done, tab.finished)
	}
}

// TestSubTabCloseMsgRemovesDoneUnfocusedTab verifies that a subTabCloseMsg
// removes a done tab when the user is not focused on it.
func TestSubTabCloseMsgRemovesDoneUnfocusedTab(t *testing.T) {
	m := newTabModel()
	m = step(m, evt(event.KindSubagentSpawned, event.SubagentSpawned{SubagentID: "sub_chat-a", Task: "task A", Capability: "discovery"}, tabSID))
	m = step(m, evt(event.KindSubagentSpawned, event.SubagentSpawned{SubagentID: "sub_chat-b", Task: "task B", Capability: "discovery"}, tabSID))

	// Both tabs become done.
	m = step(m, evt(event.KindSubagentCompleted, event.SubagentCompleted{SubagentID: "sub_chat-a", Status: "ok"}, tabSID))
	m = step(m, evt(event.KindSubagentCompleted, event.SubagentCompleted{SubagentID: "sub_chat-b", Status: "ok"}, tabSID))

	// User is on main tab (subCur == -1), so neither is focused.
	if m.subCur != -1 {
		t.Fatalf("precondition: subCur = %d, want -1 (main)", m.subCur)
	}
	if len(m.subTabs) != 2 {
		t.Fatalf("precondition: %d tabs, want 2", len(m.subTabs))
	}

	// Auto-close tab A.
	m = step(m, subTabCloseMsg{ChatID: "sub_chat-a"})

	if len(m.subTabs) != 1 {
		t.Fatalf("after close: %d tabs, want 1", len(m.subTabs))
	}
	if m.subTabs[0].chatID != "sub_chat-b" {
		t.Errorf("remaining tab = %q, want chat-b", m.subTabs[0].chatID)
	}
}

// TestSubTabCloseMsgSkipsFocusedTab verifies that a subTabCloseMsg does NOT
// remove the tab the user is currently viewing.
func TestSubTabCloseMsgSkipsFocusedTab(t *testing.T) {
	m := newTabModel()
	m = step(m, evt(event.KindSubagentSpawned, event.SubagentSpawned{SubagentID: "sub_chat-a", Task: "task A", Capability: "discovery"}, tabSID))
	m = step(m, evt(event.KindSubagentCompleted, event.SubagentCompleted{SubagentID: "sub_chat-a", Status: "ok"}, tabSID))

	// Focus tab A.
	m.subCur = tabIndexByN(m.subTabs, 1)
	if m.subCur < 0 {
		t.Fatal("could not focus tab A")
	}

	// Auto-close fires — should skip because the tab is focused.
	m = step(m, subTabCloseMsg{ChatID: "sub_chat-a"})

	if len(m.subTabs) != 1 {
		t.Errorf("focused tab was removed: %d tabs, want 1", len(m.subTabs))
	}
}

// TestSubTabCloseMsgSkipsNotDoneTab verifies that a subTabCloseMsg does NOT
// remove a tab that is not yet done (safety against a stale timer).
func TestSubTabCloseMsgSkipsNotDoneTab(t *testing.T) {
	m := newTabModel()
	m = step(m, evt(event.KindSubagentSpawned, event.SubagentSpawned{SubagentID: "sub_chat-a", Task: "task A", Capability: "discovery"}, tabSID))

	// Tab is running (not done). A stale close message arrives.
	m = step(m, subTabCloseMsg{ChatID: "sub_chat-a"})

	if len(m.subTabs) != 1 {
		t.Errorf("running tab was removed: %d tabs, want 1", len(m.subTabs))
	}
}

// TestSubTabCloseMsgNoOpsOnMissingTab verifies that a subTabCloseMsg for a
// ChatID that no longer exists is a safe no-op.
func TestSubTabCloseMsgNoOpsOnMissingTab(t *testing.T) {
	m := newTabModel()
	m = step(m, evt(event.KindSubagentSpawned, event.SubagentSpawned{SubagentID: "sub_chat-a", Task: "task A", Capability: "discovery"}, tabSID))
	m = step(m, evt(event.KindSubagentCompleted, event.SubagentCompleted{SubagentID: "sub_chat-a", Status: "ok"}, tabSID))

	// Close a tab that doesn't exist.
	m = step(m, subTabCloseMsg{ChatID: "chat-nonexistent"})

	if len(m.subTabs) != 1 {
		t.Errorf("missing-tab close changed tab count: %d, want 1", len(m.subTabs))
	}
}

// TestSubTabCloseMsgReflowsOnLastTab verifies that removing the last subagent
// tab triggers a reflow (symmetric with the 0→1 reflow on SubagentStartMsg).
func TestSubTabCloseMsgReflowsOnLastTab(t *testing.T) {
	m := newTabModel()
	m = m.reflow() // compute viewport height for 200x50 with no tabs
	noTabVpH := m.vp.Height

	m = step(m, evt(event.KindSubagentSpawned, event.SubagentSpawned{SubagentID: "sub_chat-a", Task: "task A", Capability: "discovery"}, tabSID))
	// First tab appearance triggers reflow — viewport shrinks by 1 row (tab bar).
	withTabVpH := m.vp.Height
	if withTabVpH >= noTabVpH {
		t.Errorf("after start: vp height %d, want < %d (tab bar took a row)", withTabVpH, noTabVpH)
	}

	m = step(m, evt(event.KindSubagentCompleted, event.SubagentCompleted{SubagentID: "sub_chat-a", Status: "ok"}, tabSID))
	// Auto-close the only tab.
	m = step(m, subTabCloseMsg{ChatID: "sub_chat-a"})

	if len(m.subTabs) != 0 {
		t.Fatalf("expected 0 tabs, got %d", len(m.subTabs))
	}
	// Reflow should have restored the viewport height to the no-tab value.
	if m.vp.Height != noTabVpH {
		t.Errorf("after last-tab close: vp height %d, want %d (tab bar row reclaimed)", m.vp.Height, noTabVpH)
	}
}

// TestSubTabCloseMsgFixesSubCur verifies that after removing a tab before the
// focused one, subCur is correctly remapped via tabIndexByN.
func TestSubTabCloseMsgFixesSubCur(t *testing.T) {
	m := newTabModel()
	m = step(m, evt(event.KindSubagentSpawned, event.SubagentSpawned{SubagentID: "sub_chat-a", Task: "A", Capability: "discovery"}, tabSID))
	m = step(m, evt(event.KindSubagentSpawned, event.SubagentSpawned{SubagentID: "sub_chat-b", Task: "B", Capability: "discovery"}, tabSID))
	m = step(m, evt(event.KindSubagentSpawned, event.SubagentSpawned{SubagentID: "sub_chat-c", Task: "C", Capability: "discovery"}, tabSID))
	m = step(m, evt(event.KindSubagentCompleted, event.SubagentCompleted{SubagentID: "sub_chat-a", Status: "ok"}, tabSID))
	m = step(m, evt(event.KindSubagentCompleted, event.SubagentCompleted{SubagentID: "sub_chat-b", Status: "ok"}, tabSID))
	m = step(m, evt(event.KindSubagentCompleted, event.SubagentCompleted{SubagentID: "sub_chat-c", Status: "ok"}, tabSID))

	// Focus tab C (index 2).
	m.subCur = tabIndexByN(m.subTabs, 3) // n=3 is tab C
	if m.subCur < 0 || m.subTabs[m.subCur].chatID != "sub_chat-c" {
		t.Fatalf("setup: subCur=%d, expected focus on chat-c", m.subCur)
	}

	// Auto-close tab A (index 0, before focused).
	m = step(m, subTabCloseMsg{ChatID: "sub_chat-a"})

	// Focus should still be on chat-c, now at index 1.
	if m.subCur < 0 || m.subTabs[m.subCur].chatID != "sub_chat-c" {
		t.Errorf("after close: subCur=%d, expected focus on chat-c", m.subCur)
	}
}

// TestSubagentDoneArmsExactlyOneCloseTimer verifies card #133: only the FIRST
// SubagentDoneMsg for a chatID arms the 30s auto-close timer. A duplicate/
// replayed Done for an already-done tab must NOT arm an additional timer (which
// would leak a timer and fire a redundant subTabCloseMsg). We assert the first
// Done returns a close-timer command and the duplicate returns nil. (We do NOT
// execute the tea.Tick command — it blocks ~30s waiting for the timer to fire.)
func TestSubagentDoneArmsExactlyOneCloseTimer(t *testing.T) {
	m := newTabModel()
	m = step(m, evt(event.KindSubagentSpawned, event.SubagentSpawned{SubagentID: "sub_chat-a", Task: "task A", Capability: "discovery"}, tabSID))

	// First Done → arms one auto-close timer (non-nil command).
	mu, cmd1 := m.Update(evt(event.KindSubagentCompleted, event.SubagentCompleted{SubagentID: "sub_chat-a", Status: "ok"}, tabSID))
	m = mu.(tuiModel)
	// Duplicate Done → must NOT arm another (nil command).
	_, cmd2 := m.Update(evt(event.KindSubagentCompleted, event.SubagentCompleted{SubagentID: "sub_chat-a", Status: "ok"}, tabSID))

	if cmd1 == nil {
		t.Error("first Done did not arm an auto-close timer")
	}
	if cmd2 != nil {
		t.Error("duplicate Done armed an additional auto-close timer (card #133 regression)")
	}
}
