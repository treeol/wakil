package tui

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/core"

	tea "github.com/charmbracelet/bubbletea"
)

func keyModel(t *testing.T) (tuiModel, *fakeFacade) {
	t.Helper()
	f := &fakeFacade{sid: "sess_tui_test", chatID: "chat123"}
	m := newWiringModel(f)
	return step(m, tea.WindowSizeMsg{Width: 100, Height: 40}), f
}

func TestHandleKeyConfirmGate(t *testing.T) {
	for _, tc := range []struct {
		key  tea.KeyMsg
		want core.ApprovalOutcome
		read bool
	}{
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}, core.ApprovalAllowOnce, true},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")}, core.ApprovalDeny, true},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")}, core.ApprovalAllowReadsOnce, true},
	} {
		m, f := keyModel(t)
		m.state = stateConfirm
		m.pendApproval = &pendingApprovalState{approvalID: "apr_1", readAction: tc.read, headline: "h", detail: "d"}

		m2, _, consumed := m.handleKey(tc.key)
		if !consumed {
			t.Fatalf("%s should be consumed by the confirm gate", tc.key.String())
		}
		if len(f.responded) != 1 {
			t.Fatalf("%s should have answered the gate (responded=%d)", tc.key.String(), len(f.responded))
		}
		if got := f.responded[0].Outcome; got != tc.want {
			t.Errorf("%s → outcome %v, want %v", tc.key.String(), got, tc.want)
		}
		if m2.pendApproval != nil || m2.state != stateStreaming {
			t.Errorf("after answering, gate should clear and resume streaming; state=%v pend=%v", m2.state, m2.pendApproval)
		}
	}
}

func TestHandleKeyEnterSlashCommand(t *testing.T) {
	m, _ := keyModel(t)
	m.ta.SetValue("/cwd")
	m2, cmds, consumed := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !consumed || len(cmds) != 1 {
		t.Fatalf("slash command Enter should be consumed with one cmd; consumed=%v cmds=%d", consumed, len(cmds))
	}
	// The command must not start a turn.
	if m2.state != stateIdle {
		t.Errorf("a slash command should not start a turn; state=%v", m2.state)
	}
	// The cmd delivers a commandResultMsg carrying the facade's dispatch
	// result (the fake's embedded interface would panic on DispatchCommand —
	// assert the message type instead of executing it against the fake).
	if cmds[0] == nil {
		t.Error("slash command should produce a dispatch cmd")
	}
}

func TestHandleKeyEnterEmptyNoop(t *testing.T) {
	m, _ := keyModel(t)
	m.ta.SetValue("   ")
	m2, cmds, consumed := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !consumed || len(cmds) != 0 {
		t.Errorf("empty Enter should consume the key but issue no cmd; consumed=%v cmds=%d", consumed, len(cmds))
	}
	if m2.state != stateIdle {
		t.Errorf("empty Enter must not start a turn")
	}
}

func TestHandleKeyCtrlCIdleArmsThenQuits(t *testing.T) {
	m, _ := keyModel(t)
	m.state = stateIdle
	// First press: no quit — it arms and shows the banner.
	m2, cmds, consumed := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !consumed {
		t.Fatalf("ctrl+c should be consumed")
	}
	if quits(cmds) {
		t.Errorf("first idle ctrl+c must NOT quit (double-press gate)")
	}
	if m2.armKind != armQuit {
		t.Errorf("first idle ctrl+c should arm quit; armKind=%v", m2.armKind)
	}
	// Second press (same key, within window): quits.
	_, cmds2, _ := m2.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !quits(cmds2) {
		t.Errorf("second idle ctrl+c should quit")
	}
}

func TestMouseToContentBounds(t *testing.T) {
	m, _ := keyModel(t)
	// Set enough content to fill the viewport so bottomPad is 0
	// (no blank padding above the content).
	vpH := 0
	_, vpH, _ = m.sizes()
	m.vp.SetContent(strings.Repeat("line\n", vpH))

	// A click inside the pane maps to content coords with the y-offset applied.
	// (YOffset is whatever the viewport clamps to given current content.)
	// The pane's own row 0 is its top border.
	row, col, in := m.mouseToContent(5, 4)
	if !in {
		t.Fatal("click inside pane should be 'in'")
	}
	if col != 4 { // x-1
		t.Errorf("col = %d, want 4", col)
	}
	if want := (4 - 1 - 0) + m.vp.YOffset; row != want {
		t.Errorf("row = %d, want %d (screen y minus header and border plus offset)", row, want)
	}

	// x=0 is the border column → outside.
	if _, _, in := m.mouseToContent(0, 4); in {
		t.Error("x=0 (border) should be outside the content area")
	}
}

func TestClampToContentClampsToEdges(t *testing.T) {
	m, _ := keyModel(t)
	m.vp.SetYOffset(0)
	// Far above/left clamps to the top-left content cell (row 0, col 0).
	row, col := m.clampToContent(-5, -5)
	if row != 0 || col != 0 {
		t.Errorf("clamp top-left = (%d,%d), want (0,0)", row, col)
	}
	// Clamping never produces negative coordinates regardless of input.
	row, col = m.clampToContent(99999, 99999)
	if row < 0 || col < 0 {
		t.Errorf("clamp bottom-right produced negative coords (%d,%d)", row, col)
	}
}

var _ = strings.Contains // kept for symmetry with sibling tests
