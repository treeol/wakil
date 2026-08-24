# B2 & B5 — Final Fix Plan (v3)

Addresses all findings from Mashūra v1 and v2 reviews (3 panels each).

## Architecture

### Locks

| Lock | Location | Purpose | Type |
|------|----------|---------|------|
| `stateMu` | App | RPC-driven session settings (model, backend, rawtools, counsel, subagent overrides, maxctx, ctxlimit, maxpar, session, client routing fields) | `sync.RWMutex` |
| `convMu` | App (existing) | Conv slice + Workflow | `sync.RWMutex` |
| `saveMu` | App | Serializes SaveSession disk writes | `sync.Mutex` |
| `transitionMu` | shared coordinator | Serializes session transitions (Load/Init) vs turn starts vs idle maintenance (Compact) | `sync.Mutex` |
| `hostTurn.mu` | hostTurn (existing) | hostTurn internal state (sessionID, turnActive, sessionEmit) | `sync.Mutex` |

### Lock ordering (corrected — nesting is explicit)

```
transitionMu → stateMu → convMu     (transitions, snapshots)
transitionMu → hostTurn.mu          (transition claim/check)
hostTurn.mu alone                   (turn internal state)
saveMu alone                        (after snapshot released)
```

Rules:
1. **transitionMu nests stateMu nests convMu** — transitions acquire all three.
2. **transitionMu nests hostTurn.mu** — for the claim/check phase only.
3. **hostTurn.mu is never held while acquiring App locks** — the existing
   `resetSessionBinding` writes to `app.EventSink/OnTokRate` under hostTurn.mu
   must be moved outside hostTurn.mu (Phase 1 fix).
4. **saveMu is always standalone** — acquired after all other locks released.
5. **No lock held across I/O, network, or callbacks** (Confirm, Stream,
   WriteSession, ResolveContextLimit).

### Exported methods own ALL locks (no caller-side App locking)

Cross-package handlers NEVER call `app.stateMu.Lock()`. Instead:

```go
// App methods own the lock — callers never lock App internals
func (a *App) SetModelOverride(model string)        // stateMu.Lock inside
func (a *App) SetBackendSelection(backend, model string)
func (a *App) SetRawToolsValue(v bool)
func (a *App) SetCounselModeValue(mode string, cap int)
func (a *App) SetSubagentOverrides(ep, model string)
func (a *App) SetMaxCtxOverride(v int)
func (a *App) SetMaxParallel(n int)
func (a *App) SetSessionLabelValue(label string)     // stateMu.RLock + saveMu
func (a *App) InstallSession(s *Session)              // stateMu.Lock + convMu.Lock
func (a *App) NewConversationTransition(chatID string) // stateMu.Lock + convMu.Lock
func (a *App) SnapshotTurnSettings() TurnSettings     // stateMu.RLock
func (a *App) SnapshotSessionState() SessionStateSnapshot // stateMu.RLock
func (a *App) SaveSessionDetached()                   // stateMu.RLock + convMu.RLock + saveMu
func (a *App) RestoreRepoStateLocked(ctx) (result, error) // stateMu.Lock (I/O outside lock)
func (a *App) RestoreRepoStateResumeLocked() (result)     // stateMu.Lock (I/O outside lock)
```

Unexported `...Locked` helpers exist for in-package callers that already hold
the lock (e.g., `prepareTurn` calls `applyModelOverrideLocked`).

### Transition coordinator (cross-package API)

`transitionMu` lives on a shared coordinator accessible from both
`hostTurn` (turn-start path) and `SessionStateHandler` (transition path).
The coordinator is an interface or struct passed to both:

```go
// TransitionCoordinator serializes session transitions, turn starts,
// and idle maintenance (Compact). Implemented by the host layer;
// the handler receives it as a dependency.
type TransitionCoordinator interface {
    // WithTransition acquires the transition lock for a session transition
    // (LoadSession, InitNewSession). Rejects if a turn is active.
    // The fn runs while the lock is held; the turn-start path blocks.
    WithTransition(fn func() error) error

    // WithIdleMaintenance acquires the transition lock for idle-only
    // maintenance (Compact). Rejects if a turn is active.
    // The fn runs while the lock is held; turn-start blocks.
    WithIdleMaintenance(fn func() error) error

    // WithTurnStart acquires the transition lock for turn start,
    // then runs fn (which does claimSession + turnActive set under
    // hostTurn.mu). Rejects if a transition is in progress.
    WithTurnStart(fn func() error) error
}
```

