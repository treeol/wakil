package tui

import (
	"fmt"
	"strings"

	agent "github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"

	"github.com/charmbracelet/glamour"
	glamourstyles "github.com/charmbracelet/glamour/styles"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

// mdCache memoizes a glamour renderer per width. The TUI runs single-threaded
// (Bubble Tea's Update loop), so no locking is needed. The renderer is rebuilt
// only when the viewport width changes (resize).
var mdCache struct {
	w int
	r *glamour.TermRenderer
}

func markdownRenderer(w int) *glamour.TermRenderer {
	if mdCache.r != nil && mdCache.w == w {
		return mdCache.r
	}
	// Start from the dark theme but drop the document margin and the leading/
	// trailing blank lines so formatted text sits flush in the conversation pane.
	style := glamourstyles.DarkStyleConfig
	zero := uint(0)
	style.Document.Margin = &zero
	style.Document.BlockPrefix = ""
	style.Document.BlockSuffix = ""
	r, err := glamour.NewTermRenderer(
		glamour.WithStyles(style),
		glamour.WithWordWrap(w),
	)
	if err != nil {
		return nil
	}
	mdCache.w = w
	mdCache.r = r
	return r
}

// renderMarkdown formats s as markdown wrapped to w columns. On any failure it
// falls back to the plain ANSI-aware wrapper so output is never lost.
func renderMarkdown(s string, w int) string {
	r := markdownRenderer(w)
	if r == nil {
		return wrapAnsi(s, w)
	}
	out, err := r.Render(s)
	if err != nil {
		return wrapAnsi(s, w)
	}
	return strings.Trim(out, "\n")
}

// --- styles ---

var (
	// Borders are hidden (blank, not drawn) rather than removed: a hidden border
	// occupies the same 2 cols / 2 rows as a drawn one, so every layout
	// calculation that assumes a 1-cell frame stays correct — the panes keep
	// their positions and spacing, just without the visible lines.
	styleConvBorder = lipgloss.NewStyle().
			Border(lipgloss.HiddenBorder())

	styleInputBorder = lipgloss.NewStyle().
				Border(lipgloss.HiddenBorder())

	styleState = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
)

func styleOK(s string) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render(s)
}
func styleErr(s string) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Render(s)
}
func dim2(s string) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(s)
}
func sprint(f string, a ...interface{}) string { return fmt.Sprintf(f, a...) }

// brailleMeter renders a horizontal fill gauge out of braille cells. The bar is
// split into two equal zones (half cells each) separated by a thin divider, to
// reflect that the context window runs as two 256k halves of one budget. Filled
// cells take `color`; the unfilled track is dim.
func brailleMeter(used, total int, color lipgloss.Color, half int) string {
	cells := half * 2
	if total <= 0 {
		total = 1
	}
	frac := float64(used) / float64(total)
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := frac * float64(cells)

	// Bottom-up vertical fill: empty → quarter → three-quarter → full.
	levels := []rune{'⣀', '⣤', '⣶', '⣿'}
	fill := lipgloss.NewStyle().Foreground(color)
	track := lipgloss.NewStyle().Foreground(lipgloss.Color("237"))
	div := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("┊")

	var b strings.Builder
	for i := 0; i < cells; i++ {
		cf := filled - float64(i) // this cell's fill amount, in [0,1] when partial
		switch {
		case cf >= 1:
			b.WriteString(fill.Render(string(levels[3])))
		case cf <= 0:
			b.WriteString(track.Render(string(levels[0])))
		default:
			lvl := int(cf*3) + 1 // any nonzero fill shows at least one step
			if lvl > 3 {
				lvl = 3
			}
			b.WriteString(fill.Render(string(levels[lvl])))
		}
		if i == half-1 {
			b.WriteString(div)
		}
	}
	return b.String()
}

