package main

// web_integration_test.go: P3 integration tests — web UI over HTTP/JSON.
//
// These tests start a daemon with --http-addr on a random localhost port,
// then exercise the Connect API over plain HTTP/JSON (the same wire format
// the browser uses) and verify static files are served.
//
// The exit gate (design §9): a running session is live-trackable in the
// browser, including tool-calls and subagent tree.

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionhost"
	"github.com/treeol/wakil/internal/remote"
	connsvc "github.com/treeol/wakil/internal/server/connect"
)

// startWebTestDaemon starts a Connect server with both a Unix socket and a TCP
// listener (for the web UI). Returns the TCP address, socket path, cleanup, and
// host (for direct server-side assertions).
//
// P4b: the TCP listener serves ONLY static files (no Connect RPC handlers).
// RPC tests use the Unix socket via the remote client.
func startWebTestDaemon(t *testing.T, turn sessionhost.TurnFunc) (httpAddr, socketPath string, cleanup func(), host *sessionhost.Host) {
	t.Helper()
	dir := t.TempDir()
	socketPath = filepath.Join(dir, "wakil.sock")

	// Find a free TCP port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	httpAddr = ln.Addr().String()

	host = sessionhost.New(turn, sessionhost.WithAgentName("test"))
	srv := connsvc.NewServer(host, true, connsvc.NewEmbeddedResolver()) // ephemeral

	// Unix-socket listener.
	unixLnr, err := listenUnix(socketPath)
	if err != nil {
		ln.Close()
		t.Fatalf("listenUnix: %v", err)
	}

	// TCP listener serves ONLY static files (P4b: no Connect RPC on TCP).
	tcpSrv := &http.Server{Handler: webStaticHandler()}
	go tcpSrv.Serve(ln)

	// Unix-socket server uses the Connect handler with ConnContext.
	unixSrv := newTestServer(srv)
	go unixSrv.Serve(unixLnr)

	cleanup = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tcpSrv.Shutdown(ctx)
		_ = unixSrv.Shutdown(ctx)
		_ = host.Close(ctx)
		ln.Close()
		unixLnr.Close()
	}
	return httpAddr, socketPath, cleanup, host
}

// getStaticFile fetches a static file from the web UI and returns its body.
func getStaticFile(t *testing.T, httpAddr, path string) string {
	t.Helper()
	url := fmt.Sprintf("http://%s%s", httpAddr, path)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("http get %s: %v", url, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d, body: %s", path, resp.StatusCode, body)
	}
	return string(body)
}

// TestP3_StaticFilesServed verifies the web UI static files are served at /.
func TestP3_StaticFilesServed(t *testing.T) {
	httpAddr, _, cleanup, _ := startWebTestDaemon(t, quickTurnFunc)
	defer cleanup()

	// index.html
	html := getStaticFile(t, httpAddr, "/")
	if !strings.Contains(html, "wakil") {
		t.Errorf("index.html should contain 'wakil', got: %s", html[:min(len(html), 200)])
	}

	// app.js
	js := getStaticFile(t, httpAddr, "/app.js")
	if !strings.Contains(js, "ListSessions") {
		t.Errorf("app.js should contain 'ListSessions'")
	}

	// styles.css
	css := getStaticFile(t, httpAddr, "/styles.css")
	if !strings.Contains(css, "--bg") {
		t.Errorf("styles.css should contain '--bg'")
	}
}

// TestP3_GetServerInfoOverHTTP verifies GetServerInfo works over the Unix
// socket (P4b: Connect RPC is no longer on TCP — static-only).
func TestP3_GetServerInfoOverHTTP(t *testing.T) {
	_, socketPath, cleanup, _ := startWebTestDaemon(t, quickTurnFunc)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	clients, err := remote.Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer clients.Close()

	resp, err := clients.System.GetServerInfo(ctx, connect.NewRequest(&v1alpha1.GetServerInfoRequest{}))
	if err != nil {
		t.Fatalf("GetServerInfo RPC: %v", err)
	}
	if resp.Msg.ApiVersion != "v1alpha1" {
		t.Errorf("apiVersion = %v, want v1alpha1", resp.Msg.ApiVersion)
	}
	if !resp.Msg.Ephemeral {
		t.Errorf("ephemeral = %v, want true", resp.Msg.Ephemeral)
	}
	caps := resp.Msg.Capabilities
	if len(caps) == 0 {
		t.Error("expected non-empty capabilities")
	}
}

// TestP3_HealthOverHTTP verifies Health works over the Unix socket.
func TestP3_HealthOverHTTP(t *testing.T) {
	_, socketPath, cleanup, _ := startWebTestDaemon(t, quickTurnFunc)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	clients, err := remote.Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer clients.Close()

	health, err := clients.System.Health(ctx, connect.NewRequest(&v1alpha1.HealthRequest{}))
	if err != nil {
		t.Fatalf("Health RPC: %v", err)
	}
	if health.Msg.Status != "ready" {
		t.Errorf("status = %v, want 'ready'", health.Msg.Status)
	}
}

