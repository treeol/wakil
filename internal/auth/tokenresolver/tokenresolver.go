// Package tokenresolver implements the PrincipalResolver for web session
// cookie authentication (P4c). It reads the Cookie header from the request
// context (injected by the auth interceptor middleware), extracts the
// wakild session cookie, hashes it, and looks up the web_sessions table.
// The current membership role is read from the memberships table at
// resolve time — role changes take effect immediately.
//
// This resolver is transport-aware: it only applies to TCP (hosted)
// connections. It returns ErrCredentialAbsent when no cookie is present
// (allowing the dispatch to try another resolver) and ErrInvalidCredential
// when a cookie is present but invalid/expired/revoked (hard fail — no
// fallthrough).
package tokenresolver

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/auth"
	"github.com/treeol/wakil/internal/auth/apitoken"
	"github.com/treeol/wakil/internal/auth/jointoken"
	"github.com/treeol/wakil/internal/auth/tokenstore"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
)

// CookieName is the name of the wakild session cookie.
const CookieName = "wakild_session"

// WebSessionResolver resolves browser session cookies to principals.
// It implements auth.PrincipalResolver.
type WebSessionResolver struct {
	store *tokenstore.Store
}

// New creates a web session resolver backed by the given store.
func New(store *tokenstore.Store) *WebSessionResolver {
	return &WebSessionResolver{store: store}
}

// Resolve implements auth.PrincipalResolver. It reads the Cookie header
// from the context, extracts the session cookie, looks it up in the DB,
// and resolves the principal with the current membership role.
//
// Returns:
//   - (Principal, nil) on success
//   - (_, ErrCredentialAbsent) if no cookie is present (try next resolver)
//   - (_, ErrInvalidCredential) if a cookie is present but invalid (hard fail)
//   - (_, other error) on DB failure (internal error)
func (r *WebSessionResolver) Resolve(ctx context.Context) (core.Principal, error) {
	headers, ok := auth.HTTPHeadersFromContext(ctx)
	if !ok {
		// No HTTP headers in context — this resolver doesn't apply.
		return core.Principal{}, auth.ErrCredentialAbsent
	}

	cookieStr := readSessionCookie(headers)
	if cookieStr == "" {
		// No session cookie present — not our credential type.
		return core.Principal{}, auth.ErrCredentialAbsent
	}

	// A cookie IS present — we must validate it. If invalid, hard fail.
	// Do NOT fall through to another resolver on invalid cookie.
	tokenHash := jointoken.HashToken(cookieStr)

	now := time.Now().UnixNano()
	session, err := r.store.LookupWebSession(ctx, tokenHash, now)
	if err != nil {
		if errors.Is(err, tokenstore.ErrSessionInvalid) {
			return core.Principal{}, auth.ErrInvalidCredential
		}
		// DB failure — internal error, not authentication failure.
		return core.Principal{}, err
	}

	// Read the current membership role (not cached in session).
	role, err := r.store.LookupMembershipRole(ctx, session.TenantID, session.UserID)
	if err != nil {
		if errors.Is(err, tokenstore.ErrMembershipNotFound) {
			// Membership was deleted — session is no longer valid.
			return core.Principal{}, auth.ErrInvalidCredential
		}
		return core.Principal{}, err
	}

	// Verify user is still active.
	if err := r.store.CheckUserActive(ctx, session.UserID); err != nil {
		if errors.Is(err, tokenstore.ErrUserSuspended) || errors.Is(err, tokenstore.ErrUserNotFound) {
			return core.Principal{}, auth.ErrInvalidCredential
		}
		return core.Principal{}, err
	}

	// Verify tenant is still active.
	if err := r.store.CheckTenantActive(ctx, session.TenantID); err != nil {
		if errors.Is(err, tokenstore.ErrTenantSuspended) || errors.Is(err, tokenstore.ErrTenantNotFound) {
			return core.Principal{}, auth.ErrInvalidCredential
		}
		return core.Principal{}, err
	}

	// Touch the session (sliding window) — best-effort, not in the auth
	// critical path. A failure here is logged but does not reject the
	// request.
	_ = r.store.TouchWebSession(ctx, session.ID, now, now+int64(24*60*60*1e9))

	return core.Principal{
		TenantID:   event.TenantID(session.TenantID),
		UserID:     event.UserID(session.UserID),
		Role:       core.Role(role),
		AuthMethod: core.AuthSession,
	}, nil
}

