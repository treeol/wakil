# Card #148 P0 — Chunk 6 plan: interim TUI→App control seam (deliverable 5, step 1 of 2)

Status: DRAFT v2 (Mashura-reviewed; feedback folded in — see §Revision)
Branch: `feature/wakild-daemon`
Parent plan: `docs/cards/card-148-wakild-impl-plan.md` (deliverable 5)
Inventory: `docs/cards/card-148-d10-inventory.md` (line numbers REGENERATED below — the
committed inventory is stale: it still references `internal/tui/tui_cmds.go`, which no
longer exists; the turn driver is now `internal/agent/tui_cmds.go`.)

## Scope — an interim seam, NOT deliverable-5 completion

Deliverable 5 says: *"move control surface (`SetAutoApprove`-style) behind **Service**
methods."* This chunk does **not** move the control surface behind the D7
`SessionService` boundary. It introduces a **stepping-stone interface** in
`internal/agent` (`Control`, plus a distinct `StateApply` for round-trip results)
that the TUI uses for every mutation of App state, replacing raw field writes and
direct `*App` method invocations. The TUI **still holds `*agent.App`** for reads
and turn-driving, so the D12 exit gate ("neither `internal/tui` nor `cmd/wakil`
may hold or import `*agent.App`") **remains red** after this chunk — as does the
card-level deliverable 5. Stated plainly, not hidden.

**Why ship an interim seam at all:** the chunk is a reversible, test-guarded
structural step that (a) removes the *visible* field-write hazard, (b) fixes two
real defects the review surfaced (Conv locking, `ResumeSessionMsg` hidden
mutation), and (c) leaves the turn-loop rework (the actual Service re-route) to
its own chunk without entangling it here. Its value is **contingent on the
turn-driving chunk landing** (deliverable 7); if that never lands, `Control` is a
second interface that partially overlaps `*App`'s method set — a documented
half-measure, not a goal.

**In scope (this chunk):**
1. `Control` (genuine session/consent/persistence commands) + `StateApply`
   (round-trip runtime results) interfaces in `internal/agent`, implemented by
   `*App`. `tuiModel` gains `control Control` and `apply StateApply` fields, both
   bound to `app` at the single construction site.
2. New `*App` methods so **no TUI mutation is a raw field write**, including the
   operation-specific pending-image methods and the Conv-locked
   `AppendSystemMessage`.
3. Fix `ResumeSessionMsg` (hidden mutation: TUI resume-picker → agent function
   writing `Conv` without `convMu`) — take the lock.
4. A **narrowed, honestly-scoped** structural guard test over `internal/tui`
   non-test source (see §5).
5. Fake-`Control` TUI tests proving routing, not just end-state parity.

**Explicitly deferred (documented, not hidden):**
- **Turn-driving re-route.** `agent.RunTurn`/`RunFinalReview`/`StartSideQuestion`
  still take `*agent.App` and are still invoked by the TUI. Those functions mutate
  `Out`/`Confirm`/`OnReasoning`/`OnTokRate`/`ToolCache`/`WorkflowStepTrace`
  internally (see `internal/agent/tui_cmds.go`). That is the turn-driving chunk's
  business (deliverable 7 / exit gate). This chunk does not touch it.
- **Reads.** The TUI still reads App state (`Consent()`, `Client.ChatID`, `Costs`,
  `Cfg`, `EffectiveModel()`, `ContextLimit()`, `Workflow`, `Conv`,
  `PendingImages`, `RawTools`, `SelectedBackend`). Reads are out of scope; the
  caveat is that reads over *shared mutable* state (`Conv`, `PendingImages`,
  `CtxLimit`, `Workflow`) participate in the races §2 documents and are only
  safely deferrable to the extent they are read-only w.r.t. a field whose writer
  is also deferred. `Conv` is the exception and is handled (see §2).
- **Headless (`cmd/wakil/run.go` + `main.go`).** Same class of work, but it is
  exit gate #2, not deliverable 5's TUI mutation severing.
- **The round-trip** (agent → TUI message → TUI writes back to App): methodized
  onto `StateApply` now, *removed* when turns route through the host (§3).
- **Display-only state** (`InfoPanelOpen`, cursor/scroll/tabs) stays TUI-owned;
  the App mirror exists only for repo-state persistence (§4).

## §1 Regenerated TUI mutation inventory (current HEAD, non-test)

21 sites. `[M]` = existing method call, `[W]` = raw field write, `[H]` = hidden
mutation behind an agent free function the TUI calls.

| # | Site | Expression | Class |
|---|---|---|---|
| 1 | tui.go:509 | `m.app.StartupNote = ""` | [W] |
| 2 | tui.go:1006 | `m.app.SetAllowDestructive(false)` | [M] |
| 3 | tui.go:1027 | `m.app.RevokeAuto()` | [M] |
| 4 | tui.go:1183 | `m.app.PendingImages = reconcileImageChips(...)` | [W] |
| 5 | tui.go:1391 | `m.app.PendingImages = reconcileImageChips(...)` | [W] |
| 6 | tui.go:1481 | `m.app.StartSideQuestion(...)` | [M] |
| 7 | tui_agent_msgs.go:184 | `m.app.SetAutoApprove(true)` | [M] |
| 8 | tui_agent_msgs.go:185 | `m.app.SaveRepoState(...)` | [M] |
| 9 | tui_agent_msgs.go:194 | `m.app.SetAllowDestructive(true)` | [M] |
| 10 | tui_agent_msgs.go:680 | `m.app.CtxLimit = msg.Limit` | [W] |
| 11 | tui_agent_msgs.go:681 | `m.app.CtxPressureWarned = false` | [W] |
| 12 | tui_agent_msgs.go:691 | `m.app.ModelList = msg.Models` | [W] |
| 13 | tui_agent_msgs.go:725 | `m.app.PendingImages = append(...)` | [W] |
| 14 | tui_agent_msgs.go:818 | `m.app.PendingImages = nil` | [W] |
| 15 | tui_agent_msgs.go:823 | `m.app.Workflow = nil` | [W] |
| 16 | tui_agent_msgs.go:826 | `m.app.NewConversation(...)` | [M] |
| 17 | tui_agent_msgs.go:883 | `m.app.Conv = append(...)` | [W] |
| 18 | tui_agent_msgs.go:889 | `m.app.SaveSession()` | [M] |
| 19 | tui_agent_msgs.go:903 | `m.app.Tools = msg.Tools` | [W] |
| 20 | info_panel.go:36 | `m.app.SetInfoPanelOpen(...)` | [M] |
| 21 | resume_picker.go:100 | `agent.ResumeSessionMsg(app, &s)` | [H] |

Site 21 was **absent from the first draft** and is invisible to any
`m.app.<Field> =` scan: the TUI hands `*App` to an agent free function that
mutates `Conv`/`Client.ChatID`/`Session`/`Workflow` inside `internal/agent`. It
is the same class as `RunTurn`/`RunFinalReview` (turn-driving, deferred) but is
*not* turn-driving — it is session resume — so it is fixed here (§2).

## §2 Design — two narrow interfaces, and the concurrency model (corrected)

**Decision D13 — mutation goes through two consumer-shaped interfaces, both
implemented by `*App`, bound at one construction site.** `tuiModel.app` stays
`*agent.App` for reads and turn-driving (deferred). Two new fields carry the
mutation surface, split by intent:

- **`Control`** — genuine session/consent/persistence commands a user triggers:
  `SetAutoApprove`, `SetAllowDestructive`, `RevokeAuto`, `NewConversation`,
  `SaveSession`, `SetWorkflow(nil)`, `StartSideQuestion` (returns
  `context.CancelFunc` — command dispatch, documented), `AppendSystemMessage`,
  `SaveRepoState`, `SetInfoPanelOpen`, `ConsumeStartupNote`.
- **`StateApply`** — round-trip runtime results produced by background/agent
  work, not user commands: `SetCtxLimit`, `SetModelList`, `SetTools`,
  `ReplacePendingImages`, `AddPendingImage`, `ClearPendingImages`.

`tuiModel` holds `control Control` and `apply StateApply`, both set to `app` in
`NewTUIModel` (the **single** construction/binding site — any other file binding
them is a guard failure). The split addresses the review's finding that
`SetCtxLimit`/`SetModelList`/`SetTools` are not frontend controls and would be a
nonsensical wire contract.

**What this is NOT:** not compiler-enforced (App's fields stay exported; `m.app`
stays present — unexporting fields is blocked by `cmd/wakil` writes, and removing
`m.app` is the turn-driving chunk). The boundary is a **tested convention**,
enforced by the §5 guard. Not wire-ready: `SaveRepoState(func(*RepoState))`,
`StartSideQuestion(...) context.CancelFunc`, `SetPolicy(*policy.Policy)`,
`SetWorkflow(*workflow.WorkflowState)`, `SetTools([]proxy.Tool)` all carry Go
callbacks or internal types that cannot cross a wire (violating D6's data-only
doctrine). `Control`/`StateApply` are throwaway P0 seams; the P2 wire surface is
still `SessionService` + data-only requests. No wire-ready claim is made.

**Concurrency model (corrected per review):** the fields these setters touch are
NOT uniformly synchronized today. The honest classification:

| Field | Current sync | Action this chunk |
|---|---|---|
| `consent`, `policy` | `atomic.Value` (proper) | untouched — already safe |
| `Conv` | `convMu` exists; site 17 **bypasses it** (bug) | `AppendSystemMessage` **takes `convMu.Lock()`** — completes the existing locked read side (`ConvSnapshot`/`NewConversation`). This is a real fix, not theatre. |
| `CtxLimit`, `CtxPressureWarned`, `ModelList`, `Tools`, `PendingImages`, `Workflow`, `StartupNote` | plain fields; written on TUI goroutine, read on agent goroutine — **unsynchronized cross-goroutine access (a data race by definition)** | setters group writes semantically; they do **not** add locks, because locking only the write side while the read side reads the raw field would leave the race intact. The race is **pre-existing and out of scope** here; removing it requires the turn-driving chunk (agent owns its own state) or paired getters. Documented, not called "discipline". |

Specific method notes:

- `AppendSystemMessage(m proxy.Message)` — takes `convMu.Lock()`, appends to
  `Conv`, returns. Matches `NewConversation`. **Fixes site 17's lock bypass.**
- `ConsumeStartupNote() string` — returns and clears `StartupNote`. The value is
  written once at startup (main.go) before the TUI runs and consumed once by
  `tuiModel.Init()` before any turn starts; it is single-goroutine by lifecycle,
  NOT atomic. The method doc states the lifecycle exclusion; it does not claim
  atomicity.
- `SetCtxLimit(lim ContextLimit)` — sets `CtxLimit` and resets
  `CtxPressureWarned`. This groups the two writes semantically; it does **not**
  make them atomic w.r.t. concurrent readers (same pre-existing race). Stated in
  the doc.
- `ResumeSessionMsg` — change its `app.Conv = s.Conv` to go through
  `convMu.Lock()` (or delegate to a new `App.applyResume` method that locks).
  The TUI call site (site 21) is documented in the guard as a **pass-`*App`-to-
  agent-function** case rather than converted to `Control`, because the function
  also returns the `NewConvMsg` the TUI needs — the cleaner home is the
  turn-driving chunk. The **Conv locking fix lands now** regardless.

Pending-image operations (review: read-modify-write over raw state can lose
updates; specify ownership):

- `ReplacePendingImages(imgs []proxy.ImagePart)` — replaces the slice wholesale
  (sites 4, 5). **Copies** the input slice (does not retain the caller's backing
  array).
- `AddPendingImage(img proxy.ImagePart)` — appends one (site 13).
- `ClearPendingImages()` — sets to nil (sites 14, and the handoff path).
- Reconciliation (`reconcileImageChips`) stays in the TUI (it is display-chip
  logic); its *result* is passed to `ReplacePendingImages`. Reads of
  `PendingImages` for rendering stay on `m.app` (deferred reads), which is safe
  only because those reads happen on the same TUI goroutine that just wrote — the
  cross-goroutine reader (the agent, in `Send`) is the pre-existing race noted
  above.

## §3 The round-trip, methodized not removed

Sites 10–12, 19 are the agent→TUI→App write-back round-trip
(`BackendCtxLimitMsg`, `ModelListUpdatedMsg`, `MCPReconnectedMsg`). They move
onto `StateApply` so they are no longer raw field writes, but the round-trip
itself is **not removed** here — that is the turn-driving chunk's business (when
turns run through the host, the agent updates its own state atomically and these
TUI-loop writes disappear). The separation onto `StateApply` (not `Control`) keeps
"user commands" and "runtime result application" from conflating on one surface.

