# Proposal: Cross-Session Recall — searching old sessions from a new one

**Status:** Proposal v2 (revised after Mashūra review)
**Author:** Wakil
**Date:** 2026-08-02
**Related card:** (created in Trello after review)

---

## 1. Goal

In a fresh wakil session I want to recall things done in *earlier* sessions of this
repo. When I say "remember when we did X," wakil should (a) recognize the request may
span prior sessions, (b) search its saved session history, and (c) fold at least a
summary — or, on demand, a chunk of the original transcript — into the current context.

Today wakil **can't do this**: session transcripts persist to disk, but nothing indexes
or searches them from a new session. The only cross-session bridge is the *manual,
workspace-scoped* durable memory store.

---

## 2. Current implementation (verified)

### 2.1 Sessions are fully persisted — but only restorable, not searchable

- Every session's **transcript** is saved atomically as indented JSON at
  `<data>/sessions/<chat_id>.json`
  (`Session` struct: `chat_id, model, label, workspace, created, updated, conv, saved_workflow`
  — `internal/agent/session.go:21-30`; dir resolution `:32-44`; `WriteSession` `:48-57`;
  list/load `ListSessions` `:60-86`, `LoadSession` `:90-116`).
- `SaveSession` snapshots `Conv` **on every exit path of a user turn** — it is deferred
  inside `Send`, so it runs once per user turn (not per tool-loop iteration)
  (`internal/agent/app.go:594-610` impl, `:658` defer). `/new`, `/reset`, `/handoff`
  save the old session then rotate to a fresh `chat_id` (`commands.go`, `handoff.go`).
  **No session is ever deleted.**
- Session **listing is metadata-only**: `PrintSessions` / `SessionListText` show date,
  turn count, and first 40–50 chars of the first user message. No full-text search, no
  summary, no retrieval into context.
- **Caveat:** "transcript" ≠ always verbatim. Compaction folds older turns into a lossy
  `[Summary of earlier conversation]` system message (`compact.go:336-343`), and
  `enforceHardMax` can drop turns. So explicit recall can only return *retained*
  messages, summaries, or spill artifacts — never recover detail hardened out long ago.

### 2.2 Summaries exist but are in-memory and collapsed into one "latest" blob

- Compaction folds older turns into a leading `[Summary of earlier conversation]` system
  message (`compact.go:80-100` render, `:197-211` summarizer, `:336-343` assembly;
  trigger `finalizeTurn` in `turn_phases.go:297-312`).
- `proxySummarizer` renders the transcript and asks the model to keep
  *facts, decisions, file paths, commands, open tasks* (`compact.go:197-211`).
- Each new compaction **replaces** the prior summary — no history of progressive
  summaries. The summary survives only as a system message inside the persisted `Conv`,
  so at session end you get the *latest* running summary, not a per-session distilled
  record. **This existing in-transcript summary is free, harvestable data.**

### 2.3 Cross-session bridge today = durable memory only

- The memory store is the sole cross-session mechanism (`internal/memory/store.go`):
  SQLite WAL + FTS5, workspace-scoped, `session_id` recorded per row but **not** used
  for recall.
- It is **write-on-explicit-action**: entries land only via `memory_put` / `memory_promote`.
  Transcripts are never auto-ingested. Durable writes land as **PROPOSED** and require
  explicit promotion — a deliberate trust gate.
- At startup the preamble injects only a **counts digest** (active durable/mid/pending +
  top-5 recent keys) — not content (`app.go:1049-1074`).
- `retrieveMemoryContext` (`internal/agent/retrieval.go:39-83`) is the existing
  *turn-entry retrieval* pipe: searches memory + skills, byte-caps, folds results into
  the **user message** (never the preamble) as **untrusted data**
  (`formatRetrievedContext` `:88-115`). **This is the exact pattern to extend.**
  - Note: `formatRetrievedContext`'s header literally says "context from memory" (`:92`).
    Reusing it verbatim for session content would mislabel provenance — a separate
    formatter/header is required.

**Gap summary:** transcripts are durable but dead (metadata-only listing); summaries are
ephemeral/collapsed (but harvestable); memory is manual and workspace-scoped. So
"remember when we did X" has no path from a new session.

---

## 3. Proposed design

### A. Session index (searchable history)
A persistent, **workspace-scoped** index of prior sessions, built on the same SQLite
FTS5 conventions as the memory store.

**Indexed content** (per session): metadata (chat_id, workspace, created, updated),
each **user turn** (text + turn ordinal), each **assistant textual response** (the
answers/decisions/patch descriptions where "what we did" actually lives), and a
**per-session summary**. Raw tool output is **not** indexed by default (high-volume,
noisy, credential-bearing). Source identifiers (chat_id + turn ordinal) are kept so
explicit recall can cite and expand the exact source.

**Write path — hybrid, not pure lazy:**
- **Eager at finalization** (the clean, cheap hook): when a session is rotated away via
  `/new`, `/reset`, `/handoff`, or on clean exit, ingest it into the index. These are the
  same triggers as end-of-session summary capture, so it's one hook.
