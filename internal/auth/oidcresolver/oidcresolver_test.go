package oidcresolver

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/treeol/wakil/internal/auth"
	"github.com/treeol/wakil/internal/auth/tokenstore"
	"github.com/treeol/wakil/internal/core"
)

// mockValidator is a test TokenValidator that returns preset claims or error.
type mockValidator struct {
	claims *Claims
	err    error
}

func (m *mockValidator) Validate(ctx context.Context, rawToken string) (*Claims, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.claims, nil
}

// setupTestStore creates a file-based SQLite DB with migrations.
func setupTestStore(t *testing.T) *tokenstore.Store {
	t.Helper()
	dir := t.TempDir()
	dbPath := dir + "/oidc_test.db"
	ts, err := tokenstore.OpenTestDB(dbPath)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { ts.Close() })
	return ts
}

// withHeaders creates a context with HTTP headers for the resolver.
func withHeaders(headers http.Header) context.Context {
	return auth.WithHTTPHeaders(context.Background(), headers)
}

// TestOIDCResolverDisabled tests that when no validator is configured, the
// resolver returns ErrCredentialAbsent (disabled, not an error).
func TestOIDCResolverDisabled(t *testing.T) {
	store := setupTestStore(t)
	// No validator, no issuer — disabled.
	r := New(store, nil, Config{})

	p, err := r.Resolve(context.Background())
	if !errors.Is(err, auth.ErrCredentialAbsent) {
		t.Errorf("error = %v, want ErrCredentialAbsent", err)
	}
	if p.AuthMethod != "" {
		t.Errorf("AuthMethod = %q, want empty", p.AuthMethod)
	}
}

// TestOIDCResolverDisabledValidatorOnly tests that a non-nil validator with
// an empty Issuer is also treated as disabled (IsConfigured consistency).
func TestOIDCResolverDisabledValidatorOnly(t *testing.T) {
	store := setupTestStore(t)
	validator := &mockValidator{claims: &Claims{Subject: "test-sub"}}
	// Validator set, but no issuer — still disabled.
	r := New(store, validator, Config{})

	_, err := r.Resolve(context.Background())
	if !errors.Is(err, auth.ErrCredentialAbsent) {
		t.Errorf("error = %v, want ErrCredentialAbsent (issuer empty = disabled)", err)
	}
}

// TestOIDCResolverNoHeaders tests that the resolver returns
// ErrCredentialAbsent when no HTTP headers are in the context.
func TestOIDCResolverNoHeaders(t *testing.T) {
	store := setupTestStore(t)
	validator := &mockValidator{claims: &Claims{Subject: "test-sub"}}
	r := New(store, validator, Config{Issuer: "https://test.issuer"})

	_, err := r.Resolve(context.Background())
	if !errors.Is(err, auth.ErrCredentialAbsent) {
		t.Errorf("error = %v, want ErrCredentialAbsent", err)
	}
}

// TestOIDCResolverNoBearerToken tests that the resolver returns
// ErrCredentialAbsent when no Bearer token is present.
func TestOIDCResolverNoBearerToken(t *testing.T) {
	store := setupTestStore(t)
	validator := &mockValidator{claims: &Claims{Subject: "test-sub"}}
	r := New(store, validator, Config{Issuer: "https://test.issuer"})

	headers := http.Header{}
	ctx := withHeaders(headers)

	_, err := r.Resolve(ctx)
	if !errors.Is(err, auth.ErrCredentialAbsent) {
		t.Errorf("error = %v, want ErrCredentialAbsent", err)
	}
}

// TestOIDCResolverAPITokenIgnored tests that Bearer tokens with the "tok_"
// prefix are ignored (they are API tokens, not JWTs).
func TestOIDCResolverAPITokenIgnored(t *testing.T) {
	store := setupTestStore(t)
	validator := &mockValidator{claims: &Claims{Subject: "test-sub"}}
	r := New(store, validator, Config{Issuer: "https://test.issuer"})

	headers := http.Header{}
	headers.Set("Authorization", "Bearer tok_abc123")
	ctx := withHeaders(headers)

	_, err := r.Resolve(ctx)
	if !errors.Is(err, auth.ErrCredentialAbsent) {
		t.Errorf("error = %v, want ErrCredentialAbsent (tok_ prefix should be ignored)", err)
	}
}

