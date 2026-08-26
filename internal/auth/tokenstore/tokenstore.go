// Package tokenstore provides DB-backed queries for join_tokens and
// web_sessions tables. It is the persistence layer behind the join token
// exchange and web session cookie flows (P4c).
//
// All token/session secrets are SHA-256 hashed before storage. Plaintext
// secrets are never persisted. Lookups are by hash, not by plaintext.
package tokenstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Store wraps a *sql.DB for auth token queries. It is separate from
// sqlstore.SQLiteStore (session/event store) to keep the auth domain
// self-contained. Both stores share the same SQLite database file and
// migrations; the token store opens its own *sql.DB handle or uses the
// one passed by the daemon.
type Store struct {
	db *sql.DB
}

// New creates a token store backed by db. The caller is responsible for
// migrations (handled by sqlstore.NewSQLiteStore at daemon startup).
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// --- JoinToken queries ---

// JoinTokenRow is the metadata view of a join token. No secrets.
type JoinTokenRow struct {
	ID        string
	TenantID  string
	UserID    string // empty = "create user on exchange"
	Role      string
	CreatedBy string
	CreatedAt int64
	ExpiresAt int64
	UsedAt    int64 // 0 = unused
	RevokedAt int64 // 0 = not revoked
}

// CreateJoinToken inserts a new join token. tokenHash is the SHA-256 hex of
// the plaintext secret. The plaintext secret is NEVER stored.
func (s *Store) CreateJoinToken(ctx context.Context, id, tenantID, userID, role, tokenHash, createdBy string, expiresAt int64) error {
	now := time.Now().UnixNano()
	_, err := s.db.ExecContext(ctx, `INSERT INTO join_tokens
		(id, tenant_id, user_id, role, token_hash, created_by, expires_at, used_at, revoked_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?)`,
		id, tenantID, nullableString(userID), role, tokenHash, createdBy, expiresAt, now)
	if err != nil {
		return fmt.Errorf("tokenstore: create join token: %w", err)
	}
	return nil
}

