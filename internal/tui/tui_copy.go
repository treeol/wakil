package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// nativeSelection mirrors the application mouse-reporting state. When true,
// mouse reporting is DISABLED (tea.DisableMouse) so the terminal's own
// click-drag selection works — the reflex most users reach for first, and one
// the in-app drag selection can't replace. Toggled with alt+t.
//
// Lifecycle: toggling sets this flag and returns the corresponding bubbletea
// Cmd; the renderer owns the actual escape sequences. In-app selection state
// is cleared on every transition (its coordinates only make sense while the
// app receives mouse events). While nativeSelection is true, handleMouse
// still runs defensively (a terminal that ignored the disable will keep
// delivering events) but the status line shows a persistent "select" hint so
// the mode is discoverable — not just a transient flash.
type nativeSelectionState struct {
	active bool
}

// toggleNativeSelection flips terminal-selection mode. Returns the new model,
// the bubbletea Cmd that enables/disables mouse reporting, and whether the
// mode is now on (for the status hint).
func (m tuiModel) toggleNativeSelection() (tuiModel, tea.Cmd) {
	m.nativeSel.active = !m.nativeSel.active
	// Coordinates of any in-app selection are meaningless once mouse reporting
	// changes; clear highlight (restoring styled viewport content) alongside.
	if m.sel.active || m.sel.pressed {
		wasActive := m.sel.active
		m.sel = selection{}
		if wasActive {
			m.refreshViewport()
		}
	}
	if m.nativeSel.active {
		return m, tea.DisableMouse
	}
	return m, tea.EnableMouseCellMotion
}

// lastAssistantSource returns the raw (pre-glamour) text of the most recent
// committed assistant response, or "" when none exists. Reads convItem.text,
// which holds the source string addItem received (flushStreaming commits the
// untransformed stream buffer; session_view.go stores message content
// verbatim). Streaming-in-flight text is intentionally NOT included: the
// shortcut copies completed responses. Leading/trailing blank lines are
// trimmed; interior formatting is preserved exactly.
func (m tuiModel) lastAssistantSource() string {
	if m.items == nil {
		return ""
	}
	for i := len(*m.items) - 1; i >= 0; i-- {
		it := (*m.items)[i]
		if it.kind != iAsst {
			continue
		}
		return strings.Trim(it.text, "\n")
	}
	return ""
}

// copyLastResponseCmd builds the copy command for the ctrl+y shortcut. The
// model return value carries the flash for the empty-history case.
func (m tuiModel) copyLastResponseCmd() (tuiModel, tea.Cmd) {
	text := m.lastAssistantSource()
	if text == "" {
		before := m.statusRows()
		m.flash = "nothing to copy — no assistant response yet"
		return m.reflowIfStatusHeightChanged(before), nil
	}
	return m, m.copyToClipboard(text)
}
