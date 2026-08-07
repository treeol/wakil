# Card #121: Non-Blocking Execution — Async Mashūra + Shell, Never Synchronously Awaited

**Type:** Feature (architecture / execution-model change)
**Mashūra-reviewed:** Plan reviewed by panel (gpt-5.6-sol, claude-fable-5, glm-5.2) on 2026-08-07, two rounds. Round-2 refinement folded in below (oracle opinions, re-verify at implementation time). **Implementation reviewed by the same panel post-implementation; all concrete defects fixed** (shutdown hang, eviction cost loss, drain/check_pending double-delivery guard, dead check_pending pointers, shell reaper deadline race, error-text neutralization, placeholder honesty).
**Board:** wakiil — funnel
**Status:** Phases 1–3 IMPLEMENTED (2026-08-07). Phases 4–5 remain gated/deferred.

## Problem

Every task wakil executes currently blocks the agent turn. The user's concrete pain:

- **Mashūra calls block.** A `mashura__review` can take up to ~30s; wakil sits and waits, unable to do other work. The user tried to run "mashura + docker check in parallel" and wakil issued the mashura call and *blocked*.
- **Shell commands block.** Only when `ShellTimeoutSec > 0` does `run_shell` auto-background (and only *after* the deadline has elapsed — it still waits up to that deadline). With no deadline it blocks on synchronous `RunShell`. CombinedOutput waits for exit.
- Execution formats are **inconsistent**: background jobs (run_background) already spawn beautifully and are polled via `read_process_log`, but mashura and default commands just block.

**Desired behavior:** wakil should be able to issue a mashura call (or any long task) and *continue other work*; when the answer arrives wakil gets a ping and can grep/load the result. In general, long-lived tasks should never be synchronously awaited.

## Verified current state (all file:line, from 2026-08-07 code inspection)

### The agent loop is strictly sequential
- `streamTurn` (`internal/agent/turn_phases.go:101-293`): streams a response, executes tool calls one at a time, feeds results back, repeats. The per-batch walk (`turn_phases.go:265-286`) runs every non-`dispatch_subagent` tool **synchronously** via `a.handleToolCall`; only a contiguous run of ≥2 `dispatch_subagent` calls runs concurrently (`runParallelSubagentBlock`, `turn_phases.go:271-281`).
- `handleToolCall` (`internal/agent/app.go:1359-1399`) → `ExecuteToolCall` (`app.go:1717-1834`): the switch routes to handler methods, each returning a string `stringToToolResult`. No batch/parallel executor for non-subagent tools.
- `finalizeToolResult` (`turn_phases.go:205-256`): appends one `role:"tool"` message per tool call **in order**, on the main goroutine. Ordering matters — non-subagent tools act as ordering barriers.

### Mashūra is fully synchronous
- `handleMashura` (`internal/agent/mashura.go:154-218`): parse → gate (`a.Confirm`) → `counsel.RunPanel(ctx, …)` **blocks** → `FormatPanelResult` returned as the tool string. Appended to `Conv` by `finalizeToolResult`.
- `RunPanel` (`internal/counsel/oracle.go:298-375`): panel/debate use per-slot goroutines but end in `wg.Wait()` — the caller blocks until the whole panel finishes. Fallback mode is sequential.
- All HTTP is plain non-streaming POST (`doJSONPost`, `oracle.go:37-58`, `io.ReadAll(LimitReader(4MB))`). **No SSE, no ping, no partial results.**
- Default timeout 300s (`oracle.go:26-32,132-137`). Zero retry logic (`oracle.go:510-521`).
- Auto-counsel (struggle detection → `maybeSuggestDebug`, `mashura.go:865-934`) also calls `handleMashura` synchronously.

### Shell is partly built but still blocks
- `handleRunShell` (`internal/agent/tool_handlers.go:35-79`): if `ShellTimeoutSec == 0`, calls `a.Exec.RunShell` **synchronously** (blocks to completion). If `> 0`, calls `runShellWithDeadline`.
- `runShellWithDeadline` (`tool_handlers.go:86-193`): spawns bg proc, waits on `select {done | timer | ctx.Done}` **up to the deadline**. On timeout returns a `bg<n>` pointer for `read_process_log` polling. **Fallback paths still block:** at the 5-live-proc limit (`tool_handlers.go:100-109`) and on `StartBackground` failure (`tool_handlers.go:128-135`) it silently falls back to blocking `RunShell`.
- Background machinery exists and works: `bgEntry`/`bgRegistry` (`bg_registry.go:15-19`, `app.go:468-482`), reaper goroutine closes `done` when process exits (`tool_handlers.go:152-163`), `StartBackground`/`KillPgid`/`IsProcessAlive`/`ReadFileTail` (`internal/exec/exec_ops.go:258-353`).
- Other handlers also call `Exec.RunShell` synchronously (find_files, search_files, mashura dir expansion), and several tools (`kill_process` 5s wait, browser/LSP/MCP/web) block.

