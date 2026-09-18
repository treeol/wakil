package tokenstore

import (
	"context"
	"testing"
	"time"
)

// ─── GetAPITokenByID tests ─────────────────────────────────────────────────

func TestGetAPITokenByID(t *testing.T) {
	store := newTestStore(t)
	seedTenantAndUser(t, store)
	ctx := context.Background()

	tokenHash := hashHex("api_secret")
	if err := store.CreateAPIToken(ctx, "at_1", "tnt_1", "usr_1", "CI token",
		tokenHash, `["sessions:read"]`, futureNanos()); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	// Happy path — found.
	row, err := store.GetAPITokenByID(ctx, "at_1", "tnt_1")
	if err != nil {
		t.Fatalf("GetAPITokenByID: %v", err)
	}
	if row.ID != "at_1" || row.TenantID != "tnt_1" || row.UserID != "usr_1" {
		t.Errorf("GetAPITokenByID row mismatch: %+v", row)
	}
	if row.Name != "CI token" || row.ScopesJSON != `["sessions:read"]` {
		t.Errorf("GetAPITokenByID fields mismatch: name=%q scopes=%q", row.Name, row.ScopesJSON)
	}
	if row.ExpiresAt != futureNanos() {
		t.Errorf("ExpiresAt: got %d, want %d", row.ExpiresAt, futureNanos())
	}

	// Not found in this tenant.
	_, err = store.GetAPITokenByID(ctx, "at_1", "tnt_other")
	if err != ErrAPITokenNotFound {
		t.Errorf("GetAPITokenByID (wrong tenant): got %v, want ErrAPITokenNotFound", err)
	}

	// Non-existent ID.
	_, err = store.GetAPITokenByID(ctx, "nonexistent", "tnt_1")
	if err != ErrAPITokenNotFound {
		t.Errorf("GetAPITokenByID (nonexistent): got %v, want ErrAPITokenNotFound", err)
	}
}

// ─── TouchAPIToken tests ───────────────────────────────────────────────────

func TestTouchAPIToken(t *testing.T) {
	store := newTestStore(t)
	seedTenantAndUser(t, store)
	ctx := context.Background()

	tokenHash := hashHex("api_secret")
	if err := store.CreateAPIToken(ctx, "at_1", "tnt_1", "usr_1", "CI token",
		tokenHash, "[]", 0); err != nil {
		t.Fatalf("CreateAPIToken: %v", err)
	}

	// Touch — should update last_used_at.
	touchTime := nowNanos() + int64(5*time.Minute)
	if err := store.TouchAPIToken(ctx, "at_1", touchTime); err != nil {
		t.Fatalf("TouchAPIToken: %v", err)
	}

	// Verify via GetAPITokenByID.
	row, err := store.GetAPITokenByID(ctx, "at_1", "tnt_1")
	if err != nil {
		t.Fatalf("GetAPITokenByID after touch: %v", err)
	}
	if row.LastUsedAt != touchTime {
		t.Errorf("LastUsedAt: got %d, want %d", row.LastUsedAt, touchTime)
	}
}

// ─── CheckTenantActive tests ───────────────────────────────────────────────

func TestCheckTenantActive(t *testing.T) {
	store := newTestStore(t)
	seedTenantAndUser(t, store) // creates active tenant "tnt_1"
	ctx := context.Background()

	// Active tenant — should succeed.
	if err := store.CheckTenantActive(ctx, "tnt_1"); err != nil {
		t.Errorf("CheckTenantActive (active): %v", err)
	}

	// Non-existent tenant.
	if err := store.CheckTenantActive(ctx, "tnt_nonexistent"); err != ErrTenantNotFound {
		t.Errorf("CheckTenantActive (nonexistent): got %v, want ErrTenantNotFound", err)
	}

	// Suspended tenant.
	_, err := store.db.ExecContext(ctx,
		`INSERT INTO tenants (id, slug, display_name, status, created_at) VALUES (?, ?, ?, 'suspended', ?)`,
		"tnt_suspended", "tnt_suspended", "Suspended Tenant", nowNanos())
	if err != nil {
		t.Fatalf("seed suspended tenant: %v", err)
	}
	if err := store.CheckTenantActive(ctx, "tnt_suspended"); err != ErrTenantSuspended {
		t.Errorf("CheckTenantActive (suspended): got %v, want ErrTenantSuspended", err)
	}
}

// ─── CheckUserActive suspended branch ──────────────────────────────────────

