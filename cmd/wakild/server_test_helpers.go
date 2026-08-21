package main

// server_test_helpers.go: shared helpers for tests that need a Connect server
// with principal resolution and ConnContext (P4b).

import (
	"context"
	"net"
	"net/http"

	"github.com/treeol/wakil/internal/auth/peercred"
	connsvc "github.com/treeol/wakil/internal/server/connect"
)

// newTestServer creates a Connect server with the embedded (test-only)
// principal resolver. Used by tests that exercise Connect RPCs over a Unix
// socket without needing real SO_PEERCRED resolution.
func newTestServer(srv *connsvc.Server) *http.Server {
	return &http.Server{
		Handler: srv.Handler(),
		// ConnContext is a no-op here — the embedded resolver ignores peer
		// credentials and always returns EmbeddedPrincipal. Production code
		// uses the LocalResolver, which requires credentials in context.
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			creds, ok, err := peercred.FromConn(conn)
			if ok && err == nil {
				// Stash credentials even in test mode — tests that want
				// to verify SO_PEERCRED can inspect them.
				return withTestPeerCreds(ctx, creds)
			}
			return ctx
		},
	}
}

// withTestPeerCreds stores peer creds in context under a test-only key.
// The embedded resolver ignores this; it's available for tests that inspect
// the resolved identity.
func withTestPeerCreds(ctx context.Context, creds peercred.Credentials) context.Context {
	return context.WithValue(ctx, testPeerCredsKey{}, creds)
}

type testPeerCredsKey struct{}
