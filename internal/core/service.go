// Package core is the transport-free heart of wakild (card #148). It owns the
// domain vocabulary that every client (TUI, Web-UI, CLI/CI) and every handler
// consumes, regardless of whether the core runs embedded in a single process
// (embedded mode, the default) or behind a Connect/gRPC server (P2).
//
// The hard rule from docs/design/wakild-foundation.md §2.1 applies here in full
// force: this package (and everything under internal/core) imports neither
// api/gen nor internal/server, and never bubbletea. Handlers in internal/server
// are pure translators between wire types and these domain types; the proto in
// P2 is written against this package, not the other way around.
//
// This file defines the three service interfaces (plan decision D7), the
// identity model (D4), the session state machine, and the store-side contracts
// the service is implemented against (D3). It is a contracts file, not an
// implementation — the in-memory session host lands in chunk 4, the SQLite-
// backed store in P1.
//
// D7 — three behavior-first interfaces, not one god-interface:
//
//   - SessionService: the command surface. Create sessions, submit input,
//     respond to approvals, interrupt, close. Construction/bootstrap (config,
//     proxy.Client, executor, trace sinks, persistence) is deliberately NOT on
//     these interfaces — it never leaks through to a client.
//
//   - EventReader: the observation surface. Subscribe to live events and list
//     the durable history (replay). Two clients reading the same session see
//     the same ordered events.
//
//   - SessionReader: the query surface. Get/list sessions and snapshots without
//     ever mutating them.
//
// D3 — the store contracts the service is built against (EventAppender,
// EventLog). These are the seams P1 swaps from in-memory to SQLite; the service
// depends on interfaces, never on the concrete store. Sequence assignment is
// NOT a separate seam: it is internal to EventAppender.Append, because
// assignment and durable append must be atomic (contiguous per-session
// sequences) and no producer may ever stamp its own sequence.
//
// Authorization: every method takes an explicit Principal (resolved from auth
// in P2, never deserialized from the wire body). Tenant isolation is enforced
// at the service layer BEFORE any store method is called — the store contracts
// below are trusted primitives keyed by globally-unique SessionID, matching the
// plan's P1 note that tenant filtering on the events table is redundant.
//
// Naming note: D7's client-facing EventReader (Subscribe/ListEvents) and D3's
// store-side cursor-read contract would otherwise collide under one name in one
// package, so the store read side is EventLog. This is the only deliberate
// deviation from the plan's inline names; the semantics are exactly as written
// there (subscribe/list vs. cursor-addressable read).
package core

import (
	"context"
	"errors"
	"time"

	"github.com/treeol/wakil/internal/core/event"
)

// ---- Identity (D4, doc §6.1) ----

// Role is a tenant membership role (doc §6.3). Kept deliberately small.
type Role string

const (
	// RoleOwner has full control including tenant deletion and billing.
	RoleOwner Role = "owner"
	// RoleAdmin manages users, backends/credentials, agents, workspaces.
	RoleAdmin Role = "admin"
	// RoleMember creates and drives sessions and writes gated memory. Session
	// deletion is a P2 command (DeleteSession) not present on the P0 command
	// surface.
	RoleMember Role = "member"
	// RoleViewer reads sessions and traces, starts nothing.
	RoleViewer Role = "viewer"
)

// AuthMethod records how a principal was resolved.
type AuthMethod string

const (
	// AuthEmbedded is the in-process single-user path (the default): no
	// credentials, constant local tenant/user.
	AuthEmbedded AuthMethod = "embedded"
	// AuthLocal is Unix-socket peer-credentials.
	AuthLocal AuthMethod = "local"
	// AuthHosted is OIDC/API-token against the daemon.
	AuthHosted AuthMethod = "hosted"
)

// Principal is the resolved caller of every service method (doc §6.1). The
// core never sees an anonymous call: even embedded mode resolves a constant
// local principal (D4), so the multi-tenant and single-user paths are the same
// code path with different resolution.
//
// In P2 the principal is resolved from authentication and passed explicitly —
// never deserialized from a client-supplied request body, so a client cannot
// spoof its own identity or role.
type Principal struct {
	TenantID event.TenantID
	UserID   event.UserID
	Role     Role
	// Scopes is the set of API-token scopes; empty for non-token auth. Callers
	// must not mutate a principal they receive; construct fresh ones.
	Scopes     []string
	AuthMethod AuthMethod
}