// View renders the full-screen TUI. Dimensions come from sizes() so this
// function and reflow() always agree on layout math.
func (m tuiModel) View() string {
	if !m.ready {
		return "initializing…\n"
	}

	vpW, vpH, inputOuterH := m.sizes()
	_ = inputOuterH // used implicitly via JoinVertical

	// --- conversation pane ---
	// When a sub tab is active render its output instead of the main viewport.
	var convContent string
	if m.subCur >= 0 && m.subCur < len(m.subTabs) {
		convContent = m.renderSubTabContent(m.subTabs[m.subCur], vpW, vpH)
	} else {
		convContent = bottomAlignViewport(m.vp.View(), vpH)
	}
	conv := styleConvBorder.
		Width(vpW).
		Height(vpH).
		Render(convContent)

	// The right sidebar was removed (WP-9.1): the conversation pane is the full
	// top row. Its former content now lives in the on-demand info panel below.
	top := conv

	// Input spans the full terminal width.
	// styleInputBorder border = borderW cols; inner = m.width-borderW.
	// Textarea is the full inner width — the hist/ctx gauge and the status
	// line moved to the status zone directly above this box, so the input box
	// is just the textarea + border.
	input := styleInputBorder.
		Width(m.width - borderW).
		Render(m.ta.View())

	// The "@"/"/" completion picker or the resume picker sits between the
	// conversation and the input — the two are mutually exclusive (opening
	// the resume picker closes the completion picker; see openResumePicker).
	// The info panel (when open) sits above the picker. The status line
	// (1–2 rows, statusRows() is the single source of truth) sits directly
	// above the input; the tab bar (when sub tabs exist) is below it.
	var sections []string
	sections = append(sections, top)
	if m.infoPanelVisible() {
		sections = append(sections, lipgloss.NewStyle().Width(m.width-borderW).Render(strings.Join(m.renderInfoPanel(), "\n")))
	}
	if m.resumePicker.active {
		sections = append(sections, m.renderResumePicker())
	} else if m.comp.active {
		sections = append(sections, m.renderCompletion())
	}
	sections = append(sections, lipgloss.NewStyle().Width(m.width-borderW).Render(strings.Join(m.statusLines(), "\n")))
	sections = append(sections, input)
	if len(m.subTabs) > 0 {
		sections = append(sections, m.renderMainTabBar())
	}
	return lipgloss.JoinVertical(lipgloss.Left, sections...)
}

// renderSubTabContent renders the subagent's tool-call output and final
// summary into the conversation-pane area.
func (m tuiModel) renderSubTabContent(tab *subTab, vpW, vpH int) string {
	// Rebuild wrapped lines only when the buffer grew or the width changed.
	bufLen := tab.buf.Len()
	if tab.cachedLines == nil || tab.cacheBufLen != bufLen || tab.cacheVpW != vpW {
		var lines []string
		for _, ln := range strings.Split(tab.buf.String(), "\n") {
			if strings.TrimSpace(ansi.Strip(ln)) != "" {
				lines = append(lines, wrapAnsi(ln, vpW))
			}
		}
		tab.cachedLines = lines
		tab.cacheBufLen = bufLen
		tab.cacheVpW = vpW
	}
	lines := tab.cachedLines
	// Pad from top (bottom-anchored like the main viewport).
	pad := vpH - len(lines)
	if pad > 0 {
		prefix := make([]string, pad)
		lines = append(prefix, lines...)
	}
	if len(lines) > vpH {
		lines = lines[len(lines)-vpH:]
	}
	return strings.Join(lines, "\n")
}

// bottomAlignViewport takes the viewport's top-aligned View() output (which
// pads short content with blank lines at the bottom) and moves that padding to
// the top, so the last content line sits flush against the input box below.
// When content fills or exceeds vpH, the output is unchanged.
func bottomAlignViewport(view string, vpH int) string {
	lines := strings.Split(view, "\n")
	// Count trailing blank lines.
	trailing := 0
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "" {
			trailing++
		} else {
			break
		}
	}
	if trailing <= 0 {
		return view // content fills the viewport — nothing to move
	}
	// Move trailing blanks to the top.
	content := lines[:len(lines)-trailing]
	blank := make([]string, trailing)
	return strings.Join(append(blank, content...), "\n")
}

