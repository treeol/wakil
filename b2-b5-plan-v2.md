# B2 & B5 — Revised Full Fix Plan (v2)

## Scope

Fix the data races (B2) and non-atomic session transitions (B5) in the daemon
path properly, addressing all findings from the Mashūra 3-panel review.

## Architecture

### Two separate locks with distinct responsibilities

Per Mashūra's recommendation, use separate synchronization instead of one broad
mutex:

1. **`stateMu sync.RWMutex` on App** — protects RPC-driven scalar settings
   (model, backend, rawtools, counsel, subagent overrides, maxctx, ctxlimit,
   maxpar). Writers are RPC handlers + restore functions. Readers are the turn
   goroutine (via snapshot) and `GetSessionState` (via snapshot).

2. **`transitionMu sync.Mutex` on hostTurn** — serializes session transitions
   (LoadSession, InitNewSession) against turn starts. This is NOT on App; it
   lives on the hostTurn (or a dedicated coordinator). It avoids the ABBA
   deadlock that would arise from using `stateMu` for transition serialization
   while `hostTurn.mu` is involved in turn claiming.

3. **`saveMu sync.Mutex` on App** — serializes SaveSession writes so concurrent
   saves don't mutate the same Session object. Used as an exclusive lock around
   the copy-then-write sequence.

4. **Existing `convMu`** — protect Conv properly. Fix the pre-existing Compact
   bypass.

### Lock ordering (globally documented)

```
transitionMu (hostTurn)     — transition vs turn-start
stateMu (App)               — RPC settings vs turn snapshot
convMu (App)                 — Conv/Workflow
saveMu (App)                 — SaveSession serialization
hostTurn.mu (hostTurn)      — hostTurn internal state
```

No nested acquisition across these lock domains:
- `transitionMu` is acquired/released BEFORE `stateMu` is touched
- `stateMu` is acquired/released BEFORE `convMu` (when both needed)
- `saveMu` is acquired AFTER releasing `convMu` and `stateMu` (copy-then-write)
- `hostTurn.mu` is never held while calling App methods

### Exported App APIs (so `connect` package doesn't touch the mutex)

The `connect` package cannot access unexported fields. Instead of exposing the
mutex, export App methods that perform complete mutations and snapshots:

```go
// Snapshot type for turn goroutine and GetSessionState
type TurnSettings struct {
    Model             string
    Backend           string
    AuxModel          string
    CounselMode       string
    MaxCounsel        int
    RawTools          bool
    CtxMaxChars       int
    SubagentModel     string
    SubagentEndpoint  string
    MaxParallel       int
    // ... see full field list below
}

func (a *App) SnapshotTurnSettings() TurnSettings     // RLock stateMu
func (a *App) SnapshotSessionState() SessionStateSnapshot // RLock stateMu
func (a *App) SetBackendSelection(backend, model string)   // Lock stateMu
func (a *App) SetModelOverride(model string)               // Lock stateMu
func (a *App) SetRawToolsValue(v bool)                    // Lock stateMu
func (a *App) SetCounselModeValue(mode string, cap int)   // Lock stateMu
func (a *App) SetSubagentOverrides(ep, model string)      // Lock stateMu
func (a *App) SetMaxCtxOverride(v int)                    // Lock stateMu
func (a *App) SetMaxParallel(n int)                       // Lock stateMu
func (a *App) SetSessionLabelValue(label string)         // Lock stateMu + saveMu
func (a *App) InstallSession(s *Session)                 // Lock stateMu + convMu
func (a *App) NewConversationTransition(chatID string)    // Lock stateMu + convMu
```

---

## Complete Field Inventory

### Fields protected by `stateMu`