- **Lazy backfill + reconciliation** for pre-existing sessions: on first search, scan
  `ListSessionsScoped` and ingest sessions not yet in the index. A **watermark/manifest
  table** (per-session `last_ingested_updated, last_ingested_hash`) means cold starts
  only diff changed sessions, not re-read+re-parse everything. Reconcile **deletions**
  (purge index rows for session files removed from disk) and handle corruption by
  rebuilding from the transcript JSON (source of truth; index is disposable).
  - Backfill runs **in the background**, not on the critical path of the first query —
  degrade gracefully (return empty + log) until ready. `SaveSession` already writes the
  JSON every turn, so a partially-ingested active session is fine; the active `chat_id`
  is excluded from search anyway.

**Location:** new package `internal/sessionhistory`, DB at
`<data>/sessionhistory/<workspace-key>/sessions.db` (host-side, sandbox-excluded —
mirrors the memory store). Schema versioning + migration path (like `migrateSchema` in
`store.go`), or an explicit "rebuildable from source" statement.

### B. Recall into context
One retrieval backend, two front ends. Searches for memory / skills / sessions are
**parallelized** and share one byte cap (not three independent caps).

1. **Implicit (turn entry, gated):** extend the existing turn-entry retrieval to also
   search the session index — but **not unconditionally on every turn**. Gate it: fire
   session search only on explicit recall phrasing (a "remember …" / "when did we …" /
   "previous session" heuristic) or as a fallback when memory/skill retrieval returns
   nothing. Inject only **session summaries** (not verbatim turns), capped and framed as
   **untrusted data** with a header that names the session source (not "from memory").
   - This keeps existing turns byte-identical (no cache/footprint change) when there's
     no recall intent.
2. **Explicit (on demand):** a `remember <query>` slash command for user-directed
   search + expansion. Prefer **verbatim-turn/transcript chunks as a user-only operation**
   — a model-invocable `session_get` that returns raw transcript is an *injection
   amplifier* (snippet A can induce pulling in snippet B...). If a tool is added, it is
   restricted to summaries or gated on confirmation, workspace-scoped, and returns at
   most N bytes with chat_id + turn citation, marked untrusted.

**Recursive-persistence guard (critical):** retrieval is folded into the user message,
which gets stored in `Conv` and could be re-indexed as "user turn" text → a
**self-amplifying echo** (retrieved old content re-indexed, re-retrieved, re-distorted).
Must (a) **strip retrieval blocks before indexing**, and (b) **exclude the current
`chat_id`** from search.

### C. Per-session summary capture
End each session with a durable, distilled summary (fixes the "collapsed-into-one-blob"
smell in §2.2).

- On `/new`, `/reset`, `/handoff`, clean exit: run one `proxySummarizer` pass over the
  finalized transcript and store the summary **in the index** as `summary_version` +
  source hash + taint (summaries are generated from untrusted transcripts, so they are
  **tainted**).
- **Minimum-turn / minimum-size threshold** before summarizing — skip trivial 1–2 turn
  sessions (wasteful inference).
- **Fallback for backfilled/crashed sessions:** on lazy ingest, if no end-of-session
  summary exists, **harvest the existing in-transcript `[Summary of earlier
  conversation]` compaction summary** as a fallback. This is free data already on disk.
- Best-effort: if the backend is down at exit, no summary is generated this time; the
  backfill/harvest path covers it next session.

**Index-only in Phase 1 — no auto durable-memory bridge.** Auto-writing session
summaries into the trusted memory store would bypass its promote-gate and pollute a
curated, trust-gated store (and duplicate content across stores → drift). If a bridge is
wanted later, it is an **explicit user action**: `memory_promote` of a recalled summary.

---

## 4. What we reuse vs. build

**Reuse (exists, verified):**
- Session read/listing + workspace scoping: `session.go` (`ListSessionsScoped`,
  `LoadSessionScoped`, `SessionScope`, `sameWorkspace`, `All` opt-in).
- Transcript rendering: `renderTranscript` (`compact.go:80-100`).
- Summarizer primitive: `proxySummarizer` + `RecordInferenceCost` (`compact.go:197-211`).
- Retrieval/injection pattern + byte caps + untrusted framing + timeouts:
  `retrieval.go` (`retrieveMemoryContext`, `formatRetrievedContext`; caps at `:26-48`).
  **Refactor the header to be parameterized** so session content isn't mislabeled "from
  memory."
- SQLite + FTS5 store conventions + migrations: `internal/memory/store.go`.

**New:**
- `internal/sessionhistory` index store (schema + FTS5 + manifest/watermark).
- Eager-finalization + lazy-backfill ingest; reconciliation of deletes/changes.
- Gated implicit recall integration (parallel searches, shared cap, strip-before-index,
  current-session exclusion).
