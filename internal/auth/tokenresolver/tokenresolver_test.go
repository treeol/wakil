package tokenresolver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/auth"
	"github.com/treeol/wakil/internal/auth/jointoken"
	"github.com/treeol/wakil/internal/auth/tokenstore"
	"github.com/treeol/wakil/internal/core"
)

// TestResolveNoCookie verifies that the resolver returns ErrCredentialAbsent
// when no cookie is present.
func TestResolveNoCookie(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	resolver := New(store)

	ctx := context.Background()
	// No headers in context.
	_, err := resolver.Resolve(ctx)
	if !isCredentialAbsent(err) {
		t.Errorf("expected ErrCredentialAbsent, got %v", err)
	}

	// Headers but no cookie.
	ctx = withHeaders(ctx, http.Header{})
	_, err = resolver.Resolve(ctx)
	if !isCredentialAbsent(err) {
		t.Errorf("expected ErrCredentialAbsent, got %v", err)
	}
}

// TestResolveValidCookie verifies that a valid session cookie resolves to
// the correct principal with the current role.
func TestResolveValidCookie(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := jointoken.New(store)
	resolver := New(store)
	ctx := context.Background()

	// Create and exchange a token to get a session.
	result, err := issuer.Create(ctx, jointoken.CreateRequest{
		TenantID:    "tnt_local",
		Role:        "member",
		UserID:      "",
		Email:       "alice@example.com",
		DisplayName: "Alice",
		CreatedBy:   "usr_local",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	exResult, err := issuer.Exchange(ctx, jointoken.ExchangeRequest{Token: result.Token, Email: "alice@example.com", DisplayName: "Alice"})
	headers := http.Header{}
	headers.Set("Cookie", CookieName+"="+exResult.SessionCookie)
	ctx = withHeaders(ctx, headers)

	// Resolve.
	p, err := resolver.Resolve(ctx)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if p.TenantID != "tnt_local" {
		t.Errorf("tenant = %q, want tnt_local", p.TenantID)
	}
	if p.Role != core.RoleMember {
		t.Errorf("role = %q, want member", p.Role)
	}
	if p.AuthMethod != core.AuthSession {
		t.Errorf("auth_method = %q, want session", p.AuthMethod)
	}
}

// TestResolveInvalidCookie verifies that an invalid cookie returns
// ErrInvalidCredential (hard fail, not ErrCredentialAbsent).
func TestResolveInvalidCookie(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	resolver := New(store)

	// Build a context with a bogus cookie.
	headers := http.Header{}
	headers.Set("Cookie", CookieName+"=wst_bogus_invalid_value")
	ctx := withHeaders(context.Background(), headers)

	_, err := resolver.Resolve(ctx)
	if !isInvalidCredential(err) {
		t.Errorf("expected ErrInvalidCredential for bogus cookie, got %v", err)
	}
}

// TestResolveRevokedSession verifies that a revoked session returns
// ErrInvalidCredential.
func TestResolveRevokedSession(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := jointoken.New(store)
	resolver := New(store)
	ctx := context.Background()

	result, err := issuer.Create(ctx, jointoken.CreateRequest{
		TenantID:    "tnt_local",
		Role:        "member",
		UserID:      "",
		Email:       "bob@example.com",
		DisplayName: "Bob",
		CreatedBy:   "usr_local",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	exResult, err := issuer.Exchange(ctx, jointoken.ExchangeRequest{Token: result.Token, Email: "bob@example.com", DisplayName: "Bob"})
	tokenHash := jointoken.HashToken(exResult.SessionCookie)
	if err := store.RevokeWebSessionByHash(ctx, tokenHash); err != nil {
		t.Fatalf("revoke session: %v", err)
	}

	// Try to resolve.
	headers := http.Header{}
	headers.Set("Cookie", CookieName+"="+exResult.SessionCookie)
	ctx = withHeaders(ctx, headers)

	_, err = resolver.Resolve(ctx)
	if !isInvalidCredential(err) {
		t.Errorf("expected ErrInvalidCredential for revoked session, got %v", err)
	}
}

// TestReadSessionCookie verifies the cookie parsing.
func TestReadSessionCookie(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{"wakild_session=abc123", "abc123"},
		{"wakild_session=abc123; other=val", "abc123"},
		{"other=val; wakild_session=xyz", "xyz"},
		{"no_cookie_here=1", ""},
		{"", ""},
	}
	for _, tt := range tests {
		h := http.Header{}
		if tt.header != "" {
			h.Set("Cookie", tt.header)
		}
		got := readSessionCookie(h)
		if got != tt.want {
			t.Errorf("readSessionCookie(%q) = %q, want %q", tt.header, got, tt.want)
		}
	}
}

// --- Test helpers ---

func withHeaders(ctx context.Context, h http.Header) context.Context {
	return auth.WithHTTPHeaders(ctx, h)
}

func isCredentialAbsent(err error) bool {
	return err == auth.ErrCredentialAbsent
}

func isInvalidCredential(err error) bool {
	return err == auth.ErrInvalidCredential || strings.Contains(err.Error(), "invalid credential")
}

// setupTestStore is shared with the jointoken package's test setup.
// We define it here because test files can't import from other packages'
// test helpers.
func setupTestStore(t *testing.T) (*tokenstore.Store, func()) {
	t.Helper()
	return setupTestStoreImpl(t)
}
