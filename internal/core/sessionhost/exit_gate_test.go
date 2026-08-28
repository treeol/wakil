package sessionhost

// exit_gate_test.go: P0 exit-gate certification tests (card #148, impl-plan §3).
//
// Gates covered:
//   - Gate 4 — durable Seq values are unique and strictly increasing under
//     CONCURRENT production: concurrent SubmitInput from many producers, plus
//     concurrent session-emitter appends from detached workers inside a turn.
//   - Gate 5 — two live subscribers from the same cursor observe the SAME
//     durable event order (and it equals the log order).
//   - Gate 9 (D9) — replaying the durable log (ListEvents from 0)
//     reconstructs the client-visible projection: user+assistant transcript
//     and approval terminal state, exactly as a live subscriber saw.
//
// These run against the in-memory host (P0 store). When P1 swaps in the
// SQLite-backed store these tests must keep passing unchanged — that is the
// point of the sequencer-interface decision (D3).

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
)

// durableSeqs extracts the committed Seq values of events in order.
func durableSeqs(events []event.Event) []event.Seq {
	out := make([]event.Seq, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Seq)
	}
	return out
}

// assertStrictlyIncreasing fails on a zero seq (draft leak), a duplicate, or
// an out-of-order value.
func assertStrictlyIncreasing(t *testing.T, seqs []event.Seq, label string) {
	t.Helper()
	seen := make(map[event.Seq]bool, len(seqs))
	for i, s := range seqs {
		if s == 0 {
			t.Fatalf("%s: seq[%d] is 0 (unassigned draft leaked to the committed store)", label, i)
		}
		if seen[s] {
			t.Fatalf("%s: duplicate seq %d at index %d", label, s, i)
		}
		seen[s] = true
		if i > 0 && seqs[i-1] >= s {
			t.Fatalf("%s: seq %d at index %d is <= previous %d (not strictly increasing)",
				label, s, i, seqs[i-1])
		}
	}
}

// TestExitGateConcurrentSeqUniqueAndIncreasing (gate 4): many concurrent
// SubmitInput producers plus concurrent session-emitter appends from detached
// workers; the durable log must have unique, strictly increasing seqs.
func TestExitGateConcurrentSeqUniqueAndIncreasing(t *testing.T) {
	const producers = 8
	const perProducer = 10
	const workers = 8
	const perWorker = 25

	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		if in.TurnIndex == 0 {
			var wg sync.WaitGroup
			for w := 0; w < workers; w++ {
				wg.Add(1)
				go func(w int) {
					defer wg.Done()
					for j := 0; j < perWorker; j++ {
						_ = in.SessionEmit.Emit(event.KindAsyncJobStarted, event.AsyncJobStarted{
							OpID: event.OpID(fmt.Sprintf("op_gate_%d_%d", w, j)),
						})
					}
				}(w)
			}
			wg.Wait()
		}
		return in.Text, nil
	})
	p := testPrincipal()
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted, rejected := 0, 0
	for i := 0; i < producers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < perProducer; j++ {
				_, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{
					SessionID: s.ID,
					Text:      fmt.Sprintf("msg-%d-%d", i, j),
				})
				mu.Lock()
				if err != nil {
					rejected++
				} else {
					accepted++
				}
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if accepted == 0 {
		t.Fatal("all concurrent SubmitInput calls rejected — test exercised nothing")
	}

	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})

	events, err := h.ListEvents(context.Background(), p, s.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) == 0 {
		t.Fatal("no durable events in the log")
	}
	assertStrictlyIncreasing(t, durableSeqs(events), "concurrent log")
	if rejected > 0 {
		t.Logf("note: %d SubmitInput rejected as legal backpressure; %d accepted", rejected, accepted)
	}
}

