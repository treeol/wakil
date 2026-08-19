# Card #148 P0 — Chunk 5 plan: event-emission seam + agent-loop adapter

Status: DRAFT v2 (Mashura-reviewed; feedback folded in — see §Revision)
Branch: `feature/wakild-daemon`
Parent plan: `docs/cards/card-148-wakild-impl-plan.md` (deliverables 4 + 6)
Inventory: `docs/cards/card-148-d10-inventory.md`

## Scope (v2, narrowed after review)

Chunk 5 delivers the **turn-scoped event-emission seam** and a **single-session
agent adapter** that runs a real `*agent.App` turn and translates its outbound
signals into domain events. It is intentionally the smallest honest increment
that proves the command/event boundary end-to-end, and it is **additive**: the
production TUI (`RunTurn`) and headless (`runHeadlessApp`) paths are *not*
re-routed through the host in this chunk — they keep working exactly as today.

**In scope:**
1. `sessionhost`: a turn-scoped `Emitter` (durable + ephemeral), serialized
   append→notify, host-owned-kind allowlist, turn fencing, `internal_error`
   failure classification, `TurnInput.UserID` (submitter).
2. `internal/wiring` (new): the adapter `HostTurnFunc` that installs/restores
   the agent's output callbacks, runs the turn loop via the exported
   `App.SendOutcome`/`WaitForAsyncCompletion`/`Resume`, translates stream and
   reasoning output to deltas, and returns the authoritative message text.
3. Approval shim (deliverable 6): a context-aware confirmer that emits
   `ApprovalRequested` → `ApprovalResolved` with full outcome fidelity.
4. Deterministic host-side contract tests + an integration test that drives a
   real (fake-backend) `*agent.App` turn through `SessionService`/`EventReader`.

**Explicitly deferred** (documented, not hidden):
- **Tool-call and subagent durable events.** `ToolStartMsg`/`ToolResultMsg`
  carry no domain `ToolCallID`, arg digest, status, or duration; fabricating
  those into the *durable* audit log would pollute it (panel #12/#13). They stay
  on the legacy `EventSink` in this chunk; teaching the agent to emit them with
  real IDs/digests/status/duration is its own chunk. The domain vocabulary
  already supports them — the agent must be taught to produce them.
- **Multi-session App factory.** One `*agent.App` is one mutable conversation;
  the adapter supports exactly one embedded host session (asserted, fail-loud on
  reuse). An App-per-session factory is a deliverable-5 concern.
- **Severing TUI→App mutation (deliverable 5), headless JSON projection
  (deliverable 7), async approval rework (P2).**

## Revised design decisions

### §1 Turn seam — `Emitter`, not a flat callback

`TurnFunc`'s signature is **unchanged** (`func(ctx, input TurnInput) (string,
error)`); the seam is additive: `TurnInput` gains one field.

```go
// Emitter lets a turn publish events while it runs. Safe for concurrent use;
// turn-scoped: the host closes it at finalization, after which Emit returns
// ErrEmitterClosed and Notify drops. Durable events are appended (seq assigned
// by the store) and delivered to subscribers; ephemeral events are streamed
// live-only, may be dropped, and never appear in ListEvents.
type Emitter interface {
    Emit(kind event.Kind, payload any) error  // durable only; host-owned kinds rejected
    Notify(kind event.Kind, payload any)      // ephemeral only; best-effort
}