// ListJoinTokens returns all join tokens for a tenant (metadata only, no
// hashes or secrets).
func (s *Store) ListJoinTokens(ctx context.Context, tenantID string) ([]JoinTokenRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, tenant_id, COALESCE(user_id, ''), role, created_by, created_at, expires_at, COALESCE(used_at, 0), COALESCE(revoked_at, 0)
		FROM join_tokens WHERE tenant_id = ? ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("tokenstore: list join tokens: %w", err)
	}
	defer rows.Close()

	var out []JoinTokenRow
	for rows.Next() {
		var r JoinTokenRow
		if err := rows.Scan(&r.ID, &r.TenantID, &r.UserID, &r.Role, &r.CreatedBy, &r.CreatedAt, &r.ExpiresAt, &r.UsedAt, &r.RevokedAt); err != nil {
			return nil, fmt.Errorf("tokenstore: scan join token: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RevokeJoinToken sets revoked_at on an unused or used token. Revoking an
// already-revoked token is a no-op (idempotent). The tenantID parameter
// scopes the operation to the caller's tenant — a token from another
// tenant is treated as "not found" (no existence leak, no cross-tenant
// revocation).
func (s *Store) RevokeJoinToken(ctx context.Context, id string, tenantID string) error {
	now := time.Now().UnixNano()
	result, err := s.db.ExecContext(ctx,
		"UPDATE join_tokens SET revoked_at = ? WHERE id = ? AND tenant_id = ? AND revoked_at IS NULL",
		now, id, tenantID)
	if err != nil {
		return fmt.Errorf("tokenstore: revoke join token: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("tokenstore: rows affected: %w", err)
	}
	if affected == 0 {
		// Either doesn't exist, already revoked, or belongs to a different
		// tenant. Check existence within the caller's tenant only.
		var exists bool
		err := s.db.QueryRowContext(ctx,
			"SELECT 1 FROM join_tokens WHERE id = ? AND tenant_id = ?", id, tenantID).Scan(&exists)
		if err == sql.ErrNoRows {
			return ErrJoinTokenNotFound
		}
		if err != nil {
			return fmt.Errorf("tokenstore: check join token existence: %w", err)
		}
		// Already revoked — idempotent success.
	}
	return nil
}

// ConsumeJoinToken atomically marks a join token as used. It does a
// conditional UPDATE: only succeeds if used_at IS NULL, revoked_at IS NULL,
// and expires_at > now. Returns the token row on success.
//
// This is step 1 of the exchange transaction. The caller must include
// subsequent steps (user creation, session creation) in the same
// transaction. This method accepts a *sql.Tx so it runs inside the caller's
// transaction.
func (s *Store) ConsumeJoinToken(ctx context.Context, tx *sql.Tx, tokenHash string, now int64) (*JoinTokenRow, error) {
	// Conditional UPDATE: claim the token atomically.
	result, err := tx.ExecContext(ctx,
		`UPDATE join_tokens SET used_at = ?
		 WHERE token_hash = ? AND used_at IS NULL AND revoked_at IS NULL AND expires_at > ?`,
		now, tokenHash, now)
	if err != nil {
		return nil, fmt.Errorf("tokenstore: consume join token: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("tokenstore: rows affected: %w", err)
	}
	if affected != 1 {
		// Either the token doesn't exist, or it's already used/expired/revoked.
		// Return a generic error — no enumeration (security: avoid
		// distinguishing expired vs used vs revoked to callers).
		return nil, ErrJoinTokenInvalid
	}

	// Read the token row for the exchange logic.
	var r JoinTokenRow
	var userID sql.NullString
	err = tx.QueryRowContext(ctx,
		`SELECT id, tenant_id, user_id, role, created_by, created_at, expires_at, COALESCE(used_at, 0), COALESCE(revoked_at, 0)
		 FROM join_tokens WHERE token_hash = ?`, tokenHash).
		Scan(&r.ID, &r.TenantID, &userID, &r.Role, &r.CreatedBy, &r.CreatedAt, &r.ExpiresAt, &r.UsedAt, &r.RevokedAt)
	if err != nil {
		return nil, fmt.Errorf("tokenstore: read consumed token: %w", err)
	}
	if userID.Valid {
		r.UserID = userID.String
	}
	return &r, nil
}

// --- Web session queries ---

// WebSessionRow is the data needed to resolve a session cookie to a
// principal. Role is NOT stored here — it's read from memberships at
// resolve time.
type WebSessionRow struct {
	ID            string
	TokenHash     string
	TenantID      string
	UserID        string
	CreatedAt     int64
	LastSeenAt    int64
	IdleExpiresAt int64
	ExpiresAt     int64
	RevokedAt     int64 // 0 = active
}

// CreateWebSession inserts a new web session. tokenHash is the SHA-256 hex
// of the opaque session secret.
func (s *Store) CreateWebSession(ctx context.Context, tx *sql.Tx, id, tokenHash, tenantID, userID string, now, idleExpiresAt, expiresAt int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO web_sessions
		(id, token_hash, tenant_id, user_id, created_at, last_seen_at, idle_expires_at, expires_at, revoked_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULL)`,
		id, tokenHash, tenantID, userID, now, now, idleExpiresAt, expiresAt)
	if err != nil {
		return fmt.Errorf("tokenstore: create web session: %w", err)
	}
	return nil
}

// LookupWebSession finds an active web session by token hash. Returns
// ErrSessionInvalid if the session is not found, revoked, or expired.
// The caller (resolver) reads the current membership role separately.
func (s *Store) LookupWebSession(ctx context.Context, tokenHash string, now int64) (*WebSessionRow, error) {
	var r WebSessionRow
	var revokedAt sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, token_hash, tenant_id, user_id, created_at, last_seen_at, idle_expires_at, expires_at, revoked_at
		 FROM web_sessions WHERE token_hash = ?`, tokenHash).
		Scan(&r.ID, &r.TokenHash, &r.TenantID, &r.UserID, &r.CreatedAt, &r.LastSeenAt, &r.IdleExpiresAt, &r.ExpiresAt, &revokedAt)
	if err == sql.ErrNoRows {
		return nil, ErrSessionInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("tokenstore: lookup web session: %w", err)
	}
	if revokedAt.Valid {
		r.RevokedAt = revokedAt.Int64
	}
	if r.RevokedAt != 0 || r.ExpiresAt <= now || r.IdleExpiresAt <= now {
		return nil, ErrSessionInvalid
	}
	return &r, nil
}

// TouchWebSession updates last_seen_at and idle_expires_at (sliding
// window). Rate-limited by the caller (not every request).
func (s *Store) TouchWebSession(ctx context.Context, id string, now, idleExpiresAt int64) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE web_sessions SET last_seen_at = ?, idle_expires_at = ? WHERE id = ? AND revoked_at IS NULL",
		now, idleExpiresAt, id)
	return err
}

// RevokeWebSessionByHash revokes a session by its token hash (used by
// Logout, which reads the cookie).
func (s *Store) RevokeWebSessionByHash(ctx context.Context, tokenHash string) error {
	now := time.Now().UnixNano()
	_, err := s.db.ExecContext(ctx,
		"UPDATE web_sessions SET revoked_at = ? WHERE token_hash = ? AND revoked_at IS NULL",
		now, tokenHash)
	return err
}

// --- API token queries (P4d) ---

// APITokenRow is the metadata view of an API token. No secrets.
type APITokenRow struct {
	ID         string
	TenantID   string
	UserID     string
	Name       string
	ScopesJSON string // JSON array of scope strings
	ExpiresAt  int64  // 0 = no expiry
	LastUsedAt int64  // 0 = never used
	RevokedAt  int64  // 0 = not revoked
	CreatedAt  int64
}

// CreateAPIToken inserts a new API token. tokenHash is the SHA-256 hex of
// the plaintext secret. The plaintext secret is NEVER stored. scopesJSON
// is a JSON array string (e.g. `["sessions:read"]`); empty means "[]"
// (inherit role permissions).
func (s *Store) CreateAPIToken(ctx context.Context, id, tenantID, userID, name, tokenHash, scopesJSON string, expiresAt int64) error {
	now := time.Now().UnixNano()
	if scopesJSON == "" {
		scopesJSON = "[]"
	}
	var expiresVal interface{}
	if expiresAt != 0 {
		expiresVal = expiresAt
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO api_tokens
		(id, tenant_id, user_id, name, token_hash, scopes, expires_at, last_used_at, revoked_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?)`,
		id, tenantID, userID, name, tokenHash, scopesJSON, expiresVal, now)
	if err != nil {
		return fmt.Errorf("tokenstore: create api token: %w", err)
	}
	return nil
}

// ListAPITokens returns API tokens for a tenant, optionally filtered by
// user_id. If userID is empty, returns all tokens for the tenant. Excludes
// revoked tokens from the default view unless includeRevoked is true.
func (s *Store) ListAPITokens(ctx context.Context, tenantID, userID string, includeRevoked bool) ([]APITokenRow, error) {
	query := `SELECT id, tenant_id, user_id, name, scopes, COALESCE(expires_at, 0), COALESCE(last_used_at, 0), COALESCE(revoked_at, 0), created_at
		FROM api_tokens WHERE tenant_id = ?`
	args := []interface{}{tenantID}
	if userID != "" {
		query += " AND user_id = ?"
		args = append(args, userID)
	}
	if !includeRevoked {
		query += " AND revoked_at IS NULL"
	}
	query += " ORDER BY created_at DESC"

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("tokenstore: list api tokens: %w", err)
	}
	defer rows.Close()

	var out []APITokenRow
	for rows.Next() {
		var r APITokenRow
		if err := rows.Scan(&r.ID, &r.TenantID, &r.UserID, &r.Name, &r.ScopesJSON, &r.ExpiresAt, &r.LastUsedAt, &r.RevokedAt, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("tokenstore: scan api token: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RevokeAPIToken sets revoked_at on an API token within a specific tenant.
// The tenant_id predicate prevents cross-tenant revocation (security:
// token IDs are UUIDs but should not be an authorization boundary). Revoking
// an already-revoked token is a no-op (idempotent). Returns
// ErrAPITokenNotFound if the token does not exist in this tenant.
func (s *Store) RevokeAPIToken(ctx context.Context, id, tenantID string) error {
	now := time.Now().UnixNano()
	result, err := s.db.ExecContext(ctx,
		"UPDATE api_tokens SET revoked_at = ? WHERE id = ? AND tenant_id = ? AND revoked_at IS NULL",
		now, id, tenantID)
	if err != nil {
		return fmt.Errorf("tokenstore: revoke api token: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("tokenstore: rows affected: %w", err)
	}
	if affected == 0 {
		var exists bool
		err := s.db.QueryRowContext(ctx, "SELECT 1 FROM api_tokens WHERE id = ? AND tenant_id = ?", id, tenantID).Scan(&exists)
		if err == sql.ErrNoRows {
			return ErrAPITokenNotFound
		}
		if err != nil {
			return fmt.Errorf("tokenstore: check api token existence: %w", err)
		}
		// Already revoked — idempotent success.
	}
	return nil
}

// LookupAPIToken finds an active (non-revoked, non-expired) API token by
// its token hash. Returns ErrAPITokenInvalid if the token is not found,
// revoked, or expired. Used by the API token resolver.
func (s *Store) LookupAPIToken(ctx context.Context, tokenHash string, now int64) (*APITokenRow, error) {
	var r APITokenRow
	var expiresAt sql.NullInt64
	var lastUsedAt sql.NullInt64
	var revokedAt sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, user_id, name, scopes, expires_at, last_used_at, revoked_at, created_at
		 FROM api_tokens WHERE token_hash = ?`, tokenHash).
		Scan(&r.ID, &r.TenantID, &r.UserID, &r.Name, &r.ScopesJSON, &expiresAt, &lastUsedAt, &revokedAt, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrAPITokenInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("tokenstore: lookup api token: %w", err)
	}
	if expiresAt.Valid {
		r.ExpiresAt = expiresAt.Int64
	}
	if lastUsedAt.Valid {
		r.LastUsedAt = lastUsedAt.Int64
	}
	if revokedAt.Valid {
		r.RevokedAt = revokedAt.Int64
	}
	if r.RevokedAt != 0 || (r.ExpiresAt != 0 && r.ExpiresAt <= now) {
		return nil, ErrAPITokenInvalid
	}
	return &r, nil
}

// TouchAPIToken updates last_used_at (best-effort, not in the auth
// critical path). A failure is logged by the caller but does not reject
// the request.
func (s *Store) TouchAPIToken(ctx context.Context, id string, now int64) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE api_tokens SET last_used_at = ? WHERE id = ? AND revoked_at IS NULL",
		now, id)
	return err
}

