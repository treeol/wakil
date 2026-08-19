// Package sessionhost is the P0 in-memory session host (card #148, plan §2
// deliverable 3). It implements the three service interfaces from
// internal/core — SessionService, EventReader, SessionReader — over an
// in-memory EventAppender/EventLog.
//
// The core design decisions, all taken from docs/design/wakild-foundation.md and
// the Mashura-gated plan:
//
//   - One executor goroutine per session with a bounded input queue (§5.1). All
//     inputs are serialized through it — there is no locking inside a turn.
//   - SubmitInput is genuinely non-blocking (§3.4, plan D1): it enqueues and
//     returns a TurnAck immediately. Everything after that — TurnStarted,
//     TurnCompleted, SessionError, SessionClosed — is delivered as events, never
//     as this method's return value.
//   - The executor goroutine is the single commit→notify AND state-transition
//     point. requestClose/Interrupt only set flags and cancel; every durable
//     event (including SessionClosed) is emitted by the executor. This keeps the
//     single-producer invariant for the in-memory log true by construction.
//   - Interrupt/CloseSession cancel the turn through an internal context and
//     emit TurnCompleted{cancelled} / SessionClosed — never a silent abort
//     (§5.5).
//   - Turn finalization is one lock-protected linearization point (finishTurn):
//     the decision "natural completion vs interrupt vs close vs backend error"
//     is resolved atomically under session.mu, so a close or interrupt racing a
//     turn's return can never be lost (§5.5, §5.6).
//   - Crash recovery is a stub (§5.7): RecoverRunning re-creates caller-supplied
//     sessions (that a prior incarnation left in `running`) in `error` state and
//     appends a SessionError{daemon_restart} event. There is no durable store in
//     P0, so the list of "was running" sessions comes from the caller; P1 reads
//     it from SQLite.

// Known P0 seams (documented, not hidden):
//
//   - An acked-but-never-run input is ALWAYS observable: it is either executed
//     (TurnStarted → TurnCompleted) or explicitly abandoned with a durable
//     TurnCompleted{outcome:"cancelled"} carrying its turn_id (on session error
//     or close). TurnAck guarantees acceptance, not execution (see core.TurnAck);
//     abandonment is never silent.
//   - A slow subscriber is disconnected with ErrSubscriptionGap once it falls
//     behind on durable events — the executor is never blocked, and durable
//     events are never silently lost (exit gate #6). Ephemeral events may be
//     dropped silently (D2). Replay (the segment before live delivery) is NOT
//     bounded: it is staged in sequence order and drained by Next, so a history
//     larger than the live buffer is never lost.
//   - Commit→notify is synchronous on the executor goroutine (each committed
//     event is delivered before the next is produced). Offloading to a per-
//     session fan-out goroutine to shrink turn latency is a P1 refinement.
//   - Session state and the durable event log are not transactionally atomic
//     with each other in P0 (separate locks). Residual windows (e.g. observing
//     state=closed before the SessionClosed event is observable via ListEvents)
//     are a P1 concern — the plan's D3 makes append+last_seq one SQLite
//     transaction there. SessionSnapshot already derives LastSeq from the events
//     it returns, so its own bundle is internally consistent.
//   - ReadAction is accepted and carried into TurnInput, but its semantics
//     (relaxed approval gates) are not enforced: read-only turn handling lands
//     with the agent integration.
package sessionhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/id"
)

// Compile-time proof that Host satisfies every service interface.
var (
	_ core.SessionService = (*Host)(nil)
	_ core.EventReader    = (*Host)(nil)
	_ core.SessionReader  = (*Host)(nil)
)

// Sentinel errors specific to the host (not part of the core contract surface).
var (
	// ErrHostClosed is returned by CreateSession/RecoverRunning after Close.
	ErrHostClosed = errors.New("sessionhost: host closed")
	// ErrSubscriptionGap is returned by EventSubscription.Next when a live
	// subscription fell behind on durable events and was disconnected. The
	// caller must resubscribe from its last durable cursor (LastSeq of a
	// SessionSnapshot, or ListEvents).
	ErrSubscriptionGap = errors.New("sessionhost: subscription lagged; resubscribe from saved cursor")
)