// TestExitGateTwoSubscribersSameOrder (gate 5): two subscribers from cursor 0
// see the identical durable order, equal to the log order.
func TestExitGateTwoSubscribersSameOrder(t *testing.T) {
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		for i := 0; i < 5; i++ {
			if err := in.Emit.Emit(event.KindToolCallStarted, event.ToolCallStarted{
				TurnID:     in.TurnID,
				ToolCallID: event.ToolCallID(fmt.Sprintf("tcl_gate_%d_%d", in.TurnIndex, i)),
				Name:       "gate_tool",
			}); err != nil {
				return "", err
			}
		}
		return in.Text, nil
	})
	p := testPrincipal()
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	sub1, err := h.Subscribe(context.Background(), p, s.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe 1: %v", err)
	}
	defer sub1.Close()
	sub2, err := h.Subscribe(context.Background(), p, s.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe 2: %v", err)
	}
	defer sub2.Close()

	const turns = 4
	for i := 0; i < turns; i++ {
		if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{SessionID: s.ID, Text: fmt.Sprintf("turn-%d", i)}); err != nil {
			t.Fatalf("SubmitInput %d: %v", i, err)
		}
	}
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})

	// Ground truth: the durable log.
	events, err := h.ListEvents(context.Background(), p, s.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var want []event.Event
	for _, e := range events {
		if e.Kind.Class() == event.ClassDurable {
			want = append(want, e)
		}
	}

	collect := func(sub core.EventSubscription, label string) []event.Event {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var got []event.Event
		for len(got) < len(want) {
			ev, err := sub.Next(ctx)
			if err != nil {
				t.Fatalf("%s: Next after %d events: %v (want %d)", label, len(got), err, len(want))
			}
			if ev.Kind.Class() == event.ClassDurable {
				got = append(got, ev)
			}
		}
		return got
	}
	got1 := collect(sub1, "sub1")
	got2 := collect(sub2, "sub2")

	assertStrictlyIncreasing(t, durableSeqs(got1), "sub1")
	assertStrictlyIncreasing(t, durableSeqs(got2), "sub2")
	for i := range want {
		if got1[i].Seq != want[i].Seq || got1[i].Kind != want[i].Kind {
			t.Fatalf("sub1 diverges at %d: got seq=%d kind=%s, want seq=%d kind=%s",
				i, got1[i].Seq, got1[i].Kind, want[i].Seq, want[i].Kind)
		}
		if got2[i].Seq != want[i].Seq || got2[i].Kind != want[i].Kind {
			t.Fatalf("sub2 diverges at %d: got seq=%d kind=%s, want seq=%d kind=%s",
				i, got2[i].Seq, got2[i].Kind, want[i].Seq, want[i].Kind)
		}
	}
}

