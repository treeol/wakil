# Staging Store

Wakil's staging store is a fast, ephemeral, in-sandbox key-value store
that agents use for scratch space and subagent handoffs. It is backed by
[kvr](../kvrust/), a Rust UDS KV server running as a background process
inside the sandbox container.

## What staging is

- **Fast in-sandbox scratch memory.** Agents can store intermediate
  results, computed values, and handoff data without writing to the
  workspace filesystem.
- **Repo-scoped.** Each workspace gets its own staging directory
  (`~/.local/share/wakil/staging/<workspace-key>/`), isolated from other
  repos.
- **Survives sandbox restarts.** kvr saves a snapshot on graceful
  shutdown and periodically (default every 300s), so staging data
  persists across sandbox teardown/restart cycles within a session.
- **NOT durable memory.** Staging data is ephemeral — it is not the
  durable host-side store (a separate, later ticket). Staging data can
  disappear (sandbox crash without snapshot, manual cleanup, TTL expiry).
  Never rely on staging for data that must survive.

## Trust model

**All staging content is untrusted.** Staging is writable by sandbox
code, which means generated code or compromised agent flows can write
arbitrary content to the store. Wakil's host-side code never treats
staging content as trusted — the only host-side consumers are:

1. **The tool layer** (`staging_*` tools) — returns raw values to the
   agent without validation.
2. **Lifecycle management** — starts/stops kvr, manages the staging
   directory, and probes key presence at startup to surface a
   "staging: entries restored" note. Does not read values.

**Staging tools are ungated** — they bypass the confirmation prompt
that gates `run_shell`, `write_file`, etc. This is by design: staging
writes touch no workspace state, and the gate lives at promotion (the
future durable store ticket), not at the staging layer.

The future durable store and promote flow (next ticket) will treat all
staging content as untrusted input.

### Known property: snapshot file is writable by sandbox code

The snapshot file (`staging.kvr`) lives on the staging mount, which is
writable by sandbox code. This means generated code can bypass the kvr
protocol by editing the file directly. This is **acceptable** because:

- Staging content is untrusted by definition.
- kvr's snapshot loader is defensive: it validates the CRC32, caps
  allocation, and refuses to half-load on any error. Worst case: kvr
  starts empty.
- The snapshot never lives inside the repo/workspace mount, so it cannot
  be accidentally committed.

## Key conventions

Keys are prefixed by the writing agent's identity:

- **Main agent**: `main/<key>`
- **Subagents**: `sub-<id>/<key>` (where `<id>` is the first 8 chars of
  the subagent's per-dispatch-unique ChatID)

The tool layer **unconditionally** prepends the prefix on
`staging_put` and `staging_delete` — agents cannot write outside their
prefix. This is provenance-lite: it establishes who wrote what without
any agent-level cooperation.

**Cross-prefix reads are allowed.** `staging_get`, `staging_get_many`,
and `staging_list` accept full keys/prefixes — an agent can read
another agent's data. This is the point: subagent handoffs.

### Example: subagent handoff

1. Main agent dispatches a subagent with ChatID `abc12345-...`.
2. Subagent writes: `staging_put(key="findings", value="bug at parser.go:42")`
   → stored as `sub-abc12345/findings`.
3. Main agent reads: `staging_get(key="sub-abc12345/findings")` →
   returns `"bug at parser.go:42"`.

## TTL guidance

**TTL is strongly encouraged.** Staging is ephemeral — set a TTL on
anything that should not outlive the session. Use `ttl_seconds` on
`staging_put`:

- **Handoff data**: 300–600s (5–10 minutes) — enough for the main
  agent to read it back after the subagent completes.
- **Intermediate results**: 60–300s (1–5 minutes) — enough for the
  current turn.
- **Long-lived scratch**: 3600s (1 hour) — for data that should survive
  the session but not forever.

Without a TTL, entries persist until explicitly deleted or the store
reaches its entry limit (default 100,000).

## Tools

| Tool | Description |
|---|---|
| `staging_put(key, value, ttl_seconds?)` | Store a value. TTL present → SETX, absent → SET. Key is prefixed with your agent identity. |
| `staging_get(key)` | Retrieve a value by full key (cross-prefix allowed). |
| `staging_delete(key)` | Delete a key under your prefix. Key is prefixed automatically. |
| `staging_list(prefix?)` | List keys, optionally filtered by prefix. Caps at 200 with truncation marker. |
| `staging_get_many(keys)` | Retrieve multiple values by full keys (JSON array, cross-prefix allowed). |

### Discovery tier note

`staging_put` in the discovery tier (read-only subagents) is a
**deliberate exception** to "discovery is read-only": staging writes
touch no workspace state, and handoff-writing is the tier's purpose.
The discovery tier cannot write files, run shell commands, or mutate
workspace state — staging is the only write capability it has, and it
writes to an isolated, untrusted store.

## Relationship to the future durable store

Staging is the **ephemeral layer**. The durable host-side store (next
ticket) will be the **trusted layer** — but only after content is
promoted through a review/acceptance flow. The promote flow will:

1. Read from staging (untrusted input).
2. Validate/sanitize as needed.
3. Write to the durable store with provenance metadata.

Until the durable store exists, staging is the only in-sandbox memory.
Nothing in staging is trusted, promoted, or durable by default.
