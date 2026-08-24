# B2 & B5 — Formalized Analysis and Fix Plan

## B2: App Field Data Races from RPC Handlers

### Problem

In daemon mode, RPC handlers run in their own goroutines (gRPC server worker pool).
The turn goroutine runs concurrently. Both access shared App fields without
synchronization. The embedded path avoids this by construction: a fresh App is
created per conversation, and the TUI event loop serializes mutations.

### Unsynchronized Fields Inventory

| Field | Type | RPC Writer (ssh) | Turn-Goroutine Reader | Lock? |
|-------|------|-------------------|----------------------|-------|
| `SelectedModel` | string | SetModel:166 | turn_phases.go:39 (direct) | none |
| `SelectedBackend` | string | SetBackend:201-204 | turn_phases.go:46,70-93 (direct, ×8) | none |
| `RawTools` | bool | SetRawTools:396 | turn_phases.go:249 (direct) | none |
| `CounselMode` | string | SetCounselMode:431-447 | turn_phases.go:56, mashura.go:1033 | none |
| `MaxCounsel` | int | SetCounselMode:441,447 | mashura.go:1048,1075 | none |
| `SubagentModelOverride` | string | SetSubagentModel | subagent.go:686 | none |
| `SubagentEndpointOverride` | string | SetSubagentEndpoint | subagent.go:651 | none |
| `EffectiveCtxMaxCharsOverride` | int | SetMaxCtx | compact.go:169 (via activeThresholds) | none |
| `Session` | *Session | LoadSession:634 | app.go:647,651,653,656,661,664 (SaveSession) | none |
| `Client.ChatID` | string | LoadSession:631 | app.go:694, SaveSession path | none |

### Fields That ARE Safe (atomic.Value or mutex)

| Field | Protection |
|-------|-----------|
| `AutoApprove` / `AllowDestructive` / `AllowReads` | `consent atomic.Value` |
| `Conv` | `convMu sync.RWMutex` (SetConv uses lock) |
| `Workflow` | `convMu` (SetWorkflow uses lock) |
| `saveFailedWarned` | `atomic.Bool` |
| `subagentGlobalSem` | `subagentSemMu` |

### Proposed Fix: `stateMu sync.RWMutex` on App

Add a new `stateMu sync.RWMutex` field to App. RPC handlers acquire the write
lock when mutating RPC-driven fields. The turn goroutine acquires the read lock
at the points where it snapshots these fields for the turn (prepareTurn,
streamTurn entry, dispatchSubagent entry).

**Writers (RPC handlers) — take write lock:**
- `SetModel` / `ApplyModelOverride` → protect `SelectedModel`
- `SetBackend` → protect `SelectedBackend`
- `SetRawTools` → protect `RawTools`
- `SetCounselMode` → protect `CounselMode`, `MaxCounsel`
- `SetSubagentModel` → protect `SubagentModelOverride`
- `SetSubagentEndpoint` → protect `SubagentEndpointOverride`
- `SetMaxCtx` → protect `EffectiveCtxMaxCharsOverride`
- `LoadSession` → protect `Client.ChatID`, `Session` (see B5)

**Readers (turn goroutine) — take read lock:**
- `prepareTurn` (turn_phases.go:39-56) → snapshot `SelectedModel`,
  `SelectedBackend`, `CounselMode` into locals before the turn starts
- `streamTurn` (turn_phases.go:249) → read `RawTools` under lock
- `checkEgressConsent` (turn_phases.go:70-93) → read `SelectedBackend` under
  lock (or accept the snapshot from prepareTurn)
- `activeThresholds` (compact.go:124) → read
  `EffectiveCtxMaxCharsOverride` under lock
- `resolveSubagentEndpointName` / `resolveSubagentEndpointView`
  (subagent.go:651,686) → read overrides under lock
- `SaveSession` (app.go:646-667) → read `Session` under lock
  (or accept the B5 transition lock covers this)

**Design decision: snapshot vs lock-per-read**
- Preferred: snapshot at turn entry. `prepareTurn` takes `stateMu.RLock`,
  copies all needed fields into local variables, releases. The rest of the
  turn uses locals — no repeated lock acquisition. This matches the existing
  code comment philosophy ("never snapshot into a closure" — but that's about
  not carrying stale values across goroutine boundaries, which is exactly what
  the snapshot-at-entry prevents).
- Alternative: RLock per access site. More correct for mid-turn changes (e.g.
  `/rawtools` toggled mid-turn would take effect on the next tool result), but
  more lock contention and more code surface.

