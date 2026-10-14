# Long-Session Mode — Request-Scoped Context Collapse (v3)

> Status: implemented v1 (commits 943044c..0d648d6). See `docs/long-session-mode-plan.md`.
> History: v1 (task-based classification) → v2 (Mashūra review op-3, 2026-10-13:
> "projection, not mutation") → v3 (this doc: reframed around request-scoped
> collapse per Valon's direction). v2 conclusions that still hold are folded in.

## 1. Goal

The live context should not accumulate the *working set* of past requests.
A request may need to load 100k of files/searches/tool output to produce a 10k
answer (e.g. a plan). Once the answer exists, the answer *is* the distilled
result of the 100k — carrying the 100k into every later request is waste.

Target behavior: after a request completes, the session falls back toward
**base = preamble + [summary] + prior requests' (user message + final answer)**.
Context grows during a request and shrinks back after it. Not tied to "tasks":
the unit is the **request** (one human turn), and the decision is
"is this request's working set needed for the upcoming work?"

This is the discovery-subagent pattern (child loads 100k in a throwaway context,
returns a digest) applied to every main-agent turn.

## 2. Turn anatomy

A human turn (one `SendOutcome` call, `app.go:967`) produces:

| Part | Messages | Example size |
|------|----------|--------------|
| **U** — user message | the user message (incl. folded retrieval block / workflow directive, `app.go:1000-1012`) | 0.5–3k |
| **W** — working set | assistant messages carrying `tool_calls` + their intermediate text, all `tool` results, injected user-role envelopes (async results, budget/limit prompts, `turn_phases.go:183-260`) | 0–100k+ |
| **A** — final answer | the last assistant message with no tool calls (`turn_phases.go:333-346`) | 0.5–10k |

**Collapse** = in later requests, send U + A (+ a short keep-note) instead of
U + W + A.

## 3. What exists today (verified)

- **Stale tool-result eviction** (`app.go:1697-1758`, `finalizeTurn` →
  `turn_phases.go:587`): stubs `tool` message *content* older than
  `tool_result_ttl` turns as `[evicted — N chars — full content at: <path>]`
  (`internal/tools/toolcap.go:79-85`), spilling to disk first if needed.
  Skips pinned messages and results ≤ 50 bytes. Default TTL 3
  (`internal/config/config.go:835`); `-1` = never; disabled if
  `tool_result_cap <= 0`. Subagents use `-1` (`subagent.go:36`).
- **Compaction / hard-max** (`compact.go:302-522`, `668-756`): size-triggered
  summarization of older turns; best-effort ceiling.
- **Handoff** renders user + assistant *text only* (`renderIndexableTranscript`
  via `handoff.go:326-330`) — i.e. handoff already treats W as disposable.

So today's eviction is a partial, delayed form of collapse: it only removes
tool-result **content**, keeps tool-call arguments and intermediate assistant
text, and fires TTL turns late.

## 4. Immediate experiment: `tool_result_ttl: 0`

Verified behavior (`app.go:1723` + `internal/agent/toolcap_test.go:25-49`):
eviction runs at the **end** of each turn and keeps the most recent `ttl+1`
user-role turns intact. With `ttl = 0`:

- At the end of turn N+1, turn N's tool results are stubbed.
- So a turn's results are still re-sent throughout the **next** turn, then gone.
  (One-turn lag, not immediate.)
- The lag is the *typical* case, not a guarantee: `Compact` also calls
  eviction (`compact.go:315`), including via `enforceHardMax` and the
  backend-downshift path `fitConvToWindow`, which runs at `Send` entry before
  the new user message is appended; and the boundary counts injected
  user-role envelopes as turns. Both can evict earlier. [Mashūra op-4]

Not collapsed by this setting:
- tool-call arguments (e.g. `write_file`/`edit_file` payloads can be large),
- intermediate assistant text between tool rounds,
- pinned subagent result breadcrumbs,
- results ≤ 50 bytes.

Side note [inferred — confirm]: turn boundaries key on `Role == "user"`, and
async-result envelopes are user-role, so a turn that received async results
mid-turn may have earlier results from the same turn evicted a bit sooner.
Harmless for the experiment.

What to watch while running it: how often the model re-reads a file it just
read one turn earlier, and whether answers lose quality on follow-ups like
"implement step 3 of that plan". The re-read rate is the main risk this design
has to justify.

Config (user config file): `"tool_result_ttl": 0` (requires
`tool_result_cap > 0`, default 8000).

## 5. Design

### 5.1 Projection, not mutation (carried over from v2 review)

`a.Conv` remains the full log of record. The single model call site
(`turn_phases.go:265-267`, `msgs := a.Conv`) becomes
`msgs := a.contextProjection()`, which omits W for collapsed turns.

Why: `SaveSession`, `/resume`, `/rewind` checkpoints (`ConvLen` indices),
handoff, and `ConvSnapshot` readers keep working unchanged; collapse is
reversible; no orphaned `tool_calls`/`tool` pairs can be created inside
`a.Conv`. The projection drops W as a whole, so the emitted sequence is
`user → assistant(text)`, which is schema-valid.

### 5.2 Stable turn identity

Record a `turnStart` (monotonic turn ID + `a.Conv` index) at `SendOutcome`
entry. W = everything between U and A for that ID, **including** injected
user-role envelopes. This replaces `Role == "user"` as the turn boundary for
collapse (that boundary is unreliable — see §4 side note). Compaction must
remap/clear these records the same way it clears checkpoints
(`compact.go:517`).

### 5.3 Who decides, and when

Decision options for each finished turn's W:

- **collapse** — send U + A + keep-note only.
- **keep** — send everything (today's behavior).
- **partial** — keep specific tool results (by tool_call_id), collapse the rest.
  Kept results must be closed over structurally: keeping a `tool` result
  requires keeping its parent assistant `tool_calls` message **and all sibling
  results** of that message (same closure `Compact` applies to pinned tool
  messages, `compact.go:340-386`). Otherwise the projection is schema-invalid.
  [Mashūra op-4]

Proposed hybrid:

1. **End of turn — model proposes.** A dedicated tool (e.g. `context_keep`)
   with structured args: `{mode, keep_ids[], note}`. A tool, not parsed text
   (sentinel parsing is fragile and leaks into the answer). The `note` lists
   what would be needed to resume — files + line ranges, spill paths,
   decisions — capped (e.g. 1 KB).
2. **Next request — policy applies.** The decision is applied when the next
   request is assembled, not at end of turn. This lets the user override
   (`/keep`, `/collapse`) and keeps the most recent turn's state inspectable.
3. **Default when the model doesn't call the tool:** configurable;
   `keep` initially (zero behavior change), `collapse-after-1` later
   (collapse W of turns older than the previous one).

Failure asymmetry: a wrong **collapse** is recoverable (spill paths in the note
→ `read_file`, or re-run the tool); a wrong **keep** only costs tokens
(= today). So the system can be biased toward collapse once telemetry shows
the re-read rate is acceptable.

Alternative considered: deciding relevance at the *next* request with a small
classifier call over (next user message, prior turn digests). More accurate in
principle (it knows the upcoming work), but adds a per-request inference and
latency. Defer unless telemetry shows the end-of-turn proposal is poor.

### 5.4 Hard rules (never collapse)

- The current, still-running turn (mid-turn W is managed by existing
  per-turn tool budget / capping / compaction).
- A suspended turn or any turn while async ops are pending
  (`countActiveAsyncOps() > 0`, `turn_phases.go:347-356, 567`) — the pending
  completion lands into that context.
- Any turn while the async inbox holds completed-but-undelivered results
  (`hasInboxContent()`, `turn_phases.go:363`). `streamTurn` can exit on
  `forceFinish` without draining the inbox, so "no active ops" alone is not
  enough. [Mashūra op-4]
- Subagent apps (`IsSubagent`) — mode hard-disabled.
- If spilling W's tool results fails, keep W (archive before exclusion).

### 5.5 Pinned subagent breadcrumbs

Today these are pinned so the summary path survives compaction
(`turn_phases.go:455-459`). Under collapse, the breadcrumb's purpose is met by
copying its spill path into the keep-note; the projection may then omit the
pinned tool message. This resolves v2's "pinned vs bounded" contradiction for
this mode specifically. `a.Conv` keeps the pin, so compaction behavior is unchanged.

### 5.6 Interaction with existing systems

- **Compaction**: `TranscriptSize` should be measured on the projection
  (that is what's sent). Compaction still rewrites `a.Conv` as today when it
  fires; it should fire far less often. Long-run growth of base (sum of U + A)
  is still handled by compaction — answers themselves are not collapsed in
  this design.
- **Eviction**: becomes largely redundant when collapse is active; keep it as a
  fallback for `keep` turns.
- **Memory / kvr staging**: not needed for the core mechanism. Disk spill is
  the recovery path (works in all modes; staging is Docker-only and
  ephemeral). Memory remains supplemental (`retrieveMemoryContext` is keyword
  FTS, 3 entries, 500-char values — too weak as primary recovery). A keep-note
  could optionally be written to staging/mid-tier memory for cross-session
  reuse; not required.
- **Handoff**: already W-agnostic (user + assistant text only) — compatible.
  Keep-notes live in assistant-side or projection-only content; decide whether
  handoff should include them (probably yes — they are the "how to resume").
- **Persistence**: collapse decisions must be saved with the session (new
  field on `Session`, `session.go:21-31`) so `/resume` rebuilds the same
  projection.

### 5.7 Prompt cache [inferred — confirm with backend telemetry]

Prefix caches reuse up to the first changed byte. Collapsing turn N when
turn N+1 starts changes content right after U_N, so the cached tail from
A_N onward is recomputed once — but that tail is now small. Subsequent turns
are append-only again. Today's TTL eviction mutates content 1–3 turns back on
every turn, which arguably breaks the cache more often. Also: cached input is
billed at a discount on many backends, so dollar savings may be smaller than
token savings; the larger wins may be answer quality (less long-context
dilution), latency, and window headroom.

### 5.8 Keep-note placement

Projected form of a collapsed turn: `U`, then `A` with the keep-note appended
inside a delimited block, e.g.

```
[working set collapsed — 14 tool calls, 96k chars]
keep: internal/agent/compact.go:302-522 (Compact), turn_phases.go:581-602
spill: <path>, <path>
```

The note is model-authored → treat as model output (same trust as A), but any
spill paths must be validated as being under the tool-cache dir before being
trusted for automated reads.

## 6. Risks / open questions

1. **Re-read cost vs carry cost** — the core bet. Measure in P1 / the TTL=0
   experiment.
2. **Model compliance** — will the model call `context_keep` reliably and
   write useful notes? Telemetry + default policy cover non-compliance.
3. **Large answers** — a 10k plan stays in base forever until compaction.
   Possible later extension: rolling summarization of old answers.
4. **Large tool-call arguments in W** (`write_file` payloads) — collapse removes
   them; the file on disk is the truth, so this is a feature.
5. **Proxy memory path** — `compact.go:78-80` says the proxy's memory path
   ignores client-sent history. Confirm the main inference path actually
   consumes `a.Conv` (inspect `internal/proxy/client.go`); otherwise
   client-side projection has no effect on that path.
6. **Debuggability** — the user should be able to see what was collapsed
   (`/ctx` view; a dim "· collapsed turn 42 working set (96k)" note).

## 7. Phasing

- **P0 (now, user)**: run with `"tool_result_ttl": 0`; observe re-reads and
  follow-up quality.
- **P1 — shadow telemetry**: turn IDs (§5.2); per turn log |U|, |W|, |A|,
  tool count; log "would-collapse" projected size; measure re-read rate
  (files read in turn N re-read in N+1..N+3). No behavior change.
- **P2 — projection plumbing**: `contextProjection()` = identity; collapse
  records persisted in `Session`; `/ctx` shows projected vs full size.
- **P3 — deterministic policy**: `collapse-after-1` behind config flag (no
  model decision): W of turns older than the previous one is omitted, spill
  guaranteed first. Hard rules §5.4. Tests below.
- **P4 — model decision**: `context_keep` tool + keep-note; `/keep`,
  `/collapse` overrides; bias policy per telemetry.
- **P5 — tuning**: compaction measured on projection, rolling answer
  summarization, optional keep-note → memory.

## 8. Acceptance criteria

1. With the flag off: byte-identical requests to today (golden test).
2. Projection never contains a `tool_calls` message without all its `tool`
   results, or vice versa.
3. A turn with pending async ops, suspension, or undelivered inbox content
   is never collapsed.
4. `/resume` of a session reproduces the same projection.
5. `/rewind` and `/handoff` behave identically with the flag on and off.
6. Synthetic run of 50 turns (each W = 50k, A = 2k): projected size stays
   ≤ preamble + 50 × (|U| + |A| + note) + current turn; compaction fires
   ≤ 1× vs baseline count.
7. Every spill path referenced in a keep-note exists and is readable.
8. Telemetry reports re-read rate and total input tokens vs baseline.

## 9. Implementation plan v1 (deterministic collapse, no model decision)

> Superseded by the full build spec in `docs/long-session-mode-plan.md`
> (mode named `/mode standard|long-session`). The outline below is kept for history.

Scope decision (2026-10-13): skip the shadow phase and the `context_keep` tool for
now. Ship a deterministic, flag-gated collapse: the working set of every
**completed** request is archived and dropped from what's sent; U + A stay.
The model-driven partial keep comes later.

Verified for this plan: the main path sends every message it is given
(`proxy/client.go:816-934`, `marshalWireMessages` → `body.Messages`), so a
client-side projection does change what the backend sees. The single main
call site is `turn_phases.go:266` / `:301`.

Steps:

0. **Wire guard.** Add `Turn int` to `proxy.Message` with a JSON tag so it is
   persisted in session JSON, and confirm `wireMessage` (`client.go:127-133`)
   excludes it. Add a golden test: wire bytes unchanged with the field set.
   Grep for any other `json.Marshal` of `[]proxy.Message` that reaches a backend.
1. **Turn stamping.** `turnSeq` counter on `App`; `SendOutcome` stamps the user
   message; one helper `appendTurnMsg` replaces the direct
   `a.Conv = append(...)` sites in `turn_phases.go` (assistant, tool, inbox,
   budget/limit prompts) and `mashura.go`. Messages with `Turn == 0` (legacy,
   handoff seed, preamble, summary) are never collapsed.
2. **Collapse records + archive** (new `internal/agent/collapse.go`).
   At `SendOutcome` entry (before the new user message is appended),
   `collapseCompletedTurns()` finds eligible turns and for each:
   render W in full → `tools.SpillToCache(chatID, "turn-N", …)`; on failure skip
   (keep W). Store `collapseRec{Turn, SpillPath, Note, Bytes}`.
   Eligible: `Turn > 0`, has a final answer (last message is assistant text with
   no tool calls), |W| ≥ `context_collapse_min_bytes`, not kept via `/keep`,
   not a subagent, and — for the most recent turn only — no active async ops
   and no undelivered inbox content.
   Note is deterministic: tool counts + paths/queries touched + spill path.
3. **Projection.** `contextProjection()`: identity when off; otherwise for each
   collapsed turn emit U, then a copy of A with the note appended (new string,
   never mutating the shared message). Use it at `turn_phases.go:266`, the
   `ContextUsage` fallback (`ctxlimit.go:424`), and the side-question snapshot.
4. **Compaction / hard-max on what's sent.** Size checks in `Compact`,
   `enforceHardMax`, `fitConvToWindow` use the projected size. When collapse
   is on, `Compact` first materialises collapsed turns in `a.Conv` (W → note)
   before computing the boundary — compaction already clears checkpoints, so
   no new rewind regression.
5. **Persistence.** `Session.Collapsed []collapseRec`; `SaveSession` writes it;
   resume (`conversation_manager.go:327-331`) restores records and `turnSeq`.
6. **Separate mode, normal stays default.**
   - Config `context_mode`: `normal` (default) | `long`; flag `--long-session`;
     `context_collapse_min_bytes` (default 2000).
   - TUI: `/mode long|normal|status`; `/keep` (exempt the last turn).
   - Persisted per repo in RepoState like `/auto`, `/rawtools`
     (`conversation_manager.go:96-153`), so it survives `/new`, `/handoff`
     and restarts; CLI flag wins over RepoState (same explicit-flag rule as
     `--auto`/`--model`).
   - Status-line `long` segment, mirroring the `raw` segment
     (`core/sessionclient/info_snapshot.go:64-65`).
   - Mid-session switch: normal→long collapses finished turns at the next
     request; long→normal makes the projection the identity again (full
     `a.Conv` returns); `fitConvToWindow`/compaction trims if it no longer fits.
   - Subagents always run in normal mode.
   - Dim note per collapse: `· collapsed turn N working set (96k → 0.3k, archived)`.
   - Update `docs/configuration.md`, `config.example.json`, `/help`, README.
7. **Tests.** Off = byte-identical requests; projection never orphans tool
   messages; spill failure keeps W; async-pending last turn not collapsed;
   `/resume` reproduces projection; `/rewind` after collapse; compaction on
   projected size; subagent untouched; legacy `Turn == 0` untouched; handoff
   output unchanged.

Known limits of v1: `a.Conv` and the session JSON keep growing (only what is
*sent* is bounded); the tool cache has no retention yet; answers themselves are
never collapsed (long-run growth still ends in compaction, just much later).

## 10. Config draft

```json
{
  "context_collapse": "off",            // off | shadow | after-1 | model
  "context_collapse_note_cap": 1024
}
```
