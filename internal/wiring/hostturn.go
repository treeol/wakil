// Package wiring composes the transport-free core (internal/core, chunk 4's
// in-memory session host) with the real agent loop (internal/agent), translating
// the agent's outbound signals into domain events (card #148 chunk 5, plan
// deliverables 4 + 6).
//
// It is the ONLY package that bridges the two: sessionhost stays free of
// internal/agent (D12), and the agent package is unaware of the host. This is
// the composition package the plan's §7 names as the home of the adapter.
//
// Chunk-5 scope (see docs/cards/card-148-chunk5-plan.md):
//   - one *agent.App drives exactly one embedded host session. The returned
//     TurnFunc is bound to the first session it serves: a second session
//     reusing the same TurnFunc fails loudly (one App is one mutable
//     conversation and cannot safely back two host sessions).
//   - the adapter runs a turn via the agent's resilience entrypoint
//     (agent.DriveTurnWithResilience — chunk 7) and returns the authoritative
//     assistant text (TurnOutcome.Text) for the host's MessageCommitted. Stream-
//     error retry and empty-response recovery are applied INSIDE that entrypoint,
//     so the host sees exactly one final turn (complete/stream_error/cancelled).
//   - stream chunks become MessageDelta (ephemeral), reasoning becomes
//     ReasoningDelta (ephemeral),
//   - approvals go through a context-aware confirmer that emits
//     ApprovalRequested → ApprovalResolved with full outcome fidelity (D5 shim),
//     and the resolver returns an ApprovalResolution{Choice, Reason} so a
//     decline's rationale travels in the event (D18).
//
// Deferred (not hidden): tool-call/subagent durable events (the agent does not
// yet emit them with valid domain IDs/digests/status), per-session App factory
// (deliverable 5), the async wire approval path (P2), and workflow transitions
// (a turn failure surfaces as the adapter's error → SessionError).
package wiring

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/id"
	"github.com/treeol/wakil/internal/core/sessionhost"
	"github.com/treeol/wakil/internal/policy"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/workflow"
)

// ApprovalRequest is the resolved request a resolver answers. It mirrors the
// fields the agent's Confirmer receives, so a resolver has everything a
// synchronous gate sees.
type ApprovalRequest struct {
	ToolName   string
	Headline   string
	Detail     string
	ReadAction bool
}

// ApprovalResolution is a resolver's answer to one approval request: the choice
// plus a human-readable reason. Reason is meaningful on ChoiceDecline (and the
// adapter's forced declines) and empty for approved/allowed-reads resolutions.
type ApprovalResolution struct {
	Choice agent.ConfirmChoice
	Reason string
}

// ApprovalResolver answers one approval request. It is invoked in its own
// goroutine and its result is raced against ctx cancellation: a resolver that
// ignores ctx cannot block the executor (the confirmer forces a decline). Its
// returned value is trusted only if it arrives before cancellation wins the
// race; otherwise the confirmer resolves the approval as declined.
//
// nil means "decline everything" (the safe headless-without-approval default).
type ApprovalResolver func(ctx context.Context, req ApprovalRequest) ApprovalResolution

// adapterConfig holds HostTurnFunc options.
type adapterConfig struct {
	resolver ApprovalResolver
	// asyncApproval enables the async approval path (7b2 D25). When true,
	// the confirmer parks the session in awaiting_approval and blocks on
	// RespondToApproval instead of using the inline resolver. The TUI session
	// uses this; the headless path does not (keeps sync inline resolution).
	asyncApproval bool
	// planAutoAdvance enables the headless --plan after-turn resolver (7c):
	// when HandleWorkflowTransition pauses the workflow waiting for a user
	// action, the adapter applies the legacy headless auto-advance policy
	// (present→implement, review-skip, step advance / final review) instead
	// of leaving the session idle. It also makes enqueue rejection terminal
	// (an error) instead of a silent drop: a headless run has no user to
	// resume a silently-paused workflow.
	planAutoAdvance bool
	// noOracle mirrors HeadlessOptions.NoOracle for the review-skip reason
	// (legacy parity: "oracle disabled by flag (--no-oracle)").
	noOracle bool
	// coordinator serializes turn starts against session transitions
	// (LoadSession, InitNewSession). The daemon path sets this; the
	// headless path leaves it nil (single turn, no transitions).
	coordinator *TransitionCoordinator
}

// AdapterOption configures the HostTurnFunc adapter.
type AdapterOption func(*adapterConfig)

// WithResolver sets the approval resolver. nil (default) declines everything.
func WithResolver(r ApprovalResolver) AdapterOption {
	return func(c *adapterConfig) { c.resolver = r }
}

// WithAsyncApproval enables the async approval path (7b2 D25). When enabled,
// the confirmer emits ApprovalRequested, parks the session in
// awaiting_approval via TurnInput.ParkApproval, and blocks until
// RespondToApproval or ctx cancellation. The TUI session uses this; the
// headless path does not (keeps sync inline resolution for parity).
func WithAsyncApproval() AdapterOption {
	return func(c *adapterConfig) { c.asyncApproval = true }
}

// WithCoordinator sets the transition coordinator that serializes turn starts
// against session transitions (LoadSession, InitNewSession, Compact). The
// daemon path sets this; the headless path does not (single turn, no
// transitions).
func WithCoordinator(coord *TransitionCoordinator) AdapterOption {
	return func(c *adapterConfig) { c.coordinator = coord }
}

// WithPlanAutoAdvance enables the headless --plan after-turn resolver (7c).
// Only the plan-mode headless driver sets it. noOracle feeds the legacy
// review-skip reason string.
func WithPlanAutoAdvance(noOracle bool) AdapterOption {
	return func(c *adapterConfig) {
		c.planAutoAdvance = true
		c.noOracle = noOracle
	}
}

