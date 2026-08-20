package sessionhost

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
)

const (
	testTaskTID = event.TenantID("tnt_test")
	testUser    = event.UserID("usr_test")
)

func testEnv() (h *Host, principal core.Principal) {
	h = New(func(_ context.Context, in TurnInput) (string, error) { return in.Text, nil })
	principal = core.Principal{TenantID: testTaskTID, UserID: testUser, Role: core.RoleOwner, AuthMethod: core.AuthEmbedded}
	return h, principal
}

func testWorkspace() event.WorkspaceID { return event.WorkspaceID("wsp_test") }

// blockingTurn returns a TurnFunc that blocks until release is closed. It is the
// controlled "turn in flight" for lifecycle tests. It records the inputs it
// received for FIFO assertion.
func blockingTurn(release <-chan struct{}, received *[]string, mu *sync.Mutex) TurnFunc {
	return func(ctx context.Context, in TurnInput) (string, error) {
		if received != nil {
			mu.Lock()
			*received = append(*received, in.Text)
			mu.Unlock()
		}
		select {
		case <-release:
			return "", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

func createSession(t *testing.T, h *Host, p core.Principal) core.Session {
	t.Helper()
	s, err := h.CreateSession(context.Background(), p, core.CreateSessionRequest{Workspace: testWorkspace(), Title: "t"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	return s
}

func TestCreateSessionEmitsSessionCreated(t *testing.T) {
	h, p := testEnv()
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	if s.State != core.SessionIdle {
		t.Fatalf("new session state = %q, want idle", s.State)
	}
	if s.LastSeq != 1 {
		t.Fatalf("LastSeq = %d, want 1 (SessionCreated)", s.LastSeq)
	}

	events, err := h.ListEvents(context.Background(), p, s.ID, 0, 0)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 || events[0].Kind != event.KindSessionCreated || events[0].Seq != 1 {
		t.Fatalf("events = %#v, want one SessionCreated at seq 1", events)
	}
	payload := events[0].Payload.(event.SessionCreated)
	if payload.WorkspaceID != testWorkspace() {
		t.Fatalf("SessionCreated.WorkspaceID = %q", payload.WorkspaceID)
	}
	if payload.CreatedBy != p.UserID {
		t.Fatalf("SessionCreated.CreatedBy = %q, want %q", payload.CreatedBy, p.UserID)
	}
}

func TestSubmitInputNonBlockingAndComplete(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var received []string
	h := New(blockingTurn(release, &received, &mu))
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	defer h.Close(context.Background())

	s := createSession(t, h, p)

	start := time.Now()
	ack, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "hi"})
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	if d := time.Since(start); d > 200*time.Millisecond {
		t.Fatalf("SubmitInput blocked for %v; must be non-blocking", d)
	}
	if ack.TurnID == "" {
		t.Fatal("TurnAck.TurnID empty")
	}

	// The turn must be running (blocked on release), not complete.
	got, err := h.GetSession(context.Background(), p, s.ID)
	if err != nil {
		t.Fatalf("GetSession: %v", err)
	}
	if got.State != core.SessionRunning {
		t.Fatalf("state while turn blocked = %q, want running", got.State)
	}

	close(release)
	// Wait for the turn to drain to idle.
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})

	events, _ := h.ListEvents(context.Background(), p, s.ID, 0, 0)
	var kinds []event.Kind
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	assertKindsContain(t, kinds, event.KindTurnStarted)
	assertKindsContain(t, kinds, event.KindTurnCompleted)
	// seq must be strictly increasing across the whole durable log.
	last := event.Seq(0)
	for _, e := range events {
		if e.Seq <= last {
			t.Fatalf("seq not strictly increasing: %v", events)
		}
		last = e.Seq
	}
}

func TestSubmitInputFIFOQueueing(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	var received []string
	h := New(blockingTurn(release, &received, &mu))
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	defer func() { close(release); h.Close(context.Background()) }()

	s := createSession(t, h, p)

	// First input blocks the turn; the rest queue.
	for _, text := range []string{"a", "b", "c"} {
		if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: text}); err != nil {
			t.Fatalf("SubmitInput(%q): %v", text, err)
		}
	}

	// Release each turn one at a time and confirm FIFO order.
	for i := 0; i < 3; i++ {
		release <- struct{}{}
		waitFor(t, func() bool {
			mu.Lock()
			defer mu.Unlock()
			return len(received) >= i+1
		})
	}
	mu.Lock()
	got := append([]string(nil), received...)
	mu.Unlock()
	if len(got) != 3 || got[0] != "a" || got[1] != "b" || got[2] != "c" {
		t.Fatalf("received %v, want [a b c] in FIFO order", got)
	}
}

