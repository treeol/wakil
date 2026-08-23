# Branch Audit: feature/wakild-daemon → master

**Date:** 2026-08-23  
**Scope:** Full branch audit before merge. 70 commits, 264 files, ~60k insertions.  
**Sources:** Inline code review + Mashūra 3-panel review (gpt-5.6-sol, claude-fable-5, glm-5.2)

## Build Status

- `go build ./...` — ✅ clean
- `go vet ./...` — ✅ clean
- `go test -race ./internal/agent/ ./internal/remote/ ./internal/server/connect/` — ✅ passes
- Stale `wakild` references remain in docs/ and code comments (historical, low priority)

---

## Findings by Priority

### 🔴 Blockers (must fix before merge)

#### B1. Consent state leaks across sequential daemon sessions

**Files:** `internal/agent/app.go:686-700`, `internal/server/connect/session_state_handler.go:548-602,643-660`

`App.NewConversation` does NOT clear consent (`AutoApprove`, `AllowDestructive`, `AllowReads`). Neither `InitNewSession` nor `LoadSession` resets consent. The daemon serves multiple sequential sessions on one App — so client A's `/auto destructive` grant survives into client B's fresh session.

**Fix:** Reset consent (and other ephemeral state) in `InitNewSession` and `LoadSession` handlers before installing new session state. At minimum call `app.RevokeAuto()` and `app.SetAllowReads(false)`.

#### B2. Data races: unsynchronized App field access from RPC handlers

**Files:** `internal/server/connect/session_state_handler.go` (all SetX handlers), `internal/agent/app.go`

The handler comment claims "single-App-single-turn" safety, but RPC handlers run on concurrent goroutines (connect-go spawns a goroutine per request). The following fields are written by RPC handlers and read by the turn goroutine without synchronization:

- `SelectedBackend`, `SelectedModel` — `SetModel`/`SetBackend` handlers write, turn reads
- `RawTools` — `SetRawTools` writes, turn reads
- `CounselMode`, `MaxCounsel` — `SetCounselMode` writes, turn reads
- `SubagentEndpointOverride`, `SubagentModelOverride` — handlers write, turn reads
- `Cfg.MaxParallelSubagents` — `SetMaxParallelSubagents` writes, turn reads
- `EffectiveCtxMaxCharsOverride` — `SetEffectiveCtxMax` writes, turn reads
- `Session` — `LoadSession` writes (`app.Session = s`), `GetSessionState` reads
- `InfoPanelOpen` — `SetInfoPanelOpen` (facade/TUI goroutine), `GetSessionState` reads

In the embedded path these are race-free because `HandleTUICommand` runs on the single TUI event loop. In the daemon path, the RPC handler breaks that invariant.

**Fix:** Route mutations through a mutex/serialization layer, or queue them onto the event loop. At minimum, add a `sync.RWMutex` on the App for session-scoped state fields.

#### B3. `goMutation` drops errors and reorders mutations

**File:** `internal/remote/facade.go:557-568`

`goMutation` fires RPCs on background goroutines that:
- Drop all errors (`_ = call(...)`)
- Don't update cached state after completion (TUI displays stale consent until next `refreshState`)
- Have no ordering guarantee (two rapid `/auto` toggles can execute out of order)
- Bump `snapshotVersion` BEFORE the RPC runs (advertises a change that may never land)

**Concrete failure:** `/auto on` then `/auto destructive` can arrive out of order → destructive rejected because auto is still off. User sees "auto: ON" but daemon didn't apply it.

**Fix:** After RPC completion, call `refreshState`. On error, surface a notice event. For ordering, use a per-fade mutation queue instead of independent goroutines.

#### B4. `restoreDone` guard: once-per-App-lifetime blocks multi-session restore

**File:** `internal/server/connect/session_state_handler.go:523-530`

`restoreDone` is set on the first `RestoreRepoState` call and never cleared. The daemon serves multiple sequential sessions — the second fresh conversation (`/new`) never gets repo-state restored. The guard comment says "matches embedded restore-once-per-process" but embedded = once-per-session (new process each time), while daemon = once-per-daemon-lifetime = once-for-all-sessions. These are not equivalent.

Additionally, `/new` rotation (`RemoteConversationManager.NewConversation`) never calls `RestoreRepoState` at all — only `BootstrapRemote` does, for the first conversation.

**Fix:** Reset `restoreDone` in `InitNewSession` (or `resetSessionBinding`). Call `RestoreRepoState` after `InitNewSession` in the rotation path.

#### B5. `LoadSession` non-atomic session transition

**File:** `internal/server/connect/session_state_handler.go:548-602`