// statusRows computes how many rows the status line occupies. It uses the
// same flowSegments packing as the renderer, over the same segment list, so
// sizes()/infoPanelVisible() reserve exactly what View() renders.
func (m tuiModel) statusRows() int {
	return len(m.statusLines())
}

// statusLines renders the status zone above the input: identity + gauge on
// one line when it fits, wrapped onto two when it doesn't. Reverse search
// owns the row exclusively (one line) while active.
func (m tuiModel) statusLines() []string {
	w := m.width - borderW
	if w < 1 {
		w = 1
	}
	if sp := m.searchPrompt(); sp != "" {
		l := renderStatusDot(m.state, m.dotPhase) + " " + sp
		if lipgloss.Width(l) > w {
			l = ansi.Truncate(l, w, "")
		}
		return []string{l}
	}
	in := m.headerStatusInput()
	segments := statusSegments(in)
	if m.app != nil {
		segments = append(segments, m.ctxSegment())
	}
	return flowSegments(segments, w)
}

// statusSegments builds the ordered identity segment list: dot glued to
// AUTO/state first (fixed slots 1+2), then least → most volatile:
// model, sub, plan, raw, backend, t/s, flash.
func statusSegments(in statusLineInput) []string {
	head := []string{renderStatusDot(in.state, in.dotPhase)}
	if in.autoApprove {
		label := "AUTO"
		if in.allowDestructive {
			label = "AUTO!"
		}
		head = append(head, lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true).Render(label))
	}
	var stateSeg string
	switch in.state {
	case stateStreaming:
		if in.reasoning {
			stateSeg = styleState.Render("reasoning")
		} else {
			stateSeg = styleState.Render("streaming")
		}
	case stateConfirm:
		stateSeg = styleState.Render("confirming")
	case stateCompacting:
		stateSeg = styleState.Render("compacting")
	case stateIdle:
		if in.hadTurn {
			stateSeg = dim2("awaiting input")
		}
	}
	if stateSeg != "" {
		head = append(head, stateSeg)
	}
	segs := []string{strings.Join(head, " ")}
	if in.model != "" {
		segs = append(segs, dim2(in.model))
	}
	if in.submodel != "" && in.submodel != in.model {
		segs = append(segs, dim2("sub:"+in.submodel))
	}
	if in.workflowLabel != "" {
		segs = append(segs, lipgloss.NewStyle().Foreground(lipgloss.Color("33")).Render("plan "+in.workflowLabel))
	}
	if in.rawTools {
		segs = append(segs, lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("raw"))
	}
	if in.backendUsed != "" {
		isDefault := in.backendUsed == in.backendDefault
		isOverridden := in.backendRequested != "" && in.backendUsed != in.backendRequested
		if !isDefault || isOverridden {
			label := in.backendUsed
			if isOverridden {
				label += "!"
			}
			segs = append(segs, lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Render(label))
		}
	}
	if in.tps > 0 && in.state == stateStreaming {
		segs = append(segs, dim2(sprint("%.0f t/s", in.tps)))
	}
	if in.flash != "" {
		segs = append(segs, lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render(in.flash))
	}
	return segs
}

