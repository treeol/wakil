package tui

import (
	"strings"
	"unicode"

	"github.com/treeol/wakil/internal/core/sessionclient"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// resumePickerMaxRows bounds how many session rows the modal shows at once.
const resumePickerMaxRows = 20

// resumePickerState is the session browser opened by bare "/resume": a modal
// drawn over the conversation area (it takes no layout rows), with a search
// line. Zero value = inactive.
//
// The list is read from the resume index on open (instant) and refreshed by a
// background reconcile (resumePickerRefreshMsg), so the screen never waits for
// the session files. Typing filters; arrows move; enter resumes; esc closes;
// tab (or "a" while the query is empty) toggles workspace ↔ all repos.
type resumePickerState struct {
	active   bool
	all      []sessionclient.SessionSummary // everything the scope returned
	sessions []sessionclient.SessionSummary // all, filtered by query
	scope    sessionclient.SessionScope
	hidden   int // sessions filtered out by scope; shown as a hint
	sel      int
	query    string
	loading  bool // a reconcile is in flight
	// gen identifies the current scope's reconcile: a refresh whose gen no
	// longer matches (scope toggled, picker reopened) is dropped.
	gen int
}

// resumePickerRefreshMsg carries the result of a background reconcile.
type resumePickerRefreshMsg struct {
	gen      int
	sessions []sessionclient.SessionSummary
	hidden   int
	err      error
}

// openResumePicker activates the picker with a session list (m4b: the facade's
// ListSessions result, not an agent message).
func (m tuiModel) openResumePicker(sessions []sessionclient.SessionSummary, scope sessionclient.SessionScope, hidden int) tuiModel {
	m.comp = completionState{} // never both pickers open at once
	m.resumePicker = resumePickerState{
		active: true,
		all:    sessions,
		scope:  scope,
		hidden: hidden,
		gen:    m.resumePicker.gen + 1,
	}
	m.resumePicker.sessions = filterSessions(sessions, "")
	return m
}

// closeResumePicker deactivates the picker without side effects. gen survives
// so a late refresh of the closed picker cannot match a reopened one.
func (m tuiModel) closeResumePicker() tuiModel {
	m.resumePicker = resumePickerState{gen: m.resumePicker.gen}
	return m
}

// resumeReconcileCmd refreshes the picker's list in the background: the facade
// brings the index up to date from the session files (stat-only when warm),
// then the result arrives as a resumePickerRefreshMsg tagged with gen.
func (m tuiModel) resumeReconcileCmd(gen int, scope sessionclient.SessionScope) tea.Cmd {
	f := m.facade
	if f == nil {
		return nil
	}
	return func() tea.Msg {
		sessions, hidden, err := f.ListSessions(scope, true)
		return resumePickerRefreshMsg{gen: gen, sessions: sessions, hidden: hidden, err: err}
	}
}

// applyResumeRefresh installs a reconcile result, keeping the selection on the
// same chat when it is still listed. It is dropped when the picker is closed
// or the result belongs to an earlier scope.
func (m tuiModel) applyResumeRefresh(r resumePickerRefreshMsg) tuiModel {
	if !m.resumePicker.active || r.gen != m.resumePicker.gen {
		return m
	}
	m.resumePicker.loading = false
	if r.err != nil {
		return m // keep what the index gave us
	}
	selID := ""
	if m.resumePicker.sel < len(m.resumePicker.sessions) {
		selID = m.resumePicker.sessions[m.resumePicker.sel].ChatID
	}
	m.resumePicker.all = r.sessions
	m.resumePicker.hidden = r.hidden
	m = m.refilterResumePicker()
	if selID != "" {
		for i, s := range m.resumePicker.sessions {
			if s.ChatID == selID {
				m.resumePicker.sel = i
				break
			}
		}
	}
	return m
}

// refilterResumePicker recomputes the visible list from all + query and clamps
// the selection.
func (m tuiModel) refilterResumePicker() tuiModel {
	m.resumePicker.sessions = filterSessions(m.resumePicker.all, m.resumePicker.query)
	if m.resumePicker.sel >= len(m.resumePicker.sessions) {
		m.resumePicker.sel = 0
	}
	return m
}

// filterSessions keeps the sessions whose title, label or workspace contains
// query (case-insensitive substring). An empty query keeps everything.
func filterSessions(all []sessionclient.SessionSummary, query string) []sessionclient.SessionSummary {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return all
	}
	var out []sessionclient.SessionSummary
	for _, s := range all {
		if strings.Contains(strings.ToLower(s.DisplayTitle()), q) ||
			strings.Contains(strings.ToLower(s.Label), q) ||
			strings.Contains(strings.ToLower(s.Workspace), q) {
			out = append(out, s)
		}
	}
	return out
}