## §4 InfoPanelOpen — TUI-owned, App-mirror only for persistence

`infoPanelModel.active` remains the source of truth (TUI-owned). `Control.SetInfoPanelOpen`
persists it to repo-state (existing `App.SetInfoPanelOpen` behavior). No second
App field is added; the existing mirror stays because repo-state persistence is
already keyed off it. Documented as the one place a "display-only" flag is
deliberately mirrored.

## §5 Exit criteria

1. `go build ./...`, `go vet ./...`, `go test -race ./...` green. (`/tmp` is
   `noexec`; race runs use workspace temp/cache dirs — chunk 5's established bar.)
   **Caveat recorded for the record:** `-race` detects only executed
   interleavings; green does not prove the §2 races are absent, only that current
   tests don't exercise them concurrently.
2. **Structural guard test** (new, in `internal/tui`, `go/parser`-based source
   scan), with a **narrowed claim**: in `internal/tui` non-test source,
   (a) zero assignment / compound-assignment / inc-dec statements whose
   left-hand side is syntactically rooted at `m.app`; (b) zero method calls
   through `m.app` whose method name is in the reflected method set of
   `Control` ∪ `StateApply` (obtained via `reflect` on the interface types, so
   the denylist cannot drift independently of the interfaces). The test is
   **explicitly heuristic**: it does not and cannot catch aliasing (`a := m.app;
   a.X = ...`), index/map assignment through `m.app`, method values
   (`f := m.app.SetWorkflow; f(nil)`), or mutation inside agent functions the
   TUI calls with `*App`. Those are documented gaps; the real structural
   enforcement is removing `*agent.App` from `tuiModel` (turn-driving chunk).
3. **Second guard** (same test or sibling): the **enumerated set** of
   pass-`*App`-to-agent-function call sites in `internal/tui` is exactly
   `{RunTurn, RunFinalReview, ResumeSessionMsg}` and nothing more — so a new
   hidden-mutation helper the TUI calls is caught by name. Turn-driving ones
   (`RunTurn`, `RunFinalReview`) are documented as deferred exceptions;
   `ResumeSessionMsg` is allowed only after its Conv-locking fix (§2).
4. **Parity + routing:** the existing TUI suite passes, *and* new fake-`Control`/
   fake-`StateApply` tests prove that representative interactions (`/auto` toggle,
   handoff rotation, info-panel toggle, pending-image append) route through the
   interfaces rather than `m.app` — so the tests cannot pass merely because
   `control` and `app` point at the same object. Existing tests build `tuiModel`
   via keyed literals or `NewTUIModel`; the new fields are additive and
   non-breaking (verified: all literals are keyed).
5. **Satisfaction asserts:** `var _ Control = (*App)(nil)` and
   `var _ StateApply = (*App)(nil)` in `internal/agent`.
6. **No `cmd/wakil` changes** this chunk (headless is deferred). Only
   `internal/agent` and `internal/tui` are touched.

## §6 Test plan

- **New setter unit tests** in `internal/agent`: `AppendSystemMessage` holds
  `convMu` (assert no race with a `ConvSnapshot` reader under `-race`);
  `ResumeSessionMsg` takes the lock (same); `SetCtxLimit` resets
  `CtxPressureWarned`; `ConsumeStartupNote` returns-then-clears;
  `ReplacePendingImages` copies its input slice (mutating the caller's slice
  after the call does not change App state); `AddPendingImage`/`ClearPendingImages`
  round-trip.
- **Structural guard tests** (criteria 2, 3).
- **Fake-Control/StateApply routing tests** (criterion 4).
- **Parity:** existing `internal/tui` + `internal/agent` suites unchanged.

## §7 Package home / import direction

No new package. `internal/agent` gains the two interfaces and the new methods;
`internal/tui` gains the two fields, the guard tests, and the routing tests.
Import direction stays TUI → agent (already the case). The D12 structural gate
(`internal/core` imports nothing forbidden) is untouched — this chunk does not
touch `internal/core`.

## §8 Depends-on (strategic)

This chunk's value is contingent on the turn-driving chunk (deliverable 7)
landing. Until the TUI stops holding `*agent.App` and routes turns through the
host, `Control`/`StateApply` are a partial overlap of `*App`'s method set with a
tested convention behind them — not a real ownership boundary. If deliverable 7
slips, revisit whether this seam should have been skipped in favor of doing the
re-route directly.

## §Revision (fold-ins from Mashura review op-8)

Three panels (gpt-5.6-sol, claude-fable-5, glm-5.2) converged. Folded in:
- **Reframed as interim seam, not deliverable-5 completion**; D12 + exit gate
  explicitly remain red; "behind Service methods" deviation from the deliverable
  wording recorded.
- **Dropped "compiler-enforced" and "wire-ready" claims** — it is a tested
  convention over a throwaway P0 seam.
- **Conv locking fixed**: `AppendSystemMessage` and `ResumeSessionMsg` take
  `convMu`; the "no locks" blanket was wrong for `Conv`.
- **`ResumeSessionMsg` added to the inventory** (site 21) and its lock bypass
  fixed; guard extended with an enumerated pass-`*App`-to-agent-function set.
- **Round-trip split onto `StateApply`**, off `Control` (user commands vs.
  runtime results).
- **Concurrency model made honest**: `atomic.Value` fields (safe) vs. plain
  fields (pre-existing race, out of scope) vs. `Conv` (fixed now); "goroutine-
  discipline" wording removed; `ConsumeStartupNote` atomicity claim dropped in
  favor of a stated lifecycle exclusion.
- **Guard narrowed** to the exact syntactic property it detects; aliasing/
  index/map/method-value gaps documented; mutator denylist derived from the
  interfaces via `reflect`.
- **Pending-image operations specified** (replace/add/clear, slice-copy
  semantics).
- **Narrowed interfaces to the 21 actual sites**; dropped `SetAllowReads`/
  `SetConsent`/`SetPolicy` (not TUI non-test sites).
- **Fake-Control routing tests** added; parity criterion no longer "unchanged
  tests pass" alone.
- **Construction exception** named (`NewTUIModel` is the sole binding site).
- **`-race` caveat** recorded (green ≠ race-free without exercising interleavings).
- **Depends-on note** added.