All three methods use the same `transitionMu` internally. This is the
shared instance that prevents ABBA — both sides go through the coordinator.

---

## Phase 1: Fix pre-existing issues

### 1a. Move App field writes out of resetSessionBinding

`resetSessionBinding` (hostturn.go:543-574) writes `app.EventSink = nil` and
`app.OnTokRate = nil` while holding `hostTurn.mu`. This violates the "no App
access under hostTurn.mu" rule.

**Fix:** Move the `app.EventSink = nil` and `app.OnTokRate = nil` writes
OUTSIDE the `hostTurn.mu` critical section. The hostTurn fields
(`sessionEmit`, `turnEmit`, `sessionID`) stay under `hostTurn.mu`; the App
field writes happen after releasing `hostTurn.mu`.

### 1b. Compact — atomic idle reservation via transitionMu

The TOCTOU: `IsTurnActive()` → Compact is check-then-act. A turn can start
between the check and the Compact body.

**Fix:** Compact RPC handler calls `coordinator.WithIdleMaintenance(func() error { ... })`.
This acquires `transitionMu`, verifies idle (no turn active), and holds the
lock through the Compact operation. A turn starting during Compact blocks
on `transitionMu` until Compact finishes.

This also covers the Compact-RPC-vs-SaveSession-RPC race (both go through
the coordinator — Compact via WithIdleMaintenance, SaveSession via
SaveSessionDetached which doesn't need transitionMu but takes convMu which
Compact also takes for the non-Stream parts).

**Compact Conv access:** Compact must take `convMu` for the non-Stream
Conv reads/writes. The Stream call (summarizer) is done on a snapshot —
copy the `older` slice under `convMu.RLock`, release the lock, then call
the summarizer, then re-acquire `convMu.Lock` to swap the new Conv. Use a
generation check to detect if Conv changed during summarization (abort
compaction if it did).

### 1c. Expose a real IsTurnActive (not resetSessionBinding)

Add an `IsTurnActive() bool` method to the coordinator that reads
`turnActive` under `hostTurn.mu` — no side effects.

---

## Phase 2: Add stateMu and exported App methods

### App struct additions (`app.go`)

```go
// stateMu protects RPC-driven session settings from concurrent access.
// Lock ordering: stateMu is acquired BEFORE convMu. Never hold stateMu
// across I/O, network calls, or callbacks.
stateMu sync.RWMutex

// saveMu serializes SaveSession disk writes. Always standalone —
// acquired after stateMu and convMu are released.
saveMu sync.Mutex
```

### TurnSettings snapshot type

```go
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
    ClientModel          string
    ClientBackend        string
    ClientChatID         string
    CtxLimit             ContextLimit
    Workflow             Workflow  // snapshot
    InfoPanelOpen        bool
}
```

### SessionStateSnapshot type (for GetSessionState)

```go
type SessionStateSnapshot struct {
    // All fields GetSessionState currently reads, gathered in one
    // coherent snapshot under stateMu.RLock.
    TurnSettings
    BackendList  []BackendInfo
    ModelList    []string
    SessionLabel string
    ChatID       string
    PromptNote   string
    CtxLimitInfo ContextLimitInfo
    // ... etc
}
```

---

## Phase 3: Turn goroutine changes

### prepareTurn (turn_phases.go)

