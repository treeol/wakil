package connect

import (
	"context"
	"errors"
	"net/http"
	"time"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
	"github.com/treeol/wakil/internal/auth"
	"github.com/treeol/wakil/internal/auth/apitoken"
	"github.com/treeol/wakil/internal/auth/jointoken"
	"github.com/treeol/wakil/internal/auth/tokenstore"
	"github.com/treeol/wakil/internal/core"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// AuthHandler implements AuthServiceHandler. It provides join token
// management, exchange, session cookie handling, and WhoAmI/Logout.
//
// Authentication policy:
//   - CreateJoinToken, ListJoinTokens, RevokeJoinToken: authenticated,
//     owner/admin role enforced at the handler level.
//   - ExchangeJoinToken: PUBLIC (unauthenticated).
//   - WhoAmI, Logout: authenticated (any method).
type AuthHandler struct {
	issuer        *jointoken.Issuer
	apiIssuer     *apitoken.Issuer
	store         *tokenstore.Store
	resolver      principalResolver
	cookieName    string // session cookie name (from tokenresolver.CookieName)
	oidcCfg       OIDCConfig
	secureCookies bool // P4f: set Secure flag on session cookies (TLS mode)
}

// OIDCConfig holds the OIDC handler configuration. When Issuer is empty,
// OIDC RPCs return Unimplemented.
type OIDCConfig struct {
	Issuer      string
	ClientID    string
	RedirectURI string
	// AuthURLBuilder is the function that builds the IdP authorization URL.
	// If nil, GetOIDCAuthURL returns Unimplemented.
	AuthURLBuilder func(redirectURI string) (string, error)
}

// Compile-time assertion.
var _ wakilv1alpha1connect.AuthServiceHandler = (*AuthHandler)(nil)

// NewAuthHandler creates an auth handler.
func NewAuthHandler(issuer *jointoken.Issuer, apiIssuer *apitoken.Issuer, store *tokenstore.Store, resolver principalResolver) *AuthHandler {
	return &AuthHandler{
		issuer:     issuer,
		apiIssuer:  apiIssuer,
		store:      store,
		resolver:   resolver,
		cookieName: "wakild_session",
	}
}

// NewAuthHandlerWithSecureCookies creates an auth handler that sets the Secure
// flag on session cookies when secureCookies is true. Used when the TCP
// listener is TLS-enabled (P4f).
func NewAuthHandlerWithSecureCookies(issuer *jointoken.Issuer, apiIssuer *apitoken.Issuer, store *tokenstore.Store, resolver principalResolver, secureCookies bool) *AuthHandler {
	h := NewAuthHandler(issuer, apiIssuer, store, resolver)
	h.secureCookies = secureCookies
	return h
}

// NewAuthHandlerWithOIDC creates an auth handler with OIDC support.
// If oidcCfg.Issuer is empty, OIDC RPCs return Unimplemented.
func NewAuthHandlerWithOIDC(issuer *jointoken.Issuer, apiIssuer *apitoken.Issuer, store *tokenstore.Store, resolver principalResolver, oidcCfg OIDCConfig) *AuthHandler {
	h := NewAuthHandler(issuer, apiIssuer, store, resolver)
	h.oidcCfg = oidcCfg
	return h
}

// NewAuthHandlerWithOIDCAndSecureCookies creates an auth handler with OIDC
// support and the Secure flag on session cookies when secureCookies is true.
func NewAuthHandlerWithOIDCAndSecureCookies(issuer *jointoken.Issuer, apiIssuer *apitoken.Issuer, store *tokenstore.Store, resolver principalResolver, oidcCfg OIDCConfig, secureCookies bool) *AuthHandler {
	h := NewAuthHandlerWithOIDC(issuer, apiIssuer, store, resolver, oidcCfg)
	h.secureCookies = secureCookies
	return h
}

// CreateJoinToken issues a new join token. Owner/admin only.
func (h *AuthHandler) CreateJoinToken(ctx context.Context, req *connect.Request[v1alpha1.CreateJoinTokenRequest]) (*connect.Response[v1alpha1.CreateJoinTokenResponse], error) {
	p, err := resolvePrincipal(ctx, h.resolver)
	if err != nil {
		return nil, mapError(err)
	}

	// Authorization: only owner or admin can issue join tokens.
	if p.Role != core.RoleOwner && p.Role != core.RoleAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only owners and admins can issue join tokens"))
	}

	// Only owners can issue owner-role tokens.
	if req.Msg.Role == "owner" && p.Role != core.RoleOwner {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only owners can issue owner-role tokens"))
	}

	// The tenant_id in the request must match the principal's tenant.
	// (Cross-tenant token issuance is forbidden.)
	if req.Msg.TenantId != string(p.TenantID) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("cannot issue tokens for a different tenant"))
	}

	result, err := h.issuer.Create(ctx, jointoken.CreateRequest{
		TenantID:    req.Msg.TenantId,
		Role:        req.Msg.Role,
		UserID:      req.Msg.UserId,
		Email:       req.Msg.Email,
		DisplayName: req.Msg.DisplayName,
		CreatedBy:   string(p.UserID),
	})
	if err != nil {
		if errors.Is(err, jointoken.ErrInvalidRole) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		if errors.Is(err, jointoken.ErrInsufficientRole) {
			return nil, connect.NewError(connect.CodePermissionDenied, err)
		}
		if errors.Is(err, jointoken.ErrMissingUserInfo) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		return nil, mapError(err)
	}

	return connect.NewResponse(&v1alpha1.CreateJoinTokenResponse{
		Token:     result.Token,
		Id:        result.ID,
		ExpiresAt: timestamppb.New(result.ExpiresAt),
	}), nil
}