// ctxSegment renders the ctx/hist gauge as ONE status segment (it joins the
// same flow as the identity segments):
//
//	ctx ⣿⣀⣀⣀⣀⣀┊⣀⣀⣀⣀⣀⣀ 48k / 1.0M 24% · hist 13 22k
//
// Colors match the retired bottom-right block: green → amber (past the
// usable budget) → red (≥90% of n_ctx); the "ctx" key is amber when the
// ceiling came from the config fallback or the model was unresolved.
func (m tuiModel) ctxSegment() string {
	lim := m.app.ContextLimit()
	used := m.app.ContextTokensUsed()
	total := lim.NCtx
	pct := 0
	if total > 0 {
		pct = used * 100 / total
	}
	color := lipgloss.Color("2")
	if used >= total*90/100 {
		color = lipgloss.Color("1")
	} else if used >= lim.Usable() {
		color = lipgloss.Color("214")
	}
	ctxKey := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("ctx")
	if !lim.FromBackend() || lim.ModelUnresolved {
		ctxKey = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("ctx")
	}
	totalStr := fmt.Sprintf("%dk", total/1000)
	if total >= 1000000 {
		totalStr = fmt.Sprintf("%.1fM", float64(total)/1e6)
	}
	usedStr := fmt.Sprintf("%dk", used/1000)
	if used >= 1000000 {
		usedStr = fmt.Sprintf("%.1fM", float64(used)/1e6)
	}
	return ctxKey + " " + brailleMeter(used, total, color, 6) + " " +
		dim2(sprint("%s / %s", usedStr, totalStr)) + " " +
		lipgloss.NewStyle().Foreground(color).Render(sprint("%d%%", pct)) +
		dim2(sprint(" · hist %d %dk", len(m.app.Conv), agent.TranscriptSize(m.app.Conv)/1000))
}

// flowSegments packs segments left-to-right with " · " separators onto as
// few rows as needed (max 2 — the status zone never grows beyond that; on
// pathologically narrow terminals the rightmost segments are dropped with a
// trailing ellipsis, still reachable via F2). A segment never splits across
// rows: when it doesn't fit the current row it moves wholesale to the next.
// Returns at least one row.
func flowSegments(segs []string, w int) []string {
	const sep = " · "
	sepW := lipgloss.Width(sep)
	const maxRows = 2

	var rows []string
	cur := ""
	curW := 0
	dropped := false
	flush := func() {
		rows = append(rows, cur)
		cur, curW = "", 0
	}
	for _, seg := range segs {
		segW := lipgloss.Width(seg)
		addW := segW
		if curW > 0 {
			addW += sepW
		}
		if curW > 0 && curW+addW > w {
			if len(rows) == maxRows-1 {
				// Last row is full: drop this segment AND everything after it
				// (strict rightmost suffix — never omit a middle segment and
				// keep a later one, which would reorder semantic importance).
				dropped = true
				break
			}
			flush()
			addW = segW
		}
		if curW == 0 && segW > w {
			// Single segment wider than the row: truncate it in place.
			seg = ansi.Truncate(seg, w, "…")
			segW = lipgloss.Width(seg)
			addW = segW
			if curW > 0 {
				addW += sepW
			}
		}
		if curW > 0 {
			cur += sep
		}
		cur += seg
		curW += addW
	}
	flush()
	if dropped {
		last := rows[len(rows)-1]
		if lipgloss.Width(last)+1 > w {
			last = ansi.Truncate(last, w-1, "")
		}
		rows[len(rows)-1] = last + dim2("…")
	}
	return rows
}

// headerStatusInput assembles the statusLineInput for the status line from
// model state.
func (m tuiModel) headerStatusInput() statusLineInput {
	var workflowLabel string
	if m.app != nil && m.app.Workflow != nil {
		workflowLabel = m.app.Workflow.SidebarLabel()
	}
	backendUsed, backendRequested, backendDefault := "", "", ""
	model, submodel := "", ""
	if m.app != nil {
		if m.app.Client != nil {
			backendUsed = m.app.Client.LastUsedBackend()
		}
		backendRequested = m.app.SelectedBackend
		backendDefault = m.app.Cfg.Backend
		model = m.app.EffectiveModel()
		submodel = m.app.EffectiveSubagentModel()
	}
	return statusLineInput{
		state:            m.state,
		autoApprove:      m.app != nil && m.app.AutoApprove,
		allowDestructive: m.app != nil && m.app.AllowDestructive,
		rawTools:         m.app != nil && m.app.RawTools,
		reasoning:        m.reasoning != nil && m.reasoning.Len() > 0 && !m.reasoningDone,
		tps:              m.tps,
		workflowLabel:    workflowLabel,
		flash:            m.flash,
		dotPhase:         m.dotPhase,
		hadTurn:          m.hadTurn,
		backendUsed:      backendUsed,
		backendRequested: backendRequested,
		backendDefault:   backendDefault,
		model:            model,
		submodel:         submodel,
	}
}

