package main

// p4b_test.go: tests for P4b — SO_PEERCRED, fail-closed TCP, and principal
// resolution over the Unix socket.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"
	"github.com/treeol/wakil/internal/remote"
)

// TestP4b_TCPDoesNotServeConnectRPC verifies that the TCP listener (web UI)
// does NOT serve Connect RPC handlers. A POST to a Connect RPC path should
// return 404 (not handled), not 200 with an RPC response.
//
// This is the critical security test: before P4b, the TCP listener served
// Connect RPC with EmbeddedPrincipal (owner), which was an authentication
// bypass. P4b removes Connect from TCP — only static files are served.
func TestP4b_TCPDoesNotServeConnectRPC(t *testing.T) {
	httpAddr, _, cleanup, _ := startWebTestDaemon(t, quickTurnFunc)
	defer cleanup()

	// Try to call ListSessions over TCP (the old insecure path).
	url := "http://" + httpAddr + "/wakil.v1alpha1.SessionService/ListSessions"
	body, _ := json.Marshal(map[string]interface{}{})
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("http post: %v", err)
	}
	defer resp.Body.Close()

	// Static-file server returns 404 for unknown paths — Connect handlers
	// are NOT mounted on TCP.
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("Connect RPC should NOT be available on TCP (got 200) — security: TCP has no auth")
	}
}

// TestP4b_StaticFilesStillServedOnTCP verifies that static files (the web UI)
// are still served on TCP after removing Connect handlers.
func TestP4b_StaticFilesStillServedOnTCP(t *testing.T) {
	httpAddr, _, cleanup, _ := startWebTestDaemon(t, quickTurnFunc)
	defer cleanup()

	html := getStaticFile(t, httpAddr, "/")
	if !strings.Contains(html, "wakild") {
		t.Errorf("index.html should contain 'wakild', got: %s", html[:min(len(html), 200)])
	}
}

// TestP4b_UnixSocketRPCWorks verifies that Connect RPCs work over the Unix
// socket (the authenticated path via SO_PEERCRED + embedded resolver in tests).
func TestP4b_UnixSocketRPCWorks(t *testing.T) {
	_, socketPath, cleanup, _ := startWebTestDaemon(t, quickTurnFunc)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rt, rtCleanup, err := remote.BootstrapRemote(ctx, socketPath, event.WorkspaceID("wsp_test"), "", nil)
	if err != nil {
		t.Fatalf("BootstrapRemote: %v", err)
	}
	defer rtCleanup()

	sid := facadeSessionID(t, rt.Facade)

	// Verify the session was created via the Unix socket.
	if sid == "" {
		t.Fatal("expected non-empty session ID")
	}

	// ListSessions should work and include the session.
	sessions, _, err := rt.Facade.ListSessions(sessionclient.SessionScope{All: true})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	found := false
	for _, s := range sessions {
		if s.ChatID == string(sid) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("session %s not found in ListSessions", sid)
	}
}
