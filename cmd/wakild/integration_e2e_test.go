package main

// integration_e2e_test.go: end-to-end daemon↔client integration tests (card #148 P2f).
//
// These tests spin up a real Connect server (sessionhost.Host + connect.Server)
// over a Unix socket, then use the internal/remote client stack to exercise the
// full wire path: CreateSession → SubmitInput → StreamEvents → ListEvents →
// GetSessionSnapshot → ListSessions → CloseSession → DeleteSession.
//
// Unlike integration_test.go (which tests Health/GetServerInfo only), these
// tests drive the actual remote client code path — the same code the TUI uses
// in --daemon mode — against a real (ephemeral) host with a stub TurnFunc.

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionclient"
	"github.com/treeol/wakil/internal/core/sessionhost"
	"github.com/treeol/wakil/internal/remote"
	connsvc "github.com/treeol/wakil/internal/server/connect"
)

// stubTurnFunc is a TurnFunc that emits a ToolCallStarted/Completed pair and
// returns a canned message. It exercises the turn-emitted event path (non-
// host-reserved durable kinds) so StreamEvents and ListEvents can verify the
// full wire path. It blocks on ctx until the turn finishes, testing interrupt.
func stubTurnFunc(ctx context.Context, input sessionhost.TurnInput) (string, error) {
	if input.Emit != nil {
		_ = input.Emit.Emit(event.KindToolCallStarted, event.ToolCallStarted{
			TurnID:     input.TurnID,
			ToolCallID: "tcl_test",
			Name:       "run_shell",
			ArgDigest:  "stub",
		})
		_ = input.Emit.Emit(event.KindToolCallCompleted, event.ToolCallCompleted{
			ToolCallID: "tcl_test",
			Name:       "run_shell",
			Status:     "ok",
		})
	}
	<-ctx.Done()
	return fmt.Sprintf("stub reply to %q", input.Text), nil
}

// quickTurnFunc is a TurnFunc that returns immediately without waiting for
// ctx cancellation. Used by tests that don't test interrupt behavior.
func quickTurnFunc(ctx context.Context, input sessionhost.TurnInput) (string, error) {
	if input.Emit != nil {
		_ = input.Emit.Emit(event.KindToolCallStarted, event.ToolCallStarted{
			TurnID:     input.TurnID,
			ToolCallID: "tcl_quick",
			Name:       "read_file",
			ArgDigest:  "quick",
		})
		_ = input.Emit.Emit(event.KindToolCallCompleted, event.ToolCallCompleted{
			ToolCallID: "tcl_quick",
			Name:       "read_file",
			Status:     "ok",
		})
	}
	return fmt.Sprintf("quick reply to %q", input.Text), nil
}

// startTestDaemon spins up a Connect server (sessionhost.Host + connect.Server)
// over a Unix socket in a temp directory. It returns the socket path, a
// cleanup function, and the host (for direct server-side assertions).
//
// The caller defers cleanup().
func startTestDaemon(t *testing.T, turn sessionhost.TurnFunc) (socketPath string, cleanup func(), host *sessionhost.Host) {
	t.Helper()
	dir := t.TempDir()
	socketPath = filepath.Join(dir, "wakild.sock")

	host = sessionhost.New(turn, sessionhost.WithAgentName("test"))
	srv := connsvc.NewServer(host, true) // ephemeral=true

	listener, err := listenUnix(socketPath)
	if err != nil {
		t.Fatalf("listenUnix: %v", err)
	}

	httpSrv := &http.Server{Handler: srv.Handler()}
	go httpSrv.Serve(listener)

	cleanup = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
		_ = host.Close(ctx)
		listener.Close()
	}
	return socketPath, cleanup, host
}

