# Card #122: Async subagents + Idle/Wake mode — never block on dispatch_subagent, lean into idle

**Type:** Feature (architecture / execution-model extension)
**Mashūra-reviewed:** Review panel (gpt-5.6-sol; full review — the 8KB async-envelope truncation that clipped the other oracles' sections is a CARD #121 BUG now fixed by the universal-spill change, commit d409e75; re-consult panel fully at implementation) on 2026-08-07.
**Board:** wakiil — funnel
**Depends on:** card #121 (commits b9402c8 + 286bc8a + d409e75) — the async funnel (async_ops.go), placeholder+envelope protocol, check_pending, cost-at-terminal-completion, group-liveness, universal spill-to-disk for oversized results.

## Problem / motivation

Card #121 made Mashūra and background **shell** jobs non-blocking, but **subagents still block the turn**:
- `dispatch_subagent` calls `sub.Send(...)` synchronously on the calling goroutine (subagent.go:987).
- `dispatch_subagents` parallel blocks still block the parent on `wg.Wait()` (subagent_parallel.go:163).

So a `sleep 30` shell job is now non-blocking, but a 30s subagent dispatch still freezes the whole turn. The user wants subagents in the same fully-async model.

Second, even for existing async work (mashura, shell), there is **no idle/wait mode**: when all remaining work is background and the model has nothing else productive to do, the only option today is to burn `MaxToolIterations` polling `check_pending` in a loop, or end the turn and wait for the next user ping. The user wants the agent to be able to **lean in while pending work runs, and be woken (pinged) when a completion arrives** — true turn suspension + resume, which card #121 explicitly deferred as "Send→event-API turn suspension."

This card ships both together because they are codependent: async subagents multiply the "work pending, nothing to do" condition, and idle/wake is what makes that condition non-painful.

## Verified current state (all file:line)

### Subagents block the parent turn
- `dispatchSubagent` (subagent.go:786-1141): constructs a child App and calls `sub.Send(ctx, task)` directly on the calling goroutine — synchronous. Returns `SubagentSummary` (objective/findings/checked/skipped/uncertainty/spill_refs/files_changed/external_calls).
- Summary is JSON, ≤4000 chars via `Render()` (subagent.go:38-110), spilled to a durable toolcache path with `[subagent summary at: <path>]` marker.
- `subagentWriterMu` comment (subagent.go) explicitly relies on "the parent is strictly blocked during both dispatch modes" — this is the invariant that breaks under async.
- `runSubagentJobs` (subagent_parallel.go:107-165): truly concurrent (semaphore `MaxParallelSubagents`), but parent awaits `wg.Wait()` (line 163).
- `runParallelSubagentBlock` (subagent_parallel.go:174-298): Phase A prepare (main) / Phase B workers / Phase C finalize (main: cost fold, DoneMsgs, spill, `[subagent summary at: path]` marker at :284-285). Parent blocks until all join.
- Parallel-block detection: turn_phases.go:269-283 routes contiguous `dispatch_subagent` runs (≥2) to the block.
- Sequential single dispatch: tool_handlers.go:1060 (`handleDispatchSubagent`), 1188 (`handleDispatchSubagents`).

### The async funnel (from #121) is the natural substrate
- `async_ops.go`: `asyncOp` (terminal state, usage, okModels, exactly-once cost/delivery), `enqueueAsyncOp` (cap 8 active, detached worker ctx, `done` closed at terminal, fails-closed), `drainAsyncInbox` (turn goroutine, top of streamTurn iteration), `handleCheckPending`, `StopAllAsyncOps`.
- Protocol: placeholder tool result closes the tool_call_id; real result delivered later as a marker-framed untrusted user envelope; cost committed at terminal completion.
- `asyncOp` currently models **text + counsel usage + oracle grounding + shell LSP dirty** — it does NOT yet model subagent completion effects (cost rows, arbitrary grounding, TUI SubagentDoneMsg, files_changed, pinning).

### Turn loop / wake primitives
- `streamTurn` (turn_phases.go:101-293): drains the inbox before each iteration, loops on tool calls; no suspend/wake — returns final text and ends.
- `Send(ctx)(string, error)` — synchronous; no continuation object.
- `handleCheckPending` explicitly says "call check_pending again later" — encourages spinning.
- `MaxToolIterations` is the hard backstop against runaway loops.

## Design

### Part A — Async subagents through the funnel (Phase 1)

**Security-critical decision first (blocker):** making **discovery** subagents async is safe (read-only). Making **edit-capable** (or tools-capable) subagents async is NOT safe under the current locking model — `subagentWriterMu` only serializes child-vs-child edits, not child-vs-parent. A parent write_file racing an async child's edit, or snapshot-restore overwriting a parent edit, is a data-corruption hazard.

So Phase 1 scopes: **discovery (and tools-capable? — see below) subagents become async; edit-capable dispatch stays synchronous** (or suspends the parent — see Open Questions).

- Phase A stays on the turn goroutine: parse, validate, capability gate, consent (edit/tools require /auto), endpoint/backend snapshot, mint ChatIDs, send Start events, **reserve capacity atomically**.
- Phase B: children run on workers (already do in `runSubagentJobs`). The parent **does not wait**.
- Phase C (finalize) becomes the **completion callback**, delivered through the async funnel on terminal completion — but committed by the **turn goroutine at drain** (not the worker), preserving single-Conv-owner.
- Completion effects `asyncOp` must carry for subagents:
  - `result` = subagent summary render (≤4000 char, already capped) + spill path marker
  - `costRows []costRow` → folded via a **mutex-safe cost-commit API** at terminal (prove foldSubagentCost thread-safety or add a serializer)
  - `grounding []proxy.GroundingEntry` (arbitrary, not just oracle)
  - `filesChanged` → LSP dirty-mark at drain (like shell)
  - a TUI `SubagentDoneMsg` event is emitted at CRITICAL completion by a worker-safe event path.
  - **pinning**: currently subagent tool messages are pinned (`finalizeToolResult`, `IsSubagentResult`). With async, the placeholder is the pinned tool message; the delivered envelope carries the summary (pin the user envelope or keep the spill path durable — decide).

**Granularity decision:** one async op per `dispatch_subagent` call; **one batch op per `dispatch_subagents`** tool call (stable child IDs + a terminal aggregate result), reserving the whole batch atomically. Do NOT partially dispatch then silently block/reject the rest.

**Capacity fallback:** `enqueueAsyncOp` "full" → for subagents, **reject with a clear capacity error** (or bounded queue), NOT silent synchronous fallback — synchronous fallback reintroduces blocking exactly when the system is loaded. Edit-capable async being out of scope also avoids this.

### Part B — Idle / Wake mode (Phase 2)

**The load-bearing change:** a textual placeholder ("a ping is coming") cannot schedule a wake. Turn suspension requires a control-plane state transition and a serialized resume.

**New turn result type:**
```go
type TurnOutcome struct {
    Kind TurnOutcomeKind // Final | Suspended
    Text string
    Wait WaitSpec
}
```
- `Send` (or the loop) returns `Suspended` instead of a final answer when: the model produced final text, there is pending async work, AND it has no further independent tool work (the "idle" condition).
- The caller (TUI / headless run loop) retains a continuation and registers waiters.

**Wake protocol (race-free):** on registration, **first check for already-terminal work** under the registry lock (no lost wake); then subscribe. Completion appends under lock and signals a **coalescing notification**. Multiple near-simultaneous completions coalesce into ONE resumed model request. Only one resume may own `Conv`.

**Idle token in the tool result:** when the model emits final text while ops are pending, the handler returns a standard outcome the model/loop understands:
```
Task work remains: op-1 (mashura__review), op-2 (sub agent "map X") — I will wake you when something completes. No need to poll.
```
The LOOP (not the model) decides suspension vs. continue.

**`wait_for_completion` (guarded) instead of spin `check_pending`:** a blocking-or-suspending tool the model can call to explicitly hand control and await the next completion — this removes the `check_pending` spin loop as the only option. (`check_pending` stays for eager retrieval.)

**User-input arbitration while suspended:** define whether an incoming user message cancels / supersedes / joins the pending wake. (Recommend: user message supersedes and re-enters the loop once.)

**Lost-wake & duplicate-resume tests** are a hard acceptance criterion.

## Security / correctness invariants (must-not-violate)
1. **Protocol closure:** every subagent tool_call_id gets exactly ONE placeholder tool result; the real summary arrives as a separate envelope message (never a second tool result for the same ID).
2. **Single Conv owner:** workers never write Conv; completion effects are committed by the turn goroutine at drain (moved Phase C).
3. **Exactly-once cost/delivery:** subagent cost folded exactly once (mutex-safe API); grounding/delivery idempotent.
4. **Edit/tools-capable async requires a workspace-mutation coordinator** — otherwise edit subagents stay synchronous (Phase 1 scope cut) to preserve the child-vs-parent safety invariant.
5. **Confirm gates before dispatch:** edit/tools consent still checked at Phase A before any child launches.
6. **Bounded concurrency:** global subagent concurrency cap (not just per-invocation semaphore) to prevent overlapping async batches ballooning parallelism. Capacity fail = reject/queue, never silent sync fallback.
7. **No parent/child org leak:** batch reserve-all-or-nothing; child ChatIDs isolated.
8. **Wake correctness:** no lost wake (check-then-subscribe under lock), coalesced resume, exactly one Conv owner per resume, shutdown drains pending waits.

## Phasing
- **Phase 1 — Async discovery subagents:** extend `asyncOp` with subagent completion fields (costRows, grounding, filesChanged, SubagentDoneMsg, spill, pinning decision); A stays turn-goroutine, B workers, C at drain; one op per single call / one batch op per batch call; capacity reject-not-fallback; global concurrency cap; tests for protocol closure + exactly-once + race.
- **Phase 2 — Idle/Wake engine:** `TurnOutcome{Final|Suspended}`, continuation retention at callers (TUI + headless), coalescing wake + check-then-subscribe, `wait_for_completion` tool, user-arbitration. Tests: lost-wake, duplicate-resume, coalescing, shutdown drain.
- **Phase 3 — Edit-capable async (ONLY after a workspace-mutation coordinator):** generalize the coordinator to parent+child mutation paths (incl. mutating shell), then allow edit/tools async. Gated on Phase 3's safety being proven — otherwise edit stays sync indefinitely.
- **Phase 4 — Multi-source wake unification:** mashura, shell, subagent completions all drive the same wake engine (they already share the funnel); polish TUI indicator, metrics.

## Open questions / decisions to confirm
1. **Edit-capable async scope** — Phase 1 cut (edit stays sync) vs. building a workspace-mutation coordinator first. Mashūra flag: child-only mutex is insufficient. Recommend: keep edit sync in Phase 1; coordinator is Phase 3.
2. **Tools-capable children** — same external-side-effect class as edit (parent MCP could race a child). Likely also stays sync in Phase 1.
3. **Batch vs per-child op** — recommended one batch op for `dispatch_subagents`; confirm furniture (TUI wants per-child progress though — may need child IDs emitted even under a batch op).
4. **Pinning** — pin the placeholder tool message and/or the delivered summary envelope so compaction can't dissolve the subagent breadcrumb.
5. **`wait_for_completion` semantics** — timeout? return on ANY completion vs a specific id? interplay with user-input arbitration.
6. **Idle threshold** — what counts as "idle" (model says done + pending ops + no queued tool calls). Need a deterministic predicate.
7. **New TurnOutcome return** — how big is the integration surface (all Send callers: TUI, headless run loop, benchmark harness)? Full read of Send/callers at implementation.

## Acceptance criteria (per mall-column oracle guidance)
Phase 1:
- Issuing `dispatch_subagent` (discovery) returns a placeholder in <~100ms while the child runs.
- The child's structured summary is delivered exactly once via the envelope; check_pending serves it eagerly; no duplicate delivery.
- Subagent cost folded exactly once (mutex-safe), incl. on child failure.
- Child result spill path + `[subagent summary at: path]` marker intact.
- No worker writes Conv (`-race` clean on the funnel).
- A 3-dispatch parallel block completes as one batch op (or N child ops with stable IDs) with no partial-silent-block.
- Capacity full → clear rejection, never silent sync run.
- Global subagent concurrency cap enforced across overlapping async batches.
- Edit-capable dispatch remains synchronous (safe) in Phase 1 — regression test asserts this.
Phase 2:
- A model turn with pending ops + no further work returns `Suspended`, not `Final`.
- Caller resumes on completion; near-simultaneous completions coalesce into one request.
- No lost wake when completion races registration (check-then-subscribe under lock).
- Exactly one resume owns Conv at a time; `-race` clean.
- `wait_for_completion` returns without spinning; clears the `MaxToolIterations` pressure.
- User message while suspended supersedes cleanly; shutdown drains pending waits.
- Idle predicate is deterministic; `MaxToolIterations` accounting not inflated by wake resume.

## Key files
- `internal/agent/subagent.go` — `dispatchSubagent`, `SubagentSummary`, `subagentWriterMu`
- `internal/agent/subagent_parallel.go` — `runSubagentJobs`, `runParallelSubagentBlock` (A/B/C)
- `internal/agent/async_ops.go` — funnel (`asyncOp`, `enqueueAsyncOp`, `drainAsyncInbox`, `check_pending`)
- `internal/agent/turn_phases.go` — `streamTurn` drain loop, parallel-block detection
- `internal/agent/tool_handlers.go` — `handleDispatchSubagent(s)`, CapOrStub/pinning
- `internal/agent/app.go` — `Send`, `streamTurn` caller, Conv ownership
- `cmd/wakil/run.go`, TUI — Send callers needing TurnOutcome integration

## Effort estimate
- Phase 1: Medium (asyncOp extension, A/B/C split, cost-commit API, concurrency cap, tests)
- Phase 2: Medium-high (TurnOutcome, wake engine, coalescing, wait_for_completion, callers)
- Phase 3: High (workspace-mutation coordinator) — gated, likely its own card
- Phase 4: Small (unify wake sources, polish)
- Total: Medium-high across 4 phases; Phases 1+2 ship together as the natural pair.