// appOwners tracks which *agent.App instances are claimed by a HostTurnFunc, so
// a second claim fails loudly. Package-global because the constraint is
// process-wide.
//
// 7b1: the claim is now RELEASABLE. Rotation (/new, /resume, /handoff) builds
// a FRESH *agent.App (a new pointer) for the new conversation — the fresh
// pointer does not collide with the old one in this map. Release is still
// needed: it signals "this App is no longer in use" so the wiring facade can
// safely tear it down (stop async ops, close resources), and it makes the
// claim lifecycle testable (claim → release → the map is empty). Without
// Release, a leaked hostTurn would hold a stale App pointer in the map
// forever, and the wiring layer would have no explicit "done" signal.
//
// Release is rejected while a turn is still active (turnActive) — the caller
// must ensure the session is idle/closed before releasing.
var (
	appOwnersMu sync.Mutex
	appOwners   = map[*agent.App]*hostTurn{}
)

// ErrAppInUse is returned when the same *agent.App is used to build more than
// one HostTurnFunc (single-App constraint), and when one HostTurnFunc's TurnFunc
// is used to drive more than one host session (single-session constraint).
var ErrAppInUse = errors.New("wiring: agent.App already owned by another host session")

// ErrTurnActive is returned by Release when the hostTurn's TurnFunc is still
// executing a turn. The caller must ensure the session is idle or closed
// before releasing — releasing an in-flight turn would leave the agent loop
// running against a freed App.
var ErrTurnActive = errors.New("wiring: cannot release App while a turn is active")

// hostTurn is the runtime binding of an App to its one host session.
type hostTurn struct {
	app             *agent.App
	resolver        ApprovalResolver
	asyncApproval   bool // 7b2: use ParkApproval instead of inline resolver
	planAutoAdvance bool // 7c: headless --plan after-turn resolver
	noOracle        bool // 7c: legacy review-skip reason parity
	coordinator     *TransitionCoordinator // serializes turn-start vs transitions

	// declineLatch captures the reason of the first declined approval
	// resolution this session (7c). It is the CONTROL function the legacy
	// *declinedReason pointer provided: the after-turn resolver checks it
	// before running any workflow transition, so a declined approval (tool
	// OR oracle confirm) terminates the workflow exactly like legacy checked
	// declinedReason post-Send. Mutex-protected: the confirmer runs on the
	// turn goroutine or a worker racing the turn loop.
	declineMu       sync.Mutex
	declineReason   string
	declineOccurred bool

	// sessionEmit is the session-scoped emitter, captured on the first turn.
	// It persists across turns so detached work (async jobs, side questions)
	// emitting between turns reaches the session-scoped surface, not the
	// stale main.go-installed sink. D24: "out-of-turn events are legal until
	// session close."
	sessionEmit sessionhost.SessionEmitter

	// turnEmit is the CURRENT turn's turn-scoped emitter (7b3 m4). Set at the
	// start of each turn and cleared when the turn ends; read by the
	// EventSink closure to route tool-call messages (turn-scoped kinds —
	// the session emitter rejects them by design, D24) to the live turn's
	// emitter. nil between turns: tool calls only execute inside turns, so
	// a nil turnEmit means there is nothing legal to emit.
	//
	// turnEmitTurnID is the turn ID belonging to turnEmit. The session-
	// scoped EventSink closure is installed ONCE (first turn) but must
	// stamp tool events with the CURRENT turn's ID — without this field the
	// closure would capture the first turn's in.TurnID forever and stamp
	// every later turn's tool events with the wrong turn (m4b review
	// finding). Both fields are written under mu at turn start/end and read
	// under the same lock by the closure — EventSink can be invoked from
	// tool-executing goroutines, so the unlocked read was a data race.
	turnEmit       sessionhost.Emitter
	turnEmitTurnID event.TurnID

	mu         sync.Mutex
	sessionID  event.SessionID // first session this turn served; zero until first use
	turnActive bool            // true while the TurnFunc is executing a turn
	released   bool            // true after Release; prevents re-entry
}

// recordDecline latches a declined approval reason (7c control latch).
// LAST-wins: legacy overwrote *declinedReason on every decline
// (workflow_legacy.go headlessConfirmer) and checked it post-turn — parity.
// Mutex-protected: the confirmer can run on worker goroutines racing the
// turn loop.
func (ht *hostTurn) recordDecline(reason string) {
	ht.declineMu.Lock()
	ht.declineOccurred = true
	ht.declineReason = reason
	ht.declineMu.Unlock()
}

// takeDecline returns the latched decline (reason, true), or ("", false).
func (ht *hostTurn) takeDecline() (string, bool) {
	ht.declineMu.Lock()
	defer ht.declineMu.Unlock()
	return ht.declineReason, ht.declineOccurred
}

// HostTurnHandle bundles a TurnFunc with its Release method (7b1). The factory
// (and eventually the facade) uses Release to free the App ownership claim when
// a conversation rotates (/new, /resume, /handoff), so a fresh App can be claimed
// for the new conversation.
type HostTurnHandle struct {
	Turn sessionhost.TurnFunc
	app  *agent.App
	ht   *hostTurn
}

