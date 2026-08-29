# wakild — Foundation-Plan

**Ziel:** Wakil vom Single-Process-TUI zu einem langlebigen Dienst (`wakild`) mit stabiler API, stabilem Datenmodell und mehreren Clients (TUI, Web-UI, CLI, CI) umbauen — mandantenfähig, ohne die heutige Einzelplatz-Erfahrung zu verschlechtern.

**Leitprinzip:** Die schwierigen Dinge (Identität von Objekten, Tenancy, Event-Ordnung, Concurrency) werden *früh* im Fundament verankert, auch wenn die Features, die sie brauchen (Auth, Web-UI, Sharing), erst spät kommen. Nachträglich `tenant_id` und eine Event-Sequenz in alle Pfade einziehen ist der teure Weg.

---

## 1. Zielbild

```
                 ┌──────────────────────────────────────────┐
   TUI ─────────▶│                                          │
   Web-UI ──────▶│   wakild (Host-Prozess, State-Owner)     │
   CLI/CI ──────▶│                                          │
                 │  ┌────────────────────────────────────┐  │
                 │  │ API-Layer (Connect/gRPC + Streams) │  │
                 │  ├────────────────────────────────────┤  │
                 │  │ Auth / Tenancy / Policy            │  │
                 │  ├────────────────────────────────────┤  │
                 │  │ Core: Sessions, Agents, Tools,     │  │
                 │  │       Subagents, Memory, Sidecar   │  │
                 │  ├────────────────────────────────────┤  │
                 │  │ Store: Control-Plane + Blob/Trace  │  │
                 │  └────────────────────────────────────┘  │
                 └───────────┬──────────────────────────────┘
                             │ Tool-Calls über Sandbox-Grenze
                             ▼
                    Sandbox-Runtime (Docker / eigene RT)
```

Unverändert gültig aus der heutigen Architektur:

- **State-Ownership bleibt beim Host-Prozess.** Der Daemon ist die Verkörperung dieses Prinzips — die Sandbox bleibt dumm, Clients werden dumm.
- **Tool-Calls kreuzen die Sandbox-Grenze**, nicht der Zustand.
- **Writer-Serialisierung** existiert bereits für edit-fähige Subagents; sie muss auf Session- und Tenant-Ebene gehoben werden.

---

## 2. Architekturschnitt

### 2.1 Paket-Layout

```
wakil/
  cmd/
    wakil/            # Client-Binary (TUI + CLI-Subcommands)
    wakild/           # Daemon-Binary
  api/
    proto/wakil/v1/   # .proto — die Vertragsfläche
    gen/              # generierter Code (buf)
  internal/
    core/             # Agent-Loop, Tools, Subagents — KEINE Transport-Abhängigkeit
      session/
      agent/
      tool/
      subagent/
      memory/
      sidecar/        # Guard + Helper + Telemetrie
    store/
      control/        # relationale Control-Plane
      blob/           # Traces, Artefakte, grosse Payloads
      migrate/
    server/           # API-Handler: Proto <-> core, sonst nichts
    auth/             # Identität, Token, Policy-Enforcement
    sandbox/          # Runtime-Abstraktion
    tui/              # Bubbletea-Client
  web/                # Web-UI (statisch, spricht dieselbe API)
```

**Harte Regel:** `internal/core` importiert weder `api/gen` noch `internal/server`. Der Core kennt Domain-Typen, keine Wire-Typen. Handler sind reine Übersetzer. Das ist die Bedingung dafür, dass die API später wechseln kann, ohne den Core anzufassen — und dafür, dass der Core im Test ohne Daemon läuft.

### 2.2 Betriebsmodi

| Modus | Transport | Auth | Zielgruppe |
|---|---|---|---|
| `embedded` | in-process | keine | heutige TUI, Tests, `wakil run` in CI |
| `local` | Unix-Socket `$XDG_RUNTIME_DIR/wakild.sock` | Peer-Credentials (UID) | Einzelplatz, Default |
| `hosted` | TCP + TLS | OIDC / API-Token, Pflicht | Team/Mandanten |

`embedded` ist wichtig: Der Umbau darf nicht dazu führen, dass ein einzelner Entwickler zwei Prozesse und einen Login braucht, um eine Datei zu editieren. Das TUI startet den Core in-process, *aber über dasselbe Interface*, das der Daemon exponiert. Ein Flag (`--daemon <addr>`) wechselt zum Remote-Modus.

---

## 3. API-Design

### 3.1 Transportwahl: Connect (connectrpc.com)

