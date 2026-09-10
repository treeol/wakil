package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// spinTickMsg drives the loading animation.
type spinTickMsg struct{}

func spinTick() tea.Cmd {
	return tea.Tick(90*time.Millisecond, func(time.Time) tea.Msg {
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

// logoRows is the 6-row ASCII art for "wakīl" generated from figlet
// 'standard' font, with a macron bar (__) prepended above the 'i'.
// Every row is exactly logoWidth runes wide.
const logoWidth = 24

var logoRows = []string{
	"                   __   ", // macron over the i
	"               _    _ _ ", // row 0: tops of w a k i l
	"__      ____ _| | _(_) |", // row 1
	"\\ \\ /\\ / / _` | |/ / | |", // row 2
	" \\ V  V / (_| |   <| | |", // row 3
	"  \\_/\\_/ \\__,_|_|\\_\\_|_|", // row 4: baseline
}

// noiseGlyphs are the random characters used in the unrevealed columns.
var noiseGlyphs = []rune{
	'#', '%', '&', '+', '=', '*', '@', '^', '~',
	':', ';', '<', '>', '?', '0', '1', '2', '3',
	'4', '5', '6', '7', '8', '9', '\\', '/',
}

// loadingModel is a minimal Bubble Tea model shown during startup while the
// container and conversation are being set up. It renders the "wakīl" logo
// emerging from ASCII noise (left-to-right reveal), then a subtle color
// shimmer while the bootstrap runs.
type loadingModel struct {
	width    int
	height   int
	ready    bool
	frameIdx int
	status   string

	// bootstrapCmd is the tea.Cmd that runs the async startup work.
	bootstrapCmd tea.Cmd

	// swapFn is called when the bootstrap Cmd completes.
	swapFn func(tea.Msg) (tea.Model, tea.Cmd)
}

// NewLoadingModel creates a loading model that shows the wakīl logo
// emerging from noise while bootstrapCmd runs, then swaps to the real
// model via swapFn when it completes.
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
		m.frameIdx++
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
		if m.swapFn != nil {
			newModel, initCmd := m.swapFn(msg)
			if newModel != nil {
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
		return m, nil
	}
}

func (m *loadingModel) View() string {
	if !m.ready {
		return "starting wakil…\n"
	}

	// Phase 1: Reveal — sweep left-to-right over ~1.8s (20 frames at 90ms).
	// Columns left of fillCol show the real logo; columns right show noise.
	// This makes the logo "emerge" from left to right.
	revealFrames := 20
	revealed := m.frameIdx >= revealFrames

	var fillCol int
	if revealed {
		fillCol = logoWidth // fully revealed
	} else {
		fillCol = (m.frameIdx * logoWidth) / revealFrames
	}

	// After reveal, cycle accent color for a subtle shimmer (blue shades).
	accentColor := lipgloss.Color("39")
	if revealed {
		shimmer := []lipgloss.Color{"39", "38", "33", "38"}
		accentColor = shimmer[(m.frameIdx/4)%len(shimmer)]
	}

	accent := lipgloss.NewStyle().Foreground(accentColor).Bold(true)
	noiseStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("242"))
	status := lipgloss.NewStyle().Foreground(lipgloss.Color("252")).Render(m.status)
	hint := lipgloss.NewStyle().Foreground(lipgloss.Color("240")).Render("press q to abort")

	// Render the logo row by row.
	var logoLines []string
	for rowIdx, row := range logoRows {
		var b strings.Builder
		runes := []rune(row)
		for col, ch := range runes {
			if col < fillCol {
				// Revealed: show the real logo character.
				if ch != ' ' {
					b.WriteString(accent.Render(string(ch)))
				} else {
					b.WriteRune(' ')
				}
			} else {
				// Unrevealed: show noise (varies per row AND column).
				if ch != ' ' {
					noise := noiseGlyphs[(m.frameIdx*7+col*13+rowIdx*31)%len(noiseGlyphs)]
					b.WriteString(noiseStyle.Render(string(noise)))
				} else {
					b.WriteRune(' ')
				}
			}
		}
		logoLines = append(logoLines, b.String())
	}
	logo := strings.Join(logoLines, "\n")

	// Center horizontally.
	leftPad := (m.width - logoWidth) / 2
	if leftPad < 0 {
		leftPad = 0
	}
	pad := strings.Repeat(" ", leftPad)
	logoPadded := strings.Split(logo, "\n")
	for i, l := range logoPadded {
		logoPadded[i] = pad + l
	}
	logo = strings.Join(logoPadded, "\n")

	// Center vertically.
	numLogoRows := len(logoRows)
	topPad := (m.height - numLogoRows - 4) / 2
	if topPad < 0 {
		topPad = 0
	}

	lines := make([]string, 0, m.height)
	for i := 0; i < topPad; i++ {
		lines = append(lines, "")
	}
	lines = append(lines, logo)
	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("%s%s", pad, status))
	lines = append(lines, "")
	lines = append(lines, fmt.Sprintf("%s%s", pad, hint))
	for len(lines) < m.height {
		lines = append(lines, "")
	}
	return lipgloss.JoinVertical(lipgloss.Left, lines...)
}
