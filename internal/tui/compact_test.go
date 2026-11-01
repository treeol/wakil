package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/muesli/termenv"
)

// forceColors makes lipgloss emit colour escapes (the test profile has none).
func forceColors(t *testing.T) {
	t.Helper()
	old := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.ANSI256)
	t.Cleanup(func() { lipgloss.SetColorProfile(old) })
}

func forceTruecolor(t *testing.T) {
	t.Helper()
	old := splashTruecolor
	splashTruecolor = func() bool { return true }
	t.Cleanup(func() { splashTruecolor = old })
}

func sizedModel(w, h int) tuiModel {
	m := newWiringModel(&fakeFacade{sid: "sess_tui_test", chatID: "chat123"})
	return step(m, tea.WindowSizeMsg{Width: w, Height: h})
}

func TestCompactThresholds(t *testing.T) {
	for _, c := range []struct {
		w, h    int
		compact bool
	}{
		{120, 40, false},
		{compactMinW, compactMinH, false},
		{compactMinW - 1, 40, true},
		{120, compactMinH - 1, true},
		{40, 12, true},
	} {
		m := sizedModel(c.w, c.h)
		if got := m.compact(); got != c.compact {
			t.Errorf("%dx%d: compact = %v, want %v", c.w, c.h, got, c.compact)
		}
	}
}

// Compact: one-row textarea, no status line, nothing beyond the terminal.
func TestCompactLayout(t *testing.T) {
	m := sizedModel(50, 14) // hadTurn=true, so the status line would normally show
	if m.ta.Height() != inputRowsCompact {
		t.Errorf("textarea height = %d, want %d", m.ta.Height(), inputRowsCompact)
	}
	if m.effectiveStatusRows() != 0 || m.statusVisible() {
		t.Error("status line must be hidden in compact mode")
	}
	v := m.View()
	if strings.Contains(ansi.Strip(v), "ctx") || strings.Contains(ansi.Strip(v), "•") {
		t.Errorf("status content rendered in compact mode:\n%s", ansi.Strip(v))
	}
	if h := lipgloss.Height(v); h != 14 {
		t.Errorf("View height = %d, want 14 (sizes() and View() must agree)", h)
	}
	// Growing the window brings the normal layout back.
	m = step(m, tea.WindowSizeMsg{Width: 120, Height: 40})
	if m.ta.Height() != inputRowsNormal || !m.statusVisible() {
		t.Errorf("normal layout not restored: ta=%d statusVisible=%v", m.ta.Height(), m.statusVisible())
	}
}

// Reverse search and the rotation indicator exist only in the status zone, so
// compact mode keeps it for them.
func TestCompactKeepsStatusForSearchAndRotation(t *testing.T) {
	m := sizedModel(50, 14)
	m.rotating = true
	if !m.statusVisible() {
		t.Error("rotation indicator must stay visible in compact mode")
	}
	m.rotating = false
	m.searchActive = true
	if !m.statusVisible() {
		t.Error("reverse search must stay visible in compact mode")
	}
	m = m.reflow()
	if h := lipgloss.Height(m.View()); h != 14 {
		t.Errorf("View height = %d, want 14", h)
	}
}

func TestPromptBarRedWhenAuto(t *testing.T) {
	forceColors(t)
	f := &fakeFacade{sid: "sess_tui_test", chatID: "chat123"}
	m := newWiringModel(f)
	m = step(m, tea.WindowSizeMsg{Width: 120, Height: 40})

	const red, bar = "\x1b[38;5;196m", "┃"
	off := m.renderInputRows()
	if strings.Contains(off, red) {
		t.Error("prompt bar is red with AUTO off")
	}
	f.consent.AutoApprove = true
	on := m.renderInputRows()
	rows := strings.Split(on, "\n")
	if len(rows) != inputRowsNormal {
		t.Fatalf("rows = %d, want %d", len(rows), inputRowsNormal)
	}
	for i, r := range rows {
		if !strings.Contains(r, red) || !strings.HasPrefix(ansi.Strip(r), bar) {
			t.Errorf("row %d: want a red %q bar, got %q", i, bar, r)
		}
	}
	// Also in compact mode.
	m = step(m, tea.WindowSizeMsg{Width: 50, Height: 14})
	if got := m.renderInputRows(); !strings.Contains(got, red) || strings.Contains(got, "\n") {
		t.Errorf("compact AUTO bar: want one red row, got %q", got)
	}
}

func TestFeatherShownOnlyWhenRoom(t *testing.T) {
	forceTruecolor(t)
	for _, c := range []struct {
		w, h int
		want bool
	}{
		{160, 40, true},
		{featherMinW, 40, true},
		{featherMinW - 1, 40, false},
		{160, compactMinH - 1, false}, // compact wins
	} {
		m := sizedModel(c.w, c.h)
		if got := m.featherW() > 0; got != c.want {
			t.Errorf("%dx%d: feather shown = %v, want %v", c.w, c.h, got, c.want)
		}
	}
	// Without truecolor it never shows.
	splashTruecolor = func() bool { return false }
	if m := sizedModel(160, 40); m.featherW() != 0 {
		t.Error("feather shown without truecolor")
	}
}

// With the feather on, the input block is exactly m.width wide and as tall as
// without it; the feather sits at the right and has transparent corners.
func TestFeatherLayout(t *testing.T) {
	forceTruecolor(t)
	m := sizedModel(140, 40)
	in := m.renderInput()
	lines := strings.Split(in, "\n")
	if len(lines) != m.inputOuterBoxH() {
		t.Fatalf("input block height = %d, want %d", len(lines), m.inputOuterBoxH())
	}
	for i, l := range lines {
		if w := ansi.StringWidth(l); w != m.width {
			t.Errorf("line %d width = %d, want %d", i, w, m.width)
		}
	}
	if !strings.Contains(in, "\x1b[38;2;") {
		t.Error("feather not rendered")
	}
	if !strings.Contains(ansi.Strip(lines[1]), "┃") {
		t.Error("prompt bar missing from input box")
	}
	// Full View must fit the terminal.
	if h := lipgloss.Height(m.View()); h > 40 {
		t.Errorf("View height %d exceeds 40", h)
	}
}

// Every size from tiny to large renders within bounds, with and without the
// feather and in both layouts.
func TestViewFitsAcrossSizes(t *testing.T) {
	forceTruecolor(t)
	for _, w := range []int{20, 40, 59, 60, 99, 100, 140, 220} {
		for _, h := range []int{6, 12, 17, 18, 30, 60} {
			m := sizedModel(w, h)
			v := m.View()
			if got := lipgloss.Height(v); got > h {
				t.Errorf("%dx%d: View height %d exceeds terminal", w, h, got)
			}
			for i, l := range strings.Split(v, "\n") {
				if lw := ansi.StringWidth(l); lw > w {
					t.Errorf("%dx%d: line %d width %d exceeds terminal", w, h, i, lw)
					break
				}
			}
		}
	}
}
