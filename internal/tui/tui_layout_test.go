package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/bubbles/textarea"
	agent "wakil/internal/agent"
	"wakil/internal/config"
)

// layoutModel builds a tuiModel with just enough state for sizes() to run.
func layoutModel(w, h int) tuiModel {
	ta := textarea.New()
	ta.SetHeight(3)
	return tuiModel{ta: ta, width: w, height: h}
}

// heightInvariant checks that viewport + border(2) + completionHeight + inputOuter
// equals the terminal height exactly. It is the sum that Bubble Tea renders
// into an AltScreen, so any mismatch drifts the cursor tracker.
func heightInvariant(m tuiModel) (total, want int) {
	_, _, inputOuterH := m.sizes()
	tabH := 0
	if len(m.subTabs) > 0 {
		tabH = 1
	}
	total = m.vp.Height + 2 + m.completionHeight() + tabH + inputOuterH
	return total, m.height
}

// TestLayoutFillsHeightNoGap asserts the conversation pane and the input box
// stack to exactly the terminal height with no blank gap. The activity dot is
// always present so the status row is always reserved regardless of state.
func TestLayoutFillsHeightNoGap(t *testing.T) {
	const w, h = 120, 40

	idle := layoutModel(w, h)
	_, vpH, inputOuterH := idle.sizes()
	topOuterH := vpH + 2 // conv pane outer = inner + border
	if topOuterH+inputOuterH != h {
		t.Fatalf("idle layout leaves a gap: top=%d input=%d sum=%d want %d",
			topOuterH, inputOuterH, topOuterH+inputOuterH, h)
	}
	// The activity dot is always present → status row always reserved:
	// border (2) + textarea (3) + status (1).
	if inputOuterH != idle.ta.Height()+2+1 {
		t.Fatalf("idle input height = %d, want %d (dot always visible)", inputOuterH, idle.ta.Height()+3)
	}

	streaming := layoutModel(w, h)
	streaming.state = stateStreaming
	_, vpH2, inputOuterH2 := streaming.sizes()
	if (vpH2+2)+inputOuterH2 != h {
		t.Fatalf("streaming layout leaves a gap: sum=%d want %d", (vpH2+2)+inputOuterH2, h)
	}
	// Idle and streaming both have the same status-row height (dot always shown).
	if inputOuterH2 != inputOuterH {
		t.Fatalf("idle and streaming input heights should be equal (dot always shown): idle=%d streaming=%d",
			inputOuterH, inputOuterH2)
	}
}

// TestLayoutCompletionPickerHeightInvariant checks that opening and closing the
// "@" completion picker keeps the rendered height equal to the terminal height.
// This is the invariant that was broken before the reflow fix: computeCompletion
// set m.comp.active without calling reflow(), leaving m.vp.Height stale and
// causing the viewport to overflow the AltScreen by completionHeight() rows.
func TestLayoutCompletionPickerHeightInvariant(t *testing.T) {
	const w, h = 120, 40
	base := compTree(t) // temp dir with a handful of files

	app := &agent.App{Cfg: config.DefaultConfig(), Client: newTestClient(""), Exec: newFakeExecutor()}
	app.Cfg.MentionBase = base
	m := NewTUIModel(app)
	m = step(m, tea.WindowSizeMsg{Width: w, Height: h})

	vpHBefore := m.vp.Height
	if total, want := heightInvariant(m); total != want {
		t.Fatalf("pre-completion invariant broken: rendered=%d terminal=%d", total, want)
	}

	// Type "@" — computeCompletion should open the picker and reflow should fire.
	m = step(m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'@'}})

	if !m.comp.active {
		t.Fatal("picker should be active after typing @")
	}
	compH := m.completionHeight()
	if compH == 0 {
		t.Fatal("completionHeight() must be > 0 with an active picker")
	}

	// Viewport must have shrunk by exactly completionHeight() rows.
	if got, want := m.vp.Height, vpHBefore-compH; got != want {
		t.Errorf("vp.Height with picker open = %d, want %d (shrunk by compH=%d)", got, want, compH)
	}

	// Total rendered height must still equal the terminal height.
	if total, want := heightInvariant(m); total != want {
		t.Errorf("picker-open invariant broken: rendered=%d terminal=%d (overflow=%d)", total, want, total-want)
	}

	// Dismiss with Esc — reflow should fire again and viewport should be restored.
	m = step(m, tea.KeyMsg{Type: tea.KeyEsc})

	if m.comp.active {
		t.Fatal("picker should be inactive after Esc")
	}
	if m.vp.Height != vpHBefore {
		t.Errorf("vp.Height after dismiss = %d, want %d (not restored)", m.vp.Height, vpHBefore)
	}
	if total, want := heightInvariant(m); total != want {
		t.Errorf("post-dismiss invariant broken: rendered=%d terminal=%d", total, want)
	}
}
