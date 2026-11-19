package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
)

type bootDone struct{}

func (bootDone) IsBootstrapDone() {}

func newBootModel(t *testing.T, w, h int) (*loadingModel, *int) {
	t.Helper()
	swaps := new(int)
	m := NewLoadingModel("starting container…", nil, func(tea.Msg) (tea.Model, tea.Cmd) {
		*swaps++
		real := NewTUIModelWithFacade(&fakeFacade{sid: "s", chatID: "c"}, nil,
			core.Principal{TenantID: event.EmbeddedTenantID, UserID: event.EmbeddedUserID, Role: core.RoleOwner})
		return real, nil
	}).(*loadingModel)
	nm, _ := m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	return nm.(*loadingModel), swaps
}

// The first frame is the normal view: the splash and the input box, plus one
// status line naming what is still starting. No box, no progress bar, no
// "press q" hint.
func TestStartupViewIsTheNormalView(t *testing.T) {
	m, _ := newBootModel(t, 120, 40)
	out := ansi.Strip(m.View())
	for _, want := range []string{"type a task", "· starting container…"} {
		if !strings.Contains(out, want) {
			t.Errorf("start-up view lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"press q", "█", "░", "initializing"} {
		if strings.Contains(out, bad) {
			t.Errorf("start-up view still has loading-screen element %q", bad)
		}
	}
	// Exactly the height of the terminal, like the normal view.
	if n := strings.Count(m.View(), "\n") + 1; n != 40 {
		t.Errorf("start-up view is %d rows, want 40", n)
	}
	// The status is drawn inside the conversation pane, so the start-up view
	// has the layout of the real model on a fresh start (no status row) and the
	// swap does not move the input box.
	if m.view.effectiveStatusRows() != 0 {
		t.Errorf("status rows = %d, want 0", m.view.effectiveStatusRows())
	}
}

func TestStartupViewShowsProgressUntilReady(t *testing.T) {
	m, _ := newBootModel(t, 120, 40)
	for _, s := range []string{"creating session…", "setting up…"} {
		nm, _ := m.Update(loadingProgressMsg{text: s})
		m = nm.(*loadingModel)
		out := ansi.Strip(m.View())
		if !strings.Contains(out, "· "+s) {
			t.Errorf("status %q not shown:\n%s", s, out)
		}
	}
}

// Fits at small sizes (compact mode drops chrome but the status stays).
func TestStartupViewFitsSmallSizes(t *testing.T) {
	for _, sz := range [][2]int{{80, 24}, {60, 18}, {50, 14}, {30, 10}} {
		m, _ := newBootModel(t, sz[0], sz[1])
		v := m.View()
		if n := strings.Count(v, "\n") + 1; n > sz[1] {
			t.Errorf("%dx%d: view is %d rows, exceeds the terminal", sz[0], sz[1], n)
		}
		for _, l := range strings.Split(v, "\n") {
			if w := ansi.StringWidth(l); w > sz[0] {
				t.Errorf("%dx%d: line is %d wide", sz[0], sz[1], w)
				break
			}
		}
	}
}

// Enter does nothing while starting; ctrl+c quits; typing is kept and moved
// into the real input at the swap.
func TestStartupKeysAndTypeAhead(t *testing.T) {
	m, swaps := newBootModel(t, 120, 40)
	for _, r := range "hello" {
		nm, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
		m = nm.(*loadingModel)
	}
	nm, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = nm.(*loadingModel)
	if cmd != nil {
		t.Fatalf("Enter while starting must do nothing, got a command")
	}
	if got := m.view.ta.Value(); got != "hello" {
		t.Fatalf("typed text = %q, want hello (Enter must not clear or send it)", got)
	}
	if !strings.Contains(ansi.Strip(m.View()), "hello") {
		t.Error("typed text not visible in the start-up view")
	}

	next, _ := m.Update(bootDone{})
	real, ok := next.(tuiModel)
	if !ok || *swaps != 1 {
		t.Fatalf("swap did not install the real model (ok=%v swaps=%d)", ok, *swaps)
	}
	if real.ta.Value() != "hello" {
		t.Errorf("real input = %q, want the type-ahead hello", real.ta.Value())
	}
	if !real.ready || real.width != 120 || real.height != 40 {
		t.Errorf("real model not sized at the swap: ready=%v %dx%d", real.ready, real.width, real.height)
	}
	if real.booting() {
		t.Error("the real model must not show the start-up status")
	}

	_, quit := m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if quit == nil {
		t.Fatal("ctrl+c must quit while starting")
	}
}

// A failed bootstrap (swapFn returns nil) quits so the terminal is restored.
func TestStartupSwapErrorQuits(t *testing.T) {
	m := NewLoadingModel("x", nil, func(tea.Msg) (tea.Model, tea.Cmd) { return nil, tea.Quit }).(*loadingModel)
	_, cmd := m.Update(bootDone{})
	if cmd == nil {
		t.Fatal("a failed bootstrap must quit")
	}
}
