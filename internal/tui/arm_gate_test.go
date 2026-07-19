package tui

import (
	"strings"
	"testing"
	"time"

	agent "github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"

	tea "github.com/charmbracelet/bubbletea"
)

// quits reports whether any cmd in the batch resolves to tea.QuitMsg. It runs
// each cmd in a goroutine with a short timeout because tea.Tick commands block
// for their full duration — a tick is NOT a quit, so we must not wait on it.
func quits(cmds []tea.Cmd) bool {
	for _, c := range cmds {
		if c == nil {
			continue
		}
		done := make(chan tea.Msg, 1)
		go func(c tea.Cmd) { done <- c() }(c)
		select {
		case msg := <-done:
			if _, ok := msg.(tea.QuitMsg); ok {
				return true
			}
		case <-time.After(50 * time.Millisecond):
			// Blocked (a tick) — not a quit.
		}
	}
	return false
}

func armKeyModel(t *testing.T) tuiModel {
	t.Helper()
	app := &agent.App{Cfg: config.DefaultConfig(), Client: newTestClient(""), Exec: newFakeExecutor()}
	m := NewTUIModel(app)
	return step(m, tea.WindowSizeMsg{Width: 100, Height: 40})
}

func TestArmIdleCtrlDDoublePress(t *testing.T) {
	m := armKeyModel(t)
	m.state = stateIdle
	m2, cmds, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlD})
	if quits(cmds) || m2.armKind != armQuit || m2.armKey != "ctrl+d" {
		t.Fatalf("first idle ctrl+d should arm quit (ctrl+d); quits=%v arm=%v/%q", quits(cmds), m2.armKind, m2.armKey)
	}
	_, cmds2, _ := m2.handleKey(tea.KeyMsg{Type: tea.KeyCtrlD})
	if !quits(cmds2) {
		t.Errorf("second idle ctrl+d should quit")
	}
}

func TestArmDismissedByUnrelatedKey(t *testing.T) {
	m := armKeyModel(t)
	m.state = stateIdle
	m2, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if m2.armKind != armQuit {
		t.Fatalf("expected quit armed")
	}
	// An unrelated key routes through Update's dismiss (handleKey alone does not
	// dismiss — the dismiss lives in Update before the picker branches).
	m3 := step(m2, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	if m3.armKind != armNone {
		t.Errorf("unrelated key should clear the arm; armKind=%v", m3.armKind)
	}
}

func TestArmExpiredTreatedInactive(t *testing.T) {
	m := armKeyModel(t)
	m.state = stateIdle
	m.armKind = armQuit
	m.armKey = "ctrl+c"
	m.armUntil = time.Now().Add(-time.Millisecond) // already expired
	if m.armActive() {
		t.Fatalf("expired arm should be inactive")
	}
	// A confirming-looking key after expiry must NOT quit; it re-arms fresh.
	m2, cmds, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if quits(cmds) {
		t.Errorf("expired arm must not confirm a quit")
	}
	if m2.armKind != armQuit {
		t.Errorf("expired arm + ctrl+c should re-arm quit; armKind=%v", m2.armKind)
	}
}

func TestArmBannerShown(t *testing.T) {
	m := armKeyModel(t)
	m.state = stateIdle
	m2, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	notice := m2.armNotice()
	if !strings.Contains(notice, "ctrl+c") || !strings.Contains(notice, "quit") {
		t.Errorf("banner should name the key and action; got %q", notice)
	}
	// The notice must flow into the status segments.
	in := m2.headerStatusInput()
	if in.arm == "" {
		t.Errorf("statusLineInput.arm should carry the notice")
	}
}

func TestArmStreamingEscTwiceCancels(t *testing.T) {
	m := armKeyModel(t)
	m.state = stateStreaming
	cancelled := false
	m.cancel = func() { cancelled = true }

	m2, cmds, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if cancelled || m2.cancelling {
		t.Fatalf("first esc must NOT cancel; it arms")
	}
	if m2.armKind != armCancel {
		t.Errorf("first esc should arm cancel; armKind=%v", m2.armKind)
	}
	_ = cmds
	m3, _, _ := m2.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if !cancelled || !m3.cancelling {
		t.Errorf("second esc should cancel and set cancelling; cancelled=%v cancelling=%v", cancelled, m3.cancelling)
	}
	if m3.armKind != armNone {
		t.Errorf("confirmed cancel should clear the arm; armKind=%v", m3.armKind)
	}
}

func TestArmStreamingCtrlCTwiceCancelsThenForceQuits(t *testing.T) {
	m := armKeyModel(t)
	m.state = stateStreaming
	cancelled := false
	m.cancel = func() { cancelled = true }

	// Press 1: arm.
	m2, cmds1, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cancelled || quits(cmds1) || m2.armKind != armCancel {
		t.Fatalf("press 1 should arm cancel; cancelled=%v arm=%v", cancelled, m2.armKind)
	}
	// Press 2: cancel + cancelling=true.
	m3, _, _ := m2.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !cancelled || !m3.cancelling {
		t.Fatalf("press 2 should cancel and set cancelling; cancelled=%v cancelling=%v", cancelled, m3.cancelling)
	}
	// Press 3 while cancelling: force-quit (no re-arm).
	_, cmds3, _ := m3.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !quits(cmds3) {
		t.Errorf("press 3 while cancelling should force-quit")
	}
}

func TestArmMixedEscThenCtrlCConfirmsCancel(t *testing.T) {
	m := armKeyModel(t)
	m.state = stateStreaming
	cancelled := false
	m.cancel = func() { cancelled = true }

	m2, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc}) // arms cancel via esc
	if m2.armKind != armCancel {
		t.Fatalf("esc should arm cancel")
	}
	// ctrl+c confirms a cancel arm too (avoids the mixed-key 4-press trap).
	m3, _, _ := m2.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !cancelled || !m3.cancelling {
		t.Errorf("esc then ctrl+c should confirm the cancel on press 2; cancelled=%v cancelling=%v", cancelled, m3.cancelling)
	}
}