// toggleResumeScope flips workspace ↔ all repos. The index is read at once (a
// cheap file read) and a fresh reconcile is started for the new scope.
func (m tuiModel) toggleResumeScope() (tuiModel, tea.Cmd) {
	scope := m.resumePicker.scope
	scope.All = !scope.All
	sessions, hidden, err := m.facade.ListSessions(scope, false)
	if err != nil {
		return m, nil
	}
	m.resumePicker.scope = scope
	m.resumePicker.all = sessions
	m.resumePicker.hidden = hidden
	m.resumePicker.gen++
	m.resumePicker.loading = true
	m.resumePicker.sel = 0
	m = m.refilterResumePicker()
	return m, m.resumeReconcileCmd(m.resumePicker.gen, scope)
}

// handleResumePickerKey processes input while the picker is open. Returns the
// updated model, a Cmd (resume on Enter, reconcile on a scope toggle), and
// whether the key was consumed. Every key is consumed while the picker is
// active — it owns input exclusively, like the confirm gate — except
// ctrl+c/ctrl+d, which still quit.
func (m tuiModel) handleResumePickerKey(msg tea.KeyMsg) (tuiModel, tea.Cmd, bool) {
	if !m.resumePicker.active {
		return m, nil, false
	}
	n := len(m.resumePicker.sessions)
	switch msg.Type {
	case tea.KeyCtrlC, tea.KeyCtrlD:
		// Let the normal quit handling apply — a picker with nothing running
		// underneath should not block quitting the program.
		return m, nil, false
	case tea.KeyUp, tea.KeyCtrlP:
		if n > 0 {
			m.resumePicker.sel = (m.resumePicker.sel - 1 + n) % n
		}
		return m, nil, true
	case tea.KeyDown, tea.KeyCtrlN:
		if n > 0 {
			m.resumePicker.sel = (m.resumePicker.sel + 1) % n
		}
		return m, nil, true
	case tea.KeyPgUp:
		m.resumePicker.sel = maxInt(0, m.resumePicker.sel-resumePickerMaxRows/2)
		return m, nil, true
	case tea.KeyPgDown:
		if n > 0 {
			m.resumePicker.sel = minInt(n-1, m.resumePicker.sel+resumePickerMaxRows/2)
		}
		return m, nil, true
	case tea.KeyTab:
		m, cmd := m.toggleResumeScope()
		return m, cmd, true
	case tea.KeyEsc:
		m = m.closeResumePicker()
		return m, nil, true
	case tea.KeyBackspace:
		if r := []rune(m.resumePicker.query); len(r) > 0 {
			m.resumePicker.query = string(r[:len(r)-1])
			m = m.refilterResumePicker()
		}
		return m, nil, true
	case tea.KeyCtrlU:
		m.resumePicker.query = ""
		m = m.refilterResumePicker()
		return m, nil, true
	case tea.KeyEnter:
		if n == 0 || m.resumePicker.sel >= n {
			m = m.closeResumePicker()
			return m, nil, true
		}
		s := m.resumePicker.sessions[m.resumePicker.sel]
		m = m.closeResumePicker()
		// Rotation through the ConversationManager (async —
		// ResumeConversation loads the session). beginRotate also sets the
		// rotating flag (previously the picker resume never blocked sends)
		// and arms the rotation status label + dot tick. The status zone may
		// flip 0↔N rows from a fresh splash state — reflow synchronously,
		// same as the applyCommandResult rotation path (the tick can't
		// detect the flip: it snapshots after rotating is already set).
		id := s.ChatID
		// Reflow only when the model is fully initialized — raw test models
		// have a nil items slice; reflow→refreshViewport would dereference it.
		// The production model always has items non-nil (NewTUIModel).
		before := m.effectiveStatusRows()
		m, cmd := m.beginRotate(rotateResume, rotationRequest{kind: rotateResume, sessionID: id})
		if m.items != nil {
			m = m.reflowIfStatusHeightChanged(before)
		}
		return m, cmd, true
	case tea.KeySpace:
		m.resumePicker.query += " "
		m = m.refilterResumePicker()
		return m, nil, true
	case tea.KeyRunes:
		// "a" with an empty query keeps its old meaning (scope toggle); once
		// a query exists it is just a search character.
		if m.resumePicker.query == "" && string(msg.Runes) == "a" && !msg.Paste {
			m, cmd := m.toggleResumeScope()
			return m, cmd, true
		}
		var b strings.Builder
		for _, r := range msg.Runes {
			if unicode.IsPrint(r) {
				b.WriteRune(r)
			}
		}
		if b.Len() > 0 {
			m.resumePicker.query += b.String()
			m.resumePicker.sel = 0
			m = m.refilterResumePicker()
		}
		return m, nil, true
	}
	return m, nil, true // swallow every other key while the picker owns input
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// resumeModalSize is the picker box's outer width and its row budget for the
// current terminal size.
func (m tuiModel) resumeModalSize() (width, rows int) {
	width = m.width - 8
	if width > 110 {
		width = 110
	}
	if width < 30 {
		width = m.width - 2
	}
	if width < 10 {
		width = 10
	}
	rows = m.height - 13
	if rows > resumePickerMaxRows {
		rows = resumePickerMaxRows
	}
	if rows < 1 {
		rows = 1
	}
	return width, rows
}

// renderResumePicker draws the picker box. view() centres it over the
// conversation area; it takes no layout rows of its own.
func (m tuiModel) renderResumePicker() string {
	width, rows := m.resumeModalSize()
	inner := width - 4 // border (2) + padding (2)
	if inner < 6 {
		inner = 6
	}
	line := func(s string) string { return lipgloss.NewStyle().MaxWidth(inner).Render(s) }

	scopeLabel := "this workspace"
	if m.resumePicker.scope.All {
		scopeLabel = "all repos"
	}
	head := "resume — " + scopeLabel
	if m.resumePicker.hidden > 0 && !m.resumePicker.scope.All {
		head += sprint("  (%d hidden)", m.resumePicker.hidden)
	}
	if m.resumePicker.loading {
		head += "  · indexing…"
	}
	var b strings.Builder
	b.WriteString(line(dim2(head)))
	b.WriteString("\n" + line("> "+m.resumePicker.query+"▏"))

	sessions := m.resumePicker.sessions
	switch {
	case len(sessions) == 0 && m.resumePicker.query != "":
		b.WriteString("\n" + line(dim2("  no match")))
	case len(sessions) == 0:
		b.WriteString("\n" + line(dim2("  no sessions — press tab for all repos")))
	default:
		start := 0
		if m.resumePicker.sel >= rows {
			start = m.resumePicker.sel - rows + 1
		}
		end := minInt(start+rows, len(sessions))
		for i := start; i < end; i++ {
			s := sessions[i]
			turns, _ := s.Turns()
			id := formatShortID(s.ChatID)
			if s.Label != "" {
				id += " [" + s.Label + "]"
			}
			text := sprint("%-20s  %s  %3d turns  %s", id, s.Updated.Format("01-02 15:04"), turns, strings.ReplaceAll(s.DisplayTitle(), "\n", " "))
			if m.resumePicker.scope.All && s.Workspace != "" {
				text += "  · " + s.Workspace
			}
			if s.Legacy {
				text += "  (legacy)"
			}
			if i == m.resumePicker.sel {
				b.WriteString("\n" + lipgloss.NewStyle().Reverse(true).MaxWidth(inner).Render(" "+text+" "))
			} else {
				b.WriteString("\n" + lipgloss.NewStyle().Foreground(lipgloss.Color("252")).MaxWidth(inner).Render("  "+text))
			}
		}
	}
	b.WriteString("\n" + line(dim2(sprint("%d/%d · ↑↓ select · enter resume · tab scope · esc close", minInt(m.resumePicker.sel+1, len(sessions)), len(sessions)))))
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color("238")).
		Padding(0, 1).
		Width(width - 2).
		Render(b.String())
}
