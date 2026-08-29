# Card #148 — P1 Chunk Plan v2: SQLite Event Log + Store-Backed Sequencer

**Branch:** `feature/wakild-daemon` (continues from P0 @ `7eba60b`)
**Phase:** P1 — Datenmodell + Store
**Date:** 2026-08-20
**Mashūra review:** op-40 (3 panels) → v1 revised → op-41 (3 panels) → v2.1 amendments folded.

## Goal

Swap the P0 in-memory `MemLog` for a SQLite-backed `Store` implementation. The
`exit_gate_test.go` tests (Gate #9: replay) must pass unchanged against `MemLog`.
A **shared store-contract harness** runs the same assertions against `SQLiteStore`
(including a reopen/durability test). Add migrations, tenant-keyed session rows,
and a cross-tenant isolation test at the host layer.

## Decisions governing this work (from impl-plan + design doc)

- **D3:** Sequencer is store-pluggable. P1: `Append` + `sessions.last_seq` become
  one SQLite transaction. Producers never assign seq.
- **D2:** Durable events persisted + sequenced; ephemeral never persisted.
- **D9:** Replay = projection reconstruction (option 2), not full event sourcing.
- **D4:** Embedded mode resolves `Principal{tenant: "tnt_local", user: "usr_local", role: "owner"}`.
- **Schema §4.3:** `events` PK = `(session_id, seq)`. `tenant_id` on every table.
- **Migration approach:** numbered forward-only SQL files, embedded via `embed`,
  applied at startup, version in `schema_migrations`.
- **Storage split §4.4:** SQLite (WAL) for control-plane + event log.

## v2 design changes (addressing op-40 + op-41 findings)

### 1. No `EnsureSession` — atomic session creation in `Append` (all 3 panels)

**Problem (v1):** `EnsureSession` split session-row creation from the first event
append across two operations, creating orphan-row failure states. It also
broadened the `Store` interface unnecessarily.

**v2 design:** `SQLiteStore.Append` detects `KindSessionCreated` and inserts the
session row + the event in **one transaction**. For all other kinds, the session
row must already exist (enforced by FK). **No interface change to `Store`** —
`MemLog` is unchanged (it auto-creates per-session logs on first append, same as
P0). The session row is derived from the event draft + `SessionCreated` payload:
- `id` = `Event.SessionID`
- `tenant_id` = `Event.TenantID`
- `workspace` = `SessionCreated.WorkspaceID`
- `created_by` = `SessionCreated.CreatedBy`
- `created_at` = `Event.Ts.UnixNano()` (nanoseconds since epoch, UTC)
- `last_seq` = 1 (assigned in same transaction)
- `title` = NULL (not in the payload; session listing is P2/P3)

**`RecoverRunning` compatibility:** recovered sessions already have rows in
SQLite from before the crash. `RecoverRunning` emits `KindSessionError` (not
`KindSessionCreated`), so the FK is already satisfied. The `SessionError` append
increments `last_seq` and inserts the event after the pre-crash history — correct
behavior.

**Append to unknown session (non-SessionCreated):** FK violation → error. This
matches the intent: only `SessionCreated` creates a session; all other events
require an existing session. `MemLog` auto-creates per-session logs (P0 behavior)
— this divergence is acceptable because `MemLog` is the test stub, not the
production store. The shared contract harness tests will call `EnsureSessionRow`
(setup) before testing non-SessionCreated appends on SQLite; on MemLog, the first
append auto-creates the log.

Wait — that would make the contract harness store-specific. Better: the harness
creates a session by appending `KindSessionCreated` first (same on both stores),
then appends subsequent events. `MemLog` auto-creates on first append (any kind);
`SQLiteStore` creates the row only on `KindSessionCreated`. The harness must
always append `SessionCreated` first — this is what the host does in practice.

### 2. Schema: drop `state`/`closed_at`, add FK, add `payload_encoding` (all 3 panels)

**v1 problem:** `state`/`closed_at` columns had no P1 writer — dead columns. No
FK in the DDL despite claiming it. No payload version discriminator.

**v2 schema (P1 subset only):**
```sql
CREATE TABLE sessions (
  id         TEXT PRIMARY KEY,
  tenant_id  TEXT NOT NULL,
  workspace  TEXT NOT NULL,
  created_by TEXT NOT NULL,
  created_at INTEGER NOT NULL,   -- Unix nanoseconds (time.Time.UnixNano())
  last_seq   INTEGER NOT NULL DEFAULT 0,
  CHECK (last_seq >= 0)
);

CREATE TABLE events (
  tenant_id   TEXT NOT NULL,
  session_id TEXT NOT NULL,
  seq        INTEGER NOT NULL,
  ts         INTEGER NOT NULL,     -- Unix nanoseconds
  kind       TEXT NOT NULL,
  payload    BLOB NOT NULL,
  encoding   TEXT NOT NULL DEFAULT 'json-v1',
  PRIMARY KEY (session_id, seq),
  FOREIGN KEY (session_id) REFERENCES sessions(id),
  CHECK (seq > 0)
);
```

**No `state`/`closed_at`/`title` in P1** — add them in P2 when session-listing
and state-transition persistence arrive. The P1 sessions table stores only what
the event-log functionality needs: the FK target and `last_seq`.

**`encoding` column** on events: `'json-v1'` for P1. When P2 introduces proto,
new rows get `'proto-v1'`; the reader checks the encoding and dispatches. Old
JSON rows remain readable.

**Tenant integrity:** `events.tenant_id` is stored but `EventLog.Read` queries
by `session_id` only (globally unique). The composite FK
`FOREIGN KEY (tenant_id, session_id) REFERENCES sessions(tenant_id, id)` ensures
`events.tenant_id` matches `sessions.tenant_id`. Additionally, `Append` binds
`tenant_id` from the event draft and the INSERT fails if it doesn't match the
session row's tenant (the composite FK enforces this).

Wait — `sessions.id` is PK (just `id`). For a composite FK, I need a UNIQUE
constraint on `(tenant_id, id)`. Let me add `UNIQUE(tenant_id, id)` to sessions
and use the composite FK on events:
```sql
CREATE TABLE sessions (
  id         TEXT PRIMARY KEY,
  tenant_id  TEXT NOT NULL,
  workspace  TEXT NOT NULL,
  created_by TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  last_seq   INTEGER NOT NULL DEFAULT 0,
  CHECK (last_seq >= 0),
  UNIQUE (tenant_id, id)
);

CREATE TABLE events (
  tenant_id   TEXT NOT NULL,
  session_id TEXT NOT NULL,
  seq        INTEGER NOT NULL,
  ts         INTEGER NOT NULL,
  kind       TEXT NOT NULL,
  payload    BLOB NOT NULL,
  encoding   TEXT NOT NULL DEFAULT 'json-v1',
  PRIMARY KEY (session_id, seq),
  FOREIGN KEY (tenant_id, session_id) REFERENCES sessions(tenant_id, id),
  CHECK (seq > 0)
);
```

### 3. JSON codec with version discriminator (all 3 panels)

**API:**
```go
func MarshalPayload(kind Kind, payload any) ([]byte, error)
func UnmarshalPayload(kind Kind, data []byte) (any, error)
```

- `MarshalPayload`: validates `payload` type matches `kind` via `payloadTypes`
  registry, then `json.Marshal`. No `MarshalBinary` on `Event` (dropped from v1).
- `UnmarshalPayload`: looks up `payloadTypes[kind]`, `reflect.New(t).Interface()`,
  `json.Unmarshal` into the typed pointer, runs `Validate()` on the result.
  Returns error for unknown kind, malformed JSON, or validation failure.
- Unknown JSON fields are tolerated (forward-compatible additive fields).
- `NaN`/`Inf` in `float64` fields: `encoding/json` returns an error — test this.
- `nil` vs empty slices: JSON round-trip converts `nil` → `null` → `nil` (not
  empty). `reflect.DeepEqual` treats these differently. The codec tests use
  field-by-field comparison, not `reflect.DeepEqual` on the whole struct. For
  slice fields, compare `len()` and elements, treating `nil` and `[]T{}` as
  equivalent (the payloads use `[]string` for display fields — semantically
  equivalent).

### 4. Transaction mechanism (gpt-5.6, claude-fable)

**v1 problem:** `BEGIN IMMEDIATE` inside `sql.Tx` is invalid with `database/sql`.

**v2 design:** Follow the existing `internal/memory/store.go` pattern:
- `SetMaxOpenConns(1)` — single connection, no pool contention.
- `db.BeginTx(ctx, nil)` — starts a deferred transaction (sufficient with single
  connection; the single connection serializes all access).
- Inside the tx: `INSERT`/`UPDATE`/`SELECT` via `tx.ExecContext`/`tx.QueryRowContext`.
- `defer tx.Rollback()` on every error path; `tx.Commit()` at the end.
- `RowsAffected` check on `UPDATE` to detect missing session row.

**Append transaction (non-SessionCreated):**
```sql
UPDATE sessions SET last_seq = last_seq + 1 WHERE id = ?;
-- check RowsAffected == 1, else error (session not found)
SELECT last_seq FROM sessions WHERE id = ?;
INSERT INTO events (tenant_id, session_id, seq, ts, kind, payload, encoding)
  VALUES (?, ?, ?, ?, ?, ?, 'json-v1');
```

**Append transaction (SessionCreated — atomic session + event):**
```sql
INSERT INTO sessions (id, tenant_id, workspace, created_by, created_at, last_seq)
  VALUES (?, ?, ?, ?, ?, 1);
INSERT INTO events (tenant_id, session_id, seq, ts, kind, payload, encoding)
  VALUES (?, ?, 1, ?, ?, ?, 'json-v1');
```
Both in one tx; if either fails, rollback. Returns the committed event with
`Seq=1`.

**Pragmas (per connection — `SetMaxOpenConns(1)` ensures one):**
```sql
PRAGMA journal_mode = WAL;
PRAGMA busy_timeout = 5000;
PRAGMA foreign_keys = ON;
```

### 5. `Read` limit semantics (all 3 panels)

**v2:** When `limit <= 0`, omit the `LIMIT` clause entirely (return all events
with `seq > after`). When `limit > 0`, bind `LIMIT ?`. The SQL is built with a
format string (safe — `limit` is an int, not user-supplied text).

### 6. Timestamp storage (claude-fable, glm)

**v2:** `ts` stored as `INTEGER` = `Event.Ts.UnixNano()` (nanoseconds since Unix
epoch, UTC). Round-trip: `time.Unix(0, nanos)`. Tests compare timestamps with
`time.Equal` (not `reflect.DeepEqual`, which fails on the monotonic clock
component). The `Ts` field is producer-assigned and preserved (per the
`EventAppender` contract).

### 7. Chunk ordering (all 3 panels)

**v2 ordering:**
1. **P1a** — payload codec + compatibility policy
2. **P1b** — migrations + schema (P1a is not strictly required, but the codec
   types are referenced by P1c tests)
3. **P1c** — SQLite store implementation (depends on P1a codec + P1b migrations)
4. **P1d** — shared store-contract harness + replay + reopen/durability test
   (depends on P1c)
5. **P1e** — cross-tenant isolation test at host layer (depends on nothing new —
   can run against MemLog; verifies P0 authz is correct)

## Chunks

### P1a — Event payload codec

**Files:**
- `internal/core/event/codec.go` (new): `MarshalPayload(kind, payload)` and
  `UnmarshalPayload(kind, data)` functions.
- `internal/core/event/codec_test.go` (new): round-trip tests for all registered
  kinds; NaN/Inf rejection test; unknown kind error; validation failure error;
  nil-vs-empty slice handling.

**Exit:** all durable event kinds round-trip through
`MarshalPayload→UnmarshalPayload` with field equality (using `time.Equal` for
timestamps, len+elements for slices).

### P1b — Migrations + schema

**Files:**
- `internal/store/migrations/001_init.sql` (new): `schema_migrations` bootstrap
  + `sessions` + `events` tables (v2 schema above).
- `internal/store/migrations/migrate.go` (new): `Apply(ctx, *sql.DB) error` —
  applies pending migrations. Each migration runs in its own transaction. Version
  tracked in `schema_migrations`. Duplicate version numbers are rejected (UNIQUE
  on `version`). `schema_migrations` created before the first migration in the
  same transaction.
- `internal/store/migrations/migrate_test.go` (new): migration applied on fresh
  DB; idempotent (applying twice is a no-op); tables exist after migration.

**Exit:** migrations run cleanly on a fresh DB; `sessions` + `events` tables
created with FK and CHECK constraints; idempotent re-application is a no-op.

### P1c — SQLite store implementation

**Files:**
- `internal/core/sessionhost/sqlstore/sqlstore.go` (new): `SQLiteStore` struct
  with `*sql.DB`, `NewSQLiteStore(ctx, dbPath) (*SQLiteStore, error)` constructor.
  Implements `sessionhost.Store` (EventAppender + EventLog — no interface change).
  Calls `migrations.Apply` in constructor.
- `internal/core/sessionhost/sqlstore/sqlstore_test.go` (new): unit tests for
  Append/Read/LastSeq; SessionCreated atomic session+event; non-SessionCreated
  append to unknown session fails; ephemeral draft rejection; invalid draft
  rejection; concurrent seq uniqueness under `-race`; `Read` cursor semantics
  (after=0, after>0, limit<=0, limit>0); empty/nonexistent session; cursor beyond
  last seq; `LastSeq` for nonexistent session returns 0.

**Exit:** `SQLiteStore` passes all unit tests; `go build`, `go vet` green.

### P1d — Shared contract harness + replay + reopen/durability

**Files:**
- `internal/core/sessionhost/store_contract_test.go` (new): shared
  `runStoreContract(t, newStore func(t) Store)` function. Tests:
  - Create session (append SessionCreated) → Read returns it at seq 1.
  - Append multiple events → Read returns them in ascending seq order.
  - LastSeq returns the highest committed seq.
  - Concurrent appends → seq unique and contiguous.
  - Cursor semantics: after=0 → all; after=N → seq > N; limit bounds result.
  - Ephemeral draft rejection.
  - Invalid draft rejection.
  - Replay: append a sequence of events → Read from 0 → reconstruct projection.
- `internal/core/sessionhost/store_contract_memlog_test.go` (new): invokes
  `runStoreContract` with `NewMemLog`.
- `internal/core/sessionhost/sqlstore/sqlstore_contract_test.go` (new): invokes
  `runStoreContract` with `NewSQLiteStore(tempFile)`. **Plus reopen test:**
  append events → close DB → reopen → Read returns same events; LastSeq returns
  same value. **Plus corrupt payload test:** manually corrupt a payload BLOB →
  Read returns an error (not a silent bad event). **Plus migration idempotency:**
  open → close → reopen (migrations re-applied) → no error.

**Exit:** shared contract passes on both `MemLog` and `SQLiteStore`; reopen test
confirms durability; `exit_gate_test.go` passes UNCHANGED (still uses `MemLog`).

### P1e — Cross-tenant isolation test at host layer

**Files:**
- `internal/core/sessionhost/cross_tenant_test.go` (new): creates two sessions
  in different tenants via `Host.CreateSession`. Appends events to both. Asserts:
  - `Host.ListEvents(tenantA, sessionA)` → succeeds, returns events.
  - `Host.ListEvents(tenantB, sessionA)` → returns `ErrSessionNotFound` (not
    empty, not `ErrNotAuthorized` — per `core/service.go` sentinel definition:
    "cross-tenant invisibility is `ErrSessionNotFound`").
  - `Host.GetSession(tenantB, sessionA)` → `ErrSessionNotFound`.
  - `Host.Subscribe(tenantB, sessionA)` → `ErrSessionNotFound`.
  - `Host.ListSessions(tenantB)` → does not include tenantA's session.
  - The store is NOT called after authorization fails (verify via a wrapper that
    counts calls — optional, if clean to implement).
- This test runs against the default `MemLog` (no SQLite dependency needed —
  it tests host-level authz, which is P0 code).

**Exit:** cross-tenant access is rejected with `ErrSessionNotFound` at the host
layer; no existence leak; both `ListEvents` and `Subscribe` paths covered.

## Acceptance criteria (P1 exit gate)

1. `SQLiteStore` implements `sessionhost.Store` (EventAppender + EventLog) — no
   interface change.
2. `Append` + `last_seq` advance is one SQLite transaction (atomic seq assignment).
3. `SessionCreated` append atomically creates the session row + first event.
4. Non-`SessionCreated` append to unknown session fails (FK enforced).
5. `Read` returns events in ascending seq order, cursor-exclusive. `limit <= 0`
   means no limit (clause omitted).
6. `LastSeq` returns the highest committed seq, 0 for nonexistent session.
7. Shared store-contract harness passes on both `MemLog` and `SQLiteStore`.
8. Reopen/durability test: events survive close → reopen.
9. `exit_gate_test.go` passes UNCHANGED (still uses `MemLog`).
10. Cross-tenant isolation: `ErrSessionNotFound` at host layer for ListEvents,
    GetSession, Subscribe; no existence leak.
11. `go build ./...`, `go vet ./...`, `go test -race ./...` all green.
12. Migration idempotent; corrupt payload → error (not silent).

## Known residuals / deferred

- **P2 scope (not P1):** `state`/`closed_at`/`title` session columns (no P1
  writer); `ListSessions` backed by SQLite; `turns`/`tool_calls`/`usage_records`
  tables; proto serialization for events (P1 uses JSON-v1); WAL checkpointing
  policy; kvr blob store; durable input-queue recovery + request-ID dedup
  (service.go comments note these as P1, but they require turn-level executor
  instrumentation that exceeds the SQLite event-log scope — explicitly deferred
  to P2).
- **Global write lock** (single connection via `SetMaxOpenConns(1)`): matches
  MemLog's global mutex. Per-session write parallelism is a P2 concern.
- **JSON BLOB versioning:** `encoding` column is `'json-v1'`. P2 proto migration
  reads old rows as JSON, writes new rows as proto. No cross-version field
  compatibility guarantee until P2 (the DB is non-compat-guaranteed until then).
- **`Sessions.state` staleness:** not applicable in P1 (column doesn't exist yet).

---

## v2.1 Addendum (op-41 findings folded)

### P1f — Wiring: daemon uses SQLiteStore (new chunk)

**Problem (op-41 #1, claude-fable #1):** The "swap" goal is unmet without a
wiring chunk. The chunks only add `SQLiteStore` + tests; production still uses
`MemLog`.

**Design:**
- Embedded mode (default) constructs `SQLiteStore` from a data-dir path
  (`<wakil-data>/sessionhost.db`). The path comes from config (extending the
  existing config pattern — minimal addition).
- `NewSQLiteStore` is injected via `WithStore()` in the same place the
  embedded-mode host is constructed today.
- `SQLiteStore.Close()` is called on daemon shutdown (after `Host.Close()`).
- Add a `host_restart_test.go` integration test: create session → append events →
  close host + store → reopen store → construct new host with the same store →
  `SessionSnapshot` returns persisted events → `ListEvents` returns same history.
  This runs against `SQLiteStore`, not `MemLog`.

**Files:**
- `internal/wiring/` (wherever the host is constructed today — verify path):
  construct `SQLiteStore` and inject via `WithStore`.
- `internal/core/sessionhost/sqlstore/sqlstore_restart_test.go` (new):
  host-level reopen/recovery integration test.

**Exit:** A real host instance constructed with `SQLiteStore` persists sessions
across close → reopen; `SessionSnapshot` reconstructs events from the store.

### Design amendments

1. **UPDATE with tenant guard (glm H1, gpt-5.6 #2):** Non-`SessionCreated` Append:
   ```sql
   UPDATE sessions SET last_seq = last_seq + 1 WHERE id = ? AND tenant_id = ?;
   ```
   Check `RowsAffected == 1`; else return `ErrSessionNotFound` (session not found
   or tenant mismatch — indistinguishable by design, matching the `service.go`
   "no existence leak" contract). This is the primary rejection mechanism; the
   composite FK is a backstop on the event INSERT.

2. **`UnmarshalPayload` returns value, not pointer (claude-fable #5):**
   `reflect.New(t).Elem().Interface()` returns the value `T`, not `*T`. This
   matches `MemLog`'s in-memory `Payload` representation (the producer's value).
   Type switches (`case event.SessionCreated:`) work identically across stores.

3. **`Close()` method (gpt-5.6, claude-fable):** `SQLiteStore.Close() error`
   closes the `*sql.DB`. Required for reopen/durability tests.

4. **Duplicate `SessionCreated` (glm B4):** `INSERT INTO sessions` PK violation
   returns an error wrapping `core.ErrSessionNotFound` (the session already
   exists — a duplicate creation is treated as "conflict"). Test in P1c.

5. **Migration bootstrap ownership (gpt-5.6 #4):** `migrate.go` owns
   `schema_migrations` creation (`CREATE TABLE IF NOT EXISTS`), NOT
   `001_init.sql`. `001_init.sql` creates only `sessions` + `events`. The
   runner: (1) begin tx, (2) `CREATE TABLE IF NOT EXISTS schema_migrations`,
   (3) read applied versions, (4) apply pending migrations (each in its own tx),
   (5) insert version after success. Concurrent `Apply` is serialized by
   `SetMaxOpenConns(1)` within one process; cross-process is serialized by
   SQLite's write lock (the `BEGIN` on `schema_migrations` blocks).

6. **Document MemLog/SQLite divergence (glm B3, claude-fable #2):**
   `SQLiteStore` rejects non-`SessionCreated` first append (FK/RowsAffected
   failure). `MemLog` auto-creates a per-session log on any append. This is a
   **deliberate, documented divergence** — the shared harness always appends
   `SessionCreated` first (matching host behavior). A store-specific test on
   `SQLiteStore` pins the rejection. `MemLog` is NOT tightened (it would break
   existing tests that rely on lazy creation).

7. **`RecoverRunning` invariant (glm B2):** After `CreateSession` returns
   successfully, the `SessionCreated` event has been appended (atomic
   session+event). The host calls `emitDraft(SessionCreated)` before
   `register()` — if the append fails, `emitDraft` panics and `CreateSession`
   never returns. Therefore a session row ALWAYS exists in SQLite before the
   host is aware of the session. `RecoverRunning` only processes sessions the
   caller supplies (from the store in P1+); a session with no row cannot appear
   in the recovery list. This invariant is verified by the restart test.

8. **Envelope-vs-payload ID (glm B1):** `SessionCreated` payload contains
   `WorkspaceID`, `AgentName`, `CreatedBy` — NO `TenantID` or `SessionID`. The
   envelope carries those. There is no mismatch possible. No cross-check needed.

9. **PRAGMA persistence test (gpt-5.6 #2, claude-fable #3):** P1d adds a test
   that queries `PRAGMA foreign_keys`, `PRAGMA busy_timeout`, `PRAGMA journal_mode`
   after reopen. The constructor sets pragmas via `db.Exec` on every open (same
   as `internal/memory/store.go` pattern). `SetMaxOpenConns(1)` limits pool
   concurrency; connection recycling is possible but pragmas are re-applied on
   each `Open` call (the `*sql.DB` is not recycled during a single process
   lifetime — only on reopen). The `internal/memory/store.go` reference
   implementation uses this exact pattern successfully.

10. **`Read` decodes payloads eagerly (glm H5):** `SQLiteStore.Read` calls
    `UnmarshalPayload` per row and returns `event.Event` with `Payload` populated
    as the value type. The `encoding` column is checked; unknown encoding → error.

11. **`Read` uses explicit `ORDER BY seq ASC`** (claude-fable, gpt-5.6).

12. **`encoding` CHECK constraint (glm H3):** `CHECK (encoding IN ('json-v1'))`
    on the events table. Expanded in P2.

13. **Clean up dead text (claude-fable):** The superseded single-column FK DDL
    and the `EnsureSessionRow` mention are removed from the plan body.

### Revised chunk ordering

P1a (codec) → P1b (migrations) → P1c (SQLite store) → P1d (contract harness +
reopen) → P1e (cross-tenant host test) → **P1f (wiring + restart test)**.

### Revised acceptance criteria (additions)

13. A host constructed with `SQLiteStore` persists sessions across close →
    reopen; `SessionSnapshot` / `ListEvents` return persisted events.
14. `PRAGMA foreign_keys` returns `1` after reopen.
15. Duplicate `SessionCreated` append returns an error.
16. Non-`SessionCreated` append to unknown session returns `ErrSessionNotFound`.
17. `SQLiteStore.Close()` releases the database handle.

### Revised scope note (op-41 scope contradiction)

The `service.go` comments mentioning durable input-queue recovery and
request-ID dedup as P1 are corrected: these require turn-level executor
instrumentation (turns table, pending-queue persistence) that is out of scope for
the event-log store. They are explicitly deferred to P2. The impl-plan §4 P1
definition ("SQLite control-plane + event log, migrations, tenant-keyed repos,
replay exit test, cross-tenant query test") does not include input-queue
recovery.
