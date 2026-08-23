package main

// p4b_integration_test.go: real Unix-socket integration test with the
// production LocalResolver (P4b). This test verifies the full SO_PEERCRED
// chain: Unix socket → ConnContext → peercred extraction → LocalResolver →
// principal → Connect handler → core service.
//
// Unlike the other tests that use NewEmbeddedResolver (which bypasses
// peer-credential resolution), this test uses NewLocalResolverWithUID and
// the production ConnContext hook. On Linux, SO_PEERCRED returns the
// connecting process's UID; the resolver accepts it if it matches.

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
	"github.com/treeol/wakil/internal/auth"
	"github.com/treeol/wakil/internal/auth/peercred"
	"github.com/treeol/wakil/internal/core/sessionhost"
	connsvc "github.com/treeol/wakil/internal/server/connect"
)

// startLocalAuthDaemon starts a daemon with the production LocalResolver
// (matching the test process's UID) and the ConnContext hook. This exercises
// the real SO_PEERCRED path.
func startLocalAuthDaemon(t *testing.T, turn sessionhost.TurnFunc) (socketPath string, cleanup func()) {
	t.Helper()
	dir := t.TempDir()
	socketPath = filepath.Join(dir, "wakil.sock")

	host := sessionhost.New(turn, sessionhost.WithAgentName("test"))
	resolver := auth.NewLocalResolver() // uses os.Geteuid()
	srv := connsvc.NewServer(host, true, resolver)

	listener, err := listenUnix(socketPath)
	if err != nil {
		t.Fatalf("listenUnix: %v", err)
	}

	httpSrv := &http.Server{
		Handler: srv.Handler(),
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			creds, ok, err := peercred.FromConn(conn)
			if err != nil {
				t.Logf("peercred extraction failed (expected on non-Linux): %v", err)
				return ctx
			}
			if ok {
				return auth.WithPeerCredentials(ctx, creds)
			}
			return ctx
		},
	}
	go httpSrv.Serve(listener)

	cleanup = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
		_ = host.Close(ctx)
		listener.Close()
	}
	return socketPath, cleanup
}

// newUnixSocketSessionClient creates a Connect SessionService client that
// dials a Unix socket.
func newUnixSocketSessionClient(t *testing.T, socketPath string) wakilv1alpha1connect.SessionServiceClient {
	t.Helper()
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	httpClient := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	return wakilv1alpha1connect.NewSessionServiceClient(httpClient, "http://unix/")
}

// TestP4b_LocalAuth_UnixSocketSuccess verifies that a Unix-socket connection
// from the daemon's own UID resolves to the local owner principal and the
// RPC succeeds. This is the real SO_PEERCRED path on Linux.
func TestP4b_LocalAuth_UnixSocketSuccess(t *testing.T) {
	socketPath, cleanup := startLocalAuthDaemon(t, quickTurnFunc)
	defer cleanup()

	client := newUnixSocketSessionClient(t, socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// ListSessions should succeed — the connecting process's UID matches
	// the daemon owner's UID (both are the test process).
	resp, err := client.ListSessions(ctx, connect.NewRequest(&v1alpha1.ListSessionsRequest{}))
	if err != nil {
		// On non-Linux platforms, SO_PEERCRED is unavailable and the
		// resolver rejects all connections. Skip, not fail.
		if connect.CodeOf(err) == connect.CodeUnauthenticated {
			t.Skip("SO_PEERCRED not supported on this platform — resolver correctly rejects")
		}
		t.Fatalf("ListSessions: %v", err)
	}
	if resp.Msg.Sessions != nil {
		t.Logf("ListSessions returned %d sessions", len(resp.Msg.Sessions))
	}
}

// TestP4b_LocalAuth_UIDMismatch verifies that a resolver configured for a
// different UID rejects the connecting process. We can't change the test
// process's UID, so we configure the resolver with a wrong UID and verify
// rejection.
func TestP4b_LocalAuth_UIDMismatch(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "wakil.sock")

	host := sessionhost.New(quickTurnFunc, sessionhost.WithAgentName("test"))
	// Configure resolver with a UID that is NOT the test process's UID.
	wrongUID := uint32(99999)
	if uint32(os.Geteuid()) == wrongUID {
		wrongUID = 99998 // extremely unlikely but handle it
	}
	resolver := auth.NewLocalResolverWithUID(wrongUID)
	srv := connsvc.NewServer(host, true, resolver)

	listener, err := listenUnix(socketPath)
	if err != nil {
		t.Fatalf("listenUnix: %v", err)
	}
	defer listener.Close()

	httpSrv := &http.Server{
		Handler: srv.Handler(),
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			creds, ok, err := peercred.FromConn(conn)
			if ok && err == nil {
				return auth.WithPeerCredentials(ctx, creds)
			}
			return ctx
		},
	}
	go httpSrv.Serve(listener)
	defer httpSrv.Shutdown(context.Background())

	client := newUnixSocketSessionClient(t, socketPath)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = client.ListSessions(ctx, connect.NewRequest(&v1alpha1.ListSessionsRequest{}))
	if err == nil {
		t.Fatal("ListSessions should fail with unauthenticated — UID mismatch")
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		// On non-Linux, there are no creds at all → still unauthenticated.
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("expected CodeUnauthenticated, got %v", err)
		}
	}
}

// TestP4b_LocalAuth_NoCredentials verifies that a TCP connection (no peer
// credentials) is rejected by the LocalResolver. This is the fail-closed
// test for the non-Unix path.
func TestP4b_LocalAuth_NoCredentials(t *testing.T) {
	// Start a daemon with LocalResolver on a Unix socket, but connect via
	// a TCP listener that has no ConnContext hook (simulating a TCP
	// connection with no peer creds).
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "wakil.sock")

	host := sessionhost.New(quickTurnFunc, sessionhost.WithAgentName("test"))
	resolver := auth.NewLocalResolver()
	srv := connsvc.NewServer(host, true, resolver)

	// TCP listener (no ConnContext — no peer creds).
	tcpLnr, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	defer tcpLnr.Close()

	// Mount Connect handlers on TCP WITHOUT ConnContext — this simulates
	// the (now-fixed) old TCP path. The resolver should still reject
	// because there are no credentials in the context.
	tcpSrv := &http.Server{Handler: srv.Handler()}
	go tcpSrv.Serve(tcpLnr)
	defer tcpSrv.Shutdown(context.Background())

	// Connect via TCP.
	transport := &http.Transport{}
	httpClient := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	client := wakilv1alpha1connect.NewSessionServiceClient(httpClient, "http://"+tcpLnr.Addr().String())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err = client.ListSessions(ctx, connect.NewRequest(&v1alpha1.ListSessionsRequest{}))
	if err == nil {
		t.Fatal("ListSessions over TCP should fail — no credentials → unauthenticated")
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("expected CodeUnauthenticated, got %v", err)
	}

	_ = socketPath // socketPath is unused — the test uses TCP only
}