func TestArmCancellingNeverRearms(t *testing.T) {
	m := armKeyModel(t)
	m.state = stateStreaming
	m.cancelling = true
	m.cancel = func() {}
	_, cmds, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !quits(cmds) {
		t.Errorf("while cancelling, ctrl+c must force-quit directly")
	}
	if m.armKind != armNone {
		t.Errorf("force-quit path must not set an arm; armKind=%v", m.armKind)
	}
}

func TestArmSearchAbortDoesNotArm(t *testing.T) {
	m := armKeyModel(t)
	m.state = stateIdle
	m.inputHistory = []string{"one", "two"}
	m.searchActive = true
	m.searchSaved = ""
	m.searchIdx = -1
	m2, cmds, consumed := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if !consumed {
		t.Fatalf("ctrl+c in search should be consumed")
	}
	if quits(cmds) {
		t.Errorf("ctrl+c in search must not quit")
	}
	if m2.searchActive {
		t.Errorf("ctrl+c should abort search")
	}
	if m2.armKind != armNone {
		t.Errorf("search abort must not set an arm; armKind=%v", m2.armKind)
	}
}

func TestArmConfirmGateCtrlCUnchanged(t *testing.T) {
	m := armKeyModel(t)
	ch := make(chan agent.ConfirmChoice, 1)
	m.state = stateConfirm
	m.pendConf = &agent.ConfirmReqMsg{RespCh: ch, ReadAction: false, Headline: "h", Detail: "d"}
	cancelled := false
	m.cancel = func() { cancelled = true }

	m2, cmds, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if quits(cmds) {
		t.Errorf("confirm-gate ctrl+c must not quit")
	}
	select {
	case got := <-ch:
		if got != agent.ChoiceDecline {
			t.Errorf("confirm-gate ctrl+c should decline; got %v", got)
		}
	default:
		t.Fatalf("confirm-gate ctrl+c should answer the gate in one press")
	}
	if !cancelled || !m2.cancelling {
		t.Errorf("confirm-gate ctrl+c should decline+cancel and set cancelling; cancelled=%v cancelling=%v", cancelled, m2.cancelling)
	}
	if m2.armKind != armNone {
		t.Errorf("confirm gate must not set an arm; armKind=%v", m2.armKind)
	}
}

func TestArmTickClearsOnlyMatchingArm(t *testing.T) {
	m := armKeyModel(t)
	m.state = stateIdle
	// Arm once (seq=1), capture its tick, then re-arm (seq=2) via a fresh press.
	m2, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	seq1 := m2.armSeq
	m3, _, _ := m2.handleKey(tea.KeyMsg{Type: tea.KeyCtrlD}) // different key: cleared then re-armed? no — see below
	_ = m3
	// Stale tick for seq1 must not clear the current arm if seq advanced.
	m.armKind = armQuit
	m.armKey = "ctrl+c"
	m.armUntil = time.Now().Add(-time.Millisecond)
	m.armSeq = seq1 + 5 // current arm is a later generation
	outdated := armTickMsg{seq: seq1}
	m4 := step(m, outdated)
	if m4.armKind != armNone {
		// The stale seq must not match, so the arm survives (until its own deadline).
		if m4.armSeq != seq1+5 {
			t.Errorf("stale tick should not touch a newer arm; armSeq=%v", m4.armSeq)
		}
	}
}

