package tokenresolver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/auth"
	"github.com/treeol/wakil/internal/auth/apitoken"
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
		{"wakil_session=abc123", "abc123"},
		{"wakil_session=abc123; other=val", "abc123"},
		{"other=val; wakil_session=xyz", "xyz"},
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

// --- API token resolver tests (P4d) ---

// TestResolveNoBearerToken verifies that the API token resolver returns
// ErrCredentialAbsent when no Bearer token is present.
func TestResolveNoBearerToken(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	resolver := NewAPIResolver(store)

	ctx := context.Background()
	// No headers in context.
	_, err := resolver.Resolve(ctx)
	if !isCredentialAbsent(err) {
		t.Errorf("expected ErrCredentialAbsent, got %v", err)
	}

	// Headers but no Authorization header.
	ctx = withHeaders(ctx, http.Header{})
	_, err = resolver.Resolve(ctx)
	if !isCredentialAbsent(err) {
		t.Errorf("expected ErrCredentialAbsent, got %v", err)
	}

	// Authorization header but not Bearer.
	h := http.Header{}
	h.Set("Authorization", "Basic dXNlcjpwYXNz")
	ctx = withHeaders(ctx, h)
	_, err = resolver.Resolve(ctx)
	if !isCredentialAbsent(err) {
		t.Errorf("expected ErrCredentialAbsent for non-Bearer, got %v", err)
	}
}

// TestResolveValidBearerToken verifies that a valid API token resolves to
// the correct principal with the current role and scopes.
func TestResolveValidBearerToken(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := apitoken.New(store)
	resolver := NewAPIResolver(store)
	ctx := context.Background()

	// Create an API token for the seeded local owner.
	result, err := issuer.Create(ctx, apitoken.CreateRequest{
		TenantID: "tnt_local",
		UserID:   "usr_local",
		Name:     "test token",
		Scopes:   []string{"sessions:read", "sessions:write"},
	})
	if err != nil {
		t.Fatalf("create api token: %v", err)
	}

	// Build a context with the Bearer token.
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+result.Token)
	ctx = withHeaders(ctx, headers)

	// Resolve.
	p, err := resolver.Resolve(ctx)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if p.TenantID != "tnt_local" {
		t.Errorf("tenant = %q, want tnt_local", p.TenantID)
	}
	if p.UserID != "usr_local" {
		t.Errorf("user = %q, want usr_local", p.UserID)
	}
	if p.Role != core.RoleOwner {
		t.Errorf("role = %q, want owner", p.Role)
	}
	if p.AuthMethod != core.AuthAPIToken {
		t.Errorf("auth_method = %q, want api_token", p.AuthMethod)
	}
	if len(p.Scopes) != 2 {
		t.Fatalf("len(scopes) = %d, want 2", len(p.Scopes))
	}
	if p.Scopes[0] != "sessions:read" || p.Scopes[1] != "sessions:write" {
		t.Errorf("scopes = %v, want [sessions:read, sessions:write]", p.Scopes)
	}
}

// TestResolveInvalidBearerToken verifies that an invalid Bearer token returns
// ErrInvalidCredential (hard fail).
func TestResolveInvalidBearerToken(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	resolver := NewAPIResolver(store)

	headers := http.Header{}
	headers.Set("Authorization", "Bearer tok_bogus_invalid_value")
	ctx := withHeaders(context.Background(), headers)

	_, err := resolver.Resolve(ctx)
	if !isInvalidCredential(err) {
		t.Errorf("expected ErrInvalidCredential for bogus token, got %v", err)
	}
}

// TestResolveRevokedAPIToken verifies that a revoked API token returns
// ErrInvalidCredential.
func TestResolveRevokedAPIToken(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := apitoken.New(store)
	resolver := NewAPIResolver(store)
	ctx := context.Background()

	result, err := issuer.Create(ctx, apitoken.CreateRequest{
		TenantID: "tnt_local",
		UserID:   "usr_local",
		Name:     "will revoke",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := issuer.Revoke(ctx, result.ID, "tnt_local"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+result.Token)
	ctx = withHeaders(ctx, headers)

	_, err = resolver.Resolve(ctx)
	if !isInvalidCredential(err) {
		t.Errorf("expected ErrInvalidCredential for revoked token, got %v", err)
	}
}

// TestReadBearerToken verifies Bearer token extraction from the
// Authorization header.
func TestReadBearerToken(t *testing.T) {
	tests := []struct {
		header string
		want   string
	}{
		{"Bearer tok_abc123", "tok_abc123"},
		{"Bearer  tok_abc123", "tok_abc123"}, // double space after Bearer
		{"bearer tok_abc123", ""},            // lowercase — not a Bearer token
		{"Basic dXNlcjpwYXNz", ""},
		{"", ""},
	}
	for _, tt := range tests {
		h := http.Header{}
		if tt.header != "" {
			h.Set("Authorization", tt.header)
		}
		got := readBearerToken(h)
		if got != tt.want {
			t.Errorf("readBearerToken(%q) = %q, want %q", tt.header, got, tt.want)
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
