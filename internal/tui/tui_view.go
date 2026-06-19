package tui

import (
	"fmt"
	"os"
	"strings"

	agent "wakil/internal/agent"
	"wakil/internal/config"
	"wakil/internal/proxy"
	"wakil/internal/tools"

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
	styleSidebarBorder = lipgloss.NewStyle().
				Border(lipgloss.HiddenBorder()).
				Padding(0, 1)

	styleConvBorder = lipgloss.NewStyle().
			Border(lipgloss.HiddenBorder())

	styleInputBorder = lipgloss.NewStyle().
				Border(lipgloss.HiddenBorder())

	styleTitle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("33"))
	styleState = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
)

func styleUser(s string) string {
	return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("33")).Render(s)
}
func styleAsst(s string) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Render(s)
}
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

	// --- right sidebar ---
	// In Lip Gloss, .Width() is the content+padding block; the border is added
	// outside it. So Width(sidebarWidth-2) + border(2) = sidebarWidth outer.
	// Combined with conv (m.width-sidebarWidth) = m.width ✓
	sb := m.renderSidebar(vpH)

	top := lipgloss.JoinHorizontal(lipgloss.Top, conv, sb)

	// Input spans the full terminal width.
	// styleInputBorder border = 2 cols; inner = m.width-2.
	// Textarea was set to m.width-4 (border 2 + Bubbles side padding 2).
	// The hist/ctx block sits at the bottom-right, beside the textarea (both are
	// 3 rows tall). On a narrow terminal the block is dropped and the textarea
	// reclaims the full width — reflow()/sizes() use the same helper so widths agree.
	row := m.ta.View()
	if block, _, show := m.inputContextBlock(); show {
		row = lipgloss.JoinHorizontal(lipgloss.Top, m.ta.View(), strings.Repeat(" ", ctxGap), block)
	}
	// The status line is only rendered when non-empty, so an idle input box sits
	// flush against the conversation pane (no wasted blank row). sizes() reserves
	// the matching height via the same statusLine() helper.
	body := row
	if status := m.statusLine(); status != "" {
		body = status + "\n" + body
	}
	input := styleInputBorder.
		Width(m.width - 2).
		Render(body)

	// The "@" completion picker sits between the conversation and the input.
	// The tab bar (when sub tabs exist) sits at the bottom, below the input.
	var sections []string
	sections = append(sections, top)
	if m.comp.active {
		sections = append(sections, m.renderCompletion())
	}
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

// statusLineInput carries all the state needed by buildStatusLine. It is a
// plain struct so the builder is a pure function and can be unit-tested.
type statusLineInput struct {
	state            agentState
	autoApprove      bool
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
	// Reverse-search prompt. When non-empty, buildStatusLine shows ONLY this
	// (suppressing all other segments) so the search prompt owns the status row.
	searchPrompt string
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

// buildStatusLine constructs the status line string from left to right by
// persistence:
//
//	(a) activity dot   — always present
//	(b) static modes   — AUTO, raw, workflow phase+step
//	(c) activity state — streaming / reasoning / confirming / compacting /
//	                     awaiting input (idle after ≥1 turn)
//	(d) transient      — t/s rate, flash
//
// Segments are joined with " · ". Returns at minimum the dot.
func buildStatusLine(in statusLineInput) string {
	const sep = " · "

	dot := renderStatusDot(in.state, in.dotPhase)

	// Reverse-search prompt: when active, owns the status row exclusively.
	if in.searchPrompt != "" {
		return dot + sep + in.searchPrompt
	}

	var parts []string

	// (b) Static modes — persistent, always left of activity.
	// Backend segment: shown when the last turn used a non-default backend, or
	// when the proxy overrode the requested backend (misroute marker).
	if in.backendUsed != "" {
		isDefault := in.backendUsed == in.backendDefault
		isOverridden := in.backendRequested != "" && in.backendUsed != in.backendRequested
		if !isDefault || isOverridden {
			label := in.backendUsed
			if isOverridden {
				label += "!"
			}
			parts = append(parts, lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Render(label))
		}
	}
	if in.autoApprove {
		parts = append(parts, lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true).Render("AUTO"))
	}
	if in.rawTools {
		parts = append(parts, lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("raw"))
	}
	if in.workflowLabel != "" {
		parts = append(parts, lipgloss.NewStyle().Foreground(lipgloss.Color("33")).Render("plan "+in.workflowLabel))
	}

	// (c) Activity state.
	switch in.state {
	case stateStreaming:
		if in.reasoning {
			parts = append(parts, styleState.Render("reasoning"))
		} else {
			parts = append(parts, styleState.Render("streaming"))
		}
	case stateConfirm:
		parts = append(parts, styleState.Render("confirming"))
	case stateCompacting:
		parts = append(parts, styleState.Render("compacting"))
	case stateIdle:
		if in.hadTurn {
			parts = append(parts, dim2("awaiting input"))
		}
	}

	// (d) Transient metrics — only meaningful during streaming.
	if in.tps > 0 && in.state == stateStreaming {
		parts = append(parts, dim2(sprint("%.0f t/s", in.tps)))
	}
	if in.flash != "" {
		parts = append(parts, lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render(in.flash))
	}

	if len(parts) == 0 {
		return dot
	}
	return dot + sep + strings.Join(parts, sep)
}

