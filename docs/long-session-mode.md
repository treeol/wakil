# Long-Session Mode (context collapse)

Long-session mode is the default context mode. After a request (one human turn)
finishes, its **working set** — tool calls, tool results, intermediate assistant
text, injected envelopes — is archived to disk and no longer sent to the model.
Later requests see only that request's **user message + final answer + a note**
pointing to the archive. `standard` mode opts out (full history sent).

Implementation lives in `internal/agent/collapse.go` (turn stamping, collapse
records, projection, note building, rewind pruning, memory indexing) and
`internal/agent/app_state.go` (mode state). Config keys: `context_mode`
(`standard` | `long-session`, default `long-session`; `long` is an alias),
flag `--context-mode`, env `WAKIL_CONTEXT_MODE`,
`context_collapse_min_bytes` (default 2000), `context_tail_turns` (default
20), `context_tail_answer_bytes` (default 500). The collapse note is capped
by a fixed internal constant (1200 bytes), not a config key.

## Core principle: projection, not mutation

`a.Conv` remains the complete log of record. The single model call site
(`internal/agent/turn_phases.go`) sends `a.contextProjection()`, which omits
the working set of collapsed turns. Consequences:

- `standard` mode is byte-identical to pre-collapse behavior (the projection is
  `a.Conv` itself).
- Switching modes mid-session is instant and reversible: nothing was deleted.
- `/rewind` checkpoints (`ConvLen` indices), `/resume`, `/handoff`, session JSON,
  the TUI transcript view and `ConvSnapshot` readers keep working unchanged.
- The one exception: a **forced** compaction (`/compact force` or hard-max
  fallback) materializes collapsed turns into `a.Conv`, destroying their
  archive pointers. The soft `Compact` path is a no-op in long-session mode.

Every message is stamped with a monotonic turn number (`Turn` on
`proxy.Message`, local-only bookkeeping — a golden test in
`internal/proxy/turn_wire_test.go` guarantees it never reaches the wire).

## Turn anatomy

A human turn produces:

- **U** — the user message,
- **W** — the working set: assistant messages with tool calls, tool results,
  intermediate text, injected envelopes,
- **A** — the final answer (last assistant message, no tool calls).

A **collapsed** turn is sent as U, then A capped to
`context_tail_answer_bytes` with the deterministic note appended.

## Collapse rules

At the end of every turn and at the start of every request, each not-yet-collapsed
turn becomes collapsed when all of these hold:

1. It carries a turn number > 0 (legacy messages, preamble, summary and handoff
   seed are never collapsed).
2. It finished cleanly (last message is assistant text without tool calls);
   aborted or errored turns are kept.
3. Its working set is at least `context_collapse_min_bytes`.
4. It is not in the `/keep` set.
5. This is not a subagent (subagents always run in standard mode).
6. For the most recent turn only: no active async ops, no undelivered inbox
   content, and the turn did not end suspended.
7. Archiving to the tool-cache spill file succeeded (on failure the turn is
   kept and one warning is shown per session).

Turns skipped by rules 6/7 are reconsidered at the next request.

## The note

The note is appended to A in the projection only — for partial (mid-turn)
collapse records the checkpoint block is appended to a **copy of U** instead.
It is wakil-authored (deterministic): tool-call counts by name, write targets
(read: *intent to modify*, not completed changes) / read paths parsed from
tool-call arguments, the working-set size, and the archive path. When a
`collapse_context` summary was supplied, a `[checkpoint summary: …]` wrapper
(model-authored) is spliced in beside the deterministic parts. The archive
path is host-side: `read_file` resolves it via a host-side intercept; a
sandbox shell "not found" does not mean the archive is gone.

## Mid-turn collapse: the `collapse_context` tool

The model can proactively collapse accumulated working sets at a sub-task
boundary without waiting for the turn to end (`collapse_context`, registered in
`internal/agent/mcp_manager.go`, ungated, and excluded from the model picker).
It accepts an optional summary, next steps, and a named memory stash; in
standard mode it is a no-op. Partial (mid-turn) collapse elides the prefix
between the first message and the `collapse_context` call.

## Recall and recovery

- **Working set**: archived to disk as a rendered text file (tool arguments and
  results as retained at archive time); the model can `read_file` it with
  offset/limit, or re-run the tool. Note: the archive holds the *working set*,
  not the final answer — the full answer stays in the conversation/session
  record (subject to compaction); a tail-capped answer is recovered from there,
  not from the archive.
- **Answer excerpts**: each collapse record carries a deterministic excerpt of
  the turn's final answer (pipeline-extracted, never model-authored), used by
  degraded pointers and the memory index. Partial (mid-turn) records and
  records persisted before the field existed have no excerpt.
- **Memory index**: finished-turn collapses are auto-indexed into mid-tier
  memory as best-effort (including store-unavailable cases) with a **7-day
  TTL**; an optional model-named stash (`ctx/<chat>/...` only — other
  namespaces and path-like suffixes are refused) goes alongside it. A
  once-per-session reconciliation tombstones index entries whose archive file
  has gone missing (stashes are never pruned).
- **Tail degrade**: ranking among collapsed turns only, the
  `context_tail_turns` **most recent** collapsed turns stay at full size
  (U + capped A + note); **older** collapsed turns degrade to a single
  user-role pointer line (U echo + summary/excerpt + archive/memory/stash
  pointers). `/keep`-exempt and never-collapsed turns don't consume tail
  slots and always pass through at full size. This is count-triggered, not
  window-pressure-triggered; `context_tail_turns: 0` disables it.

## Interaction with existing systems

- **Compaction**: size checks measure the projection, not `a.Conv`; the soft
  `Compact` path is a no-op in long-session mode (Layer-2 tail-cap owns the
  middle cut). `/compact` is refused with a pointer to `/compact force`.
- **Eviction** (`tool_result_ttl`): largely redundant when collapse is active;
  kept as a fallback for kept turns.
- **Handoff**: summarizes the full `a.Conv` (not the projection); collapse
  records are persisted in the session record so `/resume` and handoff rebuild
  the same projection.
- **Rewind**: truncation prunes collapse/keep entries whose turn no longer
  exists and resets the turn sequence.

## Commands

- `/mode standard | long-session | status` — switch or inspect (persisted per
  repo; the status line shows `long` / `std`).
- `/keep <n>` / `/keep off <n>` — exempt request n from collapse or remove the
  exemption (a kept turn is restored into what is sent).
- `/mode status` — collapsed/kept counts, sent vs full sizes, archive dir.

## Known limits

- `a.Conv` and the session JSON keep growing (only what is *sent* is bounded,
  and even the sent size grows with uncapped questions, kept turns and
  never-collapsed turns — the tail cap and degrade bound the *answer* and
  *collapsed-turn* components only).
- Tool-cache retention is age-based only (`tool_cache_retention_days`, default
  30, pruned once at startup in the background; 0 = never). There is no
  size-based cleanup.
- Answers in the tail are capped by `context_tail_answer_bytes` (0 disables);
  answers outside the tail are never collapsed. Very long base growth still
  ends in compaction, just much later.
- Daemon parity: mode switching over the remote facade is not supported yet.