// Validate performs structural identity validation: tenant and user must be
// present and well-formed. It deliberately does NOT validate Role or Scopes —
// those are authorization inputs checked by the authz layer, not identity
// claims, and a principal may be resolved before its role is known. Structural
// validity (can this principal address a tenant at all?) is separate from
// authorization (may this principal do X?).
func (p Principal) Validate() error {
	if err := p.TenantID.Validate(); err != nil {
		return err
	}
	if err := p.UserID.Validate(); err != nil {
		return err
	}
	return nil
}

// EmbeddedPrincipal returns the constant principal for embedded/local
// single-user mode (D4, doc §6.1): default tenant, default user, owner, no
// scopes. It is a function, not a variable, so callers always receive a fresh
// value and cannot mutate shared global state.
func EmbeddedPrincipal() Principal {
	return Principal{
		TenantID:   event.EmbeddedTenantID,
		UserID:     event.EmbeddedUserID,
		Role:       RoleOwner,
		AuthMethod: AuthEmbedded,
	}
}

// ---- Session lifecycle state (doc §4.3 sessions.state) ----

// SessionState is the lifecycle state of a session. The legal transitions are
// defined by CanTransitionTo; see the table in its doc comment.
type SessionState string

const (
	// SessionIdle has no turn in flight and is ready for input.
	SessionIdle SessionState = "idle"
	// SessionRunning has a turn executing.
	SessionRunning SessionState = "running"
	// SessionAwaitingApproval is parked on a tool approval (P2 async path; in
	// P0 the shim resolves synchronously and does not park the executor — D5).
	SessionAwaitingApproval SessionState = "awaiting_approval"
	// SessionError is unrecoverable; re-driven by SubmitInput (error → running).
	SessionError SessionState = "error"
	// SessionClosed is terminal.
	SessionClosed SessionState = "closed"
)

// stateTransitions is the single source of truth for the session state machine.
//
//	idle              → running | closed
//	running           → idle | awaiting_approval | error | closed
//	awaiting_approval → running | error | closed
//	error             → running (re-drive via SubmitInput) | closed
//	closed            → (terminal)
//
// A single turn is in flight at a time; multiple inputs are FIFO-queued (see
// SubmitInput). Approval does not park a second turn — awaiting_approval is a
// sub-state of the running turn.
var stateTransitions = map[SessionState][]SessionState{
	SessionIdle:             {SessionRunning, SessionClosed},
	SessionRunning:          {SessionIdle, SessionAwaitingApproval, SessionError, SessionClosed},
	SessionAwaitingApproval: {SessionRunning, SessionError, SessionClosed},
	SessionError:            {SessionRunning, SessionClosed},
	SessionClosed:           {},
}

// CanTransitionTo reports whether next is a legal successor of s.
func (s SessionState) CanTransitionTo(next SessionState) bool {
	for _, n := range stateTransitions[s] {
		if n == next {
			return true
		}
	}
	return false
}

// Session is the query-side view of a session (doc §4.3). It carries identity
// and lifecycle metadata; the message history lives in the event log, not here.
type Session struct {
	ID        event.SessionID
	TenantID  event.TenantID
	Workspace event.WorkspaceID
	State     SessionState
	LastSeq   event.Seq
	CreatedBy event.UserID
	Title     string
	CreatedAt time.Time
	ClosedAt  time.Time // zero until closed
}

// ---- Requests / acknowledgements ----

// CreateSessionRequest pins the inputs a new session is created with. In P0 it
// carries only what the in-memory host needs; agent-revision and backend
// pinning (agent_revision_id, backend_id in the schema) are ADDITIVE fields
// that land with the store in P1 — adding them is backward-compatible and does
// not break this contract.
type CreateSessionRequest struct {
	Workspace event.WorkspaceID
	Title     string
}

