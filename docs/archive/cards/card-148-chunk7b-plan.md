# Card #148 P0 — Chunk 7b plan: TUI turn-driving re-route (drop `*agent.App` from `internal/tui` — Gate #1)

Status: DRAFT v2 (Mashura-reviewed; 3 panels — gpt-5.6-sol, claude-fable-5, glm-5.2 — feedback folded in; see §Revision)
Branch: `feature/wakild-daemon`
Parent plan: `docs/cards/card-148-wakild-impl-plan.md` (deliverable 5 step 2, deliverable 7, exit gate #1)
Predecessors: chunk 7 (headless single-task re-route, gate #2 "single-task partial"), chunk 6 (TUI mutation seam)

## Scope — close Gate #1 for the TUI

Gate #1 — *neither `internal/tui` nor `cmd/wakil` holds/imports `*agent.App`* — is
still red: `tuiModel.app *agent.App` (tui.go:127), the turn-driving entry points
(`RunTurn`/`RunFinalReview`/`HandleTUICommand`/`ResumeSessionMsg`), and the TUI
bootstrap (`cmd/wakil/main.go`) all hold the App and drive turns directly.

This chunk closes Gate #1: `internal/tui` stops holding/importing `*agent.App`,
the TUI drives turns through the session host (`SessionService` + `EventReader`),
and the interactive approval gate becomes TUI-as-resolver over events (D5 async
approval lifted out of P2).

**Decomposes into three sub-chunks, each Mashura-gated and committed green:**

- **7b1 — client contract + lifecycle architecture (server-side, no TUI cut).**
  Pin the agent-free facade contract (package home + DTO inventory), the
  per-session App factory with an `appOwners` **release path**, and the
  one-Host-per-conversation architecture (D22/D23/D26/D27 foundations). Gate #1
  stays red.
- **7b2 — session-scoped emission + async approval.** Add the host's
  session-scoped emit surface (turn-scoped `Emitter` cannot carry detached
  work), the replay-bearing event kinds (user message, compaction, workflow,
  async-job, side-question), and lift D5 async approval (park at
  `awaiting_approval`, `ApprovalRequested` → `RespondToApproval` → resume).
- **7b3 — the terminal cut.** TUI consumes events + snapshot, drives
  `SubmitInput`/`Interrupt`/`RespondToApproval`, dispatches commands via the
  agent-free facade, rotates via 7b1. Delete `*agent.App` from `tuiModel` and
  stop `cmd/wakil/main.go` importing `agent`. **Close Gate #1.**

**Architecture choice (the panels' blocking gap, now pinned):** one Host per
conversation. Each TUI conversation = one `*agent.App` → one `HostTurnFunc` →
one host session (the chunk-5 `claimSession` single-session constraint already
enforces exactly this). The facade owns the triple `(App, Host, subscription)`;
rotation swaps all three. There is **no** routing `TurnFunc` and **no** multi-
session-per-App executor factory — the existing one-App-one-session model is
kept, and only its *permanent* `appOwners` claim is made releasable.

## Grounding (verified this session — not asserted)

1. **Turn-driving entry points the TUI calls directly** (pass `*App`):
   `agent.RunTurn` (tui.go:1214,1419; tui_agent_msgs.go:536,597,642,863,942),
   `agent.RunFinalReview` (tui_agent_msgs.go:920), `agent.HandleTUICommand`
   (tui.go:1159), `agent.ResumeSessionMsg` (resume_picker.go:100),
   `StartSideQuestion` via `m.control` (tui.go:1488).
2. **The TUI consumes ~30 `agent.` message types** in `handleAgentMsg` plus reads
   of App state — re-confirmed. `agent.*` symbols in non-test TUI source:
   47× `agent.App`, 25× `SubagentStartMsg`, 24× `AgentDoneMsg`, 19×
   `AsyncJobStartMsg`, 17× `AsyncJobDoneMsg`, 15× `SubagentDoneMsg`, 12×
   `NewConvMsg`/`ContextLimit`, 11× `WithCosts`, 9× `SysNoteMsg`/`StrPtr`/
   `ConsentSnapshot`/`Cmd`, 8× `AsyncJobChunkMsg`, 7× `ShortID`/`RunTurn`/
   `CompactedMsg`, and ~25 more singletons (grep, this session).
3. **`Cmd`/`Msg` are not opaque-neutral.** `cmd.go`: `type Msg = any` (line 17),
   `type Cmd = func() Msg` (line 25), `BatchMsg{Cmds []Cmd}` (line 36),
   `ClipboardImageRequest` sentinel (line 68). Any dispatcher returning
   `agent.Cmd` makes `internal/tui` import `agent` (to call it, receive
   `agent.Msg`, and type-switch) — a direct Gate #1 violation, not an "opaque
   type" workaround.
4. **The adapter drops most of the TUI's output.** `hostturn.go:159-169`
   installs `Out`/`OnReasoning` but sets `app.EventSink = func(any){}` and
   `app.OnTokRate = func(float64){}`. Subagent/async-job/tool-status/
   side-question/compaction/learn-nudge/model-list/ctx-limit/workflow/tokrate
   messages reach no subscriber.
5. **The turn-scoped `Emitter` cannot carry post-turn / detached work.**
   `host.go:163-166`: `Emitter` is turn-scoped and fenced at finalization;
   `Emit` returns `ErrEmitterClosed` after. `hostReservedKinds` (host.go:154-161)
   reserves `TurnStarted`/`MessageCommitted`/`TurnCompleted`/`SessionError`/
   `SessionClosed` — a turn cannot emit them. The TUI's `WFStartTurnMsg` is sent
   *after* `AgentDoneMsg` (tui_cmds.go:98-101); `AsyncJobDoneMsg` and
   `SideQuestionDoneMsg` arrive after the turn, even across rotation (the
   origin-guard handlers exist for this). These have **no legal emit path** in
   the current host.
6. **Approval is a synchronous block on both sides.** TUI gate = channel
   (`ConfirmReqMsg.RespCh`, answered at tui.go:737-744). Host adapter resolves
   *inside* the turn (`resolveApproval`, hostturn.go:303-326). `Host.
   RespondToApproval` is the P0 stub returning `ErrApprovalNotFound`
   (host.go:381-388). `SessionAwaitingApproval` is a **top-level state**
   (service.go:151) with defined transitions (`running → awaiting_approval →
   running`; service.go:166-169) already handled by `Interrupt` (host.go:419),
   but it is **never set anywhere** — no code path parks the session there.
7. **`appOwners` claims an App permanently** (hostturn.go:96-99) and the
   TurnFunc binds one session forever (`claimSession`, hostturn.go:198-209).
   Rotation (`/new`/`/resume`/`/handoff`) needs a fresh App per conversation —
   the deferred per-session factory + release path.
8. **`DriveTurnWithResilience` does not do workflow.** It runs
   `runTurnToFinal` + `HandleStreamError` retry + `HandleEmptyResponse`, then
   returns `TurnOutcome{Kind, Text}` only (resilience.go:160-177) — no Warn, no
   nudge, no `HandleWorkflowTransition`, no `HandleFinalReview`. The TUI's
   workflow loop (`RunTurn` → `HandleWorkflowTransition` → `WFStartTurnMsg`;
   `RunFinalReview` → `HandleFinalReview`) is **not** in the host path today.
9. **Slash commands mutate App directly** via `HandleTUICommand`
   (commands.go:263+); `SessionService` has no command surface.
10. **The TUI already imports `proxy` directly** (tui.go:12; `subTab.grounding
    []proxy.GroundingEntry`). Gate #1 forbids `*agent.App`, not `proxy`. Only
    `agent.*` types need neutralization; `proxy.*` types can stay.
11. **Client-initiated mutations outside slash commands** (from `control.go` /
    `tui_agent_msgs.go`): `SetAutoApprove` + `SaveRepoState` (deferred /auto
    grant at AgentDoneMsg, lines 183-199), `SetAllowDestructive`, `SetWorkflow
    (nil)`, `AppendSystemMessage` (handoff context), `SaveSession`,
    `ConsumeStartupNote`, `AddPendingImage`/`ClearPendingImages`/
    `ReplacePendingImages`, `SetCtxLimit`/`SetModelList`/`SetTools`. These must
    all be named on the facade.

## The three walls (corrected per review)

**W1 — async approval.** Real, but the v1 wording was internally contradictory:
with the synchronous `Confirmer func(...) bool`, the agent call stack *must*
block until a bool returns. The correct statement: the **turn goroutine blocks**
on a one-shot decision channel raced with ctx; `RespondToApproval` and the
Bubble Tea loop do **not** block. A continuation-based non-blocking executor
would require resumable agent-loop state — nothing supports that and it is out
of scope. (D25.)

**W2 — event vocabulary.** Real, but v1 overstated it: not every display signal
is a durable domain event. The correct split (D24) is five classes, not one
table of ~10 new durable kinds. Most "query-state" signals are **snapshot
fields, not events**; most TUI-local signals are **client-local, not domain
events**.

**W3 — rotation factory.** Real, but v1 named "per-session factory" without
choosing the Host architecture. Pinned now: one Host per conversation (Scope).

## Design decisions

### D22 — Server-first, one Host per conversation, no interim seam

Build server capabilities (7b1, 7b2) before cutting the TUI over (7b3), so 7b3
is a client swap against a complete surface. The TUI keeps driving `*agent.App`
until 7b3. There is deliberately **no** second App-backed facade the TUI adopts
and later discards — the chunk-6 `Control`/`StateApply` seam is *removed* at
7b3, not layered over.

**Rejected alternative — the resolver-bridge** (all three panels raised it, so
it is addressed explicitly, not ignored): keep the sync `ConfirmReqMsg.RespCh`
channel and bridge it through a wiring `ApprovalResolver` that blocks on a reply
channel the TUI answers via a facade `AnswerApproval` method — avoiding the
async-approval host state machine entirely. Rejected because (a) it is precisely
the "second interim seam" D22 exists to avoid; (b) it leaves `RespondToApproval`
a stub for the TUI path and never enters `awaiting_approval`, so `Interrupt`
during approval diverges from the P2 design; (c) `ApprovalResolved.Resolver`
would record the submitter principal, not the user who actually answered.
**Documented fallback:** if D25's executor-parking proves riskier than expected
mid-7b2, the bridge is the named retreat — but it is a retreat, not the plan.

### D23 — Command surface, agent-free

`DispatchCommand(app, line) (handled, quit bool, cmd agent.Cmd)` is a **Gate #1
violation** and is withdrawn. The command boundary is agent-free from 7b1:

- Wiring owns `HandleTUICommand` internally. It classifies each slash command
  (panel taxonomy) and routes it:
  - **TUI-local** (help, info-panel toggle, /queue, completion, clipboard) —
    already handled in `internal/tui`; never reaches wiring.
  - **Session-mutation** (`/auto`, `/auto destructive`, `/rawtools`, `/backend`,
    `/model`, `/submodel`, `/profile`, `/policy`, `/compact`, `/cwd`, `/mode`,
    `/history`) → facade methods with neutral args/returns.
  - **Rotation** (`/new`, `/resume`, `/handoff`) → facade rotation ops (D27).
  - **Turn-submission** (`/learn`, `/remember`, `/recall` continuation, and the
    `/plan` workflow submissions) → facade `Submit`.
  - **Independent-async** (`/ask`) → facade `StartSideQuestion` (D29).
  - **Process-action** (quit) → TUI-local.
- Command **results** that today ride `agent.Msg` become either (i) neutral
  notice strings in a `CommandResult` struct, or (ii) domain events, per a
  written mapping table (7b2/7b3 implementation detail, but the *boundary* is:
  no `agent.*` type in any facade return). `BatchMsg`/`ClipboardImageRequest`
  mechanics are absorbed wiring-side (the clipboard sentinel becomes a facade
  operation the TUI already owns).

```go
// in the facade package (D26), not agent:
type CommandResult struct {
    Handled bool
    Quit    bool
    Notice  string          // replaces SysNoteMsg/CompactedMsg text
    Submit  string          // non-empty → the TUI submits this as the next turn
    Rotate  *RotateRequest  // non-nil → the TUI rotates (D27)
    SideQuestion string     // non-empty → the TUI starts a side question
}
```

### D24 — Event schema, session-scoped emission, replay truth

**Session-scoped emission (new host surface — the panels' biggest missing
step).** Detached work (async jobs, side questions) and post-turn signals
(`WFStartTurnMsg`, stream-warn, learn-nudge) outlive the turn, so they cannot
use the turn-scoped `TurnInput.Emit`. Add a session-scoped emit path owned by
the host and fenced only at session close (not turn completion). The adapter
wires `app.EventSink`/`app.OnTokRate` to **this** surface, not to `in.Emit`;
turn-scoped durable events (`Approval*`, `ToolCall*`, subagent events) keep
using `in.Emit`. The exact host API shape (`Host.EmitSessionEvent(...)` vs.
passing a session-scoped `Emitter` into `TurnFunc`) is a 7b2 implementation
detail, but the *contract* — out-of-turn events are legal until session close —
is the decision.

**Replay truth (transcript reconstruction).** `TurnStarted` has no user text and
`MessageCommitted` only assistant output, so "session_created + replay
reconstructs the TUI" is currently false. Two additions:
- `user_message_committed` (durable, host-emitted on `SubmitInput`, carrying
  `TurnID` + the submitted `Text`). This is the durable user side of the
  transcript and what `/resume` replays.
- The facade's `ClientSnapshot` (D26) carries the authoritative live transcript;
  replay reconstructs it from `user_message_committed` + `MessageCommitted` +
  `conversation_compacted` boundaries.

**Event additions (five-class split — not every signal is an event):**

| Class | Signals | Home |
|---|---|---|
| Durable replay facts | approval, tool lifecycle, compaction, workflow transition, async-job started/completed, side-question completed, `user_message_committed` | domain events |
| Ephemeral session notifications | `message_delta`, `reasoning_delta`, `subagent_progress`, `tok_rate`, `async_job_progress`, `side_question_progress`, learn-nudge | ephemeral events |
| Query-state | model list, context limit, consent, costs, tools, pending images, selected backend, raw tools | **snapshot fields (NOT events)** |
| Client-local | resume-picker open, transient sys notes, clipboard results | TUI-local (NOT events) |
| Explicit operations | side-question start/cancel, async-job lifecycle | host/facade ops + session-scoped emit |

This *shrinks* the v1 table: `model_list_updated`/`context_limit_resolved` are
dropped (snapshot fields); `learn_nudge` is **not** folded into `turn_completed`
(host-reserved kind — illegal; it becomes an ephemeral `learn_nudge` or a
snapshot-local advisory); the workflow signals become explicit events (below).

**Correlation fields.** Every new event carries explicit IDs: `TurnID` on
turn-scoped events; an operation ID on side-question and async-job events; a
stable subagent-ID↔`ChatID` mapping (the TUI routes by `ChatID`; the domain
uses `SubagentID` — the adapter defines one stable translation, not a fresh ID
per callback); and `OriginSessionID` (replacing `OriginChatID`) on any event
that can be delivered late, so the TUI's post-rotation guards key on the domain
session ID.

**Tool command text — redaction policy (security, not implementation detail).**
The TUI's status line shows the running tool's command; `tool_call_started`
today carries only `ArgDigest`. Raw command text must **not** go into the
durable event. Decision: a **bounded, redacted `CommandPreview`** (the existing
`ArgDigest` stays authoritative for the durable record; the preview is
truncated + secret-scrubbed, or empty when scrubbing cannot be guaranteed).
The full raw text stays in the ephemeral `ToolStart` projection the TUI renders,
never in the durable log.

**Subagent payload enrichment (bounded).** The TUI's tabs need
backend/model/toolNames on spawn and files/cost/status/preview/usedBackend on
completion. Extend the domain payloads with **bounded** neutral fields (panel:
"bound previews and lists; avoid unbounded grounding/files"). grounding/
ctxSize/hardMax are info-panel diagnostics — kept thin/ephemeral, not folded
into the durable completion payload. A missing `active` (queued→running)
transition is added if queue/running state must render.

### D25 — Async approval: block the turn goroutine, flip host state, resolve

The correct minimal model (all three panels converged):

1. The turn goroutine calls the synchronous `Confirmer`; the async confirmer
   registers a pending approval under session synchronization, emits
   `ApprovalRequested` (durable), then blocks on a one-shot decision channel
   raced with `ctx.Done()`.
2. **Host state flip — new hook:** the host must transition the session to
   `SessionAwaitingApproval` while its executor is blocked inside the TurnFunc.
   The adapter exposes a host-owned turn-control callback (the confirmer
   notifies the host "approval pending" → `running → awaiting_approval`). This
   is the missing hook; `SessionAwaitingApproval` exists but is never set today.
3. `RespondToApproval` validates the principal, atomically resolves the pending
   entry, emits nothing itself — the turn goroutine, woken, applies any consent
   mutation (`SetAllowReads`), emits `ApprovalResolved{Outcome, Reason, Resolver}`
   (durable), removes the pending entry, and returns the bool. Resolver =
   answering principal. (`RespondToApproval` returns `ErrApprovalNotFound` for
   unknown, `ErrApprovalAlreadyResolved` for a stale/conflicting decision;
   same-outcome duplicate is idempotent — the existing contract, now served.)
4. **Cancel/close-during-approval:** `Interrupt`/`CloseSession` cancels the turn
   ctx; the blocked select wakes with a **forced decline** → `ApprovalResolved
   {declined, "cancelled", Resolver: system/interrupt-principal}` emitted
   *before* the turn emitter is fenced, then the pending entry is removed.
5. **Per-session resolver choice:** headless keeps the inline `resolveApproval`
   (chunk-7 parity, no regression); the TUI session uses the async resolver.
   Multiple concurrent approvals are **not** required (the agent's single turn
   loop has at most one pending gate at a time; if two arise, they queue
   FIFO under the session lock).
6. Durable-append failure of `ApprovalRequested`/`ApprovalResolved` fails the
   turn (existing `errorLatch` semantics, generalized).

**Contradiction resolved:** the turn goroutine blocks (not "parks without
blocking"); the *service call* and the TUI loop do not block. `awaiting_approval`
is a top-level state, not a sub-state (v1 was wrong on both counts).

**D5-in-P0 is a choice, not a necessity** (panel, acknowledged): it is not
implied by the literal import gate — the resolver-bridge (D22) would close the
import gate without it. It *is* required by the chosen "event-only, no interim
seam" acceptance. Stated as such.

### D26 — Facade, DTO inventory, event pump

**Package home (pinned).** The facade contract lives in a **new agent-free
package** `internal/core/sessionclient` (interface + neutral DTOs), importing
`event` and `proxy` leaf types only. `internal/tui` consumes it; `internal/
wiring` implements it. This avoids the panel's flagged trap: if the interface
lived in `internal/wiring`, `tui → wiring → agent` is a transitive agent leak
that passes an import guard but launders agent types through wiring's API.

**DTO inventory (the un-priced cost, now sized).** Only `agent.*` types need
neutralization — the TUI already imports `proxy` legitimately. The mirrors:

| agent type (leaks into TUI) | Neutral home |
|---|---|
| `agent.App` (47×) | removed — facade interface |
| `agent.Control`/`agent.StateApply` | removed — facade methods |
| `agent.ConfirmReqMsg`/`ConfirmChoice` | `sessionclient.ApprovalRequest`/`Choice` |
| `agent.ContextLimit` (12×) | `sessionclient.ContextLimit` |
| `agent.ConsentSnapshot` (9×) | `sessionclient.Consent` |
| `agent.Session`/`SessionScope` (resume) | `sessionclient.SessionSummary` |
| `agent.BackendInfo` (2×) | `sessionclient.Backend` (or proxy leaf) |
| `agent.SideQuestionID` (5×) | `sessionclient.OpID` |
| `agent.RepoState` (SaveRepoState cb) | absorbed into facade methods (no callback) |
| `agent.Msg` types (~30) | replaced by `event.Event` payloads (already neutral) |
| `agent.Truncate/ShortID/DerefStr/Indent/Yellow/TranscriptSize/StrPtr/BuildHandoffContext` | move to a neutral util (`internal/core/format` or `internal/tui`-local) |

`proxy.Message`/`Tool`/`ImagePart`/`GroundingEntry`/`CostTracker` stay as-is (the
TUI already depends on `proxy`; Gate #1 does not touch it).

**Snapshot, not live getters** (panel answer to Q4): the facade exposes an
**immutable `ClientSnapshot`** (session ID, transcript, consent, backend, context
limit, costs, workflow, pending images, raw-tools flag, selected backend) —
atomically constructed, versioned/sequence-stamped, reconciled on events. The
panel's correctness caveat (consent can change mid-turn via a deferred grant)
is handled by version-stamping + re-read-on-turn-complete; consent is the one
hot field the facade may expose as a live read alongside the snapshot. The
per-keystroke completion source (`compSrcFromApp`, tui.go:696,701) becomes a
facade `CompletionSource()` reading the snapshot — staleness bounded by one
keystroke, acceptable.

**Client-initiated mutations (named, from grounding #11)** map onto facade
methods: `SetAutoApprove`, `SetAllowDestructive`, `RevokeAuto`,
`SetWorkflow(nil)`, `AppendSystemMessage`, `SaveSession`, `ConsumeStartupNote`,
`ReplacePendingImages`/`AddPendingImage`/`ClearPendingImages`, `SetCtxLimit`,
`SetModelList`, `SetTools`, `SetInfoPanelOpen`. (7b2/7b3 decide which are
facade ops vs. host commands; the *list* is complete here.)

**Event pump + subscription recovery (new).** A pump goroutine drives
`EventSubscription.Next` and posts to the Bubble Tea loop via `Program.Send`.
On `ErrSubscriptionGap` (slow subscriber), the TUI resubscribes from its
last-seen cursor and reconciles against a fresh `ClientSnapshot`. After
rotation, the pump is torn down and rebuilt so old-session events cannot enter
the new model.

### D27 — Rotation through the factory, with explicit state-carryover

`/new`/`/resume`/`/handoff` swap the whole `(App, Host, subscription)` triple.
The factory is **not** always "fresh":

- **`/new`** — fresh App, but with a **state-carryover list** (the parity
  landmine the review flagged): consent/auto mode **survives** (same workspace,
  same user intent); **pending images survive** `/new` on purpose (documented in
  NewConvMsg handler, tui_agent_msgs.go:753-754) — a fresh App loses them unless
  explicitly transferred; model list, selected backend, cost tracker, and input
  history carry over. Deferred `/auto` grants and tabs do **not**.
- **`/handoff`** — fresh App; consent/auto survives; pending images and workflow
  are **cleared** (documented divergence from `/new`).
- **`/resume`** — **not** a fresh App: `NewSessionAppFromSession(cfg, exe, opts,
  loadedSession)` builds an App pre-loaded with the persisted transcript/
  chatID/workflow (`ResumeSessionMsg`'s mutation, done at construction). This
  corrects v1's "fresh App" error.

The explicit carryover table is an exit criterion (§Exit), not an implementation
surprise.

### D28 — Workflow model (the host-sees-one-turn tension, resolved)

Today the TUI starts a **separate `RunTurn` per workflow step** (`WFStartTurnMsg`
→ `RunTurn`) and a separate `RunFinalReview`. Under the host, `DriveTurnWith
Resilience` does neither. Decision:

- The adapter gains a **TUI-mode TurnFunc** = `DriveTurnWithResilience` +
  `HandleWorkflowTransition` (+ `HandleFinalReview` when the final-review gate
  fires). This is the headless single-task TurnFunc **plus** the workflow step
  the headless path deliberately omitted (chunk 7's D16).
- Each `SubmitInput` = one workflow step's turn, exactly as today: after the
  turn, `HandleWorkflowTransition` runs; if a transition fires, the adapter
  emits `workflow_turn_started{UserText}` (durable) and completes the turn with
  `WorkflowWillContinue: true` (a new field on `TurnCompleted`, replacing
  `AgentDoneMsg.WorkflowWillContinue` — the TUI's queue-flush/auto-grant gate).
- The TUI, seeing `workflow_turn_started`, **submits** the next input — the
  turn-per-step boundary and the `WorkflowWillContinue` semantics are preserved,
  not flattened.
- `WFFinalReviewMsg` → a `workflow_final_review` event; the TUI submits a
  final-review input (or the TUI-mode TurnFunc runs `HandleFinalReview` inline
  when the gate fires — chosen at 7b2; parity is the bar).

**Stream-warn parity (flag, not hidden):** today a backend stream error becomes
`AgentDoneMsg{Warn, Err=nil}` and the TUI stays usable. Under the host, a turn
error parks the session in `error` (re-drive needed). The adapter maps the
`DriveTurnWithResilience` error through `HandleStreamError` *first*, so a
retryable stream error that recovers is invisible; a **retry-exhausted** stream
error is the only case that reaches the host as an error. The `Warn` path
(retry exhausted but the user should be told "backend unreachable, /resume to
continue") must be carried — either a `TurnCompleted` `Warn` field or an
ephemeral `session_warning`. Decision deferred to 7b2 with parity as the bar,
flagging that `turn_completed` currently has no warn notion.

### D29 — Side questions and detached async jobs (out-of-turn operations)

`StartSideQuestion` returns `context.CancelFunc` and streams via `EventSink` —
an independent operation, not a turn. Two options, decided at 7b2:
(a) the facade exposes `StartSideQuestion(text) OpID` + `CancelSideQuestion(OpID)`
as **wiring-side operations** that use the session-scoped emitter for
`side_question_progress`/`side_question_completed` (operation-ID correlated);
(b) side questions become a special `SubmitInput` mode. **(a)** is chosen: it
matches the current shape (cloned client, no Conv mutation) and keeps the main
turn clean. Detached async jobs (Mashūra panels, detached shell) likewise emit
through the session-scoped surface with `OriginSessionID` for the post-rotation
guard; session close does **not** cancel them (matching today's detached
semantics), and late completions are filtered by `OriginSessionID`.

## Exit criteria

1. `go build ./...`, `go vet ./...`, `go test -race ./...` green (workspace
   temp/cache dirs — `/tmp` noexec; ENV note).
2. **7b1:** facade contract in `internal/core/sessionclient` is agent-free
   (`go list -deps ./internal/core/sessionclient` shows no `internal/agent`);
   `appOwners` release path tested (claim → release → re-claim succeeds, and
   release-before-turn-complete is rejected); command classification covers
   **every** slash command in a written matrix (owner, output, async, rotation
   behavior, parity expectation).
3. **7b2:** every new event kind passes the payload-type/Validate/completeness
   tests; session-scoped emitter legal after turn completion and fenced at
   session close; async approval round-trips (`ApprovalRequested` →
   `RespondToApproval` → `ApprovalResolved` → resume) under `-race`, including
   cancel-during-approval → forced decline; headless sync-mode parity unchanged.
4. **7b3 — Gate #1:** zero `internal/agent` import in `internal/tui` non-test
   source (AST/type-level guard + `go list -deps`), and `cmd/wakil/main.go` no
   longer imports `agent`; facade signatures and DTOs carry no `agent.*` type
   (not just no import statement).
5. **Replay correctness:** a resumed/reconnected TUI reconstructs the user+assistant
   transcript and approval/operation terminal states from
   `user_message_committed` + `MessageCommitted` + durable events.
6. **Rotation parity:** the carryover table (D27) is verified — consent/auto and
   input history survive `/new`+`/handoff`; pending images survive `/new` and
   clear on `/handoff`; workflow clears on handoff; `/resume` loads the persisted
   transcript.
7. **Subscription recovery:** `ErrSubscriptionGap` → resubscribe+cursor+snapshot
   reconcile; no old-session event leaks into the model after rotation.
8. **Lifecycle/leak:** close-while-awaiting-approval, release-while-cancelling,
   detached-job-after-turn, side-question-during-rotation — no goroutine/
   resource leak (leak checks).
9. **Parity:** TUI behavior unchanged through the boundary — streamed response,
   tool call + approve/decline, stream error, cancel/interrupt, suspended+
   resume, subagent tabs, async-job tabs, side question, compaction, rotation.
10. **Schema limits/privacy:** bounded text/list sizes on every new payload;
    tool command text redacted (never raw in durable); stable IDs + correlation
    on every new event.

## Test plan

- **7b1:** factory claim/release lifecycle; release-safety (reject release while
  a turn is active); command-classification matrix over the full slash-command
  set (assert each returns the neutral `CommandResult`, no `agent` type).
- **7b2:** event schema completeness+validation for new kinds; session-scoped
  emitter (emit-after-turn-legal, fenced-at-close); async-approval state machine
  (pending → resolve → resume; duplicate; unknown; cancel/close-during-approval →
  forced decline; append-failure → turn error); headless sync-mode regression.
- **7b3:** structural guard + guard-detects-regression (mirrors
  `cmd/wakil/headless_seam_test.go`, for `internal/tui`); a TUI integration test
  driving a fake host (create→subscribe→submit→project→render); approval-answer
  (TUI keys → `RespondToApproval` → resume); rotation carryover table test;
  `ErrSubscriptionGap` recovery test; event-pump leak test.
- **Parity:** existing `internal/tui`/`internal/agent`/`cmd/wakil` suites
  unchanged for non-re-routed paths; `--plan` (7c) and headless (7) unaffected.
- **Lifecycle/leak:** the §Exit-criteria-8 matrix.

## Package home / import direction

- `internal/core/sessionclient` (NEW): facade interface + neutral DTOs; imports
  `event` + `proxy` only.
- `internal/core/event`: new kinds + payloads (7b2).
- `internal/core/sessionhost`: session-scoped emitter + real
  `RespondToApproval` + `awaiting_approval` park + pending-approval tracking
  (7b2).
- `internal/agent`: TUI-mode turn loop pieces (`HandleWorkflowTransition`/
  `HandleFinalReview` re-used, not moved); no new `*App` coupling.
- `internal/wiring`: per-session factory + release + command dispatcher (7b1);
  session-scoped projection + async resolver + TUI-mode TurnFunc (7b2);
  sessionclient implementation (7b3).
- `internal/tui`: drops `internal/agent` (7b3); consumes `sessionclient` + `event`.
- `cmd/wakil`: `main.go` drops `agent` (7b3); TUI bootstrap via wiring.

Import direction: `tui → (sessionclient, event)`, `cmd/wakil → (wiring, tui)`,
`wiring → (agent, core*, sessionclient-impl)`, `tui ↛ agent`, `tui ↛ wiring`.
No cycle.

## Honest gaps / risks (not hidden)

- **`--plan` remains direct** (chunk 7c). The TUI's workflow loop is re-routed
  here (D28) but the workflow *engine* still mutates App inside the turn — the
  host sees one turn per workflow step, which is preserved semantics, not a
  flattening.
- **7b3 is the largest single cut** (~70 read sites + a ~30-message switch +
  DTO mirrors), even with 7b1/7b2 done. It may need internal milestones; flagged
  now.
- **Async approval is a P2 feature pulled into P0** (D25); executor-parking
  interacts with `Interrupt`/`CloseSession`. Riskiest sub-chunk; the
  resolver-bridge is the documented retreat if needed.
- **Stream-warn parity** (D28) has no `turn_completed` warn notion yet; deferred
  to 7b2 with parity as the bar.
- **Event-schema churn** is now *smaller* than v1 (query-state moved to
  snapshot, not events), but the durable additions (`user_message_committed`,
  `workflow_turn_started`, `async_job_*`, `side_question_*`,
  `conversation_compacted`) are real P1 migrations. Sized honestly.
- **Snapshot read timing** changes subtly vs. today's direct App reads; covered
  by behavior tests, not assumed equivalent.

## Resolved open questions (v1 Q1–Q5 → decisions, per panels)

1. **Command home** → D23: agent-free facade + neutral `CommandResult`; full
   event-ification of command results is out of scope for P0.
2. **Executor mechanics** → D25: block the turn goroutine on a decision channel
   raced with ctx; host flips to `awaiting_approval` via a new hook; cancel →
   forced decline before the emitter fence.
3. **Subagent richness** → D24: bounded neutral fields in payloads (split by
   replay need); no maximal schema; no TUI-side ChatID query.
4. **Snapshot vs. live** → D26: immutable version-stamped snapshot, reconciled
   on events; consent is the one live-read exception; per-keystroke completion
   source reads the snapshot.
5. **TokRate/learn-nudge/side-question** → D24/D29: tok_rate ephemeral;
   learn-nudge ephemeral (not folded into `turn_completed`); side-question is an
   explicit operation with op-ID + ephemeral progress + durable completion.

## ENV note (carry forward)

`/tmp` is `noexec`; `go test -race` needs
`GOTMPDIR=/mnt/wakil/.gotmp GOCACHE=/mnt/wakil/.gocache`. Discovery subagents die
on context limits — prefer inline grep. Full suite = 29 packages.

## §Revision (fold-ins from Mashura review op-10 — gpt-5.6-sol, claude-fable-5, glm-5.2)

Three panels converged. Folded in:
- **Architecture pinned (W3):** one Host per conversation; no routing TurnFunc;
  the v1 "per-session factory without a Host model" gap closed (Scope, D22).
- **`DispatchCommand` withdrawn** — `agent.Cmd` return is a Gate #1 violation
  (`cmd.go`: `Cmd = func() Msg`, `Msg = any`). Replaced by D23's agent-free
  `CommandResult` + five-way command classification.
- **D5-in-P0 reframed as a choice, not necessity** — the resolver-bridge
  alternative is named and explicitly rejected with reasons + a documented
  fallback (D22).
- **Async-approval contradiction resolved (D25):** turn goroutine blocks on a
  decision channel; `awaiting_approval` is a top-level state set via a new host
  hook; cancel-during-approval → forced decline before the emitter fence.
- **Session-scoped emitter (the biggest missing step):** turn-scoped `Emitter`
  cannot carry `WFStartTurnMsg`/`AsyncJobDoneMsg`/`SideQuestionDoneMsg`; new
  host surface + `app.EventSink`/`OnTokRate` wired to it, not `in.Emit` (D24).
- **Replay truth:** `user_message_committed` durable event + `ClientSnapshot`
  transcript; "session_created + replay" is no longer claimed true as-is.
- **Event schema shrunk:** five-class split; `model_list_updated`/
  `context_limit_resolved` dropped (snapshot fields); `learn_nudge` removed from
  `turn_completed` (host-reserved); correlation IDs + `OriginSessionID`;
  subagent fields bounded.
- **Tool-command redaction** — `CommandPreview`, never raw command in durable.
- **Facade home + DTO inventory pinned:** `internal/core/sessionclient`
  (agent-free), full `agent.*` mirror table, `proxy.*` stays, snapshot over
  live getters, event pump + `ErrSubscriptionGap` recovery.
- **Rotation state-carryover table (parity landmine):** `/new` vs `/handoff` vs
  `/resume` divergence; `/resume` = `NewSessionAppFromSession` (not fresh).
- **Workflow model (D28):** TUI-mode TurnFunc; `WorkflowWillContinue` on
  `TurnCompleted`; `workflow_turn_started`/`workflow_final_review`; stream-warn
  parity flagged.
- **Side question + detached jobs (D29):** operation-ID correlated, session-
  scoped, `OriginSessionID` guard, session-close does not cancel detached work.
- **Exit criteria + tests expanded** with the panels' 8 additions (ownership
  model, agent-free DTOs, replay, subscription recovery, lifecycle/leak, command
  matrix, schema limits/privacy, exact structural gate + `go list -deps`).