`LoadSession` resets the host binding BEFORE validating/loading the requested session. If loading fails (not found, corrupt), the previous binding has already been cleared — the App is left in an unbound state. It then updates ChatID, Conv, Session, and Workflow in separate operations with no transition lock, so concurrent reads can observe mixed old/new state.

**Fix:** Use a transition lock covering: (1) reject active turn, (2) load/validate target, (3) reset ephemeral state, (4) install all session state, (5) bind host session.

### 🟠 High Priority (should fix before merge)

#### H1. `/auto` not restored on session resume

**Files:** `internal/agent/repostate.go:183-203`, `internal/wiring/bootstrap_tui.go:112`, `internal/remote/bootstrap.go:62-71`

`AutoApprove` is persisted to RepoState, but `RestoreRepoState` is deliberately skipped on resume (`if opts.RestoreRepoState && resumeID == ""`). The skip was designed to prevent model/backend changes mid-transcript, but it also blocks endpoint-independent settings (AutoApprove, RawTools, maxpar, maxctx, mashura, info panel) that have nothing to do with model/backend.

**Fix:** Split `RestoreRepoState` into two paths:
- Full restore (fresh conversation): model/backend + all endpoint-independent settings
- Partial restore (resume): only endpoint-independent settings, skip model/backend

Mashūra unanimously recommends: do NOT put consent in `Session` (a transcript file becoming a grant-bearer is a security risk). Restore from RepoState on resume instead, subject to `AutoExplicit` precedence.

#### H2. `SetCounselMode` doesn't persist to RepoState

**File:** `internal/server/connect/session_state_handler.go:399-434`

Every other mutation RPC handler calls `SaveRepoState` — `SetCounselMode` does not. `CounselMode` and `MaxCounsel` are also absent from `RepoState` struct entirely. This is likely an omission, not a deliberate decision.

**Fix:** Add `CounselMode` and `MaxCounsel` fields to `RepoState`, and add `SaveRepoState` calls in the handler. Or explicitly document why counsel is not persisted.

#### H3. `SetAllowDestructive` check-then-act breaks pair invariant

**File:** `internal/server/connect/session_state_handler.go:236-256`

The handler checks `app.Consent().AutoApprove` then calls `app.SetAllowDestructive(v)` as two separate operations. A concurrent `RevokeAuto` between them yields `AutoApprove=false, AllowDestructive=true` — the exact state the consent.go doc says can never be observed.

**Fix:** Add a single atomic conditional mutator: `EnableDestructiveIfAuto() bool` implemented in the CAS loop.

#### H4. `updateRepoState` read-modify-write is not concurrency-safe

**File:** `internal/agent/repostate.go:144-168`

Multiple RPC handlers can concurrently load the same old file, mutate different fields, and overwrite each other. Atomic rename prevents corruption but not lost updates.

**Fix:** Add a per-workspace mutex serializing all `updateRepoState` calls.

#### H5. Consent CAS initialization race

**File:** `internal/agent/consent.go:66-89`

Two concurrent first mutations that both observe `raw == nil` both `Store` — one update lost. The comment claims concurrent-writer safety but that's false before initialization. `bootstrap.go:176` does call `SetConsent` at construction, but the nil branch remains a latent race for tests/early access.

**Fix:** Guarantee constructor initialization and delete the nil branch, or use `atomic.Pointer[ConsentSnapshot]` instead of `atomic.Value`.

#### H6. RPC `session_id` not validated

**File:** `internal/server/connect/session_state_handler.go:19-24`

State RPCs accept `session_id` but don't validate it against the active session. A stale facade can mutate a newly-active session. `claimSession` protects turns but not state RPCs.

**Fix:** Add a session generation/lease token. Validate every state RPC against the active session/generation.

### 🟡 Medium Priority (should fix, may not block merge)

#### M1. Remote `SaveRepoState` facade only forwards `AutoApprove`

**File:** `internal/remote/facade.go:621-636`

`SaveRepoState` only forwards `AutoApprove` (when true). `InfoPanelOpen` and other mutator fields are silently dropped — no remote wire path. This means info panel preference is lost in daemon mode.

**Fix:** Add RPCs for the missing fields, or explicitly document them as TUI-local in daemon mode.

#### M2. `SaveRepoState` facade can't persist `false` values

**File:** `internal/remote/facade.go:624`

`if m.AutoApprove { ... }` — a mutator requesting `false` is indistinguishable from "unset". Needs `*bool` or presence semantics.

#### M3. `InfoPanelOpen` ownership contradiction

**Files:** `internal/remote/facade.go:641`, `internal/server/connect/session_state_handler.go:124`

