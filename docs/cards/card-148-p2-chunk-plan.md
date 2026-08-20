# Card #148 — P2 Chunk Plan (Daemon + API) — REVISED

**Branch:** `feature/wakild-daemon`
**Prior:** P0 ✅, P1 ✅ @ `4ebcf7a`
**P2 exit gate (doc §P2):** Two TUI instances on one session show identical durable projection; reconnect after disconnect loses no durable event (seq contiguity); `buf breaking` in CI active.

---

## Key findings from codebase (informs this plan)

1. **D5 async approval is ALREADY IMPLEMENTED.** `host.go:438` implements `RespondToApproval` with a parking mechanism (`ApprovalParkFunc`), buffered(1) decision channel, idempotent resolution (same-outcome = no error, different = `ErrApprovalAlreadyResolved`), interrupt-during-approval, `SystemUserID` on forced decline. The adapter (`hostturn.go:543`) already has both sync (headless) and async (TUI via `ParkApproval`) paths. P2 only needs to expose this over the wire.

2. **TurnFunc is transport-free.** `internal/wiring/hostturn.go` provides `HostTurnFunc(app, opts)` → `sessionhost.TurnFunc`. The daemon calls `wiring.BuildApp(cfg, exe, opts)` + `HostTurnFunc(app)` — same as headless. `BuildApp` owns all construction: proxy client, MCP, LSP, browser, tools, memory, trace, staging, session history.

3. **`DeleteSession` does NOT exist** on any interface. `service.go:70` comment says "Session deletion is a P2 command." P2 must add it to `SessionService` (or a separate `SessionAdminService`).

4. **`CreateSessionRequest` has only Workspace + Title.** `agent_revision_id`/`backend_id` were NOT added in P1. P2 proto should include them as optional fields (additive, backward-compatible).

5. **`SessionSnapshot` is on `SessionReader`** (3 methods: `GetSession`, `ListSessions`, `SessionSnapshot`). The proto needs either a `SessionSnapshot` RPC or client-side composition with a consistency boundary (GetSession returns `last_seq=N`, ListEvents through `N`).

6. **Principal is server-resolved.** Core comments (`service.go:294-296`): "principal is resolved from auth and never read from the request body." P2 local mode: server constructs a fixed `EmbeddedPrincipal` per connection. Client NEVER sends authority. Socket permissions (0600) are the security boundary until P4 adds `SO_PEERCRED`.

7. **33 event kinds** confirmed in `event.go:70-104`. The design doc §3.4 shows 17 oneof variants (field numbers 10–26). The 16 additional 7b2/7c kinds continue from 27.

8. **Store encoding ≠ wire encoding.** Wire is always protobuf. The durable store `encoding` column stays `json-v1` in P2. `proto-v1` store encoding is deferred (out of P2 scope).

---

## Revised chunk order (per Mashūra feedback)

```
P2a — Proto schema + buf toolchain
P2b — Core additions: DeleteSession + SessionSnapshot RPC support
P2c — Connect server adapter (core↔proto bridge, error mapping, fixed principal)
P2d — wakild binary + Unix-socket transport
P2e — Remote client + TUI --daemon mode (dedup, reconnect, session attach)
P2f — Integration tests + buf breaking CI
```

The async approval (D5) requires NO core rework — it's already done. P2c exposes `RespondToApproval` over the wire. P2e makes the TUI call it remotely instead of through the in-process host. This collapses what was originally P2e into parts of P2c + P2e.

---

## P2a — Proto schema + buf toolchain

**Goal:** Define the wire contract in Protobuf and set up code generation.

### Setup
- Install `buf` CLI, `protoc-gen-go`, `protoc-gen-connect-go`
- Add to `go.mod`: `google.golang.org/protobuf`, `connectrpc.com/connect`, `buf.build/go/protoyaml` (if needed)
- Create `buf.yaml` (module: `github.com/treeol/wakil`), `buf.gen.yaml` (Go + Connect plugins)
- `buf.lock` for external proto deps (`google/protobuf/timestamp.proto`)