// TurnFunc executes one turn for a session and returns the produced message
// text. It is the seam where, in later chunks, the real agent loop is plugged
// in: chunks 5–7 replace this flat callback with the Executor that emits tool-
// call/approval/subagent events and the message projection.
//
// The ctx is cancelled by Interrupt and CloseSession; a TurnFunc must return
// promptly once ctx is done. Returning a non-nil error while ctx is NOT
// cancelled signals a backend failure and moves the session to `error`
// (SessionError{backend_failure}); returning an error while ctx IS cancelled
// is treated as cancellation.
type TurnFunc func(ctx context.Context, input TurnInput) (string, error)

// TurnInput is what the executor hands to a TurnFunc for one turn.
type TurnInput struct {
	SessionID  event.SessionID
	TurnID     event.TurnID
	TurnIndex  uint64
	Text       string
	ReadAction bool
}

// Options configure the host. Use the functional Option values below; the zero
// value is not meaningful on its own (see defaultOptions).
type Options struct {
	AgentName  string
	QueueDepth int
	SubBuffer  int
	Now        func() time.Time
	IDs        *id.Generator
	Store      Store
}

func defaultOptions() Options {
	return Options{
		AgentName:  "wakil",
		QueueDepth: 64,
		SubBuffer:  256,
		Now:        time.Now,
		IDs:        id.New(),
		Store:      NewMemLog(),
	}
}

// Option mutates Options.
type Option func(*Options)

// WithAgentName sets the agent identity recorded in SessionCreated events.
func WithAgentName(name string) Option { return func(o *Options) { o.AgentName = name } }

// WithQueueDepth sets the maximum number of accepted-but-unfinished inputs per
// session (the running turn plus any queued). SubmitInput returns ErrSessionBusy
// once that many inputs are outstanding.
func WithQueueDepth(n int) Option { return func(o *Options) { o.QueueDepth = n } }

// WithSubBuffer sets the per-subscription live-delivery buffer, in events. It
// does NOT bound replay: replayed history is staged separately.
func WithSubBuffer(n int) Option { return func(o *Options) { o.SubBuffer = n } }

// WithNow injects the clock (tests).
func WithNow(f func() time.Time) Option { return func(o *Options) { o.Now = f } }

// WithIDGenerator injects the id source (tests; deterministic via id.NewFromReader).
func WithIDGenerator(g *id.Generator) Option { return func(o *Options) { o.IDs = g } }

// WithStore injects the backing store (tests).
func WithStore(s Store) Option { return func(o *Options) { o.Store = s } }

// Host is the in-memory session host. It implements all three core service
// interfaces. A Host owns one in-process executor goroutine per live session;
// Close terminates them.
type Host struct {
	mu       sync.Mutex
	sessions map[event.SessionID]*session
	closed   atomic.Bool

	store      Store
	ids        *id.Generator
	now        func() time.Time
	turn       TurnFunc
	agentName  string
	queueDepth int
	subBuffer  int
}