// TestExitGateReplayReconstructsProjection (gate 9, D9): a fresh client
// replays ListEvents from 0 and reconstructs the same client-visible
// projection a live subscriber saw — the user+assistant transcript and the
// approval terminal state.
func TestExitGateReplayReconstructsProjection(t *testing.T) {
	h := New(func(ctx context.Context, in TurnInput) (string, error) {
		if in.TurnIndex == 1 {
			// One turn requests an approval, parks, and records the resolved
			// outcome durably once decided.
			approvalID := event.ApprovalID("apr_gate_1")
			if err := in.Emit.Emit(event.KindApprovalRequested, event.ApprovalRequested{
				ApprovalID: approvalID,
				ToolName:   "run_shell",
				Headline:   "gate test",
				Detail:     "rm -rf /tmp/gate",
			}); err != nil {
				return "", err
			}
			outcome, _, _ := in.ParkApproval(ctx, approvalID)
			if err := in.Emit.Emit(event.KindApprovalResolved, event.ApprovalResolved{
				ApprovalID: approvalID,
				Outcome:    outcome,
			}); err != nil {
				return "", err
			}
		}
		return fmt.Sprintf("answer-%d", in.TurnIndex), nil
	})
	p := testPrincipal()
	defer h.Close(context.Background())
	s := createSession(t, h, p)

	// Live subscriber from 0 — ground-truth client view.
	live, err := h.Subscribe(context.Background(), p, s.ID, 0)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer live.Close()

	// Submit 3 turns; the middle one parks on the approval — resolve it when
	// the session enters awaiting_approval.
	for i := 0; i < 3; i++ {
		if _, err := h.SubmitInput(context.Background(), p, core.SubmitInputRequest{
			SessionID: s.ID,
			Text:      fmt.Sprintf("user-%d", i),
		}); err != nil {
			t.Fatalf("SubmitInput %d: %v", i, err)
		}
		if i == 1 {
			waitFor(t, func() bool {
				g, _ := h.GetSession(context.Background(), p, s.ID)
				return g.State == core.SessionAwaitingApproval
			})
			if err := h.RespondToApproval(context.Background(), p, core.ApprovalDecision{
				SessionID:  s.ID,
				ApprovalID: event.ApprovalID("apr_gate_1"),
				Outcome:    core.ApprovalAllowOnce,
			}); err != nil {
				t.Fatalf("RespondToApproval: %v", err)
			}
		}
	}
	waitFor(t, func() bool {
		g, _ := h.GetSession(context.Background(), p, s.ID)
		return g.State == core.SessionIdle
	})

	// Fold the live subscriber's durable stream into the same projection and
	// compare — replay and live must agree exactly.
	collectLive := func() (transcript []struct{ role, text string }, approvals map[string]string) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		approvals = map[string]string{}
		for {
			ev, err := live.Next(ctx)
			if err != nil {
				if len(transcript) < 6 {
					t.Fatalf("live Next after %d events: %v (want 6 transcript entries)", len(transcript), err)
				}
				break
			}
			if ev.Kind.Class() != event.ClassDurable {
				continue
			}
			switch ev.Kind {
			case event.KindUserMessageCommitted:
				pl := ev.Payload.(event.UserMessageCommitted)
				transcript = append(transcript, struct{ role, text string }{"user", pl.Text})
			case event.KindMessageCommitted:
				pl := ev.Payload.(event.MessageCommitted)
				transcript = append(transcript, struct{ role, text string }{"assistant", pl.Text})
			case event.KindApprovalResolved:
				pl := ev.Payload.(event.ApprovalResolved)
				approvals[string(pl.ApprovalID)] = pl.Outcome
			}
		}
		return transcript, approvals
	}
	liveTranscript, liveApprovals := collectLive()

	// --- replay client: ListEvents from 0, same fold ---
	events, err := h.ListEvents(context.Background(), p, s.ID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	var replayTranscript []struct{ role, text string }
	replayApprovals := map[string]string{}
	for _, e := range events {
		switch e.Kind {
		case event.KindUserMessageCommitted:
			pl := e.Payload.(event.UserMessageCommitted)
			replayTranscript = append(replayTranscript, struct{ role, text string }{"user", pl.Text})
		case event.KindMessageCommitted:
			pl := e.Payload.(event.MessageCommitted)
			replayTranscript = append(replayTranscript, struct{ role, text string }{"assistant", pl.Text})
		case event.KindApprovalResolved:
			pl := e.Payload.(event.ApprovalResolved)
			replayApprovals[string(pl.ApprovalID)] = pl.Outcome
		}
	}

	if len(replayTranscript) != 6 {
		t.Fatalf("replay transcript length = %d, want 6 (3 user + 3 assistant): %+v", len(replayTranscript), replayTranscript)
	}
	for i := range replayTranscript {
		if replayTranscript[i] != liveTranscript[i] {
			t.Fatalf("replay diverges from live at %d: replay=%+v live=%+v", i, replayTranscript[i], liveTranscript[i])
		}
	}
	wantRole := func(i int) string {
		if i%2 == 1 {
			return "assistant"
		}
		return "user"
	}
	for i, te := range replayTranscript {
		if te.role != wantRole(i) {
			t.Fatalf("transcript[%d] role = %q, want %q", i, te.role, wantRole(i))
		}
	}
	// ParkApproval returns adapter-vocabulary outcomes ("approved"/"declined"/
	// "allowed_reads"); the adapter emits those verbatim into
	// ApprovalResolved.Outcome (hostturn.go). Assert the production truth.
	if got := replayApprovals["apr_gate_1"]; got != "approved" {
		t.Fatalf("replayed approval outcome = %q, want approved", got)
	}
	if got := liveApprovals["apr_gate_1"]; got != "approved" {
		t.Fatalf("live approval outcome = %q, want approved", got)
	}
}
