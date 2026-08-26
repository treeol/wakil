package auth

import (
	"context"
	"net/http"

	"github.com/treeol/wakil/internal/auth/peercred"
)

// peerCredsKey is the context key for peer credentials extracted at
// connection-accept time. It is unexported to prevent accidental misuse.
// WithPeerCredentials is exported because the ConnContext hook (in
// cmd/wakil/daemon_server.go) needs to call it; remote clients cannot inject credentials
// through the context — they arrive only via SO_PEERCRED at the transport
// layer.
type peerCredsKey struct{}

// WithPeerCredentials stores peer credentials in the context. Called by the
// ConnContext hook at connection-accept time.
func WithPeerCredentials(ctx context.Context, creds peercred.Credentials) context.Context {
	return context.WithValue(ctx, peerCredsKey{}, creds)
}

// PeerCredentialsFromContext extracts peer credentials from the context.
// Returns (creds, true) if present, (zero, false) otherwise. The caller
// (PrincipalResolver) MUST fail closed when ok is false.
func PeerCredentialsFromContext(ctx context.Context) (peercred.Credentials, bool) {
	creds, ok := ctx.Value(peerCredsKey{}).(peercred.Credentials)
	return creds, ok
}

// httpHeadersKey is the context key for HTTP request headers. A middleware
// interceptor injects the request's headers into the context before the
// handler runs, so the token/cookie resolver can read Cookie and
// Authorization headers without the PrincipalResolver interface changing.
type httpHeadersKey struct{}

// WithHTTPHeaders stores the HTTP request headers in the context. Called by
// the auth interceptor middleware (which wraps the Connect handler) so the
// token resolver can read Cookie and Authorization headers.
func WithHTTPHeaders(ctx context.Context, h http.Header) context.Context {
	return context.WithValue(ctx, httpHeadersKey{}, h)
}

// HTTPHeadersFromContext extracts HTTP request headers from the context.
// Returns (header, true) if present, (nil, false) otherwise.
func HTTPHeadersFromContext(ctx context.Context) (http.Header, bool) {
	h, ok := ctx.Value(httpHeadersKey{}).(http.Header)
	return h, ok
}