// Release frees the App ownership claim so the wiring facade can tear down the
// old App (stop async ops, close resources) when a conversation rotates. Returns
// ErrTurnActive if a turn is still in flight — the caller must ensure the session
// is idle or closed first. Idempotent.
//
// Note: rotation builds a FRESH *agent.App (new pointer) for the new
// conversation — the fresh pointer does not collide in appOwners. Release is
// the explicit "this App is done" signal for the old one; it does NOT free the
// App or close the host (that is the facade's job in 7b3). It only removes the
// ownership claim so the wiring layer knows the App is no longer in use.
func (h *HostTurnHandle) Release() error { return h.ht.Release() }

// App returns the bound *agent.App. Used by the factory/facade to access the
// App for snapshot construction and direct reads (until 7b3 removes those).
func (h *HostTurnHandle) App() *agent.App { return h.app }

// ResetSessionBinding clears the session binding so the same App can serve
// a new session after the previous one closes. Used by the daemon path where
// one agent.App serves multiple sequential sessions across TUI reconnections.
// Returns ErrTurnActive if a turn is still in flight.
func (h *HostTurnHandle) ResetSessionBinding() error { return h.ht.ResetSessionBinding() }

// HostTurnFunc returns a sessionhost.TurnFunc that drives the real agent loop
// through app. See the package doc for the single-session binding contract.
//
// The returned TurnFunc is bound to the FIRST session it serves: submitting to
// a second, distinct session returns an error wrapping sessionhost.ErrInternal
// (so the session parks in error with internal_error, not backend_failure),
// rather than running a second turn over the same mutable App.
//
// Note: this function does NOT return a Release handle — the App claim is
// permanent for the process lifetime when called through this entry point.
// Callers that need rotation (the TUI path) should use NewHostTurnHandle, which
// returns a HostTurnHandle with a Release method.
func HostTurnFunc(app *agent.App, opts ...AdapterOption) (sessionhost.TurnFunc, error) {
	h, err := NewHostTurnHandle(app, opts...)
	if err != nil {
		return nil, err
	}
	return h.Turn, nil
}

// NewHostTurnHandle builds a HostTurnHandle (7b1) — the TurnFunc plus a Release
// method that frees the App ownership claim on rotation. The factory uses this;
// the headless path continues to use HostTurnFunc (no release needed — the
// process exits after one turn).
func NewHostTurnHandle(app *agent.App, opts ...AdapterOption) (*HostTurnHandle, error) {
	if app == nil {
		return nil, fmt.Errorf("wiring: nil agent.App")
	}
	c := adapterConfig{}
	for _, o := range opts {
		o(&c)
	}
	ht := &hostTurn{app: app, resolver: c.resolver, asyncApproval: c.asyncApproval, planAutoAdvance: c.planAutoAdvance, noOracle: c.noOracle, coordinator: c.coordinator}

	appOwnersMu.Lock()
	if _, claimed := appOwners[app]; claimed {
		appOwnersMu.Unlock()
		return nil, ErrAppInUse
	}
	appOwners[app] = ht
	appOwnersMu.Unlock()

	return &HostTurnHandle{Turn: ht.run, app: app, ht: ht}, nil
}

// Release frees the App ownership claim so a fresh App can be built for a new
// conversation (7b1: the appOwners release path). It returns ErrTurnActive if
// the TurnFunc is currently executing a turn — the caller must ensure the
// session is idle or closed first (e.g. by calling CloseSession and waiting
// for the TurnCompleted event). After Release, the TurnFunc must not be used
// again (the host session that owns it should be closed).
//
// Release is idempotent: calling it twice is safe (the second call is a no-op).
func (ht *hostTurn) Release() error {
	ht.mu.Lock()
	if ht.released {
		ht.mu.Unlock()
		return nil
	}
	if ht.turnActive {
		ht.mu.Unlock()
		return ErrTurnActive
	}
	ht.released = true
	ht.mu.Unlock()

	appOwnersMu.Lock()
	delete(appOwners, ht.app)
	appOwnersMu.Unlock()
	return nil
}

