package tui

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"
)

// newAutoModel builds a streaming model whose facade consent starts at c.
func newAutoModel(c sessionclient.Consent) (tuiModel, *fakeFacade) {
	f := &fakeFacade{sid: "sess_tui_test", chatID: "chat123", consent: c}
	m := newWiringModel(f)
	m.state = stateStreaming
	return m, f
}

func consentOn() sessionclient.Consent {
	return sessionclient.Consent{AutoApprove: true, AllowDestructive: true}
}

// TestMidTurnAuto_Revoke_Immediate verifies that /auto ON→OFF mid-turn
// revokes auto-approval immediately (not deferred).
func TestMidTurnAuto_Revoke_Immediate(t *testing.T) {
	m, _ := newAutoModel(consentOn())

	m = midTurnEnter(m, "/auto", stateStreaming)

	if m.facade.Consent().AutoApprove {
		t.Error("revoke should turn AutoApprove OFF immediately")
	}
	if m.facade.Consent().AllowDestructive {
		t.Error("revoke should clear AllowDestructive immediately (pair invariant)")
	}
	if m.pendingAutoGrant {
		t.Error("revoke should clear pendingAutoGrant (no grant pending after OFF)")
	}
	last := lastItemText(m)
	if !strings.Contains(last, "revoked") {
		t.Errorf("expected revoke notice, got: %q", last)
	}
}

// TestMidTurnAuto_Grant_Deferred verifies that /auto OFF→ON mid-turn defers
// to the next idle.
func TestMidTurnAuto_Grant_Deferred(t *testing.T) {
	m, _ := newAutoModel(sessionclient.Consent{})

	m = midTurnEnter(m, "/auto", stateStreaming)

	if m.facade.Consent().AutoApprove {
		t.Error("deferred grant must NOT apply immediately — auto should still be OFF")
	}
	if !m.pendingAutoGrant {
		t.Error("OFF→ON /auto mid-turn should set pendingAutoGrant")
	}
	last := lastItemText(m)
	if !strings.Contains(last, "pending grant") {
		t.Errorf("expected pending-grant notice, got: %q", last)
	}
}

// TestMidTurnAuto_Grant_CoalesceCancel verifies that a second /auto mid-turn
// cancels a pending grant (toggle parity).
func TestMidTurnAuto_Grant_CoalesceCancel(t *testing.T) {
	m, _ := newAutoModel(sessionclient.Consent{})

	m = midTurnEnter(m, "/auto", stateStreaming)
	if !m.pendingAutoGrant {
		t.Fatal("first /auto should set pendingAutoGrant")
	}

	m = midTurnEnter(m, "/auto", stateStreaming)
	if m.pendingAutoGrant {
		t.Error("second /auto should cancel the pending grant")
	}
	if m.facade.Consent().AutoApprove {
		t.Error("auto should still be OFF after cancelling pending grant")
	}
}

// TestMidTurnAuto_Grant_AppliedAtIdle verifies that a deferred grant applies
// at the next true idle (clean TurnCompleted, no workflow, no error).
func TestMidTurnAuto_Grant_AppliedAtIdle(t *testing.T) {
	m, f := newAutoModel(sessionclient.Consent{})

	m = midTurnEnter(m, "/auto", stateStreaming)
	if !m.pendingAutoGrant {
		t.Fatal("setup: /auto should set pendingAutoGrant")
	}

	m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{TurnID: "trn_1", Outcome: "complete"}, f.sid))

	if !m.facade.Consent().AutoApprove {
		t.Error("deferred grant should apply at clean idle")
	}
	if m.pendingAutoGrant {
		t.Error("pendingAutoGrant should be cleared after applying")
	}
}

// TestMidTurnAuto_Grant_HeldOnError verifies that a deferred grant does NOT
// apply when the turn ends with an error.
func TestMidTurnAuto_Grant_HeldOnError(t *testing.T) {
	m, f := newAutoModel(sessionclient.Consent{})

	m = midTurnEnter(m, "/auto", stateStreaming)

	m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{TurnID: "trn_1", Outcome: "stream_error"}, f.sid))

	if m.facade.Consent().AutoApprove {
		t.Error("deferred grant should NOT apply on error — hold for next clean idle")
	}
	if !m.pendingAutoGrant {
		t.Error("pendingAutoGrant should still be set (held on error)")
	}
}

// TestMidTurnAuto_Grant_HeldOnWorkflowContinue verifies that a deferred grant
// does NOT apply when the workflow will auto-continue.
func TestMidTurnAuto_Grant_HeldOnWorkflowContinue(t *testing.T) {
	m, f := newAutoModel(sessionclient.Consent{})

	m = midTurnEnter(m, "/auto", stateStreaming)

	m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{TurnID: "trn_1", Outcome: "complete", WorkflowWillContinue: true}, f.sid))

	if m.facade.Consent().AutoApprove {
		t.Error("deferred grant should NOT apply when workflow will continue")
	}
	if !m.pendingAutoGrant {
		t.Error("pendingAutoGrant should still be set (held during workflow continuation)")
	}
}

// TestMidTurnAuto_Destructive_Revoke_Immediate verifies that /auto destructive
// mid-turn revokes the destructive grant immediately when it's currently ON.
func TestMidTurnAuto_Destructive_Revoke_Immediate(t *testing.T) {
	m, _ := newAutoModel(consentOn())

	m = midTurnEnter(m, "/auto destructive", stateStreaming)

	if m.facade.Consent().AllowDestructive {
		t.Error("destructive should be revoked immediately")
	}
	if !m.facade.Consent().AutoApprove {
		t.Error("auto should still be ON (only destructive was revoked)")
	}
}

