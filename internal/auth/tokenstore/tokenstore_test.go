package tokenstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"
)

// hashHex returns the SHA-256 hex of a string, mirroring how callers
// (jointoken, auth_handler) hash plaintext secrets before storage.
func hashHex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// newTestStore creates an in-memory SQLite store with all migrations applied.
// Returns the store and a cleanup function.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := OpenTestDB(dbPath)
	if err != nil {
		t.Fatalf("OpenTestDB: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

// seedTenantAndUser creates a tenant and user so FK constraints on join_tokens,
// web_sessions, and api_tokens are satisfied. The tenant is "tnt_1" with an
// "owner" user "usr_1".
func seedTenantAndUser(t *testing.T, store *Store) {
	t.Helper()
	ctx := context.Background()
	now := nowNanos()
	// Create tenant.
	_, err := store.db.ExecContext(ctx,
		`INSERT INTO tenants (id, slug, display_name, status, created_at) VALUES (?, ?, ?, 'active', ?)`,
		"tnt_1", "tnt_1", "Test Tenant", now)
	if err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	// Create user.
	_, err = store.db.ExecContext(ctx,
		`INSERT INTO users (id, email, display_name, auth_subject, password_hash, status, created_at) VALUES (?, ?, ?, NULL, NULL, 'active', ?)`,
		"usr_1", "usr_1@test.com", "Test User", now)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	// Create membership.
	_, err = store.db.ExecContext(ctx,
		`INSERT INTO memberships (tenant_id, user_id, role, created_at) VALUES (?, ?, ?, ?)`,
		"tnt_1", "usr_1", "owner", now)
	if err != nil {
		t.Fatalf("seed membership: %v", err)
	}
}

// seedTenantOnly creates a tenant and a user+membership for it (needed for
// join_tokens FK on (tenant_id, created_by) → memberships).
func seedTenantOnly(t *testing.T, store *Store, tenantID string) {
	t.Helper()
	ctx := context.Background()
	now := nowNanos()
	_, err := store.db.ExecContext(ctx,
		`INSERT INTO tenants (id, slug, display_name, status, created_at) VALUES (?, ?, ?, 'active', ?)`,
		tenantID, tenantID, tenantID, now)
	if err != nil {
		t.Fatalf("seed tenant %s: %v", tenantID, err)
	}
	userID := "usr_" + tenantID
	_, err = store.db.ExecContext(ctx,
		`INSERT INTO users (id, email, display_name, auth_subject, password_hash, status, created_at) VALUES (?, ?, ?, NULL, NULL, 'active', ?)`,
		userID, userID+"@test.com", "User "+tenantID, now)
	if err != nil {
		t.Fatalf("seed user %s: %v", userID, err)
	}
	_, err = store.db.ExecContext(ctx,
		`INSERT INTO memberships (tenant_id, user_id, role, created_at) VALUES (?, ?, ?, ?)`,
		tenantID, userID, "owner", now)
	if err != nil {
		t.Fatalf("seed membership %s: %v", tenantID, err)
	}
}

// nowNanos returns a fixed timestamp for deterministic tests.
func nowNanos() int64 { return time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC).UnixNano() }

// futureNanos returns a timestamp 1 hour in the future (for expiry).
func futureNanos() int64 { return nowNanos() + int64(time.Hour) }

// pastNanos returns a timestamp 1 hour in the past (for expired tokens).
func pastNanos() int64 { return nowNanos() - int64(time.Hour) }

// ─── Join Token tests ──────────────────────────────────────────────────────

func TestCreateAndListJoinTokens(t *testing.T) {
	store := newTestStore(t)
	seedTenantAndUser(t, store)
	// Also seed a second tenant for cross-tenant isolation test.
	seedTenantOnly(t, store, "tnt_2")
	ctx := context.Background()

	// Create two tokens for the same tenant.
	if err := store.CreateJoinToken(ctx, "jt_1", "tnt_1", "usr_1", "member",
		hashHex("secret1"), "usr_1", futureNanos()); err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	if err := store.CreateJoinToken(ctx, "jt_2", "tnt_1", "", "admin",
		hashHex("secret2"), "usr_1", futureNanos()); err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}
	// Create a token for a different tenant — should NOT appear in tnt_1's list.
	if err := store.CreateJoinToken(ctx, "jt_3", "tnt_2", "usr_tnt_2", "member",
		hashHex("secret3"), "usr_tnt_2", futureNanos()); err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}

	tokens, err := store.ListJoinTokens(ctx, "tnt_1")
	if err != nil {
		t.Fatalf("ListJoinTokens: %v", err)
	}
	if len(tokens) != 2 {
		t.Fatalf("expected 2 tokens for tnt_1, got %d", len(tokens))
	}
	// Ordered by created_at DESC — both have the same nanosecond timestamp,
	// so order is by insertion. Verify IDs are present.
	ids := map[string]bool{tokens[0].ID: true, tokens[1].ID: true}
	if !ids["jt_1"] || !ids["jt_2"] {
		t.Errorf("expected jt_1 and jt_2, got %v", ids)
	}
}