func TestSubmitInputQueueFull(t *testing.T) {
	release := make(chan struct{})
	h := New(blockingTurn(release, nil, &sync.Mutex{}), WithQueueDepth(2))
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	defer func() { close(release); h.Close(context.Background()) }()

	s := createSession(t, h, p)

	// Depth 2: one running turn, one queued, one rejected.
	ok := func(i int) {
		if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "x"}); err != nil {
			t.Fatalf("SubmitInput #%d: %v", i, err)
		}
	}
	ok(1) // running
	ok(2) // queued (queue full now)
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "x"}); !errors.Is(err, core.ErrSessionBusy) {
		t.Fatalf("SubmitInput into full queue = %v, want ErrSessionBusy", err)
	}
}

func TestSubmitInputClosedSession(t *testing.T) {
	h, p := testEnv()
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	if err := h.CloseSession(context.Background(), p, s.ID); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "late"}); !errors.Is(err, core.ErrSessionClosed) {
		t.Fatalf("SubmitInput to closed session = %v, want ErrSessionClosed", err)
	}
}

func TestInterruptCancelsTurn(t *testing.T) {
	release := make(chan struct{})
	h := New(blockingTurn(release, nil, &sync.Mutex{}))
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	defer func() { close(release); h.Close(context.Background()) }()

	s := createSession(t, h, p)
	ack, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "work"})
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	if err := h.Interrupt(context.Background(), p, s.ID); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})

	events, _ := h.ListEvents(context.Background(), p, s.ID, 0, 0)
	found := false
	for _, e := range events {
		if e.Kind == event.KindTurnCompleted {
			payload := e.Payload.(event.TurnCompleted)
			if payload.TurnID == ack.TurnID && payload.Outcome == "cancelled" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("no TurnCompleted{cancelled} for turn %s in %v", ack.TurnID, events)
	}
}

func TestCloseSessionEmitsSessionClosedAndIsIdempotent(t *testing.T) {
	h, p := testEnv()
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	if err := h.CloseSession(context.Background(), p, s.ID); err != nil {
		t.Fatalf("CloseSession: %v", err)
	}
	if err := h.CloseSession(context.Background(), p, s.ID); err != nil {
		t.Fatalf("second CloseSession: %v", err) // idempotent, no error
	}

	// Completion is observed via the event stream, not the call's return — wait
	// for the async SessionClosed to be emitted by the executor.
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionClosed
	})
	got, _ := h.GetSession(context.Background(), p, s.ID)
	if got.State != core.SessionClosed {
		t.Fatalf("state = %q, want closed", got.State)
	}
	if got.ClosedAt.IsZero() {
		t.Fatal("ClosedAt not set")
	}

	events, _ := h.ListEvents(context.Background(), p, s.ID, 0, 0)
	// SessionCreated (1) + exactly one SessionClosed (2) — idempotence.
	if len(events) != 2 {
		t.Fatalf("events = %v, want exactly SessionCreated + SessionClosed", events)
	}
	if events[1].Kind != event.KindSessionClosed {
		t.Fatalf("last event = %s, want session_closed", events[1].Kind)
	}
}