### Proto files
- `api/proto/wakil/v1alpha1/wakil.proto` — package `wakil.v1alpha1`, imports `google/protobuf/timestamp.proto`
- `api/proto/wakil/v1alpha1/event.proto` — `Event` message with `oneof payload` for all 33 kinds (field numbers 10–42, matching design doc 10–26 + 7b2/7c additions 27–42)
- `api/proto/wakil/v1alpha1/session.proto` — `SessionService` RPCs
- `api/proto/wakil/v1alpha1/event_service.proto` — `EventService` RPCs (StreamEvents server-streaming, ListEvents, GetSessionSnapshot)
- `api/proto/wakil/v1alpha1/system.proto` — `SystemService` (GetServerInfo, Health)

### Services (P2 surface)
```protobuf
service SystemService {
  rpc GetServerInfo(GetServerInfoRequest) returns (ServerInfo);
  rpc Health(HealthRequest) returns (HealthStatus);
}

service SessionService {
  rpc CreateSession(CreateSessionRequest) returns (Session);
  rpc GetSession(GetSessionRequest) returns (Session);
  rpc ListSessions(ListSessionsRequest) returns (ListSessionsResponse);
  rpc DeleteSession(DeleteSessionRequest) returns (DeleteSessionResponse);  // P2 per service.go:70
  rpc SubmitInput(SubmitInputRequest) returns (SubmitAck);
  rpc RespondToApproval(RespondToApprovalRequest) returns (RespondToApprovalResponse);
  rpc Interrupt(InterruptRequest) returns (InterruptResponse);
  rpc CloseSession(CloseSessionRequest) returns (CloseSessionResponse);
}

service EventService {
  rpc StreamEvents(StreamEventsRequest) returns (stream Event);
  rpc ListEvents(ListEventsRequest) returns (ListEventsResponse);
  rpc GetSessionSnapshot(GetSessionSnapshotRequest) returns (SessionSnapshot);
}
```

### Key messages
- `ServerInfo` — api_version (string), capabilities (repeated string), ephemeral (bool — flags no-durability mode)
- `Principal` — tenant_id, user_id, role, auth_method (server-constructed; client never sends authority fields)
- `Session` — id, tenant_id, workspace, state, last_seq, created_by, title, created_at, closed_at
- `CreateSessionRequest` — workspace (string), title (string), agent_revision_id (string, optional), backend_id (string, optional)
- `SubmitInputRequest` — session_id, text, read_action, request_id (idempotency key — P2 does NOT enforce dedup, just carries it)
- `SubmitAck` — session_id, turn_id
- `RespondToApprovalRequest` — session_id, approval_id, outcome (enum: APPROVAL_OUTCOME_UNSPECIFIED=0, DENY=1, ALLOW_ONCE=2, ALLOW_READS_ONCE=3), reason
- `StreamEventsRequest` — session_id, after_seq (uint64, exclusive cursor — 0 = from start)
- `ListEventsRequest` — session_id, after_seq, limit (int32 — 0 = server default, max enforced), before_seq (uint64, optional upper bound for snapshot consistency)
- `GetSessionSnapshotRequest` — session_id
- `SessionSnapshot` — session (Session), events (repeated Event), last_seq (uint64)

### Event oneof (33 kinds, field numbers)
Field numbers 10–26 from design doc §3.4. 7b2/7c additions 27–42:
```
10  session_created        27  user_message_committed
11  turn_started           28  conversation_compacted
12  message_delta          29  workflow_turn_started
13  reasoning_delta        30  workflow_final_review
14  tool_call_requested     31  async_job_started
15  approval_requested     32  async_job_completed
16  approval_resolved      33  side_question_completed
17  tool_call_completed    34  tok_rate
18  subagent_spawned       35  async_job_progress
19  subagent_progress      36  side_question_progress
20  subagent_completed     37  learn_nudge
21  memory_proposed        38  session_note
22  guard_triggered        39  workflow_outcome
23  context_warning        40  workflow_warning
24  turn_completed         41  (reserved for future)
25  session_error          42  (reserved for future)
26  session_closed
```
Note: `tool_call_requested` (14) is in the design doc but the actual kind is `tool_call_started` — use the actual kind name. Reserved numbers 41–42 for future additions.