// TestOIDCResolverInvalidToken tests that an invalid JWT returns
// ErrInvalidCredential (hard fail, no fallthrough).
func TestOIDCResolverInvalidToken(t *testing.T) {
	store := setupTestStore(t)
	validator := &mockValidator{err: errors.New("invalid jwt")}
	r := New(store, validator, Config{Issuer: "https://test.issuer"})

	headers := http.Header{}
	headers.Set("Authorization", "Bearer invalid.jwt.token")
	ctx := withHeaders(headers)

	_, err := r.Resolve(ctx)
	if !errors.Is(err, auth.ErrInvalidCredential) {
		t.Errorf("error = %v, want ErrInvalidCredential", err)
	}
}

// TestOIDCResolverEmptySubject tests that a JWT with an empty `sub` claim
// returns ErrInvalidCredential.
func TestOIDCResolverEmptySubject(t *testing.T) {
	store := setupTestStore(t)
	validator := &mockValidator{claims: &Claims{Subject: ""}}
	r := New(store, validator, Config{Issuer: "https://test.issuer"})

	headers := http.Header{}
	headers.Set("Authorization", "Bearer some.jwt.token")
	ctx := withHeaders(headers)

	_, err := r.Resolve(ctx)
	if !errors.Is(err, auth.ErrInvalidCredential) {
		t.Errorf("error = %v, want ErrInvalidCredential", err)
	}
}

// TestOIDCResolverUserNotFoundNoProvision tests that an unknown `sub` with
// auto-provisioning disabled returns ErrInvalidCredential.
func TestOIDCResolverUserNotFoundNoProvision(t *testing.T) {
	store := setupTestStore(t)
	validator := &mockValidator{claims: &Claims{Subject: "unknown-sub"}}
	r := New(store, validator, Config{
		Issuer:          "https://test.issuer",
		AutoProvision:   false,
		DefaultTenantID: "tnt_local",
	})

	headers := http.Header{}
	headers.Set("Authorization", "Bearer some.jwt.token")
	ctx := withHeaders(headers)

	_, err := r.Resolve(ctx)
	if !errors.Is(err, auth.ErrInvalidCredential) {
		t.Errorf("error = %v, want ErrInvalidCredential", err)
	}
}

// TestOIDCResolverAutoProvision tests that an unknown `sub` with
// auto-provisioning enabled creates a new user and resolves the principal.
// The role must always be "member" regardless of config.DefaultRole.
func TestOIDCResolverAutoProvision(t *testing.T) {
	store := setupTestStore(t)
	validator := &mockValidator{claims: &Claims{
		Subject:     "new-oidc-user",
		Email:       "newuser@example.com",
		DisplayName: "New User",
	}}
	r := New(store, validator, Config{
		Issuer:          "https://test.issuer",
		AutoProvision:   true,
		DefaultTenantID: "tnt_local",
		// DefaultRole is deliberately set to "owner" — the resolver must
		// ignore it and always provision as "member".
		DefaultRole: "owner",
	})

	headers := http.Header{}
	headers.Set("Authorization", "Bearer some.jwt.token")
	ctx := withHeaders(headers)

	p, err := r.Resolve(ctx)
	if err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}

	if p.AuthMethod != core.AuthOIDC {
		t.Errorf("AuthMethod = %q, want %q", p.AuthMethod, core.AuthOIDC)
	}
	if string(p.TenantID) != "tnt_local" {
		t.Errorf("TenantID = %q, want tnt_local", p.TenantID)
	}
	if p.Role != core.RoleMember {
		t.Errorf("Role = %q, want member (auto-provision must always be member)", p.Role)
	}
	if string(p.UserID) == "" {
		t.Error("UserID is empty")
	}

	// Verify the user was actually created by looking them up again.
	user, err := store.LookupUserByAuthSubject(context.Background(), "new-oidc-user")
	if err != nil {
		t.Fatalf("lookup user after provisioning: %v", err)
	}
	if user.Email != "newuser@example.com" {
		t.Errorf("user email = %q, want newuser@example.com", user.Email)
	}
}

