# Card #148 — P5: Write-Path in UI

**Goal (design doc line 558–561):** Sessions aus dem UI starten, Approvals im UI beantworten, Agents und Backends im UI konfigurieren, Workspace-Verwaltung, Usage-Ansicht.

**Exit criterion:** Ein neuer User kann ohne Terminal von Login bis abgeschlossener Session arbeiten.

Branch: `feature/wakild-daemon`
Design doc: `docs/design/wakild-foundation.md`

---

## Current state (verified)

### RPCs already implemented
- **SessionService** (8 RPCs): CreateSession, GetSession, ListSessions, DeleteSession, SubmitInput, RespondToApproval, Interrupt, CloseSession — all fully implemented in `session_handler.go`
- **BackendService** (4 RPCs): CreateBackend, ListBackends, UpdateBackend, DeleteBackend — fully implemented in `backend_handler.go`
- **AuthService** (11 RPCs): CreateJoinToken, ListJoinTokens, RevokeJoinToken, ExchangeJoinToken, WhoAmI, Logout, CreateAPIToken, ListAPITokens, RevokeAPIToken, GetOIDCAuthURL, ExchangeOIDCCode — fully implemented in `auth_handler.go`
- **EventService** (3 RPCs): StreamEvents, ListEvents, GetSessionSnapshot — fully implemented in `event_handler.go`
- **SystemService** (2 RPCs): GetServerInfo, Health

### Missing services (no proto/handler/store)
- WorkspaceService — needed for workspace management from UI
- AgentService — needed for agent configuration from UI
- TenantService, MemoryService, TelemetryService — deferred (not blocking P5 exit criterion)

### Web UI (P3, read-only)
- `web/index.html`, `web/app.js`, `web/styles.css` — vanilla JS, no framework
- Two tabs: Sessions list + Live Viewer (500ms polling)
- No write-path, no login, no auth handling
- `rpc()` helper uses fetch without credentials (won't send cookies)

---

## P5 Chunks

### P5a — Web UI auth + session write-path
The core login-to-session flow. The RPCs exist; the UI must call them.

- Login page: join token input → ExchangeJoinToken → cookie set → redirect to dashboard
- WhoAmI on load: if unauthenticated, show login; if authenticated, show dashboard
- Logout button
- `rpc()` helper: add `credentials: 'same-origin'` for cookie support
- Create session form: workspace path + title → CreateSession
- Session view: input box + submit → SubmitInput
- Approval buttons: allow once / allow reads once / deny → RespondToApproval
- Interrupt + Close session buttons
- Delete session button

### P5b — WorkspaceService backend
Proto + migration `005_workspaces.sql` + store + handler.

Proto: `workspace.proto` + `workspace_service.proto`
- CreateWorkspace, ListWorkspaces, GetWorkspace, DeleteWorkspace
- Fields: id, tenant_id, name, host_path, sandbox_profile (JSON), vcs_remote, created_at

Migration: `workspaces` table per design §4.3 (with `tenant_id`, `UNIQUE(tenant_id, name)`).

Store: `internal/store/workspacestore/` — tenant-scoped CRUD (same pattern as backendstore).

Handler: `workspace_handler.go` — owner/admin role-gated, tenant-scoped.

Wire into `Server.Handler()` and `NewServerWith*` constructors.

### P5c — AgentService backend
Proto + migration `006_agents.sql` + store + handler.

Proto: `agent.proto` + `agent_service.proto`
- CreateAgent, GetAgent, ListAgents, CreateAgentRevision, ListAgentRevisions
- Agent: id, tenant_id, name, head_rev
- AgentRevision: id, tenant_id, agent_id, rev_number, spec (JSON), spec_hash, created_by, created_at

Migration: `agents` + `agent_revisions` tables per design §4.3.

Store: `internal/store/agentstore/` — tenant-scoped CRUD.

Handler: `agent_handler.go` — owner/admin role-gated.

### P5d — Web UI management pages
- Backends tab: list, create (label, type, base_url, api_key), update, delete
- Workspaces tab: list, create (name, host_path), delete
- Agents tab: list, create (name), view revisions

### P5e — E2E verification
- `go build ./...`, `go vet ./...`, `go test -race ./...`
- `buf lint`, `buf breaking`
- `gofmt`
- Manual: start daemon, open web UI, login with join token, create session, submit input, see events, respond to approval, close session — all from browser

---

## Implementation order
P5a → P5b → P5c → P5d → P5e

P5a is the critical path (exit criterion). P5b/c are needed for full "workspace/agent config from UI". P5d wires the new services into the UI. P5e is the gate.