### Compatibility rules (§3.2)
- `reserved` numbers and names for any removed fields
- Enums for states/outcomes (not free-form strings on the wire)
- Unknown oneof variants: preserve as bytes (default proto behavior)
- `buf lint` enforces: PACKAGE_DIRECTORY_MATCH, STANDARD_PACKAGE, ENUM_FIRST_VALUE_ZERO, etc.

### Generated code
- `api/gen/wakil/v1alpha1/` — generated `.pb.go` and `_connect.go` files (committed)
- Codegen drift check in CI: `buf generate && git diff --exit-code`

### Exit criteria
- `buf lint` passes
- `buf generate` produces committed code, `git diff --exit-code` clean
- `go build ./api/gen/...` passes
- `go vet ./...` passes
- Every event kind has a proto mapping (enumerated test)

---

## P2b — Core additions: DeleteSession + SessionSnapshot wire support

**Goal:** Add `DeleteSession` to the core interface and ensure `SessionSnapshot` has a wire path.

### DeleteSession
- Add `DeleteSession(ctx, principal, sessionID) error` to `SessionService` interface (`internal/core/service.go`)
- Implement on `Host` (`internal/core/sessionhost/host.go`) — mark session as deleted, emit `SessionClosed{reason: "deleted"}`, prevent further appends
- This is a soft-delete: the session and its events remain in the store (audit trail) but `GetSession`/`ListSessions` exclude it
- Add `ErrSessionNotFound` for already-deleted sessions (same as non-existent)
- Tests: delete → GetSession returns NotFound; delete → ListSessions excludes; delete → SubmitInput returns Closed; double-delete = NotFound

### SessionSnapshot
- `SessionSnapshot` is already on `SessionReader` — no core change needed
- The proto `GetSessionSnapshot` RPC maps directly to `host.SessionSnapshot()`
- Atomicity: the host reads `session.lastSeq` under `s.mu`, then reads events from the store. The store `Read` is point-in-time for durable events. A concurrent append may increase `lastSeq` after the snapshot is taken — the snapshot is consistent up to `lastSeq` at read time. This is acceptable (doc §3.4: "replay bundle, not pre-rendered projection").

### Exit criteria
- `DeleteSession` implemented + tested on Host
- `go test -race ./internal/core/...` green
- `go build ./...` passes

---

## P2c — Connect server adapter

**Goal:** Implement Connect service handlers that delegate to the existing host.

### Files
- `internal/server/connect/` — new package
- `internal/server/connect/event_conv.go` — `core.Event` ↔ `proto.Event` (oneof payload mapping for all 33 kinds)
- `internal/server/connect/session_handler.go` — SessionService Connect handler
- `internal/server/connect/event_handler.go` — EventService Connect handler (StreamEvents bridges `EventSubscription.Next` to server-stream; GetSessionSnapshot)
- `internal/server/connect/system_handler.go` — SystemService handler
- `internal/server/connect/principal.go` — server-side principal resolution (fixed `EmbeddedPrincipal` for P2 local mode; no client-supplied authority)
- `internal/server/connect/errors.go` — core sentinel errors → Connect error codes
- `internal/server/connect/converter_test.go` — round-trip test for all 33 event kinds (core→proto→core equality)
- `internal/server/connect/handler_test.go` — handler tests with a real host

