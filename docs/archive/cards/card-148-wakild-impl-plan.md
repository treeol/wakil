# Card #148 — wakild Foundation: Implementation Plan (FINAL, Mashura-gated)

Status: FINAL — plan only, not implemented. STOPPED per instruction.
Branch: `feature/wakild-daemon`
Card: https://trello.com/c/Ba4YYGXM
Design doc: `docs/design/wakild-foundation.md`
Reviewed by: Mashura (gpt-5.6-sol, claude-fable-5, glm-5.2) — see "Mashura feedback folded in" below.

---

## 0. Grounding — verified current state (corrected after review)

V1. **`internal/agent` is transport-free but not service-ready.** `agent.App`
(app.go) imports no bubbletea (confirmed by adapter.go header comment + import
block). Two frontends exist: TUI (`tuiModel.app *agent.App`) and headless
(`cmd/wakil/run.go`). **But** "no bubbletea import" ≠ "stable service
boundary": `App` is a host object coupling config, `proxy.Client`, executor,
persistence, telemetry, consent, workflow, and UI-display state, with public
mutable fields consumed directly by both frontends.

V2. **An untyped, unsequenced event channel exists.** `App.EventSink
func(interface{})` + `sendEvent` (app.go:347–350, 546–551) posts ToolStartMsg,
confirm requests, etc. to the TUI. Embryonic `StreamEvents`, but callback-based
and TUI-vocabulary-specific.

V3. **Session persistence is per-chat JSON files**, `~/.local/share/wakil/
sessions/<chatID>.json` (session.go:48), atomic-write. The doc's "593 files"
baseline question is **not** confirmed to be about these JSON files — the trace
package also creates per-session JSONL files, so the discrepancy may concern
traces instead. Marked unverified (was overstated in the draft).

V4. **Trace JSONL has most telemetry fields the doc wants to "add" — but NOT
`context_limit`.** `trace.Record` (trace.go:26) has `backend`, `reasoning_chars`
AND `reasoning_tokens`, `input/output_tokens`, `outcome`, `sft_eligible`. It does
**not** have `context_limit`. So §11 step 4 is *mixed*: `backend` = population;
`reasoning_*` = unification *decision* (still dual-unit); `context_limit` =
genuine schema addition; `sft_eligible` = policy/consent semantics, not
population (Store.Write forces it false, trace.go:103–107).

V5. **Doc claims not yet verified:** "593 files", "21.9% stream_error",
"backend ~35%", "p99 816k" — baseline numbers not reproduced against this
repo's data. `LastUsedBackend()` may legitimately return "" (the `backend` field
is `omitempty`); whether ~35% is a capture bug or genuine header absence
(`X-Ilm-Backend-Used`) is only answerable from the JSONL files.

---

## 1. Decisions made (the review forced these; no longer open)

**D1 — P0 is daemon-shaped, not a blocking façade.** The per-session executor
(single goroutine + input queue, §5.1) lands in P0 as an in-memory session host.
`SubmitInput` is genuinely non-blocking and returns `TurnAck{turn_id}`. Rationale:
the command/event split is the doc's "wichtigster Design-Entscheid"; shipping a
blocking `SubmitInput` that just wraps `SendOutcome` would make P2 rewrite the
whole boundary — exactly the §10 "Jede Abkürzung hier wird in P2 doppelt
bezahlt" warning. Accepted consequence: P0 is larger than "mechanical."

**D2 — Two event classes.** (a) **Durable domain events**: replayable,
cursor-addressable, strictly sequenced per session; include a
`MessageCommitted`-style event so replay has a semantic equivalent of live
deltas. (b) **Ephemeral stream notifications**: live-only (e.g. token
`MessageDelta`), not part of durable sequence semantics, may be dropped. Live
rendering consumes both; replay consumes (a) only. P0 uses an in-memory
`EventAppender`/`EventReader`; P1 swaps in SQLite — the interface is designed
for that swap now.

**D3 — Sequencer is an interface.** `Seq` assignment is store-pluggable:
in-memory (per-session counter owned by the executor) in P0, store-backed
(`sessions.last_seq` + `events(session_id, seq)` transactional) in P1. P0 does
**not** hardcode an `atomic.Uint64` on `App`. Sequence is assigned at durable
append (in P0: at append to the in-memory log, serialized under the executor
lock, so concurrent producers cannot interleave). Durability-before-visibility
semantics are defined in the EventAppender contract; P1 makes append+last_seq
one transaction.

