package sessionhost

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
)

// TestEmitterDurableOrdering verifies that concurrent Emit from many goroutines
// is observed by a subscriber in exact increasing Seq order (the emitMu
// append→notify serialization), never reordered.
func TestEmitterDurableOrdering(t *testing.T) {
	const n = 200
	var wg sync.WaitGroup
	var gotMu sync.Mutex
	var got []event.Seq
	ready := make(chan struct{})

	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		// One stable tool-call ID for all concurrent emits (validated once, not
		// derived from TurnID — avoids coupling to the ID generator's length).
		toolCallID := event.ToolCallID("tcl_ordering_test")
		// Fan out n concurrent durable emits, then wait for them all.
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func() {
				defer wg.Done()
				<-ready
				if err := in.Emit.Emit(event.KindToolCallStarted, event.ToolCallStarted{
					TurnID:     in.TurnID,
					ToolCallID: toolCallID,
					Name:       "test_tool",
					ArgDigest:  "d",
				}); err != nil {
					t.Errorf("Emit: %v", err)
				}
			}()
		}
		close(ready)
		wg.Wait()
		return "done", nil
	})
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	defer h.Close(context.Background())

	s := createSession(t, h, p)
	sub, err := h.Subscribe(context.Background(), p, s.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	// Drain replay (SessionCreated).
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := sub.Next(ctx); err != nil {
		t.Fatalf("drain: %v", err)
	}

	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "go"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	// Read live events until we see TurnCompleted; record the durable SEQ of
	// every ToolCallStarted, in delivery order.
	seenCompleted := false
	for !seenCompleted {
		ev, err := sub.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		switch ev.Kind {
		case event.KindToolCallStarted:
			gotMu.Lock()
			got = append(got, ev.Seq)
			gotMu.Unlock()
		case event.KindTurnCompleted:
			seenCompleted = true
		}
	}

	gotMu.Lock()
	defer gotMu.Unlock()
	if len(got) != n {
		t.Fatalf("observed %d ToolCallStarted, want %d", len(got), n)
	}
	for i, s := range got {
		if i == 0 {
			continue
		}
		if s <= got[i-1] {
			t.Fatalf("seq not strictly increasing: %v", got)
		}
	}
}

// TestEmitterFenceRejectsLateEmit verifies the terminal-ordering contract: once
// a turn completes, its emitter is fenced and a later (concurrent) Emit returns
// ErrEmitterClosed.
func TestEmitterFenceRejectsLateEmit(t *testing.T) {
	release := make(chan struct{})
	var em Emitter
	turnRan := make(chan struct{})

	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		em = in.Emit
		close(turnRan)
		<-release
		return "done", nil
	})
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	defer h.Close(context.Background())

	s := createSession(t, h, p)
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "go"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	<-turnRan
	close(release)
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})

	if em == nil {
		t.Fatal("emitter was nil")
	}
	err := em.Emit(event.KindMemoryProposed, event.MemoryProposed{Key: "k", Kind: "note", Writer: "w"})
	if !errors.Is(err, ErrEmitterClosed) {
		t.Fatalf("Emit after fence = %v, want ErrEmitterClosed", err)
	}
}

// TestEmitterRejectsHostReservedAndEphemeralKinds verifies the allowlist.
func TestEmitterRejectsHostReservedAndEphemeralKinds(t *testing.T) {
	release := make(chan struct{})
	turnEntered := make(chan struct{})
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		close(turnEntered)
		if err := in.Emit.Emit(event.KindTurnCompleted, event.TurnCompleted{TurnID: in.TurnID, Outcome: "complete"}); !errors.Is(err, core.ErrInvalidInput) {
			t.Errorf("host-reserved Emit err = %v, want ErrInvalidInput", err)
		}
		if err := in.Emit.Emit(event.KindMessageDelta, event.MessageDelta{Text: "x"}); !errors.Is(err, core.ErrInvalidInput) {
			t.Errorf("ephemeral Emit err = %v, want ErrInvalidInput", err)
		}
		if err := in.Emit.Emit(event.KindSessionClosed, event.SessionClosed{Reason: "x"}); !errors.Is(err, core.ErrInvalidInput) {
			t.Errorf("host-reserved SessionClosed Emit err = %v, want ErrInvalidInput", err)
		}
		<-release
		return "done", nil
	})
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	defer h.Close(context.Background())

	s := createSession(t, h, p)
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "go"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	<-turnEntered
	close(release)
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})
}