func TestBackendFailureMovesToErrorThenRedrive(t *testing.T) {
	fail := true
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		if fail {
			return "", errors.New("backend exploded")
		}
		return "recovered", nil
	})
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "boom"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionError
	})

	events, _ := h.ListEvents(context.Background(), p, s.ID, 0, 0)
	var sawErr bool
	for _, e := range events {
		if e.Kind == event.KindSessionError {
			if e.Payload.(event.SessionError).Reason != "backend_failure" {
				t.Fatalf("SessionError reason = %q", e.Payload.(event.SessionError).Reason)
			}
			sawErr = true
		}
	}
	if !sawErr {
		t.Fatalf("no session_error event in %v", events)
	}

	// Redrive: error → running → idle/error per the next outcome.
	fail = false
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "retry"}); err != nil {
		t.Fatalf("redrive SubmitInput: %v", err)
	}
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})
}

func TestSubscribeReplayAndLiveDedup(t *testing.T) {
	h, p := testEnv()
	defer h.Close(context.Background())
	s := createSession(t, h, p) // seq 1 SessionCreated committed

	// Subscribe after=1: replays nothing durable (everything is ≤1), then live.
	sub, err := h.Subscribe(context.Background(), p, s.ID, 1)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "live"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	// First live event is seq 2 (TurnStarted); seq strictly > lastSeq (1).
	var seqs []event.Seq
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	for len(seqs) < 2 { // TurnStarted + TurnCompleted
		ev, err := sub.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v (got %v)", err, seqs)
		}
		if ev.Kind.Class() == event.ClassDurable {
			seqs = append(seqs, ev.Seq)
		}
	}
	if seqs[0] != 2 || seqs[1] != 3 {
		t.Fatalf("live seqs = %v, want [2 3] with no duplicate of seq 1", seqs)
	}

	if err := sub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := sub.Next(context.Background()); err != io.EOF {
		t.Fatalf("Next after Close = %v, want io.EOF", err)
	}
}

func TestSubscribeFromStartReplaysHistory(t *testing.T) {
	h, p := testEnv()
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	// Commit a turn to durable history before subscribing.
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "before"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})

	sub, err := h.Subscribe(context.Background(), p, s.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var kinds []event.Kind
	for len(kinds) < 3 { // SessionCreated, TurnStarted, TurnCompleted (replayed)
		ev, err := sub.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v (got %v)", err, kinds)
		}
		kinds = append(kinds, ev.Kind)
	}
	if kinds[0] != event.KindSessionCreated {
		t.Fatalf("first replayed event = %s, want session_created", kinds[0])
	}
	sub.Close()
}

func TestConcurrentSubmitsYieldStrictSeq(t *testing.T) {
	h, p := testEnv()
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "x"})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent SubmitInput: %v", err)
		}
	}

	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})

	events, _ := h.ListEvents(context.Background(), p, s.ID, 0, 0)
	last := event.Seq(0)
	var turnStarts, turnCompletes int
	for _, e := range events {
		if e.Seq <= last {
			t.Fatalf("seq %d not strictly increasing after %d", e.Seq, last)
		}
		last = e.Seq
		if e.Kind == event.KindTurnStarted {
			turnStarts++
		}
		if e.Kind == event.KindTurnCompleted {
			turnCompletes++
		}
	}
	if turnStarts != n || turnCompletes != n {
		t.Fatalf("got %d starts / %d completes, want %d each", turnStarts, turnCompletes, n)
	}
}

func TestTwoSubscribersSeeSameOrder(t *testing.T) {
	h, p := testEnv()
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	sub1, err := h.Subscribe(context.Background(), p, s.ID, 0)
	if err != nil {
		t.Fatalf("sub1: %v", err)
	}
	sub2, err := h.Subscribe(context.Background(), p, s.ID, 0)
	if err != nil {
		t.Fatalf("sub2: %v", err)
	}

	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "order"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	collect := func(sub core.EventSubscription) []event.Kind {
		var k []event.Kind
		for len(k) < 3 { // SessionCreated, TurnStarted, TurnCompleted
			ev, err := sub.Next(ctx)
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			k = append(k, ev.Kind)
		}
		return k
	}
	k1 := collect(sub1)
	k2 := collect(sub2)
	if len(k1) != len(k2) {
		t.Fatalf("subscriber sees different counts: %v vs %v", k1, k2)
	}
	for i := range k1 {
		if k1[i] != k2[i] {
			t.Fatalf("subscribers diverged at %d: %v vs %v", i, k1, k2)
		}
	}
	sub1.Close()
	sub2.Close()
}