// waitForSessionIdle polls the host until the session returns to idle (or the
// timeout expires). Used to let the stub turn complete before querying events.
func waitForSessionIdle(t *testing.T, host *sessionhost.Host, sid event.SessionID, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s, err := host.GetSession(context.Background(), core.EmbeddedPrincipal(), sid)
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if s.State == core.SessionIdle || s.State == core.SessionClosed {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session %s did not reach idle/closed within %v", sid, timeout)
}

// facadeSessionID extracts the session ID from a Facade's Snapshot().
func facadeSessionID(t *testing.T, f sessionclient.Facade) event.SessionID {
	t.Helper()
	snap := f.Snapshot()
	if snap.SessionID == "" {
		t.Fatal("expected non-empty session ID from Snapshot()")
	}
	return snap.SessionID
}

// TestE2E_CreateSession verifies the remote client can create a session on the
// daemon and the session appears in ListSessions.
func TestE2E_CreateSession(t *testing.T) {
	sock, cleanup, _ := startTestDaemon(t, quickTurnFunc)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rt, rtCleanup, err := remote.BootstrapRemote(ctx, sock, event.WorkspaceID("wsp_test"), "", nil)
	if err != nil {
		t.Fatalf("BootstrapRemote: %v", err)
	}
	defer rtCleanup()

	sid := facadeSessionID(t, rt.Facade)

	// ListSessions should include the new session.
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

// TestE2E_SubmitInputAndListEvents verifies the full turn cycle: SubmitInput →
// TurnStarted → ToolCallStarted → ToolCallCompleted → MessageCommitted →
// TurnCompleted events are all visible via ListEvents.
func TestE2E_SubmitInputAndListEvents(t *testing.T) {
	sock, cleanup, host := startTestDaemon(t, quickTurnFunc)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	rt, rtCleanup, err := remote.BootstrapRemote(ctx, sock, event.WorkspaceID("wsp_test"), "", nil)
	if err != nil {
		t.Fatalf("BootstrapRemote: %v", err)
	}
	defer rtCleanup()

	sid := facadeSessionID(t, rt.Facade)

	// Submit a turn.
	ack, err := rt.Facade.SubmitInput(ctx, core.EmbeddedPrincipal(), core.SubmitInputRequest{
		SessionID: sid,
		Text:      "hello daemon",
	})
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if ack.TurnID == "" {
		t.Error("expected non-empty turn ID")
	}

	// Wait for the turn to complete and events to be visible.
	waitForSessionIdle(t, host, sid, 5*time.Second)

	// List all events.
	events, err := rt.Facade.ListEvents(ctx, core.EmbeddedPrincipal(), sid, 0, 1000)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}

	// Verify the expected event sequence.
	kinds := make([]event.Kind, 0, len(events))
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}

	// Expected: SessionCreated, UserMessageCommitted, TurnStarted,
	// ToolCallStarted, ToolCallCompleted, MessageCommitted, TurnCompleted
	expectedKinds := []event.Kind{
		event.KindSessionCreated,
		event.KindUserMessageCommitted,
		event.KindTurnStarted,
		event.KindToolCallStarted,
		event.KindToolCallCompleted,
		event.KindMessageCommitted,
		event.KindTurnCompleted,
	}
	if len(kinds) < len(expectedKinds) {
		t.Fatalf("expected at least %d events, got %d: %v", len(expectedKinds), len(kinds), kinds)
	}
	for i, want := range expectedKinds {
		if kinds[i] != want {
			t.Errorf("event[%d] = %q, want %q", i, kinds[i], want)
		}
	}
}

// TestE2E_SessionSnapshot verifies GetSessionSnapshot returns the correct
// session state and event history after a turn.
func TestE2E_SessionSnapshot(t *testing.T) {
	sock, cleanup, host := startTestDaemon(t, quickTurnFunc)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rt, rtCleanup, err := remote.BootstrapRemote(ctx, sock, event.WorkspaceID("wsp_test"), "", nil)
	if err != nil {
		t.Fatalf("BootstrapRemote: %v", err)
	}
	defer rtCleanup()

	sid := facadeSessionID(t, rt.Facade)

	// Submit a turn.
	_, err = rt.Facade.SubmitInput(ctx, core.EmbeddedPrincipal(), core.SubmitInputRequest{
		SessionID: sid,
		Text:      "snapshot test",
	})
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	waitForSessionIdle(t, host, sid, 5*time.Second)

	// Get the snapshot.
	snap, err := rt.Facade.SessionSnapshot(ctx, core.EmbeddedPrincipal(), sid)
	if err != nil {
		t.Fatalf("SessionSnapshot: %v", err)
	}
	if snap.Session.ID != sid {
		t.Errorf("snapshot session ID = %q, want %q", snap.Session.ID, sid)
	}
	if snap.LastSeq == 0 {
		t.Error("expected non-zero LastSeq after a turn")
	}
	if len(snap.Events) == 0 {
		t.Error("expected non-empty events in snapshot")
	}
}

// TestE2E_CloseSession verifies the remote client can close a session and the
// session transitions to closed state.
func TestE2E_CloseSession(t *testing.T) {
	sock, cleanup, _ := startTestDaemon(t, stubTurnFunc)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rt, rtCleanup, err := remote.BootstrapRemote(ctx, sock, event.WorkspaceID("wsp_test"), "", nil)
	if err != nil {
		t.Fatalf("BootstrapRemote: %v", err)
	}
	defer rtCleanup()

	sid := facadeSessionID(t, rt.Facade)

	// Close the session.
	if err := rt.Facade.CloseSession(ctx, core.EmbeddedPrincipal(), sid); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}

	// Verify the session is closed by listing events and finding SessionClosed.
	events, err := rt.Facade.ListEvents(ctx, core.EmbeddedPrincipal(), sid, 0, 1000)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	found := false
	for _, e := range events {
		if e.Kind == event.KindSessionClosed {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected SessionClosed event after CloseSession")
	}
}