// run executes one turn on the bound App.
func (ht *hostTurn) run(ctx context.Context, in sessionhost.TurnInput) (text string, retErr error) {
	// Turn-start: claim + activate atomically under the coordinator lock so
	// no transition (LoadSession, InitNewSession) interleaves. If the
	// coordinator is nil (headless path), run without it — single turn, no
	// transitions possible.
	if ht.coordinator != nil {
		if err := ht.coordinator.WithTurnStart(func() error {
			return ht.claimAndActivate(in.SessionID)
		}); err != nil {
			return "", err
		}
	} else {
		if err := ht.claimAndActivate(in.SessionID); err != nil {
			return "", err
		}
	}

	defer func() {
		ht.mu.Lock()
		ht.turnActive = false
		ht.mu.Unlock()
		if ht.coordinator != nil {
			ht.coordinator.ClearTurnActive()
		}
	}()

	app := ht.app

	// Capture the session-scoped emitter on the first turn. It persists for
	// the session lifetime so detached work (async jobs, side questions)
	// emitting between turns reaches the session-scoped surface, not the
	// stale main.go-installed sink. D24: "out-of-turn events are legal until
	// session close." Session-scoped callbacks (EventSink, OnTokRate) are
	// installed once and NOT restored per-turn — the per-turn restore was a
	// 7b2 bug that broke detached event delivery between turns.
	if ht.sessionEmit == nil && in.SessionEmit != nil {
			// Install session-scoped callbacks permanently. These use the
			// session-scoped emitter, not the turn-scoped in.Emit.
			//
			// sessionEmit is read under ht.mu in the closures because
			// ResetSessionBinding (daemon path) writes it to nil between
			// sessions, and detached work (D24: legal until session close) can
			// invoke these callbacks concurrently with a reset. Without the
			// lock this is a data race; without the nil-check OnTokRate panics.
			ht.mu.Lock()
			ht.sessionEmit = in.SessionEmit
			ht.mu.Unlock()
			app.SetOnTokRate(func(rate float64) {
				ht.mu.Lock()
				se := ht.sessionEmit
				ht.mu.Unlock()
				if se != nil {
					se.Notify(event.KindTokRate, event.TokRate{Rate: rate})
				}
			})
			app.SetEventSink(func(msg any) {
				// Turn-scoped messages are routed to the live turn's emitter,
				// because the session emitter REJECTS turn-scoped kinds
				// (KindSubagentSpawned, KindSubagentCompleted, tool-call
				// kinds — see turnScopedKinds in host.go). This includes
				// subagent spawn/done events, which are durable and
				// turn-scoped: they must go through the turn emitter while a
				// turn is active. Ephemeral subagent events (progress,
				// active, finished) use Notify, which the session emitter
				// accepts, so they fall through to the session surface.
				//
				// Everything else projects on the session surface (async
				// jobs, notes, …). The current turn's emitter AND its
				// TurnID are read under ht.mu together — the sink can fire
				// from tool goroutines concurrently with a turn boundary,
				// and the pair must stay consistent.
				switch msg.(type) {
				case agent.ToolStartMsg, agent.ToolResultMsg,
					agent.SubagentStartMsg, agent.SubagentDoneMsg:
					ht.mu.Lock()
					te, teTurn := ht.turnEmit, ht.turnEmitTurnID
					ht.mu.Unlock()
					if te != nil {
						projectAgentEvent(te, teTurn, msg)
					}
					return
				}
				ht.mu.Lock()
				se := ht.sessionEmit
				ht.mu.Unlock()
				projectAgentEvent(se, "", msg)
			})
	}

	// Only Out, Confirm, and OnReasoning are turn-scoped — they use the
	// turn-scoped in.Emit, which is fenced at turn completion. Snapshot and
	// restore these per-turn. Session-scoped callbacks (EventSink, OnTokRate)
	// are NOT restored — they persist for the session lifetime (above).
	oldOut, oldConfirm, oldOnReasoning := app.Out, app.Confirm, app.OnReasoning
	defer func() {
		app.Out, app.Confirm, app.OnReasoning = oldOut, oldConfirm, oldOnReasoning
	}()

	// Shared emit-failure latch: the confirmer (often on a worker goroutine)
	// records durable-append failures here; the turn loop surfaces the first as
	// the turn's error → SessionError{internal_error}.
	emitErr := &errorLatch{}

	// Install turn-scoped callbacks.
	app.Out = agent.NewProgWriter(func(m agent.StreamChunkMsg) {
		in.Emit.Notify(event.KindMessageDelta, event.MessageDelta{Text: m.Text})
	})
	app.OnReasoning = func(s string) {
		in.Emit.Notify(event.KindReasoningDelta, event.ReasoningDelta{Text: s})
	}
	app.Confirm = newHostConfirmer(ctx, app, in, ht.resolver, ht.asyncApproval, emitErr, ht)

	// Publish the turn-scoped emitter for the EventSink's tool-call routing
	// (above) and clear it when the turn ends — the emitter is fenced at turn
	// finalization anyway; keeping a stale reference would only mask that.
	ht.mu.Lock()
	ht.turnEmit = in.Emit
	ht.turnEmitTurnID = in.TurnID
	ht.mu.Unlock()
	defer func() {
		ht.mu.Lock()
		ht.turnEmit = nil
		ht.turnEmitTurnID = ""
		ht.mu.Unlock()
	}()

	// Per-turn resets the TUI path performs before the turn (tui_cmds.go RunTurn).
	app.ToolCache = nil
	app.WorkflowStepTrace = nil
	app.Client.ResetGrounding()

	// Drive the turn through the agent's resilience entrypoint (retry transient
	// stream errors, recover empty completions) and return the authoritative
	// final text for the host's MessageCommitted.
	out, err := agent.DriveTurnWithResilience(ctx, app, in.Text)
	if err != nil {
		// Classify fatal request errors (4xx) so the host distinguishes them
		// from retryable backend failures (SessionError{request_error} vs
		// {backend_failure}) without importing the proxy package (D12).
		if errors.Is(err, proxy.ErrBackendFatal) {
			return "", fmt.Errorf("%w: %v", sessionhost.ErrBackendFatal, err)
		}
		return "", err
	}
	// Surface any approval-emitter failure as an internal error (the agent loop
	// itself succeeded; the durable audit append did not).
	if err := emitErr.load(); err != nil {
		return "", err
	}

	// Learn-nudge parity (7b3 m4): the old RunTurn Cmd computed the end-of-turn
	// nudge here — (a) the learn-candidate log fired this turn, (b) at least one
	// web/oracle grounding entry was added client-side, (c) this query wasn't
	// nudged already this session. The nudge is an ephemeral event; the TUI
	// renders it as a transient dimmed note. TakeLearnNudge replicates the old
	// RunTurn computation (and clears the pending state) in one call.
	nudge := app.TakeLearnNudge()

	// Workflow continuation (7b3 m4, plan decision "host enqueues"; 7c adds
	// the headless-plan waiting-state resolver and makes enqueue rejection
	// terminal in plan mode): the old TUI path ran HandleWorkflowTransition
	// inside the RunTurn Cmd and the TUI answered WFStartTurnMsg by starting
	// the next turn. Here the adapter runs the transition after the turn and,
	// when the engine wants a follow-up, enqueues the continuation through the
	// host — the TUI is passive. The host emits this turn's TurnCompleted with
	// WorkflowWillContinue=true (it sees the queued input) and the executor
	// starts the continuation without an idle gap.
	//
	// HandleWorkflowTransition may itself drive oracle calls through the confirm
	// gate (app.Confirm is the turn-scoped hostConfirmer installed above) and
	// may run HandleFinalReview inline for verify-state remediation — matching
	// the old RunTurn semantics where it ran in the same goroutine before the
	// done signal.
	//
	// resolvePlanAfterTurn implements the 7c invariant (Mashura op-34): every
	// completed plan-mode turn yields exactly ONE of {terminal WorkflowOutcome
	// event, one queued continuation, error}. A live workflow never becomes
	// silently idle. In non-plan mode the behavior is unchanged from 7b3 m4.
	if err := ht.resolveAfterTurn(ctx, app, in); err != nil {
		return "", err
	}

	// Emit the learn nudge (if any) as an ephemeral event. Runs after the
	// workflow continuation so ordering matches the old RunTurn, which sent
	// the nudge inside AgentDoneMsg before any WFStartTurnMsg.
	if nudge != "" && ht.sessionEmit != nil {
		ht.sessionEmit.Notify(event.KindLearnNudge, event.LearnNudge{Text: nudge})
	}
	return out.Text, nil
}

