// Package jointoken implements join token generation, issuance, and
// exchange for the wakil daemon (P4c).
//
// Join tokens are one-time-use, admin-issued credentials that bootstrap new
// users into the system. They are exchanged for a browser session cookie
// (via the web session creation in the same transaction).
//
// Token format: jnt_<base64url(32 random bytes)> — 256 bits of entropy,
// visually prefixed for log safety. Only the SHA-256 hash is stored.
//
// Exchange atomicity: the entire exchange (consume token, create user if
// needed, create membership, create web session) runs in one SQLite
// transaction. A failure at any step rolls back — the token is NOT
// consumed if user/session creation fails.
package jointoken

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/treeol/wakil/internal/auth/tokenstore"
)

// Prefix is the visual prefix for join tokens (log safety).
const Prefix = "jnt_"

// DefaultExpiry is the default lifetime of a join token (7 days).
const DefaultExpiry = 7 * 24 * time.Hour

// Issuer handles join token creation and exchange. It wraps the tokenstore
// and provides the business logic: role-based authorization, token
// generation, atomic exchange.
type Issuer struct {
	store *tokenstore.Store
}

// New creates a join token issuer backed by the given store.
func New(store *tokenstore.Store) *Issuer {
	return &Issuer{store: store}
}

// CreateRequest is the input for issuing a join token.
type CreateRequest struct {
	TenantID    string
	Role        string
	UserID      string // optional: bind to existing user; "" = create on exchange
	Email       string // for create-on-exchange: new user's email
	DisplayName string // for create-on-exchange: new user's display name
	CreatedBy   string // the issuing user's ID
}

// CreateResult is the output of a successful token creation.
type CreateResult struct {
	Token     string // plaintext token — shown ONCE, never again
	ID        string
	ExpiresAt time.Time
}

// Create issues a new join token. The issuing principal's role must be
// owner or admin. Only owners can issue owner-role tokens. The plaintext
// token is returned exactly once; only its SHA-256 hash is stored.
func (i *Issuer) Create(ctx context.Context, req CreateRequest) (*CreateResult, error) {
	// Validate role.
	switch req.Role {
	case "owner", "admin", "member", "viewer":
	default:
		return nil, ErrInvalidRole
	}

	// Only owners can issue owner-role tokens.
	if req.Role == "owner" {
		// The caller must have verified the issuer is an owner. We re-check
		// via the store to be safe (defense-in-depth).
		role, err := i.store.LookupMembershipRole(ctx, req.TenantID, req.CreatedBy)
		if err != nil {
			return nil, fmt.Errorf("jointoken: check issuer role: %w", err)
		}
		if role != "owner" {
			return nil, ErrInsufficientRole
		}
	}

	// For create-on-exchange tokens, validate email is provided.
	if req.UserID == "" {
		if req.Email == "" || req.DisplayName == "" {
			return nil, ErrMissingUserInfo
		}
	}

	// Generate a 256-bit random token.
	plaintext, err := generateToken()
	if err != nil {
		return nil, fmt.Errorf("jointoken: generate token: %w", err)
	}
	tokenHash := hashToken(plaintext)

	id := "jnt_" + uuid.NewString()
	expiresAt := time.Now().Add(DefaultExpiry)

	err = i.store.CreateJoinToken(ctx, id, req.TenantID, req.UserID, req.Role, tokenHash, req.CreatedBy, expiresAt.UnixNano())
	if err != nil {
		return nil, err
	}

	return &CreateResult{
		Token:     plaintext,
		ID:        id,
		ExpiresAt: expiresAt,
	}, nil
}

// ExchangeRequest is the input for exchanging a join token for a session.
type ExchangeRequest struct {
	Token       string // plaintext join token (jnt_...)
	Email       string // for create-on-exchange: new user's email
	DisplayName string // for create-on-exchange: new user's display name
}

// ExchangeResult is the output of a successful exchange.
type ExchangeResult struct {
	Principal     PrincipalInfo
	SessionCookie string // the web session secret to set as a cookie
}

// PrincipalInfo is the resolved identity after exchange.
type PrincipalInfo struct {
	TenantID string
	UserID   string
	Role     string
}

