package sessionhost

// emit.go: event emission and delivery — durable append + live fan-out.

import (
	"context"
	"fmt"
	"sync/atomic"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
)

// emitDraft validates, appends, and delivers one durable event. It is the host's
// single commit→notify point. It must NOT be called while holding s.mu (lock
// order is store.mu then subsMu, never nested under s.mu).
//
// In P0 an append failure can only be an invalid draft — a programmer error. The
// host panics rather than silently dropping a durable event; P1's store-backed
// append returns errors the host turns into SessionError.
func (h *Host) emitDraft(s *session, kind event.Kind, payload any) {
	draft := event.Event{
		TenantID:  s.tenant,
		SessionID: s.sid,
		Ts:        h.now(),
		Kind:      kind,
		Payload:   payload,
	}

	// Serialize append→notify across ALL producers (executor's own terminals AND
	// concurrent turn-emitted events) so durable delivery order equals Seq order.
	s.emitMu.Lock()
	committed, err := h.store.Append(context.Background(), draft)
	if err == nil {
		h.notify(s, committed)
	}
	s.emitMu.Unlock()

	if err != nil {
		panic(fmt.Sprintf("sessionhost: durable append %s failed: %v", kind, err))
	}
}

// notify delivers a committed durable event to every live subscriber. It is
// non-blocking: a lagging subscriber is disconnected (never blocks the executor).
// Caller must hold s.emitMu (durable ordering) — the notify fan-out below is what
// that lock protects.
func (h *Host) notify(s *session, ev event.Event) {
	h.deliver(s, ev, ev.Kind.Class() == event.ClassDurable)
}

// notifyEphemeral delivers one ephemeral notification to live subscribers,
// dropping it when the live buffer is full (never disconnecting). Ephemeral
// events are not ordered against each other or against durable events and do
// not require emitMu.
func (h *Host) notifyEphemeral(s *session, ev event.Event) {
	h.deliver(s, ev, false)
}

// deliver fans an event out to every live subscriber. durable=true uses push's
// durable path (buffer-full → disconnect/gap); durable=false uses the ephemeral
// path (buffer-full → drop).
func (h *Host) deliver(s *session, ev event.Event, durable bool) {
	s.subsMu.Lock()
	subs := make([]*subscription, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.subsMu.Unlock()
	for _, sub := range subs {
		if durable {
			sub.push(ev)
		} else {
			sub.pushEphemeral(ev)
		}
	}
}

// ---- hostEmitter: the turn-scoped Emitter implementation ----

// hostEmitter implements Emitter, funneling turn-emitted events into the
// session's durable log and live subscribers. It is safe for concurrent use:
// durable Emit is serialized under s.emitMu; Notify is lock-free-ish (subscriber
// fan-out only). It is fenced (closed) at turn finalization.
type hostEmitter struct {
	s      *session
	host   *Host
	closed atomic.Bool
}

func newEmitter(h *Host, s *session) *hostEmitter { return &hostEmitter{s: s, host: h} }

// close fences the emitter: subsequent Emit returns ErrEmitterClosed and Notify
// drops. Idempotent.
func (e *hostEmitter) close() { e.closed.Store(true) }

func (e *hostEmitter) Emit(kind event.Kind, payload any) error {
	if kind.Class() == event.ClassEphemeral {
		return fmt.Errorf("%w: %s is ephemeral (durable Emit only)", core.ErrInvalidInput, kind)
	}
	if hostReservedKinds[kind] {
		return fmt.Errorf("%w: %s is host-owned", core.ErrInvalidInput, kind)
	}
	draft := event.Event{
		TenantID:  e.s.tenant,
		SessionID: e.s.sid,
		Ts:        e.host.now(),
		Kind:      kind,
		Payload:   payload,
	}

	// The fence check and the append are atomic with the host's terminal
	// emission (finishTurn's emitDraft holds the same lock), so a turn-emitted
	// durable event can never be appended AFTER its turn's TurnCompleted: it
	// either appends before the terminal event, or is rejected. finishTurn's
	// em.close() is a lock-free atomic flag — the lock is what linearizes the
	// append against the terminal event, not the flag read per se.
	e.s.emitMu.Lock()
	defer e.s.emitMu.Unlock()
	if e.closed.Load() {
		return ErrEmitterClosed
	}
	committed, err := e.host.store.Append(context.Background(), draft)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrEmitFailed, err)
	}
	e.host.notify(e.s, committed)
	return nil
}

func (e *hostEmitter) Notify(kind event.Kind, payload any) {
	if e.closed.Load() {
		return
	}
	if kind.Class() != event.ClassEphemeral {
		return // durable kinds are not notifications; dropped by contract
	}
	ev := event.Event{
		TenantID:  e.s.tenant,
		SessionID: e.s.sid,
		Ts:        e.host.now(),
		Kind:      kind,
		Payload:   payload,
	}
	if err := ev.ValidateCommitted(); err != nil {
		return // invalid ephemeral notification: drop (best-effort by contract)
	}
	e.host.notifyEphemeral(e.s, ev)
}

// ---- sessionEmitter: the session-scoped SessionEmitter implementation (7b2) ----

// sessionEmitter implements SessionEmitter. It shares the same append→notify
// path as hostEmitter but is fenced at session close (not turn close). A
// session-scoped emitter is created once per session (in newSession) and
// passed into every TurnInput.SessionEmit.
type sessionEmitter struct {
	s    *session
	host *Host
	// closed is set when the session closes (finalizeClose). After that,
	// Emit returns ErrEmitterClosed and Notify drops.
	closed atomic.Bool
}

func newSessionEmitter(h *Host, s *session) *sessionEmitter {
	return &sessionEmitter{s: s, host: h}
}

// closeSessionEmitter fences the emitter at session close. Idempotent.
func (e *sessionEmitter) close() { e.closed.Store(true) }

func (e *sessionEmitter) Emit(kind event.Kind, payload any) error {
	if kind.Class() == event.ClassEphemeral {
		return fmt.Errorf("%w: %s is ephemeral (durable Emit only)", core.ErrInvalidInput, kind)
	}
	if hostReservedKinds[kind] {
		return fmt.Errorf("%w: %s is host-owned", core.ErrInvalidInput, kind)
	}
	if turnScopedKinds[kind] {
		return fmt.Errorf("%w: %s is turn-scoped (use the turn Emitter, not SessionEmitter)", core.ErrInvalidInput, kind)
	}
	draft := event.Event{
		TenantID:  e.s.tenant,
		SessionID: e.s.sid,
		Ts:        e.host.now(),
		Kind:      kind,
		Payload:   payload,
	}
	e.s.emitMu.Lock()
	defer e.s.emitMu.Unlock()
	if e.closed.Load() {
		return ErrEmitterClosed
	}
	committed, err := e.host.store.Append(context.Background(), draft)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrEmitFailed, err)
	}
	e.host.notify(e.s, committed)
	return nil
}

func (e *sessionEmitter) Notify(kind event.Kind, payload any) {
	if e.closed.Load() {
		return
	}
	if kind.Class() != event.ClassEphemeral {
		return
	}
	ev := event.Event{
		TenantID:  e.s.tenant,
		SessionID: e.s.sid,
		Ts:        e.host.now(),
		Kind:      kind,
		Payload:   payload,
	}
	if err := ev.ValidateCommitted(); err != nil {
		return
	}
	e.host.notifyEphemeral(e.s, ev)
}