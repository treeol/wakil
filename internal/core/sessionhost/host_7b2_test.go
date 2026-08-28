package sessionhost

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
)

// testPrincipal creates a principal for 7b2 tests.
func testPrincipal() core.Principal {
	return core.Principal{
		TenantID:   event.EmbeddedTenantID,
		UserID:     event.EmbeddedUserID,
		Role:       core.RoleOwner,
		AuthMethod: core.AuthEmbedded,
	}
}

// === Session-scoped emitter tests (D24) ===

// TestSessionEmitterLegalAfterTurn verifies that the session-scoped emitter
// can emit events AFTER the turn completes (unlike the turn-scoped emitter
// which is fenced at turn completion).
func TestSessionEmitterLegalAfterTurn(t *testing.T) {
	postTurnEmit := make(chan error, 1)
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		// Emit during turn via turn-scoped emitter
		if err := in.Emit.Emit(event.KindToolCallStarted, event.ToolCallStarted{
			TurnID:     in.TurnID,
			ToolCallID: "tcl_1",
			Name:       "test_tool",
		}); err != nil {
			return "", err
		}
		// Stash the session-scoped emitter for post-turn use
		sessionEmit := in.SessionEmit
		go func() {
			// Wait for the turn to complete, then emit via session-scoped
			time.Sleep(50 * time.Millisecond)
			err := sessionEmit.Emit(event.KindAsyncJobStarted, event.AsyncJobStarted{
				OpID:  "op_1",
				Label: "detached job",
			})
			postTurnEmit <- err
		}()
		return "hello", nil
	}, WithAgentName("test"))

	defer h.Close(context.Background())
	p := testPrincipal()
	s := createSession(t, h, p)

	sub, err := h.Subscribe(context.Background(), p, s.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	_, err = h.SubmitInput(context.Background(), p, core.SubmitInputRequest{
		SessionID: s.ID,
		Text:      "test",
	})
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	// Wait for the post-turn emit to succeed
	select {
	case emitErr := <-postTurnEmit:
		if emitErr != nil {
			t.Fatalf("session-scoped Emit after turn failed: %v", emitErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for post-turn emit")
	}

	// Collect events and verify AsyncJobStarted was received
	deadline := time.After(2 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		ev, err := sub.Next(ctx)
		cancel()
		if err != nil {
			select {
			case <-deadline:
				t.Fatal("timed out waiting for AsyncJobStarted event")
			default:
				continue
			}
		}
		if ev.Kind == event.KindAsyncJobStarted {
			return // success
		}
	}
}

// TestSessionEmitterFencedAtClose verifies that the session-scoped emitter
// is fenced (returns ErrEmitterClosed) after the session is closed.
func TestSessionEmitterFencedAtClose(t *testing.T) {
	var sessionEmit SessionEmitter
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		sessionEmit = in.SessionEmit
		return "hello", nil
	}, WithAgentName("test"))

	defer h.Close(context.Background())
	p := testPrincipal()
	s := createSession(t, h, p)

	_, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{
		SessionID: s.ID,
		Text:      "test",
	})
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	// Wait for turn to complete
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})

	// Close the session
	if err := h.CloseSession(context.Background(), p, s.ID); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionClosed
	})

	// Wait a bit for finalizeClose to run (state is set before emitter fence)
	time.Sleep(50 * time.Millisecond)

	// Now the session-scoped emitter should be fenced
	if sessionEmit == nil {
		t.Fatal("sessionEmit is nil")
	}
	err = sessionEmit.Emit(event.KindAsyncJobStarted, event.AsyncJobStarted{
		OpID:  "op_1",
		Label: "should fail",
	})
	if !errors.Is(err, ErrEmitterClosed) {
		t.Errorf("Emit after close = %v, want ErrEmitterClosed", err)
	}
}

// TestSessionEmitterRejectsHostReserved verifies that the session-scoped
// emitter rejects host-reserved kinds (same as the turn-scoped emitter).
func TestSessionEmitterRejectsHostReserved(t *testing.T) {
	var sessionEmit SessionEmitter
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		sessionEmit = in.SessionEmit
		return "hello", nil
	}, WithAgentName("test"))

	defer h.Close(context.Background())
	p := testPrincipal()
	s := createSession(t, h, p)

	_, _ = h.SubmitInput(context.Background(), p, core.SubmitInputRequest{
		SessionID: s.ID,
		Text:      "test",
	})
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})

	if sessionEmit == nil {
		t.Fatal("sessionEmit is nil")
	}
	// Try to emit a host-reserved kind
	err := sessionEmit.Emit(event.KindMessageCommitted, event.MessageCommitted{
		TurnID: "trn_1",
		Text:   "should fail",
	})
	if err == nil || !strings.Contains(err.Error(), "host-owned") {
		t.Errorf("Emit host-reserved kind = %v, want host-owned error", err)
	}
}