```go
func (a *App) prepareTurn() {
    a.stateMu.Lock()
    defer a.stateMu.Unlock()

    // Reset per-turn state (these are turn-scoped, not read by RPC)
    a.exhausted = false
    a.stopReason = ""
    a.turnBudgetStubbed = false
    a.confinementTripped = false
    a.confinementPathsHit = nil

    if a.CounselMode != "" {
        a.counselCalls = 0
        a.struggleSuggested = nil
    }

    // Apply model/backend selection to Client (turn-stable writes)
    if a.defaultModel == "" {
        a.defaultModel = a.Client.Model
    }
    if a.SelectedModel != "" {
        a.Client.Model = a.SelectedModel
    } else {
        a.Client.Model = a.defaultModel
    }
    a.Client.Backend = a.SelectedBackend
    a.Client.AuxModel = a.Cfg.AuxModel
}
```

Holds `stateMu.Lock` for field assignments only — no I/O, no callbacks.
This is a write lock because the turn goroutine writes `Client.Model/Backend`.
`GetSessionState` takes `stateMu.RLock` and sees a consistent state.

### Client.Model race with OpenAI-kind SetModel RPC

**Problem:** `ApplyModelOverride` for OpenAI kind writes `Client.Model`
directly from the RPC handler. This races with `Stream` reading `Client.Model`
on the turn goroutine (Stream doesn't take stateMu).

**Fix:** For OpenAI kind, `SetModelOverride` (the exported locked method)
writes `Client.Model`, `Client.ConfiguredModel`, `Cfg.Endpoint.Model` under
`stateMu.Lock`. The turn goroutine's `prepareTurn` also writes `Client.Model`
under `stateMu.Lock`. `Stream` reads `Client.Model` without a lock — BUT
`Stream` is only called from the turn goroutine, and `prepareTurn` runs
before `Stream` on the same goroutine. The race is RPC-write vs turn-read.

Since `Stream` can't take `stateMu` (it's in the proxy package), the fix is:
**block SetModel during an active turn for OpenAI kind**, same as Compact.
The coordinator's `WithIdleMaintenance` (or a simpler `WithTransition`)
rejects the RPC if a turn is active. For ilm-proxy kind, SetModel only
writes `SelectedModel` (not `Client.Model`), so no race.

### checkEgressConsent (turn_phases.go) — TOCTOU fix

```go
func (a *App) checkEgressConsent() bool {
    // Snapshot backend under lock
    a.stateMu.RLock()
    backend := a.SelectedBackend
    a.stateMu.RUnlock()

    if backend == "" || !IsExternalBackend(a.BackendList, a.Cfg, backend) {
        return true
    }
    if a.consentedBackends != nil && a.consentedBackends[backend] {
        return true
    }

    // Confirm is a blocking callback — NO lock held
    if !a.Confirm("external_backend", ...) {
        // Decline: conditional write — only revert if backend hasn't changed
        a.stateMu.Lock()
        if a.SelectedBackend == backend {
            a.SelectedBackend = ""
            a.Client.Backend = ""
        }
        a.stateMu.Unlock()
        return false
    }

    // Record consent for the target backend
    if a.consentedBackends == nil {
        a.consentedBackends = make(map[string]bool)
    }
    a.consentedBackends[backend] = true
    return true
}
```