// TestOIDCResolverAutoProvisionNoTenant tests that auto-provisioning without
// a configured DefaultTenantID fails (no silent fallback to tnt_local).
func TestOIDCResolverAutoProvisionNoTenant(t *testing.T) {
	store := setupTestStore(t)
	validator := &mockValidator{claims: &Claims{
		Subject:     "no-tenant-user",
		Email:       "notenant@example.com",
		DisplayName: "No Tenant User",
	}}
	r := New(store, validator, Config{
		Issuer:        "https://test.issuer",
		AutoProvision: true,
		// No DefaultTenantID — should fail, not fall back to tnt_local.
	})

	headers := http.Header{}
	headers.Set("Authorization", "Bearer some.jwt.token")
	ctx := withHeaders(headers)

	_, err := r.Resolve(ctx)
	if !errors.Is(err, auth.ErrInvalidCredential) {
		t.Errorf("error = %v, want ErrInvalidCredential (no DefaultTenantID)", err)
	}
}

// TestOIDCResolverExistingUser tests that an existing user with a matching
// auth_subject is resolved correctly.
func TestOIDCResolverExistingUser(t *testing.T) {
	store := setupTestStore(t)

	// Manually create a user with an auth_subject.
	// We use the auto-provision path to create the user, then test
	// the existing-user path.
	validator1 := &mockValidator{claims: &Claims{
		Subject:     "existing-user-sub",
		Email:       "existing@example.com",
		DisplayName: "Existing User",
	}}
	r1 := New(store, validator1, Config{
		Issuer:          "https://test.issuer",
		AutoProvision:   true,
		DefaultTenantID: "tnt_local",
	})

	headers := http.Header{}
	headers.Set("Authorization", "Bearer some.jwt.token")
	ctx := withHeaders(headers)

	p1, err := r1.Resolve(ctx)
	if err != nil {
		t.Fatalf("first resolve (provision): %v", err)
	}
	userID := string(p1.UserID)

	// Now resolve again with a validator that returns the same subject.
	validator2 := &mockValidator{claims: &Claims{Subject: "existing-user-sub"}}
	r2 := New(store, validator2, Config{
		Issuer:          "https://test.issuer",
		AutoProvision:   false, // no provisioning this time
		DefaultTenantID: "tnt_local",
	})

	p2, err := r2.Resolve(ctx)
	if err != nil {
		t.Fatalf("second resolve (existing): %v", err)
	}
	if string(p2.UserID) != userID {
		t.Errorf("UserID = %q, want %q (same user)", p2.UserID, userID)
	}
	if p2.AuthMethod != core.AuthOIDC {
		t.Errorf("AuthMethod = %q, want %q", p2.AuthMethod, core.AuthOIDC)
	}
}

// TestOIDCResolverSuspendedUser tests that a suspended user is rejected.
func TestOIDCResolverSuspendedUser(t *testing.T) {
	store := setupTestStore(t)

	// Provision a user first.
	validator := &mockValidator{claims: &Claims{
		Subject:     "suspend-me",
		Email:       "suspend@example.com",
		DisplayName: "Suspend User",
	}}
	r := New(store, validator, Config{
		Issuer:          "https://test.issuer",
		AutoProvision:   true,
		DefaultTenantID: "tnt_local",
	})

	headers := http.Header{}
	headers.Set("Authorization", "Bearer some.jwt.token")
	ctx := withHeaders(headers)

	p, err := r.Resolve(ctx)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}

	// Suspend the user.
	_, err = store.DB().ExecContext(context.Background(),
		"UPDATE users SET status = 'suspended' WHERE id = ?", string(p.UserID))
	if err != nil {
		t.Fatalf("suspend user: %v", err)
	}

	// Resolve again — should fail with ErrInvalidCredential.
	_, err = r.Resolve(ctx)
	if !errors.Is(err, auth.ErrInvalidCredential) {
		t.Errorf("error = %v, want ErrInvalidCredential for suspended user", err)
	}
}

// TestOIDCResolverIsConfigured tests the IsConfigured method.
func TestOIDCResolverIsConfigured(t *testing.T) {
	store := setupTestStore(t)

	// No validator, no issuer — not configured.
	r1 := New(store, nil, Config{})
	if r1.IsConfigured() {
		t.Error("IsConfigured() = true, want false (no validator)")
	}

	// Validator set but no issuer — not configured.
	r2 := New(store, &mockValidator{}, Config{})
	if r2.IsConfigured() {
		t.Error("IsConfigured() = true, want false (no issuer)")
	}

	// Both set — configured.
	r3 := New(store, &mockValidator{}, Config{Issuer: "https://test.issuer"})
	if !r3.IsConfigured() {
		t.Error("IsConfigured() = false, want true")
	}
}