// TestE2E_Interrupt verifies the remote client can interrupt a running turn and
// the turn completes with "cancelled" outcome.
func TestE2E_Interrupt(t *testing.T) {
	sock, cleanup, host := startTestDaemon(t, stubTurnFunc)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rt, rtCleanup, err := remote.BootstrapRemote(ctx, sock, event.WorkspaceID("wsp_test"), "", nil)
	if err != nil {
		t.Fatalf("BootstrapRemote: %v", err)
	}
	defer rtCleanup()

	sid := facadeSessionID(t, rt.Facade)

	// Submit a turn (stubTurnFunc blocks on ctx until interrupted).
	_, err = rt.Facade.SubmitInput(ctx, core.EmbeddedPrincipal(), core.SubmitInputRequest{
		SessionID: sid,
		Text:      "long running",
	})
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	// Interrupt it.
	if err := rt.Facade.Interrupt(ctx, core.EmbeddedPrincipal(), sid); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	// Wait for the session to return to idle.
	waitForSessionIdle(t, host, sid, 5*time.Second)

	// Verify TurnCompleted with outcome "cancelled".
	events, err := rt.Facade.ListEvents(ctx, core.EmbeddedPrincipal(), sid, 0, 1000)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	foundCancelled := false
	for _, e := range events {
		if e.Kind == event.KindTurnCompleted {
			tc, ok := e.Payload.(event.TurnCompleted)
			if !ok {
				t.Fatalf("TurnCompleted payload type = %T", e.Payload)
			}
			if tc.Outcome == "cancelled" {
				foundCancelled = true
			}
		}
	}
	if !foundCancelled {
		t.Error("expected TurnCompleted with outcome=cancelled")
	}
}

// TestE2E_ResumeSession verifies the remote client can resume an existing
// session by ID and see its event history.
func TestE2E_ResumeSession(t *testing.T) {
	sock, cleanup, host := startTestDaemon(t, quickTurnFunc)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Create the first session.
	rt1, rt1Cleanup, err := remote.BootstrapRemote(ctx, sock, event.WorkspaceID("wsp_test"), "", nil)
	if err != nil {
		t.Fatalf("BootstrapRemote (1st): %v", err)
	}

	sid := facadeSessionID(t, rt1.Facade)

	// Submit a turn so the session has events.
	_, err = rt1.Facade.SubmitInput(ctx, core.EmbeddedPrincipal(), core.SubmitInputRequest{
		SessionID: sid,
		Text:      "first turn",
	})
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	waitForSessionIdle(t, host, sid, 5*time.Second)

	// Close the first facade (don't close the session itself — rt1Cleanup
	// calls mgr.Close(f) which closes the session; so we must not call it
	// before resuming. Instead, we skip rt1Cleanup and manually close only
	// the client resources).
	// Actually, rt1Cleanup closes the facade which closes the session on the
	// daemon. For resume, we need the session to still be alive. So we
	// skip rt1Cleanup for now and clean up after the resume test.
	// We can't skip it though — the clients will leak. But the daemon cleanup
	// will shut everything down. Let's just skip rt1Cleanup.

	// Resume by full session ID.
	rt2, rt2Cleanup, err := remote.BootstrapRemote(ctx, sock, event.WorkspaceID("wsp_test"), string(sid), nil)
	if err != nil {
		// The first facade's cleanup closes the session on the daemon. We
		// need to avoid that. Let's call rt1Cleanup AFTER the resume.
		rt1Cleanup()
		t.Fatalf("BootstrapRemote (resume): %v", err)
	}
	defer rt2Cleanup()

	// Now clean up the first runtime (it won't close the session since
	// rt2's facade has it now — but actually CloseSession is idempotent).
	rt1Cleanup()

	sid2 := facadeSessionID(t, rt2.Facade)
	if sid2 != sid {
		t.Errorf("resumed session ID = %q, want %q", sid2, sid)
	}

	// The resumed session should see the events from the first turn.
	events, err := rt2.Facade.ListEvents(ctx, core.EmbeddedPrincipal(), sid, 0, 1000)
	if err != nil {
		t.Fatalf("ListEvents (resumed): %v", err)
	}
	if len(events) == 0 {
		t.Error("expected non-empty event history after resume")
	}

	// Verify SessionCreated is in the history.
	foundCreated := false
	for _, e := range events {
		if e.Kind == event.KindSessionCreated {
			foundCreated = true
			break
		}
	}
	if !foundCreated {
		t.Error("expected SessionCreated in resumed history")
	}
}