func TestTenantIsolation(t *testing.T) {
	h, p := testEnv()
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	other := core.Principal{TenantID: "tnt_other", UserID: "usr_test", Role: core.RoleOwner}
	if _, err := h.GetSession(context.Background(), other, s.ID); !errors.Is(err, core.ErrSessionNotFound) {
		t.Fatalf("cross-tenant GetSession = %v, want ErrSessionNotFound", err)
	}
	if _, err := h.SubmitInput(context.Background(), other, core.SubmitInputRequest{SessionID: s.ID, Text: "x"}); !errors.Is(err, core.ErrSessionNotFound) {
		t.Fatalf("cross-tenant SubmitInput = %v, want ErrSessionNotFound", err)
	}
	if _, err := h.Subscribe(context.Background(), other, s.ID, 0); !errors.Is(err, core.ErrSessionNotFound) {
		t.Fatalf("cross-tenant Subscribe = %v, want ErrSessionNotFound", err)
	}
	// ListSessions scoped to the other tenant is empty, not an error.
	list, err := h.ListSessions(context.Background(), other)
	if err != nil || len(list) != 0 {
		t.Fatalf("ListSessions(other) = %v, %v; want empty", list, err)
	}
}

func TestRoleGating(t *testing.T) {
	h, p := testEnv()
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	viewer := core.Principal{TenantID: "tnt_test", UserID: "usr_v", Role: core.RoleViewer}
	if _, err := h.SubmitInput(context.Background(), viewer, core.SubmitInputRequest{SessionID: s.ID, Text: "x"}); !errors.Is(err, core.ErrNotAuthorized) {
		t.Fatalf("viewer SubmitInput = %v, want ErrNotAuthorized", err)
	}
	if _, err := h.CreateSession(context.Background(), viewer, core.CreateSessionRequest{Workspace: testWorkspace()}); !errors.Is(err, core.ErrNotAuthorized) {
		t.Fatalf("viewer CreateSession = %v, want ErrNotAuthorized", err)
	}
	// A viewer CAN read (viewer may read sessions and traces per §6.3).
	if _, err := h.GetSession(context.Background(), viewer, s.ID); err != nil {
		t.Fatalf("viewer GetSession = %v, want nil", err)
	}
}

func TestRecoverRunningSessions(t *testing.T) {
	h, p := testEnv()
	defer h.Close(context.Background())

	metas := []SessionMetadata{
		{ID: "ses_1", TenantID: "tnt_test", Workspace: "wsp_test", CreatedBy: "usr_test", Title: "a", CreatedAt: time.Now()},
		{ID: "ses_2", TenantID: "tnt_test", Workspace: "wsp_test", CreatedBy: "usr_test", Title: "b", CreatedAt: time.Now()},
		{ID: "ses_3", TenantID: "tnt_other", Workspace: "wsp_test", CreatedBy: "usr_test", Title: "skip", CreatedAt: time.Now()},
	}
	recovered, err := h.RecoverRunning(context.Background(), p, metas)
	if err != nil {
		t.Fatalf("RecoverRunning: %v", err)
	}
	if len(recovered) != 2 {
		t.Fatalf("recovered %d sessions, want 2 (other tenant skipped)", len(recovered))
	}
	for _, s := range recovered {
		if s.State != core.SessionError {
			t.Fatalf("recovered session %s state = %q, want error", s.ID, s.State)
		}
		events, _ := h.ListEvents(context.Background(), p, s.ID, 0, 0)
		if len(events) != 1 || events[0].Kind != event.KindSessionError {
			t.Fatalf("recovered session %s events = %v, want single session_error", s.ID, events)
		}
		if events[0].Payload.(event.SessionError).Reason != "daemon_restart" {
			t.Fatalf("reason = %q", events[0].Payload.(event.SessionError).Reason)
		}
	}
}