// statusLineInput carries all the state needed by statusSegments. It is a
// plain struct so the builder is a pure function and can be unit-tested.
// (Reverse search is handled before this is built — statusLines() checks
// m.searchPrompt() first — so there is no search field here.)
type statusLineInput struct {
	state            agentState
	autoApprove      bool
	allowDestructive bool // /auto destructive grant active (renders AUTO!)
	rawTools         bool
	reasoning        bool    // extended-thinking in progress
	tps              float64 // decode speed; 0 = not measured yet
	workflowLabel    string  // e.g. "implement 3/6" or "" when no workflow
	flash            string  // transient copied/error message
	dotPhase         int     // 0-3, cycles while busy for the pulsing dot
	hadTurn          bool    // at least one completed turn in this session
	// Backend display (P29). backendUsed is the X-Ilm-Backend-Used response
	// header from the most recent turn; backendRequested is what was requested
	// (App.SelectedBackend); backendDefault is the configured default (Cfg.Backend).
	backendUsed      string
	backendRequested string
	backendDefault   string
	// model and submodel are the effective main and subagent models (WP-9.1),
	// shown so the most-glanceable former-sidebar fields survive without opening
	// the info panel. Empty = omitted.
	model    string
	submodel string
}

// dotPulseShades are the four color levels cycled by the pulsing activity dot.
var dotPulseShades = []lipgloss.Color{"235", "241", "247", "252"}

// renderStatusDot renders the always-present activity indicator •.
// Idle: dim static.  Confirm: solid bright (paused-busy).
// Streaming/compacting: pulses through dotPulseShades.
func renderStatusDot(state agentState, phase int) string {
	const dot = "•"
	switch state {
	case stateConfirm:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true).Render(dot)
	case stateStreaming, stateCompacting:
		shade := dotPulseShades[phase%len(dotPulseShades)]
		return lipgloss.NewStyle().Foreground(shade).Render(dot)
	default: // idle
		return lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(dot)
	}
}

// searchPrompt builds the reverse-search prompt string for the status line.
// Returns "" when search is not active. On match:
//
//	(reverse-i-search)`<query>': <truncated match>
//
// On no match:
//
//	(failed reverse-i-search)`<query>': <last match or empty>
//
// The match preview is truncated so the total width fits within the terminal.
func (m tuiModel) searchPrompt() string {
	if !m.searchActive {
		return ""
	}
	tag := "reverse-i-search"
	if m.searchFailed {
		tag = "failed reverse-i-search"
	}
	prefix := "(" + tag + ")`" + m.searchQuery + "': "
	// Reserve room for the prefix + dot + glued space (2 chars: "• ").
	// Use lipgloss.Width (display columns) not byte length, so multibyte
	// queries (CJK, accented chars) reserve the correct width.
	maxPreview := m.width - lipgloss.Width(prefix) - 2
	if maxPreview < 0 {
		maxPreview = 0
	}
	preview := ""
	if m.searchIdx >= 0 && m.searchIdx < len(m.inputHistory) {
		preview = m.inputHistory[m.searchIdx]
	}
	preview = truncateForDisplay(preview, maxPreview)
	return prefix + preview
}

// truncateForDisplay trims s to maxRunes display columns (using lipgloss.Width
// for CJK/emoji correctness), appending "…" if truncated.
// Returns s unchanged if maxRunes <= 0 and s fits.
func truncateForDisplay(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= maxRunes {
		return s
	}
	// Walk runes accumulating display width until we reach the budget.
	visual := 0
	for i, r := range s {
		w := lipgloss.Width(string(r))
		if visual+w > maxRunes-1 { // -1 for the ellipsis
			return s[:i] + "…"
		}
		visual += w
	}
	return s
}