// SubmitInputRequest carries one user input to enqueue. It is a value with no
// reply channel: the result is a TurnAck plus events over EventReader.Subscribe
// (D6 — data only, no channels).
type SubmitInputRequest struct {
	SessionID event.SessionID
	// Text is the user's input for this turn.
	Text string
	// ReadAction marks the input as read-only, which may relax approval gates.
	// It is carried through to the TurnStarted and ApprovalRequested payloads so
	// the approver can decide on the basis of the event stream alone.
	ReadAction bool
	// RequestID is an optional client-supplied idempotency key, reserved now so
	// P2's wire retries can deduplicate without a breaking change. It is NOT
	// enforced in P0 (no durable store to dedup against); enforcement lands with
	// durable storage. Empty means "no idempotency requested".
	RequestID string
}

// TurnAck is the immediate acknowledgement of a submitted input (D1, doc §3.4).
//
// A successful TurnAck guarantees the input has been ACCEPTED into the
// session's FIFO queue (or re-driven an errored session) and will be executed —
// it does NOT guarantee the turn has started or completed. Everything after
// this — TurnStarted, deltas, tool calls, approvals, errors, completion —
// arrives as events, never as this method's return value. In P0 acceptance is
// in-memory; P1 makes acceptance durable (input persisted before the ack).
type TurnAck struct {
	SessionID event.SessionID
	TurnID    event.TurnID
}

// ApprovalOutcome is a client's answer to an approval request. A single enum —
// not a bool pair — so the illegal/ambiguous combinations are unrepresentable.
type ApprovalOutcome string

const (
	// ApprovalDeny rejects the tool call.
	ApprovalDeny ApprovalOutcome = "deny"
	// ApprovalAllowOnce allows the tool call this one time.
	ApprovalAllowOnce ApprovalOutcome = "allow_once"
	// ApprovalAllowReadsOnce allows read-only execution this one time.
	ApprovalAllowReadsOnce ApprovalOutcome = "allow_reads_once"
)

// ApprovalDecision is a client's answer to an approval request. The resolver is
// the method's principal argument; it is recorded in the ApprovalResolved
// event's Resolver field for the durable audit trail.
type ApprovalDecision struct {
	SessionID  event.SessionID
	ApprovalID event.ApprovalID
	Outcome    ApprovalOutcome
	Reason     string
}

// Validate performs structural validation of the decision.
func (d ApprovalDecision) Validate() error {
	if err := d.SessionID.Validate(); err != nil {
		return err
	}
	if err := d.ApprovalID.Validate(); err != nil {
		return err
	}
	switch d.Outcome {
	case ApprovalDeny, ApprovalAllowOnce, ApprovalAllowReadsOnce:
	default:
		return errors.New("core: invalid approval outcome")
	}
	return nil
}

// ---- D7: the three service interfaces ----

// SessionService is the command surface of the core. No call waits on an agent
// turn (doc §3.4): SubmitInput enqueues and returns a TurnAck immediately; all
// turn progress is observed via EventReader. It is implemented by the session
// host (chunk 4) and, from P2, reached over the wire by external clients.
//
// Every method takes the caller's Principal as its first argument after ctx.
// This is the one convention across the whole surface, matching P2 where the
// principal is resolved from auth and never read from the request body.
type SessionService interface {
	// CreateSession creates a new session owned by the principal's tenant.
	CreateSession(ctx context.Context, principal Principal, req CreateSessionRequest) (Session, error)
	// SubmitInput enqueues one input for the session and returns immediately
	// with a TurnAck. Inputs to a running session are FIFO-queued (bounded
	// depth); it returns ErrSessionBusy only when the queue is full. It returns
	// ErrSessionClosed for a closed session, and ErrNotAuthorized when the
	// principal may not drive the session. Submitting to an errored session is
	// the explicit re-drive path (error → running).
	SubmitInput(ctx context.Context, principal Principal, req SubmitInputRequest) (TurnAck, error)
	// RespondToApproval answers a pending approval request. The resolver is the
	// principal argument (recorded in the ApprovalResolved event). It returns
	// ErrApprovalNotFound for an unknown approval and
	// ErrApprovalAlreadyResolved for a stale/duplicate decision — duplicate
	// decisions are idempotent, not an error, only when the decision matches the
	// already-recorded outcome. In P0 this is the shim's seam (D5); in P2 it is
	// the authoritative wire path.
	RespondToApproval(ctx context.Context, principal Principal, d ApprovalDecision) error
	// Interrupt cancels the in-flight turn. The ctx is the caller's RPC deadline
	// only; once accepted, cancellation is delivered to the turn through the
	// session's internal context, independent of the caller's ctx. It emits
	// TurnCompleted{cancelled} — never a silent abort (doc §5.5).
	Interrupt(ctx context.Context, principal Principal, sessionID event.SessionID) error
	// CloseSession ends the session: it INITIATES cancellation and returns
	// immediately (it does not block on turn completion). The executor drains
	// and the SessionClosed event is delivered asynchronously — completion is
	// observed via EventReader, not via this call's return (doc §5.6).
	CloseSession(ctx context.Context, principal Principal, sessionID event.SessionID) error
}