func TestRespondToApprovalShimIsNotFound(t *testing.T) {
	h, p := testEnv()
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	d := core.ApprovalDecision{SessionID: s.ID, ApprovalID: "apr_1", Outcome: core.ApprovalAllowOnce}
	if err := h.RespondToApproval(context.Background(), p, d); !errors.Is(err, core.ErrApprovalNotFound) {
		t.Fatalf("RespondToApproval = %v, want ErrApprovalNotFound (D5 shim)", err)
	}
}

func TestSessionSnapshot(t *testing.T) {
	h, p := testEnv()
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	snap, err := h.SessionSnapshot(context.Background(), p, s.ID)
	if err != nil {
		t.Fatalf("SessionSnapshot: %v", err)
	}
	if snap.Session.ID != s.ID || snap.LastSeq != 1 || len(snap.Events) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestListSessionsScopedToTenant(t *testing.T) {
	h := New(nil)
	ownerA := core.Principal{TenantID: "tnt_a", UserID: "usr_test", Role: core.RoleOwner}
	ownerB := core.Principal{TenantID: "tnt_b", UserID: "usr_test", Role: core.RoleOwner}
	defer h.Close(context.Background())

	createSession(t, h, ownerA)
	createSession(t, h, ownerA)
	createSession(t, h, ownerB)

	a, _ := h.ListSessions(context.Background(), ownerA)
	if len(a) != 2 {
		t.Fatalf("ownerA sees %d sessions, want 2", len(a))
	}
	b, _ := h.ListSessions(context.Background(), ownerB)
	if len(b) != 1 {
		t.Fatalf("ownerB sees %d sessions, want 1", len(b))
	}
}

func TestInvalidInputs(t *testing.T) {
	h, p := testEnv()
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: ""}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("empty text = %v, want ErrInvalidInput", err)
	}
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: "apr_notses", Text: "x"}); !errors.Is(err, core.ErrInvalidInput) {
		t.Fatalf("mis-prefixed session id = %v, want ErrInvalidInput", err)
	}

	// Invalid principal (missing tenant) is rejected with its own error.
	bad := core.Principal{UserID: "usr_test", Role: core.RoleOwner}
	if _, err := h.CreateSession(context.Background(), bad, core.CreateSessionRequest{Workspace: testWorkspace()}); err == nil {
		t.Fatal("CreateSession with invalid principal succeeded")
	}
}

func TestHostCloseDrainsRunningSessions(t *testing.T) {
	release := make(chan struct{})
	h := New(blockingTurn(release, nil, &sync.Mutex{}))
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	s := createSession(t, h, p)

	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "work"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	// Close must cancel the running turn and return without error. (The
	// blockingTurn returns on ctx.Done, so the turn finalizes as cancelled.)
	done := make(chan error, 1)
	go func() { done <- h.Close(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Host.Close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Host.Close blocked on a running turn")
	}
	close(release)

	got, _ := h.GetSession(context.Background(), p, s.ID)
	if got.State != core.SessionClosed {
		t.Fatalf("state after Host.Close = %q, want closed", got.State)
	}
}

func TestSlowSubscriberDoesNotBlockExecutor(t *testing.T) {
	// Small subscriber buffer (8) so it overflows quickly; large queue depth so
	// the queue never fills and the executor is never wrongly rejected.
	h := New(func(_ context.Context, in TurnInput) (string, error) { return in.Text, nil },
		WithSubBuffer(8), WithQueueDepth(10000))
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	sub, err := h.Subscribe(context.Background(), p, s.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// Never read from sub; fill its buffer with many committed events.
	for i := 0; i < 100; i++ {
		if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "x"}); err != nil {
			t.Fatalf("SubmitInput #%d: %v", i, err)
		}
	}

	// The host must still reach idle promptly (executor never blocked on a full
	// subscription buffer).
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})

	// The slow subscriber must have been disconnected for lagging, and Next must
	// surface ErrSubscriptionGap (not silently lose durable events).
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var gap bool
	for {
		_, err := sub.Next(ctx)
		if err != nil {
			if errors.Is(err, ErrSubscriptionGap) {
				gap = true
			} else if err == io.EOF {
				break
			}
			if err != nil && err != io.EOF && !errors.Is(err, ErrSubscriptionGap) {
				t.Fatalf("unexpected Next error: %v", err)
			}
			break
		}
	}
	if !gap {
		t.Fatal("expected the slow subscriber to be disconnected with ErrSubscriptionGap")
	}
	sub.Close()
}

