package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/treeol/wakil/internal/core/sessionclient"
)

// rotationTestModel builds a minimal wired model via newWiringModel (items
// etc. initialized), then strips turn history so the effective layout matches
// a fresh splash state (no prior turn, no items).
func rotationTestModel() tuiModel {
	f := &fakeFacade{sid: "sess_tui_test", chatID: "chat123"}
	m := newWiringModel(f)
	*m.items = (*m.items)[:0]
	m.hadTurn = false
	m.state = stateIdle
	return m
}

// TestRotationBegin_RendersLabelFreshState covers AC1: applying a rotate
// command result in a fresh splash state renders the rotation label
// immediately (View() runs after every Update — no tick needed) and the
// status zone is forced visible.
func TestRotationBegin_RendersLabelFreshState(t *testing.T) {
	for _, tc := range []struct {
		typ   string
		label string
	}{
		{"handoff", "handing off"},
		{"new", "starting new session"},
		{"resume", "resuming session"},
	} {
		m := rotationTestModel()
		m, _, _ = m.applyCommandResult(sessionclient.CommandResult{
			Handled: true,
			Rotate:  &sessionclient.RotateRequest{Type: tc.typ},
		}, nil)
		if !m.rotating {
			t.Fatalf("%s: rotating flag not set", tc.typ)
		}
		if m.rotationStart.IsZero() {
			t.Fatalf("%s: rotationStart not set", tc.typ)
		}
		if !m.statusVisible() {
			t.Fatalf("%s: status zone must be visible during rotation from fresh state", tc.typ)
		}
		if !m.dotArmed {
			// From a fresh idle model the tick must be armed at rotation
			// begin — otherwise nothing animates and the timer never updates.
			t.Fatalf("%s: dot tick not armed at rotation begin", tc.typ)
		}
		var view string
		for _, l := range m.statusLines() {
			view += l + "\n"
		}
		v := plain(view)
		if !strings.Contains(v, tc.label) {
			t.Fatalf("%s: status lines missing rotation label %q: %q", tc.typ, tc.label, v)
		}
		if !strings.Contains(v, "…") || !strings.Contains(v, "0s") {
			t.Fatalf("%s: missing elapsed seconds in %q", tc.typ, v)
		}
	}
}

// TestRotationBegin_Picker covers the resume-picker entry point: beginRotate
// sets the rotating flag and arms the tick (previously the picker never set
// rotating — sends were not blocked during picker-initiated resume).
func TestRotationBegin_Picker(t *testing.T) {
	m := newPickerModel()
	m, _, _ = m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyDown})
	m, cmd, _ := m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected rotation command")
	}
	if !m.rotating {
		t.Fatal("picker resume must set rotating (beginRotate)")
	}
	if m.rotationStart.IsZero() {
		t.Fatal("rotationStart must be set")
	}
	if !m.dotArmed {
		t.Fatal("dot tick must be armed at picker resume")
	}
}

// TestRotationLabel_WinsOverStreaming covers AC4: if a turn is somehow in
// flight during rotation, the rotating label wins.
func TestRotationLabel_WinsOverStreaming(t *testing.T) {
	m := rotationTestModel()
	m.state = stateStreaming
	m.hadTurn = true
	m, _, _ = m.applyCommandResult(sessionclient.CommandResult{
		Handled: true,
		Rotate:  &sessionclient.RotateRequest{Type: "handoff"},
	}, nil)
	var view string
	for _, l := range m.statusLines() {
		view += l + "\n"
	}
	v := plain(view)
	if !strings.Contains(v, "handing off") {
		t.Fatalf("rotation label must win over streaming: %q", v)
	}
	if strings.Contains(v, "streaming") {
		t.Fatalf("streaming label must be suppressed during rotation: %q", v)
	}
}

// TestRotationTick_ReArmsAndAnimates covers AC2: consecutive dot ticks during
// rotation advance the phase and keep the chain alive (dotArmed stays true).
func TestRotationTick_ReArmsAndAnimates(t *testing.T) {
	m := rotationTestModel()
	m, _, _ = m.applyCommandResult(sessionclient.CommandResult{
		Handled: true,
		Rotate:  &sessionclient.RotateRequest{Type: "new"},
	}, nil)
	if m.dotPhase != 0 {
		t.Fatalf("setup: dotPhase = %d, want 0", m.dotPhase)
	}
	m = step(m, dotTickMsg{})
	if !m.dotArmed {
		t.Fatal("tick chain must re-arm while rotating")
	}
	if m.dotPhase != 1 {
		t.Fatalf("dotPhase = %d, want 1 after first tick", m.dotPhase)
	}
	m = step(m, dotTickMsg{})
	if m.dotPhase != 2 {
		t.Fatalf("dotPhase = %d, want 2 after second tick", m.dotPhase)
	}
	if !m.rotating {
		t.Fatal("rotating must persist across ticks")
	}
}

