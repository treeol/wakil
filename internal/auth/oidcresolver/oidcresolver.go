// Package oidcresolver implements the PrincipalResolver for OIDC
// authentication (P4e). It reads a Bearer JWT from the Authorization header,
// validates it via a pluggable TokenValidator, extracts the `sub` claim,
// and looks up the user by auth_subject in the database.
//
// When no OIDC provider is configured (validator is nil), the resolver
// returns ErrCredentialAbsent — it is disabled, not an error. This lets
// the MultiResolver chain skip it and try the next resolver.
//
// The resolver is transport-aware: it applies to TCP (hosted) connections.
// It returns ErrCredentialAbsent when no Bearer token is present (allowing
// the dispatch to try another resolver) and ErrInvalidCredential when a
// Bearer token is present but invalid (hard fail — no fallthrough).
//
// Auto-provisioning: on first OIDC login, if the `sub` claim does not match
// any existing user, a new user is created with the auth_subject set and a
// default `member` membership in the configured tenant. This is controlled
// by the AutoProvision config field.
package oidcresolver

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/treeol/wakil/internal/auth"
	"github.com/treeol/wakil/internal/auth/tokenstore"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
)

// TokenValidator validates an OIDC JWT Bearer token and extracts the claims
// needed for principal resolution. Implementations are responsible for:
//   - Verifying the JWT signature (JWKS fetch, signature verification)
//   - Verifying the issuer (iss claim matches configured issuer)
//   - Verifying the audience (aud claim matches configured client_id)
//   - Checking token expiry (exp claim)
//   - Extracting the `sub` claim (the user's stable IdP identifier)
//
// When OIDC is not configured, the validator is nil and the resolver returns
// ErrCredentialAbsent for all requests.
type TokenValidator interface {
	// Validate validates a raw JWT string and returns the extracted claims.
	// Returns an error if the token is invalid, expired, or has wrong
	// issuer/audience.
	Validate(ctx context.Context, rawToken string) (*Claims, error)
}

// Claims are the OIDC claims needed for principal resolution.
type Claims struct {
	// Subject is the `sub` claim — the user's stable identifier at the IdP.
	// This maps to users.auth_subject in the database.
	Subject string
	// Email is the `email` claim (optional). Used for auto-provisioning.
	Email string
	// DisplayName is the `name` claim (optional). Used for auto-provisioning.
	DisplayName string
}

// Config holds the OIDC resolver configuration. When Issuer is empty, OIDC
// is disabled and the resolver returns ErrCredentialAbsent.
type Config struct {
	// Issuer is the OIDC issuer URL (e.g. "https://auth.example.com/").
	// Empty = OIDC disabled.
	Issuer string
	// ClientID is the OIDC client ID (used for audience verification).
	ClientID string
	// ClientSecret is the OIDC client secret (used for token exchange).
	ClientSecret string
	// RedirectURI is the callback URL for the authorization code flow.
	RedirectURI string
	// DefaultTenantID is the tenant new OIDC users are provisioned in.
	DefaultTenantID string
	// DefaultRole is the role new OIDC users get (default: "member").
	DefaultRole string
	// AutoProvision controls whether new OIDC users are auto-created on
	// first login. If false, an unknown `sub` is a hard fail.
	AutoProvision bool
}

// Resolver resolves OIDC JWT Bearer tokens to principals.
type Resolver struct {
	validator TokenValidator
	store     *tokenstore.Store
	config    Config
}

// New creates an OIDC resolver. If validator is nil, the resolver is
// disabled — it returns ErrCredentialAbsent for all requests.
func New(store *tokenstore.Store, validator TokenValidator, config Config) *Resolver {
	if config.DefaultRole == "" {
		config.DefaultRole = "member"
	}
	return &Resolver{
		validator: validator,
		store:     store,
		config:    config,
	}
}

// Resolve implements auth.PrincipalResolver. It reads the Authorization
// header from the context, extracts the Bearer JWT, validates it via the
// TokenValidator, and resolves the principal by looking up the `sub` claim
// in the users table.
//
// Returns:
//   - (Principal, nil) on success
//   - (_, ErrCredentialAbsent) if OIDC is disabled (no validator) or no
//     Bearer token is present (try next resolver)
//   - (_, ErrInvalidCredential) if a Bearer token is present but invalid
//     (hard fail — no fallthrough)
//   - (_, other error) on DB failure or auto-provisioning failure
func (r *Resolver) Resolve(ctx context.Context) (core.Principal, error) {
	// If no validator is configured OR no issuer is set, OIDC is disabled.
	// Both conditions must be true for the resolver to be active.
	if r.validator == nil || r.config.Issuer == "" {
		return core.Principal{}, auth.ErrCredentialAbsent
	}

	headers, ok := auth.HTTPHeadersFromContext(ctx)
	if !ok {
		// No HTTP headers in context — this resolver doesn't apply.
		return core.Principal{}, auth.ErrCredentialAbsent
	}

	bearerToken := readBearerToken(headers)
	if bearerToken == "" {
		// No Bearer token present — not our credential type.
		return core.Principal{}, auth.ErrCredentialAbsent
	}

	// A Bearer token IS present — we must validate it. If invalid, hard fail.
	claims, err := r.validator.Validate(ctx, bearerToken)
	if err != nil {
		return core.Principal{}, fmt.Errorf("%w: oidc token validation: %v", auth.ErrInvalidCredential, err)
	}

	if claims.Subject == "" {
		return core.Principal{}, fmt.Errorf("%w: oidc token has empty sub claim", auth.ErrInvalidCredential)
	}

	// Look up the user by auth_subject.
	user, err := r.store.LookupUserByAuthSubject(ctx, claims.Subject)
	if err != nil {
		if errors.Is(err, tokenstore.ErrUserNotFound) {
			// User not found — try auto-provisioning if enabled.
			if r.config.AutoProvision {
				return r.provisionUser(ctx, claims)
			}
			// Unknown `sub` — hard fail (do not fall through to another
			// resolver; the caller presented an OIDC credential).
			return core.Principal{}, fmt.Errorf("%w: oidc subject not found and auto-provisioning disabled", auth.ErrInvalidCredential)
		}
		return core.Principal{}, err
	}

	return r.resolvePrincipalFromUser(ctx, user)
}