func TestRevokeJoinToken(t *testing.T) {
	store := newTestStore(t)
	seedTenantAndUser(t, store)
	ctx := context.Background()

	if err := store.CreateJoinToken(ctx, "jt_1", "tnt_1", "usr_1", "member",
		hashHex("secret1"), "usr_1", futureNanos()); err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}

	// Revoke from the correct tenant — should succeed.
	if err := store.RevokeJoinToken(ctx, "jt_1", "tnt_1"); err != nil {
		t.Errorf("RevokeJoinToken: %v", err)
	}
	// Revoke again — idempotent.
	if err := store.RevokeJoinToken(ctx, "jt_1", "tnt_1"); err != nil {
		t.Errorf("RevokeJoinToken (idempotent): %v", err)
	}

	// Revoke from a different tenant — should return ErrJoinTokenNotFound.
	if err := store.RevokeJoinToken(ctx, "jt_1", "tnt_other"); err != ErrJoinTokenNotFound {
		t.Errorf("RevokeJoinToken from wrong tenant: got %v, want ErrJoinTokenNotFound", err)
	}

	// Revoke a non-existent token — should return ErrJoinTokenNotFound.
	if err := store.RevokeJoinToken(ctx, "nonexistent", "tnt_1"); err != ErrJoinTokenNotFound {
		t.Errorf("RevokeJoinToken nonexistent: got %v, want ErrJoinTokenNotFound", err)
	}
}

func TestConsumeJoinToken(t *testing.T) {
	store := newTestStore(t)
	seedTenantAndUser(t, store)
	ctx := context.Background()

	tokenHash := hashHex("secret1")
	if err := store.CreateJoinToken(ctx, "jt_1", "tnt_1", "usr_1", "member",
		tokenHash, "usr_1", futureNanos()); err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}

	tx, err := store.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	// Consume the token — should succeed.
	row, err := store.ConsumeJoinToken(ctx, tx, tokenHash, nowNanos())
	if err != nil {
		t.Fatalf("ConsumeJoinToken: %v", err)
	}
	if row.ID != "jt_1" || row.TenantID != "tnt_1" || row.UserID != "usr_1" || row.Role != "member" {
		t.Errorf("ConsumeJoinToken row mismatch: %+v", row)
	}

	// Consume again — should fail (already used).
	_, err = store.ConsumeJoinToken(ctx, tx, tokenHash, nowNanos())
	if err != ErrJoinTokenInvalid {
		t.Errorf("ConsumeJoinToken (already used): got %v, want ErrJoinTokenInvalid", err)
	}

	// Consume a non-existent token hash — should fail.
	_, err = store.ConsumeJoinToken(ctx, tx, hashHex("nonexistent"), nowNanos())
	if err != ErrJoinTokenInvalid {
		t.Errorf("ConsumeJoinToken (nonexistent): got %v, want ErrJoinTokenInvalid", err)
	}
}

func TestConsumeJoinTokenExpired(t *testing.T) {
	store := newTestStore(t)
	seedTenantAndUser(t, store)
	ctx := context.Background()

	tokenHash := hashHex("expired_secret")
	// Create a token that expired in the past.
	if err := store.CreateJoinToken(ctx, "jt_1", "tnt_1", "usr_1", "member",
		tokenHash, "usr_1", pastNanos()); err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}

	tx, err := store.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	_, err = store.ConsumeJoinToken(ctx, tx, tokenHash, nowNanos())
	if err != ErrJoinTokenInvalid {
		t.Errorf("ConsumeJoinToken (expired): got %v, want ErrJoinTokenInvalid", err)
	}
}