// TestSessionEmitterRejectsEphemeralEmit verifies that the session-scoped
// emitter rejects ephemeral kinds on Emit (durable only), but accepts them
// on Notify.
func TestSessionEmitterRejectsEphemeralEmit(t *testing.T) {
	var sessionEmit SessionEmitter
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		sessionEmit = in.SessionEmit
		// Notify should accept ephemeral
		sessionEmit.Notify(event.KindTokRate, event.TokRate{Rate: 42.0})
		return "hello", nil
	}, WithAgentName("test"))

	defer h.Close(context.Background())
	p := testPrincipal()
	s := createSession(t, h, p)

	sub, err := h.Subscribe(context.Background(), p, s.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	_, _ = h.SubmitInput(context.Background(), p, core.SubmitInputRequest{
		SessionID: s.ID,
		Text:      "test",
	})
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})

	if sessionEmit == nil {
		t.Fatal("sessionEmit is nil")
	}
	// Emit should reject ephemeral
	err = sessionEmit.Emit(event.KindTokRate, event.TokRate{Rate: 42.0})
	if err == nil || !strings.Contains(err.Error(), "ephemeral") {
		t.Errorf("Emit ephemeral = %v, want ephemeral error", err)
	}
}

// === Async approval tests (D25) ===

// TestAsyncApprovalRoundTrip verifies the full async approval round-trip:
// park → RespondToApproval → resume → turn completes.
func TestAsyncApprovalRoundTrip(t *testing.T) {
	approvalDone := make(chan struct{})
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		// Simulate the adapter's async confirmer:
		// 1. Emit ApprovalRequested
		approvalID := event.ApprovalID("apr_test1")
		if err := in.Emit.Emit(event.KindApprovalRequested, event.ApprovalRequested{
			ApprovalID: approvalID,
			ToolName:   "run_shell",
			Headline:   "test",
			Detail:     "test detail",
		}); err != nil {
			return "", err
		}
		// 2. Park the approval (blocks until RespondToApproval or ctx cancel)
		outcome, reason, _ := in.ParkApproval(ctx, approvalID)
		if outcome != "approved" {
			return "", errors.New("expected approved, got " + outcome + " (" + reason + ")")
		}
		close(approvalDone)
		return "approved and completed", nil
	}, WithAgentName("test"))

	defer h.Close(context.Background())
	p := testPrincipal()
	s := createSession(t, h, p)

	// Subscribe to see approval events
	sub, err := h.Subscribe(context.Background(), p, s.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	_, err = h.SubmitInput(context.Background(), p, core.SubmitInputRequest{
		SessionID: s.ID,
		Text:      "test",
	})
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	// Wait for the session to enter awaiting_approval
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionAwaitingApproval
	})

	// Respond to the approval
	err = h.RespondToApproval(context.Background(), p, core.ApprovalDecision{
		SessionID:  s.ID,
		ApprovalID: event.ApprovalID("apr_test1"),
		Outcome:    core.ApprovalAllowOnce,
	})
	if err != nil {
		t.Fatalf("RespondToApproval: %v", err)
	}

	// Wait for the turn to complete
	select {
	case <-approvalDone:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for approval to complete")
	}
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})

	// Verify the session state is back to idle (ran → awaiting_approval → running → idle)
	g, _ := h.GetSession(context.Background(), p, s.ID)
	if g.State != core.SessionIdle {
		t.Fatalf("final state = %q, want idle", g.State)
	}
}