Empfehlung: **Connect-Go mit Protobuf-Schema**, nicht rohes gRPC, nicht handgeschriebenes REST.

Begründung:
- Ein Schema, drei Wire-Formate: gRPC, gRPC-Web, plain HTTP/JSON. Die Web-UI spricht direkt mit dem Daemon — **kein Envoy/grpc-gateway-Sidecar** nötig. Das ist der entscheidende Punkt gegenüber reinem gRPC.
- `curl`-bar (HTTP/JSON) → Debugging und CI-Skripte ohne Client-Bibliothek.
- Server-Streaming für Live-Turns, funktioniert über HTTP/1.1 (wichtig hinter Reverse-Proxies).
- `buf lint` + `buf breaking` in CI = maschinell durchgesetzte API-Stabilität. Genau das, was „stabile API" operativ bedeutet.

Verworfen: JSON-RPC über Unix-Socket (kein Schema, keine Breaking-Change-Erkennung, Web-UI braucht doch wieder eine eigene Schicht); reines REST (Streaming-Semantik wird handgestrickt).

### 3.2 Versionierung und Stabilitätsgarantie

- Package `wakil.v1alpha1` bis das Fundament steht, dann `wakil.v1`.
- `buf breaking --against .git#branch=main` als CI-Gate. Ab `v1`: keine Feldentfernung, keine Typänderung, keine Semantikänderung eines bestehenden Feldes. Nur additiv.
- Deprecation: Feld/RPC wird als `deprecated` markiert, bleibt mindestens zwei Minor-Releases funktionsfähig.
- Server meldet in `GetServerInfo` seine API-Version und die Menge unterstützter Capabilities. Clients degradieren, statt zu crashen.
- **Skew-Test in CI:** ältester unterstützter Client gegen aktuellen Server.

### 3.3 Service-Schnitt

```protobuf
service SystemService {
  rpc GetServerInfo(...) returns (ServerInfo);   // Version, Capabilities, Limits
  rpc Health(...) returns (HealthStatus);
}

service AuthService {
  rpc Login(...) returns (TokenPair);            // hosted-Modus
  rpc Refresh(...) returns (TokenPair);
  rpc WhoAmI(...) returns (Principal);
  rpc CreateAPIToken(...) returns (APIToken);
  rpc RevokeAPIToken(...) returns (...);
}

service TenantService {
  rpc CreateTenant / GetTenant / ListTenants / UpdateTenant
  rpc CreateUser / ListUsers / UpdateMembership / DeactivateUser
}

service WorkspaceService {                        // Projekt-Wurzeln = Working-Dirs
  rpc CreateWorkspace / GetWorkspace / ListWorkspaces / DeleteWorkspace
  rpc AcquireLease / ReleaseLease / ListLeases     // Working-Dir-Concurrency
}

service BackendService {                          // LLM-Backends + Credentials
  rpc CreateBackend / ListBackends / UpdateBackend / DeleteBackend
  rpc TestBackend(...) returns (BackendProbe);
  rpc ListModels(...) returns (ModelCatalog);
}

service AgentService {                            // Agent-Konfiguration aus dem UI
  rpc CreateAgent / GetAgent / ListAgents
  rpc CreateAgentRevision(...) returns (AgentRevision);   // immutable
  rpc ListAgentRevisions / DiffAgentRevisions
  rpc ValidateAgentRevision(...) returns (ValidationReport);
}

service SessionService {
  rpc CreateSession(...) returns (Session);
  rpc GetSession / ListSessions / CancelSession / DeleteSession
  rpc SubmitInput(...) returns (SubmitAck);       // nicht-blockierend
  rpc RespondToApproval(...) returns (...);       // Tool-Freigaben, "[declined by user]"
  rpc Interrupt(...) returns (...);
  rpc Fork(...) returns (Session);                // von Turn N abzweigen
}

service EventService {
  rpc StreamEvents(StreamEventsRequest) returns (stream Event);   // resume via cursor
  rpc ListEvents(...) returns (EventPage);                        // Replay/Historie
}

service MemoryService {
  rpc GetEntry / PutEntry / ListEntries / DeleteEntry
  rpc ListProposals / PromoteProposal / RejectProposal   // propose-then-promote
}

service TelemetryService {
  rpc StreamTelemetry(...) returns (stream TelemetryRecord);
  rpc QueryUsage(...) returns (UsageReport);      // pro User/Tenant/Backend
}
```

### 3.4 Das zentrale Muster: Command/Event-Trennung

Der wichtigste Design-Entscheid der ganzen Übung.

