package tui

// Tests for the subagent info content shown in the on-demand info panel
// (WP-9.1), driven by the subagent's capability tier and resolved model.
// These replace the old subSidebarLines tests after the right sidebar was
// removed in favor of the full-width info panel.

import (
	"strings"
	"testing"

	agent "github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/tools"
)

// toolRowText joins the rendered sub-tool rows and strips the "tools" key so
// tests can assert on the tool names present.
func toolRowText(lines []string) string {
	return strings.Join(lines, "\n")
}

// TestSubPanelToolsDiscoveryTier verifies that a discovery-tier subagent shows
// exactly the 5 read-only tools.
func TestSubPanelToolsDiscoveryTier(t *testing.T) {
	tab := &subTab{chatID: "chat-a", capability: tools.CapabilityDiscovery, buf: new(strings.Builder)}
	joined := toolRowText(subToolListLine(tab, 80))
	for _, name := range []string{"read_file", "read_file_full", "search_files", "find_files", "list_dir"} {
		if !strings.Contains(joined, name) {
			t.Errorf("discovery tool %q not found in panel tools row: %s", name, joined)
		}
	}
	// No edit tools for discovery.
	for _, name := range []string{"write_file", "edit_file", "delete_file", "move_file"} {
		if strings.Contains(joined, name) {
			t.Errorf("discovery tier should not show edit tool %q: %s", name, joined)
		}
	}
}

// TestSubPanelToolsEditTier verifies that an edit-tier subagent shows all 9
// tools (5 read + 4 edit).
func TestSubPanelToolsEditTier(t *testing.T) {
	tab := &subTab{chatID: "chat-a", capability: tools.CapabilityEdit, buf: new(strings.Builder)}
	joined := toolRowText(subToolListLine(tab, 80))
	for _, name := range []string{"read_file", "read_file_full", "search_files", "find_files", "list_dir",
		"write_file", "edit_file", "delete_file", "move_file"} {
		if !strings.Contains(joined, name) {
			t.Errorf("edit tool %q not found in panel tools row: %s", name, joined)
		}
	}
}

// TestSubPanelToolsEmptyCapability verifies that an empty capability renders the
// 5 discovery tools (the default).
func TestSubPanelToolsEmptyCapability(t *testing.T) {
	tab := &subTab{chatID: "chat-a", capability: "", buf: new(strings.Builder)}
	joined := toolRowText(subToolListLine(tab, 80))
	for _, name := range []string{"read_file", "read_file_full", "search_files", "find_files", "list_dir"} {
		if !strings.Contains(joined, name) {
			t.Errorf("empty capability: discovery tool %q not found: %s", name, joined)
		}
	}
	if strings.Contains(joined, "write_file") {
		t.Errorf("empty capability should default to discovery (no edit tools): %s", joined)
	}
}

// TestSubPanelToolsToolsTier verifies that a tools-tier subagent shows the tools
// from tab.toolNames (passed via SubagentStartMsg), not the hardcoded list.
func TestSubPanelToolsToolsTier(t *testing.T) {
	tab := &subTab{
		chatID:     "chat-a",
		capability: tools.CapabilityTools,
		toolNames: []string{
			"read_file", "search_files", "lsp_definition",
			"trello__get_cards", "context7__resolve-library-id",
		},
		buf: new(strings.Builder),
	}
	joined := toolRowText(subToolListLine(tab, 120))
	for _, name := range []string{"trello__get_cards", "context7__resolve-library-id", "lsp_definition"} {
		if !strings.Contains(joined, name) {
			t.Errorf("tools-tier tool %q not found in panel tools row: %s", name, joined)
		}
	}
	// Should NOT show the hardcoded edit tools.
	if strings.Contains(joined, "write_file") {
		t.Errorf("tools tier should not show hardcoded edit tools: %s", joined)
	}
}

// TestSubPanelToolsTierEmptyToolNames falls back to the discovery list when
// toolNames is nil (e.g. parent didn't populate it).
func TestSubPanelToolsTierEmptyToolNames(t *testing.T) {
	tab := &subTab{chatID: "chat-a", capability: tools.CapabilityTools, toolNames: nil, buf: new(strings.Builder)}
	joined := toolRowText(subToolListLine(tab, 80))
	for _, name := range []string{"read_file", "read_file_full", "search_files", "find_files", "list_dir"} {
		if !strings.Contains(joined, name) {
			t.Errorf("nil toolNames: discovery fallback tool %q not found: %s", name, joined)
		}
	}
}

// TestSubPanelModelDisplay verifies that the model from SubagentStartMsg is
// rendered in the subagent info panel.
func TestSubPanelModelDisplay(t *testing.T) {
	m := newTabModel()
	tab := &subTab{chatID: "chat-a", capability: tools.CapabilityDiscovery, model: "child-model-x", buf: new(strings.Builder)}
	joined := strings.Join(m.infoSubLines(tab, 120), "\n")
	if !strings.Contains(joined, "child-model-x") {
		t.Errorf("model \"child-model-x\" not found in subagent info panel:\n%s", joined)
	}
}

// TestSubPanelModelEmpty shows fallback ellipsis when model is empty.
func TestSubPanelModelEmpty(t *testing.T) {
	tab := &subTab{chatID: "chat-a", capability: tools.CapabilityDiscovery, model: "", buf: new(strings.Builder)}
	if got := subTabModel(tab); got != "…" {
		t.Errorf("subTabModel(empty) = %q, want …", got)
	}
}

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