// TestE2E_DeleteSession verifies the remote client can delete a session and it
// disappears from ListSessions.
func TestE2E_DeleteSession(t *testing.T) {
	sock, cleanup, _ := startTestDaemon(t, quickTurnFunc)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rt, rtCleanup, err := remote.BootstrapRemote(ctx, sock, event.WorkspaceID("wsp_test"), "", nil)
	if err != nil {
		t.Fatalf("BootstrapRemote: %v", err)
	}
	defer rtCleanup()

	sid := facadeSessionID(t, rt.Facade)

	// Delete the session via a direct Connect client call (the Facade
	// interface doesn't expose DeleteSession, but the Connect client does).
	clients, err := remote.Dial(sock)
	if err != nil {
		t.Fatalf("DialRemote: %v", err)
	}
	defer clients.Close()

	_, err = clients.Session.DeleteSession(ctx, connect.NewRequest(&v1alpha1.DeleteSessionRequest{
		SessionId: string(sid),
	}))
	if err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	// ListSessions should no longer include it.
	sessions, _, err := rt.Facade.ListSessions(sessionclient.SessionScope{All: true})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	for _, s := range sessions {
		if s.ChatID == string(sid) {
			t.Error("deleted session still appears in ListSessions")
		}
	}
}

// TestE2E_StreamEvents verifies the remote event pump delivers events in real
// time via the StreamEvents server-stream RPC.
func TestE2E_StreamEvents(t *testing.T) {
	sock, cleanup, host := startTestDaemon(t, quickTurnFunc)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	rt, rtCleanup, err := remote.BootstrapRemote(ctx, sock, event.WorkspaceID("wsp_test"), "", nil)
	if err != nil {
		t.Fatalf("BootstrapRemote: %v", err)
	}
	defer rtCleanup()

	sid := facadeSessionID(t, rt.Facade)

	// Set up event collection via the pump.
	var mu sync.Mutex
	var collected []event.Event
	deliver := func(ev event.Event) {
		mu.Lock()
		collected = append(collected, ev)
		mu.Unlock()
	}

	// Subscribe starts the pump.
	_, err = rt.Facade.Subscribe(ctx, core.EmbeddedPrincipal(), sid, 0, deliver)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	rt.StartEventPump(ctx)

	// Submit a turn to generate events.
	_, err = rt.Facade.SubmitInput(ctx, core.EmbeddedPrincipal(), core.SubmitInputRequest{
		SessionID: sid,
		Text:      "stream test",
	})
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	// Wait for the turn to complete.
	waitForSessionIdle(t, host, sid, 5*time.Second)

	// Give the pump a moment to deliver events.
	time.Sleep(500 * time.Millisecond)

	// Verify we collected some events.
	mu.Lock()
	count := len(collected)
	mu.Unlock()

	if count == 0 {
		t.Fatal("expected pump to deliver events, got 0")
	}

	// Verify at least TurnStarted and TurnCompleted were delivered.
	mu.Lock()
	kinds := make([]event.Kind, 0, count)
	for _, e := range collected {
		kinds = append(kinds, e.Kind)
	}
	mu.Unlock()

	hasStarted := false
	hasCompleted := false
	for _, k := range kinds {
		if k == event.KindTurnStarted {
			hasStarted = true
		}
		if k == event.KindTurnCompleted {
			hasCompleted = true
		}
	}
	if !hasStarted {
		t.Error("pump did not deliver TurnStarted event")
	}
	if !hasCompleted {
		t.Error("pump did not deliver TurnCompleted event")
	}
}

// TestE2E_NewConversation verifies the remote ConversationManager can create a
// second session (rotation).
func TestE2E_NewConversation(t *testing.T) {
	sock, cleanup, _ := startTestDaemon(t, quickTurnFunc)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rt, rtCleanup, err := remote.BootstrapRemote(ctx, sock, event.WorkspaceID("wsp_test"), "", nil)
	if err != nil {
		t.Fatalf("BootstrapRemote: %v", err)
	}
	defer rtCleanup()

	sid1 := facadeSessionID(t, rt.Facade)

	// Create a second conversation.
	f2, err := rt.Manager.NewConversation(ctx, core.EmbeddedPrincipal(), rt.Facade)
	if err != nil {
		t.Fatalf("NewConversation: %v", err)
	}
	sid2 := facadeSessionID(t, f2)

	if sid1 == sid2 {
		t.Error("expected different session IDs after NewConversation")
	}

	// Both should appear in ListSessions.
	sessions, _, err := rt.Facade.ListSessions(sessionclient.SessionScope{All: true})
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	ids := make(map[string]bool)
	for _, s := range sessions {
		ids[s.ChatID] = true
	}
	if !ids[string(sid1)] {
		t.Error("first session missing from ListSessions")
	}
	if !ids[string(sid2)] {
		t.Error("second session missing from ListSessions")
	}
}
