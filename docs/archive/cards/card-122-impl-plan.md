# Card #122 — Implementation plan (rev 3: IMPLEMENTED, post-impl-review fixes applied)

IMPLEMENTATION STATUS (2026-08-08): Phase 1 + Phase 2 both implemented and
building. Mashūra implementation review (op-5) findings ALL folded:
- isIdle now includes asyncInbox (not just asyncActive) atomically under asyncMu — no stranded completions.
- force-finish no longer suspends (suspended = !forceFinish && isIdle(true)).
- queueAsyncDiscoveryBlock worker: panic-recovery finalizer + publishAsyncOp (exactly-once slot release + publish; never leaks a slot).
- publishAsyncOp shared idempotent finalizer (replaces ad-hoc asyncActive-- plus inbox append); finishAsyncOp is an alias.
- evictOldestTerminalLocked: true-oldest by createdAt, prefers delivered/non-retrievable, append-before-evict (never discards a just-completed inbox pending op).
- notifyDetachedShellExit: append-before-evict + signalWake (detached-shell completion wakes waiters + counts toward idle via inbox check).
- Resume() now defers SaveSession (persists resumed turns); note: traceTurnIndex=0 + nil error in Resume trace accepted (trace docs unchanged).
- GLOBAL subagent semaphore (subagentGlobalSem chan, ensureSubagentGlobalSem) wired into runSubagentJobs — global /maxpar cap across overlapping async batches.


Board "wakiil", card https://trello.com/c/ZPjVcWre. Phase 1 + Phase 2 ship together.

Verified current state at HEAD (d409e75): all card file:line claims hold.
- `dispatchSubagent` subagent.go:786-1141 (sub.Send :987/:1026; writer lock :983-985 edit-only)
- `runSubagentJobs` subagent_parallel.go:107-165 (wg.Wait :163), `runParallelSubagentBlock` :174-298 (A :177, B :263, C :266; spill :284-285)
- funnel `async_ops.go`: asyncOp :69-106, enqueueAsyncOp :175-265, cost-at-terminal :307-326, grounding-at-delivery :332-345, drainAsyncInbox :419-476, check_pending :488, Stop :564
- `Send` app.go:672 (string,error — no outcome type). Callers: tui_cmds.go:35, run.go:351/405, subagent.go:987/1026, resilience.go:47/120
- `CostTracker.Record` mutex-guarded (proxy/cost.go:80) → fold cost safely from worker.
- `addExternalGrounding` (memory_tools.go:454) touches Client + sticky taint → must be turn-goroutine.

## Phase 1 — Async discovery subagents (Mashūra corrections folded)
1. Fix enqueueAsyncOp active-cap admission race: maintain `asyncActive` under asyncMu; reserve atomically at register; decrement exactly once at terminal. (Also gives Phase 2 a reliable idle predicate.) Replace stale `countActiveAsyncOps` map-scan usage for admission/idle.
2. Refactor op lifecycle into shared helpers: `registerAsyncOp` + `finishAsyncOp`; both mashura and subagent enqueues use them.
3. Extend asyncOp with optional subagent effects: `[]asyncSubagentResult{ChatID, Grounding, CostRows, FilesChanged, CtxSize, UsedBackend, Err}` (per-child). Freeze slices at terminal.
4. **Cost committed at TERMINAL (worker)** via foldSubagentCost (mutex-safe tracker). **Grounding + delivery bookkeeping at DRAIN** (turn goroutine): addExternalGrounding, LSP dirty for FilesChanged, SubagentDoneMsg per child.
5. Refactor `runParallelSubagentBlock` into: `prepareSubagentBlock` (Phase A, main) → detached execute (worker: runSubagentJobs only) → commitOrDeliver (drain). Only PURE-DISCOVERY blocks go async; any block containing edit/tools stays synchronous (child-vs-parent mutation invariant).
6. One op per batch; N placeholders (one tool result per tool_call_id). Add a GLOBAL subagent semaphore (sized by MaxParallelSubagents) across overlapping batches for a true global cap.
7. Capacity full → reject with clear error (update enqueueAsyncOp doc/comment that currently says "fall back LOUDLY to synchronous" — for subagents we reject, never silent sync).
8. check_pending on a subagent op: commit effects + serve rendered result.

## Phase 2 — Idle/Wake engine
1. `TurnOutcome{Kind Final|Suspended, Text}`. Keep `Send`; add `SendOutcome(ctx,userText)(TurnOutcome,error)`; Send delegates, returns Text when Final.
2. Idle predicate = model produced final text AND `asyncActive==0`-based pending count > 0 (use atomic active count, not map scan).
3. `streamTurn` must signal "suspended" (produced final text while async work pending) rather than just returning final.
4. Caller retention: TUI `RunTurn`, headless `runSingleTaskHeadless`/`runWorkflowLoop` — if Suspended, register a waiter and resume coalesced on completion; shutdown drains.
5. Wake (race-free): check-any-pending under lock then subscribe; coalescing (one resume per batch of completions); exactly one resume owns Conv.
6. `wait_for_completion` tool (guarded) — blocks/suspends instead of check_pending spin.
7. User message while suspended supersedes once.
8. Tests: lost-wake, duplicate-resume, coalescing, shutdown drain, idle predicate determinism, MaxToolIterations not inflated.

## Security invariants kept
- Edit/tools async NOT enabled (writer lock preserves child-vs-child; parent mutation waits or call stays sync).
- Confirm gates before dispatch (Phase A).
- Protocol closure: every tool_call_id gets exactly one placeholder tool result; real summary as separate envelope.
- No worker writes Conv.
- Bounded global concurrency; fail = reject, never silent sync fallback.
