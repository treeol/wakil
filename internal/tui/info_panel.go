package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/tools"
)

// infoPanelModel holds the info EXPANSION's toggle state. There is no longer
// a separate panel region — the state only controls whether statusLines()
// appends the former-banner segments (proxy/exec/cwd/chat/mashūra/tools/
// costs/grounding/…) into the status zone above the input. It is a NAMED
// field on tuiModel (m.infoPanel), NOT embedded — its only field, active, is
// too generic to promote safely alongside the other embedded models
// (subTab.active, resumePicker.active, comp.active).
//
// active is mirrored to App.InfoPanelOpen (and persisted to repo-state) by the
// toggle path; the TUI is the source of truth during a session.
type infoPanelModel struct {
	active bool
}

// toggleInfoPanel flips the info expansion and reflows (the extra segments
// change statusRows(), so the conversation pane cedes/reclaims those rows).
// It mirrors the new state through the facade (wiring path) or the App
// control seam (legacy path), and persists it to repo-state so the
// expanded/collapsed state is remembered per session.
func (m tuiModel) toggleInfoPanel() tuiModel {
	m.infoPanel.active = !m.infoPanel.active
	m.facade.SetInfoPanelOpen(m.infoPanel.active)
	return m.reflow()
}

// infoExtraSegments renders the former banner / info-panel content as status
// segments, appended to the status flow only while the expansion is on. Each
// entry is one " · "-joinable segment (key dim, value bright — same palette
// as the old panel). Fields that duplicate the always-on status line (model,
// sub) are omitted; everything else is preserved here.
func (m tuiModel) infoExtraSegments() []string {
	if m.subCur >= 0 && m.subCur < len(m.subTabs) {
		return m.infoSubSegments(m.subTabs[m.subCur])
	}
	return m.infoMainSegments()
}

// kv is one label:value pair; kvg renders it as a dim-key/bright-value segment.
type kv struct{ k, v string }

func kvg(p kv) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(p.k) + " " +
		lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Render(p.v)
}

// infoMainSegments gathers the main-session expansion segments from the
// facade's InfoSnapshot (m4b — no App reads).
func (m tuiModel) infoMainSegments() []string {
	info := m.facade.Info()
	mode := "docker"
	if strings.HasPrefix(info.ExecMode, "direct") {
		mode = "direct"
	}
	imgVal := info.Image
	if i := strings.LastIndex(imgVal, "/"); i >= 0 {
		imgVal = imgVal[i+1:]
	}

	var segs []string
	segs = append(segs, kvg(kv{"proxy", hostOnly(info.BaseURL)}), kvg(kv{"exec", mode}))
	if mode == "docker" {
		segs = append(segs, kvg(kv{"img", imgVal}))
	}
	// The prompt source used to be a startup banner line; only shown when a
	// file path was configured — the built-in fallback note is noise.
	if note := info.PromptNote; !strings.Contains(note, "built-in fallback") {
		if i := strings.Index(note, ": "); i >= 0 {
			segs = append(segs, kvg(kv{"prompt", note[i+2:]}))
		}
	}
	segs = append(segs, kvg(kv{"cwd", info.Cwd}), kvg(kv{"chat", formatShortID(info.ChatID)}))
	if info.WorkflowLabel != "" {
		segs = append(segs, kvg(kv{"wf", info.WorkflowLabel}))
	}
	if info.OracleOn {
		segs = append(segs, kvg(kv{"mashūra", info.OracleLabel}))
	}
	segs = append(segs, m.infoToolsSegments()...)
	segs = append(segs, m.costSegments()...)
	segs = append(segs, m.infoGroundingSegments()...)
	return segs
}