// readSessionCookie extracts the wakild_session cookie from the Cookie
// header. Returns "" if not present.
func readSessionCookie(h http.Header) string {
	cookieHeader := h.Get("Cookie")
	if cookieHeader == "" {
		return ""
	}
	// Parse cookies from the header.
	cookies := (&http.Request{Header: http.Header{"Cookie": []string{cookieHeader}}}).Cookies()
	for _, c := range cookies {
		if c.Name == CookieName && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// --- API token resolver (P4d) ---

// APITokenResolver resolves Bearer tokens to principals. It reads the
// Authorization header from the request context (injected by the auth
// interceptor middleware), extracts the Bearer token, hashes it, and looks
// up the api_tokens table. The current membership role is read from the
// memberships table at resolve time — role changes take effect immediately.
//
// This resolver is transport-aware: it applies to TCP (hosted) connections.
// It returns ErrCredentialAbsent when no Bearer token is present (allowing
// the dispatch to try another resolver) and ErrInvalidCredential when a
// Bearer token is present but invalid/expired/revoked (hard fail — no
// fallthrough).
type APITokenResolver struct {
	store *tokenstore.Store
}

// NewAPIResolver creates an API token resolver backed by the given store.
func NewAPIResolver(store *tokenstore.Store) *APITokenResolver {
	return &APITokenResolver{store: store}
}

// Resolve implements auth.PrincipalResolver. It reads the Authorization
// header from the context, extracts the Bearer token, and only claims it
// if it matches the API token format (tok_ prefix). Non-tok_ Bearer tokens
// (e.g. OIDC JWTs) are left for other resolvers.
//
// Returns:
//   - (Principal, nil) on success
//   - (_, ErrCredentialAbsent) if no Bearer token is present or the token
//     doesn't match the tok_ format (try next resolver)
//   - (_, ErrInvalidCredential) if a tok_ token is present but invalid
//     (hard fail — no fallthrough)
//   - (_, other error) on DB failure (internal error)
func (r *APITokenResolver) Resolve(ctx context.Context) (core.Principal, error) {
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
	// Do NOT fall through to another resolver on invalid token.
	tokenHash := apitoken.HashToken(bearerToken)

	now := time.Now().UnixNano()
	tokenRow, err := r.store.LookupAPIToken(ctx, tokenHash, now)
	if err != nil {
		if errors.Is(err, tokenstore.ErrAPITokenInvalid) {
			return core.Principal{}, auth.ErrInvalidCredential
		}
		// DB failure — internal error, not authentication failure.
		return core.Principal{}, err
	}

	// Read the current membership role (not cached in token).
	role, err := r.store.LookupMembershipRole(ctx, tokenRow.TenantID, tokenRow.UserID)
	if err != nil {
		if errors.Is(err, tokenstore.ErrMembershipNotFound) {
			// Membership was deleted — token is no longer valid.
			return core.Principal{}, auth.ErrInvalidCredential
		}
		return core.Principal{}, err
	}

	// Verify user is still active.
	if err := r.store.CheckUserActive(ctx, tokenRow.UserID); err != nil {
		if errors.Is(err, tokenstore.ErrUserSuspended) || errors.Is(err, tokenstore.ErrUserNotFound) {
			return core.Principal{}, auth.ErrInvalidCredential
		}
		return core.Principal{}, err
	}

	// Verify tenant is still active.
	if err := r.store.CheckTenantActive(ctx, tokenRow.TenantID); err != nil {
		if errors.Is(err, tokenstore.ErrTenantSuspended) || errors.Is(err, tokenstore.ErrTenantNotFound) {
			return core.Principal{}, auth.ErrInvalidCredential
		}
		return core.Principal{}, err
	}

	// Touch the token (last_used_at) — best-effort, not in the auth critical
	// path. A failure here is logged but does not reject the request.
	_ = r.store.TouchAPIToken(ctx, tokenRow.ID, now)

	scopes, err := apitoken.ScopesFromJSON(tokenRow.ScopesJSON)
	if err != nil {
		// Malformed scopes JSON — fail closed (do not inherit role).
		return core.Principal{}, auth.ErrInvalidCredential
	}

	return core.Principal{
		TenantID:   event.TenantID(tokenRow.TenantID),
		UserID:     event.UserID(tokenRow.UserID),
		Role:       core.Role(role),
		Scopes:     scopes,
		AuthMethod: core.AuthAPIToken,
	}, nil
}

// readBearerToken extracts the Bearer token from the Authorization header.
// Returns "" if not present, not a Bearer token, or not an API token
// (doesn't match the tok_ prefix). Non-tok_ Bearer tokens (e.g. OIDC JWTs)
// are left for other resolvers to claim — this is credential-type
// disambiguation, not validation.
func readBearerToken(h http.Header) string {
	authHeader := h.Get("Authorization")
	if authHeader == "" {
		return ""
	}
	// Expected: "Bearer tok_..."
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return ""
	}
	token := strings.TrimPrefix(authHeader, prefix)
	if token == "" {
		return ""
	}
	token = strings.TrimSpace(token)
	// Only claim API tokens (tok_ prefix). Non-tok_ Bearer tokens (e.g.
	// OIDC JWTs) are not our credential type — return empty so the
	// MultiResolver can try the next resolver (OIDC).
	if !apitoken.ValidateTokenFormat(token) {
		return ""
	}
	return token
}
