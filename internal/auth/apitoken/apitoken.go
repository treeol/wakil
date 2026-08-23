// Package apitoken implements API token generation, issuance, and management
// for the wakil daemon (P4d).
//
// API tokens are long-lived, scoped credentials for machine/CI clients. They
// are SHA-256 hashed at rest (plaintext shown once at creation). Unlike join
// tokens, API tokens are reusable and revocable. They do not bootstrap users —
// the caller must already be authenticated (via session cookie or local
// resolver) to create/list/revoke API tokens.
//
// Token format: tok_<base64url(32 random bytes)> — 256 bits of entropy,
// visually prefixed for log safety. Only the SHA-256 hash is stored.
//
// Scope vocabulary (validated at creation):
//   - sessions:read  — read sessions and events
//   - sessions:write — create/submit/interrupt/close sessions
//   - sessions:admin — delete sessions, manage approvals
//   - tokens:manage  — create/list/revoke API and join tokens
//   - *              — wildcard (all scopes; equivalent to owner role permissions)
//
// Empty scopes means "inherit role permissions" — the token inherits the
// caller's membership role at resolve time, same as a session cookie.
package apitoken

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/treeol/wakil/internal/auth/tokenstore"
)

// Prefix is the visual prefix for API tokens (log safety).
const Prefix = "tok_"

// ValidScopes is the set of recognized scope strings. "*" is the wildcard.
var ValidScopes = map[string]bool{
	"sessions:read":  true,
	"sessions:write": true,
	"sessions:admin": true,
	"tokens:manage":  true,
	"*":              true,
}

// Issuer handles API token creation, listing, and revocation. It wraps the
// tokenstore and provides the business logic: scope validation, token
// generation, authorization checks.
type Issuer struct {
	store *tokenstore.Store
}

// New creates an API token issuer backed by the given store.
func New(store *tokenstore.Store) *Issuer {
	return &Issuer{store: store}
}

// CreateRequest is the input for issuing an API token.
type CreateRequest struct {
	TenantID  string
	UserID    string
	Name      string    // human-readable label
	Scopes    []string  // scope strings; empty = inherit role
	ExpiresAt time.Time // zero = no expiry
}

// CreateResult is the output of a successful token creation.
type CreateResult struct {
	Token     string // plaintext token — shown ONCE, never again
	ID        string
	ExpiresAt time.Time // zero = no expiry
}

// Create issues a new API token for the authenticated caller. The plaintext
// token is returned exactly once; only its SHA-256 hash is stored.
func (i *Issuer) Create(ctx context.Context, req CreateRequest) (*CreateResult, error) {
	if req.Name == "" {
		return nil, ErrMissingName
	}
	if req.TenantID == "" || req.UserID == "" {
		return nil, ErrMissingIdentity
	}

	// Validate scopes.
	for _, s := range req.Scopes {
		if !ValidScopes[s] {
			return nil, fmt.Errorf("%w: %q", ErrInvalidScope, s)
		}
	}

	// Generate a 256-bit random token.
	plaintext, err := generateToken()
	if err != nil {
		return nil, fmt.Errorf("apitoken: generate token: %w", err)
	}
	tokenHash := hashToken(plaintext)

	id := "tok_" + uuid.NewString()

	scopesJSON, err := scopesToJSON(req.Scopes)
	if err != nil {
		return nil, fmt.Errorf("apitoken: marshal scopes: %w", err)
	}

	var expiresAt int64
	if !req.ExpiresAt.IsZero() {
		expiresAt = req.ExpiresAt.UnixNano()
	}

	if err := i.store.CreateAPIToken(ctx, id, req.TenantID, req.UserID, req.Name, tokenHash, scopesJSON, expiresAt); err != nil {
		return nil, err
	}

	return &CreateResult{
		Token:     plaintext,
		ID:        id,
		ExpiresAt: req.ExpiresAt,
	}, nil
}

// List returns API tokens for a tenant, optionally filtered by user_id.
// If userID is empty, returns all tokens for the tenant (admin view).
// Revoked tokens are excluded by default.
func (i *Issuer) List(ctx context.Context, tenantID, userID string) ([]tokenstore.APITokenRow, error) {
	return i.store.ListAPITokens(ctx, tenantID, userID, false)
}

// Revoke revokes an API token by ID within a tenant. Idempotent — revoking
// an already-revoked token is a no-op. The tenant_id predicate ensures the
// caller can only revoke tokens in their own tenant.
func (i *Issuer) Revoke(ctx context.Context, id, tenantID string) error {
	return i.store.RevokeAPIToken(ctx, id, tenantID)
}

// --- Token generation and hashing ---

// generateToken generates a 256-bit random API token with the tok_ prefix.
func generateToken() (string, error) {
	b := make([]byte, 32) // 256 bits
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// hashToken returns the SHA-256 hex of a token string.
func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", h)
}

// HashToken is exported for the API token resolver to hash Bearer values
// before looking them up in the store.
func HashToken(token string) string {
	return hashToken(token)
}

// ValidateTokenFormat checks that a string looks like an API token.
func ValidateTokenFormat(token string) bool {
	return strings.HasPrefix(token, Prefix) && len(token) > len(Prefix)
}

// scopesToJSON converts a scope slice to a JSON array string.
func scopesToJSON(scopes []string) (string, error) {
	if len(scopes) == 0 {
		return "[]", nil
	}
	b, err := json.Marshal(scopes)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ScopesFromJSON parses a JSON array string into a scope slice. Returns
// an error if the JSON is malformed — the caller must fail closed on error
// (treat as invalid credential, not as "inherit role").
func ScopesFromJSON(jsonStr string) ([]string, error) {
	if jsonStr == "" || jsonStr == "[]" {
		return nil, nil
	}
	var scopes []string
	if err := json.Unmarshal([]byte(jsonStr), &scopes); err != nil {
		return nil, fmt.Errorf("apitoken: malformed scopes JSON: %w", err)
	}
	return scopes, nil
}

// --- Errors ---

var (
	ErrMissingName     = errors.New("apitoken: name is required")
	ErrMissingIdentity = errors.New("apitoken: tenant_id and user_id are required")
	ErrInvalidScope    = errors.New("apitoken: invalid scope")
)

// IsNotFound returns true if the error indicates the token was not found.
func IsNotFound(err error) bool {
	return errors.Is(err, tokenstore.ErrAPITokenNotFound)
}
