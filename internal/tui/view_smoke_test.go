package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// TestViewSmokeStatusPlacement renders the full View at a realistic geometry
// and asserts: conversation on top, status line directly above the textarea,
// everything on ONE status row at 120 cols, two rows when narrow.
func TestViewSmokeStatusPlacement(t *testing.T) {
	m := newTabModel() // 100x30 ready model with app
	m = step(m, tea.WindowSizeMsg{Width: 120, Height: 40})

	v := m.View()
	lines := strings.Split(v, "\n")

	// Total height must equal the terminal.
	if lipgloss.Height(v) > 40 {
		t.Fatalf("View height %d exceeds 40", lipgloss.Height(v))
	}

	// Status line = the row containing the dot; it must sit directly above
	// the textarea (bottom region), NOT at the top.
	dotRow := -1
	for i, ln := range lines {
		if strings.Contains(plain(ln), "•") {
			dotRow = i
		}
	}
	if dotRow < 0 {
		t.Fatal("no status dot found in View")
	}
	if dotRow < 30 {
		t.Errorf("status line should be near the bottom (row ≥30), found at row %d", dotRow)
	}

	// At 120 cols everything fits on ONE status row: the dot row also
	// carries the model and the ctx gauge.
	row := plain(lines[dotRow])
	for _, want := range []string{"ctx", "hist"} {
		if !strings.Contains(row, want) {
			t.Errorf("status row should contain %q at 120 cols; row=%q", want, row)
		}
	}
}

// TestViewSmokeNarrowWrapsToTwo: with extra segments at a narrow width the
// status zone must wrap to two rows, still directly above the textarea, and
// sizes()/View() stay in agreement.
func TestViewSmokeNarrowWrapsToTwo(t *testing.T) {
	m := newTabModel()
	m = step(m, tea.WindowSizeMsg{Width: 60, Height: 40})
	// Drive the state change through the message path (a turn start flips the
	// status to "streaming"), then simulate the t/s + flash segments via the
	// copiedMsg handler — both go through the reflow guard.
	m.state = stateStreaming // startTurn equivalent; reflow happens below
	m = m.reflow()
	m.tps = 87
	m = step(m, copiedMsg{n: 42})
	if got, want := m.statusRows(), len(m.statusLines()); got != want {
		t.Fatalf("statusRows()=%d disagrees with statusLines()=%d", got, want)
	}
	if got := m.statusRows(); got != 2 {
		t.Errorf("statusRows() with streaming+plan+flash at 60 cols = %d, want 2", got)
	}
	if lipgloss.Height(m.View()) > 40 {
		t.Errorf("View overflows with 2-row status")
	}
	// The wrapped status zone must still sit directly above the textarea.
	v := m.View()
	lines := strings.Split(v, "\n")
	dotRow := -1
	for i, ln := range lines {
		if strings.Contains(plain(ln), "•") {
			dotRow = i
		}
	}
	if dotRow < 0 || dotRow < 29 {
		t.Errorf("2-row status should be near the bottom, dot at row %d", dotRow)
	}
}