**Kein RPC blockiert auf einen Agent-Turn.** `SubmitInput` nimmt Input entgegen, ordnet ihn in die Session-Queue ein, gibt eine `turn_id` zurück und kehrt sofort zurück. Alles, was danach passiert — Token-Deltas, Tool-Call-Start, Tool-Ergebnis, Subagent-Dispatch, Approval-Anfrage, Fehler, Abschluss — kommt als **Event** über `StreamEvents`.

Daraus folgt:

- Ein Client-Reconnect verliert nichts. Client merkt sich `last_seq`, streamt ab `last_seq+1` weiter.
- Mehrere Clients können dieselbe Session gleichzeitig beobachten (TUI + Web-UI + Kollege) — fällt gratis ab.
- Der TUI-Renderer und der Web-Renderer konsumieren identische Events. Keine zwei Wahrheiten.
- Session-Replay = Events von 0 abspielen. Debugging, Demos, Regressionstests.
- Die 21.9 % `stream_error` aus der Sidecar-Baseline werden zu einem *Event*, nicht zu einem abgebrochenen RPC — der Client sieht, *wo* es riss.

**Event-Vertrag:**

```protobuf
message Event {
  string   tenant_id  = 1;
  string   session_id = 2;
  uint64   seq        = 3;   // lückenlos monoton, pro Session
  Timestamp ts        = 4;
  oneof payload {
    SessionCreated     session_created     = 10;
    TurnStarted        turn_started        = 11;
    MessageDelta       message_delta       = 12;   // Streaming-Text
    ReasoningDelta     reasoning_delta     = 13;
    ToolCallRequested  tool_call_requested = 14;
    ApprovalRequested  approval_requested  = 15;
    ApprovalResolved   approval_resolved   = 16;
    ToolCallCompleted  tool_call_completed = 17;
    SubagentSpawned    subagent_spawned    = 18;
    SubagentProgress   subagent_progress   = 19;
    SubagentCompleted  subagent_completed  = 20;
    MemoryProposed     memory_proposed     = 21;
    GuardTriggered     guard_triggered     = 22;   // Sidecar
    ContextWarning     context_warning     = 23;   // Sidecar, prädiktiv
    TurnCompleted      turn_completed      = 24;
    SessionError       session_error       = 25;
    SessionClosed      session_closed      = 26;
  }
}
```

`seq` ist die Wahrheit. Zeitstempel sind Metadaten, nie Ordnungskriterium.

---

## 4. Datenmodell

### 4.1 Identität

- **Typisierte, präfixierte IDs** als Strings: `tnt_`, `usr_`, `wsp_`, `agt_`, `rev_`, `ses_`, `trn_`, `tcl_`, `bke_`, `art_`, `tok_`.
- Body = **UUIDv7** (zeitsortierbar, index-freundlich, kein zentraler Zähler).
- Vorteil: Ein falsch übergebener Identifier fällt im Log sofort auf; kein `WHERE id = <uuid einer anderen Tabelle>`-Bug.

### 4.2 Objektmodell

```
Tenant
 ├─ User ──< Membership (role) >── Tenant
 ├─ Backend (LLM-Endpoint + Credential-Ref)
 ├─ Workspace (Projekt-Wurzel / Working-Dir + Sandbox-Profil)
 │   └─ Lease (exklusiver Schreibzugriff, TTL)
 ├─ Agent
 │   └─ AgentRevision (immutable: Prompt, Tools, Modell-Policy, Limits)
 ├─ Session (pinnt: workspace + agent_revision + backend)
 │   ├─ Turn
 │   │   └─ ToolCall
 │   ├─ Event (append-only, seq)
 │   ├─ Artifact (Diffs, Patches, Outputs)
 │   └─ Participant (welche User sehen/steuern)
 ├─ MemoryScope (repo/folder-gescoped)
 │   └─ MemoryEntry (+ Provenance, Write-Tier, File-Anchor)
 └─ UsageRecord (Tokens/Kosten pro User+Backend+Session)
```

### 4.3 Schema-Skizze (Control-Plane)

Jede Tabelle trägt `tenant_id`. Ohne Ausnahme, auch wenn es im Single-Tenant-Betrieb redundant wirkt.