**D4 — Tenancy and identity from P0.** No empty tenant. Embedded mode resolves a
constant principal `{tenant: "tnt_local", user: "usr_local", role: "owner",
auth_method: "embedded"}`. Service methods take a `Principal` (or it is carried
on the session host) from day one, so P4 does not retrofit signatures. Domain
session_id is a typed `ses_<uuidv7>`; the proxy `ChatID` is an **external
backend correlation field**, not the domain identity (verified against proxy
requirements in P1 before finalizing).

**D5 — Sync Confirmer → async approval: SHIM in P0.** The synchronous
`Confirmer` callback stays internal (the embedded single-user gate), but every
approval gate ALSO emits typed `ApprovalRequested`/`ApprovalResolved` domain
events as notifications, so multi-client observation works. The full async
rework (parking a turn mid-tool-loop, `RespondToApproval` driving resolution
over the wire) lands in P2 when a wire exists. Documented as a known P0
boundary, not hidden.

**D6 — Events are data-only.** No channels, no callbacks, no reply channels in
domain events (P2 puts a wire between producer and consumer). One envelope
`{tenant_id, session_id, seq, ts, oneof payload}` with typed payloads — not one
struct per kind duplicating envelope fields. A validation layer rejects invalid
kind/payload combinations (Go has no closed ADTs).

**D7 — Service interface is split into three, behavior-first, leak-free.**
`SessionService` (CreateSession/SubmitInput/RespondToApproval/Interrupt/
CloseSession), `EventReader` (Subscribe/ListEvents), `SessionReader`
(GetSession/ListSessions/SessionSnapshot). Construction/bootstrap (Config,
`proxy.Client`, executor, trace sinks, persistence) is separate from runtime
commands and never leaks through the interfaces. `Fork` is **deferred** out of
P0 (no turn-boundary substrate exists today; it adds persistence semantics
without helping sever the boundary).

**D8 — §11 step 4 is three separate pieces, gated on a real audit.** (i)
`backend` population — verify against JSONL data first; (ii) `reasoning_*`
unification — a schema/semantics decision (single field, documented unit,
adapter normalization); (iii) `context_limit` — net-new schema field + populate
from `App.CtxLimit`. Plus: `sft_eligible` stays a policy question (current
Boolean cannot express "unknown"; consider an eligibility-status enum), not a
backfill. **Audit first:** a script over the real trace dir reporting
record counts by type, backend-empty rate by outcome/model, reasoning field
presence, `context_limit` availability, malformed lines, drop evidence.

**D9 — P1 replay criterion reframed (option 2, not full event sourcing).**
The event log is an audit/rendering log alongside normalized session tables and
snapshots, NOT the sole source of all executable state (conv, workflow,
consent, images, pending async work are too much to event-source). Criterion:
> Replay reconstructs the client-visible session projection exactly; a persisted
> session snapshot + normalized state is sufficient to resume execution.

**D10 — The TUI→App mutation inventory is a named P0 artifact with a
checklist**, not a step that produces one. Must cover: direct field reads/
writes, `App.Out`, `App.OnReasoning`, `App.Confirm`, `EventSink`, `agent.Cmd`
messages, `PendingImages`, `InfoPanelOpen`, session load/save calls, workflow
commands, headless-only flags/JSON events, async-op wait/shutdown.

**D11 — Package layout: do NOT rename in P0.** `internal/agent` stays put;
`internal/core/event` (domain event types) and `internal/core/service.go`
(interfaces + request/response types) are introduced. Renaming
`internal/agent` → `internal/core/agent` (100 files) is pure churn with no
behavioral value and is deferred until the boundary is proven. Noted as a
doc-layout deviation.

**D12 — `internal/core` dependency guard is structural, not just "no tea."**
Acceptance test: outside the bootstrap package, neither `internal/tui` nor
`cmd/wakil` may hold or import `*agent.App`; core/domain packages must not
import bubbletea or generated API types. Enforced by a `go list`-based test or
architectural test, not convention.

---