// statusLine delegates to buildStatusLine after assembling the current model
// state. The "wakīl" name is intentionally absent (it still heads the sidebar).
// Both View() and sizes() derive the input-box height from this; because the
// dot is always present, the status row is always reserved.
func (m tuiModel) statusLine() string {
	var workflowLabel string
	if m.app != nil && m.app.Workflow != nil {
		workflowLabel = m.app.Workflow.SidebarLabel()
	}
	backendUsed, backendRequested, backendDefault := "", "", ""
	if m.app != nil {
		if m.app.Client != nil {
			backendUsed = m.app.Client.LastUsedBackend()
		}
		backendRequested = m.app.SelectedBackend
		backendDefault = m.app.Cfg.Backend
	}
	return buildStatusLine(statusLineInput{
		state:            m.state,
		autoApprove:      m.app != nil && m.app.AutoApprove,
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
		searchPrompt:     m.searchPrompt(),
	})
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
	// Reserve room for the prefix + dot + separator (4 chars: " · ").
	// Use lipgloss.Width (display columns) not byte length, so multibyte
	// queries (CJK, accented chars) reserve the correct width.
	maxPreview := m.width - lipgloss.Width(prefix) - 4
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

// truncateForDisplay trims s to maxRunes runes, appending "…" if truncated.
// Handles multi-byte runes correctly. Returns s unchanged if maxRunes <= 0
// and s fits.
func truncateForDisplay(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes-1]) + "…"
}

// ctxGap is the column gap between the textarea and the hist/ctx block.
const ctxGap = 2

// contextBlockWidth is the fixed column width reserved for the hist/ctx block in
// the input box. It depends only on the resolved n_ctx (constant for a session),
// never on current usage, so reflow() and View() always reserve the same width.
func (m tuiModel) contextBlockWidth() int {
	maxK := m.app.ContextLimit().NCtx / 1000
	capW := 5 + len(sprint("%dk / %dk  100%%", maxK, maxK)) // caption at full usage
	meterW := 5 + 13                                        // key(5) + 12 cells + divider
	if capW > meterW {
		return capW
	}
	return meterW
}

// inputContextBlock returns the rendered hist/ctx block, the width the textarea
// should take, and whether the block is shown. On a narrow terminal it hides the
// block and gives the textarea the full width.
func (m tuiModel) inputContextBlock() (block string, taW int, show bool) {
	full := m.width - 4 // matches the original textarea width
	bw := m.contextBlockWidth()
	taW = full - ctxGap - bw
	if taW < 24 {
		return "", full, false
	}
	return m.renderContextBlock(bw), taW, true
}