| Field | RPC Writers | Turn-Path Readers | Notes |
|-------|-------------|-------------------|-------|
| `SelectedModel` | SetModel, SetBackend | prepareTurn, EffectiveModel | |
| `SelectedBackend` | SetBackend | prepareTurn, checkEgressConsent | Turn also WRITES on decline |
| `RawTools` | SetRawTools | streamTurn:249 | |
| `CounselMode` | SetCounselMode | turn_phases:56, mashura:1033 | |
| `MaxCounsel` | SetCounselMode | mashura:1048,1075 | |
| `SubagentModelOverride` | SetSubagentModel | subagent.go:686 | |
| `SubagentEndpointOverride` | SetSubagentEndpoint | subagent.go:651 | |
| `EffectiveCtxMaxCharsOverride` | SetMaxCtx | compact.go:169 | |
| `CtxLimit` | RestoreRepoState | compact.go:128, GetSessionState | Multi-word struct |
| `Cfg.MaxParallelSubagents` | SetMaxParallel | subagent_parallel.go:109,275 | |
| `Session` | LoadSession, NewConversation | SaveSession, GetSessionState | Pointer |
| `Client.ChatID` | LoadSession, NewConversation | Stream, SaveSession | |
| `Client.Model` | prepareTurn (turn goroutine) | EffectiveModel, GetSessionState, Stream | Turn writes! |
| `Client.Backend` | prepareTurn, checkEgressConsent | Stream, calibrationKeyFor | Turn writes! |
| `Client.AuxModel` | prepareTurn | Stream | Turn writes! |
| `Client.ConfiguredModel` | ApplyModelOverride (OpenAI kind) | Stream | RPC writes |
| `defaultModel` | prepareTurn | EffectiveModel | Turn writes |
| `consentedBackends` | checkEgressConsent | checkEgressConsent | Turn-only, clear on transition |
| `preambleDay` | NewConversation, ensurePreamble | ensurePreamble | Clear on transition |
| `InfoPanelOpen` | RestoreRepoState | GetSessionState | |
| `Cfg.MashuraPanels` | RestoreRepoState | mashura.go | Map — copy under lock |
| `Cfg.OracleModel` | RestoreRepoState | mashura.go | |
| `Cfg.OracleMaxTokens` | RestoreRepoState | mashura.go | |
| `Cfg.OracleTimeoutSeconds` | RestoreRepoState | mashura.go | |
| `BackendList` | startup (one-shot) | GetSessionState, IsExternalBackend | Read-only after init |
| `Workflow` | LoadSession, SetWorkflow | SaveSession, GetSessionState | Also under convMu currently |

### Fields safe (no change needed)

| Field | Protection |
|-------|-----------|
| `AutoApprove/AllowDestructive/AllowReads` | `consent atomic.Value` |
| `Conv` | `convMu` (after Compact fix) |
| `saveFailedWarned` | `atomic.Bool` |
| `Client.LastUsedBackend` | `lastUsedBackendMu` |
| `Client.grounding/usage/calibration` | internal mutexes |

### Turn-side writes (the turn goroutine is NOT a pure reader)

These are writes the turn goroutine makes to App fields, which race with
concurrent RPC reads (GetSessionState):

1. `prepareTurn` writes: `Client.Model`, `Client.Backend`, `Client.AuxModel`,
   `defaultModel`, `exhausted`, `stopReason`, `turnBudgetStubbed`,
   `confinementTripped`, `confinementPathsHit`, `counselCalls`,
   `struggleSuggested`
2. `checkEgressConsent` writes: `SelectedBackend` (on decline),
   `Client.Backend` (on decline), `consentedBackends`
3. `ensurePreamble` reads/writes: `preambleDay`

The `Client.Model/Backend/AuxModel` writes are the hardest to fix — they're
shared fields on `proxy.Client` that `Stream` reads at call time. Two options:

**Option A (preferred): Snapshot into turnState, pass to Stream**
- `prepareTurn` returns a `turnState` struct with model/backend/auxModel
- `Stream` is called with these as parameters (or a turnState argument)
- `Client.Model/Backend/AuxModel` are no longer written by the turn goroutine
- `GetSessionState` reads `Client.Model` under `stateMu.RLock` — but it's only
  written at construction time (RPC SetModel writes `SelectedModel`, not
  `Client.Model`), so no race

