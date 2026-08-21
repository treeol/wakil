// Package auth provides principal resolution for the wakild daemon's Connect
// server (P4b). It maps transport-level peer credentials (Unix-socket UID via
// SO_PEERCRED) to a core.Principal that the service layer uses for tenant
// isolation and authorization.
//
// The resolution is fail-closed: if credentials are missing or do not match a
// known local user, the resolver returns ErrUnauthenticated. The daemon never
// infers EmbeddedPrincipal from the absence of credentials over the network —
// embedded mode calls core services in-process and does not go through the
// Connect handlers.
package auth

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
)

// ErrUnauthenticated is returned when a connection has no valid credentials
// or the credentials do not map to a known principal. Callers MUST translate
// this to Connect CodeUnauthenticated, not CodePermissionDenied — the caller
// has no identity at all, not a wrong one.
var ErrUnauthenticated = errors.New("auth: no valid credentials")

// ErrCredentialAbsent is returned by a sub-resolver when no credential of
// its type is present in the request (e.g. no Cookie header for the web
// session resolver, no SO_PEERCRED for the local resolver). This is distinct
// from ErrUnauthenticated: it means "I cannot resolve, try the next
// resolver in the chain," not "the caller is rejected." Only the
// transport-aware dispatch uses this; it is never returned directly to
// handlers.
var ErrCredentialAbsent = errors.New("auth: credential absent for this resolver")

// ErrInvalidCredential is returned when a credential IS present but is
// invalid (expired, revoked, malformed, not found). This is a hard fail:
// the dispatch must NOT fall through to another resolver. A revoked session
// cookie must not silently fall back to anonymous or another identity.
var ErrInvalidCredential = errors.New("auth: invalid credential")

// PrincipalResolver maps a connection's peer credentials to a core.Principal.
// The Connect handlers receive an injected resolver (not a global function)
// so tests can supply a fixed principal without a real Unix socket.
//
// The resolver is called per-request. Credentials are captured at
// connection-accept time (stable for the connection's lifetime) and passed
// through the request context by the ConnContext hook.
//
// P4b's LocalResolver returns a fixed owner principal for the daemon's own
// UID. Future phases (P4c+) will add DB-backed resolvers that look up user
// memberships and roles at request time; the resolver seam keeps the handler
// code unchanged.
type PrincipalResolver interface {
	// Resolve returns the principal for the given context. The context carries
	// peer credentials (or none, for TCP). Returns ErrUnauthenticated if no
	// valid principal can be resolved.
	Resolve(ctx context.Context) (core.Principal, error)
}

// LocalResolver maps Unix-socket peer UIDs to principals. For P4b, the mapping
// is narrow: only the daemon owner's UID maps to the seeded local user
// (tnt_local/usr_local/owner). All other UIDs are rejected.
//
// The daemon owner UID is captured once at startup (os.Getuid()) rather than
// re-reading it per request. This is the correct behavior for a daemon that
// does not drop privileges.
//
// Future phases (P4c+) will extend this resolver (or add alternatives) to
// support join tokens, API tokens, and OIDC — the resolver seam keeps the
// handler code unchanged.
type LocalResolver struct {
	// ownerUID is the daemon process's effective UID at startup. Only
	// connections from this UID are accepted.
	ownerUID uint32
}

// NewLocalResolver creates a resolver that accepts only the daemon's own
// effective UID. The UID is captured at construction time (startup) via
// os.Geteuid(), which matches the UID returned by SO_PEERCRED for a process
// connecting from the same user. If the daemon ever drops privileges (setuid),
// this assumption breaks — a precondition that is not currently enforced.
func NewLocalResolver() *LocalResolver {
	return &LocalResolver{ownerUID: uint32(os.Geteuid())}
}

// NewLocalResolverWithUID creates a resolver that accepts the given UID.
// Used in tests where os.Getuid() is not the connecting process.
func NewLocalResolverWithUID(uid uint32) *LocalResolver {
	return &LocalResolver{ownerUID: uid}
}

// Resolve implements PrincipalResolver. It reads peer credentials from the
// context (placed there by the ConnContext hook) and maps the UID to the
// local owner principal.
func (r *LocalResolver) Resolve(ctx context.Context) (core.Principal, error) {
	creds, ok := PeerCredentialsFromContext(ctx)
	if !ok {
		return core.Principal{}, ErrUnauthenticated
	}
	if creds.UID != r.ownerUID {
		return core.Principal{}, fmt.Errorf("%w: uid %d does not match daemon owner uid %d",
			ErrUnauthenticated, creds.UID, r.ownerUID)
	}
	return core.Principal{
		TenantID:   event.EmbeddedTenantID,
		UserID:     event.EmbeddedUserID,
		Role:       core.RoleOwner,
		AuthMethod: core.AuthLocal,
	}, nil
}
