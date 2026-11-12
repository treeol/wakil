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
20), `context_tail_answer_bytes` (default 0 = prior answers kept whole). The collapse note is capped
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

## Per-round value pruning (`context_prune`, default `off`)

A second, orthogonal mechanism: **value-based pruning of tool rounds**,
behind the `context_prune` switch (Wakil 0.7 feature). Pruning is
experimental: it is off by default, and in our own tests it gave no
reliable saving on large-context models. This release makes it safe to
turn on: marks take effect only after three further tool steps, reading a
file again after marking it locks the file for the rest of the session,
large drops need a stated reason, and thin stub notes are rejected.
Apart from superseded rounds and the emergency level (≥ 90% of hard max),
results are pruned only following a model rating (`context_mark`,
`prev`/`[[…]]` tags, or the optional rating pass), so how much gets
pruned still depends on the model. Pruning composes with collapse — it
only prunes rounds that are NOT inside a collapsed turn.

- **Round**: one assistant message that has tool_calls, plus the tool-result
  messages answering those calls. Every round gets a stable id `r<N>` from
  a per-chat counter, assigned when the assistant message is appended
  (`appendConv`). The id, rating, stub text and prune state persist with
  the session (`Session.PruneRecords`); on rewind, records of removed
  rounds are deleted.
- **Rating**: the model rates the PREVIOUS round with exactly one tag in
  its next assistant message: `[[keep]]`, `[[drop]]`, or
  `[[stub: <one line, max 200 chars, what mattered>]]`. Parsed with one
  regex anywhere in the assistant content (which may sit alongside
  tool_calls); the last tag wins; a missing or invalid tag leaves the round
  unrated (treated as keep everywhere except the superseded/emergency
  cases).
  Tags are stripped from the stored content before `appendConv`, so
  neither `Conv` nor the wire ever contains them.
- **Trigger**: inside the tool loop, after tool results are appended and
  before every subsequent model request is built (it also runs once per
  request alongside collapse in `finalizeTurn`). When the projected
  request size exceeds `context_prune_at` (default 0.70) × the existing
  hard max, prune candidates until the size is ≤
  `context_prune_target` (default 0.55) × hard max, or no candidates
  remain.
- **Order**: `drop` rounds first (oldest first), then `stub` rounds
  (oldest first), then superseded rounds, then at the emergency level the
  oldest unrated ones. `keep` rounds are never pruned; unrated rounds are
  only pruned when superseded or at the emergency level.