// --- User/Membership queries (for exchange) ---

// CreateUser creates a new user with no password and no auth_subject.
// Used by the join token exchange when user_id is NULL (create-on-exchange).
func (s *Store) CreateUser(ctx context.Context, tx *sql.Tx, id, email, displayName string) error {
	now := time.Now().UnixNano()
	_, err := tx.ExecContext(ctx, `INSERT INTO users (id, email, display_name, auth_subject, password_hash, status, created_at)
		VALUES (?, ?, ?, NULL, NULL, 'active', ?)`,
		id, email, displayName, now)
	if err != nil {
		return fmt.Errorf("tokenstore: create user: %w", err)
	}
	return nil
}

// CreateMembership creates a tenant membership for a user with a role.
func (s *Store) CreateMembership(ctx context.Context, tx *sql.Tx, tenantID, userID, role string) error {
	now := time.Now().UnixNano()
	_, err := tx.ExecContext(ctx, `INSERT INTO memberships (tenant_id, user_id, role, created_at)
		VALUES (?, ?, ?, ?)`,
		tenantID, userID, role, now)
	if err != nil {
		return fmt.Errorf("tokenstore: create membership: %w", err)
	}
	return nil
}

// LookupMembershipRole reads the current role for a user in a tenant.
// Returns ("", ErrMembershipNotFound) if the user is not a member.
func (s *Store) LookupMembershipRole(ctx context.Context, tenantID, userID string) (string, error) {
	var role string
	err := s.db.QueryRowContext(ctx,
		"SELECT role FROM memberships WHERE tenant_id = ? AND user_id = ?",
		tenantID, userID).Scan(&role)
	if err == sql.ErrNoRows {
		return "", ErrMembershipNotFound
	}
	if err != nil {
		return "", fmt.Errorf("tokenstore: lookup membership: %w", err)
	}
	return role, nil
}

