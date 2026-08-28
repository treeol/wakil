package sessionhost_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionhost"
)

// TestCrossTenantIsolation verifies that a principal from tenant B cannot
// access, read, or subscribe to a session created by tenant A. The host layer
// enforces tenant isolation via h.lookup (which checks s.tenant ==
// principal.TenantID and returns ErrSessionNotFound for mismatches — no
// existence leak). This test runs against MemLog (the default store) because it
// tests host-level authz, not store-level behavior.
func TestCrossTenantIsolation(t *testing.T) {
	// Use a simple turn function that just echoes the input.
	h := sessionhost.New(func(_ context.Context, in sessionhost.TurnInput) (string, error) {
		return in.Text, nil
	})
	defer h.Close(context.Background())

	ctx := context.Background()

	// Two principals in different tenants.
	tenantA := core.Principal{
		TenantID:   event.TenantID("tnt_a"),
		UserID:     event.UserID("usr_a"),
		Role:       core.RoleOwner,
		AuthMethod: "embedded",
	}
	tenantB := core.Principal{
		TenantID:   event.TenantID("tnt_b"),
		UserID:     event.UserID("usr_b"),
		Role:       core.RoleOwner,
		AuthMethod: "embedded",
	}

	// Tenant A creates a session.
	sessionA, err := h.CreateSession(ctx, tenantA, core.CreateSessionRequest{
		Workspace: event.WorkspaceID("wsp_test"),
		Title:     "A's session",
	})
	if err != nil {
		t.Fatalf("CreateSession A: %v", err)
	}

	// Submit a turn to generate events.
	_, err = h.SubmitInput(ctx, tenantA, core.SubmitInputRequest{
		SessionID: sessionA.ID,
		Text:      "hello from A",
	})
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	// Wait for the turn to complete (poll ListEvents until we see TurnCompleted).
	waitForTurnComplete(t, h, tenantA, sessionA.ID)

	// Tenant B tries ListEvents → ErrSessionNotFound.
	_, err = h.ListEvents(ctx, tenantB, sessionA.ID, 0, 0)
	if !errors.Is(err, core.ErrSessionNotFound) {
		t.Fatalf("ListEvents cross-tenant: expected ErrSessionNotFound, got %v", err)
	}

	// Tenant B tries GetSession → ErrSessionNotFound.
	_, err = h.GetSession(ctx, tenantB, sessionA.ID)
	if !errors.Is(err, core.ErrSessionNotFound) {
		t.Fatalf("GetSession cross-tenant: expected ErrSessionNotFound, got %v", err)
	}

	// Tenant B tries Subscribe → ErrSessionNotFound.
	_, err = h.Subscribe(ctx, tenantB, sessionA.ID, 0)
	if !errors.Is(err, core.ErrSessionNotFound) {
		t.Fatalf("Subscribe cross-tenant: expected ErrSessionNotFound, got %v", err)
	}

	// Tenant B tries SessionSnapshot → ErrSessionNotFound.
	_, err = h.SessionSnapshot(ctx, tenantB, sessionA.ID)
	if !errors.Is(err, core.ErrSessionNotFound) {
		t.Fatalf("SessionSnapshot cross-tenant: expected ErrSessionNotFound, got %v", err)
	}

	// Tenant B tries SubmitInput → ErrSessionNotFound.
	_, err = h.SubmitInput(ctx, tenantB, core.SubmitInputRequest{
		SessionID: sessionA.ID,
		Text:      "hello from B",
	})
	if !errors.Is(err, core.ErrSessionNotFound) {
		t.Fatalf("SubmitInput cross-tenant: expected ErrSessionNotFound, got %v", err)
	}

	// Tenant B tries CloseSession → ErrSessionNotFound.
	err = h.CloseSession(ctx, tenantB, sessionA.ID)
	if !errors.Is(err, core.ErrSessionNotFound) {
		t.Fatalf("CloseSession cross-tenant: expected ErrSessionNotFound, got %v", err)
	}

	// Tenant B tries Interrupt → ErrSessionNotFound.
	err = h.Interrupt(ctx, tenantB, sessionA.ID)
	if !errors.Is(err, core.ErrSessionNotFound) {
		t.Fatalf("Interrupt cross-tenant: expected ErrSessionNotFound, got %v", err)
	}

	// Tenant B tries RespondToApproval → ErrSessionNotFound.
	err = h.RespondToApproval(ctx, tenantB, core.ApprovalDecision{
		SessionID:  sessionA.ID,
		ApprovalID: event.ApprovalID("apr_bogus"),
		Outcome:    core.ApprovalDeny,
	})
	if !errors.Is(err, core.ErrSessionNotFound) {
		t.Fatalf("RespondToApproval cross-tenant: expected ErrSessionNotFound, got %v", err)
	}

	// Tenant B's ListSessions should not include tenant A's session.
	sessions, err := h.ListSessions(ctx, tenantB)
	if err != nil {
		t.Fatalf("ListSessions B: %v", err)
	}
	for _, s := range sessions {
		if s.ID == sessionA.ID {
			t.Fatal("ListSessions B: tenant A's session leaked into tenant B's list")
		}
	}

	// Verify tenant A can still access its own session (control: not broken by B's attempts).
	_, err = h.GetSession(ctx, tenantA, sessionA.ID)
	if err != nil {
		t.Fatalf("GetSession A (control): %v", err)
	}
}

// waitForTurnComplete polls ListEvents until it sees a TurnCompleted event for
// the session, or times out after 2 seconds.
func waitForTurnComplete(t *testing.T, h *sessionhost.Host, principal core.Principal, sid event.SessionID) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		events, err := h.ListEvents(ctx, principal, sid, 0, 0)
		if err != nil {
			t.Fatalf("waitForTurnComplete: ListEvents: %v", err)
		}
		for _, e := range events {
			if e.Kind == event.KindTurnCompleted {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("waitForTurnComplete: timed out waiting for TurnCompleted")
}