// TestNotifyEphemeralNotInLog verifies ephemeral events are live-only: they
// reach a live subscriber but never appear in ListEvents (Seq 0, never durable).
func TestNotifyEphemeralNotInLog(t *testing.T) {
	var gotDelta bool
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		in.Emit.Notify(event.KindMessageDelta, event.MessageDelta{Text: "delta"})
		return "committed", nil
	})
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	defer h.Close(context.Background())

	s := createSession(t, h, p)
	sub, err := h.Subscribe(context.Background(), p, s.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := sub.Next(ctx); err != nil {
		t.Fatalf("drain replay: %v", err)
	}
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "go"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	seenCompleted := false
	for !seenCompleted {
		ev, err := sub.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if ev.Kind == event.KindMessageDelta {
			if ev.Seq != 0 {
				t.Fatalf("ephemeral event has seq %d, want 0", ev.Seq)
			}
			gotDelta = true
		}
		if ev.Kind == event.KindTurnCompleted {
			seenCompleted = true
		}
	}
	if !gotDelta {
		t.Fatal("did not observe MessageDelta live")
	}

	// Ephemeral never durable.
	events, err := h.ListEvents(context.Background(), p, s.ID, 0, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	for _, e := range events {
		if e.Kind == event.KindMessageDelta {
			t.Fatalf("MessageDelta appears in durable log: %#v", e)
		}
	}
}

// TestInternalErrorClassification verifies an emitter/store failure produces
// SessionError{reason:"internal_error"}, not backend_failure, and parks in error.
func TestInternalErrorClassification(t *testing.T) {
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}

	// Inject a store whose Append fails for the tool-call kind with the fail ID,
	// so the emitter's Emit returns ErrEmitFailed but host lifecycle appends work.
	errStore := &failStore{inner: NewMemLog(), failFor: event.KindToolCallStarted, failID: event.ToolCallID("tcl_fail")}
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		return "", in.Emit.Emit(event.KindToolCallStarted, event.ToolCallStarted{
			TurnID:     in.TurnID,
			ToolCallID: event.ToolCallID("tcl_fail"),
			Name:       "x",
			ArgDigest:  "d",
		})
	}, WithStore(errStore))
	defer h.Close(context.Background())

	s := createSession(t, h, p)
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "go"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionError
	})

	events, _ := h.ListEvents(context.Background(), p, s.ID, 0, 0)
	var reason string
	for _, e := range events {
		if e.Kind == event.KindSessionError {
			reason = e.Payload.(event.SessionError).Reason
		}
	}
	if reason != "internal_error" {
		t.Fatalf("SessionError.Reason = %q, want internal_error (events=%d)", reason, len(events))
	}
}

// failStore wraps a Store and fails Append only for a designated kind whose
// payload carries a matching tool-call ID, simulating store failure for that
// specific append.
type failStore struct {
	inner   Store
	failFor event.Kind
	failID  event.ToolCallID
}

func (f *failStore) Append(ctx context.Context, d event.Event) (event.Event, error) {
	if d.Kind == f.failFor {
		if p, ok := d.Payload.(event.ToolCallStarted); ok && p.ToolCallID == f.failID {
			return event.Event{}, errors.New("store append failed")
		}
	}
	return f.inner.Append(ctx, d)
}
func (f *failStore) LastSeq(ctx context.Context, sid event.SessionID) (event.Seq, error) {
	return f.inner.LastSeq(ctx, sid)
}
func (f *failStore) Read(ctx context.Context, sid event.SessionID, after event.Seq, limit int) ([]event.Event, error) {
	return f.inner.Read(ctx, sid, after, limit)
}

// TestTurnInputUserID verifies the submitter UserID reaches the TurnInput.
func TestTurnInputUserID(t *testing.T) {
	var got event.UserID
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		got = in.UserID
		return "done", nil
	})
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_alice", Role: core.RoleOwner}
	defer h.Close(context.Background())

	s := createSession(t, h, p)
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "go"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})
	if got != "usr_alice" {
		t.Fatalf("TurnInput.UserID = %q, want usr_alice", got)
	}
}

