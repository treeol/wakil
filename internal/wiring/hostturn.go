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
	"github.com/treeol/wakil/internal/proxy"
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
	resolver      ApprovalResolver
	// asyncApproval enables the async approval path (7b2 D25). When true,
	// the confirmer parks the session in awaiting_approval and blocks on
	// RespondToApproval instead of using the inline resolver. The TUI session
	// uses this; the headless path does not (keeps sync inline resolution).
	asyncApproval bool
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
	app           *agent.App
	resolver      ApprovalResolver
	asyncApproval bool // 7b2: use ParkApproval instead of inline resolver

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
	turnActive bool           // true while the TurnFunc is executing a turn
	released   bool           // true after Release; prevents re-entry
}

// HostTurnHandle bundles a TurnFunc with its Release method (7b1). The factory
// (and eventually the facade) uses Release to free the App ownership claim when
// a conversation rotates (/new, /resume, /handoff), so a fresh App can be claimed
// for the new conversation.
type HostTurnHandle struct {
	Turn   sessionhost.TurnFunc
	app    *agent.App
	ht     *hostTurn
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
	ht := &hostTurn{app: app, resolver: c.resolver, asyncApproval: c.asyncApproval}

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
	if err := ht.claimSession(in.SessionID); err != nil {
		return "", err
	}

	// Mark the turn as active so Release rejects a concurrent rotation (7b1).
	// Also reject a second concurrent turn on the same hostTurn — the
	// single-App-single-session constraint means at most one turn may execute
	// at a time (the host's executor goroutine already serializes turns for
	// one session, but this guard is defense-in-depth).
	// Cleared by defer before run returns, so Release sees a quiet state once
	// the turn goroutine has exited.
	ht.mu.Lock()
	if ht.released {
		ht.mu.Unlock()
		return "", fmt.Errorf("%w: agent.App released before turn completed", sessionhost.ErrInternal)
	}
	if ht.turnActive {
		ht.mu.Unlock()
		return "", fmt.Errorf("%w: hostTurn already has an active turn", sessionhost.ErrInternal)
	}
	ht.turnActive = true
	ht.mu.Unlock()
	defer func() {
		ht.mu.Lock()
		ht.turnActive = false
		ht.mu.Unlock()
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
		ht.sessionEmit = in.SessionEmit
		// Install session-scoped callbacks permanently. These use the
		// session-scoped emitter, not the turn-scoped in.Emit.
		app.OnTokRate = func(rate float64) {
			ht.sessionEmit.Notify(event.KindTokRate, event.TokRate{Rate: rate})
		}
		app.EventSink = func(msg any) {
			// Tool-call messages are turn-scoped kinds (D24): the session
			// emitter REJECTS them, so route them to the live turn's emitter
			// when one exists. Everything else projects on the session
			// surface (subagents, async jobs, notes, …). The current turn's
			// emitter AND its TurnID are read under ht.mu together — the
			// sink can fire from tool goroutines concurrently with a turn
			// boundary, and the pair must stay consistent.
			switch msg.(type) {
			case agent.ToolStartMsg, agent.ToolResultMsg:
				ht.mu.Lock()
				te, teTurn := ht.turnEmit, ht.turnEmitTurnID
				ht.mu.Unlock()
				if te != nil {
					projectAgentEvent(te, teTurn, msg)
				}
				return
			}
			projectAgentEvent(ht.sessionEmit, "", msg)
		}
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
	app.Confirm = newHostConfirmer(ctx, app, in, ht.resolver, ht.asyncApproval, emitErr)

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

	// Workflow continuation (7b3 m4, plan decision "host enqueues"): the old
	// TUI path ran HandleWorkflowTransition inside the RunTurn Cmd and the TUI
	// answered WFStartTurnMsg by starting the next turn. Here the adapter runs
	// the transition after the turn and, when the engine wants a follow-up,
	// emits the durable audit marker and enqueues the continuation through the
	// host — the TUI is passive. The host emits this turn's TurnCompleted with
	// WorkflowWillContinue=true (it sees the queued input) and the executor
	// starts the continuation without an idle gap.
	//
	// HandleWorkflowTransition may itself drive oracle calls through the confirm
	// gate (app.Confirm is the turn-scoped hostConfirmer installed above) and
	// may run HandleFinalReview inline for verify-state remediation — matching
	// the old RunTurn semantics where it ran in the same goroutine before the
	// done signal.
	if app.Workflow != nil && in.EnqueueInput != nil {
		if wfNext := agent.HandleWorkflowTransition(ctx, app); wfNext != nil {
			// Enqueue first, emit the audit marker only on success: a durable
			// workflow_turn_started without a queued continuation would lie to
			// a replaying client. An enqueue rejection (session closing or
			// queue full) drops the continuation silently — the completed turn
			// stands, the workflow pauses at its current phase, and the user
			// can drive it with /plan or a plain message. Rare by construction
			// (requires a full queue mid-workflow or a concurrent close).
			if err := in.EnqueueInput(wfNext.UserText); err == nil {
				_ = in.SessionEmit.Emit(event.KindWorkflowTurnStarted, event.WorkflowTurnStarted{
					TurnID:   in.TurnID,
					UserText: wfNext.UserText,
				})
			}
		}
	}

	// Emit the learn nudge (if any) as an ephemeral event. Runs after the
	// workflow continuation so ordering matches the old RunTurn, which sent
	// the nudge inside AgentDoneMsg before any WFStartTurnMsg.
	if nudge != "" && ht.sessionEmit != nil {
		ht.sessionEmit.Notify(event.KindLearnNudge, event.LearnNudge{Text: nudge})
	}
	return out.Text, nil
}

// claimSession binds the App to a single session ID and rejects a second.
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
// that emits ApprovalRequested (durable) before resolving and ApprovalResolved
// (durable) after, with full approve/decline/allow-reads outcome fidelity.
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
func newHostConfirmer(ctx context.Context, app *agent.App, in sessionhost.TurnInput, resolver ApprovalResolver, asyncApproval bool, emitErr *errorLatch) agent.Confirmer {
	return func(toolName, headline, detail string, readAction bool) bool {
		approvalID, err := id.NewApprovalID()
		if err != nil {
			return false // cannot mint a valid ID: fail closed, no events
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
		return proceed
	}
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