```sql
CREATE TABLE tenants (
  id            TEXT PRIMARY KEY,
  slug          TEXT NOT NULL UNIQUE,
  display_name  TEXT NOT NULL,
  status        TEXT NOT NULL,          -- active | suspended
  created_at    INTEGER NOT NULL
);

CREATE TABLE users (
  id            TEXT PRIMARY KEY,
  email         TEXT NOT NULL,
  display_name  TEXT NOT NULL,
  auth_subject  TEXT,                   -- OIDC sub; NULL bei lokalen Accounts
  password_hash TEXT,                   -- argon2id; nur wenn kein IdP
  status        TEXT NOT NULL,
  created_at    INTEGER NOT NULL,
  UNIQUE(email)
);

CREATE TABLE memberships (
  tenant_id  TEXT NOT NULL REFERENCES tenants(id),
  user_id    TEXT NOT NULL REFERENCES users(id),
  role       TEXT NOT NULL,             -- owner | admin | member | viewer
  created_at INTEGER NOT NULL,
  PRIMARY KEY (tenant_id, user_id)
);

CREATE TABLE api_tokens (
  id           TEXT PRIMARY KEY,
  tenant_id    TEXT NOT NULL,
  user_id      TEXT NOT NULL,
  name         TEXT NOT NULL,
  token_hash   TEXT NOT NULL,           -- sha256 des Secrets, nie Klartext
  scopes       TEXT NOT NULL,           -- JSON-Array
  expires_at   INTEGER,
  last_used_at INTEGER,
  revoked_at   INTEGER
);

CREATE TABLE workspaces (
  id              TEXT PRIMARY KEY,
  tenant_id       TEXT NOT NULL,
  name            TEXT NOT NULL,
  host_path       TEXT NOT NULL,        -- Pfad-Parität innen/aussen
  sandbox_profile TEXT NOT NULL,        -- JSON: Image, Mounts, Limits, Netz
  vcs_remote      TEXT,
  created_at      INTEGER NOT NULL,
  UNIQUE(tenant_id, name)
);

CREATE TABLE workspace_leases (
  workspace_id TEXT NOT NULL REFERENCES workspaces(id),
  session_id   TEXT NOT NULL,
  mode         TEXT NOT NULL,           -- exclusive | shared_read
  acquired_at  INTEGER NOT NULL,
  expires_at   INTEGER NOT NULL,        -- Heartbeat verlängert
  PRIMARY KEY (workspace_id, session_id)
);

CREATE TABLE backends (
  id            TEXT PRIMARY KEY,
  tenant_id     TEXT NOT NULL,
  kind          TEXT NOT NULL,          -- openrouter | anthropic | llama_local | ...
  base_url      TEXT,
  credential_id TEXT,
  default_model TEXT,
  routing_rules TEXT,                   -- JSON, Multi-Backend-Routing
  enabled       INTEGER NOT NULL DEFAULT 1
);

CREATE TABLE credentials (
  id          TEXT PRIMARY KEY,
  tenant_id   TEXT NOT NULL,
  label       TEXT NOT NULL,
  ciphertext  BLOB NOT NULL,            -- envelope-encrypted
  key_id      TEXT NOT NULL,
  created_at  INTEGER NOT NULL
);

CREATE TABLE agents (
  id         TEXT PRIMARY KEY,
  tenant_id  TEXT NOT NULL,
  name       TEXT NOT NULL,
  head_rev   TEXT,                      -- aktuelle Revision
  UNIQUE(tenant_id, name)
);

CREATE TABLE agent_revisions (
  id           TEXT PRIMARY KEY,
  tenant_id    TEXT NOT NULL,
  agent_id     TEXT NOT NULL REFERENCES agents(id),
  rev_number   INTEGER NOT NULL,
  spec         TEXT NOT NULL,           -- JSON: Prompt, Tools, Limits, Subagent-Policy
  spec_hash    TEXT NOT NULL,           -- content-addressed
  created_by   TEXT NOT NULL,
  created_at   INTEGER NOT NULL,
  UNIQUE(agent_id, rev_number)
);

CREATE TABLE sessions (
  id                TEXT PRIMARY KEY,
  tenant_id         TEXT NOT NULL,
  workspace_id      TEXT NOT NULL,
  agent_revision_id TEXT NOT NULL,      -- gepinnt, nie "aktuelle Revision"
  backend_id        TEXT NOT NULL,
  created_by        TEXT NOT NULL,
  parent_session_id TEXT,               -- Fork
  title             TEXT,
  state             TEXT NOT NULL,      -- idle | running | awaiting_approval | error | closed
  last_seq          INTEGER NOT NULL DEFAULT 0,
  created_at        INTEGER NOT NULL,
  closed_at         INTEGER
);

CREATE TABLE turns (
  id               TEXT PRIMARY KEY,
  tenant_id        TEXT NOT NULL,
  session_id       TEXT NOT NULL REFERENCES sessions(id),
  turn_index       INTEGER NOT NULL,
  turn_type        TEXT NOT NULL,       -- tool_loop | final
  outcome          TEXT,                -- complete | stream_error | empty | cancelled
  backend_id       TEXT NOT NULL,       -- IMMER gesetzt (Baseline-Lücke)
  model            TEXT NOT NULL,
  context_limit    INTEGER NOT NULL,    -- pro Turn mitgeloggt (Baseline-Lücke)
  input_tokens     INTEGER,
  output_tokens    INTEGER,
  reasoning_tokens INTEGER,             -- vereinheitlicht (Baseline-Lücke)
  sft_eligible     INTEGER NOT NULL DEFAULT 0,
  started_at       INTEGER NOT NULL,
  ended_at         INTEGER,
  UNIQUE(session_id, turn_index)
);

CREATE TABLE tool_calls (
  id             TEXT PRIMARY KEY,
  tenant_id      TEXT NOT NULL,
  turn_id        TEXT NOT NULL REFERENCES turns(id),
  name           TEXT NOT NULL,
  arg_digest     TEXT NOT NULL,         -- Hash statt Payload (Loop-Signatur!)
  pre_cap_bytes  INTEGER,
  post_cap_bytes INTEGER,
  capped         INTEGER NOT NULL DEFAULT 0,
  approval       TEXT,                  -- auto | granted | declined
  status         TEXT NOT NULL,
  subagent_id    TEXT,
  duration_ms    INTEGER
);

CREATE TABLE events (
  tenant_id  TEXT NOT NULL,
  session_id TEXT NOT NULL,
  seq        INTEGER NOT NULL,
  ts         INTEGER NOT NULL,
  kind       TEXT NOT NULL,
  payload    BLOB NOT NULL,             -- proto-serialisiert
  PRIMARY KEY (session_id, seq)
);

CREATE TABLE usage_records (
  id          TEXT PRIMARY KEY,
  tenant_id   TEXT NOT NULL,
  user_id     TEXT NOT NULL,
  session_id  TEXT NOT NULL,
  backend_id  TEXT NOT NULL,
  model       TEXT NOT NULL,
  window_start INTEGER NOT NULL,
  input_tokens INTEGER NOT NULL,
  output_tokens INTEGER NOT NULL,
  cost_micros INTEGER
);
```

