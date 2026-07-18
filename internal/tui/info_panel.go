package tui

import (
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/tools"
)

// infoPanelModel holds the on-demand info panel's visibility state (WP-9.1).
// It is a NAMED field on tuiModel (m.infoPanel), NOT embedded — its only field,
// active, is too generic to promote safely alongside the other embedded models
// (subTab.active, resumePicker.active, comp.active). The panel replaces the
// removed right sidebar: it surfaces the sidebar's content (proxy/model/exec/
// cwd/chat/wf/mashūra/tools/costs/grounding) in the lower area on demand.
//
// active is mirrored to App.InfoPanelOpen (and persisted to repo-state) by the
// toggle path; the TUI is the source of truth during a session.
type infoPanelModel struct {
	active bool
}

// infoPanelCapH is the fixed height (rows) the panel occupies when open. A
// deterministic height shared by sizes() and View() is what keeps the viewport
// from overflowing / the cursor drifting — the same discipline as
// completionHeight()/resumePickerHeight(). Overflow content is truncated.
const infoPanelCapH = 10

// infoPanelHeight returns the rows the panel occupies in the layout. It is the
// SINGLE source of truth used by both sizes() (to reserve height) and View()
// (to render), so the two can never disagree. Returns 0 when closed.
func (m tuiModel) infoPanelHeight() int {
	if !m.infoPanelVisible() {
		return 0
	}
	return infoPanelCapH
}

// infoPanelVisible reports whether the panel is actually shown, accounting for
// both the toggle state AND short-terminal suppression. sizes() and View() must
// BOTH use this (never m.infoPanel.active directly) so they stay in agreement:
// if sizes() reserves 0 rows because the terminal is too short, View() must not
// render the panel either (otherwise the layout overflows / cursor drifts).
func (m tuiModel) infoPanelVisible() bool {
	if !m.infoPanel.active {
		return false
	}
	// Suppress the panel when reserving it would push the conversation pane below
	// its minimum height. This mirrors the suppression in sizes(): pickers and
	// input win over the on-demand panel.
	inputOuterH := m.ta.Height() + borderH
	if m.statusLine() != "" {
		inputOuterH++
	}
	tabH := 0
	if len(m.subTabs) > 0 {
		tabH = 1
	}
	withPanel := m.height - inputOuterH - m.completionHeight() - m.resumePickerHeight() - infoPanelCapH - tabH
	return withPanel >= minTopOuterH
}

// toggleInfoPanel flips the info panel's visibility and returns the updated
// model with a reflow applied (so the conversation pane cedes/reclaims the
// panel rows). It mirrors the new state to App.InfoPanelOpen and persists it to
// repo-state, so the open/closed state is remembered per session (WP-9.1).
func (m tuiModel) toggleInfoPanel() tuiModel {
	m.infoPanel.active = !m.infoPanel.active
	if m.app != nil {
		m.app.SetInfoPanelOpen(m.infoPanel.active)
	}
	return m.reflow()
}

// renderInfoPanel renders the former-sidebar content laid out for full width,
// padded/truncated to exactly infoPanelCapH rows (matching infoPanelHeight).
// When a sub tab is active it shows that subagent's info (parity with the old
// subSidebarLines); otherwise the main session info.
func (m tuiModel) renderInfoPanel() []string {
	w := m.width - borderW // inner width available inside the input-border column
	if w < 1 {
		w = 1
	}
	var lines []string
	if m.subCur >= 0 && m.subCur < len(m.subTabs) {
		lines = m.infoSubLines(m.subTabs[m.subCur], w)
	} else {
		lines = m.infoMainLines(w)
	}
	// Fit to the fixed panel height: truncate overflow, pad short.
	if len(lines) > infoPanelCapH {
		lines = lines[:infoPanelCapH]
	}
	for len(lines) < infoPanelCapH {
		lines = append(lines, "")
	}
	// Hard-clamp every line to the available width (display width, ANSI-aware).
	for i, ln := range lines {
		if lipgloss.Width(ln) > w {
			lines[i] = ansi.Truncate(ln, w, "…")
		}
	}
	return lines
}

