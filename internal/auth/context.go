package auth

import (
	"context"

	"github.com/treeol/wakil/internal/auth/peercred"
)

// peerCredsKey is the context key for peer credentials extracted at
// connection-accept time. It is unexported to prevent accidental misuse.
// WithPeerCredentials is exported because the ConnContext hook (in
// cmd/wakild) needs to call it; remote clients cannot inject credentials
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
