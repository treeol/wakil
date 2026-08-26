// pump.go: the remote event pump (card #148 P2e).
//
// The remote pump consumes the daemon's StreamEvents server-stream RPC and
// delivers domain events to the TUI via a callback (tea.Program.Send). It
// mirrors the embedded path's EventPump (internal/wiring/event_pump.go) but
// reads from a Connect server-stream instead of an in-process subscription.
//
// Key differences from the embedded pump:
//   - No subscription handle to Close: the server-stream is cancelled by
//     context cancellation. Stop cancels the stream's context.
//   - Reconnect: if the stream breaks (network error, daemon restart), the
//     pump reconnects from the last durable seq + 1 and continues. The
//     embedded path's gap recovery is a resubscribe; here it's a new RPC.
//   - Dedup by seq: the daemon may replay the boundary event (per the proto
//     contract), so the client deduplicates durable events by seq. Ephemeral
//     events (seq 0) are never deduplicated — they are live-only.
//
// The pump runs in its own goroutine. It is started by the RemoteFacade after
// Subscribe. It runs until Stop is called or the context is cancelled.
package remote

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	v1alpha1 "github.com/treeol/wakil/api/gen/wakil/v1alpha1"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/protoconv"
)

// RemoteEventPump consumes the daemon's StreamEvents RPC and delivers domain
// events to a callback. It is safe for concurrent use: Stop can be called
// from any goroutine.
type RemoteEventPump struct {
	mu      sync.Mutex
	clients *Clients
	sid     event.SessionID
	deliver func(event.Event) // the TUI's send function
	lastSeq event.Seq         // durable cursor for reconnect + dedup
	stopped atomic.Bool
	cancel  context.CancelFunc // cancels the active stream's context
	done    chan struct{}
}

// NewRemoteEventPump creates a pump for the given session. The deliver callback
// is called for each event (durable and ephemeral). It must be goroutine-safe
// (typically tea.Program.Send).
//
// initialSeq is the durable cursor the subscription starts from (the `after_seq`
// passed to StreamEvents). The pump uses lastSeq+1 for reconnects.
func NewRemoteEventPump(clients *Clients, sid event.SessionID, initialSeq event.Seq, deliver func(event.Event)) *RemoteEventPump {
	return &RemoteEventPump{
		clients: clients,
		sid:     sid,
		deliver: deliver,
		lastSeq: initialSeq,
		done:    make(chan struct{}),
	}
}

// Run starts the pump loop. It blocks until the context is cancelled or Stop
// is called. It should be called in its own goroutine.
func (p *RemoteEventPump) Run(ctx context.Context) {
	defer close(p.done)

	// The stream context is separate from the parent context so Stop can
	// cancel just the stream without cancelling the parent. The parent
	// context still governs overall lifetime.
	streamCtx, streamCancel := context.WithCancel(ctx)
	p.mu.Lock()
	p.cancel = streamCancel
	p.mu.Unlock()
	defer streamCancel()

	for {
		if p.stopped.Load() {
			return
		}

		// Start a new StreamEvents RPC from lastSeq + 1.
		after := p.lastSeq
		stream, err := p.clients.Event.StreamEvents(streamCtx, connect.NewRequest(&v1alpha1.StreamEventsRequest{
			SessionId: string(p.sid),
			AfterSeq:  uint64(after),
		}))
		if err != nil {
			if p.stopped.Load() || isCtxErr(ctx) {
				return
			}
			// Transient error: wait briefly and retry.
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}

		// Consume events from the stream.
		for {
			if p.stopped.Load() {
				return
			}
			hasMsg := stream.Receive()
			if err := stream.Err(); err != nil {
				if p.stopped.Load() {
					return
				}
				// io.EOF: the daemon closed the stream (session closed or
				// daemon shutting down). Check if the stream context was
				// cancelled (Stop or parent cancel).
				if isCtxErr(streamCtx) || isCtxErr(ctx) {
					return
				}
				if errors.Is(err, io.EOF) {
					// Graceful stream end. The session may have been closed.
					// If the session is still alive, reconnect; if not, the
					// next StreamEvents will fail with a session-not-found
					// error and the pump exits.
					break
				}
				// Any other error: reconnect from lastSeq.
				break
			}
			if !hasMsg {
				// Receive returned false with no error — stream ended.
				break
			}

			pb := stream.Msg()
			if pb == nil {
				continue
			}

			// Convert proto event to domain event.
			ev, err := eventFromProto(pb)
			if err != nil {
				// Unknown event kind: skip. The daemon may send a new event
				// kind the client doesn't know yet — don't crash the pump.
				continue
			}

			// Dedup by seq for durable events. Ephemeral events (seq 0)
			// pass through without dedup.
			if ev.Seq > 0 {
				if ev.Seq <= p.lastSeq {
					continue // already delivered
				}
				p.mu.Lock()
				if ev.Seq > p.lastSeq {
					p.lastSeq = ev.Seq
				}
				p.mu.Unlock()
			}

			// Deliver the event to the TUI.
			p.deliver(ev)
		}

		// Stream ended. If we're not stopped, reconnect after a brief delay.
		if p.stopped.Load() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
		// Re-create the stream context for the next attempt (the old one
		// was consumed by the cancelled/ended stream).
		streamCtx, streamCancel = context.WithCancel(ctx)
		p.mu.Lock()
		p.cancel = streamCancel
		p.mu.Unlock()
	}
}

// Stop signals the pump to stop. It cancels the active stream's context and
// sets the stopped flag. It does NOT block — the pump's Run goroutine exits
// on its own after the stream context is cancelled.
func (p *RemoteEventPump) Stop() {
	if !p.stopped.CompareAndSwap(false, true) {
		return
	}
	p.mu.Lock()
	cancel := p.cancel
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// Done returns a channel that is closed when the pump has fully stopped.
// Useful for rotation drain: wait for Done before starting a new pump.
func (p *RemoteEventPump) Done() <-chan struct{} { return p.done }

// LastSeq returns the current durable cursor. Safe to call from any goroutine.
func (p *RemoteEventPump) LastSeq() event.Seq {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastSeq
}

// eventFromProto converts a proto Event to a domain event via the shared
// protoconv package (card #148 P2e). The converter handles the 32-kind oneof
// switch; we don't duplicate it here.
func eventFromProto(pb *v1alpha1.Event) (event.Event, error) {
	return protoconv.EventFromProto(pb)
}

func isCtxErr(ctx context.Context) bool {
	return ctx.Err() != nil
}