// TestMidTurnAuto_Destructive_DeferGrant verifies that /auto destructive
// mid-turn defers the grant when it's currently OFF (and auto is ON).
func TestMidTurnAuto_Destructive_DeferGrant(t *testing.T) {
	m, _ := newAutoModel(sessionclient.Consent{AutoApprove: true})

	m = midTurnEnter(m, "/auto destructive", stateStreaming)

	if m.facade.Consent().AllowDestructive {
		t.Error("destructive grant should NOT apply immediately mid-turn")
	}
	if !m.pendingDestructiveGrant {
		t.Error("pendingDestructiveGrant should be set")
	}
}

// TestMidTurnAuto_Destructive_AutoOff_Refused verifies that /auto destructive
// mid-turn is refused when auto is OFF.
func TestMidTurnAuto_Destructive_AutoOff_Refused(t *testing.T) {
	m, _ := newAutoModel(sessionclient.Consent{})

	m = midTurnEnter(m, "/auto destructive", stateStreaming)

	if m.pendingDestructiveGrant {
		t.Error("should not set pendingDestructiveGrant when auto is OFF")
	}
	last := lastItemText(m)
	if !strings.Contains(last, "auto is OFF") {
		t.Errorf("expected 'auto is OFF' refusal, got: %q", last)
	}
}

// TestMidTurnAuto_Grant_AppliedBeforeQueueFlush verifies that a deferred
// grant applies BEFORE the queue flushes at idle.
func TestMidTurnAuto_Grant_AppliedBeforeQueueFlush(t *testing.T) {
	m, f := newAutoModel(sessionclient.Consent{})

	m = midTurnEnter(m, "follow up", stateStreaming)
	m = midTurnEnter(m, "/auto", stateStreaming)

	if len(m.queuedPrompts) != 1 {
		t.Fatalf("expected 1 queued prompt, got %d", len(m.queuedPrompts))
	}
	if !m.pendingAutoGrant {
		t.Fatal("expected pendingAutoGrant to be set")
	}

	m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{TurnID: "trn_1", Outcome: "complete"}, f.sid))

	if !m.facade.Consent().AutoApprove {
		t.Error("grant should have applied before queue flush")
	}
}

// TestMidTurnAuto_RevokeClearsPendingGrant verifies that revoking /auto
// (ON→OFF) mid-turn also clears any pending destructive grant.
func TestMidTurnAuto_RevokeClearsPendingGrant(t *testing.T) {
	m, _ := newAutoModel(sessionclient.Consent{AutoApprove: true})
	m.pendingDestructiveGrant = true

	m = midTurnEnter(m, "/auto", stateStreaming)

	if m.facade.Consent().AutoApprove {
		t.Error("auto should be OFF after revoke")
	}
	if m.pendingAutoGrant {
		t.Error("pendingAutoGrant should be cleared on revoke")
	}
	if m.pendingDestructiveGrant {
		t.Error("pendingDestructiveGrant should be cleared on revoke")
	}
}

// TestMidTurnAuto_PendingGrantClearedOnRotation verifies that rotation clears
// pending grants — they belong to the old conversation's turn cycle.
func TestMidTurnAuto_PendingGrantClearedOnRotation(t *testing.T) {
	m, _ := newAutoModel(sessionclient.Consent{})

	m = midTurnEnter(m, "/auto", stateStreaming)
	if !m.pendingAutoGrant {
		t.Fatal("setup: should have pending grant")
	}

	m = step(m, rotationMsg{facade: rotatedFake()})

	if m.pendingAutoGrant {
		t.Error("pendingAutoGrant should be cleared on rotation")
	}
}

// TestMidTurnAuto_NotQueued verifies that /auto mid-turn is never queued as
// a prompt.
func TestMidTurnAuto_NotQueued(t *testing.T) {
	m, _ := newAutoModel(sessionclient.Consent{})

	m = midTurnEnter(m, "/auto", stateStreaming)

	if len(m.queuedPrompts) != 0 {
		t.Errorf("/auto should not be queued as a prompt, got %d queued", len(m.queuedPrompts))
	}
}

// TestMidTurnAuto_DestructiveDeferred_AppliesIndependently verifies that a
// standalone deferred destructive grant applies at idle even WITHOUT
// pendingAutoGrant.
func TestMidTurnAuto_DestructiveDeferred_AppliesIndependently(t *testing.T) {
	m, f := newAutoModel(sessionclient.Consent{AutoApprove: true})

	m = midTurnEnter(m, "/auto destructive", stateStreaming)
	if !m.pendingDestructiveGrant {
		t.Fatal("setup: pendingDestructiveGrant should be set")
	}
	if m.pendingAutoGrant {
		t.Fatal("pendingAutoGrant should NOT be set (auto was already ON)")
	}

	m = step(m, evt(event.KindTurnCompleted, event.TurnCompleted{TurnID: "trn_1", Outcome: "complete"}, f.sid))

	if !m.facade.Consent().AllowDestructive {
		t.Error("deferred destructive grant should apply at clean idle even without pendingAutoGrant")
	}
	if m.pendingDestructiveGrant {
		t.Error("pendingDestructiveGrant should be cleared after applying")
	}
}

// TestMidTurnAuto_UnknownSubcommand_Rejected verifies that /auto with an
// unknown subcommand is rejected mid-turn.
func TestMidTurnAuto_UnknownSubcommand_Rejected(t *testing.T) {
	m, _ := newAutoModel(sessionclient.Consent{})

	m = midTurnEnter(m, "/auto foo", stateStreaming)

	if m.pendingAutoGrant {
		t.Error("/auto foo should not set pendingAutoGrant")
	}
	last := lastItemText(m)
	if !strings.Contains(last, "usage") {
		t.Errorf("expected usage notice for unknown subcommand, got: %q", last)
	}
}