// EventReader is the observation surface (D7). It exposes both live streaming
// and durable history so a reconnect loses nothing (doc §3.4).
type EventReader interface {
	// Subscribe returns an ordered live stream of events for a session. After
	// replaying durable history from the cursor (seq > after), it delivers
	// committed durable events and ephemeral notifications as they are produced.
	//
	// Handoff contract (implemented in chunk 4): the live subscription is
	// registered BEFORE the durable replay is read, so no durable event with
	// seq > after is ever missed; the replay/live boundary may therefore deliver
	// an event twice, and the caller deduplicates by seq. Ephemeral events
	// (seq 0) produced during replay may be dropped. The caller MUST Close the
	// subscription to release resources.
	Subscribe(ctx context.Context, principal Principal, sessionID event.SessionID, after event.Seq) (EventSubscription, error)
	// ListEvents returns the durable history of a session with seq > after,
	// ascending, up to limit entries. Cursor semantics are exclusive: after=0
	// returns from seq 1. limit <= 0 means "no limit"; the store bounds the
	// result size. Pagination is a known P1/P2 seam — this P0 shape is
	// intentionally minimal.
	ListEvents(ctx context.Context, principal Principal, sessionID event.SessionID, after event.Seq, limit int) ([]event.Event, error)
}

// SessionReader is the query surface (D7): read-only session state and
// snapshots, never mutation.
type SessionReader interface {
	// GetSession returns one session's current state. It returns
	// ErrSessionNotFound for sessions that do not exist or are not visible to
	// the principal's tenant (no existence leak).
	GetSession(ctx context.Context, principal Principal, sessionID event.SessionID) (Session, error)
	// ListSessions returns sessions visible to the principal's tenant. Ordering,
	// filters, and pagination are known P1/P2 seams; this P0 shape is minimal.
	ListSessions(ctx context.Context, principal Principal) ([]Session, error)
	// SessionSnapshot returns the session's metadata plus its durable event
	// history — the raw material from which a client reconstructs its
	// client-visible projection (D9). It is a replay bundle, not a pre-rendered
	// projection; clients project over the events. Unbounded for long sessions —
	// a bounded cursor-based form lands with P1 pagination.
	SessionSnapshot(ctx context.Context, principal Principal, sessionID event.SessionID) (SessionSnapshot, error)
}

// EventSubscription is a live event stream handle returned by Subscribe. It is
// ordered and must be Closed by the caller.
type EventSubscription interface {
	// Next returns the next event in order. It blocks until an event is
	// available, the subscription is closed, or ctx is cancelled. After Close,
	// Next returns io.EOF. Next is NOT safe for concurrent calls — a single
	// consumer drives it; the P2 streaming wrapper preserves this contract.
	Next(ctx context.Context) (event.Event, error)
	// Close releases the subscription's resources and unblocks Next. It is
	// idempotent: multiple Close calls are safe.
	Close() error
}

// SessionSnapshot is the D9 replay bundle: the session's metadata plus its
// durable events, from seq 1 to LastSeq. See SessionReader.SessionSnapshot for
// the projection contract.
type SessionSnapshot struct {
	Session Session
	Events  []event.Event
	LastSeq event.Seq
}

// ---- D3: store contracts ----