- `remember <query>` command (+ optionally a summary-only tool).
- End-of-session summary capture with min-turn threshold + fallback harvest.
- Security tests (cross-workspace leakage negative test, injection, secret redaction).

---

## 5. Security & correctness

- **Prompt injection:** retrieved data is framed untrusted and capped — but framing is
  **mitigation, not a boundary**. Old user/assistant text, tool output, and *generated
  summaries* can all carry adversarial instructions (a transcript can instruct the
  summarizer to embed directives, which then persist and re-inject). Controls:
  - Retrieved data is a clearly-delimited, non-instructional block in the user message.
  - Retrieved text must never authorize tools, broaden scope, or bypass confirmation.
  - Implicit recall injects **summaries/snippets only**, never raw transcripts.
  - Gated implicit recall limits exposure to turns expressing recall intent.
  - **Security test:** a malicious "old session" whose text contains instructions must
    not trigger tool use or scope expansion.
- **Secret hygiene:** a `*key*`/`*secret*`/`*token*` substring denylist is **not a
  control** (false positives like "keyboard"; false negatives like `AWS_ACCESS_KEY_ID`).
  Do **not** index raw tool output (the main credential bearer). All retrieved content
  stays untrusted + capped. Document honestly that transcript search can surface secrets
  — an inherent property — and apply restrictive file permissions to session/index files
  (session dir is created `0755`; the memory DB dir is `0700` — align to `0700`).
  Optionally add a deterministic redaction pass at index time (pattern/entropy-based),
  not keyword matching.
- **Taint granularity:** taint at the **session** level (one taint for the whole session
  if any source content is tainted) — simpler and safer than per-turn.
- **Workspace isolation / fail-closed:** empty or unresolved workspace must **not**
  silently mean global recall; legacy sessions with empty `Workspace` are excluded from
  scoped results (matching `ListSessionsScoped`); the **current** session is excluded;
  explicit IDs must **not** bypass scope for model-driven recall (unlike `LoadSession`,
  which deliberately allows global explicit-ID lookup — recall must not reuse that);
  `--all` is a **user action**, not a model-selectable option.
- **Cost:** parallelized searches with shared cap and ~2s bound (matching memory
  retrieval); non-fatal on error; min-turn threshold avoids trivial summaries; cap
  summary input deterministically; skip unchanged sessions via manifest.
- **Prompt-cache stability:** injection folds into the user message, preserving
  `Conv[0]` (`app.go:1078-1092`). But more retrieval sources = more frequent prefix
  changes. The **explicit `remember` command returns to the user, not into the next API
  call — cache-neutral.** Gated implicit recall limits churn to recall-intent turns. This
  argues for making **explicit recall the default** and implicit recall opt-in/gated.

---

## 6. Phasing

**Phase 1 — Searchable index (foundation).**
- `sessionhistory` store (schema + FTS5 + manifest/watermark + migration/rebuildability).
- Eager ingest at session finalization + lazy background backfill/reconciliation.
- Fallback harvest of existing in-transcript compaction summaries.
- Index user turns + assistant text + (fallback) summary. No tool output.
- Tests: index/query, change + delete reconciliation, workspace scoping,
  **cross-workspace leakage negative test**, non-ASCII/paraphrase precision limits
  noted as accepted limitations.

**Phase 2 — Explicit recall.**
- `remember <query>` command (+ summary-only tool if approved): returns summary or
  user-directed verbatim chunk with citation, untrusted-framed, ASCII/`--all`-gated.
- End-of-session summary capture with min-turn threshold, written to index (tainted).
- No auto durable-memory bridge (explicit promote only, if any).

**Phase 3 — Implicit recall (separate, gated, opt-in).**
- Gate-triggered session search in turn-entry retrieval (recall-phrasing heuristic or
  memory/skill fallback), shared cap, strip-before-index, current-session exclusion.
- Injection + feedback-loop security tests before enabling broadly.

---

## 7. Open questions (for implementation)

1. Confirm the `remember` surface: slash command only, or also a summary-only tool gated
   on confirmation? (Draft leans command-first; tool later and restricted.)
2. Confirm `--all` is user-only (never model-selected). 
3. Exact heuristic for gating implicit recall in Phase 3.
4. Redaction pass at index time: in scope for P1 or deferred (documented limitation)?
5. Single workspace-scoped DB per workspace vs. a single global DB with a workspace
   column (for easier `--all`). Draft picks per-workspace to mirror the memory store;
   `--all` would open multiple DBs.

---
*Reviewed 2026-08-02 by external oracles (gpt-5.6-sol, claude-fable-5, glm-5.2). Folded in:
hybrid write path; index assistant text + harvest existing summaries; gate implicit
recall; shared cap + parallel search; separate untrusted header; min-turn threshold;
index-only summaries (no durable-memory auto-bridge); session-level taint; fail-closed
scoping; cross-workspace negative test; feedback-loop guard; fix of the
"SaveSession frequency" contradiction.*
