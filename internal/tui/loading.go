package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// loadingProgressMsg updates the status text shown by the loading model.
type loadingProgressMsg struct {
	text string
}

// SendLoadingProgress sends a progress update to the loading model.
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

// loadingModel is shown during startup while the container and conversation
// are being set up. It renders a centered bordered box with the "wakīl"
// wordmark, a spinner, live status text, and a progress bar that fills as
// bootstrap stages complete.
type loadingModel struct {
	width  int
	height int
	ready  bool
	sp     spinner.Model
	status string

	// stage tracks how many SendLoadingProgress calls have arrived, used
	// to advance the progress bar.
	stage int

	// totalStages is the expected number of bootstrap stages (for the
	// progress bar). 4: container/executor → session → subscribe → done.
	totalStages int

	// bootstrapCmd is the tea.Cmd that runs the async startup work.
	bootstrapCmd tea.Cmd

	// swapFn is called when the bootstrap Cmd completes.
	swapFn func(tea.Msg) (tea.Model, tea.Cmd)
}

// NewLoadingModel creates a loading model with a spinner and progress bar.
func NewLoadingModel(status string, bootstrapCmd tea.Cmd, swapFn func(tea.Msg) (tea.Model, tea.Cmd)) tea.Model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	return &loadingModel{
		status:       status,
		sp:           sp,
		totalStages:  4,
		bootstrapCmd: bootstrapCmd,
		swapFn:       swapFn,
	}
}

func (m *loadingModel) Init() tea.Cmd {
	return tea.Batch(m.sp.Tick, m.bootstrapCmd)
}

func (m *loadingModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.sp, cmd = m.sp.Update(msg)
		return m, cmd

	case loadingProgressMsg:
		m.status = msg.text
		m.stage++
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

// progressBar renders a horizontal progress bar using block characters.
// filled is 0..total. Width is the bar width in cells.
func progressBar(filled, total, width int) string {
	if total <= 0 {
		total = 1
	}
	frac := float64(filled) / float64(total)
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filledCells := int(frac * float64(width))
	bar := strings.Repeat("█", filledCells)
	track := strings.Repeat("░", width-filledCells)
	return bar + track
}

func (m *loadingModel) View() string {
	if !m.ready {
		return "starting wakil…\n"
	}

	// Color palette.
	accent := lipgloss.Color("39")    // bright blue
	label := lipgloss.Color("252")    // light gray
	dimmed := lipgloss.Color("240")  // dim gray
	border := lipgloss.Color("238")   // dark border

	// Wordmark.
	title := lipgloss.NewStyle().
		Foreground(accent).
		Render("wakīl")

	// Spinner + status.
	spinnerText := fmt.Sprintf("%s %s",
		m.sp.View(),
		lipgloss.NewStyle().Foreground(label).Render(m.status),
	)

	// Progress bar.
	barW := 30
	bar := lipgloss.NewStyle().
		Foreground(accent).
		Render(progressBar(m.stage, m.totalStages, barW))

	// Hint.
	hint := lipgloss.NewStyle().Foreground(dimmed).Render("press q to abort")

	// Assemble the content inside a bordered box.
	content := lipgloss.JoinVertical(lipgloss.Center,
		title,
		"",
		spinnerText,
		"",
		bar,
		"",
		hint,
	)

	// Bordered box.
	box := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(border).
		Padding(1, 2).
		Render(content)

	// Center in the terminal.
	return lipgloss.Place(m.width, m.height,
		lipgloss.Center, lipgloss.Center,
		box,
	)
}