## Design — the load-bearing decisions

Three oracles converged on the same core: **async applies to result delivery, not dispatch ordering; a protocol-invalid dangling `tool_call_id` is the single hardest constraint; a single completion funnel on the main goroutine is mandatory.**

### D1 — Placeholder result + separate completion inbox (the load-bearing one)
The OpenAI/Anthropic-style protocol requires every assistant `tool_call_id` to be answered by a `tool` result *before* the next model request; you **cannot** leave a call unanswered, and you **cannot** later append a *second* `tool` result for the same ID. So:

- When an async tool is issued, return an **immediate placeholder tool result** that closes the original call:
  `"accepted as op-42 (mashura__review); queued — result will arrive separately; use check_pending/read_task_result/search_task_result."`
- The **real completion** enters the conversation later as a **separate, distinct message** (a synthetic user/control message referencing `op-42`), NOT as a second tool result with the same `tool_call_id`.
- **Must verify at implementation:** the exact legal message shape against `internal/proxy` marshaling (`marshalWireMessages`/`Stream` in `internal/proxy/client.go`) and a test against each supported backend. This is API-version-dependent.

### D2 — "Never synchronously awaited" = scheduling + result delivery, NOT concurrent execution of dependent tools
Preserving today's semantics (`write_file` → `run_shell tests` → `read_file` must still happen in order) means later tools stay **dependent** on prior ordering barriers. Non-blocking means the *agent goroutine* doesn't sleep waiting for a slow op — it can admit other independent work and be woken on completion. Do NOT fire every tool concurrently.

### D3 — Single completion funnel, main goroutine only
Workers never read-modify-write `Conv` or other mutable turn state (recentTraces, struggleSuggested, counselCalls, grounding, touchedExternal, turnToolBytes, confinement state). Workers emit **immutable completion records over one channel**; the main goroutine drains them at safe loop boundaries and applies all bookkeeping (Conv insert, cost, grounding, trace, workflow, LSP, TUI events, pending-registry transition) deterministically. Enforce with architecture + `go test -race`.