// New returns a Host that runs turn as each session's executor. turn may be nil
// for a host that only records lifecycle (tests that exercise lifecycle only);
// a nil turn panics at use, so always pass one in production wiring.
func New(turn TurnFunc, opts ...Option) *Host {
	o := defaultOptions()
	for _, apply := range opts {
		apply(&o)
	}
	if o.QueueDepth <= 0 {
		o.QueueDepth = 64
	}
	if o.SubBuffer <= 0 {
		o.SubBuffer = 256
	}
	if o.Store == nil {
		o.Store = NewMemLog()
	}
	if o.IDs == nil {
		o.IDs = id.New()
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Host{
		sessions:   make(map[event.SessionID]*session),
		store:      o.Store,
		ids:        o.IDs,
		now:        o.Now,
		turn:       turn,
		agentName:  o.AgentName,
		queueDepth: o.QueueDepth,
		subBuffer:  o.SubBuffer,
	}
}

// ---- SessionService ----

func (h *Host) CreateSession(ctx context.Context, principal core.Principal, req core.CreateSessionRequest) (core.Session, error) {
	if err := ctx.Err(); err != nil {
		return core.Session{}, err
	}
	if h.closed.Load() {
		return core.Session{}, ErrHostClosed
	}
	if err := principal.Validate(); err != nil {
		return core.Session{}, err
	}
	if !writerAllowed(principal.Role) {
		return core.Session{}, core.ErrNotAuthorized
	}
	if err := req.Workspace.Validate(); err != nil {
		return core.Session{}, core.ErrInvalidInput
	}
	sid, err := h.ids.SessionID()
	if err != nil {
		return core.Session{}, err
	}
	s := h.newSession(sid, principal.TenantID, req.Workspace, req.Title, principal.UserID, h.now())
	// Append the creation event BEFORE registration so a session is never
	// visible with an empty log, and the executor never runs before its own
	// creation marker exists (single-producer invariant).
	h.emitDraft(s, event.KindSessionCreated, event.SessionCreated{
		WorkspaceID: req.Workspace,
		AgentName:   h.agentName,
		CreatedBy:   principal.UserID,
	})
	h.register(s)
	return h.snapshot(s), nil
}

func (h *Host) SubmitInput(ctx context.Context, principal core.Principal, req core.SubmitInputRequest) (core.TurnAck, error) {
	if err := ctx.Err(); err != nil {
		return core.TurnAck{}, err
	}
	if err := principal.Validate(); err != nil {
		return core.TurnAck{}, err
	}
	if err := req.SessionID.Validate(); err != nil {
		return core.TurnAck{}, core.ErrInvalidInput
	}
	if req.Text == "" {
		return core.TurnAck{}, core.ErrInvalidInput
	}
	s, err := h.lookup(principal, req.SessionID)
	if err != nil {
		return core.TurnAck{}, err
	}
	if !writerAllowed(principal.Role) {
		return core.TurnAck{}, core.ErrNotAuthorized
	}
	turnID, err := h.ids.TurnID()
	if err != nil {
		return core.TurnAck{}, err
	}

	s.mu.Lock()
	switch {
	case s.state == core.SessionClosed || s.closing != "":
		s.mu.Unlock()
		return core.TurnAck{}, core.ErrSessionClosed
	case s.pending >= h.queueDepth:
		s.mu.Unlock()
		return core.TurnAck{}, core.ErrSessionBusy
	case s.state == core.SessionIdle || s.state == core.SessionError:
		// idle → running (first input) or error → running (re-drive).
		s.setStateLocked(core.SessionRunning)
	}
	s.pending++
	s.queue = append(s.queue, inputEnvelope{turnID: turnID, text: req.Text, readAction: req.ReadAction})
	s.mu.Unlock()

	s.signalKick()
	return core.TurnAck{SessionID: req.SessionID, TurnID: turnID}, nil
}

func (h *Host) RespondToApproval(ctx context.Context, principal core.Principal, d core.ApprovalDecision) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := principal.Validate(); err != nil {
		return err
	}
	if err := d.Validate(); err != nil {
		return core.ErrInvalidInput
	}
	s, err := h.lookup(principal, d.SessionID)
	if err != nil {
		return err
	}
	if !writerAllowed(principal.Role) {
		return core.ErrNotAuthorized
	}
	// D5 shim: in P0 the synchronous Confirmer resolves approvals before a
	// client can even observe ApprovalRequested, so there is never a pending
	// approval to answer from outside. The host records no approval state; the
	// authoritative async wire path lands in P2. Honest placeholder: the
	// session's tenant was checked (no existence leak), and the answer is "no
	// such pending approval".
	_ = s
	return core.ErrApprovalNotFound
}