// kv is one label:value row in the panel's info section.
type kv struct{ k, v string }

// infoMainLines gathers the main-session info (was mainSidebarLines) and lays
// it out for full width: a primary kv row group, then tools, costs, grounding.
func (m tuiModel) infoMainLines(w int) []string {
	a := m.app
	mode := "docker"
	cwd := ""
	if a.Exec != nil {
		if strings.HasPrefix(a.Exec.Describe(), "direct") {
			mode = "direct"
		}
		cwd = a.Exec.Cwd()
	}
	imgVal := a.Cfg.Image
	if i := strings.LastIndex(imgVal, "/"); i >= 0 {
		imgVal = imgVal[i+1:]
	}

	baseURL, chatID := "", ""
	if a.Client != nil {
		baseURL = hostOnly(a.Client.BaseURL)
		chatID = agent.ShortID(a.Client.ChatID)
	}

	info := []kv{
		{"proxy", baseURL},
		{"model", a.EffectiveModel()},
		{"exec", mode},
	}
	if mode == "docker" {
		info = append(info, kv{"img", imgVal})
	}
	info = append(info, kv{"cwd", cwd}, kv{"chat", chatID})
	if a.Workflow != nil {
		info = append(info, kv{"wf", a.Workflow.SidebarLabel()})
	}

	var lines []string
	lines = append(lines, renderKVRow(info, w)...)

	// mashūra panel availability.
	if a.Cfg.OracleEnabled {
		anthropicOk := os.Getenv(a.Cfg.OracleAPIKeyEnv) != ""
		openrouterOk := os.Getenv("OPENROUTER_API_KEY") != ""
		label := mashuraPanelLabel(a.Cfg)
		if !anthropicOk && !openrouterOk {
			label = "no key"
		}
		lines = append(lines, renderKVRow([]kv{{"mashūra", label}}, w)...)
	}

	lines = append(lines, m.infoToolsLines(w)...)
	lines = append(lines, m.costLines(w)...)
	lines = append(lines, m.infoGroundingLines(w)...)
	return lines
}

// infoSubLines gathers the active subagent's info (was subSidebarLines) for the panel.
func (m tuiModel) infoSubLines(tab *subTab, w int) []string {
	a := m.app
	statusStr := "running…"
	if tab.done {
		statusStr = "done ✓"
	} else if tab.finished {
		ts := ""
		if !tab.finishedAt.IsZero() {
			ts = " " + tab.finishedAt.Format("15:04:05")
		}
		statusStr = "✓ finished" + ts
	}

	baseURL, cwd := "", ""
	if a.Client != nil {
		baseURL = hostOnly(a.Client.BaseURL)
	}
	if a.Exec != nil {
		cwd = a.Exec.Cwd()
	}

	info := []kv{
		{"status", statusStr},
		{"proxy", baseURL},
		{"model", subTabModel(tab)},
		{"exec", "docker"},
		{"cwd", cwd},
		{"chat", agent.ShortID(tab.chatID)},
	}
	subBackend := tab.usedBackend
	if subBackend == "" {
		subBackend = tab.backend
	}
	if subBackend != "" {
		isDefault := subBackend == a.Cfg.Backend
		isOverridden := tab.backend != "" && tab.usedBackend != "" && tab.backend != tab.usedBackend
		if !isDefault || isOverridden {
			label := subBackend
			if isOverridden {
				label += "!"
			}
			info = append(info, kv{"backend", label})
		}
	}
	if tab.done && tab.costUSD > 0 {
		info = append(info, kv{"cost", proxy.FmtUSDCompact(tab.costUSD)})
	} else if tab.finished && !tab.done && tab.finCostUSD > 0 {
		info = append(info, kv{"cost", proxy.FmtUSDCompact(tab.finCostUSD)})
	}
	if tab.done && len(tab.filesChanged) > 0 {
		info = append(info, kv{"files", sprint("%d changed", len(tab.filesChanged))})
	} else if tab.finished && !tab.done && tab.finFilesN > 0 {
		info = append(info, kv{"files", sprint("%d changed", tab.finFilesN)})
	}

	lines := renderKVRow(info, w)
	lines = append(lines, m.infoToolsLines(w)...)
	lines = append(lines, subToolListLine(tab, w)...)
	lines = append(lines, m.infoGroundingLines(w)...)
	return lines
}