func TestCheckUserActiveSuspended(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// Create a suspended user.
	tx, _ := store.BeginTx(ctx)
	store.CreateUser(ctx, tx, "usr_suspended", "suspended@test.com", "Suspended User")
	tx.Commit()
	_, err := store.db.ExecContext(ctx,
		`UPDATE users SET status = 'suspended' WHERE id = ?`, "usr_suspended")
	if err != nil {
		t.Fatalf("suspend user: %v", err)
	}

	if err := store.CheckUserActive(ctx, "usr_suspended"); err != ErrUserSuspended {
		t.Errorf("CheckUserActive (suspended): got %v, want ErrUserSuspended", err)
	}
}

// ─── ConsumeJoinToken revoked branch ───────────────────────────────────────

func TestConsumeJoinTokenRevoked(t *testing.T) {
	store := newTestStore(t)
	seedTenantAndUser(t, store)
	ctx := context.Background()

	tokenHash := hashHex("revoked_secret")
	if err := store.CreateJoinToken(ctx, "jt_1", "tnt_1", "usr_1", "member",
		tokenHash, "usr_1", futureNanos()); err != nil {
		t.Fatalf("CreateJoinToken: %v", err)
	}

	// Revoke the token before consuming.
	if err := store.RevokeJoinToken(ctx, "jt_1", "tnt_1"); err != nil {
		t.Fatalf("RevokeJoinToken: %v", err)
	}

	// Consume should fail — revoked tokens are invalid.
	tx, err := store.BeginTx(ctx)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer tx.Rollback()

	_, err = store.ConsumeJoinToken(ctx, tx, tokenHash, nowNanos())
	if err != ErrJoinTokenInvalid {
		t.Errorf("ConsumeJoinToken (revoked): got %v, want ErrJoinTokenInvalid", err)
	}
}

// ─── OIDC: LookupUserByAuthSubject tests ──────────────────────────────────

func TestLookupUserByAuthSubject(t *testing.T) {
	store := newTestStore(t)
	seedTenantOnly(t, store, "tnt_1")
	ctx := context.Background()

	// Create a user with an auth_subject via the OIDC provisioning path.
	tx, _ := store.BeginTx(ctx)
	if err := store.CreateUserWithAuthSubject(ctx, tx,
		"usr_oidc_1", "oidc@test.com", "OIDC User",
		"google:12345", "tnt_1", "member"); err != nil {
		t.Fatalf("CreateUserWithAuthSubject: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	// Lookup by auth_subject — should find the user.
	row, err := store.LookupUserByAuthSubject(ctx, "google:12345")
	if err != nil {
		t.Fatalf("LookupUserByAuthSubject: %v", err)
	}
	if row.ID != "usr_oidc_1" || row.Email != "oidc@test.com" {
		t.Errorf("LookupUserByAuthSubject row mismatch: %+v", row)
	}
	if row.AuthSubject != "google:12345" || row.Status != "active" {
		t.Errorf("LookupUserByAuthSubject fields: auth_subject=%q status=%q", row.AuthSubject, row.Status)
	}

	// Non-existent auth_subject.
	_, err = store.LookupUserByAuthSubject(ctx, "nonexistent:sub")
	if err != ErrUserNotFound {
		t.Errorf("LookupUserByAuthSubject (nonexistent): got %v, want ErrUserNotFound", err)
	}
}

// ─── OIDC: CreateUserWithAuthSubject tests ────────────────────────────────

func TestCreateUserWithAuthSubjectDuplicate(t *testing.T) {
	store := newTestStore(t)
	seedTenantOnly(t, store, "tnt_1")
	ctx := context.Background()

	// Create a user with an auth_subject.
	tx, _ := store.BeginTx(ctx)
	store.CreateUserWithAuthSubject(ctx, tx,
		"usr_oidc_1", "oidc@test.com", "OIDC User",
		"google:12345", "tnt_1", "member")
	tx.Commit()

	// Attempt to create another user with the same auth_subject — should fail.
	// Note: SQLite UNIQUE constraint on auth_subject will cause an INSERT error.
	tx2, _ := store.BeginTx(ctx)
	err := store.CreateUserWithAuthSubject(ctx, tx2,
		"usr_oidc_2", "oidc2@test.com", "OIDC User 2",
		"google:12345", "tnt_1", "member")
	if err == nil {
		tx2.Commit()
		t.Fatal("CreateUserWithAuthSubject with duplicate auth_subject should fail")
	}
	tx2.Rollback()
}