func (h *Host) Interrupt(ctx context.Context, principal core.Principal, sessionID event.SessionID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := principal.Validate(); err != nil {
		return err
	}
	if err := sessionID.Validate(); err != nil {
		return core.ErrInvalidInput
	}
	s, err := h.lookup(principal, sessionID)
	if err != nil {
		return err
	}
	if !writerAllowed(principal.Role) {
		return core.ErrNotAuthorized
	}

	// Set interrupted under mu, and cancel the in-flight turn's context, all
	// gated on the session still having work in flight. finishTurn's
	// linearization point can then never miss an interrupt that races a turn's
	// return, and an interrupt on a session with nothing in flight is a clear
	// invalid-state result rather than a flag that leaks into the next turn.
	s.mu.Lock()
	switch s.state {
	case core.SessionClosed:
		s.mu.Unlock()
		return core.ErrSessionClosed
	case core.SessionRunning, core.SessionAwaitingApproval:
		s.interrupted = true
		cur := s.current
		s.mu.Unlock()
		if cur != nil {
			cur.cancel()
		}
		return nil
	default: // idle or error: nothing in flight to cancel
		s.mu.Unlock()
		return core.ErrInvalidStateTransition
	}
}

func (h *Host) CloseSession(ctx context.Context, principal core.Principal, sessionID event.SessionID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := principal.Validate(); err != nil {
		return err
	}
	if err := sessionID.Validate(); err != nil {
		return core.ErrInvalidInput
	}
	s, err := h.lookup(principal, sessionID)
	if err != nil {
		return err
	}
	if !writerAllowed(principal.Role) {
		return core.ErrNotAuthorized
	}
	h.requestClose(s, "closed")
	return nil
}

// ---- SessionReader ----

func (h *Host) GetSession(ctx context.Context, principal core.Principal, sessionID event.SessionID) (core.Session, error) {
	if err := ctx.Err(); err != nil {
		return core.Session{}, err
	}
	if err := principal.Validate(); err != nil {
		return core.Session{}, err
	}
	if err := sessionID.Validate(); err != nil {
		return core.Session{}, core.ErrInvalidInput
	}
	s, err := h.lookup(principal, sessionID)
	if err != nil {
		return core.Session{}, err
	}
	return h.snapshot(s), nil
}

func (h *Host) ListSessions(ctx context.Context, principal core.Principal) ([]core.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := principal.Validate(); err != nil {
		return nil, err
	}
	h.mu.Lock()
	sessions := make([]*session, 0, len(h.sessions))
	for _, s := range h.sessions {
		if s.tenant == principal.TenantID {
			sessions = append(sessions, s)
		}
	}
	h.mu.Unlock()

	out := make([]core.Session, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, h.snapshot(s))
	}
	return out, nil
}

func (h *Host) SessionSnapshot(ctx context.Context, principal core.Principal, sessionID event.SessionID) (core.SessionSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return core.SessionSnapshot{}, err
	}
	if err := principal.Validate(); err != nil {
		return core.SessionSnapshot{}, err
	}
	if err := sessionID.Validate(); err != nil {
		return core.SessionSnapshot{}, core.ErrInvalidInput
	}
	s, err := h.lookup(principal, sessionID)
	if err != nil {
		return core.SessionSnapshot{}, err
	}
	events, err := h.store.Read(ctx, sessionID, 0, 0)
	if err != nil {
		return core.SessionSnapshot{}, err
	}
	// Derive LastSeq from the events we actually return, so the bundle is
	// internally consistent (Events ⇔ LastSeq) even under concurrent appends.
	lastSeq := event.Seq(0)
	if len(events) > 0 {
		lastSeq = events[len(events)-1].Seq
	}
	snap := h.snapshot(s)
	snap.LastSeq = lastSeq
	return core.SessionSnapshot{
		Session: snap,
		Events:  events,
		LastSeq: lastSeq,
	}, nil
}