**Option B: Accept turn-side Client writes under stateMu**
- `prepareTurn` takes `stateMu.Lock` (not RLock — it writes)
- `checkEgressConsent` takes `stateMu.Lock` (writes on decline)
- `GetSessionState` takes `stateMu.RLock`
- This blocks GetSessionState during the entire prepareTurn, which is fine
  (prepareTurn is fast, no I/O)
- The Client.Model/Backend writes happen under the lock, so GetSessionState
  can't see a torn state
- Simpler, less invasive, but the turn goroutine holds a write lock briefly

**Decision: Option B for Client fields.** Option A is architecturally cleaner
but requires changing `proxy.Client.Stream`'s signature, which ripples through
every caller. Option B achieves race-safety with minimal surface change. The
write lock is held only for field assignments (no I/O, no callbacks), so
contention is negligible.

---

## Per-Phase Implementation

### Phase 1: Fix Compact/Conv access (prerequisite)

**Problem:** `compact.go` reads/writes `a.Conv` directly without `convMu` in:
- `Compact` (218): reads `a.Conv` at lines 221,225,243,336,342
- `Compact` (343): writes `a.Conv = newConv`
- `enforceHardMax` (492-549): reads and writes `a.Conv`
- `fitConvToWindow`: reads and writes `a.Conv`

The daemon's `Compact` RPC handler can run concurrently with a turn.

**Fix:** Two sub-options:

1. **Block Compact during active turn:** The `Compact` RPC handler checks
   `turnActive` (via the hostTurn reference or a method on the handler) and
   returns an error if a turn is active. This is the simplest approach — Compact
   is a maintenance operation that shouldn't run mid-turn anyway.

2. **Make Compact take convMu:** Add `convMu.Lock` around all Conv reads/writes
   in compact.go. But `Compact` calls `a.Client.Stream` for summarization, and
   holding `convMu` across a network call is dangerous (long hold, potential
   deadlock if the stream callback touches Conv).

**Decision: Option 1 (block Compact during active turn).** The Compact RPC
handler should reject if a turn is active. Add a `h.isTurnActive()` check (or
equivalent) to the handler. This is clean and matches the embedded path (where
`/compact` runs on the TUI event loop, never concurrent with a turn).

Files: `session_state_handler.go` (Compact handler — add turnActive check),
`hostturn.go` (expose an `IsTurnActive()` method or use the existing
`resetSessionBinding` error path).

### Phase 2: Add `stateMu` and exported App APIs

**App struct changes** (`app.go`):
```go
// stateMu protects RPC-driven session settings from concurrent access by
// RPC handler goroutines and the turn goroutine. Lock ordering:
// stateMu is always acquired BEFORE convMu. Never hold stateMu across
// I/O, network calls, or callbacks.
stateMu sync.RWMutex

// saveMu serializes SaveSession writes so concurrent saves don't mutate
// the same Session. Acquired AFTER releasing stateMu and convMu.
saveMu sync.Mutex
```

**Exported methods** (`app.go` or a new `app_state.go`):

```go
// TurnSettings is a snapshot of turn-relevant settings, taken under stateMu.RLock.
type TurnSettings struct {
    SelectedModel        string
    SelectedBackend      string
    DefaultModel         string
    AuxModel             string
    CounselMode          string
    MaxCounsel           int
    RawTools             bool
    CtxMaxCharsOverride  int
    SubagentModel        string
    SubagentEndpoint     string
    MaxParallel          int
    ClientModel          string  // Client.Model at snapshot time
    ClientBackend        string  // Client.Backend at snapshot time
    ClientChatID         string  // Client.ChatID at snapshot time
    CtxLimit             ContextLimit
}

func (a *App) SnapshotTurnSettings() TurnSettings {
    a.stateMu.RLock()
    defer a.stateMu.RUnlock()
    return TurnSettings{
        SelectedModel:       a.SelectedModel,
        SelectedBackend:     a.SelectedBackend,
        // ... all fields
    }
}
```