// infoSubSegments gathers the active subagent's expansion segments. The
// subagent tab's own fields (status/model/cost/files/backend) are TUI-local
// (populated from events); proxy/cwd come from Info().
func (m tuiModel) infoSubSegments(tab *subTab) []string {
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

	info := m.facade.Info()
	segs := []string{
		kvg(kv{"status", statusStr}),
		kvg(kv{"proxy", hostOnly(info.BaseURL)}),
		kvg(kv{"model", subTabModel(tab)}),
		kvg(kv{"exec", "docker"}),
		kvg(kv{"cwd", info.Cwd}),
		kvg(kv{"chat", formatShortID(tab.chatID)}),
	}
	subBackend := tab.usedBackend
	if subBackend == "" {
		subBackend = tab.backend
	}
	if subBackend != "" {
		isDefault := subBackend == info.ConfigBackend
		isOverridden := tab.backend != "" && tab.usedBackend != "" && tab.backend != tab.usedBackend
		if !isDefault || isOverridden {
			label := subBackend
			if isOverridden {
				label += "!"
			}
			segs = append(segs, kvg(kv{"backend", label}))
		}
	}
	if tab.done && tab.costUSD > 0 {
		segs = append(segs, kvg(kv{"cost", proxy.FmtUSDCompact(tab.costUSD)}))
	} else if tab.finished && !tab.done && tab.finCostUSD > 0 {
		segs = append(segs, kvg(kv{"cost", proxy.FmtUSDCompact(tab.finCostUSD)}))
	}
	if tab.done && len(tab.filesChanged) > 0 {
		segs = append(segs, kvg(kv{"files", sprint("%d changed", len(tab.filesChanged))}))
	} else if tab.finished && !tab.done && tab.finFilesN > 0 {
		segs = append(segs, kvg(kv{"files", sprint("%d changed", tab.finFilesN)}))
	}
	segs = append(segs, m.infoToolsSegments()...)
	segs = append(segs, subToolListSegment(tab))
	segs = append(segs, m.infoGroundingSegments()...)
	return segs
}

// subToolListSegment renders the subagent's tool list (by capability tier) as
// one segment. Discovery/edit tiers use a hardcoded list (byte-identical
// across dispatches); the tools tier uses the dynamic list passed via
// SubagentStartMsg.ToolNames.
func subToolListSegment(tab *subTab) string {
	var names []string
	if len(tab.toolNames) > 0 {
		names = append(names, tab.toolNames...)
	} else {
		names = []string{"read_file", "read_file_full", "search_files", "find_files", "list_dir"}
		if tab.capability == tools.CapabilityEdit {
			names = append(names, "write_file", "edit_file", "delete_file", "move_file")
		}
	}
	return kvg(kv{"tools", strings.Join(names, " ")})
}

// infoToolsSegments renders the MCP/searxng tool status as one segment,
// from InfoSnapshot (MCPServers + SearxngTools).
func (m tuiModel) infoToolsSegments() []string {
	info := m.facade.Info()
	var parts []string
	for _, srv := range info.MCPServers {
		icon := "✓"
		label := sprint("%d", srv.ToolN)
		if srv.Status == "failed" {
			icon, label = "✗", "failed"
		} else if srv.Status == "connecting" {
			icon, label = "…", "…"
		}
		parts = append(parts, icon+" "+srv.Name+" "+label)
	}
	if info.SearXngURL != "" {
		parts = append(parts, "✓ searxng "+sprint("%d", len(info.SearxngTools)))
	}
	if len(parts) == 0 {
		return nil
	}
	return []string{kvg(kv{"tools", strings.Join(parts, "  ")})}
}

// infoGroundingSegments renders the grounding list as one segment (was the
// "grounded on" block), from InfoSnapshot.Grounding.
func (m tuiModel) infoGroundingSegments() []string {
	dim := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	grounding := m.facade.Info().Grounding
	if len(grounding) == 0 {
		return []string{dim.Render("grounded on —")}
	}
	var entries []string
	for _, e := range grounding {
		entries = append(entries, groundingTypeTag(e.Type)+" "+e.Label)
	}
	return []string{dim.Render("grounded on ") + strings.Join(entries, dim.Render("  ·  "))}
}

