package agent

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTurnOutcomeStringUnknownFailsClosed(t *testing.T) {
	if got := TurnOutcomeKind(99).String(); got != "unknown(99)" {
		t.Fatalf("unknown kind string = %q, want unknown(99)", got)
	}
}

func TestAbortedTurnOutcomeClassifiesProviderError(t *testing.T) {
	sentinel := errors.New("provider failed")
	out := abortedTurnOutcome(context.Background(), sentinel)
	if out.Kind != TurnAborted || out.Cause != StopCauseProvider {
		t.Fatalf("provider outcome = %+v, want aborted/provider_error", out)
	}
}

func TestAbortedTurnOutcomeDistinguishesCancellationAndDeadline(t *testing.T) {
	cancelled := abortedTurnOutcome(context.Background(), context.Canceled)
	if cancelled.Kind != TurnCancelled || cancelled.Cause != StopCauseCancelled {
		t.Fatalf("cancel outcome = %+v, want cancelled/cancelled", cancelled)
	}

	ctx, cancel := context.WithDeadline(context.Background(), timePastDeadline())
	defer cancel()
	deadline := abortedTurnOutcome(ctx, context.DeadlineExceeded)
	if deadline.Kind != TurnCancelled || deadline.Cause != StopCauseDeadline {
		t.Fatalf("deadline outcome = %+v, want cancelled/deadline_exceeded", deadline)
	}
}

func TestStopCauseFromLegacyPreservesUnknownCause(t *testing.T) {
	if got := stopCauseFromLegacy("future_cause"); got != TurnStopCause("future_cause") {
		t.Fatalf("unknown legacy cause = %q, want preserved", got)
	}
	for input, want := range map[string]TurnStopCause{
		"":                      StopCauseNone,
		"iteration_limit":       StopCauseIterationLimit,
		"turn_budget_exhausted": StopCauseTurnBudgetExhausted,
		"confinement_breaker":   StopCauseConfinementBreaker,
		"hard_max_shed":         StopCauseHardMaxShed,
	} {
		if got := stopCauseFromLegacy(input); got != want {
			t.Errorf("stopCauseFromLegacy(%q) = %q, want %q", input, got, want)
		}
	}
}

func timePastDeadline() time.Time {
	return time.Now().Add(-time.Second)
}
