package agent

import (
	"context"

	"github.com/treeol/wakil/internal/trace"
)

// ─── Card #122 Phase 2: Idle/Wake engine ───────────────────────────────────
//
// A turn can END in one of two ways: the model produced a final answer and no
// async work remains (Final), or the model produced final text while async work
// (mashura panel, detached shell, discovery subagents) is still pending and it
// has no further independent tool work (Suspended). The caller — TUI or headless
// run loop — retains the continuation and, on a Suspended outcome, registers a
// waiter and resumes once a completion arrives (wake), instead of spinning on
// check_pending or ending the turn.

// TurnOutcomeKind distinguishes a fully-finished turn from a suspended one
// awaiting async completions.
type TurnOutcomeKind int

const (
	// TurnFinal: the turn is complete — the answer is final.
	TurnFinal TurnOutcomeKind = iota
	// TurnSuspended: the model produced final text while async work is pending;
	// the caller should await a completion (WaitForAsyncCompletion) and resume.
	TurnSuspended
)

func (k TurnOutcomeKind) String() string {
	if k == TurnSuspended {
		return "suspended"
	}
	return "final"
}

// TurnOutcome is the result of one Send. When Kind == TurnSuspended, Text holds
// the model's final text (the interim answer); the caller should not treat it
// as the definitive end of the turn.
type TurnOutcome struct {
	Kind TurnOutcomeKind
	Text string
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
	// Card #122 Phase 2 (review finding #7): persist on every resume exit path so
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
		return TurnOutcome{}, err
	}
	a.finalizeTurn(ctx)
	kind := TurnFinal
	if suspended {
		kind = TurnSuspended
	}
	return TurnOutcome{Kind: kind, Text: final}, nil
}