Memory-Tabellen (`memory_scopes`, `memory_entries`, `memory_proposals`) übernehmen das bereits entworfene Schema — Provenance-Tag, Write-Tier, File-Anchor, Staleness — plus `tenant_id` und `scope_key`.

### 4.4 Storage-Split: SQLite + kvr

Klare Empfehlung: **nicht** alles auf kvr, aber auch nicht alles auf SQLite.

| Ebene | Technologie | Warum |
|---|---|---|
| Control-Plane (obige Tabellen) | SQLite (WAL) | Transaktionen über mehrere Entitäten, Fremdschlüssel, Ad-hoc-Queries für UI-Filter und Usage-Reports. Genau das, wofür ein relationales Modell existiert. |
| Event-Log | SQLite-Tabelle, Blob-Payload | Braucht strikte Ordnung + Range-Scan `WHERE session_id=? AND seq>?`. Ein B-Tree-Index ist dafür ideal. |
| Traces, Artefakte, grosse Tool-Outputs | kvr | Append-heavy, gross, keine Query-Anforderung ausser Key-Lookup. Hier gewinnt kvr, und hier ist es der ehrliche Testfall für kvr in Produktion. |

Der Store wird hinter einem Interface gekapselt (`store.ControlStore`, `store.BlobStore`), sodass ein späterer Wechsel — SQLite → Postgres für echtes Multi-Node, oder Control-Plane → kvr — eine Implementierung ist, kein Refactor.

**Migrationen:** nummerierte, vorwärtsgerichtete SQL-Dateien, eingebettet via `embed`, beim Daemon-Start angewendet, Version in `schema_migrations`. Daemon verweigert den Start bei einem Schema, das neuer ist als das Binary.

---

## 5. Concurrency und Session-Ownership

Der heikelste Teil beim Übergang zu mehreren Clients und Usern.