// renderContextBlock renders the three-line hist/ctx panel padded to bw columns:
//
//	hist  13  22k
//	ctx   ⣿⣀⣀⣀⣀⣀┊⣀⣀⣀⣀⣀⣀
//	      48k / 196k  24%
//
// hist is the stored transcript size in bytes (turns + KB). ctx is the real
// token occupancy of the window — the backend's last reported prompt_tokens —
// measured against the authoritative per-slot n_ctx (resolved at startup). The
// caption color shifts green→amber→red: amber once usage crosses the usable
// budget (n_ctx minus reasoning/answer headroom), red near the ceiling. When
// n_ctx came from the config fallback rather than the backend, the "ctx" key is
// amber as a standing cue that the ceiling is unverified.
func (m tuiModel) renderContextBlock(bw int) string {
	turns := len(m.app.Conv)
	histBytes := agent.TranscriptSize(m.app.Conv)

	lim := m.app.ContextLimit()
	used := m.app.ContextTokensUsed()
	total := lim.NCtx
	usable := lim.Usable()
	pct := 0
	if total > 0 {
		pct = used * 100 / total
	}
	color := lipgloss.Color("2")
	if used >= total*90/100 {
		color = lipgloss.Color("1")
	} else if used >= usable {
		color = lipgloss.Color("214")
	}
	key := func(s string) string {
		return lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Width(5).Render(s)
	}
	// The "ctx" key turns amber when the ceiling is an unverified fallback.
	ctxKey := key("ctx")
	if !lim.FromBackend() {
		ctxKey = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Width(5).Render("ctx")
	}
	cell := func(s string) string {
		return lipgloss.NewStyle().Width(bw).MaxWidth(bw).Render(s)
	}
	hist := key("hist") + lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Render(sprint("%d  %dk", turns, histBytes/1000))
	meter := ctxKey + brailleMeter(used, total, color, 6)
	caption := key("") + dim2(sprint("%dk / %dk", used/1000, total/1000)) + "  " +
		lipgloss.NewStyle().Foreground(color).Render(sprint("%d%%", pct))
	return strings.Join([]string{cell(hist), cell(meter), cell(caption)}, "\n")
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
		more += strings.Repeat(" ", tabMoreW-len([]rune(more)))
		bar += lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render(more)
	}

	// Sub tabs. Slot layout: " "(1) + dot(1) + " "(1) + label(2) + " "(1) + task(N) + " "(1) + "×"(1) = 8+N
	// For slot = tabSubW = 18: N = tabSubW-8 = 10.
	for slot := 0; slot < count; slot++ {
		i := start + slot
		tab := m.subTabs[i]
		dot := lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("●") // running
		if tab.done {
			dot = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render("✓") // done
		}
		label := sprint("s%d", tab.n)
		if len(label) > 2 {
			label = label[:2]
		}
		taskMax := tabSubW - 8 // 8 fixed chars + taskMax = tabSubW exactly
		taskStr := ansi.Truncate(tab.task, taskMax, "…")
		// Pad task to taskMax so the × stays in a fixed column.
		taskStr = taskStr + strings.Repeat(" ", taskMax-len([]rune(ansi.Strip(taskStr))))
		xStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))

		var labelPart string
		if i == m.subCur {
			labelPart = active.Render(label)
		} else {
			labelPart = inactive.Render(label)
		}
		// Running tabs show a dim · instead of × — can't be closed mid-stream.
		var closeChar string
		if tab.done {
			closeChar = xStyle.Render("×")
		} else {
			closeChar = lipgloss.NewStyle().Foreground(lipgloss.Color("237")).Render("·")
		}
		cell := " " + dot + " " + labelPart + " " + taskStr + " " + closeChar
		bar += cell + strings.Repeat(" ", tabGap)
	}
	return lipgloss.NewStyle().Width(m.width).Render(bar)
}

