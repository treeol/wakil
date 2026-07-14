# Durable Memory

Wakil has a two-tier memory architecture. This document describes Tier 2 —
the durable, trusted, host-side memory store — and the gated flow between
the tiers.

## Tier overview

| Tier | Store | Lifetime | Gating | Location |
|------|-------|----------|--------|----------|
| T1 staging | kvr (Rust KV store) | Ephemeral (snapshot survives restarts) | Ungated — any agent | In-sandbox |
| T2 mid | SQLite (durable store) | 1h–7d TTL, auto-expires | Direct active writes — main agent AND subagents | Host-side |
| T2 durable | SQLite (durable store) | No TTL | PROPOSED on write; promotion to ACTIVE requires main agent or user | Host-side |

**Core principle: gate strictness scales with lifetime.** Sandbox code and
subagents must never gain ungated write access to durable memory — a poisoned
durable entry is a long-lived prompt-injection channel that fires in future
sessions. The gate lives host-side, where sandbox code physically cannot reach.

## Trust model

- The durable store lives on the HOST at `<wakil-data>/memory/<workspace-key>/memory.db`.
- It is never mounted into the sandbox.
- It is only writable through Wakil host-process code paths (the `memory_*`
  tool handlers in `internal/agent/memory_tools.go`).
- Tier-gating (main-only operations) is enforced in the TOOL LAYER by agent
  identity (`a.IsSubagent`), same mechanism as staging prefix enforcement —
  not by prompt instruction.
- Subagents can propose durable entries but never promote them.

## Storage

SQLite via `modernc.org/sqlite` (pure Go, no cgo). WAL mode. One DB per repo
at `<wakil-data>/memory/<workspace-key>/memory.db`. The workspace key reuses
the same SHA-256 derivation as staging (truncated to 16 hex chars for path
length). Single-writer discipline: app-level mutex + `SetMaxOpenConns(1)`.

### AUTOINCREMENT

The primary key uses `INTEGER PRIMARY KEY AUTOINCREMENT` (not plain
`INTEGER PRIMARY KEY`). The 30-day hard-delete of expired entries means plain
rowids would get reused, and reused rowids would silently corrupt supersedes
chains pointing at dead entries. AUTOINCREMENT guarantees monotonic IDs that
are never reused — a dangling supersedes reference always points at a
genuinely deleted entry, never at a recycled unrelated row.

### Schema

```
entries(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  key TEXT NOT NULL,
  value TEXT NOT NULL,
  kind TEXT NOT NULL,
  tier TEXT NOT NULL CHECK(tier IN ('mid','durable')),
  status TEXT NOT NULL CHECK(status IN ('active','proposed','rejected','superseded','expired')),
  writer TEXT NOT NULL,
  session_id TEXT NOT NULL,
  tainted INTEGER NOT NULL DEFAULT 2,  -- 0=false, 1=true, 2=unknown
  created_at INTEGER NOT NULL,          -- epoch ms
  expires_at INTEGER,                   -- NULL for durable tier
  anchors TEXT,                         -- JSON: [{"path":..., "hash":...}]
  note TEXT,                            -- reject reasons, tombstones, annotations
  supersedes INTEGER REFERENCES entries(id),
  superseded_by INTEGER REFERENCES entries(id),
  promoted_by TEXT
)
```

### One-active-per-key

Enforced by a partial unique index:
```sql
CREATE UNIQUE INDEX idx_one_active_per_key ON entries(key) WHERE status = 'active';
```
This allows unlimited PROPOSED, SUPERSEDED, REJECTED, and EXPIRED entries
per key, but exactly one ACTIVE.

### Supersede logic

When a new active entry is written for a key that already has one, the old
entry is superseded FIRST (within a transaction), then the new entry is
INSERTed. History is kept, not overwritten. The transaction uses the default
deferred begin; the app-level mutex prevents concurrent writers within one
process.

### FTS5

Full-text search via an FTS5 virtual table over `(key, value)`, kept in sync
via `AFTER INSERT`, `AFTER DELETE`, and `AFTER UPDATE` triggers. FTS5
availability is verified by a smoke test at M1.

## Tool surface

All memory tools are HOST-side (like `run_shell`), registered for all agent
tiers. Main-only tools are rejected at dispatch time for subagents.

| Tool | Available to | Description |
|------|-------------|-------------|
| `memory_put` | All tiers | Write: TTL present → mid/active; absent → durable/proposed |
| `memory_promote` | Main only | Promote proposed → active (optional edit) |
| `memory_reject` | Main only | Reject proposed entry (reason in note) |
| `memory_get` | All tiers | Get active entry for key |
| `memory_search` | All tiers | FTS5 search (active only by default) |
| `memory_list` | All tiers | List by prefix/tier/status |
| `memory_forget` | Main only | Supersede active with tombstone (nothing hard-deleted) |
| `memory_promote_from_staging` | Main only | Bridge staging → durable proposed |

### memory_put semantics

| Caller | TTL present | → tier | → status |
|--------|-------------|--------|----------|
| main | yes (3600–604800) | mid | active |
| main | no | durable | proposed |
| subagent | yes (3600–604800) | mid | active |
| subagent | no | durable | proposed |

TTL bounds: 3600 ≤ ttl ≤ 604800. Key: ≤ 256 bytes. Value: ≤ 64 KiB.

## Provenance rendering

Every entry returned to an agent carries a one-line provenance header:

```
[mid-tier | sub-4f2a91c3 | 2026-07-14 | expires 2026-07-16 | tainted]
[durable-tier | main | promoted by main | taint-unknown]
[durable-tier | sub-abc12345 | proposed | taint-unknown]
[durable-tier | main | anchors: 1 stale of 2]
```

Compact, one line, always present. Durable proposed entries rendered to the
main agent for promotion review show the taint flag prominently.