1. **Eine Session hat genau einen Executor** im Daemon (eine Goroutine mit eigener Queue). Alle Eingaben werden in diese Queue serialisiert. Damit gibt es kein Locking innerhalb der Session — dasselbe Muster wie heute, nur explizit.
2. **Working-Dir-Leases**: Eine Session, die schreiben darf, hält ein `exclusive`-Lease auf dem Workspace, per Heartbeat verlängert, mit TTL gegen tote Sessions. Andere Sessions bekommen `shared_read` oder eine klare Fehlermeldung. Ohne das führt Multi-User auf demselben Repo direkt zu korrupten Arbeitsständen.
3. **Der bestehende Writer-Mutex für Subagents** bleibt, wird aber zum *inneren* Lock unterhalb des Lease.
4. **Sandbox-Zuordnung**: eine Sandbox-Instanz pro aktiver Session (nicht pro Workspace, nicht pro Tenant geteilt). Pfad-Parität-Entscheid bleibt.
5. **Cancel/Interrupt** muss über den Context bis in den Tool-Call durchschlagen und ein `SessionClosed`- bzw. `TurnCompleted{cancelled}`-Event erzeugen — nie stiller Abbruch.
6. **Graceful Shutdown**: Daemon stoppt Annahme neuer Inputs, lässt laufende Turns auslaufen (Deadline), persistiert `last_seq`, gibt Leases frei.
7. **Crash-Recovery**: Beim Start Sessions in `running` auf `error` setzen und ein `SessionError{reason: daemon_restart}`-Event anhängen. Nie so tun, als wäre nichts passiert.

---

## 6. Auth, Tenancy, Policy

### 6.1 Prinzipal-Modell

Jeder Request wird zu einem `Principal{tenant_id, user_id, role, scopes, auth_method}` aufgelöst — auch im lokalen Modus (dort: Default-Tenant, Default-User, `owner`). **Der Core sieht nie einen anonymen Aufruf.** Damit ist der Multi-Tenant-Pfad von Tag eins derselbe Codepfad wie der Einzelplatz-Pfad, nur mit anderer Auflösung.

### 6.2 Authentifizierung

- **local**: Unix-Socket-Peer-Credentials (`SO_PEERCRED`). UID muss dem Daemon-User entsprechen. Kein Passwort.
- **hosted**: OIDC gegen einen selbst gehosteten IdP (Zitadel/Authentik/Keycloak). Der Daemon validiert JWTs und mappt `sub` auf `users.auth_subject`.
- **Fallback lokale Accounts**: `password_hash` mit argon2id, Login-Rate-Limiting, Session-Cookie für die Web-UI (HttpOnly, SameSite=Strict, kurzlebig + Refresh). **Nur** als Option für Setups ohne IdP — nicht der Hauptpfad.
- **Maschinen/CI**: API-Tokens, gescopet, ablaufend, gehasht gespeichert, mit `last_used_at` für Audit.

Begründung für OIDC-first: Passwort-Reset, MFA, Brute-Force-Schutz und Session-Invalidierung sind vier Features, die du sonst selbst bauen, testen und pflegen musst. Sie liegen genau auf der Angriffsfläche, die dein „100 % safe vor Promotion"-Ziel bedroht.

### 6.3 Autorisierung

Rollen klein halten:

| Rolle | Darf |
|---|---|
| `owner` | alles inkl. Tenant löschen, Billing |
| `admin` | User verwalten, Backends/Credentials, Agents, Workspaces |
| `member` | Sessions erstellen/fahren, eigene Sessions löschen, Memory schreiben (gated) |
| `viewer` | Sessions und Traces lesen, nichts starten |

Durchsetzung an **einer** Stelle: einem Repository-Layer, der `tenant_id` aus dem Principal nimmt und in jede Query einsetzt. Nicht in Handlern, nicht im Core. Eine vergessene Prüfung in einem Handler ist eine Cross-Tenant-Leckage; ein Repository, das ohne Tenant-Kontext gar nicht instanziierbar ist, macht diesen Fehler strukturell unmöglich.

Zusätzlich: Ein Test, der für jede Tabelle prüft, dass keine Query ohne `tenant_id`-Prädikat existiert (statische Analyse oder Query-Hook im Testmodus).

### 6.4 Secrets

Backend-Keys nie im Klartext in der DB: Envelope-Encryption mit einem Master-Key aus Umgebung/File/KMS, `key_id` mitgespeichert für Rotation. Die API gibt Credentials **nie** zurück — nur `id`, `label`, `last_four`, `created_at`.

### 6.5 Datenschutz-Konsequenz der Mandantenfähigkeit

Traces enthalten Code, Pfade und potenziell Secrets. Das bereits geplante Secret-/PII-Scrubbing ist damit nicht mehr nur ein Schritt vor SFT-Nutzung, sondern eine Anforderung an jeden Pfad, der Trace-Daten über eine Tenant-Grenze bewegt (Export, Support-Zugriff, geteilte Sessions). Praktisch: Scrubbing beim *Schreiben* in den Blob-Store, mit dem unscrubbed Original entweder gar nicht persistiert oder in einem separaten, nicht-API-exponierten Pfad.

