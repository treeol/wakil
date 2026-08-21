package connect

import (
	"context"
	"errors"

	"github.com/treeol/wakil/internal/auth"
	"github.com/treeol/wakil/internal/core"
)

// principalResolver is injected into the Connect server and used by every
// handler to resolve the caller's principal from the request context. It
// replaces the P2 localPrincipal() stub.
//
// In P4b, the production resolver maps Unix-socket peer UIDs (SO_PEERCRED)
// to core.Principal. Tests inject a fixed resolver that returns a known
// principal without a real Unix socket.
//
// The resolver is fail-closed: if no valid credentials are present (e.g. a
// TCP connection without hosted auth), it returns ErrUnauthenticated and the
// handler rejects the request.
type principalResolver interface {
	Resolve(ctx context.Context) (core.Principal, error)
}

// errUnauthenticated is returned by resolvePrincipal when the resolver
// rejects the caller's credentials. Handlers map this to Connect
// CodeUnauthenticated.
var errUnauthenticated = errors.New("connect: unauthenticated")

// resolvePrincipal extracts the principal from the request context via the
// injected resolver. If the resolver returns auth.ErrUnauthenticated, the
// error is wrapped as errUnauthenticated for the handler to map to
// CodeUnauthenticated. Other resolver errors (e.g. a transient DB failure in
// a future P4c resolver) are returned as-is so the handler can map them to
// CodeInternal rather than masking as unauthenticated.
func resolvePrincipal(ctx context.Context, resolver principalResolver) (core.Principal, error) {
	p, err := resolver.Resolve(ctx)
	if err != nil {
		if errors.Is(err, auth.ErrUnauthenticated) {
			return core.Principal{}, errUnauthenticated
		}
		// Unexpected resolver failure — return as-is so mapError falls
		// through to CodeInternal, not CodeUnauthenticated.
		return core.Principal{}, err
	}
	return p, nil
}