// ---- EventReader ----

func (h *Host) ListEvents(ctx context.Context, principal core.Principal, sessionID event.SessionID, after event.Seq, limit int) ([]event.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := principal.Validate(); err != nil {
		return nil, err
	}
	if err := sessionID.Validate(); err != nil {
		return nil, core.ErrInvalidInput
	}
	if _, err := h.lookup(principal, sessionID); err != nil {
		return nil, err
	}
	return h.store.Read(ctx, sessionID, after, limit)
}

func (h *Host) Subscribe(ctx context.Context, principal core.Principal, sessionID event.SessionID, after event.Seq) (core.EventSubscription, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := principal.Validate(); err != nil {
		return nil, err
	}
	if err := sessionID.Validate(); err != nil {
		return nil, core.ErrInvalidInput
	}
	s, err := h.lookup(principal, sessionID)
	if err != nil {
		return nil, err
	}

	sub := newSubscription(h.subBuffer, after)

	// Attach a detach hook, then register BEFORE replay so no durable event with
	// seq > after is ever missed. Events committed during the replay window are
	// held by push (phase == replaying) and merged in sequence order by
	// completeReplay; the replayed history itself is staged in an unbounded
	// pending queue drained by Next, so it can never overflow the live buffer.
	sub.detach = func() { s.detach(sub) }
	s.subsMu.Lock()
	s.subs[sub] = struct{}{}
	s.subsMu.Unlock()

	replayed, err := h.store.Read(ctx, sessionID, after, 0)
	if err != nil {
		s.detach(sub)
		sub.Close()
		return nil, err
	}
	sub.completeReplay(replayed)
	return sub, nil
}

// ---- Crash recovery (§5.7) ----

// SessionMetadata is the caller-supplied durable description of a session that a
// prior incarnation left in `running` at crash time. In P1 it is read from the
// sessions table; in P0 it is supplied directly (the stub).
type SessionMetadata struct {
	ID        event.SessionID
	TenantID  event.TenantID
	Workspace event.WorkspaceID
	CreatedBy event.UserID
	Title     string
	CreatedAt time.Time
}

// RecoverRunning re-creates each metadata session in `error` state and appends
// a SessionError{daemon_restart} event so a client replaying from the start sees
// that the session did not silently survive the restart. It is an internal
// bootstrap hook (called at daemon start), not a client command.
//
// Because P0 has no durable store, the pre-crash durable history and the
// sessions.last_seq are NOT reconstructed here: each recovered session starts a
// fresh empty log with the daemon_restart marker as seq 1. Reconstructing the
// full log from the store is P1's job (the stub surfaces the state-machine
// contract, not the data).
func (h *Host) RecoverRunning(ctx context.Context, principal core.Principal, metas []SessionMetadata) ([]core.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if h.closed.Load() {
		return nil, ErrHostClosed
	}
	if err := principal.Validate(); err != nil {
		return nil, err
	}
	out := make([]core.Session, 0, len(metas))
	for _, m := range metas {
		if m.TenantID != principal.TenantID {
			continue // only the caller's tenant; silent skip (no existence leak)
		}
		if err := m.ID.Validate(); err != nil {
			return nil, err
		}
		if err := m.Workspace.Validate(); err != nil {
			return nil, err
		}
		if err := m.CreatedBy.Validate(); err != nil {
			return nil, err
		}
		h.mu.Lock()
		_, dup := h.sessions[m.ID]
		h.mu.Unlock()
		if dup {
			return nil, fmt.Errorf("sessionhost: duplicate session id %q in recovery", m.ID)
		}
		s := h.newSession(m.ID, m.TenantID, m.Workspace, m.Title, m.CreatedBy, m.CreatedAt)
		s.state = core.SessionError // recovered into error state; initialization, not a transition
		// Append the marker BEFORE registration so the executor never runs
		// before its restart marker exists (single-producer invariant).
		h.emitDraft(s, event.KindSessionError, event.SessionError{
			Reason: "daemon_restart",
			Err:    "session was running at daemon shutdown; recovered in error state",
		})
		h.register(s)
		out = append(out, h.snapshot(s))
	}
	return out, nil
}

