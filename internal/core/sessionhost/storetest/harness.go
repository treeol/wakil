// Package storetest provides a shared store contract test harness for
// sessionhost.Store implementations (card #148 P1). It is imported by both the
// MemLog and SQLiteStore test suites to verify behavioral equivalence.
package storetest

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionhost"
)

// StoreFactory constructs a Store for one test. The store is cleaned up by the
// factory (via t.Cleanup or similar).
type StoreFactory func(t *testing.T) sessionhost.Store

// RunContract runs a suite of store contract tests against a store constructed
// by newStore. It verifies the operations the host exercises: append
// (SessionCreated first, then other events), read (cursor semantics), LastSeq,
// and concurrent seq uniqueness.
//
// The harness always appends SessionCreated first — this matches host behavior
// (CreateSession appends SessionCreated before any other event). MemLog
// auto-creates per-session logs on any append; SQLiteStore requires
// SessionCreated first. This divergence is documented and tested in
// SQLiteStore's own tests (non-SessionCreated to unknown session → error).
func RunContract(t *testing.T, newStore StoreFactory) {
	t.Helper()

	t.Run("AppendSessionCreated_ReadReturnsIt", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		sid := event.SessionID("ses_contract_1")
		tenant := event.TenantID("tnt_test")

		committed, err := s.Append(ctx, makeDraft(event.KindSessionCreated, sid, tenant,
			event.SessionCreated{WorkspaceID: "wsp_test", AgentName: "wakil", CreatedBy: "usr_test"}))
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
		if committed.Seq != 1 {
			t.Fatalf("expected seq=1, got %d", committed.Seq)
		}

		events, err := s.Read(ctx, sid, 0, 0)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(events) != 1 || events[0].Seq != 1 {
			t.Fatalf("expected 1 event at seq 1, got %d events", len(events))
		}
		if events[0].Kind != event.KindSessionCreated {
			t.Fatalf("expected SessionCreated, got %s", events[0].Kind)
		}
	})

	t.Run("AppendMultiple_AscendingSeq", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		sid := event.SessionID("ses_contract_2")
		tenant := event.TenantID("tnt_test")

		appendSessionCreated(t, s, sid, tenant)
		for i := 1; i <= 5; i++ {
			_, err := s.Append(ctx, makeDraft(event.KindMessageCommitted, sid, tenant,
				event.MessageCommitted{TurnID: event.TurnID(fmt.Sprintf("trn_%d", i)), Text: fmt.Sprintf("msg %d", i)}))
			if err != nil {
				t.Fatalf("Append %d: %v", i, err)
			}
		}

		events, err := s.Read(ctx, sid, 0, 0)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(events) != 6 {
			t.Fatalf("expected 6 events, got %d", len(events))
		}
		for i, e := range events {
			if e.Seq != event.Seq(i+1) {
				t.Fatalf("event %d has seq %d, expected %d", i, e.Seq, i+1)
			}
		}
	})

	t.Run("LastSeq_ReturnsHighest", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		sid := event.SessionID("ses_contract_3")
		tenant := event.TenantID("tnt_test")

		appendSessionCreated(t, s, sid, tenant)
		for i := 0; i < 3; i++ {
			_, _ = s.Append(ctx, makeDraft(event.KindMessageCommitted, sid, tenant,
				event.MessageCommitted{TurnID: "trn_x", Text: "msg"}))
		}
		last, err := s.LastSeq(ctx, sid)
		if err != nil {
			t.Fatalf("LastSeq: %v", err)
		}
		if last != 4 {
			t.Fatalf("expected last_seq=4, got %d", last)
		}
	})

	t.Run("LastSeq_Nonexistent_Returns0", func(t *testing.T) {
		s := newStore(t)
		last, err := s.LastSeq(context.Background(), "ses_nonexistent")
		if err != nil {
			t.Fatalf("LastSeq: %v", err)
		}
		if last != 0 {
			t.Fatalf("expected 0 for nonexistent session, got %d", last)
		}
	})

	t.Run("ConcurrentAppend_SeqUniqueContiguous", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		sid := event.SessionID("ses_contract_4")
		tenant := event.TenantID("tnt_test")

		appendSessionCreated(t, s, sid, tenant)

		const goroutines, perG = 8, 10
		var wg sync.WaitGroup
		wg.Add(goroutines)
		for g := 0; g < goroutines; g++ {
			go func() {
				defer wg.Done()
				for i := 0; i < perG; i++ {
					_, _ = s.Append(ctx, makeDraft(event.KindMessageCommitted, sid, tenant,
						event.MessageCommitted{TurnID: "trn_c", Text: "concurrent"}))
				}
			}()
		}
		wg.Wait()

		last, err := s.LastSeq(ctx, sid)
		if err != nil {
			t.Fatalf("LastSeq: %v", err)
		}
		expected := event.Seq(1 + goroutines*perG)
		if last != expected {
			t.Fatalf("expected last_seq=%d, got %d", expected, last)
		}

		events, err := s.Read(ctx, sid, 1, 0)
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
		for i := event.Seq(2); i <= expected; i++ {
			if !seen[i] {
				t.Fatalf("missing seq %d", i)
			}
		}
	})

	t.Run("CursorSemantics", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		sid := event.SessionID("ses_contract_5")
		tenant := event.TenantID("tnt_test")

		appendSessionCreated(t, s, sid, tenant)
		for i := 1; i <= 3; i++ {
			_, _ = s.Append(ctx, makeDraft(event.KindMessageCommitted, sid, tenant,
				event.MessageCommitted{TurnID: "trn_x", Text: "msg"}))
		}

		// after=0 → all 4 events.
		events, _ := s.Read(ctx, sid, 0, 0)
		if len(events) != 4 {
			t.Fatalf("after=0: expected 4, got %d", len(events))
		}

		// after=2 → events 3,4.
		events, _ = s.Read(ctx, sid, 2, 0)
		if len(events) != 2 {
			t.Fatalf("after=2: expected 2, got %d", len(events))
		}
		if events[0].Seq != 3 {
			t.Fatalf("after=2: expected first seq=3, got %d", events[0].Seq)
		}

		// after=0, limit=2 → first 2 events.
		events, _ = s.Read(ctx, sid, 0, 2)
		if len(events) != 2 {
			t.Fatalf("limit=2: expected 2, got %d", len(events))
		}

		// after=100 → empty.
		events, _ = s.Read(ctx, sid, 100, 0)
		if len(events) != 0 {
			t.Fatalf("after=100: expected 0, got %d", len(events))
		}
	})

	t.Run("EphemeralRejected", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		sid := event.SessionID("ses_contract_6")
		tenant := event.TenantID("tnt_test")

		appendSessionCreated(t, s, sid, tenant)
		_, err := s.Append(ctx, makeDraft(event.KindMessageDelta, sid, tenant,
			event.MessageDelta{Text: "streaming"}))
		if err == nil {
			t.Fatal("ephemeral append should be rejected")
		}
	})

	t.Run("Replay_ReconstructsProjection", func(t *testing.T) {
		s := newStore(t)
		ctx := context.Background()
		sid := event.SessionID("ses_contract_7")
		tenant := event.TenantID("tnt_test")

		// Build a session projection: SessionCreated, UserMessage, TurnStarted,
		// MessageCommitted, TurnCompleted.
		appendSessionCreated(t, s, sid, tenant)
		_, _ = s.Append(ctx, makeDraft(event.KindUserMessageCommitted, sid, tenant,
			event.UserMessageCommitted{TurnID: "trn_1", Text: "hello"}))
		_, _ = s.Append(ctx, makeDraft(event.KindTurnStarted, sid, tenant,
			event.TurnStarted{TurnID: "trn_1", TurnIndex: 1}))
		_, _ = s.Append(ctx, makeDraft(event.KindMessageCommitted, sid, tenant,
			event.MessageCommitted{TurnID: "trn_1", Text: "world"}))
		_, _ = s.Append(ctx, makeDraft(event.KindTurnCompleted, sid, tenant,
			event.TurnCompleted{TurnID: "trn_1", Outcome: "complete"}))

		// Read all events and reconstruct the projection.
		events, err := s.Read(ctx, sid, 0, 0)
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if len(events) != 5 {
			t.Fatalf("expected 5 events, got %d", len(events))
		}

		// Verify the projection: kinds in order.
		expectedKinds := []event.Kind{
			event.KindSessionCreated,
			event.KindUserMessageCommitted,
			event.KindTurnStarted,
			event.KindMessageCommitted,
			event.KindTurnCompleted,
		}
		for i, e := range events {
			if e.Kind != expectedKinds[i] {
				t.Fatalf("event %d: expected %s, got %s", i, expectedKinds[i], e.Kind)
			}
		}

		// Verify payload field reconstruction.
		sc := events[0].Payload.(event.SessionCreated)
		if sc.WorkspaceID != "wsp_test" {
			t.Fatalf("WorkspaceID: expected wsp_test, got %s", sc.WorkspaceID)
		}
		um := events[1].Payload.(event.UserMessageCommitted)
		if um.Text != "hello" {
			t.Fatalf("UserMessageCommitted.Text: expected hello, got %s", um.Text)
		}
		mc := events[3].Payload.(event.MessageCommitted)
		if mc.Text != "world" {
			t.Fatalf("MessageCommitted.Text: expected world, got %s", mc.Text)
		}
		tc := events[4].Payload.(event.TurnCompleted)
		if tc.Outcome != "complete" {
			t.Fatalf("TurnCompleted.Outcome: expected complete, got %s", tc.Outcome)
		}
	})
}

// makeDraft is a helper that creates a valid durable event draft.
func makeDraft(kind event.Kind, sid event.SessionID, tenant event.TenantID, payload any) event.Event {
	return event.Event{
		TenantID:  tenant,
		SessionID: sid,
		Ts:        time.Now().UTC(),
		Kind:      kind,
		Payload:   payload,
	}
}

// appendSessionCreated appends a SessionCreated event, failing the test on error.
func appendSessionCreated(t *testing.T, s sessionhost.Store, sid event.SessionID, tenant event.TenantID) {
	t.Helper()
	_, err := s.Append(context.Background(), makeDraft(event.KindSessionCreated, sid, tenant,
		event.SessionCreated{WorkspaceID: "wsp_test", AgentName: "wakil", CreatedBy: "usr_test"}))
	if err != nil {
		t.Fatalf("Append SessionCreated: %v", err)
	}
}

// Compile-time check that core.ErrSessionNotFound is referenced.
var _ = core.ErrSessionNotFound
