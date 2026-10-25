# Turn Outcome Contract

This documents the implemented turn-outcome contract that current
TUI/headless/subagent callers rely on, and that a future continuous-mode
coordinator will consume. (Continuous mode itself is not implemented — see
[follow-ups.md](follow-ups.md).) Implementation:
`internal/agent/idle_wake.go` (types), `internal/agent/turn_phases.go` /
`internal/agent/app.go` (construction), `internal/agent/turn_outcome_test.go`
(contract tests).

## TurnOutcomeKind

A turn invocation ends in exactly one disposition. None of these kinds means a
broader *goal* is complete — goal completion is owned by the future continuous
coordinator.

| Disposition | Cause | Error | Meaning |
|---|---|---|---|
| `TurnFinal` | none | nil | ordinary final response; still not goal completion |
| `TurnSuspended` | none | nil | await tracked async completion, then resume |
| `TurnAborted` | provider / admission / iteration / confinement / hard-max / budget | original error when applicable | invocation ended abnormally |
| `TurnCancelled` | cancelled / deadline | original context error | invocation was cancelled or exceeded its caller deadline |
| `TurnDeclined` | consent not obtained | nil | required egress consent was not granted; no model request admitted |

`TurnStopCause` values: `provider_error`, `cancelled`, `deadline_exceeded`,
`consent_declined`, `turn_admission_refused`, `session_budget_exhausted`,
`iteration_limit`, `turn_budget_exhausted`, `confinement_breaker`,
`hard_max_shed`, `async_completion_lost`.

The "Error" column is the separately returned Go error from the turn call
(`SendOutcome` returns `(TurnOutcome, error)`); `TurnOutcome` itself has no
error field.

## Invariants

- The numeric values of `TurnFinal` and `TurnSuspended` are stable
  (compatibility with persisted state and existing callers).
- A shared post-finalization constructor normalizes iteration, confinement,
  hard-max and budget causes to `TurnAborted`; only a clean stop is `TurnFinal`.
- `SendOutcome` and `Resume` use the same constructor, so session-budget
  reporting is consistent.
- Unknown outcome *kinds* remain visible: `TurnOutcomeKind.String()` falls
  back to `unknown(N)`. Unknown stop *causes* pass through as their raw
  string (`TurnStopCause` is a string type). Neither is ever interpreted as
  ordinary success — callers must handle both conservatively.
- `Send` remains the lossy text/error wrapper for legacy callers.
- Outcome reporting does not claim hard pre-admission cost enforcement,
  quiescence of external effects, or verified goal completion — those remain
  future continuous-mode scope.

## Reused infrastructure invariants (verified by the CM1 audit)

- **Async/background ownership**: deduplicated wake delivery, atomic active-slot
  reservation, first-terminalizer ownership, idempotent publication/commit
  guards. Not a durable execution owner — PID/job IDs alone cannot support
  safe process-death recovery.
- **Consent/policy**: atomic copy-on-write consent; destructive consent cannot
  be enabled without auto-approval; grants are session-scoped, read-only tools
  only; consent state is not persisted, so a saved grant is not current
  authority after resume.
- **Budget**: `BudgetUSD` is per-session and sticky; the check occurs after
  inference, so the cutoff can overshoot by one turn's spend.
- **Side effects**: shell classification is friction, not a security boundary;
  unanalyzable constructs fail closed for read-only classification; existing
  `notified`/`published` flags are completion deduplication, not replay
  prevention. No general idempotency-key ledger exists.

## Deferred to the future coordinator (CM2–CM4)

- **CM2**: coordinator-owned goal completion, bounded counters — may consume
  this outcome contract as-is.
- **CM3**: authoritative durable state, single-owner leases, write-ahead
  transitions, reconciliation.
- **CM4**: dispatch-time external-operation records; safe automatic recovery of
  external mutations cannot be promised without them.
- Daemon/remote semantics remain out of scope until their persistence and
  ownership contracts are directly verified.