type TurnInput struct {
    ...
    UserID event.UserID // submitter principal (for ApprovalResolved.Resolver)
    Emit   Emitter      // turn-scoped publisher (nil in plain lifecycle tests)
}
```

- **Serialization (§ append→notify atomicity, panel-shared).** The host's
  durable append→notify is NOT atomic across producers today; adding concurrent
  emitters would let two producers interleave `notify` (seq 11 delivered before
  seq 10). Fix: a per-session `emitMu sync.Mutex` held across append *and*
  notify. `emitDraft` (host) and `Emitter.Emit` both take it, so all producers
  serialize and subscriber order is total. `notifyEphemeral` does **not** take
  it (ephemeral events are unordered/droppable), but uses the same subscriber
  fan-out.
- **Kind allowlist.** `Emit` rejects host-owned kinds (`SessionCreated`,
  `TurnStarted`, `MessageCommitted`, `TurnCompleted`, `SessionError`,
  `SessionClosed`) and any ephemeral kind; `Notify` rejects durable kinds.
  Rejection is a normal `error` (ErrInvalidInput), never a panic.
- **Validation (fixes panel #1).** `Emit` relies on `Store.Append`'s
  `ValidateDraft` (already implemented) after its own class/allowlist check — it
  does **not** call `event.Validate` (which is `ValidateCommitted` and rejects
  seq-0 durable drafts).
- **Fencing (panel #3).** The host builds one fresh emitter per turn and closes
  it inside `finishTurn` (after the host emits `MessageCommitted`/`TurnCompleted`).
  Chunk 5 routes only *synchronous* turn signals through the emitter (stream,
  reasoning, approval — all on the executor goroutine), but a test asserts the
  fence contract: a durable `Emit` after `TurnCompleted` is impossible by
  construction; a concurrent worker's late `Emit` returns `ErrEmitterClosed`.
- **Ephemeral delivery.** New `notifyEphemeral`: build an ephemeral `Event`
  (Seq 0, `ValidateCommitted`), push to subscribers (drop-on-full, never
  disconnect). `Notify` checks `ctx`-cancellation is NOT applied — see §4.

### §2 Failure classification — `internal_error`, not `backend_failure`

Today `finishTurn` maps any non-cancelled turn error to `stream_error` →
`SessionError{backend_failure}`. An adapter/store failure is not a backend
failure. Add sentinel `ErrEmitFailed` (wrapped by the emitter on append failure)
and have the adapter wrap it. `finishTurn` checks `errors.Is(turnErr,
ErrEmitFailed)`: still `TurnCompleted{stream_error}` (the outcome enum is a
turn-result fact, unchanged) but `SessionError{reason:"internal_error"}` and the
session still parks in `error` state. Chunk 4's `backend_failure` behavior and
state transitions are otherwise untouched.

### §3 Message text — authoritative source, honest delta contract (panel #11/#15)

- **`MessageCommitted.Text` = `TurnOutcome.Text`** (the `final` returned by
  `streamTurn`), returned by the adapter from `SendOutcome`/`Resume`. It is the
  authoritative assistant response.
- **`MessageDelta` is presentation streaming**, produced from `App.Out` chunks,
  and *may include tool/status rendering lines* — it is **not** guaranteed to
  concatenate to `MessageCommitted.Text`. Stated in the payload docs; the
  host-owned `MessageCommitted` stays the replay truth (D2 coalescing).

### §4 Context — durable appends survive cancellation; new emissions don't start

- Durable `Emit` uses the session's host-owned context (like `emitDraft`'s
  `context.Background()` today): a logically-produced durable event that was
  already queued still commits, preserving start/complete pairs.
- The **fence**, not ctx cancellation, is what stops late durable emissions.
- Ephemeral `Notify` is best-effort throughout (droppable by contract); it does
  not consult the turn ctx.

### §5 Approval shim — context-aware, full-fidelity, new confirmer (panel #6/#7/#8)

The legacy `tuiConfirmer`/`headlessConfirmer` cannot be wrapped: `Confirmer`
returns `bool`, and `ChoiceAllowReads` is resolved inside `tuiConfirmer`
(commands.go:122–130), invisible to any wrapper. They also block on `<-ch` with
no ctx select. So the shim is a **new, context-aware confirmer owned by the
wiring adapter**, used only in the host-driven path (the legacy paths are
unchanged):

- Mint `ApprovalID` via `id.NewApprovalID()` (already exists).
- `Emit ApprovalRequested{ApprovalID, ToolName, Headline, Detail, ReadAction}`
  (durable).
- Consult a `resolver func(ConfirmChoice) bool` (injected; the integration test
  supplies one; a production host in P2 supplies the wire path) that carries the
  full choice, *selecting on `turnCtx.Done()`*.
- Map choice → `ApprovalResolved{Outcome}`: `ChoiceApprove`→`approved`,
  `ChoiceDecline`→`declined`, `ChoiceAllowReads`→`allowed_reads`; **ctx
  cancellation → `declined`** (documented P0 semantic: cancellation is a deny
  for approval purposes; the enum stays closed).
- `Resolver` = `TurnInput.UserID` (submitter), not a hardcoded embedded
  principal. Fix the now-stale `ApprovalResolved.Resolver` doc comment in
  `payloads.go` (it claims the P0 shim leaves it empty).

**Known, documented P0 gap (unchanged):** a client that observes a durable
`ApprovalRequested` still cannot answer it — `RespondToApproval` returns
`ErrApprovalNotFound` until P2. The events are *observation* notifications (D5),
not an authoritative pending-approval state machine.

### §6 Wiring / callback lifecycle (panel #5)

- The adapter, on each turn, snapshots `App.Out`, `App.Confirm`,
  `App.OnReasoning`, `App.OnTokRate`, `App.EventSink` and restores them with
  `defer` (panic-safe). It installs: `Out` → `MessageDelta` writer; `OnReasoning`
  → `ReasoningDelta`; `Confirm` → the §5 confirmer; `OnTokRate`/`EventSink` →
  a no-op collector in the host path (legacy parity is preserved because the
  production paths never use the adapter in this chunk).
- `App.EventSink` is **not teed** in this chunk because the host-driven path has
  no TUI Program to tee to — the adapter owns the sink. The one-to-one
  constraint (§Scope) prevents simultaneous legacy and host use of one App.

### §7 Package home / import direction (panel #17)

- `sessionhost` stays pure of `internal/agent` (it gains `Emitter`,
  `ErrEmitterClosed`, `ErrEmitFailed`, per-session `emitMu`, `notifyEphemeral`,
  `TurnInput.UserID`).
- New package `internal/wiring` holds the adapter (`HostTurnFunc`) and imports
  `agent` + `core` + `core/event` + `core/id`. **No agent change needed**: the
  adapter drives turns through the already-exported `App.SendOutcome` →
  `WaitForAsyncCompletion` → `Resume` loop (replicating the 12-line
  `runTurnToFinal` once, since that helper is agent-unexported).
- D12 structural check: `go list` proves `internal/core` imports neither
  `api/gen`, `internal/server`, `internal/agent`, nor bubbletea; `sessionhost`
  imports neither `agent` nor `tui`.

## Exit criteria (v2 — acceptance criteria the panels demanded)

1. `go build ./...`, `go vet ./...`, `go test -race ./...` green.
2. **Durable ordering:** concurrent `Emitter.Emit` from multiple goroutines is
   observed in exact increasing Seq order by a subscriber (deterministic test).
3. **Terminal ordering:** no turn-scoped durable event can appear after its
   `TurnCompleted` (fence test: late Emit → `ErrEmitterClosed`).
4. **Allowlist:** host-owned and ephemeral kinds are rejected by `Emit`;
   durable kinds rejected by `Notify`.
5. **Ephemeral semantics:** ephemeral events have Seq 0 and never appear in
   `ListEvents`/`SessionSnapshot`.
6. **Approval pairing:** each emitted `ApprovalRequested` is followed by exactly
   one `ApprovalResolved` with the correct approve/decline/allow-reads outcome;
   cancellation-while-blocked resolves as `declined`.
7. **Final text:** `MessageCommitted.Text` equals `TurnOutcome.Text`, asserted
   against the fake backend's response.
8. **Failure classification:** an emitter/store failure produces
   `SessionError{internal_error}`, not `backend_failure`.
9. **Callback cleanup:** adapter restores `Out`/`Confirm`/`OnReasoning`/`OnTokRate`/
   `EventSink` after success and error.
10. **Single-session constraint:** reuse of one App across two host sessions is
    rejected loudly; integration test uses one session (two-session isolation is
    asserted at the host level, not the adapter level, this chunk).
11. **Parity:** the legacy TUI/headless paths are byte-for-byte unchanged (no
    `internal/tui`, `cmd/wakil` production file is modified).
12. **Sequence assertion scope:** strictly-increasing-seq assertions apply to
    durable events only.

## Verification / test plan

- Host-side deterministic tests in `internal/core/sessionhost` (stub TurnFunc
  that calls `Emit`/`Notify`): ordering, fence, allowlist, ephemeral-vs-durable,
  `internal_error`, `UserID` plumbing. These need no backend.
- Adapter integration test in `internal/wiring`: a real `*agent.App` against an
  `httptest` fake SSE backend (same pattern as agent's own tests), driven through
  `CreateSession`/`SubmitInput`/`Subscribe`, asserting the durable sequence
  `TurnStarted → [ApprovalRequested → ApprovalResolved]* → MessageCommitted →
  TurnCompleted` with correct text and strictly increasing seq. Answer approvals
  via an injected `resolver` (the documented P0 carve-out: `RespondToApproval` is
  a stub this chunk).

## §Revision (fold-ins from Mashura review op-5)

Three panels (gpt-5.6-sol, claude-fable-5, glm-5.2) converged. Folded in:
validation call fixed (Append validates drafts); append→notify serialized via
per-session `emitMu`; turn fencing + late-emission contract; `internal_error`
classification; single-session constraint made explicit; callback snapshot/
restore specified; ctx-aware approval confirmer with full outcome fidelity and
cancellation→declined; `TurnInput.UserID` for resolver identity + stale payload
comment fix; authoritative message text = `TurnOutcome.Text` with an honest
delta≠committed contract; tool/subagent durable events deferred (no fabricated
digest/status); `go list` structural check added; host-owned kind allowlist; no
new package under `internal/core` that imports `agent`.