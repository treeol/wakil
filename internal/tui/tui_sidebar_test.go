package tui

// Tests for the dynamic sidebar rendering driven by the subagent's
// capability tier and resolved model.

import (
	"strings"
	"testing"

	agent "github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/tools"
)

// countToolLines counts how many lines under the "tools" header match the
// "  <name>" pattern (two-space indent), stopping at the next non-indented
// or empty line.
func countToolLines(lines []string, toolsHeaderIdx int) int {
	count := 0
	for i := toolsHeaderIdx + 1; i < len(lines); i++ {
		l := lines[i]
		if l == "" || !strings.HasPrefix(l, "  ") {
			break
		}
		count++
	}
	return count
}

func findToolsHeader(lines []string) int {
	for i, l := range lines {
		if plain(l) == "tools" {
			return i
		}
	}
	return -1
}

// TestSubSidebarToolsDiscoveryTier verifies that a discovery-tier subagent
// shows exactly 5 read-only tools.
func TestSubSidebarToolsDiscoveryTier(t *testing.T) {
	m := newTabModel()
	tab := &subTab{
		chatID:     "chat-a",
		capability: tools.CapabilityDiscovery,
		buf:        new(strings.Builder),
	}
	lines := m.subSidebarLines(tab, 24)
	idx := findToolsHeader(lines)
	if idx < 0 {
		t.Fatal("tools header not found in sidebar")
	}
	count := countToolLines(lines, idx)
	if count != 5 {
		t.Errorf("discovery tier: %d tool lines, want 5", count)
	}
}

// TestSubSidebarToolsEditTier verifies that an edit-tier subagent shows all
// 9 tools (5 read + 4 edit).
func TestSubSidebarToolsEditTier(t *testing.T) {
	m := newTabModel()
	tab := &subTab{
		chatID:     "chat-a",
		capability: tools.CapabilityEdit,
		buf:        new(strings.Builder),
	}
	lines := m.subSidebarLines(tab, 24)
	idx := findToolsHeader(lines)
	if idx < 0 {
		t.Fatal("tools header not found in sidebar")
	}
	count := countToolLines(lines, idx)
	if count != 9 {
		t.Errorf("edit tier: %d tool lines, want 9", count)
	}
	// Verify the edit tools are present.
	joined := strings.Join(lines, "\n")
	for _, name := range []string{"write_file", "edit_file", "delete_file", "move_file"} {
		if !strings.Contains(joined, name) {
			t.Errorf("edit tool %q not found in sidebar", name)
		}
	}
}

// TestSubSidebarToolsEmptyCapability verifies that an empty capability
// (the default when none is specified) renders the 5 discovery tools.
func TestSubSidebarToolsEmptyCapability(t *testing.T) {
	m := newTabModel()
	tab := &subTab{
		chatID:     "chat-a",
		capability: "",
		buf:        new(strings.Builder),
	}
	lines := m.subSidebarLines(tab, 24)
	idx := findToolsHeader(lines)
	if idx < 0 {
		t.Fatal("tools header not found in sidebar")
	}
	count := countToolLines(lines, idx)
	if count != 5 {
		t.Errorf("empty capability: %d tool lines, want 5 (discovery default)", count)
	}
}

// TestSubSidebarModelDisplay verifies that the model from SubagentStartMsg
// is rendered in the sidebar.
func TestSubSidebarModelDisplay(t *testing.T) {
	m := newTabModel()
	tab := &subTab{
		chatID:     "chat-a",
		capability: tools.CapabilityDiscovery,
		model:      "child-model-x",
		buf:        new(strings.Builder),
	}
	lines := m.subSidebarLines(tab, 24)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "child-model-x") {
		t.Errorf("model \"child-model-x\" not found in sidebar output:\n%s", joined)
	}
}

// TestSubSidebarModelEmpty shows fallback ellipsis when model is empty.
func TestSubSidebarModelEmpty(t *testing.T) {
	tab := &subTab{
		chatID:     "chat-a",
		capability: tools.CapabilityDiscovery,
		model:      "",
		buf:        new(strings.Builder),
	}
	// subTabModel returns "…" for empty model.
	if got := subTabModel(tab); got != "…" {
		t.Errorf("subTabModel(empty) = %q, want …", got)
	}
}

// TestSubagentStartMsgPopulatesTab verifies that the SubagentStartMsg handler
// stores capability and model on the subTab.
func TestSubagentStartMsgPopulatesTab(t *testing.T) {
	m := newTabModel()
	m = step(m, agent.SubagentStartMsg{
		Task:       "task A",
		ChatID:     "chat-a",
		Capability: tools.CapabilityEdit,
		Model:      "child-model-x",
	})
	if len(m.subTabs) != 1 {
		t.Fatalf("expected 1 tab, got %d", len(m.subTabs))
	}
	tab := m.subTabs[0]
	if tab.capability != tools.CapabilityEdit {
		t.Errorf("capability = %q, want %q", tab.capability, tools.CapabilityEdit)
	}
	if tab.model != "child-model-x" {
		t.Errorf("model = %q, want child-model-x", tab.model)
	}
}