// ─── Web Session tests ─────────────────────────────────────────────────────

func TestCreateAndLookupWebSession(t *testing.T) {
	store := newTestStore(t)
	seedTenantAndUser(t, store)
	ctx := context.Background()

	tokenHash := hashHex("session_secret")
	now := nowNanos()
	idleExpiry := now + int64(30*time.Minute)
	absoluteExpiry := now + int64(24*time.Hour)

	tx, err := store.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if err := store.CreateWebSession(ctx, tx, "ws_1", tokenHash, "tnt_1", "usr_1",
		now, idleExpiry, absoluteExpiry); err != nil {
		t.Fatalf("CreateWebSession: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Lookup — should succeed.
	row, err := store.LookupWebSession(ctx, tokenHash, now)
	if err != nil {
		t.Fatalf("LookupWebSession: %v", err)
	}
	if row.ID != "ws_1" || row.TenantID != "tnt_1" || row.UserID != "usr_1" {
		t.Errorf("LookupWebSession row mismatch: %+v", row)
	}
}

func TestLookupWebSessionExpired(t *testing.T) {
	store := newTestStore(t)
	seedTenantAndUser(t, store)
	ctx := context.Background()

	tokenHash := hashHex("session_secret")
	now := nowNanos()
	// Absolute expiry in the past.
	tx, _ := store.BeginTx(ctx)
	store.CreateWebSession(ctx, tx, "ws_1", tokenHash, "tnt_1", "usr_1",
		now, now+int64(time.Hour), now-int64(time.Hour))
	tx.Commit()

	_, err := store.LookupWebSession(ctx, tokenHash, now)
	if err != ErrSessionInvalid {
		t.Errorf("LookupWebSession (expired): got %v, want ErrSessionInvalid", err)
	}
}

func TestLookupWebSessionIdleExpired(t *testing.T) {
	store := newTestStore(t)
	seedTenantAndUser(t, store)
	ctx := context.Background()

	tokenHash := hashHex("session_secret")
	now := nowNanos()
	// Idle expiry in the past.
	tx, _ := store.BeginTx(ctx)
	store.CreateWebSession(ctx, tx, "ws_1", tokenHash, "tnt_1", "usr_1",
		now, now-int64(time.Hour), now+int64(time.Hour))
	tx.Commit()

	_, err := store.LookupWebSession(ctx, tokenHash, now)
	if err != ErrSessionInvalid {
		t.Errorf("LookupWebSession (idle expired): got %v, want ErrSessionInvalid", err)
	}
}

func TestRevokeWebSessionByHash(t *testing.T) {
	store := newTestStore(t)
	seedTenantAndUser(t, store)
	ctx := context.Background()

	tokenHash := hashHex("session_secret")
	now := nowNanos()
	tx, _ := store.BeginTx(ctx)
	store.CreateWebSession(ctx, tx, "ws_1", tokenHash, "tnt_1", "usr_1",
		now, now+int64(time.Hour), now+int64(time.Hour))
	tx.Commit()

	// Revoke.
	if err := store.RevokeWebSessionByHash(ctx, tokenHash); err != nil {
		t.Fatalf("RevokeWebSessionByHash: %v", err)
	}

	// Lookup should now fail.
	_, err := store.LookupWebSession(ctx, tokenHash, now)
	if err != ErrSessionInvalid {
		t.Errorf("LookupWebSession after revoke: got %v, want ErrSessionInvalid", err)
	}
}

func TestTouchWebSession(t *testing.T) {
	store := newTestStore(t)
	seedTenantAndUser(t, store)
	ctx := context.Background()

	tokenHash := hashHex("session_secret")
	now := nowNanos()
	tx, _ := store.BeginTx(ctx)
	store.CreateWebSession(ctx, tx, "ws_1", tokenHash, "tnt_1", "usr_1",
		now, now+int64(30*time.Minute), now+int64(24*time.Hour))
	tx.Commit()

	// Touch — should update last_seen_at and idle_expires_at.
	newIdle := now + int64(45*time.Minute)
	if err := store.TouchWebSession(ctx, "ws_1", now+int64(5*time.Minute), newIdle); err != nil {
		t.Fatalf("TouchWebSession: %v", err)
	}

	// Verify the session is still valid.
	row, err := store.LookupWebSession(ctx, tokenHash, now+int64(5*time.Minute))
	if err != nil {
		t.Fatalf("LookupWebSession after touch: %v", err)
	}
	if row.IdleExpiresAt != newIdle {
		t.Errorf("IdleExpiresAt: got %d, want %d", row.IdleExpiresAt, newIdle)
	}
}

// ─── API Token tests ───────────────────────────────────────────────────────

func TestCreateAndLookupAPIToken(t *testing.T) {
	store := newTestStore(t)
	seedTenantAndUser(t, store)
	ctx := context.Background()

	tokenHash := hashHex("api_secret")
	if err := store.CreateAPIToken(ctx, "at_1", "tnt_1", "usr_1", "CI token",
		tokenHash, `["sessions:read"]`, futureNanos()); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	// Lookup — should succeed.
	row, err := store.LookupAPIToken(ctx, tokenHash, nowNanos())
	if err != nil {
		t.Fatalf("LookupAPIToken: %v", err)
	}
	if row.ID != "at_1" || row.TenantID != "tnt_1" || row.UserID != "usr_1" {
		t.Errorf("LookupAPIToken row mismatch: %+v", row)
	}
	if row.ScopesJSON != `["sessions:read"]` {
		t.Errorf("ScopesJSON: got %q, want %q", row.ScopesJSON, `["sessions:read"]`)
	}
}

func TestLookupAPITokenExpired(t *testing.T) {
	store := newTestStore(t)
	seedTenantAndUser(t, store)
	ctx := context.Background()

	tokenHash := hashHex("api_secret")
	// Expiry in the past.
	if err := store.CreateAPIToken(ctx, "at_1", "tnt_1", "usr_1", "CI token",
		tokenHash, "[]", pastNanos()); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	_, err := store.LookupAPIToken(ctx, tokenHash, nowNanos())
	if err != ErrAPITokenInvalid {
		t.Errorf("LookupAPIToken (expired): got %v, want ErrAPITokenInvalid", err)
	}
}

func TestLookupAPITokenNoExpiry(t *testing.T) {
	store := newTestStore(t)
	seedTenantAndUser(t, store)
	ctx := context.Background()

	tokenHash := hashHex("api_secret")
	// expiresAt = 0 means no expiry.
	if err := store.CreateAPIToken(ctx, "at_1", "tnt_1", "usr_1", "CI token",
		tokenHash, "", 0); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	row, err := store.LookupAPIToken(ctx, tokenHash, nowNanos())
	if err != nil {
		t.Fatalf("LookupAPIToken (no expiry): %v", err)
	}
	if row.ExpiresAt != 0 {
		t.Errorf("ExpiresAt: got %d, want 0 (no expiry)", row.ExpiresAt)
	}
	if row.ScopesJSON != "[]" {
		t.Errorf("ScopesJSON: got %q, want %q (empty → [])", row.ScopesJSON, "[]")
	}
}

func TestRevokeAPIToken(t *testing.T) {
	store := newTestStore(t)
	seedTenantAndUser(t, store)
	ctx := context.Background()

	tokenHash := hashHex("api_secret")
	if err := store.CreateAPIToken(ctx, "at_1", "tnt_1", "usr_1", "CI token",
		tokenHash, "[]", futureNanos()); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	// Revoke from the correct tenant — should succeed.
	if err := store.RevokeAPIToken(ctx, "at_1", "tnt_1"); err != nil {
		t.Errorf("RevokeAPIToken: %v", err)
	}
	// Revoke again — idempotent.
	if err := store.RevokeAPIToken(ctx, "at_1", "tnt_1"); err != nil {
		t.Errorf("RevokeAPIToken (idempotent): %v", err)
	}

	// Lookup should now fail.
	_, err := store.LookupAPIToken(ctx, tokenHash, nowNanos())
	if err != ErrAPITokenInvalid {
		t.Errorf("LookupAPIToken after revoke: got %v, want ErrAPITokenInvalid", err)
	}

	// Revoke from wrong tenant — should return ErrAPITokenNotFound.
	if err := store.RevokeAPIToken(ctx, "at_1", "tnt_other"); err != ErrAPITokenNotFound {
		t.Errorf("RevokeAPIToken from wrong tenant: got %v, want ErrAPITokenNotFound", err)
	}
}

func TestListAPITokens(t *testing.T) {
	store := newTestStore(t)
	seedTenantAndUser(t, store)
	seedTenantOnly(t, store, "tnt_2")
	ctx := context.Background()

	// Create a second user for the user-filter test.
	tx, _ := store.BeginTx(ctx)
	store.CreateUser(ctx, tx, "usr_2", "usr_2@test.com", "User 2")
	store.CreateMembership(ctx, tx, "tnt_1", "usr_2", "member")
	tx.Commit()

	store.CreateAPIToken(ctx, "at_1", "tnt_1", "usr_1", "token 1",
		hashHex("s1"), "[]", futureNanos())
	store.CreateAPIToken(ctx, "at_2", "tnt_1", "usr_2", "token 2",
		hashHex("s2"), "[]", futureNanos())
	store.CreateAPIToken(ctx, "at_3", "tnt_1", "usr_1", "token 3",
		hashHex("s3"), "[]", futureNanos())
	store.CreateAPIToken(ctx, "at_4", "tnt_2", "usr_3", "other tenant",
		hashHex("s4"), "[]", futureNanos())

	// Revoke one.
	store.RevokeAPIToken(ctx, "at_3", "tnt_1")

	// List all for tnt_1 (excludes revoked by default).
	tokens, err := store.ListAPITokens(ctx, "tnt_1", "", false)
	if err != nil {
		t.Fatalf("ListAPITokens: %v", err)
	}
	if len(tokens) != 2 {
		t.Fatalf("expected 2 active tokens, got %d", len(tokens))
	}

	// List all for tnt_1 including revoked.
	tokens, err = store.ListAPITokens(ctx, "tnt_1", "", true)
	if err != nil {
		t.Fatalf("ListAPITokens (includeRevoked): %v", err)
	}
	if len(tokens) != 3 {
		t.Fatalf("expected 3 tokens (incl revoked), got %d", len(tokens))
	}

	// List for a specific user.
	tokens, err = store.ListAPITokens(ctx, "tnt_1", "usr_1", false)
	if err != nil {
		t.Fatalf("ListAPITokens (user filter): %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("expected 1 token for usr_1 (excl revoked), got %d", len(tokens))
	}
	if tokens[0].ID != "at_1" {
		t.Errorf("expected at_1, got %q", tokens[0].ID)
	}
}

// ─── Membership queries ────────────────────────────────────────────────────

func TestMembershipLookup(t *testing.T) {
	store := newTestStore(t)
	seedTenantOnly(t, store, "tnt_1")
	ctx := context.Background()

	// Create a user and membership.
	tx, _ := store.BeginTx(ctx)
	if err := store.CreateUser(ctx, tx, "usr_1", "test@example.com", "Test User"); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	if err := store.CreateMembership(ctx, tx, "tnt_1", "usr_1", "member"); err != nil {
		t.Fatalf("CreateMembership: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	role, err := store.LookupMembershipRole(ctx, "tnt_1", "usr_1")
	if err != nil {
		t.Fatalf("LookupMembershipRole: %v", err)
	}
	if role != "member" {
		t.Errorf("role: got %q, want %q", role, "member")
	}

	// Non-existent membership.
	_, err = store.LookupMembershipRole(ctx, "tnt_1", "nonexistent")
	if err != ErrMembershipNotFound {
		t.Errorf("LookupMembershipRole (nonexistent): got %v, want ErrMembershipNotFound", err)
	}
}

func TestCheckUserActive(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	tx, _ := store.BeginTx(ctx)
	store.CreateUser(ctx, tx, "usr_1", "test@example.com", "Test User")
	tx.Commit()

	if err := store.CheckUserActive(ctx, "usr_1"); err != nil {
		t.Errorf("CheckUserActive: %v", err)
	}

	if err := store.CheckUserActive(ctx, "nonexistent"); err != ErrUserNotFound {
		t.Errorf("CheckUserActive (nonexistent): got %v, want ErrUserNotFound", err)
	}
}