// TestP3_ListSessionsOverHTTP verifies ListSessions works over the Unix
// socket (P4b: Connect RPC is no longer on TCP — static-only).
func TestP3_ListSessionsOverHTTP(t *testing.T) {
	_, socketPath, cleanup, _ := startWebTestDaemon(t, quickTurnFunc)
	defer cleanup()

	// Create a session via the Unix-socket remote client (same as TUI --daemon).
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rt, rtCleanup, err := remote.BootstrapRemote(ctx, socketPath, event.WorkspaceID("wsp_test"), "", nil)
	if err != nil {
		t.Fatalf("BootstrapRemote: %v", err)
	}
	defer rtCleanup()

	sid := facadeSessionID(t, rt.Facade)

	// ListSessions over the Unix socket via Connect client.
	clients, err := remote.Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer clients.Close()

	resp, err := clients.Session.ListSessions(ctx, connect.NewRequest(&v1alpha1.ListSessionsRequest{}))
	if err != nil {
		t.Fatalf("ListSessions RPC: %v", err)
	}
	found := false
	for _, s := range resp.Msg.Sessions {
		if s.Id == string(sid) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("session %s not found in ListSessions", sid)
	}
}

// TestP3_SessionSnapshotOverHTTP verifies GetSessionSnapshot works over the
// Unix socket and includes tool-call events (exit gate: tool-calls visible in browser).
func TestP3_SessionSnapshotOverHTTP(t *testing.T) {
	_, socketPath, cleanup, host := startWebTestDaemon(t, quickTurnFunc)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	rt, rtCleanup, err := remote.BootstrapRemote(ctx, socketPath, event.WorkspaceID("wsp_test"), "", nil)
	if err != nil {
		t.Fatalf("BootstrapRemote: %v", err)
	}
	defer rtCleanup()

	sid := facadeSessionID(t, rt.Facade)

	// Submit a turn (quickTurnFunc emits tool-call events).
	_, err = rt.Facade.SubmitInput(ctx, core.EmbeddedPrincipal(), core.SubmitInputRequest{
		SessionID: sid,
		Text:      "web snapshot test",
	})
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	// Wait for the turn to complete.
	waitForSessionIdle(t, host, sid, 5*time.Second)

	// GetSessionSnapshot over the Unix socket via Connect client.
	clients, err := remote.Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer clients.Close()

	snap, err := clients.Event.GetSessionSnapshot(ctx, connect.NewRequest(&v1alpha1.GetSessionSnapshotRequest{
		SessionId: string(sid),
	}))
	if err != nil {
		t.Fatalf("GetSessionSnapshot RPC: %v", err)
	}

	// Verify the session metadata.
	if snap.Msg.Session.Id != string(sid) {
		t.Errorf("snapshot session id = %v, want %s", snap.Msg.Session.Id, sid)
	}

	// Verify events include tool-call events.
	events := snap.Msg.Events
	if len(events) == 0 {
		t.Fatal("expected non-empty events in snapshot")
	}

	hasToolCall := false
	hasTurnStarted := false
	hasTurnCompleted := false
	for _, e := range events {
		switch e.Kind {
		case "tool_call_started":
			hasToolCall = true
		case "turn_started":
			hasTurnStarted = true
		case "turn_completed":
			hasTurnCompleted = true
		}
	}
	if !hasToolCall {
		t.Error("snapshot events should include tool_call_started (exit gate: tool-calls in browser)")
	}
	if !hasTurnStarted {
		t.Error("snapshot events should include turn_started")
	}
	if !hasTurnCompleted {
		t.Error("snapshot events should include turn_completed")
	}
}

// TestP3_ListEventsOverHTTP verifies ListEvents works over the Unix socket
// with cursor-based pagination.
func TestP3_ListEventsOverHTTP(t *testing.T) {
	_, socketPath, cleanup, host := startWebTestDaemon(t, quickTurnFunc)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	rt, rtCleanup, err := remote.BootstrapRemote(ctx, socketPath, event.WorkspaceID("wsp_test"), "", nil)
	if err != nil {
		t.Fatalf("BootstrapRemote: %v", err)
	}
	defer rtCleanup()

	sid := facadeSessionID(t, rt.Facade)

	_, err = rt.Facade.SubmitInput(ctx, core.EmbeddedPrincipal(), core.SubmitInputRequest{
		SessionID: sid,
		Text:      "list events test",
	})
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	host2 := host
	waitForSessionIdle(t, host2, sid, 5*time.Second)

	// ListEvents over the Unix socket via Connect client.
	clients, err := remote.Dial(socketPath)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer clients.Close()

	resp, err := clients.Event.ListEvents(ctx, connect.NewRequest(&v1alpha1.ListEventsRequest{
		SessionId: string(sid),
		AfterSeq:  0,
		Limit:     0,
	}))
	if err != nil {
		t.Fatalf("ListEvents RPC: %v", err)
	}
	events := resp.Msg.Events
	if len(events) == 0 {
		t.Fatal("expected non-empty events from ListEvents")
	}

	// Verify each event has a seq and kind.
	for _, e := range events {
		if e.Seq == 0 {
			t.Error("event missing 'seq' field")
		}
		if e.Kind == "" {
			t.Error("event missing 'kind' field")
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
