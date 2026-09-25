# Continuous Mode CM1 — Contract Audit

**Card:** [CM1](https://trello.com/c/46cJ206Q)  
**Scope:** Prerequisite contract audit for `docs/continuous-mode-proposal.md` Appendix A. This card is documentation/contract groundwork; it does not implement the continuous coordinator.

## Pre-CM1 baseline (audited before implementation)

The following was the state verified at the start of CM1. It is retained as evidence of what this card changed.

### Turn outcome contract

- `TurnOutcomeKind` currently has only `TurnFinal` and `TurnSuspended` (`internal/agent/idle_wake.go:19-29`).
- `TurnOutcome` contains only `Kind` and `Text` (`internal/agent/idle_wake.go:38-44`).
- `SendOutcome` is the authoritative turn entry point (`internal/agent/app.go:944-1060`). `Send` wraps it and discards suspension information.
- Stream errors return a zero-value `TurnOutcome` plus an error (`internal/agent/app.go:1052`), whose zero `Kind` is `TurnFinal`; callers that inspect `Kind` before the error can misclassify it.
- Egress consent decline returns `TurnFinal`, empty text, and nil error (`internal/agent/app.go:982-983`); this is not distinguishable from an ordinary empty final response through `TurnOutcome` alone.
- `App.stopReason` is an internal string set for `confinement_breaker`, `turn_budget_exhausted`, `iteration_limit`, and `hard_max_shed`, but is not surfaced in `TurnOutcome` (`internal/agent/turn_phases.go:39,243-256`, `internal/agent/compact.go:720-721`, `internal/agent/idle_wake.go:41-44`).
- `Resume` has the same error zero-value issue at `internal/agent/idle_wake.go:139-148`.

**Conclusion:** CM1 needs a typed outcome-cause contract before a coordinator can safely classify turn results. `TurnFinal` must never be interpreted as goal success.

### Async and background ownership

- Async IDs are generated under lock (`op-N` / `job-bgN`) and carry `originChatID` (`internal/agent/async_ops.go:736-745`).
- Async active slots are reserved atomically; the cap is 8 and retained terminal operations are bounded (`internal/agent/async_ops.go:39-46,724-751`).
- Wake delivery uses a buffered-1 coalescing channel and inbox-first checking, preventing a lost wake (`internal/agent/async_ops.go:296-337`, `internal/agent/idle_wake.go:77-111`).
- Publication and cost/grounding/effect commits are guarded for idempotency (`internal/agent/async_ops.go:208-225,764-815,1076-1220`).
- Background completion has first-terminalizer ownership and reaper guards (`internal/agent/tool_handlers.go:642-676`, `internal/agent/app.go:471-525`).
- Detached shell notifications bypass the active-slot accounting path (`internal/agent/tool_handlers.go:580-625`).

**Conclusion:** notification deduplication is strong, but this is not yet a durable execution owner or cross-process operation identity. PID/job IDs alone cannot support safe process-death recovery.

### Consent and policy

- Consent is atomic copy-on-write with CAS updates (`internal/agent/consent.go:19-89`).
- Destructive consent cannot be enabled without auto approval, and revoking auto clears destructive consent (`internal/agent/consent.go:122-160`).
- Grants are session-scoped and only deterministic read-only tools are eligible (`internal/agent/grants.go:1-124`).
- External backend consent remains a required gate under auto mode (`internal/agent/turn_phases.go:88-146`).
- Consent state is not persisted; resume must not treat a saved continuous grant as current authority.

**Conclusion:** permission invariants should be reused, not bypassed. A future coordinator must revalidate current policy on resume and persist a pending decision for headless blocking.

### Budget and cost

- `BudgetUSD` is per-session and sticky after exhaustion (`internal/agent/cost_state.go:20-62`).
- The check occurs after inference, so the cutoff is soft and can overshoot by one turn's inference spend (`internal/agent/cost_state.go:25-30`).
- Session cost/token totals are exposed for summary use, but no persistent continuous-goal budget was found.
- The budget state is not reset by `prepareTurn` (`internal/agent/cost_state.go:33-38`).

**Conclusion:** reuse `BudgetUSD` for session accounting, but do not claim a hard total-cost guarantee. A continuous coordinator needs explicit reservation/overshoot semantics and must define whether resumed costs are cumulative.

### Tool side effects and idempotency

- Shell classification is currently allowlist/denylist boolean logic; comments explicitly state destructive classification is friction, not a security boundary (`internal/agent/readonly.go:32-191`).
- Unanalyzable shell constructs fail closed for read-only classification (`internal/agent/readonly.go:214-218,284-303`).
- Non-read-only shell calls mark a shell-side-effect checkpoint (`internal/agent/readonly.go:71,78,1289`).
- Mutating tool paths call `Confirm` before execution (`internal/agent/tool_handlers.go:1025,1100,1221,1257,1317,1460`).
- Filesystem tools are confined to the workspace before access (`internal/agent/tool_handlers.go:788,920,948,983,1015,1063,1215,1245-1249`).
- No general idempotency key or structured effect classification was found. Existing `notified`/`published` flags are completion deduplication, not replay prevention.
- Missing/unreadable background exit markers become an explicit unknown exit status, not success (`internal/agent/tool_handlers.go:290-303`).

**Conclusion:** safe automatic recovery of external mutations cannot be promised by CM1. CM4 must introduce dispatch-time operation accounting and conservative unknown-effect blocking.

### Persistence and workspace identity

- `atomicWriteJSON` uses same-directory temp file, fsync, atomic rename, and best-effort directory sync (`internal/agent/persist.go:32-109`).
- The helper is last-writer-wins; callers needing read-modify-write atomicity must serialize externally (`internal/agent/persist.go:48-51`).
- Session persistence is best-effort/deferred around turns; remote `SaveSession` is a no-op in `internal/remote/facade.go:643` (identified during audit; requires a focused follow-up before remote/daemon scope).

**Conclusion:** existing atomic JSON is suitable as a building block but does not provide lifecycle/workspace identity consistency, single-owner leases, or cross-record reconciliation.

## Implemented CM1 contract

`TurnOutcomeKind` preserves the numeric values of `TurnFinal` and `TurnSuspended`, then appends `TurnAborted`, `TurnCancelled`, and `TurnDeclined`. `TurnOutcome` adds typed `TurnStopCause` metadata. A shared post-finalization constructor normalizes iteration, confinement, hard-max, and budget causes to `TurnAborted`; only a clean stop is `TurnFinal`. `SendOutcome` and `Resume` use the same constructor, so session-budget reporting is consistent.

Outcome contract:

| Disposition | Cause | Error | Meaning |
|---|---|---|---|
| `TurnFinal` | `none` | nil | ordinary final response; still not goal completion |
| `TurnSuspended` | `none` | nil | await tracked async completion, then resume |
| `TurnAborted` | provider/admission/iteration/confinement/hard-max/budget | original error when applicable | invocation ended abnormally |
| `TurnCancelled` | cancelled/deadline | original context error | invocation was cancelled or exceeded its caller deadline |
| `TurnDeclined` | consent not obtained | nil | required egress consent was not granted; no model request admitted |

Unknown outcome values and unknown future causes remain visible and are not interpreted as ordinary success. `runTurnToFinal` now returns the authoritative outcome, distinguishes async shutdown from normal quiescence, and rejects abnormal outcomes. `DriveTurnWithResilience` preserves outcome metadata rather than manufacturing a clean final after recovery.

Compatibility notes:

- `Send` remains the lossy text/error wrapper for legacy callers.
- Existing numeric `TurnFinal` and `TurnSuspended` values are unchanged.
- Egress refusal is surfaced as a non-success outcome. Tool-level declines remain recoverable tool results.
- Outcome reporting does not claim hard pre-admission cost enforcement, quiescence of external effects, or verified goal completion; those remain CM2-CM4 scope.

## CM1 implementation plan

1. Define a typed `TurnOutcomeKind` extension without changing existing numeric values:
   - preserve `TurnFinal` and `TurnSuspended`;
   - append explicit abnormal outcomes for errors, cancellation/deadline, and consent decline as supported by the actual confirmer contract;
   - add an exhaustive `String()` switch with unknown-value fallback.
2. Add a typed stop-cause field to `TurnOutcome`, preserving the original error separately. Do not overload `TurnFinal` as success.
3. Audit and update every supported caller of `SendOutcome`, `Resume`, `TurnFinal`, and `TurnSuspended` so abnormal outcomes cannot be promoted to success. Unknown outcome values fail conservatively.
4. Define and test valid `(Kind, StopCause, error)` combinations, including cleanup/finalization outcomes, budget exhaustion, egress decline, provider failure, cancellation, iteration limit, confinement trip, and hard-max shedding.
5. Keep this card limited to outcome/cause reporting and caller correctness. Do not implement a continuous loop, goal persistence, leases, side-effect ledger, or automatic retries here.

## Required CM1 tests

- ordinary final response remains ordinary-final and does not imply goal success;
- provider failure returns a non-final outcome while preserving the wrapped error;
- cancellation and deadline are distinct;
- egress decline is explicit and produces no provider request;
- iteration, budget, confinement, and hard-max causes are surfaced;
- `Resume` classifies failures correctly;
- unknown enum values are not success;
- supported TUI/headless/subagent callers handle abnormal outcomes conservatively;
- no race or stale stop reason leaks into a later clean turn;
- ordinary non-continuous headless behavior remains unchanged.

## Decisions for CM2–CM4

- CM2 may consume this typed outcome contract, but must still add coordinator-owned goal completion and bounded counters.
- CM3 must provide authoritative durable state, single-owner leases, write-ahead transitions, and reconciliation.
- CM4 must provide dispatch-time external-operation records; CM1 does not claim safe external replay recovery.
- Daemon/remote semantics remain out of scope until their persistence and ownership contracts are directly verified.