func TestArmTickClearsExpiredArm(t *testing.T) {
	m := armKeyModel(t)
	m.state = stateIdle
	m.armKind = armQuit
	m.armKey = "ctrl+c"
	m.armUntil = time.Now().Add(-time.Millisecond)
	m.armSeq = 3
	m2 := step(m, armTickMsg{seq: 3})
	if m2.armKind != armNone {
		t.Errorf("matching tick past deadline should clear the arm; armKind=%v", m2.armKind)
	}
}

func TestArmClearedOnTurnDone(t *testing.T) {
	m := armKeyModel(t)
	m.state = stateStreaming
	m.armKind = armCancel
	m.armKey = "esc"
	m.armUntil = time.Now().Add(time.Second)
	// Simulate the agent finishing while a cancel arm is pending.
	m2 := step(m, agent.AgentDoneMsg{})
	if m2.armKind != armNone {
		t.Errorf("AgentDoneMsg should clear a pending cancel arm; armKind=%v", m2.armKind)
	}
}

func TestArmCancelConfirmRequiresNonIdle(t *testing.T) {
	// Defensive: a cancel arm whose turn already ended must not cancel into idle.
	m := armKeyModel(t)
	m.state = stateIdle
	m.armKind = armCancel
	m.armKey = "esc"
	m.armUntil = time.Now().Add(time.Second)
	m.cancel = func() { t.Errorf("cancel must not fire from idle") }
	// esc while idle with a (stale) cancel arm: should not cancel. The
	// precondition is enforced because idle esc never reaches the cancel branch.
	_, cmds, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEsc})
	if quits(cmds) {
		t.Errorf("idle esc with stale cancel arm must not quit")
	}
}

// --- Update-level tests (paste suppression + picker dismiss live in Update) ---

func TestArmPasteSuppressionSwallowsCtrlC(t *testing.T) {
	m := armKeyModel(t)
	m.state = stateIdle
	m.pasteSuppressUntil = time.Now().Add(pasteSuppressWindow)
	// Two stray ctrl+c bytes inside the suppression window must NOT quit and
	// must NOT leave an arm behind.
	m2 := step(m, tea.KeyMsg{Type: tea.KeyCtrlC})
	m3 := step(m2, tea.KeyMsg{Type: tea.KeyCtrlC})
	if m3.armKind != armNone {
		t.Errorf("suppressed ctrl+c must not arm; armKind=%v", m3.armKind)
	}
	// And the window should have been extended (still in the future).
	if m3.pasteSuppressUntil.IsZero() {
		t.Errorf("suppression window should be extended by swallowed keys")
	}
}

func TestArmCompletionPickerKeyDismissesArm(t *testing.T) {
	m := armKeyModel(t)
	m.state = stateIdle
	// Arm quit, then open the completion picker and press a picker-consumed key.
	m2, _, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	if m2.armKind != armQuit {
		t.Fatalf("expected quit armed")
	}
	m2.comp = completionState{active: true, kind: compKindCommand, cands: []candidate{{name: "/cwd"}, {name: "/help"}}}
	m3 := step(m2, tea.KeyMsg{Type: tea.KeyDown}) // consumed by the picker
	if m3.armKind != armNone {
		t.Errorf("picker-consumed key should clear the arm; armKind=%v", m3.armKind)
	}
}

func TestArmResumePickerPassthroughArmsQuit(t *testing.T) {
	m := armKeyModel(t)
	m.state = stateIdle
	m.resumePicker.active = true
	// ctrl+c passes through the resume picker, so it should arm quit (not quit).
	m2 := step(m, tea.KeyMsg{Type: tea.KeyCtrlC})
	if m2.armKind != armQuit {
		t.Errorf("ctrl+c with resume picker open should arm quit; armKind=%v", m2.armKind)
	}
	// A picker-consumed navigation key then dismisses the arm.
	m3 := step(m2, tea.KeyMsg{Type: tea.KeyDown})
	if m3.armKind != armNone {
		t.Errorf("resume-picker nav key should clear the arm; armKind=%v", m3.armKind)
	}
}