**RPC handler changes** (`session_state_handler.go`):
- `SetModel` → calls `app.SetModelOverride(model)` (takes stateMu.Lock)
- `SetBackend` → calls `app.SetBackendSelection(backend, model)`
- `SetRawTools` → calls `app.SetRawToolsValue(v)`
- `SetCounselMode` → calls `app.SetCounselModeValue(mode, cap)`
- `SetSubagentModel` → calls `app.SetSubagentOverrides(ep, model)`
- `SetSubagentEndpoint` → calls `app.SetSubagentOverrides(ep, model)`
- `SetMaxCtx` → calls `app.SetMaxCtxOverride(v)`
- `SetMaxParallel` → calls `app.SetMaxParallel(n)`
- `SetSessionLabel` → calls `app.SetSessionLabelValue(label)`
- `GetSessionState` → calls `app.SnapshotSessionState()` (one coherent read)
- `RestoreRepoState` → calls a new `app.RestoreRepoStateLocked()` (takes
  stateMu.Lock internally)
- `RestoreRepoStateResume` → calls `app.RestoreRepoStateResumeLocked()`
- `Compact` → reject if turn active (Phase 1)

### Phase 3: Turn goroutine snapshot

**`prepareTurn` changes** (`turn_phases.go`):
- Take `stateMu.Lock` (write — because prepareTurn writes Client.Model/Backend)
- Apply model/backend/AuxModel from Selected* to Client.*
- Reset per-turn counters (counselCalls, struggleSuggested, exhausted, etc.)
- Release stateMu.Lock
- The lock is held only for field assignments — no I/O, no callbacks

**`checkEgressConsent` changes** (`turn_phases.go`):
- Take `stateMu.Lock` for the decline path (writes SelectedBackend, Client.Backend)
- Read SelectedBackend under stateMu.RLock for the check
- Or: read SelectedBackend from the prepareTurn snapshot, only take write lock
  on decline

**`streamTurn` changes** (`turn_phases.go`):
- Read `RawTools` via `stateMu.RLock` at line 249, or accept it from the
  prepareTurn snapshot
- Decision: use the prepareTurn snapshot for RawTools (turn-stable — see
  semantics below)

**`activeThresholds` changes** (`compact.go`):
- Read `CtxLimit`, `EffectiveCtxMaxCharsOverride` via `stateMu.RLock`
- Already called from the turn goroutine, so no deadlock risk

**`dispatchSubagent` / `resolveSubagentEndpoint*` changes** (`subagent.go`):
- Read `SubagentModelOverride`, `SubagentEndpointOverride`,
  `SelectedBackend`, `Cfg.MaxParallelSubagents` via `stateMu.RLock`
- Or accept from the prepareTurn snapshot
- Decision: snapshot at dispatchSubagent entry (turn-stable — see semantics)

**`SaveSession` changes** (`app.go`):
- Take `stateMu.RLock` → copy Session pointer → `stateMu.RUnlock`
- Take `convMu.RLock` → copy Conv slice → `convMu.RUnlock`
- Take `saveMu.Lock` → write the detached copy → `saveMu.Unlock`
- No lock held across `WriteSession` (disk I/O) except `saveMu`

### Phase 4: Transition serialization

**New `transitionMu` on hostTurn** (or on the SessionStateHandler):

The transition guard serializes LoadSession/InitNewSession against turn starts.

```go
// transitionMu serializes session transitions (LoadSession, InitNewSession)
// against turn starts. Acquired BEFORE stateMu; released before stateMu.
// This is separate from hostTurn.mu to avoid ABBA.
transitionMu sync.Mutex
```