// Close gracefully shuts down every session: sessions with no in-flight turn
// close synchronously, sessions with a running turn are cancelled and allowed to
// finalize (TurnCompleted{cancelled} + SessionClosed) before Close returns. It
// is idempotent and rejects new sessions once begun.
func (h *Host) Close(ctx context.Context) error {
	h.closed.Store(true)

	h.mu.Lock()
	sessions := make([]*session, 0, len(h.sessions))
	for _, s := range h.sessions {
		sessions = append(sessions, s)
	}
	h.mu.Unlock()

	for _, s := range sessions {
		h.requestClose(s, "shutdown")
	}
	for _, s := range sessions {
		select {
		case <-s.doneCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// ---- internals ----

func writerAllowed(r core.Role) bool {
	return r == core.RoleOwner || r == core.RoleAdmin || r == core.RoleMember
}

type session struct {
	mu sync.Mutex

	// Identity is immutable after construction; safe to read without mu.
	sid       event.SessionID
	tenant    event.TenantID
	workspace event.WorkspaceID
	title     string
	createdBy event.UserID
	createdAt time.Time

	// Lifecycle (guarded by mu).
	state     core.SessionState
	turnIndex uint64
	pending   int // accepted-but-not-yet-resolved inputs (running + queued)
	queue     []inputEnvelope
	current   *turnHandle // non-nil while a turn is active
	// interrupted is set by Interrupt under mu and consumed by the next turn's
	// start/finalization, so an interrupt racing a turn's return is never missed.
	interrupted bool
	closing     string // non-empty once close is requested; the SessionClosed reason
	closedAt    time.Time

	// Run-loop coordination.
	kick   chan struct{} // buffered(1); signaled when an input is enqueued or close is requested
	doneCh chan struct{} // closed when the run loop exits

	// Subscribers (guarded by subsMu, separate from mu so notify/replay never
	// contend with turn state).
	subsMu sync.Mutex
	subs   map[*subscription]struct{}
}

type inputEnvelope struct {
	turnID     event.TurnID
	text       string
	readAction bool
}

type turnHandle struct {
	cancel context.CancelFunc
}

func (h *Host) newSession(sid event.SessionID, tenant event.TenantID, workspace event.WorkspaceID, title string, createdBy event.UserID, createdAt time.Time) *session {
	if createdAt.IsZero() {
		createdAt = h.now()
	}
	return &session{
		sid:       sid,
		tenant:    tenant,
		workspace: workspace,
		title:     title,
		createdBy: createdBy,
		createdAt: createdAt,
		state:     core.SessionIdle,
		kick:      make(chan struct{}, 1),
		doneCh:    make(chan struct{}),
		subs:      make(map[*subscription]struct{}),
	}
}

// setStateLocked transitions the session state, asserting the transition table.
// Caller must hold s.mu. Panics on an illegal transition (fail loud — these are
// invariants, and a violated transition indicates a real bug).
func (s *session) setStateLocked(next core.SessionState) {
	if s.state == next {
		return
	}
	if !s.state.CanTransitionTo(next) {
		panic(fmt.Sprintf("sessionhost: illegal state transition %s -> %s", s.state, next))
	}
	s.state = next
}

// register starts the session's executor goroutine and records it in h.sessions.
// Must be called only after the session's first durable event has been appended,
// so the executor never outruns its own marker.
func (h *Host) register(s *session) {
	h.mu.Lock()
	h.sessions[s.sid] = s
	h.mu.Unlock()
	go h.runLoop(s)
}

func (h *Host) lookup(principal core.Principal, sid event.SessionID) (*session, error) {
	h.mu.Lock()
	s := h.sessions[sid]
	h.mu.Unlock()
	if s == nil || s.tenant != principal.TenantID {
		return nil, core.ErrSessionNotFound
	}
	return s, nil
}

func (s *session) signalKick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

func (s *session) detach(sub *subscription) {
	s.subsMu.Lock()
	delete(s.subs, sub)
	s.subsMu.Unlock()
}

// snapshot builds a query-side Session view. Store reads (LastSeq) happen under
// the store lock, not session.mu.
func (h *Host) snapshot(s *session) core.Session {
	s.mu.Lock()
	state := s.state
	closedAt := s.closedAt
	s.mu.Unlock()
	lastSeq, _ := h.store.LastSeq(context.Background(), s.sid)
	return core.Session{
		ID:        s.sid,
		TenantID:  s.tenant,
		Workspace: s.workspace,
		State:     state,
		LastSeq:   lastSeq,
		CreatedBy: s.createdBy,
		Title:     s.title,
		CreatedAt: s.createdAt,
		ClosedAt:  closedAt,
	}
}

// runLoop is the single executor goroutine for one session. It dequeues inputs
// in FIFO order and hands each to handleInput; it exits once the session is
// closed.
func (h *Host) runLoop(s *session) {
	defer close(s.doneCh)
	for {
		s.mu.Lock()
		for len(s.queue) == 0 && s.closing == "" {
			s.mu.Unlock()
			select {
			case <-s.kick:
			}
			s.mu.Lock()
		}
		if s.closing != "" {
			// Close requested and no turn is running (the loop only reaches here
			// between turns): finalize and exit. Queued inputs are abandoned below.
			s.mu.Unlock()
			h.finalizeClose(s)
			return
		}
		env := s.queue[0]
		s.queue = s.queue[1:]
		s.mu.Unlock()

		if h.handleInput(s, env) {
			// handleInput resolved the running turn and close was requested during
			// it; finalize (emit abandonment + SessionClosed) and exit.
			h.finalizeClose(s)
			return
		}
	}
}

// handleInput runs one dequeued input as a turn. It returns true if the session
// must now close (the turn was interrupted-or-completed while a close was
// pending); the caller (runLoop) then finalizes the close.
func (h *Host) handleInput(s *session, input inputEnvelope) bool {
	s.mu.Lock()
	// A close may have been requested in the gap between the run loop's check
	// and this dequeue. If so, do not start a new turn: abandon this input
	// explicitly (it is already out of the queue, so finalizeClose will not
	// see it) and let the caller finalize the close for the rest.
	if s.closing != "" {
		s.pending--
		s.mu.Unlock()
		h.emitDraft(s, event.KindTurnCompleted, event.TurnCompleted{TurnID: input.turnID, Outcome: "cancelled"})
		return true
	}
	s.turnIndex++
	turnIdx := s.turnIndex
	interrupted := s.interrupted
	s.interrupted = false
	turnCtx, cancel := context.WithCancel(context.Background())
	s.current = &turnHandle{cancel: cancel}
	s.mu.Unlock()

	if interrupted {
		// Interrupt arrived while this turn was queued; start it pre-cancelled.
		cancel()
	}

	// Durable: the turn began.
	h.emitDraft(s, event.KindTurnStarted, event.TurnStarted{TurnID: input.turnID, TurnIndex: turnIdx})

	text, err := h.turn(turnCtx, TurnInput{
		SessionID:  s.sid,
		TurnID:     input.turnID,
		TurnIndex:  turnIdx,
		Text:       input.text,
		ReadAction: input.readAction,
	})

	return h.finishTurn(s, input.turnID, turnCtx, text, err)
}

// finishTurn is the single linearization point for a completed turn. It resolves
// — atomically under session.mu — whether the turn completed, was cancelled
// (interrupt/close), or failed (backend error), emits the corresponding durable
// events, and transitions the session state. It returns true if the session must
// now close.
func (h *Host) finishTurn(s *session, turnID event.TurnID, turnCtx context.Context, text string, turnErr error) bool {
	s.mu.Lock()
	closing := s.closing != ""
	cancelled := turnCtx.Err() != nil || s.interrupted
	s.interrupted = false
	s.current = nil
	s.pending--

	var outcome string
	switch {
	case cancelled:
		outcome = "cancelled"
	case turnErr != nil:
		outcome = "stream_error"
	case text == "":
		outcome = "empty"
	default:
		outcome = "complete"
	}

	terminal := closing
	var abandoned []inputEnvelope
	if !closing {
		switch {
		case outcome == "stream_error":
			// Backend failure: park in error and abandon queued inputs (with an
			// explicit cancellation event for each acked turn).
			s.setStateLocked(core.SessionError)
			if len(s.queue) > 0 {
				abandoned = s.queue
				s.queue = nil
				s.pending -= len(abandoned)
			}
		case len(s.queue) == 0:
			s.setStateLocked(core.SessionIdle)
		}
		// else: queue non-empty and no error — stay running for the next turn.
	}
	// If closing, state stays running until finalizeClose; the queued inputs are
	// abandoned there.
	s.mu.Unlock()

	// Emit (never while holding s.mu — emitDraft takes store.mu and subsMu).
	if outcome == "complete" {
		h.emitDraft(s, event.KindMessageCommitted, event.MessageCommitted{TurnID: turnID, Text: text})
	}
	h.emitDraft(s, event.KindTurnCompleted, event.TurnCompleted{TurnID: turnID, Outcome: outcome})
	if outcome == "stream_error" {
		h.emitDraft(s, event.KindSessionError, event.SessionError{Reason: "backend_failure", Err: turnErr.Error()})
		for _, q := range abandoned {
			h.emitDraft(s, event.KindTurnCompleted, event.TurnCompleted{TurnID: q.turnID, Outcome: "cancelled"})
		}
	}
	return terminal
}

// finalizeClose abandons queued inputs (emitting TurnCompleted{cancelled} for
// each acked turn), emits SessionClosed, and sets the terminal state. It is the
// only place SessionClosed is produced. Called from the executor goroutine only.
func (h *Host) finalizeClose(s *session) {
	s.mu.Lock()
	reason := s.closing
	queued := s.queue
	s.queue = nil
	s.pending -= len(queued)
	s.setStateLocked(core.SessionClosed)
	s.closedAt = h.now()
	s.mu.Unlock()

	for _, q := range queued {
		h.emitDraft(s, event.KindTurnCompleted, event.TurnCompleted{TurnID: q.turnID, Outcome: "cancelled"})
	}
	h.emitDraft(s, event.KindSessionClosed, event.SessionClosed{Reason: reason})
}

// requestClose initiates session closure. It only sets a flag, cancels the
// in-flight turn (if any), and wakes the executor — the executor emits
// SessionClosed and performs the terminal transition. Idempotent.
func (h *Host) requestClose(s *session, reason string) {
	s.mu.Lock()
	if s.state == core.SessionClosed || s.closing != "" {
		s.mu.Unlock()
		return
	}
	s.closing = reason
	cur := s.current
	s.mu.Unlock()
	if cur != nil {
		cur.cancel()
	}
	s.signalKick()
}

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
	committed, err := h.store.Append(context.Background(), draft)
	if err != nil {
		panic(fmt.Sprintf("sessionhost: durable append %s failed: %v", kind, err))
	}
	h.notify(s, committed)
}

// notify delivers a committed durable event to every live subscriber. It is
// non-blocking: a lagging subscriber is disconnected (never blocks the executor).
func (h *Host) notify(s *session, ev event.Event) {
	s.subsMu.Lock()
	subs := make([]*subscription, 0, len(s.subs))
	for sub := range s.subs {
		subs = append(subs, sub)
	}
	s.subsMu.Unlock()
	for _, sub := range subs {
		sub.push(ev)
	}
}

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
