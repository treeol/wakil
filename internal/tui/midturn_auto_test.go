package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	agent "github.com/treeol/wakil/internal/agent"
)

// TestMidTurnAuto_Revoke_Immediate verifies that /auto ON→OFF mid-turn
// revokes auto-approval immediately (not deferred). A revoke only affects
// not-yet-approved decisions — it is safe to apply mid-turn.
func TestMidTurnAuto_Revoke_Immediate(t *testing.T) {
	m := newTestTUI(t)
	m.app.SetConsent(agent.ConsentSnapshot{AutoApprove: true, AllowDestructive: true, AllowReads: false})
	m.state = stateStreaming

	m = midTurnEnter(m, "/auto", stateStreaming)

	if m.app.Consent().AutoApprove {
		t.Error("revoke should turn AutoApprove OFF immediately")
	}
	if m.app.Consent().AllowDestructive {
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

// TestMidTurnAuto_Grant_Deferred verifies that /auto OFF→ON mid-turn does NOT
// apply immediately — it defers to the next idle. A mid-turn grant would
// auto-approve tools the user hasn't seen.
func TestMidTurnAuto_Grant_Deferred(t *testing.T) {
	m := newTestTUI(t)
	m.app.SetConsent(agent.ConsentSnapshot{AutoApprove: false, AllowDestructive: false, AllowReads: false})
	m.state = stateStreaming

	m = midTurnEnter(m, "/auto", stateStreaming)

	if m.app.Consent().AutoApprove {
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
// cancels a pending grant (toggle parity). The user can undo a deferred grant
// before it applies.
func TestMidTurnAuto_Grant_CoalesceCancel(t *testing.T) {
	m := newTestTUI(t)
	m.state = stateStreaming

	// First /auto: defer the grant.
	m = midTurnEnter(m, "/auto", stateStreaming)
	if !m.pendingAutoGrant {
		t.Fatal("first /auto should set pendingAutoGrant")
	}

	// Second /auto: cancel the pending grant.
	m = midTurnEnter(m, "/auto", stateStreaming)
	if m.pendingAutoGrant {
		t.Error("second /auto should cancel the pending grant")
	}
	if m.app.Consent().AutoApprove {
		t.Error("auto should still be OFF after cancelling pending grant")
	}
}

// TestMidTurnAuto_Grant_AppliedAtIdle verifies that a deferred grant applies
// at the next true idle (clean AgentDoneMsg, no workflow, no error).
func TestMidTurnAuto_Grant_AppliedAtIdle(t *testing.T) {
	m := newTestTUI(t)
	m.state = stateStreaming

	// Defer the grant.
	m = midTurnEnter(m, "/auto", stateStreaming)
	if !m.pendingAutoGrant {
		t.Fatal("setup: /auto should set pendingAutoGrant")
	}

	// Turn ends cleanly → grant should apply.
	m = step(m, agent.AgentDoneMsg{})

	if !m.app.Consent().AutoApprove {
		t.Error("deferred grant should apply at clean idle")
	}
	if m.pendingAutoGrant {
		t.Error("pendingAutoGrant should be cleared after applying")
	}
}

// TestMidTurnAuto_Grant_HeldOnError verifies that a deferred grant does NOT
// apply when the turn ends with an error — it should hold for the next clean idle.
func TestMidTurnAuto_Grant_HeldOnError(t *testing.T) {
	m := newTestTUI(t)
	m.state = stateStreaming

	// Defer the grant.
	m = midTurnEnter(m, "/auto", stateStreaming)

	// Turn ends with error → grant should NOT apply.
	m = step(m, agent.AgentDoneMsg{Err: errors.New("backend error")})

	if m.app.Consent().AutoApprove {
		t.Error("deferred grant should NOT apply on error — hold for next clean idle")
	}
	if !m.pendingAutoGrant {
		t.Error("pendingAutoGrant should still be set (held on error)")
	}
}

// TestMidTurnAuto_Grant_HeldOnWorkflowContinue verifies that a deferred grant
// does NOT apply when the workflow will auto-continue — it should hold.
func TestMidTurnAuto_Grant_HeldOnWorkflowContinue(t *testing.T) {
	m := newTestTUI(t)
	m.state = stateStreaming

	// Defer the grant.
	m = midTurnEnter(m, "/auto", stateStreaming)

	// Turn ends but workflow will continue → grant should NOT apply.
	m = step(m, agent.AgentDoneMsg{WorkflowWillContinue: true})

	if m.app.Consent().AutoApprove {
		t.Error("deferred grant should NOT apply when workflow will continue")
	}
	if !m.pendingAutoGrant {
		t.Error("pendingAutoGrant should still be set (held during workflow continuation)")
	}
}

// TestMidTurnAuto_Destructive_Revoke_Immediate verifies that /auto destructive
// mid-turn revokes the destructive grant immediately when it's currently ON.
func TestMidTurnAuto_Destructive_Revoke_Immediate(t *testing.T) {
	m := newTestTUI(t)
	m.app.SetConsent(agent.ConsentSnapshot{AutoApprove: true, AllowDestructive: true, AllowReads: false})
	m.state = stateStreaming

	m = midTurnEnter(m, "/auto destructive", stateStreaming)

	if m.app.Consent().AllowDestructive {
		t.Error("destructive should be revoked immediately")
	}
	if !m.app.Consent().AutoApprove {
		t.Error("auto should still be ON (only destructive was revoked)")
	}
}

// TestMidTurnAuto_Destructive_DeferGrant verifies that /auto destructive
// mid-turn defers the grant when it's currently OFF (and auto is ON).
func TestMidTurnAuto_Destructive_DeferGrant(t *testing.T) {
	m := newTestTUI(t)
	m.app.SetConsent(agent.ConsentSnapshot{AutoApprove: true, AllowDestructive: false, AllowReads: false})
	m.state = stateStreaming

	m = midTurnEnter(m, "/auto destructive", stateStreaming)

	if m.app.Consent().AllowDestructive {
		t.Error("destructive grant should NOT apply immediately mid-turn")
	}
	if !m.pendingDestructiveGrant {
		t.Error("pendingDestructiveGrant should be set")
	}
}

// TestMidTurnAuto_Destructive_AutoOff_Refused verifies that /auto destructive
// mid-turn is refused when auto is OFF.
func TestMidTurnAuto_Destructive_AutoOff_Refused(t *testing.T) {
	m := newTestTUI(t)
	m.app.SetConsent(agent.ConsentSnapshot{AutoApprove: false, AllowDestructive: false, AllowReads: false})
	m.state = stateStreaming

	m = midTurnEnter(m, "/auto destructive", stateStreaming)

	if m.pendingDestructiveGrant {
		t.Error("should not set pendingDestructiveGrant when auto is OFF")
	}
	last := lastItemText(m)
	if !strings.Contains(last, "auto is OFF") {
		t.Errorf("expected 'auto is OFF' refusal, got: %q", last)
	}
}

// TestMidTurnAuto_Grant_AppliedBeforeQueueFlush verifies that when both a
// deferred grant and a queued prompt are pending, the grant applies BEFORE
// the queue flushes — so the queued prompt's turn runs under the new consent.
func TestMidTurnAuto_Grant_AppliedBeforeQueueFlush(t *testing.T) {
	m := newTestTUI(t)
	m.state = stateStreaming

	// Queue a prompt AND defer a grant.
	m = midTurnEnter(m, "follow up", stateStreaming)
	m = midTurnEnter(m, "/auto", stateStreaming)

	if len(m.queuedPrompts) != 1 {
		t.Fatalf("expected 1 queued prompt, got %d", len(m.queuedPrompts))
	}
	if !m.pendingAutoGrant {
		t.Fatal("expected pendingAutoGrant to be set")
	}

	// Turn ends cleanly. The grant should apply, and the queue should flush.
	// We can't easily test the full flush (it starts a real turn), but we can
	// verify the grant applied and the queue started draining. Since flushQueuedPrompt
	// needs a real backend, we just check the grant applied — the queue flush
	// is tested separately in the Phase A tests.
	m = step(m, agent.AgentDoneMsg{})

	if !m.app.Consent().AutoApprove {
		t.Error("grant should have applied before queue flush")
	}
}

// TestMidTurnAuto_RevokeClearsPendingGrant verifies that revoking /auto
// (ON→OFF) mid-turn also clears any pending destructive grant that was set.
func TestMidTurnAuto_RevokeClearsPendingGrant(t *testing.T) {
	m := newTestTUI(t)
	m.state = stateStreaming

	// Auto is ON with a pending destructive grant.
	m.app.SetConsent(agent.ConsentSnapshot{AutoApprove: true, AllowDestructive: false, AllowReads: false})
	m.pendingDestructiveGrant = true

	// Revoke auto mid-turn.
	m = midTurnEnter(m, "/auto", stateStreaming)

	if m.app.Consent().AutoApprove {
		t.Error("auto should be OFF after revoke")
	}
	if m.pendingAutoGrant {
		t.Error("pendingAutoGrant should be cleared on revoke")
	}
	if m.pendingDestructiveGrant {
		t.Error("pendingDestructiveGrant should be cleared on revoke")
	}
}

// TestMidTurnAuto_PendingGrantClearedOnNewConv verifies that /new clears
// pending grants — they belong to the old conversation's turn cycle.
func TestMidTurnAuto_PendingGrantClearedOnNewConv(t *testing.T) {
	m := newTestTUI(t)
	m.state = stateStreaming

	// Defer a grant.
	m = midTurnEnter(m, "/auto", stateStreaming)
	if !m.pendingAutoGrant {
		t.Fatal("setup: should have pending grant")
	}

	// Start a new conversation.
	m = step(m, agent.NewConvMsg{Note: "fresh conversation"})

	if m.pendingAutoGrant {
		t.Error("pendingAutoGrant should be cleared on /new")
	}
}

// TestMidTurnAuto_NotQueued verifies that /auto mid-turn is never queued as
// a prompt — it is handled as a command, not plain text.
func TestMidTurnAuto_NotQueued(t *testing.T) {
	m := newTestTUI(t)
	m.state = stateStreaming

	m = midTurnEnter(m, "/auto", stateStreaming)

	if len(m.queuedPrompts) != 0 {
		t.Errorf("/auto should not be queued as a prompt, got %d queued", len(m.queuedPrompts))
	}
}

// TestMidTurnAuto_DestructiveDeferred_AppliesIndependently verifies that a
// standalone deferred destructive grant (auto already ON, destructive OFF→ON
// mid-turn) applies at idle even WITHOUT pendingAutoGrant. This was a bug
// found in Mashūra review: the destructive application was nested inside
// `if m.pendingAutoGrant`, so it never fired when only destructive was deferred.
func TestMidTurnAuto_DestructiveDeferred_AppliesIndependently(t *testing.T) {
	m := newTestTUI(t)
	m.app.SetConsent(agent.ConsentSnapshot{AutoApprove: true, AllowDestructive: false, AllowReads: false})
	m.state = stateStreaming

	// Defer the destructive grant (auto is already ON).
	m = midTurnEnter(m, "/auto destructive", stateStreaming)
	if !m.pendingDestructiveGrant {
		t.Fatal("setup: pendingDestructiveGrant should be set")
	}
	if m.pendingAutoGrant {
		t.Fatal("pendingAutoGrant should NOT be set (auto was already ON)")
	}

	// Turn ends cleanly → destructive grant should apply independently.
	m = step(m, agent.AgentDoneMsg{})

	if !m.app.Consent().AllowDestructive {
		t.Error("deferred destructive grant should apply at clean idle even without pendingAutoGrant")
	}
	if m.pendingDestructiveGrant {
		t.Error("pendingDestructiveGrant should be cleared after applying")
	}
}

// TestMidTurnAuto_UnknownSubcommand_Rejected verifies that /auto with an
// unknown subcommand (e.g. /auto foo) is rejected mid-turn, matching the
// idle handler's usage message.
func TestMidTurnAuto_UnknownSubcommand_Rejected(t *testing.T) {
	m := newTestTUI(t)
	m.state = stateStreaming

	m = midTurnEnter(m, "/auto foo", stateStreaming)

	if m.pendingAutoGrant {
		t.Error("/auto foo should not set pendingAutoGrant")
	}
	last := lastItemText(m)
	if !strings.Contains(last, "usage") {
		t.Errorf("expected usage notice for unknown subcommand, got: %q", last)
	}
}

// Ensure the test file compiles with all imports used.
var _ = context.Background
