package auth

import (
	"context"
	"errors"
	"testing"

	"github.com/treeol/wakil/internal/auth/peercred"
	"github.com/treeol/wakil/internal/core"
)

func TestLocalResolver_OwnerUID(t *testing.T) {
	r := NewLocalResolverWithUID(1000)
	ctx := WithPeerCredentials(context.Background(), peercred.Credentials{UID: 1000, GID: 1000, PID: 1234})

	p, err := r.Resolve(ctx)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.TenantID != "tnt_local" {
		t.Errorf("TenantID = %q, want tnt_local", p.TenantID)
	}
	if p.UserID != "usr_local" {
		t.Errorf("UserID = %q, want usr_local", p.UserID)
	}
	if p.Role != core.RoleOwner {
		t.Errorf("Role = %q, want owner", p.Role)
	}
	if p.AuthMethod != core.AuthLocal {
		t.Errorf("AuthMethod = %q, want %q", p.AuthMethod, core.AuthLocal)
	}
}

func TestLocalResolver_WrongUID(t *testing.T) {
	r := NewLocalResolverWithUID(1000)
	ctx := WithPeerCredentials(context.Background(), peercred.Credentials{UID: 1001, GID: 1001, PID: 5678})

	_, err := r.Resolve(ctx)
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
}

func TestLocalResolver_NoCredentials(t *testing.T) {
	r := NewLocalResolverWithUID(1000)

	// No peer credentials in context → fail closed.
	_, err := r.Resolve(context.Background())
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated, got %v", err)
	}
}

func TestLocalResolver_RootRejected(t *testing.T) {
	// Root (UID 0) must NOT be implicitly accepted. Only the daemon owner UID
	// is allowed.
	r := NewLocalResolverWithUID(1000)
	ctx := WithPeerCredentials(context.Background(), peercred.Credentials{UID: 0, GID: 0, PID: 0})

	_, err := r.Resolve(ctx)
	if !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("expected ErrUnauthenticated for root, got %v", err)
	}
}