// TestRotationElapsed_Increases covers the timer behavior: the rendered
// elapsed seconds derive from rotationStart (wall clock), asserted via regex.
func TestRotationElapsed_Increases(t *testing.T) {
	m := rotationTestModel()
	m, _, _ = m.applyCommandResult(sessionclient.CommandResult{
		Handled: true,
		Rotate:  &sessionclient.RotateRequest{Type: "handoff"},
	}, nil)
	m.rotationStart = time.Now().Add(-12 * time.Second)
	var view string
	for _, l := range m.statusLines() {
		view += l + "\n"
	}
	if !strings.Contains(plain(view), "12s") {
		t.Fatalf("expected 12s elapsed in %q", plain(view))
	}
}

// TestRotationFailure_ResetsIndicator covers AC3 (failure path): the
// indicator disappears, rotation fields are cleared, and the effective
// status height after apply matches what reflow installed (no drift).
func TestRotationFailure_ResetsIndicator(t *testing.T) {
	m := rotationTestModel()
	m, _, _ = m.applyCommandResult(sessionclient.CommandResult{
		Handled: true,
		Rotate:  &sessionclient.RotateRequest{Type: "new"},
	}, nil)
	if !m.rotating {
		t.Fatal("setup: rotating not set")
	}
	m = step(m, rotationMsg{failed: true, err: errTestBoom(), kind: rotateNew})
	if m.rotating {
		t.Fatal("rotating must be cleared on failure")
	}
	if !m.rotationStart.IsZero() {
		t.Fatal("rotationStart must be cleared on failure")
	}
	if m.rotationNoteShown {
		t.Fatal("rotationNoteShown must reset on rotation end")
	}
	// Viewport geometry must match sizes() unconditionally (newWiringModel
	// sets ready/width/height — the check is never vacuously skipped).
	_, wantVpH, _ := m.sizes()
	if m.vp.Height != wantVpH {
		t.Fatalf("viewport height drift: vp=%d want %d", m.vp.Height, wantVpH)
	}
}

// TestRotationSuccess_ResetsIndicator covers AC3 (success path): after
// applyRotation succeeds the rotation label is gone and fields cleared.
func TestRotationSuccess_ResetsIndicator(t *testing.T) {
	m := rotationTestModel()
	m, _, _ = m.applyCommandResult(sessionclient.CommandResult{
		Handled: true,
		Rotate:  &sessionclient.RotateRequest{Type: "handoff"},
	}, nil)
	f2 := rotatedFake()
	m = step(m, rotationMsg{facade: f2, kind: rotateHandoff})
	if m.rotating {
		t.Fatal("rotating must be cleared on success")
	}
	if !m.rotationStart.IsZero() {
		t.Fatal("rotationStart must be cleared on success")
	}
	var view string
	for _, l := range m.statusLines() {
		view += l + "\n"
	}
	if strings.Contains(plain(view), "handing off") {
		t.Fatalf("rotation label must be gone after success: %q", plain(view))
	}
	if m.facade.Snapshot().SessionID != "sess_rotated" {
		t.Fatal("facade must be swapped on success")
	}
	// Geometry in sync after success (impl-review finding #6).
	_, wantVpH, _ := m.sizes()
	if m.vp.Height != wantVpH {
		t.Fatalf("post-success viewport drift: vp=%d want %d", m.vp.Height, wantVpH)
	}
}

// TestRotationSuccess_NewRestoresSplash covers the /new success path from a
// fresh state: hadTurn=false, status zone hidden again (splash restored), and
// geometry in sync (impl-review finding #6: the 0→N→0 reflow chain).
func TestRotationSuccess_NewRestoresSplash(t *testing.T) {
	m := rotationTestModel()
	m.hadTurn = true // established session pre-rotation
	m, _, _ = m.applyCommandResult(sessionclient.CommandResult{
		Handled: true,
		Rotate:  &sessionclient.RotateRequest{Type: "new"},
	}, nil)
	m = step(m, rotationMsg{facade: rotatedFake(), kind: rotateNew})
	if m.hadTurn {
		t.Fatal("/new success must reset hadTurn (splash restored)")
	}
	if m.statusVisible() {
		t.Fatal("status zone must be hidden after /new from empty conversation")
	}
	_, wantVpH, _ := m.sizes()
	if m.vp.Height != wantVpH {
		t.Fatalf("/new viewport drift: vp=%d want %d", m.vp.Height, wantVpH)
	}
}

