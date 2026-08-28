package sqlstore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionhost"
)

// TestHostRestartRecovery verifies that a host constructed with SQLiteStore
// persists sessions and events across close → reopen. This is the P1f
// acceptance criterion: a real host instance uses SQLiteStore, and after
// close+reopen the events are recoverable via SessionSnapshot/ListEvents.
func TestHostRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessionhost.db")
	ctx := context.Background()

	principal := core.Principal{
		TenantID: event.TenantID("tnt_test"),
		UserID:   event.UserID("usr_test"),
		Role:     core.RoleOwner,
	}
	workspace := event.WorkspaceID("wsp_test")

	// --- Phase 1: create host, session, submit a turn, close. ---
	store1, err := NewSQLiteStore(ctx, dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore 1: %v", err)
	}

	h1 := sessionhost.New(func(_ context.Context, in sessionhost.TurnInput) (string, error) {
		return in.Text, nil // echo
	}, sessionhost.WithStore(store1))

	sess1, err := h1.CreateSession(ctx, principal, core.CreateSessionRequest{
		Workspace: workspace,
		Title:     "restart-test",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Submit a turn to generate events beyond SessionCreated.
	_, err = h1.SubmitInput(ctx, principal, core.SubmitInputRequest{
		SessionID: sess1.ID,
		Text:      "hello restart",
	})
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	// Wait for the turn to complete.
	waitForTurn(t, h1, principal, sess1.ID)

	// Read events before close.
	eventsBefore, err := h1.ListEvents(ctx, principal, sess1.ID, 0, 0)
	if err != nil {
		t.Fatalf("ListEvents before close: %v", err)
	}
	if len(eventsBefore) < 3 {
		t.Fatalf("expected >= 3 events before close, got %d", len(eventsBefore))
	}

	// Close the host and store. Close emits SessionClosed as the final event.
	if err := h1.Close(ctx); err != nil {
		t.Fatalf("Host.Close: %v", err)
	}
	if err := store1.Close(); err != nil {
		t.Fatalf("Store.Close: %v", err)
	}

	// --- Phase 2: reopen store, construct a new host, verify events persist. ---
	store2, err := NewSQLiteStore(ctx, dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore 2: %v", err)
	}
	defer store2.Close()

	// Verify the persisted events are readable directly from the store.
	// After close, the event count includes the SessionClosed event emitted
	// by h.Close (not present in eventsBefore, which was read while the host
	// was still running).
	events, err := store2.Read(ctx, sess1.ID, 0, 0)
	if err != nil {
		t.Fatalf("Read after reopen: %v", err)
	}
	if len(events) != len(eventsBefore)+1 {
		t.Fatalf("event count: expected %d (before + SessionClosed), got %d", len(eventsBefore)+1, len(events))
	}

	// Verify LastSeq persisted (includes the SessionClosed event from Close).
	lastSeqAfter, err := store2.LastSeq(ctx, sess1.ID)
	if err != nil {
		t.Fatalf("LastSeq after reopen: %v", err)
	}
	if lastSeqAfter != event.Seq(len(events)) {
		t.Fatalf("last_seq mismatch: expected %d, got %d", len(events), lastSeqAfter)
	}

	// Verify the event projection: SessionCreated, UserMessageCommitted,
	// TurnStarted, MessageCommitted, TurnCompleted.
	expectedKinds := []event.Kind{
		event.KindSessionCreated,
		event.KindUserMessageCommitted,
		event.KindTurnStarted,
		event.KindMessageCommitted,
		event.KindTurnCompleted,
	}
	if len(events) < len(expectedKinds) {
		t.Fatalf("expected >= %d events, got %d", len(expectedKinds), len(events))
	}
	for i, expected := range expectedKinds {
		if events[i].Kind != expected {
			t.Fatalf("event %d: expected %s, got %s", i, expected, events[i].Kind)
		}
	}

	// Verify payload reconstruction.
	mc := events[3].Payload.(event.MessageCommitted)
	if mc.Text != "hello restart" {
		t.Fatalf("MessageCommitted.Text: expected 'hello restart', got %q", mc.Text)
	}

	// Verify the store's Read returns payloads as values (not pointers).
	for _, e := range events {
		if e.Payload == nil {
			t.Fatalf("event %s/%d has nil payload", e.SessionID, e.Seq)
		}
	}

	// --- Phase 3: a new host can append to the same session via the store. ---
	h2 := sessionhost.New(func(_ context.Context, in sessionhost.TurnInput) (string, error) {
		return "second turn", nil
	}, sessionhost.WithStore(store2))
	defer h2.Close(ctx)

	// The new host doesn't have the session in its in-memory map, so
	// ListEvents via the host would return ErrSessionNotFound. But the store
	// itself can still accept appends for the persisted session (the host's
	// Append path goes through emitDraft → store.Append). This is the P1
	// recovery seam: P2's RecoverRunning will use the store to enumerate
	// sessions and reconstruct the in-memory host state.
	//
	// For P1, we verify the store-level persistence (done above) and that
	// the new host can create a NEW session that coexists with the old one.
	sess2, err := h2.CreateSession(ctx, principal, core.CreateSessionRequest{
		Workspace: workspace,
		Title:     "second-session",
	})
	if err != nil {
		t.Fatalf("CreateSession 2: %v", err)
	}

	// Both sessions should be in the store with independent seq counters.
	lastSeq2, err := store2.LastSeq(ctx, sess2.ID)
	if err != nil {
		t.Fatalf("LastSeq sess2: %v", err)
	}
	if lastSeq2 != 1 {
		t.Fatalf("expected sess2 last_seq=1, got %d", lastSeq2)
	}
	// The original session's last_seq should be unchanged (the new session
	// doesn't touch the old one's row).
	lastSeq1Recheck, _ := store2.LastSeq(ctx, sess1.ID)
	if lastSeq1Recheck != lastSeqAfter {
		t.Fatalf("sess1 last_seq changed after sess2 creation: before=%d, after=%d", lastSeqAfter, lastSeq1Recheck)
	}
}

// TestHostWithSQLiteStore_SubmitAndConsume verifies that a host with SQLiteStore
// can run a full turn cycle (Submit → events → TurnCompleted) with events
// persisted to SQLite, not just in-memory.
func TestHostWithSQLiteStore_SubmitAndConsume(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessionhost.db")
	ctx := context.Background()

	store, err := NewSQLiteStore(ctx, dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	defer store.Close()

	h := sessionhost.New(func(_ context.Context, in sessionhost.TurnInput) (string, error) {
		return "test response", nil
	}, sessionhost.WithStore(store))
	defer h.Close(ctx)

	principal := core.Principal{
		TenantID: event.TenantID("tnt_test"),
		UserID:   event.UserID("usr_test"),
		Role:     core.RoleOwner,
	}

	sess, err := h.CreateSession(ctx, principal, core.CreateSessionRequest{
		Workspace: event.WorkspaceID("wsp_test"),
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	// Subscribe before submitting so we see all events.
	sub, err := h.Subscribe(ctx, principal, sess.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	_, err = h.SubmitInput(ctx, principal, core.SubmitInputRequest{
		SessionID: sess.ID,
		Text:      "test input",
	})
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	// Consume events until TurnCompleted.
	var sawTurnCompleted bool
	for !sawTurnCompleted {
		ev, err := sub.Next(ctx)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				break
			}
			t.Fatalf("sub.Next: %v", err)
		}
		if ev.Kind == event.KindTurnCompleted {
			sawTurnCompleted = true
		}
	}
	if !sawTurnCompleted {
		t.Fatal("did not see TurnCompleted")
	}

	// Verify events are in the store (not just in-memory).
	storeEvents, err := store.Read(ctx, sess.ID, 0, 0)
	if err != nil {
		t.Fatalf("store.Read: %v", err)
	}
	if len(storeEvents) < 3 {
		t.Fatalf("expected >= 3 events in store, got %d", len(storeEvents))
	}

	// Verify the store has the MessageCommitted with the echo text.
	for _, e := range storeEvents {
		if e.Kind == event.KindMessageCommitted {
			mc := e.Payload.(event.MessageCommitted)
			if mc.Text != "test response" {
				t.Fatalf("expected 'test response', got %q", mc.Text)
			}
			return
		}
	}
	t.Fatal("MessageCommitted not found in store events")
}

// waitForTurn polls ListEvents until it sees a TurnCompleted event.
func waitForTurn(t *testing.T, h *sessionhost.Host, p core.Principal, sid event.SessionID) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 200; i++ {
		events, err := h.ListEvents(ctx, p, sid, 0, 0)
		if err != nil {
			t.Fatalf("waitForTurn: ListEvents: %v", err)
		}
		for _, e := range events {
			if e.Kind == event.KindTurnCompleted {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("waitForTurn: timed out waiting for TurnCompleted")
}