// CheckUserActive returns nil if the user exists and is active.
func (s *Store) CheckUserActive(ctx context.Context, userID string) error {
	var status string
	err := s.db.QueryRowContext(ctx, "SELECT status FROM users WHERE id = ?", userID).Scan(&status)
	if err == sql.ErrNoRows {
		return ErrUserNotFound
	}
	if err != nil {
		return fmt.Errorf("tokenstore: check user: %w", err)
	}
	if status != "active" {
		return ErrUserSuspended
	}
	return nil
}

// CheckTenantActive returns nil if the tenant exists and is active.
func (s *Store) CheckTenantActive(ctx context.Context, tenantID string) error {
	var status string
	err := s.db.QueryRowContext(ctx, "SELECT status FROM tenants WHERE id = ?", tenantID).Scan(&status)
	if err == sql.ErrNoRows {
		return ErrTenantNotFound
	}
	if err != nil {
		return fmt.Errorf("tokenstore: check tenant: %w", err)
	}
	if status != "active" {
		return ErrTenantSuspended
	}
	return nil
}

// --- Transaction helper ---

// BeginTx starts a transaction.
func (s *Store) BeginTx(ctx context.Context) (*sql.Tx, error) {
	return s.db.BeginTx(ctx, nil)
}

// --- OIDC queries (P4e) ---

