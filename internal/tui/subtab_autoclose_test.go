package tui

// Tests for subTabCloseMsg: auto-close of done subagent tabs after 30s.

import (
	"testing"

	agent "github.com/treeol/wakil/internal/agent"
)

// TestSubTabCloseMsgRemovesDoneUnfocusedTab verifies that a subTabCloseMsg
// removes a done tab when the user is not focused on it.
func TestSubTabCloseMsgRemovesDoneUnfocusedTab(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.SubagentStartMsg{Task: "task A", ChatID: "chat-a"})
	m = step(m, agent.SubagentStartMsg{Task: "task B", ChatID: "chat-b"})

	// Both tabs become done.
	m = step(m, agent.SubagentDoneMsg{ChatID: "chat-a"})
	m = step(m, agent.SubagentDoneMsg{ChatID: "chat-b"})

	// User is on main tab (subCur == -1), so neither is focused.
	if m.subCur != -1 {
		t.Fatalf("precondition: subCur = %d, want -1 (main)", m.subCur)
	}
	if len(m.subTabs) != 2 {
		t.Fatalf("precondition: %d tabs, want 2", len(m.subTabs))
	}

	// Auto-close tab A.
	m = step(m, subTabCloseMsg{ChatID: "chat-a"})

	if len(m.subTabs) != 1 {
		t.Fatalf("after close: %d tabs, want 1", len(m.subTabs))
	}
	if m.subTabs[0].chatID != "chat-b" {
		t.Errorf("remaining tab = %q, want chat-b", m.subTabs[0].chatID)
	}
}

// TestSubTabCloseMsgSkipsFocusedTab verifies that a subTabCloseMsg does NOT
// remove the tab the user is currently viewing.
func TestSubTabCloseMsgSkipsFocusedTab(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.SubagentStartMsg{Task: "task A", ChatID: "chat-a"})
	m = step(m, agent.SubagentDoneMsg{ChatID: "chat-a"})

	// Focus tab A.
	m.subCur = tabIndexByN(m.subTabs, 1)
	if m.subCur < 0 {
		t.Fatal("could not focus tab A")
	}

	// Auto-close fires — should skip because the tab is focused.
	m = step(m, subTabCloseMsg{ChatID: "chat-a"})

	if len(m.subTabs) != 1 {
		t.Errorf("focused tab was removed: %d tabs, want 1", len(m.subTabs))
	}
}

// TestSubTabCloseMsgSkipsNotDoneTab verifies that a subTabCloseMsg does NOT
// remove a tab that is not yet done (safety against a stale timer).
func TestSubTabCloseMsgSkipsNotDoneTab(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.SubagentStartMsg{Task: "task A", ChatID: "chat-a"})

	// Tab is running (not done). A stale close message arrives.
	m = step(m, subTabCloseMsg{ChatID: "chat-a"})

	if len(m.subTabs) != 1 {
		t.Errorf("running tab was removed: %d tabs, want 1", len(m.subTabs))
	}
}

// TestSubTabCloseMsgNoOpsOnMissingTab verifies that a subTabCloseMsg for a
// ChatID that no longer exists is a safe no-op.
func TestSubTabCloseMsgNoOpsOnMissingTab(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.SubagentStartMsg{Task: "task A", ChatID: "chat-a"})
	m = step(m, agent.SubagentDoneMsg{ChatID: "chat-a"})

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

	m = step(m, agent.SubagentStartMsg{Task: "task A", ChatID: "chat-a"})
	// First tab appearance triggers reflow — viewport shrinks by 1 row (tab bar).
	withTabVpH := m.vp.Height
	if withTabVpH >= noTabVpH {
		t.Errorf("after start: vp height %d, want < %d (tab bar took a row)", withTabVpH, noTabVpH)
	}

	m = step(m, agent.SubagentDoneMsg{ChatID: "chat-a"})
	// Auto-close the only tab.
	m = step(m, subTabCloseMsg{ChatID: "chat-a"})

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
	m = step(m, agent.SubagentStartMsg{Task: "A", ChatID: "chat-a"})
	m = step(m, agent.SubagentStartMsg{Task: "B", ChatID: "chat-b"})
	m = step(m, agent.SubagentStartMsg{Task: "C", ChatID: "chat-c"})
	m = step(m, agent.SubagentDoneMsg{ChatID: "chat-a"})
	m = step(m, agent.SubagentDoneMsg{ChatID: "chat-b"})
	m = step(m, agent.SubagentDoneMsg{ChatID: "chat-c"})

	// Focus tab C (index 2).
	m.subCur = tabIndexByN(m.subTabs, 3) // n=3 is tab C
	if m.subCur < 0 || m.subTabs[m.subCur].chatID != "chat-c" {
		t.Fatalf("setup: subCur=%d, expected focus on chat-c", m.subCur)
	}

	// Auto-close tab A (index 0, before focused).
	m = step(m, subTabCloseMsg{ChatID: "chat-a"})

	// Focus should still be on chat-c, now at index 1.
	if m.subCur < 0 || m.subTabs[m.subCur].chatID != "chat-c" {
		t.Errorf("after close: subCur=%d, expected focus on chat-c", m.subCur)
	}
}