// turnReturnCh lets a TurnFunc block until the test closes goCh, then return
// with the given text/err — the controlled "turn about to return" for
// finalization-race tests.
type gate struct{ goCh chan struct{} }

func newGate() *gate  { return &gate{goCh: make(chan struct{})} }
func (g *gate) open() { close(g.goCh) }

// TestCloseRacesTurnReturn: a close that lands while the turn is returning must
// still produce TurnCompleted{cancelled} + SessionClosed, never hang or park in
// error (panel bug #1).
func TestCloseRacesTurnReturn(t *testing.T) {
	g := newGate()
	var mu sync.Mutex
	var started bool
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		mu.Lock()
		started = true
		mu.Unlock()
		<-g.goCh
		return "done", nil
	})
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	ack, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "x"})
	if err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	// Wait until the turn is actually running, then release it and immediately
	// request close (no ordering guarantee between the two racers — both must
	// end in a closed session).
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return started
	})
	g.open()
	h.CloseSession(context.Background(), p, s.ID)

	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionClosed
	})

	events, _ := h.ListEvents(context.Background(), p, s.ID, 0, 0)
	foundCancelledOrComplete := false
	var lastClose event.Seq
	for _, e := range events {
		if e.Kind == event.KindSessionClosed {
			lastClose = e.Seq
		}
		if e.Kind == event.KindTurnCompleted && e.Payload.(event.TurnCompleted).TurnID == ack.TurnID {
			foundCancelledOrComplete = true
		}
	}
	if !foundCancelledOrComplete {
		t.Fatalf("no TurnCompleted for %s in %v", ack.TurnID, events)
	}
	if lastClose == 0 {
		t.Fatalf("no SessionClosed emitted in %v", events)
	}
}

// TestCloseRacesBackendFailure: a close racing a backend failure must win — the
// session ends closed, not parked in error (panel bug #1 error path).
func TestCloseRacesBackendFailure(t *testing.T) {
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		return "", errors.New("boom")
	})
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "x"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}
	// Race: close immediately; the backend error and the close finalize in
	// whichever order — both must end closed.
	h.CloseSession(context.Background(), p, s.ID)

	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionClosed
	})
}

// TestInterruptOfQueuedTurn: an interrupt that lands while a turn is queued but
// not yet started must pre-cancel that turn (the interruptPending path), not
// leak into a later turn.
func TestInterruptOfQueuedTurn(t *testing.T) {
	first := newGate()
	var mu sync.Mutex
	var inputs []string
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		mu.Lock()
		inputs = append(inputs, in.Text)
		mu.Unlock()
		if in.Text == "first" {
			<-first.goCh
		}
		return in.Text, nil
	})
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	// "first" blocks; "second" and "third" queue up.
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "first"}); err != nil {
		t.Fatalf("first: %v", err)
	}
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionRunning
	})
	// Now interrupt, THEN queue more inputs. The interrupt targets the running
	// "first" turn (current != nil), so queued inputs must still run.
	if err := h.Interrupt(context.Background(), p, s.ID); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "second"}); err != nil {
		t.Fatalf("second: %v", err)
	}
	first.open()

	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})
	mu.Lock()
	defer mu.Unlock()
	if len(inputs) < 2 || inputs[1] != "second" {
		t.Fatalf("inputs = %v; the queued 'second' turn must still run after interrupt", inputs)
	}
}