// ListJoinTokens returns all join tokens for the principal's tenant.
func (h *AuthHandler) ListJoinTokens(ctx context.Context, req *connect.Request[v1alpha1.ListJoinTokensRequest]) (*connect.Response[v1alpha1.ListJoinTokensResponse], error) {
	p, err := resolvePrincipal(ctx, h.resolver)
	if err != nil {
		return nil, mapError(err)
	}

	// Only owner or admin can list tokens.
	if p.Role != core.RoleOwner && p.Role != core.RoleAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only owners and admins can list join tokens"))
	}

	tokens, err := h.issuer.List(ctx, string(p.TenantID))
	if err != nil {
		return nil, mapError(err)
	}

	pbTokens := make([]*v1alpha1.JoinToken, 0, len(tokens))
	for _, t := range tokens {
		pbTokens = append(pbTokens, joinTokenToProto(t))
	}
	return connect.NewResponse(&v1alpha1.ListJoinTokensResponse{Tokens: pbTokens}), nil
}

// RevokeJoinToken revokes an unused join token.
func (h *AuthHandler) RevokeJoinToken(ctx context.Context, req *connect.Request[v1alpha1.RevokeJoinTokenRequest]) (*connect.Response[v1alpha1.RevokeJoinTokenResponse], error) {
	p, err := resolvePrincipal(ctx, h.resolver)
	if err != nil {
		return nil, mapError(err)
	}

	// Only owner or admin can revoke tokens.
	if p.Role != core.RoleOwner && p.Role != core.RoleAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only owners and admins can revoke join tokens"))
	}

	if err := h.issuer.Revoke(ctx, req.Msg.Id, string(p.TenantID)); err != nil {
		if errors.Is(err, tokenstore.ErrJoinTokenNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, mapError(err)
	}
	return connect.NewResponse(&v1alpha1.RevokeJoinTokenResponse{}), nil
}

// ExchangeJoinToken exchanges a join token for a browser session cookie.
// This is the only PUBLIC (unauthenticated) RPC in the system.
//
// The session cookie is delivered via Set-Cookie header, NOT in the
// response body. The response body carries only the resolved principal
// metadata.
func (h *AuthHandler) ExchangeJoinToken(ctx context.Context, req *connect.Request[v1alpha1.ExchangeJoinTokenRequest]) (*connect.Response[v1alpha1.ExchangeJoinTokenResponse], error) {
	if !jointoken.ValidateTokenFormat(req.Msg.Token) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("invalid token format"))
	}

	result, err := h.issuer.Exchange(ctx, jointoken.ExchangeRequest{
		Token:       req.Msg.Token,
		Email:       req.Msg.Email,
		DisplayName: req.Msg.DisplayName,
	})
	if err != nil {
		// Generic error for all exchange failures — no enumeration of
		// expired vs used vs revoked (security).
		if errors.Is(err, jointoken.ErrInvalidToken) || jointoken.IsNotFound(err) {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("token exchange failed"))
		}
		return nil, mapError(err)
	}

	// Set the session cookie via Set-Cookie header. The response writer is
	// available via the connect.Request's Peer, but Connect doesn't expose
	// the raw ResponseWriter directly. We use Set-Cookie via the response
	// header.
	resp := connect.NewResponse(&v1alpha1.ExchangeJoinTokenResponse{
		Principal: &v1alpha1.Principal{
			TenantId:   result.Principal.TenantID,
			UserId:     result.Principal.UserID,
			Role:       result.Principal.Role,
			AuthMethod: string(core.AuthSession),
		},
	})

	// Set the cookie on the response. Connect's Response supports
	// SetHeader via the response's trailer/header mechanism.
	setSessionCookie(resp, h.cookieName, result.SessionCookie, h.secureCookies)

	return resp, nil
}

