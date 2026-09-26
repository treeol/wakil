package agent

import (
	"context"
	"fmt"

	"github.com/treeol/wakil/internal/trace"
)

// ─── Phase 2: Idle/Wake engine ───────────────────────────────────
//
// A turn can END in one of two ways: the model produced a final answer and no
// async work remains (Final), or the model produced final text while async work
// (mashura panel, detached shell, discovery subagents) is still pending and it
// has no further independent tool work (Suspended). The caller — TUI or headless
// run loop — retains the continuation and, on a Suspended outcome, registers a
// waiter and resumes once a completion arrives (wake), instead of spinning on
// check_pending or ending the turn.

// TurnOutcomeKind distinguishes the disposition of one turn invocation. None of
// these kinds means that a broader goal is complete; goal completion is owned by
// the future continuous-mode coordinator.
type TurnOutcomeKind int

const (
	// TurnFinal: the turn produced an ordinary final response. It is only
	// eligible for coordinator evaluation and is not itself proof of success.
	TurnFinal TurnOutcomeKind = iota
	// TurnSuspended: the model produced final text while async work is pending;
	// the caller should await a completion (WaitForAsyncCompletion) and resume.
	TurnSuspended
	// TurnAborted: the turn stopped abnormally, such as a provider failure.
	TurnAborted
	// TurnCancelled: the caller cancelled the turn or its deadline expired.
	TurnCancelled
	// TurnDeclined: a required consent gate was declined; no model request was
	// admitted for this turn.
	TurnDeclined
)

// TurnStopCause is a typed reason a turn ended without an ordinary final
// disposition. Empty means no abnormal cause was recorded for this invocation.
type TurnStopCause string

const (
	StopCauseNone                   TurnStopCause = ""
	StopCauseProvider               TurnStopCause = "provider_error"
	StopCauseCancelled              TurnStopCause = "cancelled"
	StopCauseDeadline               TurnStopCause = "deadline_exceeded"
	StopCauseConsentDeclined        TurnStopCause = "consent_declined"
	StopCauseTurnAdmissionRefused   TurnStopCause = "turn_admission_refused"
	StopCauseSessionBudgetExhausted TurnStopCause = "session_budget_exhausted"
	StopCauseIterationLimit         TurnStopCause = "iteration_limit"
	StopCauseTurnBudgetExhausted    TurnStopCause = "turn_budget_exhausted"
	StopCauseConfinementBreaker     TurnStopCause = "confinement_breaker"
	StopCauseHardMaxShed            TurnStopCause = "hard_max_shed"
	StopCauseAsyncLost              TurnStopCause = "async_completion_lost"
)

func (k TurnOutcomeKind) String() string {
	switch k {
	case TurnFinal:
		return "final"
	case TurnSuspended:
		return "suspended"
	case TurnAborted:
		return "aborted"
	case TurnCancelled:
		return "cancelled"
	case TurnDeclined:
		return "declined"
	default:
		return fmt.Sprintf("unknown(%d)", int(k))
	}
}

func (c TurnStopCause) String() string { return string(c) }

// stopCauseFromLegacy maps the existing internal stop-reason strings to the
// typed public outcome contract. Unknown values are preserved conservatively so
// future causes are visible rather than silently treated as normal completion.
func stopCauseFromLegacy(reason string) TurnStopCause {
	switch reason {
	case "":
		return StopCauseNone
	case "iteration_limit":
		return StopCauseIterationLimit
	case "turn_budget_exhausted":
		return StopCauseTurnBudgetExhausted
	case "confinement_breaker":
		return StopCauseConfinementBreaker
	case "hard_max_shed":
		return StopCauseHardMaxShed
	default:
		return TurnStopCause(reason)
	}
}

// TurnOutcome is the result of one Send invocation. Text holds assistant text
// when present, but callers must inspect Kind and Cause before treating it as an
// ordinary final response.
type TurnOutcome struct {
	Kind  TurnOutcomeKind
	Text  string
	Cause TurnStopCause
}