## Taint signal (A1: session-cumulative)

An entry written in a session where the writing agent touched untrusted
external content carries `tainted=true`.

### Mechanism

A sticky per-App boolean (`touchedExternal`) is set whenever the agent's
grounding records `web` or `oracle` entries (web search, URL fetch, MCP tool
calls, mashūra oracle). Once set, it is never reset for the App's lifetime.
Taint at write time = `touchedExternal ? true : unknown`.

- **Per-agent-lifetime** is the natural granularity: subagents are short-lived
  so their flag is tight; the main agent's flag going sticky for a long session
  slightly overcounts, which is the correct direction to err for a trust flag.
- **MCP** is captured because MCP tool calls are recorded as `Type: "web"` in
  the grounding.
- **`false`** is reserved for future use — the absence of web/oracle grounding
  doesn't prove no untrusted content was touched (file-read injection is not
  captured), so the conservative answer is `unknown`, not `false`.
- **Staging bridge**: staging-promoted entries are always `taint=unknown`.
  Staging values are bare strings with no taint metadata; the original writer's
  grounding state is lost in kvr.

## Anchor staleness (A2: flag-not-filter)

Entries can be written with file anchors — workspace-relative file paths whose
SHA-256 hashes are computed at write time. At read time, current hashes are
recomputed (host-side, cheap). Mismatched or missing files mark the entry
STALE in the rendered output.

**Stale entries are still returned, flagged — never silently dropped.** A stale
architecture note is often still the best available answer; silently hiding it
because a file changed is how an agent re-derives (possibly wrongly) something
it already knew.

Expired entries are filtered from results. Stale entries are not.

## Note column (A3)

Reject reasons, forget tombstones, and annotations go in a dedicated `note`
TEXT column — not overloaded into the `anchors` JSON (which has defined
`{path, hash}` semantics).

## Expiry

- **Lazy on read**: expired entries (`expires_at < now`) are filtered from
  `memory_get`, `memory_search`, `memory_list` results.
- **Sweep on session start**: `UPDATE entries SET status='expired' WHERE
  status='active' AND expires_at < now`. Then hard-delete entries with
  `status='expired'` and `expires_at < now - 30d` (auditability window).
- **Mid-session**: reads always check `expires_at`. A long session will not
  serve expired entries.
- **Exact boundary**: `expires_at >= now` means active (inclusive). An entry
  at the exact boundary is still active; 1ms past, it is expired.

## Dangling supersedes

The 30-day hard-delete intentionally leaves dangling `supersedes`/`superseded_by`
references. The render path handles them gracefully: `GetByID` returns `ErrNotFound` for
deleted entries, and the tool layer's `renderSupersedesHistory` renders
"history unavailable — hard-deleted past audit window" rather than erroring
or mis-attributing. AUTOINCREMENT prevents ID reuse, so a dangling reference
always points at a genuinely deleted entry.

## Session-start digest

At session start, a compact memory digest is injected into the main agent's
preamble (the day-stable system message at `Conv[0]`):

```
Memory: 12 active durable entries, 3 mid-tier, 2 pending proposals.
Recent: arch/auth-flow, decision/sqlite-choice, summary/m1-store-core, ...
Use memory_search or memory_get to retrieve entries.
```

**The digest is a startup/day-rollover snapshot, NOT live.** Entries written
or promoted mid-session will not appear in the digest until the next day
rollover or session start. This is deliberate: invalidating the preamble on
every memory mutation would destroy prompt-cache prefix stability (the exact
thing `ensurePreamble` is designed to preserve). The agent uses
`memory_search` for live data. The digest is a heads-up, not a source of truth.

Pending proposals > 0 also gets a TUI startup note: `memory: N proposals pending`.

## Relationship to staging

Staging (T1) and durable memory (T2) are independent systems:

| | Staging (T1) | Durable memory (T2) |
|---|---|---|
| Store | kvr (Rust KV) | SQLite (pure Go) |
| Location | In-sandbox | Host-side |
| Lifetime | Ephemeral (snapshot) | Mid-TTL or durable |
| Gating | Ungated | Tier-gated by agent identity |
| Search | Prefix scan | FTS5 full-text |
| Provenance | Key prefix only | Full (writer, taint, staleness) |

The intended handoff: subagents stage substance in kvr, the main agent reviews
and bridges it to durable memory via `memory_promote_from_staging`. The
staging key's prefix is recorded as the `writer` (provenance flows through);
the promoter is recorded in the entry's note. The staging key is NOT deleted
by the bridge — the caller can `staging_delete` if desired.

## Discovery-tier write exception

`memory_put` in the discovery tier is a deliberate exception to "discovery is
read-only" — same rationale as `staging_put`: memory writes touch no workspace
state, and proposing durable entries is a legitimate subagent capability.

## Spill-to-disk convergence decision

**Decision: follow-up, not this ticket.** The spill mechanism
(`SpillToCache` in `internal/tools/toolcap.go`) is a transient output-delivery
system: per-session, unkeyed, unsearchable, consumed only for compaction
recovery. Durable memory is a cross-session knowledge system: entries survive
across sessions, are keyed, searchable, provenance-tagged, and gated by tier.
The two systems serve different concerns. A future enhancement could
optionally `memory_put(kind='summary', tier='mid', ttl=24h)` after successful
subagent dispatch, but that's a separate change after the durable store is
proven. This ticket must not change spill behavior, spill retention, or
read-file cache interception.

## Multi-process safety

WAL mode handles concurrent readers. Single-writer discipline via app-level
mutex. Two wakil instances on the same workspace would conflict on the SQLite
write lock — but this is already an issue for staging (same kvr socket, same
UDS path). The existing constraint "one wakil process per workspace" applies.