// Tab bar visual widths (no ANSI — used for both rendering and hit testing).
const (
	tabMainW = 8  // " main   " visual chars
	tabSubW  = 18 // " ● sN task……… ×" visual chars
	tabGap   = 1  // space between adjacent tabs
	tabMoreW = 4  // "‹N " indicator shown when older tabs are scrolled off the left
)

// tabCapacity is how many full sub-tab slots fit beside the main tab.
func (m tuiModel) tabCapacity() int {
	avail := m.width - (tabMainW + tabGap)
	slot := tabSubW + tabGap
	if avail < slot {
		return 0
	}
	return avail / slot
}

// visibleSubTabs windows the sub tabs to those that fit the terminal width,
// always keeping the newest (rightmost) visible. It returns the index of the
// first visible tab and the count shown. Render and hit-testing both call this
// so they always agree on which screen slot maps to which tab.
func (m tuiModel) visibleSubTabs() (start, count int) {
	n := len(m.subTabs)
	capacity := m.tabCapacity()
	if capacity <= 0 {
		if n == 0 {
			return 0, 0
		}
		return n - 1, 1 // no room for a full slot — show the newest, clipped by Width()
	}
	if n <= capacity {
		return 0, n
	}
	// Reserve room for the leading "‹N" hidden-count indicator.
	c := (m.width - (tabMainW + tabGap) - tabMoreW) / (tabSubW + tabGap)
	if c < 1 {
		c = 1
	}
	if c > n {
		c = n
	}
	return n - c, c
}

// subTabSlotStart returns the visual x-start of the slot-th visible sub tab
// (0-based among the visible window), measured from the left edge.
func (m tuiModel) subTabSlotStart(slot int) int {
	base := tabMainW + tabGap
	if start, _ := m.visibleSubTabs(); start > 0 {
		base += tabMoreW
	}
	return base + slot*(tabSubW+tabGap)
}

// subTabPulseShades are the yellow/orange levels cycled by an actively running
// subagent's tab dot. Queued (dispatched but waiting for a parallelism slot)
// tabs show a static dim dot instead — see renderSubTabDot.
var subTabPulseShades = []lipgloss.Color{"94", "172", "214", "220"}

// subTabDotSpec returns the glyph and color for a subagent tab's status dot:
//
//	done     → green ✓ (authoritative: SubagentDoneMsg received)
//	finished → dim green ✓ (display-only early done: SubagentFinishedMsg
//	           received, SubagentDoneMsg not yet — child landed while siblings
//	           still run; visually distinct from both running and all-done)
//	active   → yellow pulsing ● (worker holds a slot, request in flight)
//	queued   → gray static ● (dispatched, waiting for a slot under the cap)
//
// Split from rendering so the state→color mapping is testable in non-TTY
// environments where lipgloss strips escape codes.
func subTabDotSpec(tab *subTab, phase int) (glyph string, color lipgloss.Color) {
	switch {
	case tab.done:
		return "✓", "2"
	case tab.finished:
		return "✓", "242" // dim green: landed but not yet authoritatively done
	case tab.active:
		return "●", subTabPulseShades[phase%len(subTabPulseShades)]
	default:
		return "●", "240"
	}
}

func renderSubTabDot(tab *subTab, phase int) string {
	glyph, color := subTabDotSpec(tab, phase)
	return lipgloss.NewStyle().Foreground(color).Render(glyph)
}