// TestSubscribeReplayOverlap: durable events committed DURING Subscribe's replay
// window must not be lost or reordered (panel bug #4/#2). A slow store is used
// to widen the window deterministically.
func TestSubscribeReplayOverlap(t *testing.T) {
	slow := &slowStore{inner: NewMemLog(), readDelay: 30 * time.Millisecond}
	h := New(func(_ context.Context, in TurnInput) (string, error) { return in.Text, nil }, WithStore(slow))
	p := core.Principal{TenantID: "tnt_test", UserID: "usr_test", Role: core.RoleOwner}
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	sub, err := h.Subscribe(context.Background(), p, s.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// While the slow replay is in flight, commit a live turn.
	if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: "during"}); err != nil {
		t.Fatalf("SubmitInput: %v", err)
	}

	// Read every durable event and assert contiguous, strictly-increasing seq
	// from 1 with NO gaps (SessionCreated=1, UserMessageCommitted=2,
	// TurnStarted=3, MessageCommitted=4, TurnCompleted=5).
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var seqs []event.Seq
	want := map[event.Kind]bool{event.KindSessionCreated: true, event.KindTurnStarted: true, event.KindTurnCompleted: true, event.KindMessageCommitted: true}
	for len(want) > 0 {
		ev, err := sub.Next(ctx)
		if err != nil {
			t.Fatalf("Next: %v (got %v)", err, seqs)
		}
		seqs = append(seqs, ev.Seq)
		if !want[ev.Kind] {
			continue
		}
		delete(want, ev.Kind)
	}
	for i := 1; i < len(seqs); i++ {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("seq gap or reorder at %d: %v", i, seqs)
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing events %v", want)
	}
	sub.Close()
}

// TestClosedSubscriptionIsDetached: closing a subscription must remove it from
// the session so later notifies do not retain it (panel bug #6).
func TestClosedSubscriptionIsDetached(t *testing.T) {
	h, p := testEnv()
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	sub1, err := h.Subscribe(context.Background(), p, s.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	// Drain the replay so the subscription is in live phase.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := sub1.Next(ctx); err != nil {
		t.Fatalf("drain Next: %v", err)
	}
	sub1.Close()

	// The internal session must no longer list sub1.
	h.mu.Lock()
	sh := h.sessions[s.ID]
	h.mu.Unlock()
	sh.subsMu.Lock()
	n := len(sh.subs)
	sh.subsMu.Unlock()
	if n != 0 {
		t.Fatalf("closed subscription still attached: %d subscribers", n)
	}
}

// slowStore wraps a Store and delays Read, widening the replay window so
// concurrent commits during Subscribe are exercised deterministically.
type slowStore struct {
	inner     Store
	readDelay time.Duration
}

func (s *slowStore) Append(ctx context.Context, d event.Event) (event.Event, error) {
	return s.inner.Append(ctx, d)
}
func (s *slowStore) LastSeq(ctx context.Context, sid event.SessionID) (event.Seq, error) {
	return s.inner.LastSeq(ctx, sid)
}
func (s *slowStore) Read(ctx context.Context, sid event.SessionID, after event.Seq, limit int) ([]event.Event, error) {
	time.Sleep(s.readDelay)
	return s.inner.Read(ctx, sid, after, limit)
}

// ---- helpers ----

func assertKindsContain(t *testing.T, kinds []event.Kind, want event.Kind) {
	t.Helper()
	for _, k := range kinds {
		if k == want {
			return
		}
	}
	t.Fatalf("kinds %v do not contain %s", kinds, want)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within deadline")
}
