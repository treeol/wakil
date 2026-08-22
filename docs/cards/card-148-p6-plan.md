# P6 — Daemon-Mode TUI Feature Parity

**Goal:** Make `wakil --daemon` fully feature-equal with embedded `wakil`. All slash commands, consent toggles, model/backend switching, and info panels work remotely.

Branch: `feature/wakild-daemon`
Prerequisites: P5 complete (commit `c6cd06d`).

---

## Problem

The `RemoteFacade` (`internal/remote/facade.go`) is a thin client that delegates SessionService and EventReader calls to the daemon over Connect RPCs. But everything that touches `agent.App` state — slash commands, consent, model/backend switching, snapshot info, completion data — is either a no-op or returns `Handled=false` (which causes the TUI to submit the command as a regular turn to the LLM).

### Current state of RemoteFacade gaps

| Surface | Current behavior | What's needed |
|---|---|---|
| `DispatchCommand` | Only handles `/quit`, `/new`, `/reset`, `/resume`; everything else → `Handled=false` | Handle `/model`, `/backend`, `/auto`, `/compact`, `/info`, `/mcp`, `/plan`, `/handoff`, `/remember`, `/recall`, `/sessions`, `/subagent`, `/submodel`, `/maxpar`, `/maxctx`, `/counsel`, `/mashura`, `/rawtools`, `/repostate`, `/image`, `/session`, `/learn`, `/verify`, `/help` |
| `Snapshot()` | Returns session ID + title + conv only; Model, Backend, ContextLimit, ModelList, BackendList, Tools, Workflow all empty | Populate from daemon state |
| `Consent()` | Returns zero `Consent{}` | Read consent from daemon |
| `Info()` | Returns empty `InfoSnapshot{}` | Populate endpoint, model, cwd, context gauge, etc. |
| `SetAutoApprove/SetAllowDestructive/RevokeAuto` | No-ops (`bumpVersion` only) | Toggle consent on the daemon |
| `CompletionSource` | `Models()`, `Backends()`, `Sessions()` return nil | Query daemon for model list, backends, sessions |
| `SaveRepoState` | No-op | Persist repo state on daemon |
| `StartSideQuestion/CancelSideQuestion` | No-ops | Wire through daemon async ops |

### Root cause

The daemon owns the agent state (`*agent.App` lives in the daemon process). The TUI's `RemoteFacade` has no direct access to `App` fields. The Connect API exposes session/event/auth/backend/workspace/agent CRUD — but it does NOT expose:
1. **Session-scoped state**: model, backend, consent, context limit, tools, workflow — these live on the in-process `App` and are never sent over the wire.
2. **Slash-command interpretation**: the daemon's session host runs `agent.HandleTUICommand` only in embedded mode. In daemon mode, the TUI's `RemoteFacade.DispatchCommand` is the only command handler, and it's incomplete.
3. **Consent state**: the daemon owns consent (auto-approve, allow-destructive). The TUI can't read or write it.

---

## Approach

Two new proto services + wiring the RemoteFacade to call them:

### 1. SessionStateService (new proto) — read/write session-scoped state

```protobuf
service SessionStateService {
  rpc GetSessionState(GetSessionStateRequest) returns (SessionState);
  rpc SetModel(SetModelRequest) returns (SetModelResponse);
  rpc SetBackend(SetBackendRequest) returns (SetBackendResponse);
  rpc SetAutoApprove(SetAutoApproveRequest) returns (SetAutoApproveResponse);
  rpc SetAllowDestructive(SetAllowDestructiveRequest) returns (SetAllowDestructiveResponse);
  rpc RevokeAuto(RevokeAutoRequest) returns (RevokeAutoResponse);
  rpc SetSubagentEndpoint(SetSubagentEndpointRequest) returns (SetSubagentEndpointResponse);
  rpc SetSubagentModel(SetSubagentModelRequest) returns (SetSubagentModelResponse);
  rpc SetMaxParallelSubagents(SetMaxParallelSubagentsRequest) returns (SetMaxParallelSubagentsResponse);
  rpc SetEffectiveCtxMax(SetEffectiveCtxMaxRequest) returns (SetEffectiveCtxMaxResponse);
  rpc SetRawTools(SetRawToolsRequest) returns (SetRawToolsResponse);
  rpc SetCounselMode(SetCounselModeRequest) returns (SetCounselModeResponse);
  rpc Compact(CompactRequest) returns (CompactResponse);
  rpc SaveRepoState(SaveRepoStateRequest) returns (SaveRepoStateResponse);
  rpc SetSessionLabel(SetSessionLabelRequest) returns (SetSessionLabelResponse);
}
```

This maps directly to the `sessionclient.Facade` mutation methods and `HandleTUICommand` cases. The daemon-side handler calls into the `*agent.App` (which it owns) to read/set state.

### 2. Wire RemoteFacade to call SessionStateService

- `DispatchCommand` — translate each slash command to the appropriate `SessionStateService` RPC
- `Snapshot()` — call `GetSessionState` and populate `ClientSnapshot`
- `Consent()` — read from cached `GetSessionState` response
- `Info()` — call `GetSessionState` and populate `InfoSnapshot`
- `SetAutoApprove` etc. — call the matching RPC
- `CompletionSource` — query `GetSessionState` for model/backend lists

### 3. Wire the daemon-side handler

A new `SessionStateHandler` in `internal/server/connect/` that holds a reference to the `*agent.App` (or a thread-safe accessor) and translates RPCs to `App` method calls. This is the key: the daemon already HAS the `App`, it just doesn't expose its state over the wire.

### 4. Commands that submit turns (/plan, /learn, /remember, /recall, /verify, /compact)