### Error mapping
| Core sentinel | Connect code |
|---|---|
| `ErrSessionNotFound` | `NotFound` |
| `ErrSessionClosed` | `FailedPrecondition` |
| `ErrSessionBusy` | `ResourceExhausted` |
| `ErrNotAuthorized` | `PermissionDenied` |
| `ErrInvalidInput` | `InvalidArgument` |
| `ErrInvalidStateTransition` | `FailedPrecondition` |
| `ErrApprovalNotFound` | `NotFound` |
| `ErrApprovalAlreadyResolved` | `FailedPrecondition` (not `AlreadyExists` — per Mashūra: AlreadyExists is for resource creation collisions; FailedPrecondition is for "state doesn't allow this") |
| Context cancellation | `Canceled` |
| Deadline exceeded | `DeadlineExceeded` |
| Unknown internal error | `Internal` |

### StreamEvents bridge
1. Call `host.Subscribe(ctx, principal, sessionID, after)` → `EventSubscription`
2. Loop: `sub.Next(ctx)` → convert `core.Event` → `proto.Event` → `stream.Send`
3. Use the **handler's request context** (cancels on client disconnect)
4. On `io.EOF` or ctx cancel: `sub.Close()`, return
5. Ephemeral events (seq=0) sent live; durable events (seq>0) sent with their seq

### Principal resolution
- P2 local mode: server constructs `EmbeddedPrincipal()` for every request
- Client never sends principal authority fields
- The `Principal` proto message exists for response types (GetSession returns `created_by`) but is NOT in request messages
- Socket permissions (0600) are the security boundary; `--daemon` restricted to `unix://` paths in P2
- `GetServerInfo` reports `auth_method: "embedded"` so clients know auth is not active

### Hard rule (§2.1)
`internal/server/connect/` imports `api/gen` and `internal/core`. `internal/core` imports NEITHER — enforced by existing structural guard.

### Exit criteria
- Converter round-trip test: all 33 kinds core→proto→core equality ✅
- Handler tests: create session, submit input, stream events, list events, get snapshot, approval flow, delete session ✅
- `go test -race ./internal/server/...` green
- No client-supplied principal in any request message

---

## P2d — wakild binary + Unix-socket transport

**Goal:** A daemon binary that runs the Connect server over a Unix socket.

### Files
- `cmd/wakild/main.go` — entry point
- `cmd/wakild/config.go` — config (socket path, data dir, workspace roots, log level, shutdown timeout)
- `cmd/wakild/server.go` — server setup: open SQLiteStore, create host, register Connect handlers, listen
- `cmd/wakild/signal.go` — SIGTERM/SIGINT → graceful shutdown

### Config
- `--socket <path>` (default: `$XDG_RUNTIME_DIR/wakild.sock` — NOT `/tmp`; fallback `$HOME/.local/share/wakil/wakild.sock`)
- `--data-dir <path>` (default: existing agent data dir)
- `--workspace <path>` (repeatable; workspace roots the daemon serves)
- `--log-level <level>` (debug/info/warn/error)
- `--shutdown-timeout <duration>` (default: 10s — drain deadline)
- `--ephemeral` (explicit opt-in for MemLog; advertised via `GetServerInfo.ephemeral=true`)
- Reads existing `config.Config` for backend/model/credentials (same `wakil.yaml` the TUI reads)

### Store initialization
- **Fail-closed by default:** if SQLiteStore open/migration fails, `wakild` exits with an error
- `--ephemeral` flag: uses `MemLog`, advertises `ephemeral=true` in `GetServerInfo`
- This replaces the P1 best-effort MemLog fallback (which was appropriate for embedded mode but not daemon mode)

### Unix-socket transport
- Custom `net.Listener` on Unix socket
- Stale-socket handling: if socket exists, check if another process is listening (connect test); if not, unlink and rebind
- Socket permissions: `0600` (owner-only — security boundary until P4)
- Parent directory creation (`os.MkdirAll` with `0700`)
- Refuse to unlink a socket owned by another process/user
- HTTP server over the Unix socket (Connect works over HTTP/1.1)