**What about `Cfg.MaxParallelSubagents`?**
Read in `subagent_parallel.go` (not yet read). Likely read once per parallel
block — snapshot at dispatch entry.

**What about the embedded path?**
The embedded path creates a fresh App per conversation. Commands like `/model`,
`/backend`, `/rawtools` run on the TUI event loop (single-threaded), so the same
App is never accessed from two goroutines simultaneously — except for
side-question goroutines, which only read `Conv` (protected by `convMu`). The
`stateMu` would be a no-op in the embedded path (no contention), so it's safe
to add without behavior change.

---

## B5: Non-Atomic LoadSession Transition

### Problem

The daemon's `LoadSession` handler (session_state_handler.go:588-656) mutates
the **live shared App** in multiple unsynchronized steps:

```
1. resetSessionBinding()         # 611 — clears hostTurn sessionID
2. resetRestoreDone()             # 618 — clears restore guard
3. app.RevokeAuto()               # 625 — atomic CAS, safe alone
4. app.SetAllowReads(false)        # 626 — atomic CAS, safe alone
5. app.Client.ChatID = s.ChatID   # 631 — PLAIN FIELD WRITE
6. app.SetConv(s.Conv)            # 633 — convMu locked, safe w.r.t. conv
7. app.Session = s                # 634 — PLAIN FIELD WRITE
8. app.SetWorkflow(s.Workflow)    # 635 — convMu locked
9. app.RestoreRepoStateResume()   # 636 — restores settings
```

A concurrent turn or auto-save could interleave between any of these steps.
Two concrete corruption scenarios:

**Scenario 1: SaveSession races with step 6-7**
Between `SetConv` (633, new conv) and `Session=s` (634, new session), a
concurrent `SaveSession` would write the new conv into the OLD session record,
corrupting the old session's saved transcript.

**Scenario 2: turn re-claims old session between step 1 and step 5**
After `resetSessionBinding` (611) clears the binding but before `Client.ChatID`
is updated (631), a concurrent turn could re-claim the old session binding and
start a turn against the old chat ID with the new conv partially installed.

### Embedded Path Comparison

The embedded path (conversation_manager.go) avoids this entirely:
- `ResumeConversation` (line 169): loads session from disk, builds a **fresh
  App** via `newConversation` (line 178), restores state into the fresh App
  (lines 184-187). Since the App is fresh and not yet published, no concurrent
  turn can interleave.
- `NewConversation` (line 138): finalizes the old App, builds a fresh App.
- No transition lock exists in the embedded path — **structural isolation**
  (fresh App per conversation) is the atomicity mechanism.

### Proposed Fix: Transition Lock

Add a `transitionMu sync.Mutex` to the session state handler (or to the daemon
host/facade). The `LoadSession` handler acquires it for the entire transition
sequence (steps 1-9). The turn-start path (`claimSession` / `hostTurn.Start`)
must also acquire it (or check a "transitioning" flag) to block turn starts
during a transition.

**Alternative: pause-turn mechanism**
Instead of a mutex, set a `transitioning atomic.Bool` at the start of
`LoadSession`. The turn-start path checks this flag and returns
`ErrSessionTransitioning` if set. Clear the flag after step 9. This is lighter
weight but requires all turn-entry points to check the flag.

**Combined with B2 fix:**
If we add `stateMu` for B2, the `LoadSession` handler should take `stateMu.Lock`
for the entire sequence. This makes the transition atomic w.r.t. all the
RPC-driven fields. The `resetSessionBinding` + `resetRestoreDone` steps don't
touch `stateMu` fields, so they'd still need the separate transition guard
(pause-turn or the existing `turnActive` check in `resetSessionBinding`).

**Recommended approach:**
1. `stateMu.Lock` covers steps 5-9 (the App field mutations).
2. The existing `resetSessionBinding` already rejects if `turnActive` — this
   guards steps 1-4.
3. Between step 4 and step 5, no lock gap exists if we hold `stateMu.Lock`
   from step 1 through step 9. But `resetSessionBinding` acquires `hostTurn.mu`,
   not `stateMu` — so we need to hold `stateMu.Lock` for the entire handler,
   and ensure the turn path also acquires `stateMu.RLock` before starting.

This means: **turn-start acquires `stateMu.RLock`** (to block during
transition), **LoadSession acquires `stateMu.Lock`** (to serialize the full
transition). This is clean: one lock, one purpose, and it composes with the
B2 fix (same lock, RWMutex).