These commands produce a `CommandResult.Submit` string that the TUI sends as the next turn via `SubmitInput`. They need no daemon-side handler — the TUI constructs the submit text locally and the existing `SubmitInput` RPC carries it.

### 5. Commands that are TUI-local (/info, /help, /sessions, /cwd, /mode, /history, /image)

These are purely display or TUI-local actions. They don't need daemon state. The RemoteFacade can handle them locally (return `Notice` from cached state, or `ClipboardImage` for `/image clipboard`).

---

## P6 Chunks

### P6a — SessionStateService proto + daemon handler
- Proto: `session_state.proto` + `session_state.proto` (new)
- Handler: `session_state_handler.go` — calls into `*agent.App` on the daemon
- Wire into `Server` and `cmd/wakild/server.go`
- Tests: handler-level RPC tests

### P6b — RemoteFacade: Snapshot, Consent, Info, CompletionSource
- Populate `Snapshot()` from `GetSessionState` RPC (model, backend, context limit, tools, workflow)
- Populate `Consent()` from cached state
- Populate `Info()` from `GetSessionState` RPC (endpoint, model, cwd, costs)
- Populate `CompletionSource` (models, backends, sessions from daemon)
- Cache the state response and refresh on events

### P6c — RemoteFacade: Consent mutations + slash command dispatch
- `SetAutoApprove/SetAllowDestructive/RevokeAuto` → call SessionStateService RPCs
- `DispatchCommand` — handle all remaining slash commands:
  - `/model <name>` → `SetModel` RPC → `CommandResult{Notice}`
  - `/backend <name>` → `SetBackend` RPC → `CommandResult{Notice}`
  - `/auto` → `SetAutoApprove` RPC → `CommandResult{Notice}`
  - `/auto destructive` → `SetAllowDestructive` RPC → `CommandResult{Notice}`
  - `/compact` → `Compact` RPC → `CommandResult{Compacted: true}`
  - `/subagent <name>` → `SetSubagentEndpoint` RPC → `CommandResult{Notice}`
  - `/submodel <name>` → `SetSubagentModel` RPC → `CommandResult{Notice}`
  - `/maxpar <N>` → `SetMaxParallelSubagents` RPC → `CommandResult{Notice}`
  - `/maxctx <N>` → `SetEffectiveCtxMax` RPC → `CommandResult{Notice}`
  - `/rawtools` → `SetRawTools` RPC → `CommandResult{Notice}`
  - `/counsel <mode>` → `SetCounselMode` RPC → `CommandResult{Notice}`
  - `/session name "..."` → `SetSessionLabel` RPC → `CommandResult{Notice}`
  - `/repostate clear` → `SaveRepoState` RPC → `CommandResult{Notice}`
  - `/plan <task>` → `CommandResult{Submit: "..."}` (TUI builds plan task text)
  - `/plan status/approve/abort/review/verify` → handle via state or submit
  - `/handoff` → needs session history; wire via RPC or degrade gracefully
  - `/remember <query>` → `CommandResult{Submit: "..."}` (submit as turn)
  - `/recall <id>` → `CommandResult{Submit: "..."}` (submit as turn)
  - `/learn` → `CommandResult{Submit: "learn this for next time"}`
  - `/verify` → `CommandResult{Submit: "verify"}`
  - `/mcp` → `CommandResult{Notice}` from cached state (or `GetSessionState`)
  - `/info` → `CommandResult{Notice}` (TUI-local, cached state)
  - `/sessions` → already works via `ListSessions`
  - `/cwd`, `/mode`, `/history` → `CommandResult{Notice}` from cached state
  - `/image` → handle locally (clipboard/image paths)
  - `/help` → `CommandResult{Notice}` (static text)

### P6d — SaveRepoState + RepoState persistence
- `SaveRepoState` → call `SaveRepoState` RPC
- Daemon-side handler calls `app.SaveRepoState`
- Repo-state restore on session resume

DONE (commit pending):
- Added `RestoreRepoState` RPC (`restore` applies persisted per-workspace settings
  once per daemon App lifetime; the daemon re-resolves context limits server-side
  and returns the summary). Client-invoked from `BootstrapRemote` on the
  fresh-conversation path only (never resume), mirroring embedded `BootstrapTUI`.
- The remote facade's `SaveRepoState` no-op now forwards an `AutoApprove=true`
  mutator to `SetAutoApprove` (idempotent; the daemon's own RPCs already persist
  each field). `ConsumeStartupNote` surfaces the restore summary. `RevokeAuto`
  handler now persists `AutoApprove=false` (parity with `SetAutoApprove`).

### P6e — Side questions
- `StartSideQuestion` → wire to daemon async ops (already exist for Mashura; reuse the pattern)
- `CancelSideQuestion` → cancel daemon async op
- This is the lowest priority and may be deferred

### P6f — Verification
- `go build`, `go vet`, `go test -race`
- `buf lint`, `buf breaking`
- Manual: start daemon, connect TUI with `--daemon`, test every slash command

---

## Implementation order
P6a → P6b → P6c → P6d → P6e → P6f

P6a is the foundation (proto + handler). P6b and P6c are the bulk of the wiring. P6d and P6e are smaller. P6f is the gate.

---

## Complexity estimate

| Chunk | New files | New RPCs | Touches |
|---|---|---|---|
| P6a | 2 proto, 1 handler, 1 test | ~15 | `server.go`, `cmd/wakild/server.go` |
| P6b | 0 (edits only) | 0 | `internal/remote/facade.go` |
| P6c | 0 (edits only) | 0 | `internal/remote/facade.go` |
| P6d | 0 (edits only) | 0 | `internal/remote/facade.go`, handler |
| P6e | 0 (edits only) | 0 | `internal/remote/facade.go` |
| P6f | 0 | 0 | verification only |
