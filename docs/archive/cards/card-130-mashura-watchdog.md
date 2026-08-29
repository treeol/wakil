# Card #130 — Mashūra async op watchdog

Trello: https://trello.com/c/dzKKMJwk (bug — tab can spin on a hung Mashūra panel).

## Problem (verified 2026-08-10 at HEAD)
Only DISCOVERY-SUBAGENT batch ops arm a timeout watchdog (`armSubagentWatchdog`,
`internal/agent/async_ops.go:339`). Mashūra async ops (via `enqueueAsyncOpJob` →
`enqueueAsyncOpInternal`) arm NO watchdog:

- Worker wraps `counsel.RunPanel` in `context.WithTimeout(ccfg.TimeoutSeconds)`
  only when `TimeoutSeconds > 0`; when `TimeoutSeconds == 0` it runs
  `context.Background()` (mashura.go:240-247) — **no deadline**.
- `enqueueAsyncOpInternal` (async_ops.go:639-680) has `defer close(op.done)` but
  NO watchdog arming and NO `defer cancelWatchdog`.
- A hung panel holds the admission slot (`asyncActive` never decremented) and the
  TUI tab pulses indefinitely (card #126 pulsing-dot change).
- The `asyncOp.watchdog` field + `cancelWatchdog` already exist (generic) — only
  the subagent path arms/uses them.

## Scope
Protect asynchronously-registered Mashūra ops (`enqueueAsyncOpJob`). Synchronous
fallbacks (registry-full, shutdown) are out of scope for this card (a
non-cooperative sync call cannot be force-bounded without a goroutine boundary —
noted as a related gap).

## Design (informed by Mashūra review op-2)

1. **`mashuraTimeout() time.Duration`** — single authoritative effective provider
   timeout = `OracleTimeoutSeconds` if > 0, else a built-in default (300s,
   matching `config.DefaultConfig`). Used by BOTH:
   - the worker's cooperative `context.WithTimeout`, and
   - the watchdog deadline (`mashuraTimeout + grace`).
   This closes the `TimeoutSeconds == 0` → no-deadline / no-watchdog gap: even
   when config says 0, `mashuraTimeout()` returns the default 300 so both the ctx
   and the watchdog stay bounded.

2. **`armMashuraWatchdog(op, timeout)`** — separate from `armSubagentWatchdog`
   (Mashūra must NOT synthesize subagent results / send SubagentDoneMsg). On fire
   (guarded by `op.mu` + the exact-once `op.published` guard, mirroring
   armSubagentWatchdog):
   - if already `terminal && published` → no-op;
   - if `terminal && !published` → publish for the worker (idempotent);
   - else force-terminalize: set `op.terminal=true`, `op.finishedAt`, `op.err`
     (clear timeout error), a short diagnostic `op.result`, then `publishAsyncOp`
     → emits exactly one `AsyncJobDoneMsg` (uiJob branch).
   - NEVER closes `op.done` (only the worker closes it), NEVER touches
     `childChatIDs`/`subagents` (nil for Mashūra).
   - **Must NOT commit cost** (no usage in hand — the stuck worker holds it).

3. **Arm + cancel lifecycle:**
   - Arm in `enqueueAsyncOpInternal` only when `uiJob==true`, AFTER the AsyncJobStartMsg
     is sent (preserves Start-before-Done) and BEFORE `safe.Go` launches the worker.
   - Worker body gets `defer a.cancelWatchdog(op)` so a normal (slow-but-successful)
     completion cancels the timer before it fires spuriously.

4. **Late cost accounting (Mashūra review's most serious finding).** Currently, if the
   watchdog wins terminalization, the late worker hits `if op.terminal { return }`
   BEFORE storing `op.usage` — so a provider that actually billed (slow-but-completed
   panel) would LOSE its tokens from accounting. Fix: in the worker's terminalization
   block, store `op.usage`/`op.okModels` even when `op.terminal` is already true (i.e.,
   don't return early on terminal), then call `commitAsyncCost` (idempotent via
   `op.costCommitted` and now reconciles the late usage). Must NOT overwrite the
   already-published timeout `result`/`err` or re-publish (guarded by `op.terminal`
   at publish + `op.published`).

5. **Goroutine/forwarder leak on timeout is accepted + documented** — the chunk
   forwarder may leak if the panel is truly hung (same "UI must not stall the async
   slot" trade-off already documented for card #126). Slot/capacity: watchdog releases
   the logical slot but the physical worker may outlive it; accepted liveness trade-off
   (review #6).

## Files to change
- `internal/agent/async_ops.go` — add `mashuraTimeout()`, `armMashuraWatchdog()`;
  wire arming + `defer cancelWatchdog` + late-cost handling into
  `enqueueAsyncOpInternal`.
- `internal/agent/mashura.go` — replace the inline ctx-timeout logic with
  `a.mashuraTimeout()` so the worker and watchdog share one value.
- Tests: `internal/agent/mashura_watchdog_test.go` (new) + adjust existing if
  behavior changed.

## Test requirements
1. Hung Mashūra worker (no-op fn that blocks) → watchdog fires after timeout+grace →
   one `AsyncJobDoneMsg` with timeout err, slot released (`asyncActive` back to prior),
   no double-publish, no double-close; `op.done` NOT closed by watchdog; worker's
   eventual return doesn't panic.
2. Worker completes before watchdog → timer cancelled, no spurious `AsyncJobDoneMsg`,
   exactly-once publish.
3. Late usage reconciliation: watchdog wins, then worker returns usage → cost committed
   exactly once (no lost billed tokens), timeout result not overwritten.
4. Boundary race (worker completion vs timer) under `-race`.
5. `TimeoutSeconds==0` still bounded (mashuraTimeout default 300).

## Invariants preserved
- Cost committed at worker terminal (now reconciles late billed usage — never lost).
- Exactly-once delivery via `op.published` / `publishAsyncOp` funnel.
- Workers never write Conv (this path doesn't touch Conv).
- UI must not stall the async slot or terminalization.
- API keys read from env at call time, never logged.