// TestEmitterFenceConcurrentRace verifies the terminal-ordering invariant under
// concurrency: worker goroutines emitting while the turn returns to finalization
// must either (a) have their event land strictly BEFORE TurnCompleted's seq, or
// (b) be rejected with ErrEmitterClosed. No turn-emitted durable event may appear
// after its turn's TurnCompleted (exit criterion 3).
func TestEmitterFenceConcurrentRace(t *testing.T) {
	start := make(chan struct{})
	var wg sync.WaitGroup

	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		// Start workers that emit as fast as they can until the fence closes.
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for {
					err := in.Emit.Emit(event.KindMemoryProposed, event.MemoryProposed{Key: "k", Kind: "note", Writer: "w"})
					if err != nil {
						if !errors.Is(err, ErrEmitterClosed) {
							t.Errorf("Emit err = %v, want ErrEmitterClosed", err)
						}
						return
					}
				}
			}()
		}
		close(start)
		// Return immediately; the workers race finalization.
		return "done", nil
	})
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	defer h.Close(context.Background())

	s := createSession(t, h, p)
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "go"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	wg.Wait() // all workers hit the fence
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})

	events, err := h.ListEvents(context.Background(), p, s.ID, 0, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	var turnCompletedSeq event.Seq
	for _, e := range events {
		if e.Kind == event.KindTurnCompleted {
			turnCompletedSeq = e.Seq
		}
	}
	if turnCompletedSeq == 0 {
		t.Fatal("no TurnCompleted in log")
	}
	for _, e := range events {
		if e.Kind == event.KindMemoryProposed && e.Seq >= turnCompletedSeq {
			t.Fatalf("turn-emitted event seq=%d >= TurnCompleted seq=%d (terminal ordering violated)", e.Seq, turnCompletedSeq)
		}
	}
}

// TestNotifyRejectsDurableKinds verifies Notify silently drops durable kinds
// (they are not notifications) and never propagates them to subscribers or log.
func TestNotifyRejectsDurableKinds(t *testing.T) {
	var sawDurableViaLive bool
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		// A durable kind through Notify must be dropped (not delivered live).
		in.Emit.Notify(event.KindToolCallStarted, event.ToolCallStarted{
			TurnID:     in.TurnID,
			ToolCallID: event.ToolCallID("tcl_notify_test"),
			Name:       "x",
			ArgDigest:  "d",
		})
		return "done", nil
	})
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	defer h.Close(context.Background())

	s := createSession(t, h, p)
	sub, err := h.Subscribe(context.Background(), p, s.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := sub.Next(ctx); err != nil { // drain SessionCreated replay
		t.Fatalf("drain: %v", err)
	}
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "go"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	for {
		ev, err := sub.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if ev.Kind == event.KindToolCallStarted {
			sawDurableViaLive = true
		}
		if ev.Kind == event.KindTurnCompleted {
			break
		}
	}
	if sawDurableViaLive {
		t.Fatal("durable kind delivered live via Notify (must be dropped)")
	}
	// And it must not be in the durable log either (Notify is not Emit).
	events, _ := h.ListEvents(context.Background(), p, s.ID, 0, 0)
	for _, e := range events {
		if e.Kind == event.KindToolCallStarted {
			t.Fatalf("durable kind via Notify appears in log: %#v", e)
		}
	}
}

// TestSnapshotExcludesEphemeral verifies SessionSnapshot contains no ephemeral
// events (exit criterion 5: ListEvents AND SessionSnapshot exclude them).
func TestSnapshotExcludesEphemeral(t *testing.T) {
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		in.Emit.Notify(event.KindMessageDelta, event.MessageDelta{Text: "delta"})
		return "committed", nil
	})
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	defer h.Close(context.Background())

	s := createSession(t, h, p)
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "go"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})
	snap, err := h.SessionSnapshot(context.Background(), p, s.ID)
	if err != nil {
		t.Fatalf("SessionSnapshot: %v", err)
	}
	for _, e := range snap.Events {
		if e.Kind == event.KindMessageDelta {
			t.Fatalf("ephemeral MessageDelta in SessionSnapshot: %#v", e)
		}
	}
}
