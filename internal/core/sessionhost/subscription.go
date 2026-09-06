package sessionhost

// subscription.go: live event subscription machinery (replay + live delivery).

import (
	"context"
	"io"
	"sync"
	"sync/atomic"

	"github.com/treeol/wakil/internal/core/event"
)

// ---- subscription ----

type subPhase uint8

const (
	phaseReplaying subPhase = iota
	phaseLive
)

type subscription struct {
	mu    sync.Mutex
	phase subPhase

	// pending is the staged, seq-ordered, deduplicated durable replay queue. It
	// is drained by Next (single consumer) before any live event is delivered,
	// and is unbounded — replay history is never lost to a bounded buffer.
	pending []event.Event
	// holdLive collects durable live events that arrive while phase ==
	// replaying, so they can be merged after the replayed history and never
	// reorder past it.
	holdLive []event.Event

	in        chan event.Event
	done      chan struct{}
	closeOnce sync.Once
	lastSeq   event.Seq // durable cursor; advanced by Next (single consumer)
	dropped   atomic.Bool
	detach    func()
}

func newSubscription(liveCapacity int, after event.Seq) *subscription {
	return &subscription{
		phase:   phaseReplaying,
		in:      make(chan event.Event, liveCapacity),
		done:    make(chan struct{}),
		lastSeq: after,
	}
}

// completeReplay merges the replayed durable history with any live durable
// events held during replay, in sequence order, and switches to live delivery.
func (s *subscription) completeReplay(replayed []event.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	held := s.holdLive
	s.holdLive = nil
	s.pending = mergeBySeq(replayed, held)
	s.phase = phaseLive
}

// push delivers one event without blocking. During replay it holds durable
// events for merge; in live phase it enqueues. A full live buffer on a durable
// event disconnects the subscription (gap error) rather than silently losing a
// durable event; ephemeral events are dropped without disconnecting.
func (s *subscription) push(ev event.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase == phaseReplaying {
		if ev.Kind.Class() == event.ClassDurable {
			s.holdLive = append(s.holdLive, ev)
		}
		return
	}
	select {
	case <-s.done:
		return
	default:
	}
	if ev.Kind.Class() == event.ClassDurable {
		select {
		case s.in <- ev:
		default:
			s.dropped.Store(true)
			s.disconnectLocked()
		}
		return
	}
	// Ephemeral: may be dropped silently (D2); never disconnect.
	select {
	case s.in <- ev:
	default:
	}
}

// pushEphemeral delivers an ephemeral notification live-only. It is never held
// for replay (ephemeral events are not durable) and never disconnects the
// subscription on a full buffer — it drops (D2).
func (s *subscription) pushEphemeral(ev event.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase == phaseReplaying {
		return // ephemeral during replay: dropped (D2)
	}
	select {
	case <-s.done:
		return
	default:
	}
	select {
	case s.in <- ev:
	default:
		// dropped silently (D2)
	}
}

// disconnectLocked closes the subscription and detaches it from the session.
// Caller must hold s.mu.
func (s *subscription) disconnectLocked() {
	s.closeOnce.Do(func() {
		close(s.done)
		if s.detach != nil {
			s.detach()
		}
	})
}

// takePending drains one staged replay event, if any.
func (s *subscription) takePending() (event.Event, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return event.Event{}, false
	}
	ev := s.pending[0]
	s.pending = s.pending[1:]
	s.lastSeq = ev.Seq
	return ev, true
}

// acceptLive dedups durable events by seq (the replay/live handoff guarantees
// order, this is belt-and-suspenders) and reports whether the event should be
// delivered.
func (s *subscription) acceptLive(ev event.Event) bool {
	if ev.Kind.Class() != event.ClassDurable {
		return true
	}
	if ev.Seq <= s.lastSeq {
		return false
	}
	s.lastSeq = ev.Seq
	return true
}

// Next returns the next event in order: staged replay first, then live. It
// blocks until an event is available, the subscription is closed/disconnected
// (io.EOF or ErrSubscriptionGap), or ctx is cancelled. Single-consumer only.
//
// Once the subscription is closed, Next returns the terminal error and never
// drains further buffered events: Close is a hard stop (checked before every
// delivery), so a sequential Close-then-Next deterministically returns the
// terminal error. An event already in flight when Close races Next may still be
// delivered once — the guarantee is that a closed subscription's Next unblocks.
func (s *subscription) Next(ctx context.Context) (event.Event, error) {
	for {
		if err, done := s.terminalErrIfClosed(); done {
			return event.Event{}, err
		}
		if ev, ok := s.takePending(); ok {
			if err, done := s.terminalErrIfClosed(); done {
				return event.Event{}, err
			}
			return ev, nil
		}
		select {
		case ev := <-s.in:
			if !s.acceptLive(ev) {
				continue
			}
			return ev, nil
		case <-s.done:
			return event.Event{}, s.terminalErr()
		case <-ctx.Done():
			return event.Event{}, ctx.Err()
		}
	}
}

// terminalErrIfClosed reports whether the subscription is closed, returning the
// appropriate terminal error (io.EOF for clean Close, ErrSubscriptionGap for a
// disconnect). Non-blocking.
func (s *subscription) terminalErrIfClosed() (error, bool) {
	select {
	case <-s.done:
		return s.terminalErr(), true
	default:
		return nil, false
	}
}

// terminalErr returns io.EOF for a clean Close and ErrSubscriptionGap if the
// subscription was disconnected for lagging on durable events.
func (s *subscription) terminalErr() error {
	if s.dropped.Load() {
		return ErrSubscriptionGap
	}
	return io.EOF
}

// Close releases the subscription and detaches it from the session. Idempotent;
// unblocks Next.
func (s *subscription) Close() error {
	s.mu.Lock()
	s.disconnectLocked()
	s.mu.Unlock()
	return nil
}

// mergeBySeq concatenates two seq-ordered durable slices, deduplicating by seq.
// replayed is [after+1..H]; held is [H+1..H+k]. Both ascending.
func mergeBySeq(a, b []event.Event) []event.Event {
	out := make([]event.Event, 0, len(a)+len(b))
	add := func(e event.Event) {
		if len(out) == 0 || e.Seq > out[len(out)-1].Seq {
			out = append(out, e)
		}
	}
	for _, e := range a {
		add(e)
	}
	for _, e := range b {
		add(e)
	}
	return out
}