### D4 — Confirm gates fire synchronously at dispatch, before backgrounding
`a.Confirm` stays a synchronous call at issue time, on the main goroutine, before the worker/process launches. The **approved immutable request is exactly what executes** — no command/config recomputed after approval. Async must never bypass or re-order a gate. (Round-2 caveat: a backgrounded sub-agent that itself needs confirmation can't prompt — decide auto-deny / pre-authorize / marshal through the funnel. Test nested-confirm-in-background.)

### D5 — No-SSE is not the blocker; wrap RunPanel in a worker
Non-blocking dispatch ≠ streaming. `RunPanel` keeps its internal `wg.Wait` — that's fine *inside a bounded worker*. The coordinator just doesn't wait on it. SSE is a separate (later) improvement for progressive output / hung-connection detection; don't couple them.

### D6 — Fast local tools stay synchronous by default
`read_file`, `list_dir`, short greps gain nothing from async and gain failure modes. Only opt in tools that are latency/timeout/risk-eligible (mashura, long shell, later subagents/MCP/browser). This is a **policy heuristic, not an invariant** — the threshold may move.

## Architecture — phase 1 core

### Immediate acknowledgement
After validation + any immediately-resolvable gate, append one placeholder tool result per original call, in original order. Validation errors / declined actions return a normal terminal error immediately (no op created).

### Pending-operation registry + coordinator (single session-owned coordinator is the sole mutator of agent state)
```go
type OperationID string
type OperationState string // preparing | approval_needed | queued | running | succeeded | failed | cancelled | lost

type PendingOperation struct {
    ID          OperationID
    ToolCallID  string
    ToolName    string
    ArgsDigest  string
    State       OperationState
    Dependencies []OperationID
    CreatedAt, StartedAt, FinishedAt time.Time
    ResultRef   string   // host-side immutable result artifact path
    Summary     string
    ErrText     string
    Cancel      context.CancelFunc
}
```
- Bounded queue + per-operation-type concurrency; unique IDs independent of process IDs.
- Cancellation + timeout; terminal-state **immutability**; retention/cleanup; executor-generation handling.
- **No API keys, auth headers, or raw secret-bearing request structures ever stored** in registry/placeholder/results/traces/session JSON/TUI events.

### Explicit result-access tools
- `check_pending(id)` / `list_pending(state?)` / `read_task_result(id, offset?, limit?)` / `search_task_result(id, pattern, case_insensitive?)` / `cancel_task(id)`.
- At terminal completion write an **immutable host-side result artifact** (reuse the toolcache host-path interception pattern) and expose it via these controlled tools. Do NOT store a huge result in every notification; apply normal capping/spill when the model reads it. Raw bg logs are mutable while running — only immutable terminal artifacts are exposed for read/search.

### Completion inbox + safe model wake-up
Before each model request, drain completions and inject a **bounded completion-inbox message**:
```
ASYNC COMPLETIONS
- op-42 mashura__review: succeeded; result available
- op-43 run_shell: failed, exit 1; result available
```
- Distinct role/type, references the op (never reuses the original `tool_call_id`). Verify message shape against the proxy.
- Only one model stream mutates the main conversation at a time; completions arriving **during** `Stream` queue until the stream ends.

### Suspend/resume logical turns (the "continue other work" semantics)
- If the model has useful independent work, it continues.
- If it emits final text with **no** pending ops → finish normally.
- If it emits final text while relevant ops remain pending → mark the logical turn **suspended**, release the goroutine/TUI busy state, resume inference when a completion arrives, finish when the model gives a terminal answer with no awaited ops.
- Requires replacing/wrapping `Send(ctx)(string,error)` with an event-oriented API: `StartTurn(ctx,text)(TurnID,error)` + `SubscribeTurn(TurnID) <-chan TurnEvent`. (A less invasive fallback: deliver completions only on the next user turn — but that fails the "gets notified when it arrives" requirement; document which is chosen.)

## Phasing (recommended by oracles)

### Phase 0 — Specify + inventory (small)
- Define turn suspension/resumption semantics; ordering classes & barriers; queue/timeout/cancellation/persistence/shutdown policy.
- Inventory every blocking op by code search: `RunShell`, `Wait`, `ReadAll`, `Client.Do`, sleeps/poll loops, subagent dispatch, MCP/browser calls.
- **Minimal shared pending-job contract first** — common identity, terminal-state, dedup, cancellation, funnel semantics shared by both mashura and shell (avoids building two one-off registries).

### Phase 1 — Coordinator + protocol (medium)
- Pending registry, worker pool, completion channel, immutable result store, `check_pending`/`read_task_result`/`search_task_result`/`cancel_task` tools.
- Keep existing operations synchronous behind an adapter initially. Protocol + race tests **before** migrating handlers.

### Phase 2 — Mashura async (small-medium, highest value per brief)
- Split `handleMashura`: parse/validate → resolve panel+providers → prepare authoritative briefing snapshot → **gate against that exact snapshot** → enqueue provider work → workers collect panel results → completion to funnel → coordinator records usage+grounding → model reads/searches the artifact.
- Approval binds to a **payload digest** (a file changing between gate and dispatch must NOT silently alter what's sent — either send the snapshot or re-approve). Keys captured at issue time.
- Auto-counsel enqueues rather than direct-calls `handleMashura`. Counsel caps count approved dispatch (not mere preparation).

### Phase 3 — Shell push-notification (medium)
- `run_shell` always uses the managed async launch path; **remove every blocking fallback** (5-proc limit and StartBackground failure must queue or error, never fall back to blocking `RunShell`).
- Capture stdout/stderr, exit code/signal, start/end times, executor generation, truncation state — a `WaitProcess`/exit-metadata wrapper is likely needed (`IsProcessAlive` alone can't give exit code).
- `kill_process` enqueues termination+escalation rather than sleeping in the handler; LSP sync runs via the coordinator after completion.
- Convert internal shell-backed tools (`find_files`, `search_files`, mashura dir expansion) or explicitly exclude them from the requirement.

### Phase 4 — Other long-running tools (later)
Subagents, MCP, browser, web search/fetch, verification, compaction — only after dependency tests.

### Phase 5 — Persistence & recovery (later)
- Pending ops surviving restart? Mark in-flight HTTP work `lost`; reconcile live executor processes by generation. Persist only sanitized metadata.

## Must-not-violate invariants (hard gates — non-regression)

1. **Protocol closure:** every emitted `tool_call_id` receives exactly one protocol-legal tool result before the next model request; a pending placeholder is that one result; completion uses a separate message and never a duplicate `tool` result for the same ID. Verified against proxy + backend tests.
2. **Single conversation-state owner:** workers never read-modify-write `Conv`/mutable turn state; they emit immutable completion records to the funnel; only the main goroutine drains and mutates. `go test -race` clean.
3. **Exactly-once terminal completion:** reaper callbacks, cancellation, timeouts, spawn failures, worker panics each yield exactly one terminal state (succeeded/failed/cancelled/timeout/lost/never-started). No duplicate completions; a worker panic must still emit a completion so the registry never leaks.
4. **Confirmation before side effects:** synchronously at dispatch, before worker/process launch; the executed request/config exactly matches the approved snapshot; denial starts nothing.
5. **Secret hygiene off the wire:** keys read at issue time, never enter Conv/placeholders/registry/results/traces/session JSON/TUI events; completion notification text carries only id/model-list/path/digest (never briefing/source content); outputs redacted at completion; secret-capture is a narrow immutable reference, not raw material.
6. **Bounded concurrency + backpressure:** no unbounded goroutine/process creation; queue/concurrency/overload behavior explicit; **fail closed, never silently fall back to synchronous execution**.
7. **Cancellation & shutdown:** defined context ownership; shutdown drains or cancels jobs and reaps processes — no leaked goroutines/children/zombies; cancellation produces a terminal completion; process-group kill + concurrent stdout/stderr drain so a child can't block on a full pipe.
8. **External-provider gate ordering:** no external network request before the applicable provider/backend + mashura gate; declined/cancelled can never race into a provider dispatch; backgrounding never turns a gated action into an ungated one.
9. **Ordering semantics:** dependent tools stay sequenced (edit→test, start→probe, generate→read, kill→verify). Result finalization order preserved (append in tool-call order); completion *order* is not guaranteed.
10. **Behavioral compatibility + accounting:** fast local tools unchanged by default; cost/grounding/trace attributed exactly once at terminal completion under the funnel (including error/cancel paths); `MaxToolIterations` accounting adjusted so placeholder acks + completion wake-ups don't cause artificial exhaustion.

## Open questions / decisions to confirm

1. **Turn semantics:** when final text is emitted while ops are pending — suspend-and-resume, or provisional-answer + synthetic continuation turn? (Sets the `Send` → event-API scope.)
2. **Placeholder message shape:** exact legal `Conv` representation of the completion message per backend (verify against `internal/proxy` + API tests).
3. **Nested confirmation in backgrounded sub-agents:** auto-deny / pre-authorize / route through funnel.
4. **Result artifact location:** reuse toolcache host-path interception? Docker-vs-host path handling for read/search tools.
5. **Persistence across restart:** persist sanitized pending metadata, or declare pending work lossy and `lost`-mark on restart?
6. **Default deadline / which tools are async-eligible:** keep a `ShellTimeoutSec` fast-path for short read-only commands or make all long-mashura + long-shell async only?
7. **Which shell-backed internal tools** (`find_files`, `search_files`, mashura dir expansion) to convert vs. exclude.

## Acceptance criteria

Per-phase tests required. Cross-cutting:
- Model requests never contain unresolved tool_call_ids; a transcript validator asserts zero dangling ids.
- Issuing `mashura__*` returns a placeholder in < ~100ms; a test shows ≥1 other tool executing before the counsel result lands.
- No persisted transcript ever contains a dangling tool_call_id (crash/reload test).
- Gates fire before dispatch on the main goroutine; declined = nothing dispatched.
- Completion delivered exactly once to inbox + TUI, even on cancels/timeouts/panics/shutdown.
- Stale/duplicate/post-turn completions deterministically rejected or queued — never applied blindly.
- Model can read + grep terminal results without direct host-path access.
- Ordering tests preserve `write→test`, `start→probe`, `generate→read`, `kill→verify`.
- Completion arriving while awaiting user input wakes or is surfaced per a documented latency rule (a `select` on input + completion channel, or explicit "next user turn").
- No worker writes `Conv`; `go test -race ./...` clean over the funnel.
- Secrets absent from saved sessions, traces, events, task metadata, result artifacts.
- Context compaction can't delete the only reference to a pending/completed result.
- Max-tool-iteration accounting: placeholder acks + completion wake-ups don't cause artificial exhaustion.
- Session shutdown leaves no leaked goroutines or unmanaged child processes.

## Key files
- `internal/agent/turn_phases.go` — `streamTurn` loop, `finalizeToolResult` (main-goroutine funnel home)
- `internal/agent/app.go` — `handleToolCall`, `ExecuteToolCall` (dispatch switch), `Send`/turn lifecycle
- `internal/agent/mashura.go` — `handleMashura`, auto-counsel (`maybeSuggestDebug`)
- `internal/counsel/oracle.go` — `RunPanel`, `doJSONPost`, timeouts
- `internal/agent/tool_handlers.go` — `handleRunShell`, `runShellWithDeadline`, `handleRunBackground`/`kill`/`read_process_log`
- `internal/agent/bg_registry.go` — `bgEntry`/`bgRegistry` (precedent for pending registry)
- `internal/exec/exec_ops.go` — `StartBackground`/`KillPgid`/`IsProcessAlive`/`ReadFileTail`
- `internal/proxy/client.go` — wire marshaling (must confirm placeholder/completion message validity)
- `internal/counsel/modellimit.go`, `internal/agent/toolcache` — capping/spill patterns to reuse for result artifacts

## Effort estimate
- Phase 0: Small (spec + inventory + pending-job contract)
- Phase 1: Medium (coordinator + registry + result tools + tests)
- Phase 2: Medium (mashura split, digest-bound approval, funnel wiring)
- Phase 3: Medium (shell always-async, remove blocking fallbacks, exit metadata)
- Phases 4-5: Later, gated
- Total: Medium-high; Phase 2 (mashura) ships first as the highest value/lower risk.

## Implementation status (2026-08-07)

Shipped: Phases 1-3. Files: `internal/agent/async_ops.go` (+ tests),
`internal/agent/mashura.go`, `internal/agent/tool_handlers.go`,
`internal/agent/turn_phases.go`, `internal/agent/app.go`,
`internal/agent/mcp_manager.go`, `internal/agent/sessionhistory_bridge.go`,
`internal/tools/tools.go`, `cmd/wakil/main.go`, `cmd/wakil/run.go`.

What works now:
- `mashura__*` returns a placeholder immediately; the panel runs on a worker
  goroutine; the result is injected as a marker-framed untrusted-envelope user
  message before the next model request while the turn continues, or retrieved
  early via `check_pending(id)`. Cost committed at terminal completion by the
  worker (never lost to eviction/shutdown); grounding at delivery.
- `run_shell` at its deadline auto-backgrounds and the reaper PUSHES a
  completion notice (id, command digest, last 2 log lines) through the same
  funnel — no polling required. Deadline-boundary race closed (notified flag +
  self-notify). `kill_process`/shutdown disarm notifications.
- The two silent blocking fallbacks are gone: 5-proc limit fails closed with
  actionable advice; StartBackground failure falls back loudly (visible
  warning), never silently.
- `check_pending` tool: ungated read-only, excluded from subagent toolsets,
  CapOrStub-exempt so full mashūra answers pass through.
- Envelope registered in retrievalEnvelopes (stripped at index time); end
  marker neutralized in untrusted content; per-op 8KB / total 16KB caps with
  UTF-8-safe truncation and honest recovery pointers (truncated ops stay
  retrievable).
- Auto-counsel stays synchronous (explicit sync path); subagents stay
  synchronous (no turn loop to deliver into); registry-full fallback is loud
  and uses a detached context.
- Shutdown: `StopAllAsyncOps` waits under an absolute per-op deadline (no
  shared-time.After hang), reports undelivered results, rejects new work.

Deferred per phasing: other long tools (subagents/MCP/browser/web),
persistence across restart, `Send`→event-API turn suspension (true wake-up of
a finished turn), cancel_task, shell exit-code capture.
