package core

import (
	"testing"

	"github.com/treeol/wakil/internal/core/event"
)

func TestPrincipalValidate(t *testing.T) {
	// A well-formed principal validates.
	if err := EmbeddedPrincipal().Validate(); err != nil {
		t.Fatalf("embedded principal should validate: %v", err)
	}

	// Missing tenant is rejected — a principal without a tenant cannot be
	// tenant-keyed and must never reach a store.
	p := Principal{UserID: event.EmbeddedUserID, Role: RoleOwner, AuthMethod: AuthEmbedded}
	if err := p.Validate(); err == nil {
		t.Fatal("principal with empty tenant should be rejected")
	}

	// Missing user is rejected for the same reason.
	p = Principal{TenantID: event.EmbeddedTenantID, Role: RoleOwner, AuthMethod: AuthEmbedded}
	if err := p.Validate(); err == nil {
		t.Fatal("principal with empty user should be rejected")
	}

	// Wrong-prefix tenant is rejected by the typed ID validation.
	p = Principal{TenantID: "ses_wrong", UserID: event.EmbeddedUserID}
	if err := p.Validate(); err == nil {
		t.Fatal("principal with mis-prefixed tenant should be rejected")
	}
}

func TestEmbeddedPrincipalConstants(t *testing.T) {
	p := EmbeddedPrincipal()
	if err := p.TenantID.Validate(); err != nil {
		t.Fatalf("embedded tenant invalid: %v", err)
	}
	if err := p.UserID.Validate(); err != nil {
		t.Fatalf("embedded user invalid: %v", err)
	}
	if p.Role != RoleOwner {
		t.Fatalf("embedded role = %q, want owner", p.Role)
	}
	if p.AuthMethod != AuthEmbedded {
		t.Fatalf("embedded auth method = %q, want embedded", p.AuthMethod)
	}

	// Mutating one returned value must not affect the next.
	p.Scopes = []string{"tainted"}
	again := EmbeddedPrincipal()
	if len(again.Scopes) != 0 {
		t.Fatalf("EmbeddedPrincipal returned shared state: scopes leaked across calls")
	}
}

func TestStateMachineTransitions(t *testing.T) {
	legal := map[SessionState][]SessionState{
		SessionIdle:             {SessionRunning, SessionClosed},
		SessionRunning:          {SessionIdle, SessionAwaitingApproval, SessionError, SessionClosed},
		SessionAwaitingApproval: {SessionRunning, SessionError, SessionClosed},
		SessionError:            {SessionRunning, SessionClosed},
		SessionClosed:           {},
	}

	all := []SessionState{
		SessionIdle, SessionRunning, SessionAwaitingApproval, SessionError, SessionClosed,
	}

	for _, from := range all {
		allowed := map[SessionState]bool{}
		for _, to := range legal[from] {
			allowed[to] = true
			if !from.CanTransitionTo(to) {
				t.Errorf("%s -> %s should be legal but CanTransitionTo returned false", from, to)
			}
		}
		for _, to := range all {
			if !allowed[to] && from.CanTransitionTo(to) {
				t.Errorf("%s -> %s should be illegal but CanTransitionTo returned true", from, to)
			}
		}
	}
}

func TestApprovalDecisionValidate(t *testing.T) {
	valid := ApprovalDecision{
		SessionID:  "ses_1",
		ApprovalID: "apr_1",
		Outcome:    ApprovalAllowOnce,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid decision rejected: %v", err)
	}

	bad := valid
	bad.Outcome = "bogus"
	if err := bad.Validate(); err == nil {
		t.Fatal("invalid outcome should be rejected")
	}

	bad = valid
	bad.SessionID = "apr_wrong" // mis-prefixed session id
	if err := bad.Validate(); err == nil {
		t.Fatal("mis-prefixed session id should be rejected")
	}

	// The enum is closed — the two illegal bool-pair states from the old model
	// are simply unrepresentable.
	if ApprovalDeny == "" || ApprovalAllowOnce == "" || ApprovalAllowReadsOnce == "" {
		t.Fatal("approval outcome constants must be non-empty")
	}
}

func TestSentinelErrorsAreDistinct(t *testing.T) {
	errs := []error{
		ErrSessionNotFound, ErrSessionClosed, ErrSessionBusy,
		ErrNotAuthorized, ErrInvalidInput, ErrInvalidStateTransition,
		ErrApprovalNotFound, ErrApprovalAlreadyResolved, ErrSubscriptionClosed,
	}
	seen := map[string]bool{}
	for _, e := range errs {
		if e == nil {
			t.Fatal("nil sentinel error")
		}
		if seen[e.Error()] {
			t.Fatalf("duplicate sentinel error %q", e)
		}
		seen[e.Error()] = true
	}
}