// IsTurnActive returns true if a turn is currently in flight on this hostTurn.
// Thread-safe (read-only under ht.mu). Used by RPC handlers (e.g. Compact)
// to check whether idle maintenance is safe. This is a non-blocking probe —
// it does not acquire or modify any transition state.
func (ht *hostTurn) IsTurnActive() bool {
	ht.mu.Lock()
	active := ht.turnActive
	ht.mu.Unlock()
	return active
}

// claimAndActivate claims the session and sets turnActive under ht.mu. This
// runs inside the coordinator lock (if configured) so no transition
// interleaves with the claim + activate.
func (ht *hostTurn) claimAndActivate(sid event.SessionID) error {
	if err := ht.claimSession(sid); err != nil {
		return err
	}
	ht.mu.Lock()
	defer ht.mu.Unlock()
	if ht.released {
		return fmt.Errorf("%w: agent.App released before turn completed", sessionhost.ErrInternal)
	}
	if ht.turnActive {
		return fmt.Errorf("%w: hostTurn already has an active turn", sessionhost.ErrInternal)
	}
	ht.turnActive = true
	return nil
}

// claimSession binds the App to a single session ID and rejects a second.
// For the daemon path, ResetSessionBinding clears the binding so the same
// App can serve a new session after the old one closes (card #149).
func (ht *hostTurn) claimSession(sid event.SessionID) error {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	if ht.sessionID == "" {
		ht.sessionID = sid
		return nil
	}
	if ht.sessionID != sid {
		return fmt.Errorf("%w: agent.App session %s reused for %s", sessionhost.ErrInternal, ht.sessionID, sid)
	}
	return nil
}

// ResetSessionBinding clears the session binding so the same App can serve
// a new session after the previous one closes. This is used by the daemon
// path where one agent.App serves multiple sequential sessions across TUI
// reconnections.
//
// Safe to call only when no turn is active — the caller (SessionStateHandler)
// must ensure the previous session is closed and detached work has quiesced
// before calling. Returns ErrTurnActive if a turn is still in flight.
//
// Reset order (Mashūra-reviewed): hostTurn-internal callback state
// (sessionEmit, turnEmit) is cleared first under ht.mu, so late callback
// invocations find nil emitters and no-op. Then App callback fields
// (EventSink, OnTokRate) are cleared OUTSIDE ht.mu via ClearCallbacks
// (under callbackMu) — this avoids holding hostTurn.mu while touching
// App-owned fields (lock-order invariant: hostTurn.mu never nests App locks).
// The decline latch is cleared under its own lock. sessionID is cleared
// last, re-arming the handle for a new claim.
func (ht *hostTurn) ResetSessionBinding() error {
	ht.mu.Lock()
	if ht.turnActive {
		ht.mu.Unlock()
		return ErrTurnActive
	}
	if ht.released {
		ht.mu.Unlock()
		return fmt.Errorf("wiring: cannot reset a released hostTurn")
	}
	// Detach hostTurn-internal callback state BEFORE clearing App callbacks —
	// a late call from quiescing detached work would nil-deref otherwise.
	// projectAgentEvent is nil-safe, but OnTokRate calls se.Notify directly.
	ht.sessionEmit = nil
	ht.turnEmit = nil
	ht.turnEmitTurnID = ""
	ht.mu.Unlock()

	// Clear App callback fields OUTSIDE hostTurn.mu — these are App-owned
	// fields protected by callbackMu, not hostTurn.mu. Holding hostTurn.mu
	// while writing App fields violates the lock-ordering invariant
	// (hostTurn.mu is never held while touching App locks).
	ht.app.ClearCallbacks()

	// Clear the decline latch under its own lock.
	ht.declineMu.Lock()
	ht.declineReason = ""
	ht.declineOccurred = false
	ht.declineMu.Unlock()

	// Clear sessionID last — re-arms claimSession for the new session.
	ht.mu.Lock()
	ht.sessionID = ""
	ht.mu.Unlock()
	return nil
}

// errorLatch is a thread-safe first-error latch shared between the confirmer
// (worker context) and the turn loop.
type errorLatch struct {
	mu  sync.Mutex
	err error
}

func (l *errorLatch) set(err error) {
	if err == nil {
		return
	}
	l.mu.Lock()
	if l.err == nil {
		l.err = err
	}
	l.mu.Unlock()
}

func (l *errorLatch) load() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