Lock acquired AFTER Confirm returns, not around it.
Conditional write prevents TOCTOU (if backend changed during prompt,
don't revert the new selection).

### streamTurn — RawTools read

**Decision: RawTools is LIVE (re-read per tool result).**

```go
// In streamTurn at line 249:
a.stateMu.RLock()
rawTools := a.RawTools
a.stateMu.RUnlock()
if !rawTools {
    // cap tool result
}
```

### activeThresholds / EffectiveCtxCap — locked reads

```go
func (a *App) EffectiveCtxCap() int {
    a.stateMu.RLock()
    defer a.stateMu.RUnlock()
    if a.EffectiveCtxMaxCharsOverride != -1 {
        return a.EffectiveCtxMaxCharsOverride
    }
    return a.Cfg.EffectiveCtxMaxChars
}
```

`activeThresholds` reads `CtxLimit` and calls `EffectiveCtxCap` — both
under `stateMu.RLock`.

### Subagent overrides — LIVE at dispatch time

**Decision (resolved): Subagent overrides are LIVE — read at dispatch time
under stateMu.RLock, stable within a single dispatch.**

```go
func (a *App) resolveSubagentEndpointName() string {
    a.stateMu.RLock()
    defer a.stateMu.RUnlock()
    if a.SubagentEndpointOverride != "" {
        return a.SubagentEndpointOverride
    }
    return a.Cfg.SubagentEndpoint
}
```

### MaxParallelSubagents — turn-stable per batch

**Decision: MaxParallelSubagents is turn-stable per batch (read once at
batch start under stateMu.RLock, used for the whole batch).**

### Cfg field access

`Cfg` is a struct. Some fields are RPC-mutable (MaxParallelSubagents,
MashuraPanels, OracleModel, etc.), most are read-only after startup.

**Decision:** Protect the RPC-mutable Cfg fields under `stateMu`. All
other Cfg fields are immutable after startup (set in constructor, never
written again). The race detector won't flag different fields of the same
struct if they're not adjacent in memory — but to be safe, document that
RPC-mutable Cfg fields are under stateMu and all readers must participate.

`Cfg.AuxModel` (read in prepareTurn) is immutable after startup — safe.
`Cfg.CompactAtFrac`, `Cfg.KeepBytesFrac`, etc. — immutable — safe.

---

## Phase 4: SaveSession redesign

### Current problem

SaveSession mutates `a.Session.Conv`, `a.Session.Updated`,
`a.Session.Workspace`, `a.Session.SavedWorkflow` directly, then calls
`WriteSession(a.Session)`. Two concurrent saves race on Session mutation.
The Conv slice is aliased (not copied).

### New design: nested snapshot + detached write

```go
func (a *App) SaveSession() {
    a.saveSessionDetached()
}

func (a *App) saveSessionDetached() {
    // Nested snapshot: stateMu → convMu (consistent ordering)
    a.stateMu.RLock()
    a.convMu.RLock()

    if a.Session == nil {
        a.convMu.RUnlock()
        a.stateMu.RUnlock()
        return
    }

    // Deep copy Session value (not pointer)
    snap := *a.Session           // copy struct value
    snap.Conv = append([]proxy.Message(nil), a.Session.Conv...) // copy slice
    snap.SavedWorkflow = a.Workflow  // Workflow is an interface; snapshot the value
    snap.Updated = time.Now()
    if snap.Workspace == "" {
        snap.Workspace = a.SessionWorkspace()
    }
    snap.SavedWorkflow = a.Workflow

    a.convMu.RUnlock()
    a.stateMu.RUnlock()

    // Serialized write (no App locks held)
    a.saveMu.Lock()
    defer a.saveMu.Unlock()
    if err := WriteSession(&snap); err != nil {
        // warning (existing behavior)
    }
}
```

`SetSessionLabelValue` follows the same pattern:
```go
func (a *App) SetSessionLabelValue(label string) {
    a.stateMu.Lock()
    if a.Session != nil {
        a.Session.Label = label
    }
    a.stateMu.Unlock()
    a.SaveSession() // calls saveSessionDetached
}
```

### Deep copy safety

`proxy.Message` contains `Content *string` (pointer — immutable after
creation), `ToolCalls []ToolCall` (slice — not mutated after append). The
shallow slice copy (`append(nil, src...)`) creates a new backing array
with the same Message values. Since Messages are not mutated in place
(new messages are appended, not modified), this is safe. `WriteSession`
marshals the snapshot, which is detached from the live Conv.

---

## Phase 5: Transition serialization

### LoadSession handler (revised)

```go
func (h *SessionStateHandler) LoadSession(ctx context.Context, req ...) (*resp, error) {
    // Validate BEFORE acquiring any lock
    s, err := agent.LoadSessionScoped(...)
    if err != nil { return nil, ... }

    // Atomic transition via coordinator
    err = h.coordinator.WithTransition(func() error {
        // resetSessionBinding checks turnActive (under hostTurn.mu)
        if h.resetSessionBinding != nil {
            if err := h.resetSessionBinding(); err != nil {
                return err
            }
        }
        h.resetRestoreDone()

        // Install session atomically (stateMu + convMu inside)
        h.app.InstallSession(s)
        return nil
    })
    if err != nil { return nil, ... }

    // Build response (no locks needed — s is our copy)
    ...
    return resp, nil
}
```

### InstallSession method

```go
func (a *App) InstallSession(s *Session) {
    a.stateMu.Lock()
    defer a.stateMu.Unlock()

    a.RevokeAuto()      // atomic CAS, safe
    a.SetAllowReads(false) // atomic CAS, safe

    if a.Client != nil {
        a.Client.ChatID = s.ChatID
    }

    a.convMu.Lock()
    a.Conv = append([]proxy.Message(nil), s.Conv...)
    a.convMu.Unlock()

    a.Session = s
    a.SetWorkflowLocked(s.SavedWorkflow) // convMu already not held — see below
    a.consentedBackends = nil  // clear per-session consent
    a.preambleDay = ""         // reset for new session
}
```

Wait — `SetWorkflow` uses `convMu`. If we're inside `stateMu.Lock` and
call `SetWorkflow` which takes `convMu.Lock`, that's nested
`stateMu → convMu` — correct ordering. But `SetWorkflow` is an exported
method that takes `convMu.Lock` internally. If `InstallSession` calls
`SetWorkflow`, it works. But if we already hold `convMu` from the Conv
copy, we can't call `SetWorkflow` (re-entrant lock).

Fix: set Workflow directly under the same `convMu.Lock`:
```go
    a.convMu.Lock()
    a.Conv = append([]proxy.Message(nil), s.Conv...)
    a.Workflow = s.SavedWorkflow
    a.convMu.Unlock()
```

### InitNewSession handler (revised)

```go
func (h *SessionStateHandler) InitNewSession(ctx context.Context, req ...) (*resp, error) {
    chatID := agent.NewChatID()

    err = h.coordinator.WithTransition(func() error {
        if h.resetSessionBinding != nil {
            if err := h.resetSessionBinding(); err != nil {
                return err
            }
        }
        h.resetRestoreDone()
        h.app.NewConversationTransition(chatID)
        return nil
    })
    if err != nil { return nil, ... }

    return connect.NewResponse(&v1alpha1.InitNewSessionResponse{ChatId: chatID}), nil
}
```

### NewConversationTransition method

```go
func (a *App) NewConversationTransition(chatID string) {
    a.stateMu.Lock()
    defer a.stateMu.Unlock()

    a.convMu.Lock()
    a.Conv = nil
    a.convMu.Unlock()

    a.preambleDay = ""
    a.Client.ChatID = chatID
    a.Session = &Session{
        ChatID:    chatID,
        Model:     a.Client.Model,
        Created:   time.Now(),
        Workspace: a.SessionWorkspace(),
    }
    a.RevokeAuto()
    a.SetAllowReads(false)
    a.consentedBackends = nil
}
```

### Turn-start (hostturn.go run, revised)

```go
func (ht *hostTurn) run(ctx context.Context, in sessionhost.TurnInput) (text string, retErr error) {
    // Atomic claim + activate via coordinator
    err := ht.coordinator.WithTurnStart(func() error {
        if err := ht.claimSession(in.SessionID); err != nil {
            return err
        }
        ht.mu.Lock()
        defer ht.mu.Unlock()
        if ht.released {
            return fmt.Errorf("%w: released", sessionhost.ErrInternal)
        }
        if ht.turnActive {
            return fmt.Errorf("%w: already active", sessionhost.ErrInternal)
        }
        ht.turnActive = true
        return nil
    })
    if err != nil { return "", err }

    defer func() {
        ht.mu.Lock()
        ht.turnActive = false
        ht.mu.Unlock()
    }()

    // ... rest of run (no transitionMu held) ...
}
```

`WithTurnStart` acquires `transitionMu`, runs fn (which does claim +
turnActive under hostTurn.mu), releases `transitionMu`. If a transition is
in progress, turn-start blocks until it finishes.

### Multi-RPC transition gap (LoadSession → RestoreRepoStateResume)

**Decision:** LoadSession installs Session/Conv/Workflow atomically.
RestoreRepoStateResume applies endpoint-independent settings separately.
A turn starting between them sees the loaded session with pre-restore
settings. This is **acceptable and documented**:

- The loaded session's settings were different from the current repo-state
  anyway (that's why we're restoring).
- RestoreRepoStateResume fires immediately after LoadSession from the
  client. The window is tiny (client-side sequential RPC calls).
- Turn-stable fields (model, backend) are correct from LoadSession. Only
  endpoint-independent settings (auto, rawtools, counsel, maxpar, maxctx)
  may be stale, and they're "live" fields that update on the next RPC.

**Mitigation:** The client calls RestoreRepoStateResume before sending any
turn input. If the user somehow sends input between the two RPCs, the
turn runs with the loaded session's settings — which is the behavior the
user would expect (they loaded a specific session).

### RestoreRepoState — I/O outside lock

```go
func (h *SessionStateHandler) RestoreRepoState(ctx context.Context, req ...) (*resp, error) {
    // ... guard ...

    // Phase 1: Load from disk (no lock)
    result := agent.RestoreRepoStateRead(app) // reads file, returns pending values

    // Phase 2: Network probe (no lock)
    if result.NeedsCtxProbe {
        ctxLimit := agent.ResolveContextLimitForBackendModel(ctx, ...)
        // store ctxLimit for apply phase
    }

    // Phase 3: Apply under lock
    app.RestoreRepoStateApply(result, ctxLimit)

    return resp, nil
}
```

`RestoreRepoState` is split into read (I/O) → resolve (network) → apply
(lock). Same for `RestoreRepoStateResume`.

---

## Phase 6: RPC handler changes (summary)

| Handler | Change |
|---------|--------|
| `SetModel` | Calls `app.SetModelOverride(model)`. For OpenAI kind, goes through `coordinator.WithTransition` (rejects during active turn). |
| `SetBackend` | Calls `app.SetBackendSelection(backend, model)`. Under `stateMu.Lock` inside the method. |
| `SetRawTools` | Calls `app.SetRawToolsValue(v)`. |
| `SetCounselMode` | Calls `app.SetCounselModeValue(mode, cap)`. |
| `SetSubagentModel` | Calls `app.SetSubagentOverrides(ep, model)`. |
| `SetSubagentEndpoint` | Calls `app.SetSubagentOverrides(ep, model)`. |
| `SetMaxCtx` | Calls `app.SetMaxCtxOverride(v)`. |
| `SetMaxParallel` | Calls `app.SetMaxParallel(n)`. |
| `SetSessionLabel` | Calls `app.SetSessionLabelValue(label)`. |
| `GetSessionState` | Calls `app.SnapshotSessionState()` — one coherent read. |
| `RestoreRepoState` | Split: read (I/O) → resolve (network) → apply (lock). |
| `RestoreRepoStateResume` | Same split. |
| `Compact` | `coordinator.WithIdleMaintenance(func() { app.Compact(...) })`. |
| `LoadSession` | `coordinator.WithTransition(func() { resetSessionBinding; app.InstallSession(s) })`. |
| `InitNewSession` | `coordinator.WithTransition(func() { resetSessionBinding; app.NewConversationTransition(chatID) })`. |
| `SaveRepoState` | No App field reads needed — just disk I/O. |

---

## Phase 7: Race tests

New file: `internal/agent/daemon_race_test.go`

Scenarios:
1. Concurrent `SetModel` + `prepareTurn` + `GetSessionState` — no race
2. Concurrent `SetBackend` + `checkEgressConsent` + `GetSessionState`
3. `LoadSession` + concurrent turn start — no interleaving (turn blocks or rejects)
4. `InitNewSession` + concurrent turn start
5. Concurrent `SaveSession` (turn defer) + `SetSessionLabel` (RPC) — no torn save
6. `RestoreRepoState` + `activeThresholds` (CtxLimit race)
7. `Compact` RPC + turn start — Compact blocks until turn finishes
8. `Compact` RPC + `SaveSession` RPC (both idle) — no Conv race
9. Concurrent `SetCounselMode` + `mashura.go` counsel reads
10. `SetMaxParallel` + `subagent_parallel.go` read — no race

All tests run under `go test -race`.

---

## Snapshot Semantics (final, resolved)

| Field | Semantics | Implementation |
|-------|-----------|----------------|
| `SelectedModel` | Turn-stable | Snapshot in prepareTurn |
| `SelectedBackend` | Turn-stable | Snapshot in prepareTurn |
| `Client.Model/Backend/AuxModel` | Written at prepareTurn | Under stateMu.Lock |
| `RawTools` | **Live** | Re-read under stateMu.RLock per tool result |
| `CounselMode` | Turn-stable | Snapshot in prepareTurn |
| `MaxCounsel` | Turn-stable | Snapshot in prepareTurn |
| `SubagentModelOverride` | **Live** | Read under stateMu.RLock at dispatch time |
| `SubagentEndpointOverride` | **Live** | Read under stateMu.RLock at dispatch time |
| `EffectiveCtxMaxCharsOverride` | **Live** | Read under stateMu.RLock in activeThresholds |
| `CtxLimit` | Turn-stable | Snapshot in prepareTurn (RPC-mutable CtxLimit written under stateMu.Lock) |
| `MaxParallelSubagents` | **Turn-stable per batch** | Read under stateMu.RLock at batch start |
| `Session` | Transition-stable | Under stateMu |
| `Client.ChatID` | Transition-stable | Under stateMu |
| `consentedBackends` | Turn-scoped | Only turn goroutine; cleared on transition |
| `preambleDay` | Turn-scoped | Only turn goroutine; cleared on transition |
| `Workflow` | Transition-stable | Under convMu (set in InstallSession) |
| `InfoPanelOpen` | Transition-stable | Under stateMu |
| `struggleSuggested` | Turn-scoped | Only turn goroutine (counsel path) |
| `counselCalls` | Turn-scoped | Only turn goroutine |

---

## Implementation Order

1. **Phase 1:** Fix pre-existing — move App writes out of resetSessionBinding, add IsTurnActive
2. **Phase 2:** Add `stateMu`, `saveMu`, `TurnSettings`, `SessionStateSnapshot`, exported methods
3. **Phase 3:** Wire turn goroutine — prepareTurn lock, checkEgressConsent TOCTOU, streamTurn RawTools read, activeThresholds lock, subagent dispatch locks
4. **Phase 4:** Redesign SaveSession — nested snapshot + detached write
5. **Phase 5:** Add transition coordinator, wire LoadSession/InitNewSession/turn-start/Compact
6. **Phase 6:** Wire remaining RPC handlers to exported methods
7. **Phase 7:** Split RestoreRepoState/Resume into read/resolve/apply
8. **Phase 8:** Race tests
9. Build + vet + test -race
10. Mashūra review of implementation
11. Fix issues, commit

## Files Changed (estimated ~12 files)

| File | Changes |
|------|---------|
| `internal/agent/app.go` | Add stateMu, saveMu, TurnSettings, SessionStateSnapshot, exported methods, SaveSession redesign |
| `internal/agent/turn_phases.go` | prepareTurn takes stateMu.Lock, checkEgressConsent TOCTOU, streamTurn RawTools read |
| `internal/agent/compact.go` | activeThresholds/EffectiveCtxCap under stateMu.RLock, Compact convMu + generation check |
| `internal/agent/subagent.go` | resolveSubagent* under stateMu.RLock |
| `internal/agent/subagent_parallel.go` | MaxParallelSubagents under stateMu.RLock |
| `internal/agent/repostate.go` | Split RestoreRepoState/Resume into read/resolve/apply |
| `internal/agent/commands.go` | ApplyModelOverride under stateMu.Lock |
| `internal/server/connect/session_state_handler.go` | All handlers use exported methods + coordinator |
| `internal/wiring/hostturn.go` | Turn-start via coordinator, move App writes out of hostTurn.mu |
| `internal/wiring/coordinator.go` (new) | TransitionCoordinator implementation |
| `internal/agent/daemon_race_test.go` (new) | Race tests |
| `internal/agent/app_state.go` (new, optional) | Exported state methods if app.go gets too large |
