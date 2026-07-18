package tui

// Tests for the WP-9.1 two-zone layout: the right sidebar is removed, the
// conversation pane spans the full terminal width, and the on-demand info panel
// slots into the lower area without overflowing the terminal.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	agent "github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// keyMsg builds a tea.KeyMsg for a key name used in these tests.
func keyMsg(name string) tea.KeyMsg {
	switch name {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "ctrl+o":
		return tea.KeyMsg{Type: tea.KeyCtrlO}
	case "f2":
		return tea.KeyMsg{Type: tea.KeyF2}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(name)}
}

// twoZoneModel builds a ready tuiModel at the given geometry for layout tests.
func twoZoneModel(w, h int) tuiModel {
	m := layoutModel(w, h)
	m.app = &agent.App{Cfg: config.DefaultConfig(), Client: &proxy.Client{}}
	m.ready = true
	m.infoPanel.active = false
	return m
}

// maxLineWidth returns the widest ANSI-stripped line in s (display width).
func maxLineWidth(s string) int {
	max := 0
	for _, ln := range strings.Split(s, "\n") {
		if w := lipgloss.Width(ansi.Strip(ln)); w > max {
			max = w
		}
	}
	return max
}

// TestTwoZoneConversationFullWidth asserts the conversation pane's inner width
// is the full terminal width minus its border (no sidebar subtraction).
func TestTwoZoneConversationFullWidth(t *testing.T) {
	const w = 120
	m := twoZoneModel(w, 40)
	vpW, _, _ := m.sizes()
	if want := w - borderW; vpW != want {
		t.Errorf("vpW = %d, want %d (full width, no sidebar)", vpW, want)
	}
}

// TestTwoZoneNarrowTerminalWidthFloor asserts vpW never goes below 1 on a
// degenerate narrow terminal (m.width <= borderW).
func TestTwoZoneNarrowTerminalWidthFloor(t *testing.T) {
	m := twoZoneModel(2, 40) // width == borderW
	vpW, _, _ := m.sizes()
	if vpW < 1 {
		t.Errorf("vpW = %d, want >= 1 on degenerate narrow terminal", vpW)
	}
}

// TestTwoZonePanelHiddenByDefault asserts the info panel reserves no height
// and renders no section when closed.
func TestTwoZonePanelHiddenByDefault(t *testing.T) {
	m := twoZoneModel(120, 40)
	if got := m.infoPanelHeight(); got != 0 {
		t.Errorf("infoPanelHeight() = %d when closed, want 0", got)
	}
	if strings.Contains(m.View(), "grounded on") {
		t.Error("closed panel should not render info content")
	}
}

// TestTwoZonePanelToggleReservesHeight asserts opening the panel reserves
// exactly infoPanelCapH rows and shrinks the conversation viewport by the same.
func TestTwoZonePanelToggleReservesHeight(t *testing.T) {
	m := twoZoneModel(120, 40)
	_, vpHClosed, _ := m.sizes()

	m.infoPanel.active = true
	if got := m.infoPanelHeight(); got != infoPanelCapH {
		t.Errorf("infoPanelHeight() = %d when open, want %d", got, infoPanelCapH)
	}
	_, vpHOpen, _ := m.sizes()
	// Open panel cedes exactly infoPanelCapH rows of conversation height.
	if vpHOpen != vpHClosed-infoPanelCapH {
		t.Errorf("vpH open = %d, want %d (closed %d - panel %d)", vpHOpen, vpHClosed-infoPanelCapH, vpHClosed, infoPanelCapH)
	}
}

// TestTwoZoneViewFitsBounds asserts the rendered View stays within terminal
// height and per-line width in both panel states and across widths.
func TestTwoZoneViewFitsBounds(t *testing.T) {
	for _, w := range []int{200, 120, 80, 50} {
		for _, open := range []bool{false, true} {
			m := twoZoneModel(w, 40)
			m.infoPanel.active = open
			v := m.View()
			if got := lipgloss.Height(v); got > 40 {
				t.Errorf("w=%d open=%v: View height %d > 40", w, open, got)
			}
			if got := maxLineWidth(v); got > w {
				t.Errorf("w=%d open=%v: max line width %d > %d", w, open, got, w)
			}
		}
	}
}

// TestTwoZonePanelRendersContent asserts the open panel renders former-sidebar
// fields (model, proxy, exec) at full width.
func TestTwoZonePanelRendersContent(t *testing.T) {
	m := twoZoneModel(120, 40)
	m.infoPanel.active = true
	lines := m.renderInfoPanel()
	if len(lines) != infoPanelCapH {
		t.Errorf("renderInfoPanel returned %d lines, want exactly %d (fixed height)", len(lines), infoPanelCapH)
	}
	joined := ansi.Strip(strings.Join(lines, "\n"))
	if !strings.Contains(joined, "model") {
		t.Errorf("panel should render the model field:\n%s", joined)
	}
	if !strings.Contains(joined, "proxy") {
		t.Errorf("panel should render the proxy field:\n%s", joined)
	}
}