### Daemon lifecycle
1. Parse config
2. Open SQLiteStore (fail-closed unless `--ephemeral`)
3. For each workspace: `wiring.BuildApp(cfg, exe, opts)` → `wiring.HostTurnFunc(app)` → `sessionhost.New(turnFn, WithStore(store))`
4. Register Connect handlers (SessionService, EventService, SystemService)
5. Listen on Unix socket (0600)
6. `GetServerInfo` reports ready after migrations + host init + listener
7. Serve until signal
8. Graceful shutdown:
   - Stop accepting new connections (`http.Server.Shutdown`)
   - Stop accepting new inputs (host stops queue)
   - Drain running turns up to `--shutdown-timeout`
   - Pending approvals: auto-decline with `SystemUserID` (existing Host behavior on ctx cancel)
   - Close host (drains sessions), close store, remove socket

### TurnFunc per session
The daemon needs a TurnFunc per session. `HostTurnFunc(app)` is bound to one `*agent.App`. The daemon creates a new `agent.App` per session via `wiring.BuildApp`. This means:
- One `agent.App` per session (not shared across sessions)
- The daemon's `TurnFunc` factory: given a `CreateSessionRequest`, call `BuildApp` + `HostTurnFunc` → `sessionhost.New(turnFn)`
- This is heavy (MCP/LSP/browser init per session) but correct for P2. Optimization (shared resources) is a later concern.

### Exit criteria
- `go build ./cmd/wakild` produces a working binary
- Binary starts, listens on Unix socket (0600), serves requests
- Unary curl test: `curl --unix-socket <path> -H 'Content-Type: application/json' -d '{}' http://localhost/wakil.v1alpha1.SystemService/Health`
- Graceful shutdown on SIGTERM (drains, removes socket)
- Store init failure → exit (not silent fallback)
- `--ephemeral` → starts with MemLog, `GetServerInfo.ephemeral=true`

---

## P2e — Remote client + TUI --daemon mode

**Goal:** TUI connects to a remote `wakild` via `--daemon <addr>` instead of running embedded.

### Files
- `internal/client/connect/` — new package
- `internal/client/connect/client.go` — Connect client implementing `SessionService`, `EventReader`, `SessionReader` via Connect RPCs
- `internal/client/connect/event_conv.go` — `proto.Event` → `core.Event` (reverse of server converter)
- `internal/client/connect/stream.go` — `EventSubscription` impl backed by a Connect server-stream, with dedup-by-seq and reconnect
- `cmd/wakil/main.go` — add `--daemon <addr>` and `--session <session-id>` flags

### Unix-socket dialer
- Custom `http.Transport` with `DialContext` for Unix sockets
- `--daemon unix:///path/to/socket` → dial Unix socket
- `--daemon` restricted to `unix://` in P2 (no TCP — unauthenticated)
- Synthetic HTTP base URL `http://unix/` for Connect client (the dialer ignores the host)

### Remote client implementation
The client implements the three core interfaces:
- `SessionService`: CreateSession, SubmitInput, RespondToApproval, Interrupt, CloseSession, DeleteSession — all via Connect unary RPCs
- `EventReader`: Subscribe (→ EventSubscription wrapping StreamEvents), ListEvents — via Connect
- `SessionReader`: GetSession, ListSessions, SessionSnapshot (via GetSessionSnapshot RPC)

### EventSubscription with dedup + reconnect
- Tracks `lastSeq` (highest durable seq seen)
- `Next(ctx)`:
  1. Read from internal channel (fed by stream goroutine)
  2. If durable event: dedup by seq — skip if seq ≤ lastSeq, update lastSeq
  3. If ephemeral event (seq=0): pass through (no dedup possible)
- Stream goroutine: calls `StreamEvents(after=lastSeq)`, loops on `stream.Receive()`
- On transport error: backoff, reconnect from `lastSeq+1`, continue
- On clean EOF: close the channel
- On ctx cancel: close stream, close channel
- Ephemeral events during reconnect are NOT recovered (doc: "may be dropped during replay")

### Session attach
- `--session <session-id>`: TUI attaches to an existing session
- No `--session`: TUI creates a new session (current behavior)
- Session picker: `ListSessions` → choose — deferred to P3 (Web UI); P2 uses explicit `--session`

