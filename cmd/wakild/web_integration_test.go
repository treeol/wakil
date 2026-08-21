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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionhost"
	"github.com/treeol/wakil/internal/remote"
	connsvc "github.com/treeol/wakil/internal/server/connect"
)

// startWebTestDaemon starts a Connect server with both a Unix socket and a TCP
// listener (for the web UI). Returns the TCP address, socket path, cleanup, and
// host (for direct server-side assertions).
func startWebTestDaemon(t *testing.T, turn sessionhost.TurnFunc) (httpAddr, socketPath string, cleanup func(), host *sessionhost.Host) {
	t.Helper()
	dir := t.TempDir()
	socketPath = filepath.Join(dir, "wakild.sock")

	// Find a free TCP port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	httpAddr = ln.Addr().String()

	host = sessionhost.New(turn, sessionhost.WithAgentName("test"))
	srv := connsvc.NewServer(host, true) // ephemeral

	// Unix-socket listener.
	unixLnr, err := listenUnix(socketPath)
	if err != nil {
		ln.Close()
		t.Fatalf("listenUnix: %v", err)
	}

	// TCP listener uses the web handler (static files + Connect).
	tcpSrv := &http.Server{Handler: webHandler(srv)}
	go tcpSrv.Serve(ln)

	// Unix-socket server uses the plain Connect handler.
	unixSrv := &http.Server{Handler: srv.Handler()}
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

// jsonRPC makes a Connect HTTP/JSON RPC call and decodes the response.
func jsonRPC(t *testing.T, httpAddr, service, method string, reqBody interface{}) map[string]interface{} {
	t.Helper()
	url := fmt.Sprintf("http://%s/wakil.v1alpha1.%s/%s", httpAddr, service, method)
	body, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp, err := http.Post(url, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("http post %s: %v", url, err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("RPC %s/%s: status %d, body: %s", service, method, resp.StatusCode, respBody)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		t.Fatalf("unmarshal response: %v (body: %s)", err, respBody)
	}
	return result
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
	if !strings.Contains(html, "wakild") {
		t.Errorf("index.html should contain 'wakild', got: %s", html[:min(len(html), 200)])
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

// TestP3_GetServerInfoOverHTTP verifies GetServerInfo works over HTTP/JSON.
func TestP3_GetServerInfoOverHTTP(t *testing.T) {
	httpAddr, _, cleanup, _ := startWebTestDaemon(t, quickTurnFunc)
	defer cleanup()

	info := jsonRPC(t, httpAddr, "SystemService", "GetServerInfo", map[string]interface{}{})
	if info["apiVersion"] != "v1alpha1" {
		t.Errorf("apiVersion = %v, want v1alpha1", info["apiVersion"])
	}
	if info["ephemeral"] != true {
		t.Errorf("ephemeral = %v, want true", info["ephemeral"])
	}
	caps, ok := info["capabilities"].([]interface{})
	if !ok {
		t.Fatalf("capabilities should be an array, got %T", info["capabilities"])
	}
	if len(caps) == 0 {
		t.Error("expected non-empty capabilities")
	}
}

// TestP3_HealthOverHTTP verifies Health works over HTTP/JSON.
func TestP3_HealthOverHTTP(t *testing.T) {
	httpAddr, _, cleanup, _ := startWebTestDaemon(t, quickTurnFunc)
	defer cleanup()

	health := jsonRPC(t, httpAddr, "SystemService", "Health", map[string]interface{}{})
	if health["status"] != "ready" {
		t.Errorf("status = %v, want 'ready'", health["status"])
	}
}

// TestP3_ListSessionsOverHTTP verifies ListSessions works over HTTP/JSON
// after a session is created (via the Unix-socket remote client).
func TestP3_ListSessionsOverHTTP(t *testing.T) {
	httpAddr, socketPath, cleanup, _ := startWebTestDaemon(t, quickTurnFunc)
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

	// ListSessions over HTTP/JSON.
	resp := jsonRPC(t, httpAddr, "SessionService", "ListSessions", map[string]interface{}{})
	sessions, ok := resp["sessions"].([]interface{})
	if !ok {
		t.Fatalf("sessions should be an array, got %T", resp["sessions"])
	}
	found := false
	for _, s := range sessions {
		m, _ := s.(map[string]interface{})
		if m["id"] == string(sid) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("session %s not found in ListSessions over HTTP", sid)
	}
}

// TestP3_SessionSnapshotOverHTTP verifies GetSessionSnapshot works over HTTP/JSON
// and includes tool-call events (exit gate: tool-calls visible in browser).
func TestP3_SessionSnapshotOverHTTP(t *testing.T) {
	httpAddr, socketPath, cleanup, host := startWebTestDaemon(t, quickTurnFunc)
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

	// GetSessionSnapshot over HTTP/JSON.
	snap := jsonRPC(t, httpAddr, "EventService", "GetSessionSnapshot", map[string]interface{}{
		"sessionId": string(sid),
	})

	// Verify the session metadata.
	snapSession, ok := snap["session"].(map[string]interface{})
	if !ok {
		t.Fatalf("snapshot.session should be an object, got %T", snap["session"])
	}
	if snapSession["id"] != string(sid) {
		t.Errorf("snapshot session id = %v, want %s", snapSession["id"], sid)
	}

	// Verify events include tool-call events.
	events, ok := snap["events"].([]interface{})
	if !ok {
		t.Fatalf("snapshot.events should be an array, got %T", snap["events"])
	}
	if len(events) == 0 {
		t.Fatal("expected non-empty events in snapshot")
	}

	hasToolCall := false
	hasTurnStarted := false
	hasTurnCompleted := false
	for _, e := range events {
		ev, _ := e.(map[string]interface{})
		kind, _ := ev["kind"].(string)
		switch kind {
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

// TestP3_ListEventsOverHTTP verifies ListEvents works over HTTP/JSON with
// cursor-based pagination.
func TestP3_ListEventsOverHTTP(t *testing.T) {
	httpAddr, socketPath, cleanup, host := startWebTestDaemon(t, quickTurnFunc)
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

	// ListEvents with afterSeq=0 (all events).
	resp := jsonRPC(t, httpAddr, "EventService", "ListEvents", map[string]interface{}{
		"sessionId": string(sid),
		"afterSeq":  0,
		"limit":     0,
	})
	events, ok := resp["events"].([]interface{})
	if !ok {
		t.Fatalf("events should be an array, got %T", resp["events"])
	}
	if len(events) == 0 {
		t.Fatal("expected non-empty events from ListEvents")
	}

	// Verify each event has a seq and kind.
	for _, e := range events {
		ev, _ := e.(map[string]interface{})
		if _, ok := ev["seq"]; !ok {
			t.Error("event missing 'seq' field")
		}
		if _, ok := ev["kind"]; !ok {
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