// newHostConfirmer is the D5/D25 approval shim: a context-aware agent.Confirmer
// that evaluates policy (parity with tuiConfirmer), then emits
// ApprovalRequested (durable) before resolving and ApprovalResolved (durable)
// after, with full approve/decline/allow-reads outcome fidelity.
//
// Two modes (7b2 D25):
//   - Sync (headless, asyncApproval=false): the inline resolver runs in a
//     goroutine and is raced against ctx cancellation. This is the chunk-5/7
//     behavior — unchanged for headless parity.
//   - Async (TUI, asyncApproval=true): the confirmer parks the session in
//     awaiting_approval via in.ParkApproval and blocks until RespondToApproval
//     or ctx cancellation. The TUI loop does NOT block.
//
// A durable Append failure is recorded on emitErr — surfaced as the turn's
// error → internal_error by the turn loop — so an unresolved or orphaned
// approval is never silently half-committed.
func newHostConfirmer(ctx context.Context, app *agent.App, in sessionhost.TurnInput, resolver ApprovalResolver, asyncApproval bool, emitErr *errorLatch, ht *hostTurn) agent.Confirmer {
	return func(toolName, headline, detail string, readAction bool) bool {
		// ── Policy evaluation (parity with tuiConfirmer) ──────────────────
		// If a policy is active, evaluate it first. Policy allow behaves
		// like AutoApprove — but SuspendAuto carve-outs (egress, destructive)
		// still fire on top. Policy deny blocks the call. Policy ask falls
		// through to the existing confirm path. This was missing from the
		// hostConfirmer (Mashūra-found: the comment claimed "mirrors
		// tuiConfirmer exactly" but the policy block was absent).
		if pol := app.Policy(); pol != nil {
			input := agent.BuildPolicyInput(toolName, detail, readAction)
			result := pol.Evaluate(input)
			switch result.Decision {
			case policy.Deny:
				app.SendEvent(agent.SysNoteMsg{
					Text: "🚫 blocked by policy: " + result.Reason + " (rule: " + result.RuleName + ")",
				})
				return false
			case policy.Allow:
				reason := agent.SuspendAuto(toolName, app, detail)
				if reason == "" {
					app.SendEvent(agent.SysNoteMsg{
						Text: "⚡ policy allow: " + headline + "\n" + agent.Indent(detail),
					})
					return true
				}
				// Auto suspended — the policy said allow, but a hard safety
				// gate requires confirmation. Fall through to the prompt.
				headline = "⚡ auto suspended: " + reason + " — " + headline
			case policy.Ask:
				// Policy says ask — fall through to AutoApprove / prompt.
			}
		}

		// ── AutoApprove short-circuit (parity with tuiConfirmer) ──────────
		// When /auto is on and SuspendAuto does not carve out this tool,
		// auto-approve without parking. No ApprovalRequested/ApprovalResolved
		// — the call is auto-approved, not user-approved.
		if app.Consent().AutoApprove {
			reason := agent.SuspendAuto(toolName, app, detail)
			if reason == "" {
				// Emit the ⚡ auto note through the session-scoped EventSink
				// (same path as tuiConfirmer's sendEvent — the adapter's
				// EventSink projects SysNoteMsg to KindSessionNote).
				app.SendEvent(agent.SysNoteMsg{
					Text: "⚡ auto: " + headline + "\n" + agent.Indent(detail),
				})
				return true
			}
			// Auto suspended — prefix the headline so the approval prompt
			// states the cause (same convention as tuiConfirmer).
			headline = "⚡ auto suspended: " + reason + " — " + headline
		}

		approvalID, err := id.NewApprovalID()
		if err != nil {
			// Cannot mint a valid ID: fail closed AND record it — an approval
			// that produced no durable audit trail must surface as an internal
			// error, not silently look like a benign decline (op-35 finding).
			emitErr.set(fmt.Errorf("%w: approval ID generation failed: %v", sessionhost.ErrInternal, err))
			return false
		}
		req := ApprovalRequest{ToolName: toolName, Headline: headline, Detail: detail, ReadAction: readAction}

		if err := in.Emit.Emit(event.KindApprovalRequested, event.ApprovalRequested{
			ApprovalID: approvalID,
			ToolName:   toolName,
			Headline:   headline,
			Detail:     detail,
			ReadAction: readAction,
		}); err != nil {
			emitErr.set(err)
			return false // no Requested → no Resolved; fail closed
		}

		// Resolve: sync (inline resolver) or async (park + RespondToApproval).
		var res ApprovalResolution
		var resolverID event.UserID // who actually answered (D4 identity)
		if asyncApproval && in.ParkApproval != nil {
			// Async path (7b2 D25): park the session and block until
			// RespondToApproval or ctx cancellation.
			outcome, reason, rid := in.ParkApproval(ctx, approvalID)
			resolverID = rid
			if resolverID == "" {
				// Forced decline (ctx cancellation or no responder): record the
				// system principal, NOT the submitter (D25: cancel-during-approval
				// records the interrupt principal, not the user who submitted).
				resolverID = event.SystemUserID
			}
			switch outcome {
			case "approved":
				res = ApprovalResolution{Choice: agent.ChoiceApprove}
			case "allowed_reads":
				res = ApprovalResolution{Choice: agent.ChoiceAllowReads}
			default:
				res = ApprovalResolution{Choice: agent.ChoiceDecline, Reason: reason}
			}
		} else {
			// Sync path (headless parity): inline resolver raced against ctx.
			res = resolveApproval(ctx, resolver, req)
			resolverID = in.UserID // sync mode: submitter is the resolver
		}

		var outcome string
		var proceed bool
		switch res.Choice {
		case agent.ChoiceAllowReads:
			outcome, proceed = "allowed_reads", true
		case agent.ChoiceApprove:
			outcome, proceed = "approved", true
		default:
			outcome, proceed = "declined", false
		}
		// Invariant (D18): a declined resolution from this adapter carries a
		// non-empty Reason; approved/allowed-reads carry an empty Reason.
		reason := res.Reason
		if outcome == "declined" && reason == "" {
			reason = "declined"
		}

		if err := in.Emit.Emit(event.KindApprovalResolved, event.ApprovalResolved{
			ApprovalID: approvalID,
			Outcome:    outcome,
			Reason:     reason,
			Resolver:   resolverID,
		}); err != nil {
			emitErr.set(err)
			// Fail closed: a failed durable resolution audit must not permit
			// tool execution. The turn will fail with internal_error.
			return false
		}
		// Apply consent mutations ONLY after the durable audit succeeds — if the
		// emit fails, consent is not mutated (avoids a partial state where the
		// turn fails but AllowReads was already granted). The durable record is
		// the authoritative audit; consent mutation follows it, not precedes it.
		if res.Choice == agent.ChoiceAllowReads {
			app.SetAllowReads(true)
		}
		// 7c control latch: record the first explicit decline so the after-turn
		// workflow resolver terminates the workflow instead of continuing
		// (parity with the legacy *declinedReason check post-Send). Cancellation
		// declines ("cancelled") are NOT latched as user declines — a cancelled
		// turn terminates through the cancelled outcome, not a decline.
		if outcome == "declined" && reason != "cancelled" && ht != nil {
			ht.recordDecline(reason)
		}
		return proceed
	}
}