---

## Summary Plan

### Single Lock: `stateMu sync.RWMutex` on App

Solves both B2 and B5 with one mechanism:

| Operation | Lock mode | Scope |
|-----------|----------|-------|
| `LoadSession` handler | Write | Full transition (steps 1-9) |
| `InitNewSession` handler | Write | Consent reset + ChatID + Session |
| `SetModel` / `SetBackend` | Write | Individual field |
| `SetRawTools` | Write | Individual field |
| `SetCounselMode` | Write | CounselMode + MaxCounsel |
| `SetSubagentModel` / `SetSubagentEndpoint` | Write | Individual field |
| `SetMaxCtx` | Write | Individual field |
| `prepareTurn` (turn start) | Read | Snapshot all needed fields |
| `streamTurn` (mid-turn reads) | Read | RawTools, EffectiveCtxMaxCharsOverride |
| `dispatchSubagent` | Read | Subagent overrides |
| `SaveSession` | Read | Session pointer |
| Turn-start (claimSession) | Read | Block during transition |

### Additional Unsynchronized Access Points Found

- `SetSessionLabel` (ssh:526) — writes `app.Session.Label` without lock
- `RestoreRepoState` (repostate.go:288-374) — writes SubagentEndpointOverride,
  SubagentModelOverride, Cfg.MaxParallelSubagents, RawTools, AutoApprove (atomic,
  safe), InfoPanelOpen, EffectiveCtxMaxCharsOverride, Cfg.MashuraPanels,
  Cfg.OracleModel, Cfg.OracleMaxTokens, Cfg.OracleTimeoutSeconds, CounselMode,
  MaxCounsel — all without stateMu. Called from RPC handler.
- `RestoreRepoStateResume` (repostate.go:265-282) — same fields via
  `restoreEndpointIndependent`. Called from RPC handler.
- `ApplyModelOverride` (commands.go:228-238) — writes `Client.ConfiguredModel`,
  `Client.Model`, `Cfg.Endpoint.Model`, `SelectedModel`, `defaultModel` (OpenAI
  kind path). Called from RPC handler (SetModel).
- `NewConversation` (app.go:686-707) — writes `Conv` (under convMu), `Client.ChatID`,
  `Session` (plain). Called from `InitNewSession` RPC handler. Same B5 pattern
  as `LoadSession`.
- `Cfg.MaxParallelSubagents` — read in `subagent_parallel.go` (not yet read).
  Written by RestoreRepoState/RestoreRepoStateResume without lock.
- `Cfg.MashuraPanels`, `Cfg.OracleModel`, `Cfg.OracleMaxTokens`,
  `Cfg.OracleTimeoutSeconds` — written by RestoreRepoState, read by mashura.go.
- `InfoPanelOpen` — written by RestoreRepoState, read by TUI status. (TUI read
  is on the TUI event loop, not a race in embedded path; in daemon path the
  TUI reads via RPC snapshot, so no direct App field access.)

### Implementation Steps

1. Add `stateMu sync.RWMutex` to App struct
2. Wrap all RPC handler writes in `stateMu.Lock`/`Unlock`
3. Wrap `LoadSession` handler in `stateMu.Lock`/`Unlock` for full sequence
4. Snapshot RPC-driven fields at `prepareTurn` under `stateMu.RLock`
5. Take `stateMu.RLock` at other read sites (or use prepareTurn snapshots)
6. Take `stateMu.RLock` at turn-start (`claimSession` or equivalent) to block
   during transitions
7. Verify with `go test -race` (existing race detector tests)
8. Mashūra gate review

### Risks

- **Deadlock**: if any code path takes `stateMu` and then calls something that
  also takes `stateMu` (non-reentrant mutex). Mitigated by: writers are simple
  field assignments, readers snapshot into locals. No nested locking.
- **ConvMu interaction**: `SetConv` and `SetWorkflow` already use `convMu`.
  `LoadSession` takes both `stateMu.Lock` (outer) and `convMu` (inner). Order
  must be consistent — `stateMu` always outer, `convMu` always inner. The
  turn path takes `stateMu.RLock` (outer) then `convMu.RLock` (inner) for
  SaveSession. No other code path takes `convMu` then `stateMu`, so no
  lock-order inversion.
- **Performance**: RWMutex with read-heavy access (every turn start) and
  infrequent writes (only RPC setting changes). Negligible contention.