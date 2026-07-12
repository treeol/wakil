package tui

import (
	"strings"

	agent "github.com/treeol/wakil/internal/agent"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// resumePickerMaxVisible bounds how many session rows are shown at once
// before scrolling — same windowing idea as the "/" completion dropdown, but
// a picker is a full scrollable list, not capped to compMaxVisible.
const resumePickerMaxVisible = 12

// resumePickerState is the interactive session browser opened by bare
// "/resume". Zero value = inactive. Unlike the completion dropdown (which
// requires typing a prefix first), this shows every in-scope session
// immediately, newest first, and lets the user navigate/select without
// typing anything.
type resumePickerState struct {
	active   bool
	sessions []agent.Session
	scope    agent.SessionScope
	hidden   int // sessions filtered out by scope; shown as a hint
	sel      int
}

// openResumePicker activates the picker from an OpenResumePickerMsg.
func (m tuiModel) openResumePicker(msg agent.OpenResumePickerMsg) tuiModel {
	m.comp = completionState{} // never both pickers open at once
	m.resumePicker = resumePickerState{
		active:   true,
		sessions: msg.Sessions,
		scope:    msg.Scope,
		hidden:   msg.Hidden,
		sel:      0,
	}
	return m
}

// closeResumePicker deactivates the picker without side effects.
func (m tuiModel) closeResumePicker() tuiModel {
	m.resumePicker = resumePickerState{}
	return m
}

// reloadResumePicker toggles the picker's scope (current workspace ↔ all
// repos) and re-reads the session store. Synchronous disk I/O, same as
// fetchSessionShortIDs — the session store is small and local, and this only
// happens on an explicit keypress, not per-frame.
func (m tuiModel) reloadResumePicker(all bool) tuiModel {
	scope := m.resumePicker.scope
	scope.All = all
	sessions, hidden, err := agent.ListSessionsScoped(scope)
	if err != nil {
		return m
	}
	m.resumePicker.scope = scope
	m.resumePicker.sessions = sessions
	m.resumePicker.hidden = hidden
	if m.resumePicker.sel >= len(sessions) {
		m.resumePicker.sel = 0
	}
	return m
}

// handleResumePickerKey processes navigation while the picker is open.
// Returns the updated model, a Cmd (resume on Enter), and whether the key was
// consumed. Every key is consumed while the picker is active — it owns input
// exclusively, like the confirm gate.
func (m tuiModel) handleResumePickerKey(msg tea.KeyMsg) (tuiModel, tea.Cmd, bool) {
	if !m.resumePicker.active {
		return m, nil, false
	}
	n := len(m.resumePicker.sessions)
	switch msg.String() {
	case "up", "ctrl+p", "k":
		if n > 0 {
			m.resumePicker.sel = (m.resumePicker.sel - 1 + n) % n
		}
		return m, nil, true
	case "down", "ctrl+n", "j":
		if n > 0 {
			m.resumePicker.sel = (m.resumePicker.sel + 1) % n
		}
		return m, nil, true
	case "a":
		m = m.reloadResumePicker(!m.resumePicker.scope.All)
		return m, nil, true
	case "enter":
		if n == 0 || m.resumePicker.sel >= n {
			m = m.closeResumePicker()
			return m, nil, true
		}
		s := m.resumePicker.sessions[m.resumePicker.sel]
		m = m.closeResumePicker()
		app := m.app
		return m, func() tea.Msg { return agent.ResumeSessionMsg(app, &s) }, true
	case "esc":
		m = m.closeResumePicker()
		return m, nil, true
	case "ctrl+c", "ctrl+d":
		// Let the normal quit handling apply — a picker with nothing running
		// underneath should not block quitting the program.
		return m, nil, false
	}
	return m, nil, true // swallow other keys (typing) while the picker owns input
}

// resumePickerHeight is the outer (bordered) height of the picker, 0 when
// closed. Mirrors completionHeight's contract with sizes()/reflow().
func (m tuiModel) resumePickerHeight() int {
	if !m.resumePicker.active {
		return 0
	}
	rows := len(m.resumePicker.sessions)
	if rows == 0 {
		rows = 1 // "no sessions" line
	} else if rows > resumePickerMaxVisible {
		rows = resumePickerMaxVisible + 1 // extra row for "+N more"
	}
	rows++ // header row (scope + hidden-count hint)
	return rows + 2
}

// renderResumePicker draws the picker box (width matches the input pane).
func (m tuiModel) renderResumePicker() string {
	var b strings.Builder

	scopeLabel := "this workspace"
	if m.resumePicker.scope.All {
		scopeLabel = "all repos"
	}
	header := dim2("resume — " + scopeLabel + "  [↑↓ select · enter resume · a toggle scope · esc cancel]")
	if m.resumePicker.hidden > 0 && !m.resumePicker.scope.All {
		header += dim2(sprint("  (%d hidden)", m.resumePicker.hidden))
	}
	b.WriteString(header)

	sessions := m.resumePicker.sessions
	if len(sessions) == 0 {
		b.WriteString("\n" + dim2("  no sessions — press a for all repos"))
		return styleCompletionBorder.Width(m.width - 2).Render(b.String())
	}

	start := 0
	if m.resumePicker.sel >= resumePickerMaxVisible {
		start = m.resumePicker.sel - resumePickerMaxVisible + 1
	}
	end := start + resumePickerMaxVisible
	if end > len(sessions) {
		end = len(sessions)
	}
	for i := start; i < end; i++ {
		s := sessions[i]
		turns, first := agent.SessionTurns(s)
		first = strings.ReplaceAll(first, "\n", " ")
		if len(first) > 40 {
			first = first[:40] + "…"
		}
		id := agent.ShortID(s.ChatID)
		if s.Label != "" {
			id += " [" + s.Label + "]"
		}
		line := sprint("%-24s  %s  %2d turns  %s", id, s.Updated.Format("01-02 15:04"), turns, first)
		var row string
		if i == m.resumePicker.sel {
			row = lipgloss.NewStyle().Reverse(true).Render(" " + line + " ")
		} else {
			row = "  " + lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Render(line)
		}
		b.WriteString("\n" + row)
	}
	if len(sessions) > resumePickerMaxVisible {
		b.WriteString("\n" + dim2(sprint("  +%d more", len(sessions)-resumePickerMaxVisible)))
	}
	return styleCompletionBorder.Width(m.width - 2).Render(b.String())
}