// renderMainTabBar draws the full-width tab bar that appears below the
// input box when at least one subagent tab exists.
func (m tuiModel) renderMainTabBar() string {
	active := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("33"))
	inactive := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

	// Main tab: fixed tabMainW visual chars.
	mainLabel := " main   "
	var bar string
	if m.subCur == -1 {
		bar = active.Render(mainLabel)
	} else {
		bar = inactive.Render(mainLabel)
	}
	// Gap AFTER main tab so sub tab slot 0 starts at tabMainW+tabGap = 9,
	// matching subTabSlotStart(0) when no tabs are scrolled off.
	bar += strings.Repeat(" ", tabGap)

	// Window to the sub tabs that fit; show a "‹N" indicator for any older tabs
	// scrolled off the left so the newest (running) tab is always visible.
	start, count := m.visibleSubTabs()
	if start > 0 {
		more := ansi.Truncate(sprint("‹%d", start), tabMoreW-1, "")
		more += strings.Repeat(" ", tabMoreW-lipgloss.Width(more))
		bar += lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(more)
	}

	// Sub tabs. Slot layout: " "(1) + dot(1) + " "(1) + label(2) + " "(1) + task(N) + " "(1) + "×"(1) = 8+N
	// For slot = tabSubW = 18: N = tabSubW-8 = 10.
	for slot := 0; slot < count; slot++ {
		i := start + slot
		tab := m.subTabs[i]
		dot := renderSubTabDot(tab, m.dotPhase)
		label := sprint("s%d", tab.n)
		if len(label) > 2 {
			label = label[:2]
		}
		taskMax := tabSubW - 8 // 8 fixed chars + taskMax = tabSubW exactly
		taskStr := ansi.Truncate(tab.task, taskMax, "…")
		// Pad task to taskMax so the × stays in a fixed column.
		taskStr = taskStr + strings.Repeat(" ", taskMax-lipgloss.Width(ansi.Strip(taskStr)))
		xStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

		var labelPart string
		if i == m.subCur {
			labelPart = active.Render(label)
		} else {
			labelPart = inactive.Render(label)
		}
		// Running tabs show a dim · instead of × — can't be closed mid-stream.
		// Finished and done tabs show × (the child has completed).
		var closeChar string
		if tab.done || tab.finished {
			closeChar = xStyle.Render("×")
		} else {
			closeChar = lipgloss.NewStyle().Foreground(lipgloss.Color("237")).Render("·")
		}
		cell := " " + dot + " " + labelPart + " " + taskStr + " " + closeChar
		bar += cell + strings.Repeat(" ", tabGap)
	}
	return lipgloss.NewStyle().Width(m.width).Render(bar)
}

// subTabModel returns the model display string for a subagent tab. The model
// is known at Start time (resolved from the endpoint view, including any
// /submodel override). Falls back to "…" when empty (edge case: Start
// didn't carry a model — e.g. constructed directly in a test).
func subTabModel(tab *subTab) string {
	if tab.model != "" {
		return tab.model
	}
	return "…"
}

// costLines builds the "costs" sidebar block with two subtotals — billed (exact,
// what you owe external providers) and est (modeled/approx, compute estimate) —
// followed by per-source rows sorted by cost descending. The two subtotals are
// kept visually distinct so a real bill and a compute-cost guess can never be
// confused. Returns nil when no costs have been recorded.
func (m tuiModel) costLines(innerW int) []string {
	billedTotal, estimatedTotal, anyBilled, anyEstimated, rows :=
		m.app.Costs.SnapshotSplit()
	if len(rows) == 0 {
		return nil
	}
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	lines := []string{"", dimStyle.Render("costs")}

	// Subtotal rows sit in the same column grid as the source rows below them:
	// 4-char indent (matching "  glyph ") + 9-char label + 1 space + 5-char cost.
	subtotalLine := func(label, cell, colorStr string) string {
		return "    " +
			dimStyle.Width(9).MaxWidth(9).Render(label) + " " +
			lipgloss.NewStyle().Foreground(lipgloss.Color(colorStr)).Width(5).MaxWidth(5).Render(cell)
	}

	// "billed" — sum of exact+priced rows (solid green = real charge).
	if anyBilled {
		cell, color := "—", "240"
		if billedTotal > 0 {
			cell = proxy.FmtUSDCompact(billedTotal)
			color = "2" // solid green: exact, billed-grade
		}
		lines = append(lines, subtotalLine("billed", cell, color))
	}

	// "est" — sum of modeled/approx+priced rows (amber = estimate, not a bill).
	if anyEstimated {
		cell, color := "—", "240"
		if estimatedTotal > 0 {
			cell = proxy.FmtUSDCompact(estimatedTotal)
			color = "214" // amber: estimated, not billed
		}
		lines = append(lines, subtotalLine("est", cell, color))
	}

	// Per-source rows. Source names are compacted to ≤9 chars for the sidebar.
	for _, r := range rows {
		glyph, color := proxy.CostGlyphStyle(r.Confidence)
		g := lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render(glyph)
		displayName := shortSourceName(r.Source)
		name := lipgloss.NewStyle().Foreground(lipgloss.Color("252")).
			Width(9).MaxWidth(9).Render(displayName)
		costColor := "252"
		if !r.Priced {
			costColor = "240"
		}
		costSeg := lipgloss.NewStyle().Foreground(lipgloss.Color(costColor)).
			Width(5).MaxWidth(5).Render(proxy.CostCell(r))
		callsSeg := dimStyle.Render(sprint("·%d", r.Calls))
		lines = append(lines, "  "+g+" "+name+" "+costSeg+" "+callsSeg)
	}
	return lines
}

