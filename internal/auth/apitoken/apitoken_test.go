package apitoken

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/auth/tokenstore"
	"github.com/treeol/wakil/internal/store/migrations"

	_ "modernc.org/sqlite"
)

// setupTestStore creates a file-based SQLite DB with all migrations applied.
func setupTestStore(t *testing.T) (*tokenstore.Store, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			t.Fatalf("pragma %q: %v", pragma, err)
		}
	}
	if err := migrations.Apply(context.Background(), db); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	store := tokenstore.New(db)
	return store, func() { db.Close() }
}

// TestCreateAndVerify tests that a token can be created and has the correct
// format.
func TestCreateAndVerify(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	result, err := issuer.Create(ctx, CreateRequest{
		TenantID: "tnt_local",
		UserID:   "usr_local",
		Name:     "CI pipeline",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !strings.HasPrefix(result.Token, "tok_") {
		t.Errorf("token prefix = %q, want tok_", result.Token[:4])
	}
	if result.ID == "" {
		t.Error("token ID is empty")
	}
	if !ValidateTokenFormat(result.Token) {
		t.Error("ValidateTokenFormat should accept tok_ token")
	}
}

// TestCreateMissingName verifies that a token without a name is rejected.
func TestCreateMissingName(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	_, err := issuer.Create(ctx, CreateRequest{
		TenantID: "tnt_local",
		UserID:   "usr_local",
		Name:     "",
	})
	if !errors.Is(err, ErrMissingName) {
		t.Errorf("expected ErrMissingName, got %v", err)
	}
}

// TestCreateMissingIdentity verifies that a token without tenant/user is rejected.
func TestCreateMissingIdentity(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	_, err := issuer.Create(ctx, CreateRequest{
		TenantID: "",
		UserID:   "",
		Name:     "test",
	})
	if !errors.Is(err, ErrMissingIdentity) {
		t.Errorf("expected ErrMissingIdentity, got %v", err)
	}
}

// TestCreateWithScopes verifies that scopes are validated and stored.
func TestCreateWithScopes(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	result, err := issuer.Create(ctx, CreateRequest{
		TenantID: "tnt_local",
		UserID:   "usr_local",
		Name:     "scoped token",
		Scopes:   []string{"sessions:read", "sessions:write"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Verify the token can be looked up and has the right scopes.
	hash := HashToken(result.Token)
	row, err := store.LookupAPIToken(ctx, hash, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	scopes, err := ScopesFromJSON(row.ScopesJSON)
	if err != nil {
		t.Fatalf("ScopesFromJSON: %v", err)
	}
	if len(scopes) != 2 {
		t.Fatalf("len(scopes) = %d, want 2", len(scopes))
	}
	if scopes[0] != "sessions:read" || scopes[1] != "sessions:write" {
		t.Errorf("scopes = %v, want [sessions:read, sessions:write]", scopes)
	}
}

// TestCreateWithInvalidScope verifies that an invalid scope is rejected.
func TestCreateWithInvalidScope(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	_, err := issuer.Create(ctx, CreateRequest{
		TenantID: "tnt_local",
		UserID:   "usr_local",
		Name:     "bad scope",
		Scopes:   []string{"sessions:read", "invalid:scope"},
	})
	if !errors.Is(err, ErrInvalidScope) {
		t.Errorf("expected ErrInvalidScope, got %v", err)
	}
}

// TestCreateWithExpiry verifies that an expiry is stored correctly.
func TestCreateWithExpiry(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	expiresAt := time.Now().Add(30 * 24 * time.Hour) // 30 days
	result, err := issuer.Create(ctx, CreateRequest{
		TenantID:  "tnt_local",
		UserID:    "usr_local",
		Name:      "expiring",
		ExpiresAt: expiresAt,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Verify the token has the expiry set.
	hash := HashToken(result.Token)
	row, err := store.LookupAPIToken(ctx, hash, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if row.ExpiresAt == 0 {
		t.Fatal("expires_at is 0, want non-zero")
	}
	// Verify the expiry is approximately 30 days from now (within 1 second).
	diff := time.Duration(row.ExpiresAt - expiresAt.UnixNano())
	if diff > time.Second || diff < -time.Second {
		t.Errorf("expires_at diff = %v, want < 1s", diff)
	}
}

// TestCreateNoExpiry verifies that a token with no expiry can be looked up.
func TestCreateNoExpiry(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	result, err := issuer.Create(ctx, CreateRequest{
		TenantID: "tnt_local",
		UserID:   "usr_local",
		Name:     "no expiry",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	hash := HashToken(result.Token)
	row, err := store.LookupAPIToken(ctx, hash, time.Now().UnixNano())
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if row.ExpiresAt != 0 {
		t.Errorf("expires_at = %d, want 0 (no expiry)", row.ExpiresAt)
	}
}

// TestListTokens verifies that listing returns the correct tokens.
func TestListTokens(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		_, err := issuer.Create(ctx, CreateRequest{
			TenantID: "tnt_local",
			UserID:   "usr_local",
			Name:     "token",
		})
		if err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	tokens, err := issuer.List(ctx, "tnt_local", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tokens) != 3 {
		t.Errorf("len(tokens) = %d, want 3", len(tokens))
	}

	// List with user filter.
	tokens, err = issuer.List(ctx, "tnt_local", "usr_local")
	if err != nil {
		t.Fatalf("list by user: %v", err)
	}
	if len(tokens) != 3 {
		t.Errorf("len(tokens) = %d, want 3", len(tokens))
	}
}

// TestRevokeToken verifies that revoking a token works and is idempotent.
func TestRevokeToken(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	result, err := issuer.Create(ctx, CreateRequest{
		TenantID: "tnt_local",
		UserID:   "usr_local",
		Name:     "to revoke",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Revoke.
	if err := issuer.Revoke(ctx, result.ID, "tnt_local"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// Revoked token should not be listable (excluded by default).
	tokens, err := issuer.List(ctx, "tnt_local", "")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, tk := range tokens {
		if tk.ID == result.ID {
			t.Error("revoked token should not appear in default list")
		}
	}

	// Idempotent revoke.
	if err := issuer.Revoke(ctx, result.ID, "tnt_local"); err != nil {
		t.Fatalf("idempotent revoke: %v", err)
	}
}

// TestRevokeNotFound verifies that revoking a non-existent token returns
// ErrAPITokenNotFound.
func TestRevokeNotFound(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	err := issuer.Revoke(ctx, "tok_nonexistent", "tnt_local")
	if !errors.Is(err, tokenstore.ErrAPITokenNotFound) {
		t.Errorf("expected ErrAPITokenNotFound, got %v", err)
	}
}

// TestLookupRevokedToken verifies that a revoked token is not found by lookup.
func TestLookupRevokedToken(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	result, err := issuer.Create(ctx, CreateRequest{
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

	hash := HashToken(result.Token)
	_, err = store.LookupAPIToken(ctx, hash, time.Now().UnixNano())
	if !errors.Is(err, tokenstore.ErrAPITokenInvalid) {
		t.Errorf("expected ErrAPITokenInvalid for revoked token, got %v", err)
	}
}

// TestLookupExpiredToken verifies that an expired token is not found by lookup.
func TestLookupExpiredToken(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	// Create a token with a past expiry.
	result, err := issuer.Create(ctx, CreateRequest{
		TenantID:  "tnt_local",
		UserID:    "usr_local",
		Name:      "expired",
		ExpiresAt: time.Now().Add(-1 * time.Hour), // expired 1h ago
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	hash := HashToken(result.Token)
	_, err = store.LookupAPIToken(ctx, hash, time.Now().UnixNano())
	if !errors.Is(err, tokenstore.ErrAPITokenInvalid) {
		t.Errorf("expected ErrAPITokenInvalid for expired token, got %v", err)
	}
}

// TestLookupNonexistentToken verifies that a non-existent hash returns
// ErrAPITokenInvalid.
func TestLookupNonexistentToken(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	ctx := context.Background()

	_, err := store.LookupAPIToken(ctx, "nonexistent_hash", time.Now().UnixNano())
	if !errors.Is(err, tokenstore.ErrAPITokenInvalid) {
		t.Errorf("expected ErrAPITokenInvalid, got %v", err)
	}
}

// TestTokenFormat verifies generated tokens have the correct prefix and
// ValidateTokenFormat works correctly.
func TestTokenFormat(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	result, err := issuer.Create(ctx, CreateRequest{
		TenantID: "tnt_local",
		UserID:   "usr_local",
		Name:     "format test",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !ValidateTokenFormat(result.Token) {
		t.Error("ValidateTokenFormat should accept tok_ token")
	}
	if ValidateTokenFormat("tok_") {
		t.Error("ValidateTokenFormat should reject empty token body")
	}
	if ValidateTokenFormat("xxx_token") {
		t.Error("ValidateTokenFormat should reject wrong prefix")
	}
}

// TestScopesRoundTrip verifies that scopes serialize and deserialize correctly.
func TestScopesRoundTrip(t *testing.T) {
	tests := []struct {
		scopes []string
	}{
		{nil},
		{[]string{}},
		{[]string{"sessions:read"}},
		{[]string{"sessions:read", "sessions:write", "*"}},
	}
	for _, tt := range tests {
		jsonStr, err := scopesToJSON(tt.scopes)
		if err != nil {
			t.Fatalf("scopesToJSON: %v", err)
		}
		got, err := ScopesFromJSON(jsonStr)
		if err != nil {
			t.Fatalf("ScopesFromJSON: %v", err)
		}
		if len(got) != len(tt.scopes) {
			t.Errorf("round-trip: len = %d, want %d (json=%q)", len(got), len(tt.scopes), jsonStr)
			continue
		}
		for i, s := range tt.scopes {
			if got[i] != s {
				t.Errorf("round-trip[%d] = %q, want %q", i, got[i], s)
			}
		}
	}
}

// TestTouchAPIToken verifies that last_used_at is updated.
func TestTouchAPIToken(t *testing.T) {
	store, cleanup := setupTestStore(t)
	defer cleanup()
	issuer := New(store)
	ctx := context.Background()

	result, err := issuer.Create(ctx, CreateRequest{
		TenantID: "tnt_local",
		UserID:   "usr_local",
		Name:     "touch test",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	now := time.Now().UnixNano()
	if err := store.TouchAPIToken(ctx, result.ID, now); err != nil {
		t.Fatalf("touch: %v", err)
	}

	// Verify last_used_at was updated.
	hash := HashToken(result.Token)
	row, err := store.LookupAPIToken(ctx, hash, now)
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if row.LastUsedAt == 0 {
		t.Error("last_used_at is 0, want non-zero")
	}
}
