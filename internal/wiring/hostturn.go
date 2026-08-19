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
//   - the adapter runs a turn via the exported SendOutcome →
//     WaitForAsyncCompletion → Resume loop and returns the authoritative
//     assistant text (TurnOutcome.Text) for the host's MessageCommitted,
//   - stream chunks become MessageDelta (ephemeral), reasoning becomes
//     ReasoningDelta (ephemeral),
//   - approvals go through a context-aware confirmer that emits
//     ApprovalRequested → ApprovalResolved with full outcome fidelity (D5 shim).
//
// Deferred (not hidden): tool-call/subagent durable events (the agent does not
// yet emit them with valid domain IDs/digests/status), per-session App factory
// (deliverable 5), and the async wire approval path (P2). Stream-error retry
// (HandleStreamError) and workflow transitions are also not replicated here: a
// turn failure surfaces as the adapter's error (→ SessionError), and empty → an
// empty committed message.
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

// ApprovalResolver answers one approval request. It is invoked in its own
// goroutine and its result is raced against ctx cancellation: a resolver that
// ignores ctx cannot block the executor (the confirmer forces a decline). Its
// returned value is trusted only if it arrives before cancellation wins the
// race; otherwise the confirmer resolves the approval as declined.
//
// nil means "decline everything" (the safe headless-without-approval default).
type ApprovalResolver func(ctx context.Context, req ApprovalRequest) agent.ConfirmChoice

// adapterConfig holds HostTurnFunc options.
type adapterConfig struct {
	resolver ApprovalResolver
}

// AdapterOption configures the HostTurnFunc adapter.
type AdapterOption func(*adapterConfig)

// WithResolver sets the approval resolver. nil (default) declines everything.
func WithResolver(r ApprovalResolver) AdapterOption {
	return func(c *adapterConfig) { c.resolver = r }
}

// ErrAppInUse is returned when the same *agent.App is used to build more than
// one HostTurnFunc (single-App constraint), and when one HostTurnFunc's TurnFunc
// is used to drive more than one host session (single-session constraint).
var ErrAppInUse = errors.New("wiring: agent.App already owned by another host session")

// appOwners tracks which *agent.App instances are claimed by a HostTurnFunc, so
// a second claim fails loudly. Package-global because the constraint is
// process-wide. The claim is permanent for the process lifetime (there is no
// release path in chunk 5; the adapter and host share a process in embedded
// mode).
var (
	appOwnersMu sync.Mutex
	appOwners   = map[*agent.App]struct{}{}
)

// hostTurn is the runtime binding of an App to its one host session.
type hostTurn struct {
	app      *agent.App
	resolver ApprovalResolver

	mu        sync.Mutex
	sessionID event.SessionID // first session this turn served; zero until first use
}

// HostTurnFunc returns a sessionhost.TurnFunc that drives the real agent loop
// through app. See the package doc for the single-session binding contract.
//
// The returned TurnFunc is bound to the FIRST session it serves: submitting to
// a second, distinct session returns an error wrapping sessionhost.ErrInternal
// (so the session parks in error with internal_error, not backend_failure),
// rather than running a second turn over the same mutable App.
func HostTurnFunc(app *agent.App, opts ...AdapterOption) (sessionhost.TurnFunc, error) {
	if app == nil {
		return nil, fmt.Errorf("wiring: nil agent.App")
	}
	c := adapterConfig{}
	for _, o := range opts {
		o(&c)
	}
	appOwnersMu.Lock()
	if _, claimed := appOwners[app]; claimed {
		appOwnersMu.Unlock()
		return nil, ErrAppInUse
	}
	appOwners[app] = struct{}{}
	appOwnersMu.Unlock()

	ht := &hostTurn{app: app, resolver: c.resolver}
	return ht.run, nil
}

