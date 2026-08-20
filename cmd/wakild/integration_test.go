package main

// integration_test.go: integration tests for the wakild daemon (card #148 P2d).
//
// These tests exercise the daemon's HTTP server over a real Unix socket,
// hitting the Health and GetServerInfo RPCs. They use the Connect client
// to verify the wire path end-to-end (without needing a real backend).

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	wakilv1alpha1connect "github.com/treeol/wakil/api/gen/wakil/v1alpha1/wakilv1alpha1connect"
	"github.com/treeol/wakil/internal/core/sessionhost"
	connsvc "github.com/treeol/wakil/internal/server/connect"
)

// TestHealthRPC verifies the Health RPC is served over the Unix socket and
// returns a "ready" status.
func TestHealthRPC(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "wakild.sock")

	// Build a Connect server with an ephemeral host (no store).
	// SystemService handler doesn't call into the host, so a nil TurnFunc
	// (lifecycle-only) host is sufficient.
	host := sessionhost.New(nil, sessionhost.WithAgentName("test"))
	srv := connsvc.NewServer(host, true) // ephemeral=true

	listener, err := listenUnix(socketPath)
	if err != nil {
		t.Fatalf("listenUnix: %v", err)
	}
	defer listener.Close()

	httpSrv := &http.Server{Handler: srv.Handler()}
	go httpSrv.Serve(listener)
	defer httpSrv.Shutdown(context.Background())

	client := newUnixSocketClient(t, socketPath)
	resp, err := client.Health(context.Background(), connect.NewRequest(&v1alpha1.HealthRequest{}))
	if err != nil {
		t.Fatalf("Health RPC: %v", err)
	}
	if resp.Msg.Status != "ready" {
		t.Errorf("expected status %q, got %q", "ready", resp.Msg.Status)
	}
	if resp.Msg.StartedAt == nil {
		t.Error("expected non-nil started_at")
	}
}

// TestGetServerInfoRPC verifies GetServerInfo returns ephemeral=true and
// the correct api_version when running in ephemeral mode.
func TestGetServerInfoRPC(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "wakild.sock")

	host := sessionhost.New(nil, sessionhost.WithAgentName("test"))
	srv := connsvc.NewServer(host, true) // ephemeral=true

	listener, err := listenUnix(socketPath)
	if err != nil {
		t.Fatalf("listenUnix: %v", err)
	}
	defer listener.Close()

	httpSrv := &http.Server{Handler: srv.Handler()}
	go httpSrv.Serve(listener)
	defer httpSrv.Shutdown(context.Background())

	client := newUnixSocketClient(t, socketPath)
	resp, err := client.GetServerInfo(context.Background(), connect.NewRequest(&v1alpha1.GetServerInfoRequest{}))
	if err != nil {
		t.Fatalf("GetServerInfo RPC: %v", err)
	}
	if !resp.Msg.Ephemeral {
		t.Error("expected ephemeral=true")
	}
	if resp.Msg.ApiVersion != "v1alpha1" {
		t.Errorf("expected api_version %q, got %q", "v1alpha1", resp.Msg.ApiVersion)
	}
	if resp.Msg.AuthMethod != "embedded" {
		t.Errorf("expected auth_method %q, got %q", "embedded", resp.Msg.AuthMethod)
	}
}

// TestGetServerInfoNonEphemeral verifies ephemeral=false in non-ephemeral mode.
func TestGetServerInfoNonEphemeral(t *testing.T) {
	dir := t.TempDir()
	socketPath := filepath.Join(dir, "wakild.sock")

	host := sessionhost.New(nil, sessionhost.WithAgentName("test"))
	srv := connsvc.NewServer(host, false) // ephemeral=false

	listener, err := listenUnix(socketPath)
	if err != nil {
		t.Fatalf("listenUnix: %v", err)
	}
	defer listener.Close()

	httpSrv := &http.Server{Handler: srv.Handler()}
	go httpSrv.Serve(listener)
	defer httpSrv.Shutdown(context.Background())

	client := newUnixSocketClient(t, socketPath)
	resp, err := client.GetServerInfo(context.Background(), connect.NewRequest(&v1alpha1.GetServerInfoRequest{}))
	if err != nil {
		t.Fatalf("GetServerInfo RPC: %v", err)
	}
	if resp.Msg.Ephemeral {
		t.Error("expected ephemeral=false")
	}
}

// newUnixSocketClient creates a Connect SystemService client that dials a
// Unix socket. The synthetic base URL "http://unix/" is ignored by the
// custom transport (the dialer uses the socket path directly).
func newUnixSocketClient(t *testing.T, socketPath string) wakilv1alpha1connect.SystemServiceClient {
	t.Helper()
	transport := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
		},
	}
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   10 * time.Second,
	}
	return wakilv1alpha1connect.NewSystemServiceClient(httpClient, "http://unix/")
}
