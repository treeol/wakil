package sqlstore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
)

// newTestStore returns a SQLiteStore backed by a temp file and a cleanup func.
func newTestStore(t *testing.T) (*SQLiteStore, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	s, err := NewSQLiteStore(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	return s, func() {
		s.Close()
	}
}

// validDraft constructs a valid durable event draft for testing.
func validDraft(kind event.Kind, sid event.SessionID, tenant event.TenantID, payload any) event.Event {
	return event.Event{
		TenantID:  tenant,
		SessionID: sid,
		Ts:        time.Now().UTC(),
		Kind:      kind,
		Payload:   payload,
	}
}

// TestAppendSessionCreated verifies that the first SessionCreated append
// creates the session row and event atomically, returning seq=1.
func TestAppendSessionCreated(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()
	draft := validDraft(event.KindSessionCreated, "ses_test1", "tnt_test",
		event.SessionCreated{WorkspaceID: "wsp_test", AgentName: "wakil", CreatedBy: "usr_test"})

	committed, err := s.Append(ctx, draft)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if committed.Seq != 1 {
		t.Fatalf("expected seq=1, got %d", committed.Seq)
	}
	if committed.Kind != event.KindSessionCreated {
		t.Fatalf("expected kind SessionCreated, got %s", committed.Kind)
	}

	// LastSeq should return 1.
	last, err := s.LastSeq(ctx, "ses_test1")
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	if last != 1 {
		t.Fatalf("expected last_seq=1, got %d", last)
	}

	// Read should return the one event.
	events, err := s.Read(ctx, "ses_test1", 0, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Seq != 1 {
		t.Fatalf("expected event seq=1, got %d", events[0].Seq)
	}
}

// TestAppendEventAfterSessionCreated verifies that a non-SessionCreated append
// after session creation increments the seq correctly.
func TestAppendEventAfterSessionCreated(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()
	sid := event.SessionID("ses_test2")
	tenant := event.TenantID("tnt_test")

	// Create the session.
	_, err := s.Append(ctx, validDraft(event.KindSessionCreated, sid, tenant,
		event.SessionCreated{WorkspaceID: "wsp_test", AgentName: "wakil", CreatedBy: "usr_test"}))
	if err != nil {
		t.Fatalf("Append SessionCreated: %v", err)
	}

	// Append a TurnStarted.
	committed, err := s.Append(ctx, validDraft(event.KindTurnStarted, sid, tenant,
		event.TurnStarted{TurnID: "trn_test1", TurnIndex: 1}))
	if err != nil {
		t.Fatalf("Append TurnStarted: %v", err)
	}
	if committed.Seq != 2 {
		t.Fatalf("expected seq=2, got %d", committed.Seq)
	}

	// Append a MessageCommitted.
	committed, err = s.Append(ctx, validDraft(event.KindMessageCommitted, sid, tenant,
		event.MessageCommitted{TurnID: "trn_test1", Text: "hello"}))
	if err != nil {
		t.Fatalf("Append MessageCommitted: %v", err)
	}
	if committed.Seq != 3 {
		t.Fatalf("expected seq=3, got %d", committed.Seq)
	}

	// Read all events.
	events, err := s.Read(ctx, sid, 0, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events, got %d", len(events))
	}
	// Verify ascending order.
	for i, e := range events {
		if e.Seq != event.Seq(i+1) {
			t.Fatalf("event %d has seq %d, expected %d", i, e.Seq, i+1)
		}
	}
}

// TestAppendNonSessionCreatedToUnknownSession verifies that appending a
// non-SessionCreated event to a nonexistent session returns ErrSessionNotFound.
func TestAppendNonSessionCreatedToUnknownSession(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	_, err := s.Append(context.Background(),
		validDraft(event.KindTurnStarted, "ses_nonexistent", "tnt_test",
			event.TurnStarted{TurnID: "trn_test1", TurnIndex: 1}))
	if !errors.Is(err, core.ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound, got %v", err)
	}
}

// TestAppendTenantMismatch verifies that appending with the wrong tenant
// returns ErrSessionNotFound (no existence leak).
func TestAppendTenantMismatch(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()
	sid := event.SessionID("ses_test3")

	// Create session under tnt_test.
	_, err := s.Append(ctx, validDraft(event.KindSessionCreated, sid, "tnt_test",
		event.SessionCreated{WorkspaceID: "wsp_test", AgentName: "wakil", CreatedBy: "usr_test"}))
	if err != nil {
		t.Fatalf("Append SessionCreated: %v", err)
	}

	// Try to append under wrong tenant.
	_, err = s.Append(ctx, validDraft(event.KindTurnStarted, sid, "tnt_other",
		event.TurnStarted{TurnID: "trn_test1", TurnIndex: 1}))
	if !errors.Is(err, core.ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound for tenant mismatch, got %v", err)
	}
}

// TestAppendDuplicateSessionCreated verifies that a duplicate SessionCreated
// append returns an error (PK violation).
func TestAppendDuplicateSessionCreated(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()
	sid := event.SessionID("ses_dup")
	draft := validDraft(event.KindSessionCreated, sid, "tnt_test",
		event.SessionCreated{WorkspaceID: "wsp_test", AgentName: "wakil", CreatedBy: "usr_test"})

	// First append succeeds.
	if _, err := s.Append(ctx, draft); err != nil {
		t.Fatalf("first Append: %v", err)
	}
	// Second append should fail (PK violation).
	if _, err := s.Append(ctx, draft); err == nil {
		t.Fatal("duplicate SessionCreated should fail")
	}
}

// TestAppendRejectsEphemeral verifies that ephemeral drafts are rejected.
func TestAppendRejectsEphemeral(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()
	// Create session first.
	_, _ = s.Append(ctx, validDraft(event.KindSessionCreated, "ses_eph", "tnt_test",
		event.SessionCreated{WorkspaceID: "wsp_test", AgentName: "wakil", CreatedBy: "usr_test"}))

	// Try to append an ephemeral event.
	_, err := s.Append(ctx, validDraft(event.KindMessageDelta, "ses_eph", "tnt_test",
		event.MessageDelta{Text: "streaming"}))
	if err == nil {
		t.Fatal("ephemeral append should be rejected")
	}
}

// TestAppendRejectsInvalidDraft verifies that invalid drafts are rejected.
func TestAppendRejectsInvalidDraft(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	// Missing Ts (zero time) → ValidateDraft fails.
	draft := event.Event{
		TenantID:  "tnt_test",
		SessionID: "ses_invalid",
		Kind:      event.KindSessionCreated,
		Payload:   event.SessionCreated{WorkspaceID: "wsp_test", AgentName: "wakil", CreatedBy: "usr_test"},
		// Ts is zero
	}
	_, err := s.Append(context.Background(), draft)
	if err == nil {
		t.Fatal("invalid draft (zero Ts) should be rejected")
	}
}

// TestReadCursorSemantics verifies cursor-exclusive read behavior.
func TestReadCursorSemantics(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()
	sid := event.SessionID("ses_cursor")
	tenant := event.TenantID("tnt_test")

	// Create session + 5 events (total seq 1-6).
	_, _ = s.Append(ctx, validDraft(event.KindSessionCreated, sid, tenant,
		event.SessionCreated{WorkspaceID: "wsp_test", AgentName: "wakil", CreatedBy: "usr_test"}))
	for i := 1; i <= 5; i++ {
		_, _ = s.Append(ctx, validDraft(event.KindMessageCommitted, sid, tenant,
			event.MessageCommitted{TurnID: event.TurnID(fmt.Sprintf("trn_%d", i)), Text: fmt.Sprintf("msg %d", i)}))
	}

	// Read from 0 → all 6 events.
	events, err := s.Read(ctx, sid, 0, 0)
	if err != nil {
		t.Fatalf("Read from 0: %v", err)
	}
	if len(events) != 6 {
		t.Fatalf("expected 6 events from seq 0, got %d", len(events))
	}

	// Read from 3 → events 4,5,6.
	events, err = s.Read(ctx, sid, 3, 0)
	if err != nil {
		t.Fatalf("Read from 3: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 events from seq 3, got %d", len(events))
	}
	if events[0].Seq != 4 {
		t.Fatalf("expected first seq=4, got %d", events[0].Seq)
	}

	// Read with limit=2 from 0 → first 2 events.
	events, err = s.Read(ctx, sid, 0, 2)
	if err != nil {
		t.Fatalf("Read limit 2: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events with limit=2, got %d", len(events))
	}

	// Read from cursor beyond last seq → empty (not error).
	events, err = s.Read(ctx, sid, 100, 0)
	if err != nil {
		t.Fatalf("Read beyond last: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events beyond last seq, got %d", len(events))
	}
}

// TestReadNonexistentSession verifies that reading a nonexistent session
// returns empty (not an error).
func TestReadNonexistentSession(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	events, err := s.Read(context.Background(), "ses_nonexistent", 0, 0)
	if err != nil {
		t.Fatalf("Read nonexistent: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("expected 0 events for nonexistent session, got %d", len(events))
	}
}

// TestLastSeqNonexistentSession verifies that LastSeq returns 0 for a
// nonexistent session.
func TestLastSeqNonexistentSession(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	seq, err := s.LastSeq(context.Background(), "ses_nonexistent")
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	if seq != 0 {
		t.Fatalf("expected 0 for nonexistent session, got %d", seq)
	}
}

// TestConcurrentAppendSeqUniqueness verifies that concurrent appends produce
// unique, contiguous sequence numbers.
func TestConcurrentAppendSeqUniqueness(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()
	sid := event.SessionID("ses_concurrent")
	tenant := event.TenantID("tnt_test")

	// Create the session first.
	_, err := s.Append(ctx, validDraft(event.KindSessionCreated, sid, tenant,
		event.SessionCreated{WorkspaceID: "wsp_test", AgentName: "wakil", CreatedBy: "usr_test"}))
	if err != nil {
		t.Fatalf("Append SessionCreated: %v", err)
	}

	// 10 goroutines, each appending 5 events.
	const goroutines, perG = 10, 5
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func() {
			defer wg.Done()
			for i := 0; i < perG; i++ {
				_, _ = s.Append(ctx, validDraft(event.KindMessageCommitted, sid, tenant,
					event.MessageCommitted{TurnID: "trn_concurrent", Text: "msg"}))
			}
		}()
	}
	wg.Wait()

	// Verify: LastSeq should be 1 + goroutines*perG = 51.
	last, err := s.LastSeq(ctx, sid)
	if err != nil {
		t.Fatalf("LastSeq: %v", err)
	}
	expected := event.Seq(1 + goroutines*perG)
	if last != expected {
		t.Fatalf("expected last_seq=%d, got %d", expected, last)
	}

	// Verify: all seqs 2..51 present and unique (contiguous).
	events, err := s.Read(ctx, sid, 1, 0) // after=1 → skip SessionCreated
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(events) != goroutines*perG {
		t.Fatalf("expected %d events, got %d", goroutines*perG, len(events))
	}
	seen := make(map[event.Seq]bool)
	for _, e := range events {
		if seen[e.Seq] {
			t.Fatalf("duplicate seq %d", e.Seq)
		}
		seen[e.Seq] = true
	}
	// Verify contiguous 2..51.
	for i := event.Seq(2); i <= expected; i++ {
		if !seen[i] {
			t.Fatalf("missing seq %d", i)
		}
	}
}

// TestReopenDurability verifies that events persist across close → reopen.
func TestReopenDurability(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	ctx := context.Background()
	sid := event.SessionID("ses_persist")
	tenant := event.TenantID("tnt_test")

	// First instance: create session + events.
	s1, err := NewSQLiteStore(ctx, dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore 1: %v", err)
	}
	_, err = s1.Append(ctx, validDraft(event.KindSessionCreated, sid, tenant,
		event.SessionCreated{WorkspaceID: "wsp_test", AgentName: "wakil", CreatedBy: "usr_test"}))
	if err != nil {
		t.Fatalf("Append SessionCreated: %v", err)
	}
	_, err = s1.Append(ctx, validDraft(event.KindMessageCommitted, sid, tenant,
		event.MessageCommitted{TurnID: "trn_test1", Text: "persisted message"}))
	if err != nil {
		t.Fatalf("Append MessageCommitted: %v", err)
	}
	last1, _ := s1.LastSeq(ctx, sid)
	if err := s1.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Second instance: reopen, verify data persisted.
	s2, err := NewSQLiteStore(ctx, dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore 2: %v", err)
	}
	defer s2.Close()

	last2, err := s2.LastSeq(ctx, sid)
	if err != nil {
		t.Fatalf("LastSeq after reopen: %v", err)
	}
	if last2 != last1 {
		t.Fatalf("last_seq changed across reopen: before=%d, after=%d", last1, last2)
	}

	events, err := s2.Read(ctx, sid, 0, 0)
	if err != nil {
		t.Fatalf("Read after reopen: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events after reopen, got %d", len(events))
	}
	// Verify the persisted message text.
	mc, ok := events[1].Payload.(event.MessageCommitted)
	if !ok {
		t.Fatalf("expected MessageCommitted payload, got %T", events[1].Payload)
	}
	if mc.Text != "persisted message" {
		t.Fatalf("expected 'persisted message', got %q", mc.Text)
	}
}

// TestPragmasAfterReopen verifies that PRAGMA foreign_keys is ON after reopen.
func TestPragmasAfterReopen(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test.db")
	ctx := context.Background()

	s1, err := NewSQLiteStore(ctx, dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore 1: %v", err)
	}
	s1.Close()

	s2, err := NewSQLiteStore(ctx, dbPath)
	if err != nil {
		t.Fatalf("NewSQLiteStore 2: %v", err)
	}
	defer s2.Close()

	// Verify PRAGMA foreign_keys is ON.
	var fk int
	if err := s2.db.QueryRow("PRAGMA foreign_keys").Scan(&fk); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if fk != 1 {
		t.Fatalf("expected foreign_keys=1, got %d", fk)
	}

	// Verify PRAGMA journal_mode is WAL.
	var mode string
	if err := s2.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("expected journal_mode=wal, got %s", mode)
	}
}

// TestReadPayloadDecodedAsValue verifies that Read returns payloads as value
// types (not pointers), matching MemLog's in-memory representation.
func TestReadPayloadDecodedAsValue(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	ctx := context.Background()
	sid := event.SessionID("ses_val")
	tenant := event.TenantID("tnt_test")

	_, _ = s.Append(ctx, validDraft(event.KindSessionCreated, sid, tenant,
		event.SessionCreated{WorkspaceID: "wsp_test", AgentName: "wakil", CreatedBy: "usr_test"}))
	_, _ = s.Append(ctx, validDraft(event.KindTurnStarted, sid, tenant,
		event.TurnStarted{TurnID: "trn_test1", TurnIndex: 1}))

	events, err := s.Read(ctx, sid, 0, 0)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	// The first event should be a SessionCreated VALUE.
	if _, ok := events[0].Payload.(event.SessionCreated); !ok {
		t.Fatalf("expected SessionCreated value, got %T", events[0].Payload)
	}
	// The second should be a TurnStarted VALUE.
	if _, ok := events[1].Payload.(event.TurnStarted); !ok {
		t.Fatalf("expected TurnStarted value, got %T", events[1].Payload)
	}
}

// TestAppendSessionCreatedPointerPayload verifies that a pointer-form
// SessionCreated payload is handled correctly (defense-in-depth — the host
// always passes values, but the codec/type system should not panic).
func TestAppendSessionCreatedPointerPayload(t *testing.T) {
	s, cleanup := newTestStore(t)
	defer cleanup()

	draft := event.Event{
		TenantID:  "tnt_test",
		SessionID: "ses_ptr",
		Ts:        time.Now().UTC(),
		Kind:      event.KindSessionCreated,
		Payload:   &event.SessionCreated{WorkspaceID: "wsp_test", AgentName: "wakil", CreatedBy: "usr_test"},
	}
	committed, err := s.Append(context.Background(), draft)
	if err != nil {
		t.Fatalf("Append with pointer payload: %v", err)
	}
	if committed.Seq != 1 {
		t.Fatalf("expected seq=1, got %d", committed.Seq)
	}
}