// isIdle reports whether the turn loop is at a genuine idle point: the model
// produced a final message with no tool calls, and async work is STILL ACTIVELY
// RUNNING. This is the only condition that warrants a suspension: the turn must
// wait for the running work to complete so its result can be delivered.
//
// Completed-but-undelivered work (asyncActive == 0, len(asyncInbox) > 0) is NOT
// idle: the work is already done, it just needs draining. The turn loop handles
// that case by continuing to drain and re-run the model, rather than suspending
// and showing a spurious "waiting" state to the user.
func (a *App) isIdle(noToolCalls bool) bool {
	if !noToolCalls {
		return false
	}
	a.asyncMu.Lock()
	pending := a.asyncActive > 0
	a.asyncMu.Unlock()
	return pending
}

// hasInboxContent reports whether the async inbox has completed-but-undelivered
// async work (a completion that landed during the model's stream). This is NOT a
// suspend condition — the work is done, it just needs to be drained and shown to
// the model. The turn loop calls this to decide whether to continue looping
// (drain + re-run) instead of ending the turn.
func (a *App) hasInboxContent() bool {
	a.asyncMu.Lock()
	n := len(a.asyncInbox)
	a.asyncMu.Unlock()
	return n > 0
}

// WaitForAsyncCompletion blocks until an async completion is available to drain
// (or ctx is cancelled, or there is nothing left to wait for). Race-free:
// CHECK the inbox under asyncMu FIRST, then SUBSCRIBE to the coalescing wake
// channel — a completion can never be missed (no lost wake). Multiple near-
// simultaneous completions coalesce into one wake (signalWake's buffered-1
// non-blocking send), so the caller resumes exactly once for a batch.
//
// Returns (true, nil) when a completion is ready to drain; (false, nil) when
// nothing is or will become available (inbox empty + no active ops); (false, ctx.Err())
// when cancelled. On resume the caller must call SendOutcome/drainAsyncInbox to
// pick up the result — exactly one resume owns Conv at a time.
// asyncIsStopping reports whether async admission/shutdown has begun. A
// suspended turn must not silently turn this state into a clean final result.
func (a *App) asyncIsStopping() bool {
	a.asyncMu.Lock()
	defer a.asyncMu.Unlock()
	return a.asyncStopping
}

func (a *App) WaitForAsyncCompletion(ctx context.Context) (bool, error) {
	a.ensureWake()
	for {
		a.asyncMu.Lock()
		if len(a.asyncInbox) > 0 {
			a.asyncMu.Unlock()
			return true, nil
		}
		// No inbox content and nothing active → nothing will ever arrive.
		if a.asyncActive == 0 || a.asyncStopping {
			a.asyncMu.Unlock()
			return false, nil
		}
		wake := a.wake
		a.asyncMu.Unlock()

		select {
		case <-wake:
			// A completion landed (coalesced). Re-check under lock and pick it up.
			continue
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// Resume continues a SUSPENDED turn after an async completion arrived. Unlike
// SendOutcome, it does NOT append a new user prompt — the completion envelope
// drained at the top of streamTurn is the only new input. Returns a TurnOutcome;
// a second Suspended (more work pending) is possible, so callers loop
// SendOutcome → WaitForAsyncCompletion → Resume until Final.
func (a *App) Resume(ctx context.Context) (TurnOutcome, error) {
	a.ensurePreamble()
	// Slip under the context-pressure window the same way SendOutcome does.
	a.fitConvToWindow(ctx)
	// Phase 2 (review finding #7): persist on every resume exit path so
	// the async envelope + final assistant response are never lost when the turn
	// completes via a resume (SendOutcome defers this, Resume must too).
	defer a.SaveSession()

	var (
		traceReasoningChars int
		traceToolCalls      []trace.ToolTrace
		traceTurnIndex      int
	)
	if a.Trace != nil {
		defer func() {
			a.flushTraceTurn(traceTurnIndex, traceReasoningChars, traceToolCalls, nil)
		}()
	}
	rsink := a.traceReasoningSink(&traceReasoningChars)
	final, suspended, err := a.streamTurn(ctx, "", rsink, &traceToolCalls)
	if err != nil {
		return abortedTurnOutcome(ctx, err), err
	}
	a.finalizeTurn(ctx)
	return turnOutcome(final, suspended, currentTurnStopCause(a)), nil
}
