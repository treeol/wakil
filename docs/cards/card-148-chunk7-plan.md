# Card #148 P0 — Chunk 7 plan: headless turn-driving re-route (deliverable 5 step 2 + exit gate #2)

Status: DRAFT v2 (Mashura-reviewed; 3 panels, feedback folded in — see §Revision)
Branch: `feature/wakild-daemon`
Parent plan: `docs/cards/card-148-wakild-impl-plan.md` (deliverable 5 step 2, deliverable 7, exit gate #2)
Predecessors: chunk 5 (`internal/wiring` adapter), chunk 6 (TUI mutation seam)

## Scope — headless first, then TUI; this plan covers HEADLESS ONLY

Chunk 6 built the `Control`/`StateApply` mutation seam but explicitly left the
turn-driving "side door" open: the TUI (`RunTurn`, `RunFinalReview`,
`HandleTUICommand`, `ResumeSessionMsg`) and the headless driver
(`cmd/wakil/run.go`) still hold `*agent.App` and drive turns directly. Its value
is contingent on this chunk landing (all review panels agreed).

This chunk is the **headless half** of that follow-up. It re-routes the
**single-task** headless path (`wakil run "task"`, no `--plan`) through the
session host. The TUI half is chunk 7b (deferred); the `--plan` workflow loop is
chunk 7c (deferred — moved bodily into the bootstrap package here, but NOT
re-routed through the host).

**In scope (this chunk):**
1. Promote `internal/wiring` from "adapter" to **bootstrap package**: the only
   place `*agent.App` is constructed/handled for the headless path. Move
   `buildApp`/`appResources`/`closeResources`/`newHTTPClient` (currently
   `cmd/wakil/app_builder.go`, `cmd/wakil/main.go:276`) into `internal/wiring`
   behind an exported bootstrap API (D19/D20 below).
2. Add one **new agent function** `DriveTurnWithResilience` (D17) that runs a
   turn to its final-outcome *and* returns the final authoritative
   `TurnOutcome` after retry and empty-recovery — because the existing
   `HandleStreamError`/`HandleEmptyResponse` re-send internally and discard the
   recovered text (verified: resilience.go:120 re-sends via `app.Send`).
3. Re-route **single-task** through `sessionhost.Host` + `HostTurnFunc`:
   `CreateSession` → `Subscribe` (before submit) → `SubmitInput` → project
   domain events → the existing JSON-lines transcript + exit codes (D20).
4. Carry the **decline reason as event data** (D18): change
   `ApprovalResolver` to return `{Choice, Reason}`, add an optional
   `ApprovalResolved.Reason`, and latch it through a one-terminal-event
   projection state machine (D20).
5. Thin `cmd/wakil/run.go`/`main.go` to CLI shims; add the headless structural
   guard and the `internal/core` dependency check (D12, exit gate #1 stays red).

**Gate claim (honest, per panel):** this chunk satisfies **exit gate #2 for the
single-task path only**. `cmd/wakil`'s process still contains a direct-App
`--plan` path inside `internal/wiring` (chunk 7c) and the TUI path (7b). The
result is labeled **"gate #2: single-task, partial"**, not a full green. The
parent gate is NOT silently narrowed; 7c is the forcing function to close it.

**Explicitly deferred** (documented, not hidden):
- **`wakil run --plan` workflow loop** (`runWorkflowHeadless`/`runWorkflowLoop`)
  → moves bodily into `internal/wiring` (because `buildApp` moves) but keeps
  driving `App.Send` directly through `agent.HandleStreamError`/
  `HandleEmptyResponse`/`HandleWorkflowTransition`. It does **NOT** call
  `HostTurnFunc` (so it does not trip the `appOwners` single-claim constraint).
  Re-routing it through the host requires new domain signals (workflow
  waiting-state auto-resolution) — that design is chunk 7c. See D16.
- **The TUI re-route (chunk 7b)** — `internal/tui` still holds `*agent.App`;
  `tui.NewTUIModel(app *agent.App)` unchanged; `main.go`'s TUI path still
  imports `agent` + `tui`. **Gate #1 stays red until 7b.**
- **Async wire approval** and **per-session App factory** (chunk-5 deferrals; the
  App-per-session factory is what 7b `/new` rotation needs).
- **`ReadAction` gate semantics** (unchanged; carried into `ApprovalRequested`
  but not enforced).

## Grounding (verified this session — not asserted)

- `internal/wiring/hostturn.go:106-188` already runs the
  `SendOutcome → WaitForAsyncCompletion → Resume` loop and returns the
  authoritative `TurnOutcome.Text`; translates stream/reasoning to
  `MessageDelta`/`ReasoningDelta` and approvals to
  `ApprovalRequested`/`ApprovalResolved`. It does **not** retry, handle empties,
  do workflow, summarize tokens, or capture decline reasons.
- `cmd/wakil/run.go` `runSingleTaskHeadless` (349): `driveHeadlessTurn` → on
  error `HandleStreamError` (retry) → `ErrBackendStream` maps to `ExitBackendFailure`
  (4), anything else (incl. fatal 4xx `ErrBackendFatal`) maps to
  `{"type":"error"}` / `ExitError` (3) — then `HandleEmptyResponse`, then
  `*declinedReason` → `done{declined}` / `ExitDeclined` (1) winning over
  `done{pass}` / `ExitOK` (0). Token summary emitted last (325-333).
- `driveHeadlessTurn` (380) duplicates `agent.runTurnToFinal`'s loop in package
  `main`.
- `internal/agent/resilience.go` `HandleStreamError` (79) retries by calling
  `app.Send(ctx, streamRetryHint)` (120) and returns only `error`;
  `HandleEmptyResponse` (32) detects empty turns and re-sends once in IMPLEMENT
  phases. **Both discard the recovered `TurnOutcome`** — the confirmed gap D17
  closes.
- `headlessConfirmer` (run.go:220) computes **dynamic** decline reasons:
  `"blocked by policy: <reason> (rule: <name>)"`, `"destructive command declined:
  <cmd> (rerun with --allow-destructive)"`, policy-ask reasons, `"--allow-external
  required"`. A driver-side `ToolName → fixed string` mapping cannot reproduce
  these (Q2 is not genuinely open — reason must be data).
- `ApprovalResolver` (hostturn.go:60) is `func(context.Context, ApprovalRequest)
  agent.ConfirmChoice` — **no reason channel** (the gap D18 closes).
- `ApprovalResolved` (payloads.go:154-178) has no `Reason` field today.
- Session host delivers durable events to subscribers via a **channel**
  (`sub.in <- ev`, host.go:1216; `Next` does `<-s.in`, host.go:1310), and emits
  `TurnCompleted` on the executor goroutine *after* `TurnFunc` returns
  (finishTurn, host.go:900-968). Channel send→receive yields a happens-before
  edge — the basis for safely reading `app.Costs`/`app.Client.ChatID` after
  observing the terminal event (D20; confirmed as a `-race` test, see §Tests).

## Design decisions

### D16 — Workflow stays direct (chunk 7c); single-task re-routes now

The `--plan` workflow loop's "waiting for user action" auto-resolution
(WFPresent/WFReview/WFImplement) is headless-specific policy spread across
`runWorkflowLoop` (429-552) and `agent.HandleWorkflowTransition`/`HandleFinalReview`.
Re-routing it needs new domain signals ("workflow needs the next input",
"workflow reached a terminal state") — a real design addition that deserves its
own chunk + review (7c). Single-task is exactly "one turn, then done" and needs
none of it.

**7c is a forcing function, not an optional flag.** The 7c deliverable must land
before P1's replay gate (D9), or the `--plan`-direct seam will ossify. This
ordering is stated here and reflected in the Trello card; gate #2 stays
"single-task partial" until 7c.

Trade-off (honest): gate #1's `cmd/wakil` half is satisfied *structurally* for
the headless path (no `agent.` import in headless non-test files), but a
`--plan` session still runs direct inside the bootstrap package. Flagged, not
hidden.

### D17 — One outcome-returning agent helper `DriveTurnWithResilience`

The adapter cannot use `HandleStreamError`/`HandleEmptyResponse` as-is: they
retry/recover by re-sending internally and return only `error`/nothing, so the
recovered `TurnOutcome.Text` is lost and the durable `MessageCommitted` would be
stale/empty (breaking replay/D9). New exported helper in `internal/agent`:

```go
// DriveTurnWithResilience runs one user turn to its final outcome, retrying
// transient backend failures and recovering empty completions, and returns the
// final authoritative TurnOutcome (and error) for the host to commit.
func DriveTurnWithResilience(ctx context.Context, app *App, userText string) (TurnOutcome, error)
```

Contract:
1. send (`SendOutcome`) + suspend/`WaitForAsyncCompletion`/`Resume` loop
   (subsumes `runTurnToFinal`);
2. on stream error: retry with the existing `HandleStreamError` backoff/classification,
   **capturing the recovered `TurnOutcome`** (retry re-sends a fresh user turn);
3. on success: empty-response recovery (existing `HandleEmptyResponse` semantics),
   **capturing the recovery re-send's outcome** if one fires;
4. returns the FINAL authoritative `(TurnOutcome, error)`.

Implementation note: the retry/empty logic reuses the *same primitives*
(`HandleStreamError`'s classification/backoff; `HandleEmptyResponse`'s detection)
but must be re-expressed to return the outcome rather than `error`/`void`. The
existing `HandleStreamError`/`HandleEmptyResponse` are **left untouched** — the
TUI (`RunTurn`) and the deferred `--plan` loop still call them directly, so no
deferred path regresses. (7b re-routes the TUI onto `DriveTurnWithResilience`
too.)

Retry stays **within one `TurnID`** (per panel Q3: retry is the same turn today;
making it observable would change durable history + transcript + exit codes all
at once — a parity break, not a refactor). A future diagnostic retry event is
noted, not built.

### D18 — Decline reason as data: `ApprovalResolution{Choice, Reason}`

`ApprovalResolver` returns only `agent.ConfirmChoice`; the confirmer in
`newHostConfirmer` cannot know what reason the resolver would have given. Change
the wiring interface:

```go
type ApprovalResolution struct {
    Choice agent.ConfirmChoice
    Reason string
}
type ApprovalResolver func(context.Context, ApprovalRequest) ApprovalResolution
```

This is an **in-repo breaking change** to the chunk-5 `WithResolver` surface; no
external consumer exists (feature branch, P0 in progress), and the chunk-5
tests (`hostturn_test.go`) are updated mechanically. `ApprovalResolution`
replaces the bare `ConfirmChoice` return in `WithResolver`.

Define reasons for ALL forced-decline cases (not just user declines): nil
resolver, context cancellation, policy deny, policy ask, missing `--auto`,
destructive-shell gate, external-egress gate. Invariant: a declined resolution
from this adapter carries a non-empty `Reason`; approved/allowed-reads carry an
empty `Reason` (enforced in the new producer tests, not in the globally-optional
schema field).

`ApprovalResolved` gains an **optional `Reason string`** (additive; validated
as optional, like `Resolver` today). The headless policy decision function — the
`--auto`/`--allow-destructive`/`--allow-external`/policy logic currently in
`headlessConfirmer` — is **extracted into `internal/wiring`** and consulted by
BOTH the single-task resolver (returning `ApprovalResolution`) AND the deferred
`--plan` path (which still needs a plain `agent.Confirmer` + `*declinedReason`).
Extract the *decision* (policy+flags → `(choice, reason)`) once so the two paths
cannot diverge; the `headlessConfirmer` wrapper stays for `--plan` and adapts the
shared decision function to the `Confirmer` signature. Note: this shared decision
function reads `app.Policy()` and `agent.SuspendAuto(toolName, app, detail)` —
wiring holds the App, so this is fine (say so, don't imply purity).

### D19 — `internal/wiring` becomes the bootstrap package; `cmd/wakil` thins

- Move `buildApp`/`appResources`/`closeResources`/`newHTTPClient` from
  `cmd/wakil/app_builder.go` (+ `main.go:276`) into `internal/wiring`, exported:
  ```go
  // wiring.BuildApp mirrors cmd/wakil's buildApp; returns the App and a
  // handle whose Close() preserves the defer order.
  func BuildApp(cfg config.Config, exe exec.Executor, opts BuildAppOpts) (*agent.App, *AppResources)
  type AppResources struct{ ... /* mcpMgr, lspMgr, browserMgr, traceStore, memStore, skillStore, sessionHistStore */ }
  func (r *AppResources) Close()  // LIFO: StopAllBackgroundProcs → mcpMgr/lspMgr/browser/trace/mem/skill/hist close; does NOT close exe
  ```
  Both the headless entrypoint (single-task + `--plan`) and the deferred TUI
  bootstrap (`main.go`) call `wiring.BuildApp` (this is how the moved unexported
  function lands behind an exported API — the panel's compile concern).
- **Options API, not raw args:** `cmd/wakil` owns argument parsing + stderr usage;
  `internal/wiring` receives a transport-neutral struct:
  ```go
  func wiring.RunHeadless(ctx context.Context, cfg config.Config, opts wiring.HeadlessOptions) int
  type HeadlessOptions struct {
      Task string; PlanMode bool; Flags RunFlagsEquivalent; Out io.Writer
  }
  ```
  (`RunFlagsEquivalent` is a `wiring`-local struct mirroring `cmd/wakil.RunFlags`:
  Auto, AllowDestructive, AllowExternal, NoOracle, AttachImage (paths→loaded
  images? no — see below), PolicyPath, ProfileName, Verify, AutoCounsel,
  MaxCounsel, TranscriptFile.) Argument syntax, stderr, and `parseRunArgs` stay in
  `cmd/wakil`; bootstrap/runtime policy (image loading, policy load, host
  construction, event projection, teardown) stays in `internal/wiring`.
- `cmd/wakil/run.go` becomes `parseRunArgs` + a call-through to
  `wiring.RunHeadless`; `app_builder.go` is deleted (moved); `main.go`'s
  headless dispatch and `--attach-image`/`--policy`/`--profile`/`--verify`
  handling move into wiring (the flag *values* arrive via `HeadlessOptions`;
  loading images/policy happens in wiring).
- `headlessWriter`/`emitEvent` move to `internal/wiring` (they are projection
  targets now — panel gap 1b).
- Import direction: `cmd/wakil → internal/wiring → internal/agent + internal/
  core* + internal/policy + internal/tools + internal/proxy`. `sessionhost` stays
  agent/proxy-free (D12). No import cycle (policy/tools/proxy do not import
  wiring — verify with `go list`). New wiring imports enumerated (panel gap 1e).

### D20 — Projection state machine + event-derived outcomes

The single-task driver: `BuildApp` → `HostTurnFunc(app, WithResolver(...))` →
`sessionhost.New(turnFn)` → **`CreateSession` → `Subscribe` (before submit)** →
`SubmitInput` (retain the returned `TurnID`) → consume events → `CloseSession`/
close subscription on every exit path.

Order and correlation are explicit (panel gap #4/#10):
1. create session;
2. subscribe (in-process subscriber);
3. submit input; retain `TurnAck.TurnID`;
4. consume the subscription, correlating terminal events by `TurnID` (and
   session id), latching decline reasons as they arrive;
5. on `TurnCompleted`/`SessionError` for the correlated turn, stop consuming and
   project exactly ONE terminal JSON record;
6. close subscription + session, then flush, then tokens.

Terminal projection precedence (matches current code exactly):
`backend_failure` (`SessionError{internal_error}` for emit failures) **>**
declined **>** pass. Multiple declines → **last reason wins** (current
`*declinedReason` is overwritten, last assignment observable). Decline does NOT
end the turn — the driver keeps consuming until the turn's terminal event, then
emits one record.

| Domain event(s) | JSON projection | Exit code |
|---|---|---|
| `MessageDelta` (ephemeral, in-order) | `{"type":"output","line":…}` (headlessWriter line-buffer, ANSI-stripped) | — |
| `ApprovalResolved{outcome:"declined",reason}` (latched) | held until terminal | — |
| `TurnCompleted{outcome:"complete"}` + no declined | `{"type":"done","outcome":"pass"}` | `ExitOK` (0) |
| `TurnCompleted{outcome:"complete"}` + declined | `{"type":"done","outcome":"declined","reason":…}` | `ExitDeclined` (1) |
| `SessionError{reason:"backend_failure"}` (+`TurnCompleted{stream_error}`) | `{"type":"done","outcome":"backend_failure","resume_id":…}` | `ExitBackendFailure` (4) |
| `SessionError{reason:"internal_error"}` | `{"type":"error",…}` | `ExitError` (3) |
| fatal non-retryable (4xx) | `{"type":"error",…}` | `ExitError` (3) |
| `--plan` only: gaps/verify paths | `{"type":"done","outcome":"gaps"}` … | `ExitGaps` (2) |

The `empty` outcome is **not separately observable**: `DriveTurnWithResilience`
handles empty turns inside the adapter (retry once in IMPLEMENT, warn otherwise
via `App.Out` → `MessageDelta` → an `output` line, matching today). If emptiness
survives recovery, the turn still ends `complete` with whatever text was
produced. The D20-v1 "empty → ExitOK" row is removed.

**Post-turn metadata reads:** `app.Costs`/`app.Client.ChatID` (for the `tokens`
summary and `resume_id`) are read inside wiring **after** the driver has
received the terminal event. The happens-before edge is the channel-delivered
`TurnCompleted`/`SessionError` (host.go: emit on executor goroutine after
`TurnFunc` returns → `sub.in <- ev` → `Next` `<-s.in`), not a guess; a `-race`
integration test asserts it (panel gap #2c/#5a/#5b — the D18-v1 "runs on the
executor goroutine" wording was also imprecise: the confirmer runs on the
executor goroutine, but `resolveApproval` spawns the resolver in its own
goroutine; the *reason latch* on the driver side is what removes the race).

### D21 — Ephemeral drop policy vs. transcript parity

`MessageDelta` is ephemeral and "may be dropped" (D2), which conflicts with
byte-exact transcript parity. Two facts resolve it (verify the first in
`host.go` during implementation): (a) the embedded subscriber buffer is 256
events (`WithSubBuffer`), far larger than one turn's delta burst in practice, and
`pushEphemeral` drops **only on a full buffer**; (b) the headless driver may
optionally set `WithSubBuffer` high or use the direct-callback alternative
(`App.Out` fed line-by-line to the writer is already how the host path installs
`App.Out` — but panel #11/#4b prefer lossless). **Decision:** the single-task
driver sets a generous `WithSubBuffer` AND adds a golden test that a many-chunk
turn produces a byte-identical transcript to the direct path (the real proof);
if the drop path can be triggered under test, document the contract as
"ephemeral deltas are best-effort; the durable `MessageCommitted` is the replay
truth" and note the residual risk for exact-parity consumers.

## Exit criteria

1. `go build ./...`, `go vet ./...`, `go test -race ./...` green (workspace
   temp/cache dirs — `/tmp` is `noexec`; see ENV note).
2. **Gate #2 — single-task, partial:** `wakil run "task"` (no `--plan`) drives the
   session through `core.SessionService`/`EventReader` only, asserted by an
   integration test; the parent gate is annotated "single-task only; `--plan`
   remaining (7c)". Not claimed as full green.
3. **Transcription parity:** byte-identical JSON for (a) a normal complete turn
   (same `output`/`done`/`tokens`, exit 0), (b) a declined tool call
   (`done{declined,reason}` / exit 1), (c) a fatal 4xx (`error` / exit 3).
4. **Resilience parity:** transient stream errors retry inside the adapter and
   the recovered **text** lands in `MessageCommitted`; retry-exhausted stream
   error → `backend_failure` / exit 4; fatal 4xx → `error` / exit 3 (no retry).
5. **Decline reason as data:** declined approval carries a non-empty
   `ApprovalResolved.Reason`, projected to `done{declined,reason}` — not shared
   memory.
6. **Domain-schema guard:** `ApprovalResolved.Reason` (optional) passes the
   event package's payload-type/Validate/completeness tests.
7. **No regressions in deferred paths:** `wakil run --plan` still works (moved,
   not rewired); TUI `go test` green; existing `hostturn_test.go`/`cmd/wakil`
   workflow tests green from their new homes.

## Test plan

- **Headless re-route integration** (in `internal/wiring`, reusing `sseServer`/
  `fakeApp` from `hostturn_test.go`): drive the bootstrap entrypoint end-to-end,
  assert durable kind sequence + exact JSON + exit 0.
- **Decline projection:** tool-call turn, resolver declines with a reason; assert
  `done{declined,reason}` / exit 1 (latched until terminal), and
  `ApprovalResolved.Reason` non-empty and matching.
- **Resilience parity (3 cases):** (a) 5xx-then-success → retried (fake backend
  call counter), `MessageCommitted.Text` == recovered text, `complete`/0;
  (b) 5xx-exhausted → `backend_failure`/4; (c) 4xx → `error`/3, no retry.
- **Interrupt:** interrupt a running turn → `TurnCompleted{cancelled}` +
  `{"type":"done","outcome":"cancel"}` / mapped exit (gate #3 coverage).
- **Suspend+resume:** a turn that suspends on pending async work then resumes to
  `complete` (gate #3 coverage; extend the chunk-5 pattern to the new entrypoint).
- **Tool-call non-decline / approval approve-reads** paths (gate #3).
- **Committed-text-after-recovery** durability test (D9 proxy for this chunk).
- **Golden output tests** (panel #12): text split across chunks, multiple lines
  per chunk, partial-final-line flush, blank lines, ANSI stripping, reasoning
  deltas ignored, output before/after a retry, exactly one terminal record,
  `tokens` last.
- **Structural guard** (panel #7/#8): AST/`go/parser`-based, not `go list` (which
  is package-granular and cannot distinguish the deferred `main.go` TUI import).
  In `cmd/wakil` non-test source, zero `*agent.App`/`agent.` in all files EXCEPT
  the enumerated exception set (`main.go` TUI path — `buildApp` call moved to
  `wiring.BuildApp`, `tui.NewTUIModel`). `go list`-based check kept **only** for
  `internal/core` (no `bubbletea`/`api/gen`/`internal/server`/`internal/agent`).
- **Teardown-order test:** assert the resource-close/StopAll ordering (LIFO:
  flush → SaveSession → StopAllAsyncOps → StopAllBackgroundProcs → resource
  closes → exe.Close) and early-return cleanup for invalid image/policy/profile.
- **Race test:** assert the `app.Costs` post-turn read is race-free.

## Package home / import direction

`internal/wiring` grows into the bootstrap package (owns `*agent.App` for
headless; new imports `internal/policy`, `internal/tools`, `internal/proxy`).
`cmd/wakil` thins to CLI shims. `internal/agent` gains `DriveTurnWithResilience`.
`internal/core/event` gains `ApprovalResolved.Reason` (optional). `internal/
core/sessionhost` unchanged. `hostturn.go` package doc updated: remove "stream-
error retry not replicated here" / "empty → empty committed message" (false once
D17 lands).

## Honest gaps / risks (not hidden)

- **`--plan` is not re-routed** (chunk 7c; a stated forcing function before P1).
- **Gate #1 stays red** until 7b (TUI holds `*agent.App`).
- **`RespondToApproval` is still the P0 stub** (`ErrApprovalNotFound`); approvals
  resolve synchronously inside the adapter. Unchanged from chunk 5.
- **Single-App/one-shot constraint:** `appOwners` permanently claims the App with
  no release path. Fine for a one-shot CLI (`wakil run`) and for tests that build
  distinct Apps per invocation; documented as a constraint. A release/lifecycle
  mechanism is a 7b/P2 concern (daemon reuses apps).
- **Ephemeral-drop residual risk** for pathological delta bursts (D21); golden
  tests + documented contract reduce it.
- **`app.Costs` read is post-turn but not lock-guarded** — correctness from the
  channel happens-before (D20), asserted under `-race` (panel #2c tension
  resolved by naming the mechanism, not by assuming).

## ENV note (carry forward)

`/tmp` is `noexec`; `go test -race` needs
`GOTMPDIR=/mnt/wakil/.gotmp GOCACHE=/mnt/wakil/.gocache`. Discovery subagent
dispatches die on context limits — prefer inline grep. Full suite = 28 packages
(+ new wiring/agent tests).

## §Revision (fold-ins from Mashura review op-9 — gpt-5.6-sol, claude-fable-5, glm-5.2)

Three panels converged. Folded in (14 minimum changes + cross-cutting):
- **D17 rewritten** as outcome-returning `DriveTurnWithResilience` — the v1
  pseudocode lost the recovered `TurnOutcome.Text` after retry/empty-recovery
  (confirmed: resilience.go:120 re-sends via `app.Send`, returns only `error`).
- **Fatal-4xx parity fixed** — maps to `error`/exit **3**, not `backend_failure`/4
  (confirmed run.go:355-361); three distinct resilience test cases.
- **`ApprovalResolver` → `ApprovalResolution{Choice,Reason}`** (was the
  unstated, blocking gap — the enum carried no reason; new invariant list).
- **One-terminal-event projection state machine** with precedence
  backend_failure > declined > pass, last-reason-wins, `tokens` last.
- **Subscribe-before-submit + `TurnID` correlation**.
- **Gate #2 labeled "single-task, partial"**; parent gate not silently narrowed;
  7c named a forcing function before P1.
- **Interrupt + suspend/resume + tool-call-approve + committed-after-recovery**
  tests added (gate #3 coverage).
- **`go list` assertion for `cmd/wakil` replaced** with an AST/file-level guard
  (package-granularity problem) + `main.go` TUI exception; `go list` kept for
  `internal/core`.
- **Options API** `wiring.RunHeadless(ctx, cfg, HeadlessOptions) int` + exported
  `BuildApp`/`AppResources` (fixes the compile gap; parsing/stderr stay in cmd).
- **Teardown-ordering + early-return cleanup** enumerated; `headlessWriter`/
  `emitEvent` migration listed.
- **`--plan` explicitly does not call `HostTurnFunc`** (no `appOwners` conflict);
  `headlessConfirmer` decision logic extracted once and shared with `--plan`.
- **New wiring imports** (`policy`, `tools`, `proxy`) analyzed — no cycle.
- **`app.Costs` race** resolved by naming the channel happens-before + a `-race`
  test (not "no new race" assertion).
- **`empty` outcome row removed** — handled inside the adapter, not observable.
- **`appOwners` one-shot constraint documented** with release-path deferred.
- **Ephemeral-drop vs parity** addressed (D21: generous subbuffer + golden test +
  documented contract).
- Q2 note: decline reasons are dynamic (confirmed), so the question is not
  genuinely open — answered "add `Reason`, return `{Choice,Reason}`".