Remote `SetInfoPanelOpen` is a pure no-op (just `bumpVersion`), but `GetSessionState` exposes it and `Info()` projects it. A stale daemon value fights the local toggle on every `refreshState`. Pick one owner: TUI-local or daemon-owned with a dedicated RPC.

#### M4. `LoadRepoState` doc/implementation mismatch

**File:** `internal/agent/repostate.go:108-133`

Doc says "workspace mismatch after re-resolution" is a sanity check, but the implementation only checks `SchemaVersion` — it never validates `st.Workspace` against `ws`. Either add the check or fix the doc.

#### M5. `Session.Model` not applied on resume

**Files:** `internal/wiring/conversation_manager.go:183-187`, `internal/server/connect/session_state_handler.go:574-581`

Neither embedded nor daemon resume paths apply `s.Model` from the loaded session. The resumed session runs with whatever model the App last had. The `Session.Model` field is persisted but never read back.

#### M6. Remote resume workspace scope mismatch

**File:** `internal/server/connect/session_state_handler.go:566`

Remote `LoadSession` uses `SessionScope{All: true}` (global search), while embedded resume uses workspace scoping + `IncludeLegacy`. Remote resume can load a session from a different workspace into an App whose `SessionWorkspace()` belongs to the daemon's workspace.

### 🟢 Low Priority (cleanup, optional before merge)

#### L1. Stale `wakild` references in code comments

**Files:** `cmd/wakil/main.go:35`, `cmd/wakil/daemon.go:17`, `cmd/wakil/daemon_server.go:506`, `internal/core/event/event.go:6`, `internal/core/service.go:6`, `internal/core/sessionhost/host.go:6`

These reference the old binary name in comments explaining merge history. Accurate but could be tidied. Historical docs (`docs/design/wakild-foundation.md`, `docs/cards/card-148-*.md`) are fine to leave.

#### L2. 11 commands unavailable in daemon mode

**File:** `internal/remote/facade.go:848-853`

`/handoff`, `/learn`, `/remember`, `/recall`, `/image`, `/mcp`, `/mashura`, `/plan`, `/verify`, `/sessions`, `/history` return "not available remotely in daemon mode". This is a known feature gap, not a bug. Document as daemon-mode limitations.

#### L3. Side questions are no-ops in daemon mode

**File:** `internal/remote/facade.go:676-679`

`StartSideQuestion` and `CancelSideQuestion` return no-ops. Known limitation.

#### L4. `TODO` in config.go

**File:** `internal/config/config.go:313` — `TODO(per-tool-briefing)` — pre-existing, not branch-introduced.

#### L5. `PLACEHOLDER` in mashura.go

**File:** `internal/agent/mashura.go:154` — comment references a placeholder pattern. Pre-existing, not branch-introduced.

---

## State Lifetime Classification

The branch conflates three lifetimes without a defined policy. This table is the recommended classification:

| State | Current Store | Recommended Lifetime | Restored on Resume? |
|---|---|---|---|
| Transcript, workflow | `Session` | Saved session | ✅ (already done) |
| Model/backend overrides | `RepoState` | Workspace preference | ❌ (skip on resume — endpoint-dependent) |
| Subagent endpoint/model | `RepoState` | Workspace preference | ✅ (endpoint-independent) |
| AutoApprove | `RepoState` | Workspace preference | ✅ (subject to `AutoExplicit`) |
| AllowDestructive | None (structural exclusion) | Ephemeral | ❌ (clear on every transition) |
| AllowReads | None | Ephemeral | ❌ (clear on every transition) |
| RawTools | `RepoState` | Workspace preference | ✅ |
| MaxParallelSubagents | `RepoState` | Workspace preference | ✅ |
| EffectiveCtxMaxChars | `RepoState` | Workspace preference | ✅ |
| CounselMode/MaxCounsel | None | Workspace preference | ✅ (needs adding to RepoState) |
| Mashura settings | `RepoState` | Workspace preference | ✅ |
| InfoPanelOpen | `RepoState` | TUI-local workspace pref | Depends on ownership decision |
| Policy | In-memory | Session or config | ❌ |

---

## Recommended Fix Order

1. **B1** — Reset consent on session transitions (security-critical)
2. **B2** — Add synchronization for App field access (race blocker)
3. **B4** — Fix `restoreDone` scoping (breaks multi-session)
4. **B3** — Fix `goMutation` error handling + ordering
5. **B5** — Make `LoadSession` atomic
6. **H1** — Split `RestoreRepoState` for resume
7. **H2** — Persist counsel mode
8. **H3** — Fix pair invariant race
9. **H4** — Add repo-state mutex
10. **H5** — Fix consent init race
11. **H6** — Validate RPC session_id
12. Remaining medium/low priority items
