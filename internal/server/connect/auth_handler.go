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
	issuer     *jointoken.Issuer
	store      *tokenstore.Store
	resolver   principalResolver
	cookieName string // session cookie name (from tokenresolver.CookieName)
}

// Compile-time assertion.
var _ wakilv1alpha1connect.AuthServiceHandler = (*AuthHandler)(nil)

// NewAuthHandler creates an auth handler.
func NewAuthHandler(issuer *jointoken.Issuer, store *tokenstore.Store, resolver principalResolver) *AuthHandler {
	return &AuthHandler{
		issuer:     issuer,
		store:      store,
		resolver:   resolver,
		cookieName: "wakild_session",
	}
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

	if err := h.issuer.Revoke(ctx, req.Msg.Id); err != nil {
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
	setSessionCookie(resp, h.cookieName, result.SessionCookie)

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
	clearSessionCookie(resp, h.cookieName)
	return resp, nil
}

// --- Helpers ---

// setSessionCookie adds a Set-Cookie header to the Connect response.
func setSessionCookie(resp *connect.Response[v1alpha1.ExchangeJoinTokenResponse], name, value string) {
	cookie := &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   7 * 24 * 60 * 60, // 7 days
	}
	resp.Header().Set("Set-Cookie", cookie.String())
}

// clearSessionCookie adds a Set-Cookie header that expires immediately.
func clearSessionCookie(resp *connect.Response[v1alpha1.LogoutResponse], name string) {
	cookie := &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
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