// WhoAmI returns the caller's resolved principal.
func (h *AuthHandler) WhoAmI(ctx context.Context, req *connect.Request[v1alpha1.WhoAmIRequest]) (*connect.Response[v1alpha1.WhoAmIResponse], error) {
	p, err := resolvePrincipal(ctx, h.resolver)
	if err != nil {
		return nil, mapError(err)
	}
	return connect.NewResponse(&v1alpha1.WhoAmIResponse{
		Principal: &v1alpha1.Principal{
			TenantId:   string(p.TenantID),
			UserId:     string(p.UserID),
			Role:       string(p.Role),
			AuthMethod: string(p.AuthMethod),
		},
	}), nil
}

// Logout revokes the caller's web session and clears the cookie.
func (h *AuthHandler) Logout(ctx context.Context, req *connect.Request[v1alpha1.LogoutRequest]) (*connect.Response[v1alpha1.LogoutResponse], error) {
	// Read the session cookie from the request headers (in context).
	headers, ok := auth.HTTPHeadersFromContext(ctx)
	if ok {
		cookieStr := readCookieFromHeaders(headers, h.cookieName)
		if cookieStr != "" {
			// Revoke the session by hash.
			tokenHash := jointoken.HashToken(cookieStr)
			_ = h.store.RevokeWebSessionByHash(ctx, tokenHash)
		}
	}

	resp := connect.NewResponse(&v1alpha1.LogoutResponse{})
	// Clear the cookie.
	clearSessionCookie(resp, h.cookieName, h.secureCookies)
	return resp, nil
}

// --- API Token management (P4d) ---

// CreateAPIToken issues a new API token for the authenticated caller.
// The plaintext token is shown ONCE; only its SHA-256 hash is stored.
// Any authenticated role can create API tokens for their own user, but
// API-token-authenticated callers CANNOT create API tokens (prevents
// privilege escalation via delegated tokens). Only session and local
// auth methods may manage API tokens.
func (h *AuthHandler) CreateAPIToken(ctx context.Context, req *connect.Request[v1alpha1.CreateAPITokenRequest]) (*connect.Response[v1alpha1.CreateAPITokenResponse], error) {
	p, err := resolvePrincipal(ctx, h.resolver)
	if err != nil {
		return nil, mapError(err)
	}

	if h.apiIssuer == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("api token management not configured"))
	}

	// API tokens and OIDC tokens cannot create API tokens — prevents
	// privilege escalation (a scoped token or external IdP user minting a
	// broader token). Only session and local auth (interactive/owner
	// credentials) may manage API tokens.
	if p.AuthMethod == core.AuthAPIToken || p.AuthMethod == core.AuthOIDC {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("api tokens cannot manage api tokens; use session or local auth"))
	}

	// Only owners can request the wildcard "*" scope.
	for _, s := range req.Msg.Scopes {
		if s == "*" && p.Role != core.RoleOwner {
			return nil, connect.NewError(connect.CodePermissionDenied, errors.New("only owners can create wildcard-scope tokens"))
		}
	}

	var expiresAt time.Time
	if req.Msg.ExpiresAt != nil {
		expiresAt = req.Msg.ExpiresAt.AsTime()
	}

	result, err := h.apiIssuer.Create(ctx, apitoken.CreateRequest{
		TenantID:  string(p.TenantID),
		UserID:    string(p.UserID),
		Name:      req.Msg.Name,
		Scopes:    req.Msg.Scopes,
		ExpiresAt: expiresAt,
	})
	if err != nil {
		if errors.Is(err, apitoken.ErrMissingName) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		if errors.Is(err, apitoken.ErrMissingIdentity) {
			return nil, connect.NewError(connect.CodeUnauthenticated, err)
		}
		if errors.Is(err, apitoken.ErrInvalidScope) {
			return nil, connect.NewError(connect.CodeInvalidArgument, err)
		}
		return nil, mapError(err)
	}

	resp := connect.NewResponse(&v1alpha1.CreateAPITokenResponse{
		Token: result.Token,
		Id:    result.ID,
	})
	if !result.ExpiresAt.IsZero() {
		resp.Msg.ExpiresAt = timestamppb.New(result.ExpiresAt)
	}
	return resp, nil
}

