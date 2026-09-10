package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// spinTickMsg drives the runner animation.
type spinTickMsg struct{}

func spinTick() tea.Cmd {
	return tea.Tick(120*time.Millisecond, func(time.Time) tea.Msg {
		return spinTickMsg{}
	})
}

// loadingProgressMsg updates the status text shown by the loading model.
// The bootstrap Cmd sends it via SendLoadingProgress at each startup stage.
type loadingProgressMsg struct {
	text string
}

// SendLoadingProgress sends a progress update to the loading model. It uses
// programSend (installed by SetProgramSend before prog.Run starts) so the
// bootstrap goroutine can update the status text in real time.
// Safe to call from any goroutine; a no-op when programSend is nil (tests).
func SendLoadingProgress(text string) {
	if programSend != nil {
		programSend(loadingProgressMsg{text: text})
	}
}

// BootstrapDone is the interface a bootstrap-completion message must
// implement. The loading model checks for this interface rather than
// treating every unhandled message as completion — mouse events, focus
// changes, etc. are silently ignored.
//
// The marker method is exported (IsBootstrapDone) so types in other
// packages (e.g. cmd/wakil) can implement it. An unexported method would
// only be satisfiable within this package.
type BootstrapDone interface {
	tea.Msg
	IsBootstrapDone()
}

// runnerFrames is a 6-frame ASCII stick-figure running animation. Each
// frame is a multi-line string (the figure is 3 lines tall). The legs and
// arms swap positions to create a running gait.
//
// Frame 0 is a relaxed standing pose used as the initial/resting frame.
var runnerFrames = []string{
	// 0: standing
	"    o\n   /|\\\n   / \\\n",
	// 1: stride 1 — left arm forward, right leg forward
	"    o\n  / | \\\n  /  \\\n",
	// 2: stride 2 — both up mid-stride
	"    o\n  /|\\\n  / \\\n",
	// 3: stride 3 — right arm forward, left leg forward
	"    o\n   | \\\n  \\  /\n",
	// 4: stride 4 — mid-air
	"    o\n  /|\\\n   | |\n",
	// 5: stride 5 — landing
	"    o\n   /|\\\n  / \\\n",
}

// runnerColors cycle through per-frame colors to give the figure a subtle
// "energy" shimmer while running.
var runnerColors = []lipgloss.Color{
	"243", "245", "247", "249", "247", "245",
}

// loadingModel is a minimal Bubble Tea model shown during startup while the
// container and conversation are being set up. It renders an ASCII stick
// figure running in place with a status line, handles resize and quit, and
// runs a bootstrap tea.Cmd. When the bootstrap Cmd returns a BootstrapDone
// message, Update calls the swap function to replace itself with the real
// TUI model.
type loadingModel struct {
	width   int
	height  int
	ready   bool
	frameIdx int
	status  string

	// bootstrapCmd is the tea.Cmd that runs the async startup work. Its
	// returned tea.Msg must implement BootstrapDone.
	bootstrapCmd tea.Cmd

	// swapFn is called when the bootstrap Cmd completes. It receives the
	// BootstrapDone message and returns the replacement model + its Init
	// Cmd. If the bootstrap failed, swapFn may return nil model + tea.Quit.
	swapFn func(tea.Msg) (tea.Model, tea.Cmd)
}

// NewLoadingModel creates a loading model that shows a running stick figure
// while bootstrapCmd runs, then swaps to the real model via swapFn when it
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
		m.frameIdx = (m.frameIdx + 1) % len(runnerFrames)
		return m, spinTick()

	case loadingProgressMsg:
		m.status = msg.text
		return m, nil

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

	// Render the runner frame in its cycling color.
	color := runnerColors[m.frameIdx%len(runnerColors)]
	runnerStyle := lipgloss.NewStyle().Foreground(color)
	frame := runnerFrames[m.frameIdx%len(runnerFrames)]
	runnerLines := strings.Split(strings.TrimRight(frame, "\n"), "\n")
	for i, l := range runnerLines {
		runnerLines[i] = runnerStyle.Render(l)
	}
	runner := strings.Join(runnerLines, "\n")

	status := lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Render(m.status)
	hint := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("press q to abort")

	// Center the runner horizontally (indent by ~8 cols to center in the
	// left half, then the status flows to its right).
	indent := "        " // 8 spaces
	statusLine := fmt.Sprintf("%s  %s", indent, status)
	hintLine := fmt.Sprintf("%s  %s", indent, hint)

	lines := []string{"", "", ""}
	lines = append(lines, indent+runner)
	lines = append(lines, "")
	lines = append(lines, statusLine)
	lines = append(lines, "")
	lines = append(lines, hintLine)
	for len(lines) < m.height {
		lines = append(lines, "")
	}
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}