// Exchange consumes a join token and creates a web session in one atomic
// transaction. The plaintext token is verified by hash. If the token has
// user_id=NULL, a new user and membership are created. If user_id is set,
// the membership is verified. A web session is created and its opaque
// secret is returned (to be set as a cookie by the handler).
//
// Returns a generic error for invalid/expired/used/revoked tokens (no
// enumeration — security).
func (i *Issuer) Exchange(ctx context.Context, req ExchangeRequest) (*ExchangeResult, error) {
	// Validate token format.
	if !strings.HasPrefix(req.Token, Prefix) {
		return nil, ErrInvalidToken
	}

	tokenHash := hashToken(req.Token)
	now := time.Now().UnixNano()

	// Pre-transaction checks: verify tenant is active. These use the
	// shared *sql.DB connection. We must do them BEFORE opening the
	// transaction to avoid a deadlock with SetMaxOpenConns(1) (the
	// transaction holds the single connection).
	// We can't check the tenant here because we don't know it yet — it's
	// in the token row. So we do the tenant check inside the transaction
	// using tx.QueryRowContext instead of s.db.QueryRowContext.

	tx, err := i.store.BeginTx(ctx)
	if err != nil {
		return nil, fmt.Errorf("jointoken: begin tx: %w", err)
	}
	defer tx.Rollback()

	// Step 1: atomically consume the token.
	tokenRow, err := i.store.ConsumeJoinToken(ctx, tx, tokenHash, now)
	if err != nil {
		return nil, err // ErrJoinTokenInvalid (generic, no enumeration)
	}

	// Step 2: verify tenant is active (inside the transaction).
	var tenantStatus string
	err = tx.QueryRowContext(ctx,
		"SELECT status FROM tenants WHERE id = ?", tokenRow.TenantID).Scan(&tenantStatus)
	if err != nil {
		return nil, fmt.Errorf("jointoken: check tenant: %w", err)
	}
	if tenantStatus != "active" {
		return nil, tokenstore.ErrTenantSuspended
	}

	// Step 3: resolve or create the user.
	var userID string
	var role string = tokenRow.Role

	if tokenRow.UserID != "" {
		// Bound token: verify the user exists, is active, and is a member.
		var userStatus string
		err = tx.QueryRowContext(ctx,
			"SELECT status FROM users WHERE id = ?", tokenRow.UserID).Scan(&userStatus)
		if err == sql.ErrNoRows {
			return nil, tokenstore.ErrUserNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("jointoken: check user: %w", err)
		}
		if userStatus != "active" {
			return nil, tokenstore.ErrUserSuspended
		}
		userID = tokenRow.UserID
	} else {
		// Create-on-exchange: create a new user + membership.
		// Email and display_name come from the exchange request (Mashūra
		// feedback: auth_subject must NEVER come from the request — only
		// from OIDC flow. Email and display_name are acceptable from the
		// request because the token issuer authorized this bootstrap).
		if req.Email == "" || req.DisplayName == "" {
			return nil, ErrMissingUserInfo
		}
		userID = "usr_" + uuid.NewString()
		if err := i.store.CreateUser(ctx, tx, userID, req.Email, req.DisplayName); err != nil {
			return nil, err
		}
	}

	// Step 4: create membership (if create-on-exchange) or verify existing.
	if tokenRow.UserID == "" {
		if err := i.store.CreateMembership(ctx, tx, tokenRow.TenantID, userID, role); err != nil {
			return nil, err
		}
	} else {
		// Verify membership exists for bound tokens.
		var mRole string
		err = tx.QueryRowContext(ctx,
			"SELECT role FROM memberships WHERE tenant_id = ? AND user_id = ?",
			tokenRow.TenantID, userID).Scan(&mRole)
		if err == sql.ErrNoRows {
			return nil, tokenstore.ErrMembershipNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("jointoken: lookup membership: %w", err)
		}
		// Use the membership's current role, not the token's role.
		role = mRole
	}

	// Step 5: create web session.
	sessionSecret, err := generateSessionToken()
	if err != nil {
		return nil, fmt.Errorf("jointoken: generate session token: %w", err)
	}
	sessionHash := hashToken(sessionSecret)
	sessionID := "wst_" + uuid.NewString()

	idleExpiry := now + int64(24*time.Hour)       // 24h sliding window
	absoluteExpiry := now + int64(7*24*time.Hour) // 7d absolute

	if err := i.store.CreateWebSession(ctx, tx, sessionID, sessionHash, tokenRow.TenantID, userID, now, idleExpiry, absoluteExpiry); err != nil {
		return nil, err
	}

	// Commit the entire transaction.
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("jointoken: commit exchange: %w", err)
	}

	return &ExchangeResult{
		Principal: PrincipalInfo{
			TenantID: tokenRow.TenantID,
			UserID:   userID,
			Role:     role,
		},
		SessionCookie: sessionSecret,
	}, nil
}

// Revoke revokes an unused join token by ID. The tenantID parameter scopes
// the revocation to the caller's tenant — a token belonging to another
// tenant is treated as not found (no cross-tenant revocation).
func (i *Issuer) Revoke(ctx context.Context, id string, tenantID string) error {
	return i.store.RevokeJoinToken(ctx, id, tenantID)
}

// List returns all join tokens for a tenant (metadata only).
func (i *Issuer) List(ctx context.Context, tenantID string) ([]tokenstore.JoinTokenRow, error) {
	return i.store.ListJoinTokens(ctx, tenantID)
}

// --- Token generation and hashing ---

// generateToken generates a 256-bit random join token with the jnt_ prefix.
func generateToken() (string, error) {
	b := make([]byte, 32) // 256 bits
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// generateSessionToken generates a 256-bit random web session token with
// the wst_ prefix.
func generateSessionToken() (string, error) {
	b := make([]byte, 32) // 256 bits
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "wst_" + base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken returns the SHA-256 hex of a token string.
func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", h)
}

// --- Errors ---

var (
	ErrInvalidRole      = errors.New("jointoken: invalid role")
	ErrInsufficientRole = errors.New("jointoken: insufficient role to issue this token")
	ErrMissingUserInfo  = errors.New("jointoken: email and display_name required for create-on-exchange tokens")
	ErrInvalidToken     = errors.New("jointoken: invalid token")
)

// ValidateTokenFormat checks that a string looks like a join token.
// Used by the handler before calling Exchange.
func ValidateTokenFormat(token string) bool {
	return strings.HasPrefix(token, Prefix) && len(token) > len(Prefix)
}

// HashToken is exported for the web session resolver to hash cookie values
// before looking them up in the store.
func HashToken(token string) string {
	return hashToken(token)
}

// IsNotFound returns true if the error indicates the token was not found
// or was invalid/expired/used (the generic exchange error).
func IsNotFound(err error) bool {
	return errors.Is(err, tokenstore.ErrJoinTokenInvalid) ||
		errors.Is(err, tokenstore.ErrJoinTokenNotFound)
}

// Ensure *sql.Tx is used (prevents unused import if we refactor later).
var _ = (*sql.Tx)(nil)