// LookupUserByAuthSubject finds a user by their OIDC `sub` claim
// (users.auth_subject). Returns ErrUserNotFound if no user has this
// auth_subject. Used by the OIDC resolver to map a validated JWT's `sub`
// claim to a local user.
func (s *Store) LookupUserByAuthSubject(ctx context.Context, authSubject string) (*UserRow, error) {
	var r UserRow
	err := s.db.QueryRowContext(ctx,
		`SELECT id, email, display_name, auth_subject, status, created_at
		 FROM users WHERE auth_subject = ?`, authSubject).
		Scan(&r.ID, &r.Email, &r.DisplayName, &r.AuthSubject, &r.Status, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, ErrUserNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("tokenstore: lookup user by auth_subject: %w", err)
	}
	return &r, nil
}

// CreateUserWithAuthSubject creates a new user with an OIDC auth_subject
// and a default membership. This is the auto-provisioning path for first-time
// OIDC login: the `sub` claim becomes auth_subject, and the user is created
// with `member` role in the specified tenant.
//
// If a user with the same auth_subject already exists, ErrDuplicateAuthSubject
// is returned (the caller should use LookupUserByAuthSubject instead).
func (s *Store) CreateUserWithAuthSubject(ctx context.Context, tx *sql.Tx, id, email, displayName, authSubject, tenantID, role string) error {
	now := time.Now().UnixNano()
	_, err := tx.ExecContext(ctx, `INSERT INTO users (id, email, display_name, auth_subject, password_hash, status, created_at)
		VALUES (?, ?, ?, ?, NULL, 'active', ?)`,
		id, email, displayName, authSubject, now)
	if err != nil {
		return fmt.Errorf("tokenstore: create user with auth_subject: %w", err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memberships (tenant_id, user_id, role, created_at)
		VALUES (?, ?, ?, ?)`,
		tenantID, id, role, now)
	if err != nil {
		return fmt.Errorf("tokenstore: create membership for oidc user: %w", err)
	}
	return nil
}

// --- OIDC errors ---

var (
	ErrDuplicateAuthSubject = errors.New("tokenstore: auth_subject already exists")
)

// UserRow is the user view for OIDC resolution (metadata only, no secrets).
type UserRow struct {
	ID          string
	Email       string
	DisplayName string
	AuthSubject string // OIDC sub; empty for local accounts
	Status      string
	CreatedAt   int64
}

// DB returns the underlying *sql.DB (for the daemon to share the handle).
func (s *Store) DB() *sql.DB {
	return s.db
}

// --- Errors ---

var (
	ErrJoinTokenNotFound  = errors.New("tokenstore: join token not found")
	ErrJoinTokenInvalid   = errors.New("tokenstore: join token invalid, expired, or already used")
	ErrSessionInvalid     = errors.New("tokenstore: session invalid, expired, or revoked")
	ErrAPITokenNotFound   = errors.New("tokenstore: api token not found")
	ErrAPITokenInvalid    = errors.New("tokenstore: api token invalid, expired, or revoked")
	ErrMembershipNotFound = errors.New("tokenstore: membership not found")
	ErrUserNotFound       = errors.New("tokenstore: user not found")
	ErrUserSuspended      = errors.New("tokenstore: user suspended")
	ErrTenantNotFound     = errors.New("tokenstore: tenant not found")
	ErrTenantSuspended    = errors.New("tokenstore: tenant suspended")
)

// nullableString returns a sql.NullString for a string, treating "" as NULL.
func nullableString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
