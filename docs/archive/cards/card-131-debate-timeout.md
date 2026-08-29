# Card #131 — Debate mode timeout clipped in async path (1x instead of 2x)

Trello: https://trello.com/c/oRFWCH3l (bug).

## Problem (verified 2026-08-10 at HEAD)
- Async worker: `callCtx = context.WithTimeout(context.Background(), 1x)` (mashura.go async branch) — 1x provider timeout.
- `runDebate` (counsel/oracle.go:466-483): `perCallTimeout = ccfg.TimeoutSeconds` (1x), then
  `debateCtx = context.WithTimeout(ctx, 2*perCallTimeout)` = 2x.
- `context.WithTimeout` uses the EARLIER of parent deadline and new timeout → effective debate deadline = min(1x parent, 2x) = **1x**. Round 2 can be cancelled prematurely when round 1 consumed most of the budget.
- Same clipping in the sync-fallback path (`fbCtx` also 1x).

## Fix (Option A — preserves cancellation semantics)
When `mode == "debate"` in `runMashuraCore`, give the outer context **2x** up front so
`runDebate`'s derived 2x deadline is honored (min(2x, 2x) = 2x). Keep `ccfg.TimeoutSeconds` at 1x
(so `runDebate`'s `perCallTimeout` stays the true per-call budget and `2*perCall = 2x`).

- Async worker: `callCtx` = 2x when debate, else 1x.
- Sync fallback: `fbCtx` = 2x when debate, else 1x.
- Non-debate modes unchanged (1x).

Rationale vs Option B (fresh `context.Background()`-derived 2x inside runDebate): Option B detaches
from the caller's cancellation (shutdown/stop would be ignored during debate). Option A keeps the
outer context as a real cancellable 2x deadline passed down to runDebate — the intended wall-time
budget AND cancellation both preserved.

## Files to change
- `internal/agent/async_ops.go` — add `mashuraCallTimeout(mode)` (2× for debate, else 1×);
  thread `callTimeout` through `enqueueAsyncOpJob` → `enqueueAsyncOpInternal` so the watchdog
  arms with the mode-adjusted timeout (NOT the mode-blind 1× — a watchdog at 1× would
  force-terminalize a legit 2-round debate before round 2 finishes, reintroducing the bug
  one layer up / costing the user for a completed debate).
- `internal/agent/mashura.go` — async worker `callCtx` + sync-fallback `fbCtx`: use
  `a.mashuraCallTimeout(mode)`; pass the same computed value to `enqueueAsyncOpJob`.

## Test requirements
- Debate round-1 runs to ~1×, round-2 needs the remaining ~1× → round-2 NOT prematurely
  cancelled, in BOTH the async worker path and the sync-fallback path.
- The debate watchdog is armed at 2× (not 1×) — `TestMashuraDebateWatchdogNotClippedTo1x`
  discriminates: at the 1×+grace point the op must STILL be running; it terminalizes only
  after 2×+grace.
- `mashuraCallTimeout("debate") == 2×`, others `== 1×`.
- Full agent/counsel/tui + full-repo suite green under -race.

## Invariants preserved
- Cost committed at worker terminal; exactly-once publish; UI-not-stall-slot; API keys env-only.
- Cancellation semantics preserved (outer ctx is a real cancellable deadline, not a detached
  background-derived per-call inside runDebate).
- Watchdog deadline matches the worker's effective budget (mode-adjusted), so a legitimately
  running debate is never force-terminalized early (card #131 extends to the watchdog layer).