// ListAPITokens returns API tokens for the caller's tenant. If user_id is
// empty, returns the caller's own tokens. Admins/owners can list any user's
// tokens by specifying user_id. API-token-authenticated callers cannot list
// tokens (prevents enumeration via delegated tokens).
func (h *AuthHandler) ListAPITokens(ctx context.Context, req *connect.Request[v1alpha1.ListAPITokensRequest]) (*connect.Response[v1alpha1.ListAPITokensResponse], error) {
	p, err := resolvePrincipal(ctx, h.resolver)
	if err != nil {
		return nil, mapError(err)
	}

	if h.apiIssuer == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("api token management not configured"))
	}

	// API tokens and OIDC tokens cannot manage API tokens.
	if p.AuthMethod == core.AuthAPIToken || p.AuthMethod == core.AuthOIDC {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("api tokens cannot manage api tokens; use session or local auth"))
	}

	// Authorization: non-admins can only list their own tokens.
	targetUserID := req.Msg.UserId
	if targetUserID == "" {
		targetUserID = string(p.UserID)
	}
	if targetUserID != string(p.UserID) && p.Role != core.RoleOwner && p.Role != core.RoleAdmin {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("can only list your own tokens"))
	}

	tokens, err := h.apiIssuer.List(ctx, string(p.TenantID), targetUserID)
	if err != nil {
		return nil, mapError(err)
	}

	pbTokens := make([]*v1alpha1.APIToken, 0, len(tokens))
	for _, t := range tokens {
		pbTokens = append(pbTokens, apiTokenToProto(t))
	}
	return connect.NewResponse(&v1alpha1.ListAPITokensResponse{Tokens: pbTokens}), nil
}

// RevokeAPIToken revokes an API token by ID. Users can revoke their own
// tokens; admins/owners can revoke any user's tokens within the tenant.
// API-token-authenticated callers cannot revoke tokens.
func (h *AuthHandler) RevokeAPIToken(ctx context.Context, req *connect.Request[v1alpha1.RevokeAPITokenRequest]) (*connect.Response[v1alpha1.RevokeAPITokenResponse], error) {
	p, err := resolvePrincipal(ctx, h.resolver)
	if err != nil {
		return nil, mapError(err)
	}

	if h.apiIssuer == nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("api token management not configured"))
	}

	// API tokens and OIDC tokens cannot manage API tokens.
	if p.AuthMethod == core.AuthAPIToken || p.AuthMethod == core.AuthOIDC {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("api tokens cannot manage api tokens; use session or local auth"))
	}

	// Revoke is tenant-scoped: the store's RevokeAPITokenByTenant includes
	// a tenant_id predicate to prevent cross-tenant revocation.
	if err := h.apiIssuer.Revoke(ctx, req.Msg.Id, string(p.TenantID)); err != nil {
		if errors.Is(err, tokenstore.ErrAPITokenNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, mapError(err)
	}
	return connect.NewResponse(&v1alpha1.RevokeAPITokenResponse{}), nil
}

// --- OIDC (P4e) ---

// GetOIDCAuthURL returns the OIDC authorization redirect URL. The caller's
// browser is redirected to this URL to begin the OIDC flow.
//
// This RPC is PUBLIC (unauthenticated) — the caller hasn't authenticated yet.
// Returns Unimplemented when OIDC is not configured.
func (h *AuthHandler) GetOIDCAuthURL(ctx context.Context, req *connect.Request[v1alpha1.GetOIDCAuthURLRequest]) (*connect.Response[v1alpha1.GetOIDCAuthURLResponse], error) {
	if h.oidcCfg.Issuer == "" {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("oidc not configured"))
	}

	redirectURI := req.Msg.RedirectUri
	if redirectURI == "" {
		redirectURI = h.oidcCfg.RedirectURI
	}

	var authURL string
	if h.oidcCfg.AuthURLBuilder != nil {
		u, err := h.oidcCfg.AuthURLBuilder(redirectURI)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		authURL = u
	} else {
		// Without a builder, return Unimplemented — the IdP integration
		// is not wired yet.
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("oidc auth url builder not configured"))
	}

	return connect.NewResponse(&v1alpha1.GetOIDCAuthURLResponse{
		AuthUrl: authURL,
	}), nil
}

