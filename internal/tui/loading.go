package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// loadingProgressMsg updates the status text shown by the start-up model.
type loadingProgressMsg struct {
	text string
}

// SendLoadingProgress sends a progress update to the start-up model.
func SendLoadingProgress(text string) {
	if programSend != nil {
		programSend(loadingProgressMsg{text: text})
	}
}

// BootstrapDone is the interface a bootstrap-completion message must
// implement. The loading model checks for this interface rather than
// treating every unhandled message as completion.
type BootstrapDone interface {
	tea.Msg
	IsBootstrapDone()
}

// renderBootStatus is the one-line status the normal view shows while the
// conversation is still being built, e.g. "· starting container…".
func renderBootStatus(text string) string {
	return lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("· " + text)
}

// loadingModel is the program's first model. There is no separate loading
// screen: it renders the normal view (splash logo and input box) from the
// first frame, with a one-line status overlaid on the last row of the
// conversation pane naming what is still starting (taking zero layout rows so
// the swap to the real model never shifts the logo or input box). It carries
// no conversation, so nothing is sent from it: Enter is ignored, typed text is
// kept and moved into the real input at the swap, and ctrl+c quits. It never
// runs tuiModel.Update, so the real model alone reports "prompt ready".
type loadingModel struct {
	view tuiModel

	// bootstrapCmd is the tea.Cmd that runs the async startup work.
	bootstrapCmd tea.Cmd

	// swapFn is called when the bootstrap Cmd completes.
	swapFn func(tea.Msg) (tea.Model, tea.Cmd)
}

// NewLoadingModel creates the start-up model. status is the first status line.
func NewLoadingModel(status string, bootstrapCmd tea.Cmd, swapFn func(tea.Msg) (tea.Model, tea.Cmd)) tea.Model {
	v := newBaseModel()
	v.bootStatus = status
	v.convCacheBox = &convPaneCache{}
	return &loadingModel{view: v, bootstrapCmd: bootstrapCmd, swapFn: swapFn}
}

func (m *loadingModel) Init() tea.Cmd {
	return m.bootstrapCmd
}

func (m *loadingModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.view.width = msg.Width
		m.view.height = msg.Height
		m.view, _ = m.view.reflowResize()
		m.view.ready = true
		timelineOnce("first-frame-boot", "first frame (start-up view)")
		return m, nil

	case loadingProgressMsg:
		m.view.bootStatus = msg.text
		// The status text never changes the row count (always one row), so
		// no reflow is needed.
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "ctrl+d":
			return m, tea.Quit
		case "enter", "alt+enter", "shift+enter":
			return m, nil
		}
		m.view.splashGone = true
		var cmd tea.Cmd
		m.view.ta, cmd = m.view.ta.Update(msg)
		return m, cmd

	case BootstrapDone:
		return m.swap(msg)

	default:
		return m, nil
	}
}

// swap installs the real model, keeping what the user typed while starting.
func (m *loadingModel) swap(msg tea.Msg) (tea.Model, tea.Cmd) {
	if m.swapFn == nil {
		return m, tea.Quit
	}
	newModel, initCmd := m.swapFn(msg)
	if newModel == nil {
		return m, tea.Quit
	}
	if tm, ok := newModel.(tuiModel); ok {
		if typed := m.view.ta.Value(); typed != "" {
			tm.ta.SetValue(typed)
		}
		tm.splashGone = tm.splashGone || m.view.splashGone
		newModel = tm
	}
	if m.view.ready {
		nm, _ := newModel.Update(tea.WindowSizeMsg{Width: m.view.width, Height: m.view.height})
		newModel = nm
	}
	return newModel, initCmd
}

func (m *loadingModel) View() string {
	return m.view.view()
}