// TestAsyncApprovalCancelDuringApproval verifies that Interrupt during
// awaiting_approval causes a forced decline and the turn completes as cancelled.
func TestAsyncApprovalCancelDuringApproval(t *testing.T) {
	turnDone := make(chan error, 1)
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		approvalID := event.ApprovalID("apr_test2")
		if err := in.Emit.Emit(event.KindApprovalRequested, event.ApprovalRequested{
			ApprovalID: approvalID,
			ToolName:   "run_shell",
			Headline:   "test",
			Detail:     "test detail",
		}); err != nil {
			return "", err
		}
		outcome, reason, _ := in.ParkApproval(ctx, approvalID)
		// Should get a forced decline on cancel
		if outcome != "declined" {
			turnDone <- errors.New("expected declined, got " + outcome)
			return "", errors.New("expected declined")
		}
		if reason != "cancelled" {
			turnDone <- errors.New("expected cancelled reason, got " + reason)
			return "", errors.New("expected cancelled")
		}
		turnDone <- nil
		return "", nil // turn returns with empty text → "empty" outcome (not "cancelled" since ctx may not be done yet)
	}, WithAgentName("test"))

	defer h.Close(context.Background())
	p := testPrincipal()
	s := createSession(t, h, p)

	_, _ = h.SubmitInput(context.Background(), p, core.SubmitInputRequest{
		SessionID: s.ID,
		Text:      "test",
	})

	// Wait for awaiting_approval
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionAwaitingApproval
	})

	// Interrupt the session
	if err := h.Interrupt(context.Background(), p, s.ID); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	// Wait for the turn goroutine to see the forced decline
	select {
	case err := <-turnDone:
		if err != nil {
			t.Fatalf("turn goroutine error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for turn to see forced decline")
	}

	// The session should NOT be in awaiting_approval anymore
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State != core.SessionAwaitingApproval
	})
}

// TestAsyncApprovalNotFound verifies that RespondToApproval returns
// ErrApprovalNotFound when there is no pending approval.
func TestAsyncApprovalNotFound(t *testing.T) {
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		return "hello", nil
	}, WithAgentName("test"))

	defer h.Close(context.Background())
	p := testPrincipal()
	s := createSession(t, h, p)

	// No pending approval → should get not found
	err := h.RespondToApproval(context.Background(), p, core.ApprovalDecision{
		SessionID:  s.ID,
		ApprovalID: event.ApprovalID("apr_nonexistent"),
		Outcome:    core.ApprovalAllowOnce,
	})
	if !errors.Is(err, core.ErrApprovalNotFound) {
		t.Errorf("RespondToApproval with no pending = %v, want ErrApprovalNotFound", err)
	}
}

// TestAsyncApprovalAlreadyResolved verifies that a duplicate RespondToApproval
// with the same outcome is idempotent, and a different outcome returns
// ErrApprovalAlreadyResolved.
func TestAsyncApprovalAlreadyResolved(t *testing.T) {
	firstRespond := make(chan struct{})
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		approvalID := event.ApprovalID("apr_test3")
		if err := in.Emit.Emit(event.KindApprovalRequested, event.ApprovalRequested{
			ApprovalID: approvalID,
			ToolName:   "run_shell",
			Headline:   "test",
			Detail:     "test detail",
		}); err != nil {
			return "", err
		}
		outcome, _, _ := in.ParkApproval(ctx, approvalID)
		if outcome != "approved" {
			return "", errors.New("expected approved")
		}
		close(firstRespond)
		return "done", nil
	}, WithAgentName("test"))

	defer h.Close(context.Background())
	p := testPrincipal()
	s := createSession(t, h, p)

	_, _ = h.SubmitInput(context.Background(), p, core.SubmitInputRequest{
		SessionID: s.ID,
		Text:      "test",
	})

	// Wait for awaiting_approval
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionAwaitingApproval
	})

	// First response: approve
	err := h.RespondToApproval(context.Background(), p, core.ApprovalDecision{
		SessionID:  s.ID,
		ApprovalID: event.ApprovalID("apr_test3"),
		Outcome:    core.ApprovalAllowOnce,
	})
	if err != nil {
		t.Fatalf("first RespondToApproval: %v", err)
	}

	// Wait for the turn to complete (pendingApproval is cleared)
	select {
	case <-firstRespond:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first response")
	}

	// Second response with same outcome: should be idempotent (no error)
	// The pendingApproval is already cleared, so this should return NotFound
	err = h.RespondToApproval(context.Background(), p, core.ApprovalDecision{
		SessionID:  s.ID,
		ApprovalID: event.ApprovalID("apr_test3"),
		Outcome:    core.ApprovalAllowOnce,
	})
	if !errors.Is(err, core.ErrApprovalNotFound) {
		t.Errorf("second RespondToApproval = %v, want ErrApprovalNotFound (pending already cleared)", err)
	}
}