// TestTwoZoneShortTerminalSuppressesPanel asserts that on a terminal too short
// to fit the panel + min conversation height, sizes() reserves 0 panel rows AND
// View() does not render the panel — they stay in agreement (no overflow).
func TestTwoZoneShortTerminalSuppressesPanel(t *testing.T) {
	// Height too small for: input + status + panel(10) + minTopOuterH.
	m := twoZoneModel(120, 16)
	m.infoPanel.active = true
	if m.infoPanelVisible() {
		t.Fatal("panel should be suppressed on a short terminal")
	}
	if got := m.infoPanelHeight(); got != 0 {
		t.Errorf("infoPanelHeight() = %d on short terminal, want 0 (suppressed)", got)
	}
	v := m.View()
	if got := lipgloss.Height(v); got > 16 {
		t.Errorf("View height %d exceeds terminal 16 (panel rendered despite suppression)", got)
	}
}

// TestTwoZoneSizesAndViewAgreeOnPanel asserts sizes() and View() use the same
// visibility predicate across a height sweep.
func TestTwoZoneSizesAndViewAgreeOnPanel(t *testing.T) {
	for h := 12; h <= 44; h += 2 {
		m := twoZoneModel(120, h)
		m.infoPanel.active = true
		reserved := m.infoPanelHeight()
		rendered := strings.Contains(m.View(), "proxy") || strings.Contains(m.View(), "grounded on")
		if (reserved > 0) != rendered {
			t.Errorf("h=%d: sizes() reserved %d rows but View rendered=%v — disagreement", h, reserved, rendered)
		}
		if got := lipgloss.Height(m.View()); got > h {
			t.Errorf("h=%d: View height %d exceeds terminal", h, got)
		}
	}
}

// TestTwoZoneInfoSlashCommand asserts /info toggles the panel TUI-locally.
func TestTwoZoneInfoSlashCommand(t *testing.T) {
	m := newTabModel()
	m.infoPanel.active = false
	m.ta.SetValue("/info")
	m2, _, consumed := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !consumed {
		t.Fatal("/info enter should be consumed")
	}
	if !m2.infoPanel.active {
		t.Error("/info should open the info panel")
	}
}

// TestTwoZonePanelPersistsToRepoState asserts toggling writes InfoPanelOpen to
// the app (the persistence entry point).
func TestTwoZonePanelPersistsToRepoState(t *testing.T) {
	m := newTabModel()
	m.infoPanel.active = false
	m = m.toggleInfoPanel()
	if !m.app.InfoPanelOpen {
		t.Error("toggleInfoPanel should set app.InfoPanelOpen = true")
	}
	m = m.toggleInfoPanel()
	if m.app.InfoPanelOpen {
		t.Error("second toggleInfoPanel should set app.InfoPanelOpen = false")
	}
}

// TestTwoZoneRestoresPanelState asserts NewTUIModel seeds the panel from app state.
func TestTwoZoneRestoresPanelState(t *testing.T) {
	m := newTabModel()
	m.app.InfoPanelOpen = true
	m2 := NewTUIModel(m.app)
	if !m2.infoPanel.active {
		t.Error("NewTUIModel should restore infoPanel.active from app.InfoPanelOpen")
	}
}
func TestTwoZoneEscClosesPanel(t *testing.T) {
	m := newTabModel() // fully-initialized model (reflow walks items/builders)
	m.infoPanel.active = true
	m2, _, consumed := m.handleKey(keyMsg("esc"))
	if !consumed {
		t.Fatal("esc with open panel should be consumed")
	}
	if m2.infoPanel.active {
		t.Error("esc should close the info panel")
	}
}

// TestTwoZoneToggleKey asserts ctrl+o and f2 toggle the panel.
func TestTwoZoneToggleKey(t *testing.T) {
	for _, key := range []string{"ctrl+o", "f2"} {
		m := newTabModel()
		m.infoPanel.active = false
		m2, _, consumed := m.handleKey(keyMsg(key))
		if !consumed {
			t.Fatalf("%s should be consumed", key)
		}
		if !m2.infoPanel.active {
			t.Errorf("%s should open the info panel", key)
		}
		// Toggle back off.
		m3, _, _ := m2.handleKey(keyMsg(key))
		if m3.infoPanel.active {
			t.Errorf("%s (second press) should close the info panel", key)
		}
	}
}