**LoadSession handler sequence (revised):**
1. Acquire `transitionMu.Lock`
2. Call `resetSessionBinding()` — rejects if turnActive (under hostTurn.mu)
3. Acquire `stateMu.Lock`
4. Reset consent (RevokeAuto, SetAllowReads) — atomic, safe
5. Call `app.InstallSession(s)` — sets ChatID, Conv, Session, Workflow under
   stateMu + convMu
6. Release `stateMu.Lock`
7. Release `transitionMu.Unlock`
8. (Client separately calls `RestoreRepoStateResume` — see semantics below)

**InitNewSession handler sequence (revised):**
1. Acquire `transitionMu.Lock`
2. Call `resetSessionBinding()`
3. Acquire `stateMu.Lock`
4. Call `app.NewConversationTransition(chatID)` — resets Conv (convMu),
   sets ChatID, Session, consent
5. Release `stateMu.Lock`
6. Release `transitionMu.Unlock`

**Turn-start sequence (revised, `hostturn.go:run`):**
1. Acquire `transitionMu.Lock`
2. `claimSession` — bind sessionID (under hostTurn.mu)
3. Check turnActive, set turnActive (under hostTurn.mu)
4. Release `transitionMu.Unlock`
5. ... run the turn ...
6. Defer: clear turnActive (under hostTurn.mu)

This ensures:
- Load/Init and turn-start cannot interleave
- If turn-start wins the `transitionMu` race, LoadSession sees `turnActive=true`
  and rejects
- If Load wins, turn-start blocks until Load releases `transitionMu`
- `transitionMu` is never held while `stateMu` is held (no nested acquisition)
- `transitionMu` is never held while `hostTurn.mu` is held for more than the
  claim/check (no long hold)
- No App methods are called while `hostTurn.mu` is held (existing behavior
  preserved)

