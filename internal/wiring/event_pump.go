package wiring

// event_pump.go: the event pump that drives EventSubscription.Next and posts
// events to the TUI (card #148 chunk 7b3 m3).
//
// The pump runs in its own goroutine. It reads events from the session host's
// subscription and delivers them to the TUI via a callback (the TUI's
// tea.Program.Send). It handles:
//   - subscription gap recovery (resubscribe from the last durable cursor)
//   - pump cancellation (stop on rotation/close)
//   - rotation drain (stop the old pump before starting a new one)
//
// The pump is started by the facade (or ConversationManager) after Subscribe.
// It runs until the subscription is closed, the context is cancelled, or
// Stop is called.

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionhost"
)

// EventPump drives a session host subscription and delivers events to a
// callback. It is safe for concurrent use: Stop can be called from any
// goroutine.
type EventPump struct {
	mu     sync.Mutex
	sub    core.EventSubscription
	host   *sessionhost.Host
	principal core.Principal
	sessionID event.SessionID
	deliver func(event.Event) // the TUI's send function
	lastSeq event.Seq         // durable cursor for gap recovery

	stopped atomic.Bool
	done    chan struct{}
}

// NewEventPump creates an event pump for the given subscription. The deliver
// callback is called for each event (durable and ephemeral). It must be
// goroutine-safe (typically tea.Program.Send).
//
// The host, principal, and sessionID are needed for gap recovery: if the
// subscription falls behind (ErrSubscriptionGap), the pump resubscribes from
// lastSeq and continues.
//
// initialSeq is the durable cursor the subscription was created with (the
// `after` parameter passed to Subscribe). The pump uses it as the starting
// point for gap recovery — without it, a gap before any event is delivered
// would cause a resubscribe from zero, replaying the entire history.
func NewEventPump(sub core.EventSubscription, host *sessionhost.Host, principal core.Principal, sessionID event.SessionID, initialSeq event.Seq, deliver func(event.Event)) *EventPump {
	return &EventPump{
		sub:       sub,
		host:      host,
		principal: principal,
		sessionID: sessionID,
		deliver:   deliver,
		lastSeq:   initialSeq,
		done:      make(chan struct{}),
	}
}

// Run starts the pump loop. It blocks until the subscription is closed, the
// context is cancelled, or Stop is called. It should be called in its own
// goroutine.
func (p *EventPump) Run(ctx context.Context) {
	defer close(p.done)
	for {
		if p.stopped.Load() {
			return
		}
		ev, err := p.sub.Next(ctx)
		if err != nil {
			if p.stopped.Load() {
				return
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return
			}
			// Subscription gap: resubscribe from lastSeq.
			if errors.Is(err, sessionhost.ErrSubscriptionGap) {
				newSub, subErr := p.host.Subscribe(ctx, p.principal, p.sessionID, p.lastSeq)
				if subErr != nil {
					return // can't recover; stop the pump
				}
				p.mu.Lock()
				oldSub := p.sub
				p.sub = newSub
				p.mu.Unlock()
				_ = oldSub.Close()
				continue
			}
			// io.EOF or other terminal error: subscription closed.
			if errors.Is(err, io.EOF) {
				return
			}
			// Unknown error: stop the pump rather than spinning.
			return
		}

		// Advance the durable cursor for gap recovery.
		if ev.Kind.Class() == event.ClassDurable {
			p.lastSeq = ev.Seq
		}

		// Deliver the event to the TUI.
		p.deliver(ev)
	}
}

// Stop signals the pump to stop and closes the subscription. It is idempotent
// and safe to call from any goroutine. It does NOT block — the pump's Run
// goroutine exits on its own after the subscription is closed.
func (p *EventPump) Stop() {
	if !p.stopped.CompareAndSwap(false, true) {
		return
	}
	p.mu.Lock()
	sub := p.sub
	p.mu.Unlock()
	if sub != nil {
		_ = sub.Close()
	}
}

// Done returns a channel that is closed when the pump has fully stopped.
// Useful for rotation drain: wait for Done before starting a new pump.
func (p *EventPump) Done() <-chan struct{} { return p.done }
