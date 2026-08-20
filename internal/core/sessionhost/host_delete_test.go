package sessionhost

import (
	"context"
	"errors"
	"testing"

	"github.com/treeol/wakil/internal/core"
)

func TestDeleteSessionExcludesFromQueries(t *testing.T) {
	h, p := testEnv()
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	// Delete the session.
	if err := h.DeleteSession(context.Background(), p, s.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	// GetSession returns ErrSessionNotFound for deleted sessions.
	_, err := h.GetSession(context.Background(), p, s.ID)
	if !errors.Is(err, core.ErrSessionNotFound) {
		t.Fatalf("GetSession after delete: err = %v, want ErrSessionNotFound", err)
	}

	// ListSessions excludes deleted sessions.
	sessions, err := h.ListSessions(context.Background(), p)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	for _, sess := range sessions {
		if sess.ID == s.ID {
			t.Fatal("ListSessions includes deleted session")
		}
	}
}

func TestDeleteSessionRejectsFurtherOperations(t *testing.T) {
	h, p := testEnv()
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	if err := h.DeleteSession(context.Background(), p, s.ID); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	// SubmitInput to a deleted session returns ErrSessionNotFound.
	_, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "hello"})
	if !errors.Is(err, core.ErrSessionNotFound) {
		t.Fatalf("SubmitInput after delete: err = %v, want ErrSessionNotFound", err)
	}

	// Interrupt returns ErrSessionNotFound.
	err = h.Interrupt(context.Background(), p, s.ID)
	if !errors.Is(err, core.ErrSessionNotFound) {
		t.Fatalf("Interrupt after delete: err = %v, want ErrSessionNotFound", err)
	}

	// CloseSession returns ErrSessionNotFound (already deleted).
	err = h.CloseSession(context.Background(), p, s.ID)
	if !errors.Is(err, core.ErrSessionNotFound) {
		t.Fatalf("CloseSession after delete: err = %v, want ErrSessionNotFound", err)
	}

	// Second DeleteSession returns ErrSessionNotFound.
	err = h.DeleteSession(context.Background(), p, s.ID)
	if !errors.Is(err, core.ErrSessionNotFound) {
		t.Fatalf("double DeleteSession: err = %v, want ErrSessionNotFound", err)
	}
}

func TestDeleteSessionClosesActiveSession(t *testing.T) {
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	// Start a turn that blocks until cancelled.
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "block"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	// Wait for the turn to start.
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionRunning
	})

	// Delete while the turn is running.
	if err := h.DeleteSession(context.Background(), p, s.ID); err != nil {
		t.Fatalf("DeleteSession during turn: %v", err)
	}

	// The session is no longer visible.
	_, err := h.GetSession(context.Background(), p, s.ID)
	if !errors.Is(err, core.ErrSessionNotFound) {
		t.Fatalf("GetSession after delete-during-turn: err = %v, want ErrSessionNotFound", err)
	}
}

func TestDeleteSessionCrossTenantNotFound(t *testing.T) {
	h, p := testEnv()
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	tenantB := core.Principal{TenantID: "tnt_other", UserID: "usr_other", Role: core.RoleOwner}

	err := h.DeleteSession(context.Background(), tenantB, s.ID)
	if !errors.Is(err, core.ErrSessionNotFound) {
		t.Fatalf("DeleteSession cross-tenant: err = %v, want ErrSessionNotFound", err)
	}
}

func TestDeleteSessionViewerNotAllowed(t *testing.T) {
	h, p := testEnv()
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	viewer := core.Principal{TenantID: p.TenantID, UserID: "usr_viewer", Role: core.RoleViewer}

	err := h.DeleteSession(context.Background(), viewer, s.ID)
	if !errors.Is(err, core.ErrNotAuthorized) {
		t.Fatalf("DeleteSession by viewer: err = %v, want ErrNotAuthorized", err)
	}

	// Session still exists (delete was rejected).
	g, err := h.GetSession(context.Background(), p, s.ID)
	if err != nil {
		t.Fatalf("GetSession after rejected delete: %v", err)
	}
	if g.State != core.SessionIdle {
		t.Fatalf("state = %q, want idle", g.State)
	}
}