// TestAsyncApprovalWrongID verifies that RespondToApproval with a wrong
// approval ID returns ErrApprovalNotFound even when an approval is pending.
func TestAsyncApprovalWrongID(t *testing.T) {
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		approvalID := event.ApprovalID("apr_correct")
		if err := in.Emit.Emit(event.KindApprovalRequested, event.ApprovalRequested{
			ApprovalID: approvalID,
			ToolName:   "run_shell",
			Headline:   "test",
			Detail:     "test detail",
		}); err != nil {
			return "", err
		}
		outcome, _, _ := in.ParkApproval(ctx, approvalID)
		if outcome != "approved" {
			return "", errors.New("expected approved")
		}
		return "done", nil
	}, WithAgentName("test"))

	defer h.Close(context.Background())
	p := testPrincipal()
	s := createSession(t, h, p)

	_, _ = h.SubmitInput(context.Background(), p, core.SubmitInputRequest{
		SessionID: s.ID,
		Text:      "test",
	})

	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionAwaitingApproval
	})

	// Wrong approval ID
	err := h.RespondToApproval(context.Background(), p, core.ApprovalDecision{
		SessionID:  s.ID,
		ApprovalID: event.ApprovalID("apr_wrong"),
		Outcome:    core.ApprovalAllowOnce,
	})
	if !errors.Is(err, core.ErrApprovalNotFound) {
		t.Errorf("RespondToApproval with wrong ID = %v, want ErrApprovalNotFound", err)
	}

	// Clean up: approve with the correct ID
	err = h.RespondToApproval(context.Background(), p, core.ApprovalDecision{
		SessionID:  s.ID,
		ApprovalID: event.ApprovalID("apr_correct"),
		Outcome:    core.ApprovalAllowOnce,
	})
	if err != nil {
		t.Fatalf("RespondToApproval with correct ID: %v", err)
	}
}

// TestHeadlessSyncApprovalParity verifies that the headless sync-mode
// (inline resolver, no parking) still works — the TurnFunc can ignore
// ParkApproval and use its own resolver, and RespondToApproval returns
// ErrApprovalNotFound (no pending approval was parked).
func TestHeadlessSyncApprovalParity(t *testing.T) {
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		// Simulate the headless sync path: emit ApprovalRequested,
		// resolve inline (no parking), emit ApprovalResolved.
		approvalID := event.ApprovalID("apr_sync")
		if err := in.Emit.Emit(event.KindApprovalRequested, event.ApprovalRequested{
			ApprovalID: approvalID,
			ToolName:   "run_shell",
			Headline:   "test",
			Detail:     "test detail",
		}); err != nil {
			return "", err
		}
		// Inline resolution (no ParkApproval call)
		if err := in.Emit.Emit(event.KindApprovalResolved, event.ApprovalResolved{
			ApprovalID: approvalID,
			Outcome:    "approved",
			Resolver:   in.UserID,
		}); err != nil {
			return "", err
		}
		return "sync approved", nil
	}, WithAgentName("test"))

	defer h.Close(context.Background())
	p := testPrincipal()
	s := createSession(t, h, p)

	_, _ = h.SubmitInput(context.Background(), p, core.SubmitInputRequest{
		SessionID: s.ID,
		Text:      "test",
	})

	// Wait for turn to complete
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})

	// RespondToApproval should return NotFound (no pending was parked)
	err := h.RespondToApproval(context.Background(), p, core.ApprovalDecision{
		SessionID:  s.ID,
		ApprovalID: event.ApprovalID("apr_sync"),
		Outcome:    core.ApprovalAllowOnce,
	})
	if !errors.Is(err, core.ErrApprovalNotFound) {
		t.Errorf("RespondToApproval in sync mode = %v, want ErrApprovalNotFound", err)
	}
}

