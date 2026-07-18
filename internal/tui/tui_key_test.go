package tui

import (
	"strings"
	"testing"

	agent "github.com/treeol/wakil/internal/agent"

	"github.com/treeol/wakil/internal/config"

	tea "github.com/charmbracelet/bubbletea"
)

func keyModel(t *testing.T) tuiModel {
	t.Helper()
	app := &agent.App{Cfg: config.DefaultConfig(), Client: newTestClient(""), Exec: newFakeExecutor()}
	m := NewTUIModel(app)
	return step(m, tea.WindowSizeMsg{Width: 100, Height: 40})
}

func TestHandleKeyConfirmGate(t *testing.T) {
	for _, tc := range []struct {
		key  tea.KeyMsg
		want agent.ConfirmChoice
		read bool
	}{
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")}, agent.ChoiceApprove, true},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")}, agent.ChoiceDecline, true},
		{tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")}, agent.ChoiceAllowReads, true},
	} {
		m := keyModel(t)
		ch := make(chan agent.ConfirmChoice, 1)
		m.state = stateConfirm
		m.pendConf = &agent.ConfirmReqMsg{RespCh: ch, ReadAction: tc.read, Headline: "h", Detail: "d"}

		m2, _, consumed := m.handleKey(tc.key)
		if !consumed {
			t.Fatalf("%s should be consumed by the confirm gate", tc.key.String())
		}
		select {
		case got := <-ch:
			if got != tc.want {
				t.Errorf("%s → choice %v, want %v", tc.key.String(), got, tc.want)
			}
		default:
			t.Fatalf("%s should have answered the gate", tc.key.String())
		}
		if m2.pendConf != nil || m2.state != stateStreaming {
			t.Errorf("after answering, gate should clear and resume streaming; state=%v pend=%v", m2.state, m2.pendConf)
		}
	}
}

func TestHandleKeyEnterSlashCommand(t *testing.T) {
	m := keyModel(t)
	m.ta.SetValue("/cwd")
	m2, cmds, consumed := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !consumed || len(cmds) != 1 {
		t.Fatalf("slash command Enter should be consumed with one cmd; consumed=%v cmds=%d", consumed, len(cmds))
	}
	// The command must not start an agent turn.
	if m2.state != stateIdle {
		t.Errorf("a slash command should not start a turn; state=%v", m2.state)
	}
	if msg, ok := cmds[0]().(agent.SysNoteMsg); !ok || !strings.Contains(msg.Text, "/work") {
		t.Errorf("/cwd cmd should yield a cwd note; got %+v", msg)
	}
}

func TestHandleKeyEnterEmptyNoop(t *testing.T) {
	m := keyModel(t)
	m.ta.SetValue("   ")
	m2, cmds, consumed := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if !consumed || len(cmds) != 0 {
		t.Errorf("empty Enter should consume the key but issue no cmd; consumed=%v cmds=%d", consumed, len(cmds))
	}
	if m2.state != stateIdle {
		t.Errorf("empty Enter must not start a turn")
	}
}

func TestHandleKeyCtrlCIdleQuits(t *testing.T) {
	m := keyModel(t)
	m.state = stateIdle
	_, cmds, consumed := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !consumed || len(cmds) != 1 {
		t.Errorf("ctrl+c when idle should quit (one cmd); consumed=%v cmds=%d", consumed, len(cmds))
	}
}

func TestMouseToContentBounds(t *testing.T) {
	m := keyModel(t)
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
	m := keyModel(t)
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