// subTabContent returns the inner content lines for a sub-agent tab.

// renderSidebar returns the right sidebar rendered to exactly sidebarWidth cols
// and vpH+2 rows (matching the conversation pane outer height). When a sub tab
// is active it shows that subagent's info instead of the main agent's.
func (m tuiModel) renderSidebar(vpH int) string {
	innerW := sidebarWidth - 4
	var lines []string
	if m.subCur >= 0 && m.subCur < len(m.subTabs) {
		lines = m.subSidebarLines(m.subTabs[m.subCur], innerW, vpH)
	} else {
		lines = m.mainSidebarLines(innerW, vpH)
	}
	return styleSidebarBorder.
		Width(sidebarWidth - 2).
		Height(vpH).
		Render(strings.Join(lines, "\n"))
}

// subSidebarLines builds the content lines for a subagent tab's sidebar view.
func (m tuiModel) subSidebarLines(tab *subTab, innerW, maxLines int) []string {
	keyW := 6
	valW := innerW - keyW - 1
	row := func(k, v string) string {
		v = ansi.Truncate(v, valW, "…")
		return lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Width(keyW).Render(k) + " " +
			lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Width(valW).MaxWidth(valW).Render(v)
	}
	statusStr := lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render("running…")
	if tab.done {
		statusStr = lipgloss.NewStyle().Foreground(lipgloss.Color("2")).Render("done ✓")
	}
	lines := []string{
		styleTitle.Render("wakīl"),
		"",
		row("proxy", hostOnly(m.app.Client.BaseURL)),
		row("model", m.app.EffectiveModel()),
		row("exec", "docker"),
		row("cwd", m.app.Exec.Cwd()),
		row("chat", agent.ShortID(tab.chatID)),
	}
	// Backend display: show when a backend was used or requested for this subagent.
	// usedBackend takes precedence; fall back to the resolved backend.
	// Append "!" when the proxy routed to a different backend than requested.
	subBackendDisplay := tab.usedBackend
	if subBackendDisplay == "" {
		subBackendDisplay = tab.backend
	}
	if subBackendDisplay != "" {
		mainDefault := m.app.Cfg.Backend
		isDefault := subBackendDisplay == mainDefault
		isOverridden := tab.backend != "" && tab.usedBackend != "" && tab.backend != tab.usedBackend
		// Show when non-default or overridden (same logic as main status line).
		if !isDefault || isOverridden {
			label := subBackendDisplay
			if isOverridden {
				label += "!"
			}
			lines = append(lines, row("backend", label))
		}
	}
	lines = append(lines,
		"",
		statusStr,
		"",
		lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("tools"),
		"  read_file",
		"  search_files",
		"  find_files",
		"  list_dir",
	)
	if tab.done && tab.hardMaxBytes > 0 {
		pct := 0
		if tab.hardMaxBytes > 0 {
			pct = tab.ctxSize * 100 / tab.hardMaxBytes
		}
		lines = append(lines, "",
			lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("ctx"),
			sprint("  %dk / %dk  %d%%", tab.ctxSize/1000, tab.hardMaxBytes/1000, pct),
		)
	}
	lines = append(lines, "", lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("grounded on"))
	if len(tab.grounding) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("  —"))
	} else {
		tagStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
		valStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
		for _, e := range tab.grounding {
			tag := groundingTypeTag(e.Type)
			label := ansi.Truncate(e.Label, innerW-8, "…")
			lines = append(lines, "  "+tagStyle.Render(tag)+" "+valStyle.Render(label))
		}
	}
	return lines
}