// shortSourceName derives a compact ≤9 visual-char display label for a source
// key. Per-backend inference keys ("inference·<backend>") and per-model mashura
// keys ("mashura·<model>") are abbreviated so they fit the narrow sidebar column.
func shortSourceName(s string) string {
	if strings.HasPrefix(s, proxy.CostSourceInfPrefix) {
		// "inference·<backend>" or "inference·<backend>/<model>" → "inf·<abbrev>"
		tail := s[len(proxy.CostSourceInfPrefix):]
		if i := strings.IndexByte(tail, '/'); i >= 0 {
			tail = tail[:i] // keep only the backend name for display
		}
		return ansi.Truncate("inf·"+tail, 9, "…")
	}
	if strings.HasPrefix(s, proxy.CostSourceMashuraPrefix) {
		// "mashura·<provider:model>" → "m·<shortmodel>"
		model := s[len(proxy.CostSourceMashuraPrefix):]
		// strip provider prefix: "anthropic:claude-opus" → "claude-opus"
		if i := strings.IndexByte(model, ':'); i >= 0 {
			model = model[i+1:]
		}
		// strip path prefix: "google/gemini-2.5-pro" → "gemini-2.5-pro"
		if i := strings.LastIndexByte(model, '/'); i >= 0 {
			model = model[i+1:]
		}
		// trim "claude-" from Anthropic model IDs for extra compactness
		model = strings.TrimPrefix(model, "claude-")
		return ansi.Truncate("m·"+model, 9, "…")
	}
	return ansi.Truncate(s, 9, "…")
}

// groundingTypeTag returns the short bracketed tag shown before a grounding
// entry label in the sidebar. Unknown types fall back to the raw type string.
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

// mashuraPanelLabel returns a short display string for the active mashura panel,
// reading the panel config that the "review" tool would resolve to.
func mashuraPanelLabel(cfg config.Config) string {
	name := "default"
	if cfg.MashuraToolPanels != nil {
		if p := cfg.MashuraToolPanels["review"]; p != "" {
			name = p
		}
	}
	if cfg.MashuraPanels != nil {
		if p, ok := cfg.MashuraPanels[name]; ok && len(p.Models) > 0 {
			switch p.Mode {
			case "fusion":
				return fmt.Sprintf("fusion (%d models)", len(p.Models))
			case "fallback":
				return fmt.Sprintf("%s +fallback", mashuraShortModel(p.Models[0]))
			default:
				if len(p.Models) == 1 {
					return mashuraShortModel(p.Models[0])
				}
				return fmt.Sprintf("%d models", len(p.Models))
			}
		}
	}
	return cfg.OracleModel
}

// mashuraShortModel strips the "provider:" prefix for compact display.
func mashuraShortModel(s string) string {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		return s[i+1:]
	}
	return s
}