// subToolListLine renders the subagent's tool list (by capability tier) as a
// flowed full-width row. Discovery/edit tiers use a hardcoded list (byte-identical
// across dispatches); the tools tier uses the dynamic list passed via
// SubagentStartMsg.ToolNames. Ported from the removed subSidebarLines (WP-9.1).
func subToolListLine(tab *subTab, w int) []string {
	var names []string
	if len(tab.toolNames) > 0 {
		names = append(names, tab.toolNames...)
	} else {
		names = []string{"read_file", "read_file_full", "search_files", "find_files", "list_dir"}
		if tab.capability == tools.CapabilityEdit {
			names = append(names, "write_file", "edit_file", "delete_file", "move_file")
		}
	}
	return renderKVRow([]kv{{"tools", strings.Join(names, " ")}}, w)
}

// infoToolsLines renders the MCP/searxng tool status as one wrapped row.
func (m tuiModel) infoToolsLines(w int) []string {
	a := m.app
	var parts []string
	if a.MCP != nil {
		for _, srv := range a.MCP.Servers() {
			icon := "✓"
			label := sprint("%d", len(srv.Tools))
			if srv.Status == "failed" {
				icon, label = "✗", "failed"
			} else if srv.Status == "connecting" {
				icon, label = "…", "…"
			}
			parts = append(parts, icon+" "+srv.Cfg.Name+" "+label)
		}
	}
	if a.Cfg.SearXngURL != "" {
		parts = append(parts, "✓ searxng "+sprint("%d", len(tools.SearxngTools())))
	}
	if len(parts) == 0 {
		return nil
	}
	return renderKVRow([]kv{{"tools", strings.Join(parts, "  ")}}, w)
}

// infoGroundingLines renders the grounding list (was the "grounded on" block).
func (m tuiModel) infoGroundingLines(w int) []string {
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	if m.app.Client == nil {
		return []string{dim.Render("grounded on —")}
	}
	grounding := m.app.Client.Grounding()
	if len(grounding) == 0 {
		return []string{dim.Render("grounded on —")}
	}
	var entries []string
	for _, e := range grounding {
		entries = append(entries, groundingTypeTag(e.Type)+" "+e.Label)
	}
	// Flow the grounding entries across the width on as few rows as needed.
	flow := strings.Join(entries, dim.Render("  ·  "))
	head := dim.Render("grounded on ")
	var lines []string
	for _, ln := range strings.Split(wrapAnsi(head+flow, w), "\n") {
		lines = append(lines, ln)
	}
	return lines
}

// renderKVRow lays out kv pairs left-to-right, wrapping to multiple rows when
// they exceed w. Keys are dim, values bright — same palette as the old sidebar.
func renderKVRow(pairs []kv, w int) []string {
	keyStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	valStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	sep := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("  ·  ")

	var lines []string
	var cur strings.Builder
	curW := 0
	flush := func() {
		if cur.Len() > 0 {
			lines = append(lines, cur.String())
			cur.Reset()
			curW = 0
		}
	}
	for _, p := range pairs {
		seg := keyStyle.Render(p.k) + " " + valStyle.Render(p.v)
		segW := lipgloss.Width(seg)
		addW := segW
		if curW > 0 {
			addW += lipgloss.Width(sep)
		}
		if curW > 0 && curW+addW > w {
			flush()
			addW = segW
		}
		if curW > 0 {
			cur.WriteString(sep)
		}
		cur.WriteString(seg)
		curW += addW
	}
	flush()
	return lines
}