// provisionUser creates a new user from OIDC claims and resolves the principal.
// The role is hardcoded to "member" regardless of config.DefaultRole —
// auto-provisioned OIDC users are always low-privilege. If elevated roles
// are needed, an admin must change the membership after login.
// A DefaultTenantID must be explicitly configured; there is no silent
// fallback to tnt_local.
func (r *Resolver) provisionUser(ctx context.Context, claims *Claims) (core.Principal, error) {
	email := claims.Email
	if email == "" {
		email = claims.Subject + "@oidc.local"
	}
	displayName := claims.DisplayName
	if displayName == "" {
		displayName = claims.Subject
	}

	tenantID := r.config.DefaultTenantID
	if tenantID == "" {
		// No explicit tenant configured — reject provisioning rather than
		// silently falling back to tnt_local (which would put external IdP
		// users in the seeded local tenant).
		return core.Principal{}, fmt.Errorf("%w: oidc auto-provisioning requires DefaultTenantID", auth.ErrInvalidCredential)
	}

	// Auto-provisioned users are always "member" — never owner or admin.
	role := "member"

	userID := "usr_" + uuid.NewString()

	tx, err := r.store.BeginTx(ctx)
	if err != nil {
		return core.Principal{}, fmt.Errorf("oidc: begin tx for provisioning: %w", err)
	}
	defer tx.Rollback()

	if err := r.store.CreateUserWithAuthSubject(ctx, tx, userID, email, displayName, claims.Subject, tenantID, role); err != nil {
		return core.Principal{}, fmt.Errorf("oidc: provision user: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return core.Principal{}, fmt.Errorf("oidc: commit provisioning: %w", err)
	}

	// Verify tenant is active.
	if err := r.store.CheckTenantActive(ctx, tenantID); err != nil {
		if errors.Is(err, tokenstore.ErrTenantSuspended) || errors.Is(err, tokenstore.ErrTenantNotFound) {
			return core.Principal{}, fmt.Errorf("%w: oidc provisioned user tenant inactive", auth.ErrInvalidCredential)
		}
		return core.Principal{}, err
	}

	return core.Principal{
		TenantID:   event.TenantID(tenantID),
		UserID:     event.UserID(userID),
		Role:       core.Role(role),
		AuthMethod: core.AuthOIDC,
	}, nil
}

// resolvePrincipalFromUser resolves a principal from an existing user row.
// It reads the current membership role and verifies the user/tenant are active.
func (r *Resolver) resolvePrincipalFromUser(ctx context.Context, user *tokenstore.UserRow) (core.Principal, error) {
	tenantID := r.config.DefaultTenantID
	if tenantID == "" {
		// No explicit tenant configured — cannot resolve. This should not
		// happen in a correctly configured deployment, but we fail closed
		// rather than guessing tnt_local.
		return core.Principal{}, fmt.Errorf("%w: oidc resolver requires DefaultTenantID", auth.ErrInvalidCredential)
	}

	role, err := r.store.LookupMembershipRole(ctx, tenantID, user.ID)
	if err != nil {
		if errors.Is(err, tokenstore.ErrMembershipNotFound) {
			return core.Principal{}, fmt.Errorf("%w: oidc user has no membership", auth.ErrInvalidCredential)
		}
		return core.Principal{}, err
	}

	// Verify user is still active.
	if err := r.store.CheckUserActive(ctx, user.ID); err != nil {
		if errors.Is(err, tokenstore.ErrUserSuspended) || errors.Is(err, tokenstore.ErrUserNotFound) {
			return core.Principal{}, auth.ErrInvalidCredential
		}
		return core.Principal{}, err
	}

	// Verify tenant is still active.
	if err := r.store.CheckTenantActive(ctx, tenantID); err != nil {
		if errors.Is(err, tokenstore.ErrTenantSuspended) || errors.Is(err, tokenstore.ErrTenantNotFound) {
			return core.Principal{}, auth.ErrInvalidCredential
		}
		return core.Principal{}, err
	}

	return core.Principal{
		TenantID:   event.TenantID(tenantID),
		UserID:     event.UserID(user.ID),
		Role:       core.Role(role),
		AuthMethod: core.AuthOIDC,
	}, nil
}

// readBearerToken extracts the Bearer token from the Authorization header.
// Returns "" if not present or not a Bearer token. A token starting with
// "tok_" is an API token, not a JWT — it is ignored here so the API token
// resolver can handle it.
func readBearerToken(h http.Header) string {
	authHeader := h.Get("Authorization")
	if authHeader == "" {
		return ""
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return ""
	}
	token := strings.TrimPrefix(authHeader, prefix)
	if token == "" {
		return ""
	}
	token = strings.TrimSpace(token)
	// API tokens (tok_ prefix) are handled by the API token resolver.
	// Ignore them here so the MultiResolver chain can pass them through.
	if strings.HasPrefix(token, "tok_") {
		return ""
	}
	return token
}

// IsConfigured returns true if OIDC is configured (validator is set and
// issuer is non-empty).
func (r *Resolver) IsConfigured() bool {
	return r.validator != nil && r.config.Issuer != ""
}