- **Fixed rules**: the `context_prune_protect` (default 3) most recent
  rounds are never pruned — except in `immediate` mode (default; see
  `context_prune_marks`), where a round marked drop/stub via `context_mark`
  is pruned regardless of the trigger once the mark has taken effect (see
  **When a mark takes effect**), and the last-N protection does not apply to
  it (the model explicitly decided).
  A round with write-class tool calls (the same
  classification as the collapse note's "write targets") rated `drop` is
  pruned as a stub instead: `[rN write round pruned: <tool> <paths>; archive: …]`.
  Rounds in turns exempted by `/keep` are never pruned. Rounds already
  inside a collapsed turn are left to collapse.
- **Stub line**: each tool-result message of a pruned round has its content
  replaced in the projection only — assistant messages, tool_calls and
  ids are untouched, so call/result pairing is always valid:
  - drop: `[rN pruned (drop): <tool> <short target> — full result: <archive path>]`
  - stub: `[rN pruned: <model's stub line> — full result: <archive path>]`
  - superseded: `[rN superseded: <cause — reread/edited/written path or
    same command> — full result: <archive path>]` (automatic, no model
    rating; only when the round's results total at least 2048 bytes)
  - emergency: `[rN auto-emergency: <tool name> (<size>) — full result:
    <archive path>]` (automatic at the emergency level, no note)
- **Archive**: the full round (assistant message incl. tool_call ids + all
  results) is written with the existing `SpillToCache` as `round-<N>`.
  If the archive write fails, the round is NOT pruned; one warning is
  shown per session.
- **Projection only**: `a.Conv` keeps the originals. Compaction and
  materialization behave the same as collapse; after materialization the
  stubs simply become the history. Recovery: the model can `read_file`
  the archive path from the stub. The only new tool is `context_mark`
  itself.
- **Visibility**: a TUI line per pruned round (`· pruned r12 (drop, 14 KB)`);
  `/mode status` adds the rated counts (keep/drop/stub/unrated), the
  pruned round count and the bytes saved.
  `marks N` counts `context_mark` calls and persists with the session.
  `prev missing N` counts assistant replies with tool calls that carried no
  `prev` (replies made only of `context_mark` calls are not counted);
  `unrated N` counts rounds no channel has rated, so the two differ.
- **When a mark takes effect** (`context_prune_marks: immediate`): a round
  marked drop or stub with `context_mark` is pruned at a request build only
  after **three** further real rounds (rounds made of non-`context_mark`
  tool calls) have been recorded after the mark. A message made only of
  `context_mark` calls therefore prunes nothing by itself: if the final
  answer follows, it is written with the full results (pruning there would
  save nothing, because no step follows, and the answer would be written
  from the notes instead of the results). In a mixed message such as
  `[context_mark, read_file]` the `read_file` round is the first of the
  three, so the marks apply at the first request build after two more
  real rounds. Marks still pending when the request ends stay recorded
  (`mark_seq` in the round record, persisted with the session and handoff)
  and apply under the same rule in a later request, unless collapse has
  replaced those rounds by then. While a mark is pending the marked result
  is sent exactly as before the mark (with its reminder line if the round was
  unrated, without one if it already carried a rating), and the size trigger
  does not apply it either. One exception: at the emergency level (≥ 90% of
  hard max, where Wakil would stub unmarked rounds without a note) pending
  marks are released first, drops before stubs, so no unmarked round is
  pruned while a round the model marked is held. The tool result of a lone
  mark says: `marked: drop r1 · stub r2 · takes effect after three further
  tool steps`. `on-trigger` mode is unchanged: marks apply at the trigger.
- **Stub notes**: a stub replaces the result, and afterwards the model has
  only its note. When `context_mark` stubs a round whose results total 4 KB
  or more, the note must be at least `clamp(result bytes / 64, 200, 1200)`
  characters long, and for a round that read or wrote several files it must
  contain the base name of each of those files. A note that fails is rejected
  for that round only: the round stays as it was, the other rounds of the
  same call are processed normally, and the tool result names the round and
  the reason, for example `r8 not stubbed: note too thin for 38 KB across 4
  files (32 characters, at least 608 needed). A stub replaces the result;
  afterwards you only have your note. Write the facts you will still use
  with file:line, per file (missing: authz.go, web.go), or drop it.` Drops
  and rounds under 4 KB are not checked, and the never-inflate rule still
  applies afterwards. `context_mark` notes are stored up to 4000 characters
  (the `prev`, `[[stub: …]]` and rating-pass channels keep their 200); the
  note is checked as stored, so a file name beyond 4000 characters does not
  count. Every result of a round is replaced by the same stub, so a
  `context_mark` note longer than 200 characters is counted once per result
  by never-inflate (shorter stubs compare one stub with the round total, as
  before). The
  limits are constants, not config keys. The check only decides whether the
  model's own stub is accepted; Wakil never writes or extends a note.
- **Reminder**: while pruning is on, Wakil appends one fixed reminder to the
  end of every live user prompt in the request projection (the first user
  message of each request; injected user-role messages such as async results
  and budget footers, collapsed turns, collapse pointer lines and handoff
  seeds are skipped). The stored conversation, session file and TUI keep the
  user's original text. The reminder is now three sentences (296 chars):
  `[Wakil context upkeep] Tool results are tagged [rN] and are re-sent in full
  on every step until you mark them. When you are done with a result, mark it
  with context_mark: drop it, or stub it with the facts you still need, with
  file:line. Do this while you work, and keep what you are still using.` It is
  byte-identical on every request, so it does not disturb the prompt-cache
  prefix. It only reminds: the model alone decides what is kept, stubbed or
  dropped (apart from superseded rounds and the emergency level), and
  unmarked rounds are never pruned by size or age; superseded rounds are
  stubbed when the trigger runs, and only the emergency level stubs other
  unmarked rounds. There is no config key; it follows `context_prune`.
- **System prompt section**: while pruning is on, the preamble carries its own
  `## Context upkeep: pruning is on` section as the **first** section of the
  system prompt — before the agent operating instructions, in full mode and
  in core mode alike — instead of after them. Full mode puts it (1126 chars,
  measured) in front of the 51179-char operating instructions, core mode in
  front of the 17183-char core prompt. It says that `[rN]` results are
  re-sent in full on every step until marked, and asks the model to mark
  finished results while it works (finished searches, files read to find one
  thing, replaced test output), that marking is part of the task, and to keep
  results still being worked with or cited. One sentence says when a mark
  takes effect and follows `context_prune_marks`: after three further tool
  steps (`immediate`), or when the context fills up (`on-trigger`).
  The `context_mark` tool description is now the one place that keeps the
  cautions: drop/stub/keep meanings, that a stub replaces the result so only
  the note remains and carries every fact still needed per file with
  file:line (a thin note for a large result is rejected), that reading a
  file again after stubbing it costs more than keeping it (keep what you
  will still cite and cannot carry in a note), and that marking right before
  the final answer saves nothing. The sentence about a later rating pass
  appears only when `context_prune_rater` is not `"off"`. `/prune on` and
  `/prune off` refresh the preamble on the next turn.
- **Backlog line and reread line (tool results)**: while pruning is on, each round's last tool result may carry one fixed line, in the request
  projection only, computed once when that round is recorded (a round
  carries one only while it is the newest at recording time, so old
  backlog lines accumulate on older results):
  `[context · unmarked for 3+ steps: r4 (~10k tokens), r5 (~4k tokens) · if you
  are done with one, context_mark it: stub (only your note remains: facts
  with file:line) or drop. Keep what you will still cite]` when rounds
  unmarked and at least 3 real rounds old exist (the same exclusions as
  before: rated, pruned, `/keep`, pinned, collapsed, `context_mark`
  results, too small for never-inflate). At most 6 rounds are named,
  largest first, then `+N more`. Because the line is stored on the record
  at recording time, it never changes afterwards, even if a listed round
  is marked later. A round that rereads a path an earlier model-marked
  pruned round had read carries instead the reread line:
  `[reread · you dropped this file in r8 and are reading it again. It now
  stays in context and can no longer be marked]` (`stubbed` for a stub).
  A reread never follows a superseded/emergency prune by Wakil
  itself, and a round with the reread line never also carries the
  backlog line. Neither line is stored in the conversation, written to
  the archive, or shown in the TUI; both are absent with prune off.
- **Reread lock**: a file the model marked (drop or stub, any channel) and
  then read again is locked for the rest of the session. A later drop or
  stub on any round carrying that file is refused through every channel
  (`context_mark`, `prev`, `[[drop]]`/`[[stub: …]]` tags); `keep` still
  works, and other rounds in the same `context_mark` call are processed
  normally. The refusal reads `r12 not marked: <path> was marked in r8
  and read again in r12. It stays in context. Other results in this round
  stay with it.` Locked rounds never appear in a backlog line. The lock is
  keyed by cleaned, working-directory-resolved path, so `./a.go`, `a.go` and
  the absolute path are one file. The lock survives session save, resume and
  handoff (persisted like the other prune state), and `/mode status` shows
  `locked N`. Wakil-only prunes (superseded, emergency, auto) never lock.
- **Large drops need a reason** (`context_mark` `drop_reason`): dropping a
  round whose results total 4 KB or more without a `drop_reason` of at
  least 15 characters is refused for that round: `r3 not dropped: 22 KB.
  Say in drop_reason why nothing in it is needed any more, or keep it.`
  Small drops are unaffected. The reason is stored on the round record and
  shown in the dropped round's stub line (`[r3 pruned (drop: <reason>): …]`).
- **Mixed messages**: an assistant message that mixes `context_mark` with
  other tool calls registers a normal round for the other calls (so their
  results get an `[rN]` id and can be marked); only the `context_mark` call
  itself is excluded. A message made only of `context_mark` calls registers
  no round.
- **Off**: with `context_prune: off` (the default), nothing is parsed or
  stripped — behaviour and wire bytes are identical to 0.6.5 (golden-tested).

Config keys: `context_prune` (`off` | `on`, default `off`),
`context_prune_at` (0.70), `context_prune_target` (0.55),
`context_prune_protect` (3), `context_prune_marks` (`immediate`, default),
`context_prune_at_tokens` (0 — fractions only; set both token keys to
enable the absolute trigger), `context_prune_target_tokens` (0). Effective
trigger/target in chars are `min(tokens × charsPerToken(4), fraction ×
hard_max)` when both token keys are set, so a backend whose effective hard
max is large can trigger at its absolute tokens even when the fraction
never fires there, and a backend with a small effective hard max takes
the min. Validated `0 < target < at < 1`, `protect >= 1`, and, when both
token keys are set, `0 < target_tokens < at_tokens`.
- `context_prune_rater` (`"off"`): model for the one-shot rating pass that rates unrated rounds when pruning is triggered — a separate request, recorded in cost totals, never appended to the transcript. `context_prune_rater_max_rounds` (`40`) caps how many rounds one pass rates; `context_prune_rater_timeout` (`60`s) bounds it. `/mode status` shows `passes N`; `/prune status` shows the rater model.

## Commands

- `/mode standard | long-session | status` — switch or inspect (persisted per
  repo; the status line shows `long` / `std`). With pruning on,
  `/mode status` also shows `prune: on · keep N · drop N · stub N ·
  unrated N · pruned N · locked N · saved ~M KB` plus the `marks N`, `passes N` and
  `prev missing N` counts.
- `/keep <n>` / `/keep off <n>` — exempt request n from collapse or remove the
  exemption (a kept turn is restored into what is sent). Kept turns are
  never pruned either.
- `/prune on | off | status` — switch context pruning on or off for this
  chat (or inspect the effective setting). `/prune status` shows whether it
  is on, the marks mode and the rating-pass model.

## Known limits

- `a.Conv` and the session JSON keep growing (only what is *sent* is bounded,
  and even the sent size grows with uncapped questions, kept turns and
  never-collapsed turns — the tail cap and degrade bound the *answer* and
  *collapsed-turn* components only).
- Tool-cache retention is age-based only (`tool_cache_retention_days`, default
  30, pruned once at startup in the background; 0 = never). There is no
  size-based cleanup.
- Answers in the tail are kept whole by default; `context_tail_answer_bytes` > 0 opts in to a cap;
  answers outside the tail are never collapsed. Very long base growth still
  ends in compaction, just much later.