**Multi-RPC transition gap:** `RestoreRepoStateResume` is a separate RPC after
`LoadSession`. The transition is NOT atomic across both RPCs. Decision:
LoadSession installs Session/Conv/Workflow atomically (the core transition).
Repo-state settings (auto, rawtools, counsel, etc.) may apply afterward. A
turn starting between LoadSession and RestoreRepoStateResume sees the loaded
session with pre-restore settings — this is acceptable (the settings were
different in that session anyway; they'll be updated when the next RPC fires).
Document this as the chosen semantic.

### Phase 5: Snapshot semantics (per-field decisions)

| Field | Semantics | Rationale |
|-------|-----------|----------|
| `SelectedModel` | Turn-stable | Model change shouldn't apply mid-turn |
| `SelectedBackend` | Turn-stable | Backend change shouldn't apply mid-turn |
| `Client.Model/Backend/AuxModel` | Written at prepareTurn under lock | Written, not snapshot — the turn needs these on Client.Stream |
| `RawTools` | Live (re-read per tool result) | "Applies live" per code comments; /rawtools should take effect immediately |
| `CounselMode` | Turn-stable | Per-turn cap reset in prepareTurn |
| `MaxCounsel` | Turn-stable | Same as CounselMode |
| `SubagentModelOverride` | Live (read at dispatch time) | Dispatch may happen later in turn |
| `SubagentEndpointOverride` | Live (read at dispatch time) | Same |
| `EffectiveCtxMaxCharsOverride` | Live (read at compaction) | "Applied live" per code comments |
| `CtxLimit` | Turn-stable | Probed per backend/model; shouldn't change mid-turn |
| `MaxParallelSubagents` | Live (read at dispatch) | May be changed between turns |
| `Session` | Transition-stable | Set by LoadSession/InitNewSession, read by SaveSession |
| `Client.ChatID` | Transition-stable | Set by LoadSession/InitNewSession |
| `consentedBackends` | Turn-scoped | Cleared on transition; only turn goroutine reads/writes |
| `preambleDay` | Turn-scoped | Only turn goroutine reads/writes |
| `Workflow` | Transition-stable | Set by LoadSession, read by SaveSession |

"Turn-stable" = snapshotted at prepareTurn, used throughout the turn.
"Live" = re-read under stateMu.RLock at each use site.
"Transition-stable" = set during transition under stateMu.Lock, read under
stateMu.RLock.
"Turn-scoped" = only accessed by the turn goroutine, no cross-goroutine race.

### Phase 6: Race tests

New test file: `internal/agent/daemon_race_test.go`

Test scenarios:
1. Concurrent `SetModel` + `prepareTurn` + `GetSessionState` under `-race`
2. Concurrent `SetBackend` + `checkEgressConsent` + `GetSessionState`
3. `LoadSession` + concurrent turn start (verify no interleaving)
4. `InitNewSession` + concurrent turn start
5. Concurrent `SaveSession` + `SetSessionLabel` + `LoadSession`
6. `RestoreRepoState` + `activeThresholds` (CtxLimit race)
7. `Compact` RPC rejected during active turn
8. Concurrent `SetCounselMode` + `mashura.go` counsel reads

Tests use a real App with a mock proxy.Client, spawn goroutines that call RPC
handler methods concurrently with turn-path methods, and verify no `-race`
failures and no torn reads.

---

## Implementation Order

1. Phase 1: Fix Compact/Conv — block Compact RPC during active turn
2. Phase 2: Add `stateMu`, `saveMu`, exported App methods
3. Phase 3: Wire RPC handlers to use exported methods
4. Phase 4: Wire turn goroutine to snapshot/lock
5. Phase 5: Add `transitionMu`, revise LoadSession/InitNewSession/turn-start
6. Phase 6: Redesign SaveSession (detached snapshot + saveMu)
7. Phase 7: Race tests
8. Build + vet + test -race
9. Mashūra review of implementation
10. Fix any issues found
11. Commit

## Files Changed (estimated)

| File | Changes |
|------|---------|
| `internal/agent/app.go` | Add stateMu, saveMu, TurnSettings, exported methods |
| `internal/agent/turn_phases.go` | prepareTurn takes stateMu.Lock, checkEgressConsent lock, streamTurn reads from snapshot |
| `internal/agent/compact.go` | activeThresholds reads under stateMu.RLock |
| `internal/agent/subagent.go` | resolveSubagent* reads under stateMu.RLock |
| `internal/agent/subagent_parallel.go` | MaxParallelSubagents read under stateMu.RLock |
| `internal/agent/repostate.go` | RestoreRepoState/Resume take stateMu.Lock |
| `internal/agent/commands.go` | ApplyModelOverride takes stateMu.Lock |
| `internal/server/connect/session_state_handler.go` | All handlers use exported App methods, Compact checks turnActive, transitionMu wiring |
| `internal/wiring/hostturn.go` | Turn-start takes transitionMu, expose IsTurnActive |
| `internal/agent/app.go` (SaveSession) | Detached snapshot + saveMu |
| `internal/agent/daemon_race_test.go` (new) | Race tests |

## Risks and Mitigations

- **Deadlock (stateMu + convMu):** Enforce stateMu-before-convMu everywhere.
  Audit all convMu holders. Add a lint test or code comment documenting the
  order.
- **Deadlock (transitionMu + stateMu):** transitionMu is acquired/released
  BEFORE stateMu. Never nested.
- **Deadlock (transitionMu + hostTurn.mu):** transitionMu is acquired first,
  then hostTurn.mu for the claim/check, then released. hostTurn.mu is never
  held while waiting for transitionMu.
- **Recursive RLock deadlock:** The turn goroutine does NOT hold stateMu.RLock
  for the whole turn — only for brief snapshots and the prepareTurn write.
  SaveSession takes its own stateMu.RLock independently.
- **Performance:** stateMu is an RWMutex with brief holds (no I/O). contention
  is negligible. transitionMu is a Mutex but only contended during transitions
  (rare).
- **Embedded path compatibility:** The embedded path creates a fresh App per
  conversation. The new locks are no-ops there (no contention). The exported
  methods work the same. No behavior change.