// TestRotationBlockedSend_PrintsNote covers the user decision: a plain send
// during rotation prints a visible note (deduped) instead of silently
// dropping.
func TestRotationBlockedSend_PrintsNote(t *testing.T) {
	m := rotationTestModel()
	m, _, _ = m.applyCommandResult(sessionclient.CommandResult{
		Handled: true,
		Rotate:  &sessionclient.RotateRequest{Type: "handoff"},
	}, nil)
	itemsBefore := len(*m.items)
	m.ta.SetValue("hello")
	m2, cmds, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if len(cmds) != 0 {
		t.Fatalf("send during rotation must not dispatch commands, got %d", len(cmds))
	}
	if len(*m2.items) != itemsBefore+1 {
		t.Fatalf("expected one note item, before=%d after=%d", itemsBefore, len(*m2.items))
	}
	if !strings.Contains(plain((*m2.items)[len(*m2.items)-1].text), "rotation in progress") {
		t.Fatalf("note text missing: %q", plain((*m2.items)[len(*m2.items)-1].text))
	}
	// Slash command during rotation: pre-existing note path (no dedupe
	// contract there — the command-ignored note always prints).
	m, _, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/handoff")})
	m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if len(*m.items) != itemsBefore+2 {
		t.Fatalf("slash command during rotation must print its own note, got %d items", len(*m.items))
	}
	// Second rotation: dedupe resets — note prints again.
	m, _, _ = m.applyCommandResult(sessionclient.CommandResult{
		Handled: true,
		Rotate:  &sessionclient.RotateRequest{Type: "new"},
	}, nil)
	m2, cmds, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyEnter})
	if len(cmds) != 0 {
		t.Fatalf("send during second rotation must not dispatch, got %d", len(cmds))
	}
	if len(*m2.items) != itemsBefore+3 {
		t.Fatalf("rotationNoteShown must reset per rotation (notes: plain+slash+second rotation), got %d items", len(*m2.items))
	}
}

// TestRotationBegin_ReflowFreshState covers AC1's layout half: the fresh
// splash state 0→N status flip reflows synchronously (vp height matches
// sizes()).
func TestRotationBegin_ReflowFreshState(t *testing.T) {
	m := rotationTestModel()
	_, wantBefore, _ := m.sizes()
	m, _, _ = m.applyCommandResult(sessionclient.CommandResult{
		Handled: true,
		Rotate:  &sessionclient.RotateRequest{Type: "handoff"},
	}, nil)
	_, wantAfter, _ := m.sizes()
	if m.vp.Height != wantAfter {
		t.Fatalf("vp.Height = %d, want %d after rotation begin", m.vp.Height, wantAfter)
	}
	if wantAfter == wantBefore {
		t.Log("note: no height change expected in this fixture (status already visible); asserted sync anyway")
	}
}

// TestRotationBegin_PickerReflow covers the picker entry point's synchronous
// reflow (impl-review finding #1). Uses a fully-initialized wiring model with
// the picker opened, since reflow needs a live viewport/items.
func TestRotationBegin_PickerReflow(t *testing.T) {
	f := &fakeFacade{sid: "sess_tui_test", chatID: "chat123"}
	m := newWiringModel(f)
	m.hadTurn = false
	*m.items = (*m.items)[:0]
	m = m.openResumePicker(testSessions(), sessionclient.SessionScope{Workspace: "/work"}, 0)
	m, _, _ = m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyDown})
	m, cmd, _ := m.handleResumePickerKey(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("expected rotation command")
	}
	if !m.rotating {
		t.Fatal("picker resume must set rotating")
	}
	_, wantVpH, _ := m.sizes()
	if m.vp.Height != wantVpH {
		t.Fatalf("picker resume: vp.Height = %d, want %d (status flip not reflowed)", m.vp.Height, wantVpH)
	}
}

// TestRotationDotPulseIdle: the test env strips color (all lipgloss renders
// collapse to plain text), so dot shades can't be asserted by string. The
// pulse behavior is covered indirectly: TestRotationBegin_RendersLabelFreshState
// asserts the tick chain is armed (the thing that drives the pulse), and
// TestRotationTick_ReArmsAndAnimates asserts phase advances during rotation.
// Here we pin only the signature contract: pulse=true at idle must not panic
// and must render the dot.
func TestRotationDotPulseIdle(t *testing.T) {
	if got := renderStatusDot(stateIdle, 0, true); got != "•" {
		t.Fatalf("pulse dot must render the bullet glyph, got %q", got)
	}
}

// errTestBoom returns a plain error for failure-path tests.
func errTestBoom() error { return errRotationBoom{} }

type errRotationBoom struct{}

func (errRotationBoom) Error() string { return "boom" }