## 2. P0 deliverables

1. `internal/core/event` — domain event envelope + typed payloads (D6), two
   classes (D2), validation (D6).
2. `internal/core/service.go` — `SessionService` / `EventReader` /
   `SessionReader` (D7), `Principal` + typed IDs (D4), `EventAppender`/
   `EventReader`/sequencer interfaces (D3).
3. In-memory session host — per-session executor goroutine + input queue,
   non-blocking `SubmitInput → TurnAck` (D1), `Interrupt`/`CloseSession`
   lifecycle, crash-recovery stub (sessions in `running` → `error` + event).
4. Convert **all** agent→client output paths to domain events: `EventSink`
   retyped, `App.Out`/`OnReasoning`/`agent.Cmd` messages/headless JSON events
   become projections of domain events or explicit command results (D10).
5. Sever TUI→App direct mutation per the D10 inventory; move control surface
   (`SetAutoApprove`-style) behind Service methods; display-only state
   (`InfoPanelOpen`, cursor/scroll/tabs) stays TUI-owned.
6. Approval shim (D5) — emit ApprovalRequested/Resolved events around the
   existing sync Confirmer.
7. Parity tests: TUI and headless behavior unchanged through the new boundary.

## 3. P0 exit gate (concrete)

1. Neither `internal/tui` nor `cmd/wakil` holds/imports `*agent.App`.
2. A headless program drives a full session through the service boundary only.
3. Tested paths: normal streamed response, tool call, approval requested +
   resolved, stream error, cancel/interrupt, suspended turn + resume.
4. Events have unique, strictly-increasing per-session `seq` under concurrent
   production.
5. Two in-memory subscribers receive the same durable event order.
6. Slow-subscriber behavior is documented (never silently blocks the executor).
7. `go test -race ./...` passes for event + interruption paths.
8. Dependency guard test (D12) green.
9. Replay of durable events reconstructs the client-visible projection (D9).

## 4. P1–P5 sequencing (confirmed, corrected)

P1: SQLite control-plane + event log (swap in-memory appender for store-backed
sequencer, D3), migrations, tenant-keyed repositories (note: `events` primary
key is `(session_id, seq)` with globally-unique session_id — tenant filtering is
redundant there; resolve index design), replay exit test (D9), cross-tenant
query test. P2: proto + Connect server + `wakild` binary + `embedded` default +
`--daemon` + full async approval (D5 rework) + skew test + `buf breaking`. P3:
read-only web UI. P4: OIDC + tenancy + credential encryption + scrubbing. P5:
UI write-path. Cross-cutting (supply-chain, headless `wakil run`) in parallel
from P1.

Open decisions deferred to before P4 (unchanged from doc §10): working-dir
sharing (leases vs. worktrees), event-log growth policy, multi-node non-goal.

## 5. What remains unverified (honest)

- Baseline numbers (V5) — need a trace-dir audit before any §8/step-4 work.
- Whether ~35% backend absence is capture bug vs. header absence.
- Whether `ChatID` can be demoted to a correlation field without breaking proxy
  resume semantics.
- Full TUI→App mutation list (D10) — will be produced as the first P0 artifact,
  not asserted now.
- Whether `agent.Cmd`/BatchMsg invariants (adapter.go) survive the EventSink
  retyping — to be resolved during step 4.

---

## Mashura feedback folded in

- D1 (blocking vs queued — was mixed, now explicit): queued, in P0.
- D2/D3 (event sequencing/durability semantics — was a bare "seq generator"):
  two classes + sequencer interface + append-at-seq.
- D4 (tenancy deferral contradicted Leitprinzip): constant principal from P0.
- D5 (sync Confirmer → async approval was unbudgeted): shim decision.
- D6 (events must be data-only; envelope not per-kind structs).
- D7 (Service split into three; leak-free; `Fork` deferred).
- D8 (step 4 is not population-only; `context_limit` is a schema gap; audit
  first).
- D9 (P1 replay "fully reconstructed" overstated — reframed to projection).
- D10/D11/D12 (inventory as named artifact; no rename in P0; structural guard).
- Corrected V3 (593-files interpretation unsupported) and V4 (`context_limit`
  genuinely absent — the draft's "partially already implemented" overclaimed).