// mainSidebarLines builds the content lines for the "main" sidebar tab:
// proxy/model/exec info, tools, and grounding.
func (m tuiModel) mainSidebarLines(innerW, maxLines int) []string {
	keyW := 6
	valW := innerW - keyW - 1
	row := func(k, v string) string {
		v = ansi.Truncate(v, valW, "…")
		kk := lipgloss.NewStyle().
			Foreground(lipgloss.Color("240")).
			Width(keyW).
			Render(k)
		vv := lipgloss.NewStyle().
			Foreground(lipgloss.Color("252")).
			Width(valW).
			MaxWidth(valW).
			Render(v)
		return kk + " " + vv
	}

	isDirect := strings.HasPrefix(m.app.Exec.Describe(), "direct")
	mode := "docker"
	if isDirect {
		mode = "direct"
	}

	imgVal := m.app.Cfg.Image
	if i := strings.LastIndex(imgVal, "/"); i >= 0 {
		imgVal = imgVal[i+1:]
	}

	lines := []string{
		styleTitle.Render("wakīl"),
		"",
		row("proxy", hostOnly(m.app.Client.BaseURL)),
		row("model", m.app.EffectiveModel()),
		row("exec", mode),
	}
	if !isDirect {
		lines = append(lines, row("img", imgVal))
	}
	lines = append(lines,
		row("cwd", m.app.Exec.Cwd()),
		row("chat", agent.ShortID(m.app.Client.ChatID)),
	)
	if m.app.Workflow != nil {
		lines = append(lines,
			row("wf", m.app.Workflow.SidebarLabel()),
		)
	}
	if m.app.Cfg.OracleEnabled {
		anthropicOk := os.Getenv(m.app.Cfg.OracleAPIKeyEnv) != ""
		openrouterOk := os.Getenv("OPENROUTER_API_KEY") != ""
		if anthropicOk || openrouterOk {
			lines = append(lines,
				lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("mashūra")+" "+
					lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Render(mashuraPanelLabel(m.app.Cfg)))
		} else {
			lines = append(lines,
				lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("mashūra")+" "+
					lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Render("no key"))
		}
	}

	type toolEntry struct{ name, label, status string }
	var entries []toolEntry
	if m.app.MCP != nil {
		for _, srv := range m.app.MCP.Servers() {
			label := sprint("%d tools", len(srv.Tools))
			if srv.Status == "failed" {
				label = "failed"
			} else if srv.Status == "connecting" {
				label = "…"
			}
			entries = append(entries, toolEntry{srv.Cfg.Name, label, srv.Status})
		}
	}
	if m.app.Cfg.SearXngURL != "" {
		entries = append(entries, toolEntry{"searxng", sprint("%d tools", len(tools.SearxngTools())), "connected"})
	}
	// mashūra is now shown in the info section above, not in tools
	if len(entries) > 0 {
		lines = append(lines, "", lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("tools"))
		for _, e := range entries {
			icon, color := "✓", lipgloss.Color("2")
			if e.status == "failed" {
				icon, color = "✗", lipgloss.Color("1")
			} else if e.status == "connecting" {
				icon, color = "…", lipgloss.Color("214")
			}
			statusIcon := lipgloss.NewStyle().Foreground(color).Render(icon)
			namePart := lipgloss.NewStyle().Foreground(lipgloss.Color("252")).
				Width(innerW - 14).MaxWidth(innerW - 14).Render(e.name)
			countPart := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).
				Render(e.label)
			lines = append(lines, "  "+statusIcon+" "+namePart+" "+countPart)
		}
	}

	lines = append(lines, m.costLines(innerW)...)

	lines = append(lines, "", lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("grounded on"))
	grounding := m.app.Client.Grounding()
	if len(grounding) == 0 {
		lines = append(lines, lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("  —"))
	} else {
		tagStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
		valStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
		for _, e := range grounding {
			tag := groundingTypeTag(e.Type)
			// tag is 5 chars; " " = 1; "  " indent = 2 → 8 fixed, rest for label
			label := ansi.Truncate(e.Label, innerW-8, "…")
			lines = append(lines, "  "+tagStyle.Render(tag)+" "+valStyle.Render(label))
		}
	}
	return lines
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