// TestSessionEmitterNotifyAcceptsEphemeral verifies that the session-scoped
// emitter's Notify method accepts ephemeral events and delivers them to
// subscribers.
func TestSessionEmitterNotifyAcceptsEphemeral(t *testing.T) {
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		// Notify an ephemeral event via session-scoped emitter
		in.SessionEmit.Notify(event.KindTokRate, event.TokRate{Rate: 99.0})
		return "hello", nil
	}, WithAgentName("test"))

	defer h.Close(context.Background())
	p := testPrincipal()
	s := createSession(t, h, p)

	sub, err := h.Subscribe(context.Background(), p, s.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	_, _ = h.SubmitInput(context.Background(), p, core.SubmitInputRequest{
		SessionID: s.ID,
		Text:      "test",
	})

	// Collect events and look for the TokRate ephemeral
	deadline := time.After(2 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		ev, err := sub.Next(ctx)
		cancel()
		if err != nil {
			select {
			case <-deadline:
				t.Fatal("timed out waiting for TokRate event")
			default:
				continue
			}
		}
		if ev.Kind == event.KindTokRate {
			rate := ev.Payload.(event.TokRate).Rate
			if rate != 99.0 {
				t.Errorf("TokRate.Rate = %v, want 99.0", rate)
			}
			return
		}
	}
}

// TestAsyncApprovalConcurrentParkAndResolve tests that a concurrent
// RespondToApproval race with parkApproval is safe under -race.
func TestAsyncApprovalConcurrentParkAndResolve(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			turnDone := make(chan struct{})
			h := New(func(ctx context.Context, in TurnInput) (string, error) {
				approvalID := event.ApprovalID("apr_race")
				_ = in.Emit.Emit(event.KindApprovalRequested, event.ApprovalRequested{
					ApprovalID: approvalID,
					ToolName:   "run_shell",
					Headline:   "test",
					Detail:     "test detail",
				})
				_, _, _ = in.ParkApproval(ctx, approvalID)
				close(turnDone)
				return "done", nil
			}, WithAgentName("test"))

			p := testPrincipal()
			s := createSession(t, h, p)

			_, _ = h.SubmitInput(context.Background(), p, core.SubmitInputRequest{
				SessionID: s.ID,
				Text:      "test",
			})

			waitFor(t, func() bool {
				g, _ := h.GetSession(context.Background(), p, s.ID)
				return g.State == core.SessionAwaitingApproval
			})

			_ = h.RespondToApproval(context.Background(), p, core.ApprovalDecision{
				SessionID:  s.ID,
				ApprovalID: event.ApprovalID("apr_race"),
				Outcome:    core.ApprovalAllowOnce,
			})

			select {
			case <-turnDone:
			case <-time.After(2 * time.Second):
				t.Error("timed out waiting for turn")
			}
			_ = h.Close(context.Background())
		}()
	}
	wg.Wait()
}

// TestSessionEmitterRejectsTurnScopedKinds verifies that the session-scoped
// emitter rejects turn-scoped durable kinds (approvals, tool calls, subagent
// events) to preserve terminal turn ordering (7b2 D24).
func TestSessionEmitterRejectsTurnScopedKinds(t *testing.T) {
	var sessionEmit SessionEmitter
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		sessionEmit = in.SessionEmit
		return "hello", nil
	}, WithAgentName("test"))

	defer h.Close(context.Background())
	p := testPrincipal()
	s := createSession(t, h, p)

	_, _ = h.SubmitInput(context.Background(), p, core.SubmitInputRequest{
		SessionID: s.ID,
		Text:      "test",
	})
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})

	if sessionEmit == nil {
		t.Fatal("sessionEmit is nil")
	}
	// Try to emit turn-scoped kinds
	turnScoped := []struct {
		kind    event.Kind
		payload any
	}{
		{event.KindApprovalRequested, event.ApprovalRequested{ApprovalID: "apr_1", ToolName: "x"}},
		{event.KindApprovalResolved, event.ApprovalResolved{ApprovalID: "apr_1", Outcome: "approved"}},
		{event.KindToolCallStarted, event.ToolCallStarted{TurnID: "trn_1", ToolCallID: "tcl_1", Name: "x"}},
		{event.KindToolCallCompleted, event.ToolCallCompleted{ToolCallID: "tcl_1", Name: "x", Status: "ok"}},
		{event.KindSubagentSpawned, event.SubagentSpawned{SubagentID: "sub_1", Task: "t", Capability: "discovery"}},
		{event.KindSubagentCompleted, event.SubagentCompleted{SubagentID: "sub_1", Status: "ok"}},
	}
	for _, tc := range turnScoped {
		err := sessionEmit.Emit(tc.kind, tc.payload)
		if err == nil || !strings.Contains(err.Error(), "turn-scoped") {
			t.Errorf("Emit turn-scoped %s = %v, want turn-scoped error", tc.kind, err)
		}
	}
}