// ExchangeOIDCCode exchanges an OIDC authorization code for a session cookie.
// The code is received at the redirect_uri after the user authenticates at
// the IdP.
//
// This RPC is PUBLIC (unauthenticated) — the caller exchanges an IdP code,
// not a wakild credential.
// Returns Unimplemented when OIDC is not configured.
func (h *AuthHandler) ExchangeOIDCCode(ctx context.Context, req *connect.Request[v1alpha1.ExchangeOIDCCodeRequest]) (*connect.Response[v1alpha1.ExchangeOIDCCodeResponse], error) {
	if h.oidcCfg.Issuer == "" {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("oidc not configured"))
	}

	// Without an IdP integration, we cannot exchange the code.
	// When an IdP is configured, this would:
	// 1. Exchange the code for an ID token at the IdP token endpoint
	// 2. Validate the ID token (signature, issuer, audience, expiry)
	// 3. Extract the `sub` claim
	// 4. Look up or provision the user by auth_subject
	// 5. Create a web session and set the cookie
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("oidc code exchange not implemented — configure an IdP"))
}

// --- Helpers ---

// setSessionCookie adds a Set-Cookie header to the Connect response.
// When secure is true, the Secure flag is set so browsers never send the
// cookie over plaintext HTTP (P4f: TLS mode).
func setSessionCookie(resp *connect.Response[v1alpha1.ExchangeJoinTokenResponse], name, value string, secure bool) {
	cookie := &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
		MaxAge:   7 * 24 * 60 * 60, // 7 days
	}
	resp.Header().Set("Set-Cookie", cookie.String())
}

// clearSessionCookie adds a Set-Cookie header that expires immediately.
func clearSessionCookie(resp *connect.Response[v1alpha1.LogoutResponse], name string, secure bool) {
	cookie := &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   secure,
		MaxAge:   -1, // delete
	}
	resp.Header().Set("Set-Cookie", cookie.String())
}

// readCookieFromHeaders extracts a named cookie from HTTP headers.
func readCookieFromHeaders(headers http.Header, name string) string {
	cookieHeader := headers.Get("Cookie")
	if cookieHeader == "" {
		return ""
	}
	parsed := (&http.Request{Header: http.Header{"Cookie": []string{cookieHeader}}}).Cookies()
	for _, c := range parsed {
		if c.Name == name && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// joinTokenToProto converts a tokenstore.JoinTokenRow to a proto JoinToken.
func joinTokenToProto(t tokenstore.JoinTokenRow) *v1alpha1.JoinToken {
	pb := &v1alpha1.JoinToken{
		Id:        t.ID,
		TenantId:  t.TenantID,
		UserId:    t.UserID,
		Role:      t.Role,
		CreatedBy: t.CreatedBy,
	}
	if t.CreatedAt != 0 {
		pb.CreatedAt = timestamppb.New(timeFromNanos(t.CreatedAt))
	}
	if t.ExpiresAt != 0 {
		pb.ExpiresAt = timestamppb.New(timeFromNanos(t.ExpiresAt))
	}
	if t.UsedAt != 0 {
		pb.UsedAt = timestamppb.New(timeFromNanos(t.UsedAt))
	}
	if t.RevokedAt != 0 {
		pb.RevokedAt = timestamppb.New(timeFromNanos(t.RevokedAt))
	}
	return pb
}

// timeFromNanos converts a Unix nanosecond timestamp to a time.Time.
func timeFromNanos(nanos int64) time.Time {
	return time.Unix(0, nanos)
}

// apiTokenToProto converts a tokenstore.APITokenRow to a proto APIToken.
func apiTokenToProto(t tokenstore.APITokenRow) *v1alpha1.APIToken {
	pb := &v1alpha1.APIToken{
		Id:       t.ID,
		TenantId: t.TenantID,
		UserId:   t.UserID,
		Name:     t.Name,
	}
	scopes, err := apitoken.ScopesFromJSON(t.ScopesJSON)
	if err != nil {
		// Malformed scopes — leave empty (fail safe, don't show potentially
		// corrupted scope data).
		scopes = nil
	}
	pb.Scopes = scopes
	if t.CreatedAt != 0 {
		pb.CreatedAt = timestamppb.New(timeFromNanos(t.CreatedAt))
	}
	if t.ExpiresAt != 0 {
		pb.ExpiresAt = timestamppb.New(timeFromNanos(t.ExpiresAt))
	}
	if t.LastUsedAt != 0 {
		pb.LastUsedAt = timestamppb.New(timeFromNanos(t.LastUsedAt))
	}
	if t.RevokedAt != 0 {
		pb.RevokedAt = timestamppb.New(timeFromNanos(t.RevokedAt))
	}
	return pb
}