---

## 7. Konfiguration

Zwei getrennte Ebenen, die heute oft vermischt werden:

**Daemon-Konfiguration** (Datei/Env, Neustart nötig): Listen-Adressen, TLS, DB-Pfade, Master-Key, Sandbox-Runtime, IdP-Endpunkt, globale Limits.

**Domänen-Konfiguration** (in der DB, über die API änderbar, versioniert): Agents, Backends, Routing-Regeln, Workspaces, Tool-Erlaubnislisten, Approval-Policies, Quotas.

Alles, was ein User über das UI ändern soll, gehört in die zweite Kategorie. **Agent-Revisionen sind immutable und content-addressed**; eine Session pinnt eine Revision-ID. Damit ist jede Session exakt reproduzierbar und eine Konfigänderung kann laufende Arbeit nicht unter dem Fuss wegziehen. Für Benchmark-Läufe (Terminal-Bench/SWE-bench) ist das die Voraussetzung dafür, dass Zahlen überhaupt vergleichbar sind.

`ValidateAgentRevision` prüft vor dem Speichern: existieren die referenzierten Tools, ist das Modell beim Backend verfügbar, sind die Limits konsistent. Fehlkonfiguration soll beim Speichern auffallen, nicht im dritten Turn.

---

## 8. Telemetrie, Observability, Sidecar

Der Daemon ist der natürliche Ort für den geplanten Sidecar (Guard + Helper + Telemetrie) — er sieht alle Sessions, nicht nur eine.

Aus der Baseline-Studie direkt ins Fundament zu ziehen:

- `backend` **immer** setzen (heute nur auf ~35 % der Turns, nie im Store-Header) → im Schema oben NOT NULL.
- `reasoning_tokens` vs. `reasoning_chars` vereinheitlichen → ein Feld, Einheit dokumentiert, Backend-Adapter normalisieren.
- `context_limit` pro Turn mitloggen → sonst ist der prädiktive Kontext-Warner nicht baubar (die p99-Schwelle von 816k wäre zu spät; der Warner braucht Slope + Limit).
- `sft_eligible`-Setter-Logik lokalisieren und ins Schema hängen (war extern im MCP-Agent-Framework, im Report als `[unverified]`).
- Offene Baseline-Frage „536 Headers vs. 425 Sessions vs. 593 Files" löst sich im Daemon-Modell strukturell: Sessions sind Datenbankzeilen mit ID, keine Dateien mit Rotation.

Guard-Events (`GuardTriggered`, `ContextWarning`) laufen über denselben Event-Stream wie alles andere. Damit sieht sie das UI ohne Sonderpfad.

Ergänzend: strukturierte Logs (slog, JSON), `/metrics` (Prometheus) für Session-Counts, Turn-Latenzen, Tool-Fehlerraten, Token-Durchsatz. OpenTelemetry-Traces optional, aber Span-IDs von Anfang an in Events mitführen.

---

## 9. Umsetzung in Phasen

Jede Phase hat ein Exit-Kriterium. Keine Phase beginnt, bevor die vorige ihres erfüllt.

### P0 — Core-Extraktion (Voraussetzung für alles)
Agent-Loop, Tools, Subagents, Memory aus dem TUI herauslösen in `internal/core` mit einem expliziten `Service`-Interface. TUI wird zum ersten Konsumenten dieses Interface, in-process.

*Exit:* `internal/core` hat keine Bubbletea-Abhängigkeit; ein headless Testprogramm fährt eine vollständige Session ohne TUI.

### P1 — Datenmodell und Store
Schema oben implementieren, Migrationen, Repository-Layer mit erzwungenem Tenant-Kontext, typisierte IDs, Default-Tenant/Default-User im lokalen Modus. Event-Log einführen und den Core auf Event-Emission umstellen. Telemetrie-Schema-Fixes aus §8.

*Exit:* Eine Session ist vollständig aus dem Event-Log rekonstruierbar; ein Replay erzeugt dieselbe TUI-Darstellung wie der Live-Lauf. Cross-Tenant-Query-Test grün.

### P2 — Daemon und API
`api/proto`, Connect-Server, `wakild`-Binary, Unix-Socket, `wakil --daemon`. TUI spricht Remote. `embedded`-Modus bleibt als Default erhalten.