// run executes one turn on the bound App.
func (ht *hostTurn) run(ctx context.Context, in sessionhost.TurnInput) (text string, retErr error) {
	if err := ht.claimSession(in.SessionID); err != nil {
		return "", err
	}
	app := ht.app

	// Snapshot and restore every callback field the adapter owns. Panic-safe
	// via defer (an agent panic must not leave the App in a host-owned state).
	oldOut, oldConfirm := app.Out, app.Confirm
	oldOnReasoning, oldOnTokRate, oldEventSink := app.OnReasoning, app.OnTokRate, app.EventSink
	defer func() {
		app.Out, app.Confirm = oldOut, oldConfirm
		app.OnReasoning, app.OnTokRate, app.EventSink = oldOnReasoning, oldOnTokRate, oldEventSink
	}()

	// Shared emit-failure latch: the confirmer (often on a worker goroutine)
	// records durable-append failures here; the turn loop surfaces the first as
	// the turn's error → SessionError{internal_error}.
	emitErr := &errorLatch{}

	// Install the host-path callbacks.
	app.Out = agent.NewProgWriter(func(m agent.StreamChunkMsg) {
		in.Emit.Notify(event.KindMessageDelta, event.MessageDelta{Text: m.Text})
	})
	app.OnReasoning = func(s string) {
		in.Emit.Notify(event.KindReasoningDelta, event.ReasoningDelta{Text: s})
	}
	// TokRate is a TUI-local display signal with no domain meaning yet; the host
	// path has no sink for it, so it is collected away (see plan §6).
	app.OnTokRate = func(float64) {}
	app.EventSink = func(any) {}
	app.Confirm = newHostConfirmer(ctx, app, in, ht.resolver, emitErr)

	// Per-turn resets the TUI path performs before the turn (tui_cmds.go RunTurn).
	app.ToolCache = nil
	app.WorkflowStepTrace = nil
	app.Client.ResetGrounding()

	out, err := app.SendOutcome(ctx, in.Text)
	if err != nil {
		return "", err
	}
	for out.Kind == agent.TurnSuspended {
		ok, werr := app.WaitForAsyncCompletion(ctx)
		if werr != nil {
			return "", werr
		}
		if !ok {
			return out.Text, nil // nothing pending → treat as final
		}
		out, err = app.Resume(ctx)
		if err != nil {
			return "", err
		}
	}
	// Surface any approval-emitter failure as an internal error (the agent loop
	// itself succeeded; the durable audit append did not).
	if err := emitErr.load(); err != nil {
		return "", err
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

// newHostConfirmer is the D5 approval shim: a context-aware agent.Confirmer
// that emits ApprovalRequested (durable) before resolving and ApprovalResolved
// (durable) after, with full approve/decline/allow-reads outcome fidelity.
//
// The resolver runs in a goroutine and is raced against ctx cancellation, so a
// stuck resolver cannot hang the executor (a cancellation win forces a
// decline). A durable Append failure is recorded on emitErr — surfaced as the
// turn's error → internal_error by the turn loop — so an unresolved or
// orphaned approval is never silently half-committed.
func newHostConfirmer(ctx context.Context, app *agent.App, in sessionhost.TurnInput, resolver ApprovalResolver, emitErr *errorLatch) agent.Confirmer {
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

		// Resolve, racing cancellation against the goroutine-isolated resolver.
		choice := resolveApproval(ctx, resolver, req)

		var outcome string
		var proceed bool
		switch choice {
		case agent.ChoiceAllowReads:
			outcome, proceed = "allowed_reads", true
			app.SetAllowReads(true)
		case agent.ChoiceApprove:
			outcome, proceed = "approved", true
		default:
			outcome, proceed = "declined", false
		}

		if err := in.Emit.Emit(event.KindApprovalResolved, event.ApprovalResolved{
			ApprovalID: approvalID,
			Outcome:    outcome,
			Resolver:   in.UserID,
		}); err != nil {
			emitErr.set(err)
			// A failed Resolved leaves an unresolved durable Requested. Fail the
			// turn so the audit stream reports internal_error rather than
			// pretending the pairing held.
		}
		return proceed
	}
}

// resolveApproval runs the resolver (if any) in a goroutine and selects
// between its result and ctx cancellation. Cancellation (or a nil resolver)
// yields a decline. A resolver that returns after cancellation has won is
// discarded.
func resolveApproval(ctx context.Context, resolver ApprovalResolver, req ApprovalRequest) agent.ConfirmChoice {
	if resolver == nil {
		return agent.ChoiceDecline
	}
	if ctx.Err() != nil {
		return agent.ChoiceDecline
	}
	ch := make(chan agent.ConfirmChoice, 1)
	go func() {
		ch <- resolver(ctx, req)
	}()
	select {
	case c := <-ch:
		if ctx.Err() != nil {
			return agent.ChoiceDecline // cancellation won while resolver ran
		}
		return c
	case <-ctx.Done():
		return agent.ChoiceDecline
	}
}