// resolveAfterTurn is the 7c after-turn workflow region. It subsumes the 7b3
// continuation block and adds the headless-plan waiting-state resolver.
//
// Non-plan mode (planAutoAdvance=false): behavior is IDENTICAL to the 7b3 m4
// block — transition-returned continuations are enqueued with an audit
// marker; enqueue rejection silently drops (the interactive user can drive
// the workflow with /plan or a message).
//
// Plan mode (planAutoAdvance=true, headless --plan only): additionally
// resolves waiting states (transition returned nil with a live workflow) by
// applying the legacy headless auto-advance policy, and makes every path
// terminal-or-continuing — a live workflow never becomes silently idle, and
// enqueue rejection is an error, not a silent drop (no user exists to resume).
func (ht *hostTurn) resolveAfterTurn(ctx context.Context, app *agent.App, in sessionhost.TurnInput) error {
	if app.Workflow == nil {
		return nil // no workflow: nothing to do (single-task path)
	}

	// Plan-mode decline check BEFORE any transition (legacy checked
	// declinedReason post-Send pre-transition). A declined approval — tool or
	// oracle confirm — terminates the workflow here.
	if ht.planAutoAdvance {
		if reason, declined := ht.takeDecline(); declined {
			return ht.emitOutcome(in, "declined", reason)
		}
	}

	if in.EnqueueInput == nil {
		// Lifecycle-only tests: continuation impossible. In plan mode this
		// must not silently idle (invariant), but there is also no way to
		// continue — surface as internal error. Non-plan mode: no-op.
		if ht.planAutoAdvance {
			return fmt.Errorf("%w: plan-mode turn has no EnqueueInput hook", sessionhost.ErrInternal)
		}
		return nil
	}

	wfNext := agent.HandleWorkflowTransition(ctx, app)

	// Final review passed inside the transition: workflow cleared → the turn
	// completes normally and the consumer's TurnCompleted terminal (D4.4)
	// yields pass. The final-review marker is emitted for the audit trail.
	// PLAN-MODE ONLY: the 7b3 TUI path never emitted this marker; emitting it
	// unconditionally would change the TUI's durable event stream (op-35
	// review finding).
	if app.Workflow == nil {
		if ht.planAutoAdvance {
			return ht.emitFinalReviewMarker(in)
		}
		return nil
	}

	if wfNext != nil {
		// A decline latched DURING the transition (e.g. the oracle confirm the
		// transition drove was declined) must terminate the workflow NOW —
		// enqueueing the continuation would burn one extra LLM turn before the
		// next turn's pre-transition check fires (op-35 finding).
		if ht.planAutoAdvance {
			if reason, declined := ht.takeDecline(); declined {
				return ht.emitOutcome(in, "declined", reason)
			}
		}
		// Engine-requested continuation: enqueue + audit marker on success.
		if err := in.EnqueueInput(wfNext.UserText); err != nil {
			if ht.planAutoAdvance {
				return fmt.Errorf("%w: workflow continuation enqueue failed: %v", sessionhost.ErrInternal, err)
			}
			return nil // legacy 7b3 drop semantics (interactive user resumes)
		}
		_ = in.SessionEmit.Emit(event.KindWorkflowTurnStarted, event.WorkflowTurnStarted{
			TurnID:   in.TurnID,
			UserText: wfNext.UserText,
		})
		return nil
	}

	// wfNext == nil, workflow alive: a waiting state. Non-plan mode leaves it
	// to the interactive user (TUI shows /plan gates) — unchanged from 7b3.
	if !ht.planAutoAdvance {
		return nil
	}
	return ht.resolveWaitingState(ctx, app, in)
}