// EventAppender is the durable-append seam (D3). It owns durability-before-
// visibility AND sequence assignment: a successful Append has assigned the next
// sequence, persisted the event, and made it visible to readers — all as one
// atomic step. Producers never assign Seq themselves; they submit drafts and
// receive committed events.
//
// The append implementation is the single commit→notify point: after Append
// returns, the committed event WILL be delivered to subscribers by the host.
// Producers must not fan events out themselves.
//
// In P0 this is the in-memory event log serialized under the executor lock; in
// P1 Append and the session's last_seq advance become one SQLite transaction.
type EventAppender interface {
	// Append validates the draft, assigns the next sequence, persists it, and
	// returns the committed event. It accepts DURABLE drafts only — an
	// ephemeral draft (Kind.Class() == ClassEphemeral) is rejected, because
	// ephemeral events carry no sequence and are never persisted (D2). Seq and
	// Ts: Seq is assigned by Append; Ts is producer-assigned and preserved
	// (ValidateDraft already requires non-zero). The appender does not know
	// session lifecycle — enforcing "no appends to a closed session" is the
	// host's job, so the SessionClosed event itself remains appendable.
	Append(ctx context.Context, draft event.Event) (event.Event, error)
}

// EventLog is the store-side read contract (D3): the cursor-addressable durable
// history. It is read-only — append lives on EventAppender. It reads durable
// events only: ephemeral notifications are live-only and never in the log (D2).
//
// EventLog is a trusted primitive keyed by globally-unique SessionID; tenant
// authorization is enforced at the service layer before these methods are
// called (plan D4/P1 note: tenant filtering on the events table is redundant
// given globally-unique session IDs).
type EventLog interface {
	// Read returns the durable events of a session with seq > after, ascending,
	// up to limit entries. Cursor semantics are exclusive: after=0 returns from
	// seq 1. limit <= 0 means "no limit", bounded by the store implementation.
	Read(ctx context.Context, sessionID event.SessionID, after event.Seq, limit int) ([]event.Event, error)
	// LastSeq returns the highest committed durable seq for the session, 0 if
	// none. It is the cursor a reconnecting client resumes from.
	LastSeq(ctx context.Context, sessionID event.SessionID) (event.Seq, error)
}

// ---- Errors ----

// Sentinel errors the service contracts surface. Handlers translate these to
// wire-level codes; the core never leaks transport errors upward. Implementations
// wrap these sentinels so callers test with errors.Is.
var (
	// ErrSessionNotFound is returned when a session does not exist or is not
	// visible to the caller's tenant. Using one code for both avoids leaking
	// existence of other tenants' sessions.
	ErrSessionNotFound = errors.New("core: session not found")
	// ErrSessionClosed is returned when an operation targets a closed session.
	ErrSessionClosed = errors.New("core: session closed")
	// ErrSessionBusy is returned when a session's input queue is full and it
	// cannot accept another input right now (a running session with queue
	// capacity still accepts inputs — they are queued, not rejected).
	ErrSessionBusy = errors.New("core: session busy")
	// ErrNotAuthorized is returned when the principal's ROLE forbids the
	// operation (e.g. a viewer attempting a write), even though the target
	// exists. Cross-tenant invisibility is ErrSessionNotFound, not this.
	ErrNotAuthorized = errors.New("core: not authorized")
	// ErrInvalidInput is returned when a request is structurally invalid.
	ErrInvalidInput = errors.New("core: invalid input")
	// ErrInvalidStateTransition is returned when an operation would move a
	// session through an illegal state transition (see CanTransitionTo).
	ErrInvalidStateTransition = errors.New("core: invalid state transition")
	// ErrApprovalNotFound is returned when an approval decision targets an
	// unknown approval.
	ErrApprovalNotFound = errors.New("core: approval not found")
	// ErrApprovalAlreadyResolved is returned when an approval decision targets
	// an approval that was already resolved with a DIFFERENT outcome. A
	// duplicate decision matching the recorded outcome is idempotent (no error).
	ErrApprovalAlreadyResolved = errors.New("core: approval already resolved")
	// ErrSubscriptionClosed is returned by EventSubscription.Next after Close.
	// Callers may also test for io.EOF.
	ErrSubscriptionClosed = errors.New("core: subscription closed")
)