*Exit:* Zwei TUI-Instanzen hängen an derselben Session und zeigen identischen Zustand; Reconnect nach `kill -STOP`/`CONT` verliert kein Event. `buf breaking` in CI aktiv.

### P3 — Read-only Web-UI
Statisches Frontend gegen dieselbe API: Session-Liste, Live-Session-Viewer, Trace-Browser, Telemetrie-Dashboard. Kein Write-Path.

*Exit:* Eine laufende Session ist im Browser live verfolgbar, inklusive Tool-Calls und Subagent-Baum.

### P4 — Auth und Mandantenfähigkeit
OIDC-Integration, lokale Accounts als Fallback, API-Tokens, Rollen, TLS, `hosted`-Modus. Credential-Verschlüsselung. Scrubbing im Blob-Schreibpfad.

*Exit:* Zwei Mandanten auf einem Daemon; ein Penetrationstest-Skript versucht systematisch Cross-Tenant-Zugriff über jeden RPC und schlägt überall fehl.

### P5 — Write-Path im UI
Sessions aus dem UI starten, Approvals im UI beantworten, Agents und Backends im UI konfigurieren, Workspace-Verwaltung, Usage-Ansicht.

*Exit:* Ein neuer User kann ohne Terminal von Login bis abgeschlossener Session arbeiten.

### Querschnitt, parallel zu allen Phasen
- Supply-Chain: `go mod verify`, Dependency-Pinning, SBOM, `govulncheck` in CI, reproduzierbare Builds, signierte Releases. Das ist die Voraussetzung für die geplante Promotion.
- Headless-Runner (`wakil run --agent X --workspace Y --task-file Z`) fällt nach P2 fast gratis ab und ist die Grundlage für Terminal-Bench/SWE-bench-Läufe.

---

## 10. Risiken und offene Entscheidungen

| Punkt | Risiko | Vorschlag |
|---|---|---|
| P0 zieht sich | Core-Extraktion aus gewachsenem TUI-Code ist die undankbarste Phase, ohne sichtbares Ergebnis | Nicht überspringen. Jede Abkürzung hier wird in P2 doppelt bezahlt. Zeitbox setzen, aber nicht auslassen. |
| Event-Log-Grösse | `MessageDelta` pro Token bläht das Log auf | Deltas nur im Live-Stream, im persistierten Log koaleszierte Message-Blöcke; Flag `persist_deltas` für Debug-Sessions. |
| Working-Dir-Sharing | Zwei User, ein Repo, gleichzeitig | Leases (§5). Alternativ session-exklusive Checkouts (git worktree pro Session) — sauberer, aber teurer bei grossen Repos. Entscheidung vor P4 fällig. |
| Sandbox-Dichte | Eine Sandbox pro Session skaliert bei vielen Usern schlecht | Erst messen, dann optimieren. Idle-Sandboxes nach TTL einfrieren/stoppen. |
| SQLite bei vielen Writern | Ein Daemon-Prozess = ein Writer, unkritisch. Zweiter Daemon-Node = Problem | Multi-Node explizit als Nicht-Ziel für v1 deklarieren. Store-Interface hält die Postgres-Tür offen. |
| Eigene Container-Runtime | Grosses Nebenprojekt | Hinter `internal/sandbox`-Interface halten, aber nicht in den kritischen Pfad dieses Plans legen. |
| kvr in Produktion | Neuer Store unter neuem Dienst = zwei gleichzeitige Unbekannte | Deshalb der Split in §4.4: kvr trägt zuerst nur Blobs, wo ein Fehler wiederherstellbar ist. |
| Passwort-Login | Selbst gebaute Auth ist die häufigste Quelle ernster Lücken | OIDC-first (§6.2). Lokale Accounts nur als dokumentierte Fallback-Option. |

---

## 11. Erste konkrete Schritte

1. `api/proto/wakil/v1alpha1/` anlegen und **zuerst die Event-Message** schreiben — sie zwingt die Frage, welchen Zustand der Core eigentlich nach aussen kennt.
2. Inventar: welche Zustände hält das TUI heute lokal, die eigentlich Session-Zustand sind? Diese Liste ist die Arbeitsliste von P0.
3. Schema aus §4.3 als erste Migration, gegen die bestehenden 593 Trace-Files als Import-Testfall laufen lassen — ein Importer, der Altdaten ins neue Modell hebt, validiert das Modell härter als jedes Whiteboard.
4. Die drei Telemetrie-Schema-Fixes (§8) sofort in Wakil einbauen — sie sind billig, unabhängig vom Rest, und jede Session ab heute erzeugt sonst weiter unvollständige Daten.