// costSegments renders the costs block as segments: a "costs" label, the
// billed/est subtotals, and one segment per source. Returns nil when no costs
// have been recorded. The tracker pointer comes from Info()/Snapshot() (the
// same synchronized object the App held).
func (m tuiModel) costSegments() []string {
	costs := m.facade.Info().Costs
	if costs == nil {
		return nil
	}
	billedTotal, estimatedTotal, anyBilled, anyEstimated, rows := costs.SnapshotSplit()
	if len(rows) == 0 {
		return nil
	}
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	segs := []string{dimStyle.Render("costs")}
	if anyBilled {
		segs = append(segs, billedCell(billedTotal))
	}
	if anyEstimated {
		cell, color := "—", "240"
		if estimatedTotal > 0 {
			cell = proxy.FmtUSDCompact(estimatedTotal)
			color = "214"
		}
		segs = append(segs, dimStyle.Render("est")+" "+lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render(cell))
	}
	for _, r := range rows {
		glyph, color := proxy.CostGlyphStyle(r.Confidence)
		g := lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render(glyph)
		displayName := shortSourceName(r.Source)
		name := lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Render(displayName)
		costColor := "252"
		if !r.Priced {
			costColor = "240"
		}
		costSeg := lipgloss.NewStyle().Foreground(lipgloss.Color(costColor)).Render(proxy.CostCell(r))
		segs = append(segs, g+" "+name+" "+costSeg+dimStyle.Render(sprint("·%d", r.Calls)))
	}
	return segs
}

// billedCell renders the "billed" subtotal segment: a dim key and a value that
// is green when a real billed total exists, dim "—" when billed sources are
// present but contribute no priced total.
func billedCell(billedTotal float64) string {
	cell, color := "—", "240"
	if billedTotal > 0 {
		cell = proxy.FmtUSDCompact(billedTotal)
		color = "2"
	}
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	return dimStyle.Render("billed") + " " + lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render(cell)
}

// billedSegment returns the always-on "billed $X" status segment — the real
// billed spend — or "" when no billed source has been recorded. It is rendered
// in the COLLAPSED status line (the fixed group that never scrolls away), so
// the actual billed cost is visible without opening the info expansion. When
// the expansion is on, the full cost block (costSegments) carries it instead,
// so the segment is not duplicated.
func (m tuiModel) billedSegment() string {
	costs := m.facade.Info().Costs
	if costs == nil {
		return ""
	}
	billedTotal, _, anyBilled, _, _ := costs.SnapshotSplit()
	if !anyBilled {
		return ""
	}
	return billedCell(billedTotal)
}

// shortSourceName derives a compact ≤9 visual-char display label for a source
// key. Per-backend inference keys ("inference·<backend>") and per-model mashura
// keys ("mashura·<model>") are abbreviated so they fit the narrow column.
func shortSourceName(s string) string {
	if strings.HasPrefix(s, proxy.CostSourceInfPrefix) {
		tail := s[len(proxy.CostSourceInfPrefix):]
		if i := strings.IndexByte(tail, '/'); i >= 0 {
			tail = tail[:i]
		}
		return ansiTruncate("inf·"+tail, 9)
	}
	if strings.HasPrefix(s, proxy.CostSourceMashuraPrefix) {
		model := s[len(proxy.CostSourceMashuraPrefix):]
		if i := strings.IndexByte(model, ':'); i >= 0 {
			model = model[i+1:]
		}
		if i := strings.LastIndexByte(model, '/'); i >= 0 {
			model = model[i+1:]
		}
		model = strings.TrimPrefix(model, "claude-")
		return ansiTruncate("m·"+model, 9)
	}
	return ansiTruncate(s, 9)
}

// ansiTruncate trims s to n display columns with an ellipsis tail.
func ansiTruncate(s string, n int) string {
	if lipgloss.Width(s) <= n {
		return s
	}
	return truncateForDisplay(s, n)
}

// groundingTypeTag returns the short bracketed tag shown before a grounding
// entry label. Unknown types fall back to the raw type string.
func groundingTypeTag(t string) string {
	switch t {
	case "corpus":
		return "[ilm]"
	case "zdb":
		return "[zdb]"
	case "learned":
		return "[lrn]"
	case "memory":
		return "[mem]"
	case "web":
		return "[web]"
	case "oracle":
		return "[orc]"
	default:
		return "[" + t + "]"
	}
}

func hostOnly(url string) string {
	s := strings.TrimPrefix(url, "http://")
	s = strings.TrimPrefix(s, "https://")
	return s
}