// resolveWaitingState applies the legacy headless auto-advance policy
// (workflow_legacy.go:143-217) to a paused workflow. Exactly one of:
// enqueue a continuation, emit a terminal WorkflowOutcome, or error.
func (ht *hostTurn) resolveWaitingState(ctx context.Context, app *agent.App, in sessionhost.TurnInput) error {
	wf := app.Workflow

	// A decline may also have been latched by an oracle confirm DURING the
	// transition (the transition drove the oracle; the confirm gate declined;
	// the engine treated it as oracle-unavailable and paused).
	if reason, declined := ht.takeDecline(); declined {
		return ht.emitOutcome(in, "declined", reason)
	}

	switch wf.Phase {
	case workflow.WFPresent:
		// Legacy 145-147: auto-approve — advance to implementation step 1.
		wf.Phase = workflow.WFImplement
		wf.StepIdx = 1
		return ht.enqueueContinue(in)

	case workflow.WFReview:
		// Legacy 149-160: force-skip the review with a logged reason.
		var reason, logReason string
		if ht.noOracle {
			reason = "oracle disabled by flag (--no-oracle)"
			logReason = "oracle disabled by --no-oracle flag"
		} else {
			reason = "oracle review unavailable — " + wf.ReviewSkipReason
			logReason = "headless: oracle unavailable"
		}
		if ht.sessionEmit != nil {
			ht.sessionEmit.Notify(event.KindWorkflowWarning, event.WorkflowWarning{Message: reason})
		}
		agent.WFWriteReviewSkipForce(app, logReason)
		wf.Phase = workflow.WFPresent
		return ht.enqueueContinue(in)

	case workflow.WFImplement:
		if wf.StepIdx > wf.StepCount {
			// Final review already ran inside the transition (engine:88-95
			// remediation / 150-154 last-step paths) and left the workflow
			// open: map its outcome. Legacy 163-185.
			return ht.mapFinalReviewOutcome(in, wf)
		}
		// Legacy 186: advance to the next step.
		wf.StepIdx++
		if wf.StepIdx > wf.StepCount {
			// Legacy 186-209 (the no-marker crossing): the resolver itself
			// runs the final review once and maps its outcome.
			agent.HandleFinalReview(ctx, app)
			if app.Workflow == nil {
				// Review passed/cleared → workflow done → pass terminal.
				return ht.emitFinalReviewMarker(in)
			}
			return ht.mapFinalReviewOutcome(in, wf)
		}
		return ht.enqueueContinue(in)

	default:
		// WFPlan format-retry pauses are handled INSIDE the transition (the
		// engine re-drives the format directive on the next turn) — reaching
		// here in WFPlan/WFGather means an unexpected state; legacy 211-216
		// errored loudly. Keep that.
		return fmt.Errorf("%w: unexpected waiting state: %v", sessionhost.ErrInternal, wf.PhaseName())
	}
}

// enqueueContinue enqueues the legacy "continue" input with the audit marker.
func (ht *hostTurn) enqueueContinue(in sessionhost.TurnInput) error {
	if err := in.EnqueueInput("continue"); err != nil {
		return fmt.Errorf("%w: workflow continuation enqueue failed: %v", sessionhost.ErrInternal, err)
	}
	_ = in.SessionEmit.Emit(event.KindWorkflowTurnStarted, event.WorkflowTurnStarted{
		TurnID:   in.TurnID,
		UserText: "continue",
	})
	return nil
}

// mapFinalReviewOutcome maps the post-final-review workflow state to the
// legacy terminal outcomes (workflow_legacy.go:163-209). The workflow is
// still open (nil Workflow means pass — handled by callers).
func (ht *hostTurn) mapFinalReviewOutcome(in sessionhost.TurnInput, wf *workflow.WorkflowState) error {
	if err := ht.emitFinalReviewMarker(in); err != nil {
		return err
	}
	if reason, declined := ht.takeDecline(); declined || wf.VerifyDeclined {
		r := reason
		if !declined || r == "" {
			r = "verification command declined by consent gate"
		}
		return ht.emitOutcome(in, "declined", r)
	}
	if wf.VerifyFailed {
		return ht.emitOutcome(in, "verify_failed", "")
	}
	return ht.emitOutcome(in, "gaps", "")
}

// emitOutcome emits the durable terminal WorkflowOutcome. The consumer keys
// off this event; a failed append must therefore fail the turn (internal
// error), never pass silently.
func (ht *hostTurn) emitOutcome(in sessionhost.TurnInput, outcome, reason string) error {
	return in.SessionEmit.Emit(event.KindWorkflowOutcome, event.WorkflowOutcome{
		TurnID:  in.TurnID,
		Outcome: outcome,
		Reason:  reason,
	})
}

// emitFinalReviewMarker emits the durable final-review audit marker. A failed
// append is an internal error (audit markers are load-bearing for replay).
func (ht *hostTurn) emitFinalReviewMarker(in sessionhost.TurnInput) error {
	return in.SessionEmit.Emit(event.KindWorkflowFinalReview, event.WorkflowFinalReview{
		TurnID: in.TurnID,
	})
}

// resolveApproval runs the resolver (if any) in a goroutine and selects
// between its result and ctx cancellation. Cancellation (or a nil resolver)
// yields a decline with a fixed reason. A resolver that returns after
// cancellation has won is discarded.
func resolveApproval(ctx context.Context, resolver ApprovalResolver, req ApprovalRequest) ApprovalResolution {
	declined := func(reason string) ApprovalResolution {
		return ApprovalResolution{Choice: agent.ChoiceDecline, Reason: reason}
	}
	if resolver == nil {
		return declined("no resolver configured")
	}
	if ctx.Err() != nil {
		return declined("cancelled")
	}
	ch := make(chan ApprovalResolution, 1)
	go func() {
		ch <- resolver(ctx, req)
	}()
	select {
	case res := <-ch:
		if ctx.Err() != nil {
			return declined("cancelled") // cancellation won while resolver ran
		}
		return res
	case <-ctx.Done():
		return declined("cancelled")
	}
}
