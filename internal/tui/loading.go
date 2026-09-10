package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// spinnerFrames are the braille spinner animation frames.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// spinTickMsg drives the spinner animation.
type spinTickMsg struct{}

func spinTick() tea.Cmd {
	return tea.Tick(80*time.Millisecond, func(time.Time) tea.Msg {
		return spinTickMsg{}
	})
}

// BootstrapDone is the interface a bootstrap-completion message must
// implement. The loading model checks for this interface rather than
// treating every unhandled message as completion — mouse events, focus
// changes, etc. are silently ignored.
type BootstrapDone interface {
	tea.Msg
	bootstrapDoneMarker()
}

// loadingModel is a minimal Bubble Tea model shown during startup while the
// container and conversation are being set up. It renders a spinner and
// status text, handles resize and quit, and runs a bootstrap tea.Cmd. When
// the bootstrap Cmd returns a BootstrapDone message, Update calls the swap
// function to replace itself with the real TUI model.
type loadingModel struct {
	width   int
	height  int
	ready   bool
	spinIdx int
	status  string

	// bootstrapCmd is the tea.Cmd that runs the async startup work. Its
	// returned tea.Msg must implement BootstrapDone.
	bootstrapCmd tea.Cmd

	// swapFn is called when the bootstrap Cmd completes. It receives the
	// BootstrapDone message and returns the replacement model + its Init
	// Cmd. If the bootstrap failed, swapFn may return nil model + tea.Quit.
	swapFn func(tea.Msg) (tea.Model, tea.Cmd)
}

// NewLoadingModel creates a loading model that shows a spinner while
// bootstrapCmd runs, then swaps to the real model via swapFn when it
// completes.
func NewLoadingModel(status string, bootstrapCmd tea.Cmd, swapFn func(tea.Msg) (tea.Model, tea.Cmd)) tea.Model {
	return &loadingModel{
		status:       status,
		bootstrapCmd: bootstrapCmd,
		swapFn:       swapFn,
	}
}

func (m *loadingModel) Init() tea.Cmd {
	return tea.Batch(spinTick(), m.bootstrapCmd)
}

func (m *loadingModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		return m, nil

	case spinTickMsg:
		m.spinIdx = (m.spinIdx + 1) % len(spinnerFrames)
		return m, spinTick()

	case tea.KeyMsg:
		if msg.String() == "q" || msg.String() == "ctrl+c" {
			return m, tea.Quit
		}
		return m, nil

	case BootstrapDone:
		// Bootstrap completion — call the swap function.
		if m.swapFn != nil {
			newModel, initCmd := m.swapFn(msg)
			if newModel != nil {
				// Replay the window size so the new model has correct
				// dimensions immediately (Bubble Tea only sends
				// WindowSizeMsg at startup and on SIGWINCH).
				if m.ready {
					nm, _ := newModel.Update(tea.WindowSizeMsg{
						Width:  m.width,
						Height: m.height,
					})
					newModel = nm
				}
				if initCmd != nil {
					return newModel, initCmd
				}
				return newModel, nil
			}
		}
		return m, tea.Quit

	default:
		// Ignore mouse events, focus changes, and any other messages
		// that arrive during loading.
		return m, nil
	}
}

func (m *loadingModel) View() string {
	if !m.ready {
		return "starting wakil…\n"
	}
	spin := spinnerFrames[m.spinIdx%len(spinnerFrames)]
	label := lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Render(spin)
	status := lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Render(m.status)
	hint := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("press q to abort")

	lines := []string{"", "", ""}
	lines = append(lines, fmt.Sprintf("  %s  %s", label, status))
	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("  %s", hint))
	for len(lines) < m.height {
		lines = append(lines, "")
	}
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}