### TUI changes
- `--daemon <addr>`: use Connect client instead of in-process host
- No flag (default): embedded mode (current behavior — no regression)
- The TUI's event consumer loop is unchanged — it receives `core.Event` from either source
- Approval in remote mode: TUI sees `ApprovalRequested` event → prompts user → calls `RespondToApproval` via RPC (same code path as embedded, different `SessionService` impl)
- The sync `Confirmer` callback is NOT used in remote mode — the async `ParkApproval` path handles it (already implemented)

### Exit criteria
- `wakil --daemon unix:///path --session <id>` connects and works
- Remote approval: client sees `ApprovalRequested`, calls `RespondToApproval`, turn resumes
- Client implements all methods of `SessionService`, `EventReader`, `SessionReader`
- Dedup: reconnect loses no durable event (seq contiguity verified)
- No goroutine leaks (stream close test)
- Embedded mode unchanged (existing tests pass)

---

## P2f — Integration tests + buf breaking CI

**Goal:** Verify P2 exit gates and set up CI.

### Integration tests
- `internal/server/connect/integration_test.go`:
  - **Two-client same-session:** create session, two clients subscribe, submit input, both reach same durable seq N, projections equal at N
  - **Reconnect:** client subscribes, disconnects (cancel stream), events emitted while disconnected, reconnect from lastSeq, no durable event lost (seq contiguity)
  - **Pause/backpressure:** server process receives SIGSTOP, events accumulate, SIGCONT, client catches up
  - **Transport reconnect:** forcibly terminate stream, resume by cursor
  - **Crash-recovery:** (if daemon restart is in P2 scope) stop daemon, restart with same store, sessions in `running` → `error` with `SessionError{daemon_restart}` (design §5.7)

### Exit gate tests (precise definitions)
- **"Two TUIs, identical state"** → both clients' durable projections at seq N are equal (compare event lists by seq, not raw streams — ephemeral events excluded)
- **"Reconnect loses no event"** → no durable event with seq ≤ N is missing after reconnect (seq contiguity check)
- **"buf breaking"** → `buf breaking --against .git#branch=main` exits 0

### Skew test
- P2 has only `v1alpha1` — no "old client" exists yet. Create a frozen client fixture (hardcoded proto messages at the initial field numbers). This becomes the "oldest supported client" baseline. Future P2.x additions test against this fixture.
- Bootstrap: `buf breaking` on first commit — `--against .git#branch=main` may fail if proto doesn't exist on main. Use `--against .git#branch=main#subdir=api/proto` with a bootstrap skip if the path doesn't exist on main yet.

### CI
- `.github/workflows/buf-breaking.yml`:
  - `buf lint`
  - `buf generate && git diff --exit-code` (codegen drift)
  - `buf breaking --against .git#branch=main` (with bootstrap handling)
- Pinned buf + plugin versions

### Exit criteria
- Two-client projection test: equal at seq N ✅
- Reconnect: no durable event lost ✅
- `buf lint` + `buf breaking` + codegen drift check pass ✅
- Full `go test -race ./...` green ✅
- `go build ./...` + `go vet ./...` ✅

---

## What's NOT in P2 (explicitly deferred)

- `CancelSession` (design doc §3.3 lists it separately from `CloseSession`) — `CloseSession` already initiates cancellation. If `CancelSession` is different, it's P3+.
- `Fork` (design doc §3.3) — deferred.
- `WorkspaceService`, `BackendService`, `AgentService`, `AuthService`, `TenantService`, `MemoryService`, `TelemetryService` — P4+.
- `SO_PEERCRED` local auth — P4.
- `proto-v1` durable store encoding — wire is proto, store stays `json-v1`.
- `request_id` idempotency enforcement — carried in proto, enforced in P3+.
- Crash-recovery (daemon restart) — may be tested if it falls out of P1's store + P2's daemon naturally, but not a P2 exit gate.
- Session picker UI — P3 (Web UI).
- Headless `wakil run` — falls out "almost gratis" after P2 per design doc §querschnitt, but is cross-cutting, not a P2 chunk.
