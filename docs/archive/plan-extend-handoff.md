# Plan: Extend session handoff — recency-faithful context + on-demand turn retrieval

> **Status: REVISED after 2nd Mashūra round.** Round 1 (gemini-2.5-pro panel):
> verbatim-tail P1, manifest-in-context P2, user-gated `/recall` first P3.
> Round 2 (gpt-5.6-sol + claude-fable-5 + glm-5.2 panels): found concrete
> implementation-level gaps — cap anchor, feedback-loop wiring, marker spoofing,
> payload ownership, ordinal stability, wrong "lossless/matches-compact.go"
> framing. Fully folded in below. These are ORACLE OPINIONS, not ground truth;
> each must be re-verified against code at implementation time.

## Problem
Today `/handoff` produces ONE lossy summary over the **entire** transcript and that
summary is the **only** context the new session receives (`handoff.go`:
`performHandoff` → `generateHandoffSummary` → `BuildContinuationPrompt` /
`BuildHandoffContext`; `NewConversation` clears `Conv`). The new session therefore
"has way too few ctx," and — critically — the *most recent* work is compressed just
as hard as the oldest.

Two complementary desires from the user:
1. **Recency-faithful context** — the last ~30% kept closer to verbatim, older parts
   coarsely summarized.
2. **On-demand turn reference** — the new session (or user) can pull specific old
   turns into context, not just whatever the single summary kept.

## Verified current state (all file:line)
- `handoff.go:61-158` `performHandoff`: saves current `Conv`, generates one
  whole-session summary (`generateHandoffSummary` `handoff.go:162-184`), indexes it
  into session-history (`handoff.go:126-135`), builds continuation prompt
  (`handoff.go:189-203`), stores record. Delivers `HandoffMsg` (msgs.go:189-205).
- `handoff.go:196-202 / 213-220`: summary delimited as untrusted `--- BEGIN/END
  HANDOFF SUMMARY ---`, explicitly not-instructions.
- `NewConversation` (`app.go:629-644`): sets `Conv=nil` — no turn carry-over.
- TUI `HandoffMsg` handler (`tui_agent_msgs.go:513-619`): proceed → seeds first
  `RunTurn` with `ContinuationPrompt`; stop → injects `BuildHandoffContext` as a
  pinned system message.
- In-session compaction (`compact.go:51-75` `keepBoundary`, `compact.go:234-345`
  `Compact`): keeps recent turns verbatim (as live structured messages incl.
  tool calls/results), folds older into `[Summary of earlier conversation]`, with
  pinned-message + latest-user-task exemption.
- `renderIndexableTranscript` (`sessionhistory_bridge.go:603-616`): user + assistant
  TEXT only — drops tool output, tool-call args, AND system messages; strips leading
  retrieval envelopes from user text.
- Session-history index (`sessionhistory/store.go`): `Index`, `Search(query,
  workspace, excludeChatID, limit)` → `[]Result` (≤4 matched `Turn{Ordinal,Role,
  Text}` + `Summary`), `GetSummary`, `ListMeta`. Workspace-fail-closed (empty
  workspace → nil). `Turn` ordinals are **unstable** across compaction/resume
  (store.go:16-18). Handoff summary already persisted + searchable here
  (`handoff.go:126-135`).

## Design — three phases

### P1 — Recency-split handoff payload: coarse summary of older + high-fidelity recent tail
Match the *principle* of `compact.go` (summarize old, keep recent at higher fidelity,
split on user-turn boundaries) but do NOT reuse its mechanics. The handoff tail is
rendered plain text, not live structured messages.

**Definitions (resolve the round-2 contradictions):**
- **Split basis**: the `older`/`recentTail` boundary is computed on **rendered
  `renderIndexableTranscript` bytes** (what will actually be carried), NOT on
  `keepBoundary`'s raw `Content + ToolCalls[].Arguments`. Tool-heavy turns render to
  little text; measuring on raw bytes would produce a wildly variable tail.
- **30% is a TARGET, not an invariant.** Actual tail = `min(30% of rendered bytes,
  handoffTailByteCap)`; when the newest single turn alone exceeds the cap, the cap
  wins with a deterministic within-turn truncation and an explicit omission marker,
  and the latest **user request** is preserved preferentially (keep the task/redirect
  over the assistant response that answered it).
- **Tail truncation drops WHOLE turns from the oldest end of the tail** (recompute the
  boundary on rendered size), never byte-slices mid-turn — byte-slicing can amputate a
  `USER:`/`ASSISTANT:` role prefix and misattribute text.
- **Call it "high-fidelity prose tail", NOT "verbatim".** It is verbatim for
  user+assistant prose only; tool calls/results are excluded (secret-hygiene boundary)
  both from the tail and from the older-block summarizer input. This is lossy for
  *actions*. The plan must state this limitation — for coding sessions the missing
  detail is often tool output, which all three phases deliberately exclude. If that
  turns out to be what the user needs, that's a separate, larger decision (see
  Open questions).

**Payload ownership — introduce a structured result:**
```go
type HandoffPayload struct {
    CoarseSummary string // only this feeds IndexInput.Summary
    RecentTail    string // high-fidelity prose block, capped
    Manifest      string // P2; may be "" until then
}
```
Only `CoarseSummary` becomes `IndexInput.Summary` (`handoff.go:128` currently sets it
to the whole summary — must be re-pointed). The full payload goes to continuation /
pinned context and the sidecar, not to the FTS summary field. Do NOT embed nested
delimiters in any field.

**Cap strategy (round-2 correction: 4000 is the wrong anchor):**
- Add a dedicated **total handoff payload budget** with sub-budgets for coarse
  summary, tail, and manifest — individually and in total.
- Derive from the new session's effective context the way `activeThresholds`
  (compact.go:124+) does (fraction of effective window), with an absolute fallback
  (10–20k chars) for the unprobed-`CtxLimit`/startup case. Do NOT tie
  `handoffTailByteCap` semantically to `rememberFoldByteCap=4000`, which budgets a
  small folded envelope in an *ongoing* session — a different constraint than seeding
  a near-empty new session. (Exact value to confirm against real session sizes.)

**No-summarizer / failure fallback (round-2 correction):**
- If the entire **filtered transcript fits** the total payload budget → carry it all,
  skip summarization.
- If `older` block is empty → do NOT invoke the summarizer.
- Summarizer unavailable / timeout / oversized output → bounded deterministic fallback
  (capped filtered excerpt), never an unlimited `renderIndexableTranscript` dump.
- **Tail-below-min-size → whole-session summary is BACKWARDS** (it throws away exact
  recent content when it's cheapest to keep). Use the "entire transcript fits → carry
  all" rule instead.

**Harvest the existing compaction summary (round-2 bug fix):** `renderIndexableTranscript`
drops system messages, so an already-compacted session's `[Summary of earlier
conversation]` never reaches the handoff summarizer. Detect and include it as untrusted
`older` context (or use a shared conversion that returns indexed turns + harvested
prior summary together).

**Pin-awareness (round-2):** mirror `compact.go`'s pin handling — a pinned task
instruction or subagent breadcrumb in the `older` block must be carried into the
handoff verbatim, or the loss accepted explicitly. Currently the handoff split has no
pin handling.

### P2 — Turn manifest + citations (only where it earns its bytes)
**Round-2 correction: a tail-only manifest is mostly redundant** (the tail is already
present in full). The value of recall is recovering **older omitted turns**, but the
tail-only manifest doesn't list those. Refine:
- Manifest lists **whole retrievable session** ordinals with one-line snippets,
  prioritizing omitted `older` turns; OR defer the manifest entirely and ship it
  together with P3 (an in-context manifest for a `/recall` command that doesn't exist
  yet spends context, causes cache churn, and advertises unavailable behavior).
- **Not independently shippable before P3** — so the sequence becomes: P1 → P3 → P2
  (manifest rides with `/recall`), or drop in-context manifest and surface ordinals
  via the command alone.
- Shared `convToTurns(conv) (turns []sessionhistory.Turn)` helper extracted from
  `sessionToIndexInput` (`sessionhistory_bridge.go:74-119`), reused by indexer and
  manifest. Decisions needed: does it also return harvested compaction summary / taint
  / strip blocks; and it must attach assistant prose to the right ordinal consistently.
- **Ordinal stability**: build manifest AND index from the SAME `app.Session`/`app.Conv`
  snapshot so ordinals match, and treat citations as best-effort — `/resume` +
  re-index makes them stale. Verify or fail-clear in `/recall`.
- Manifest budget: total bytes + max entries + one-line control-stripped,
  marker-neutralized snippets, explicit omission notation.
- Multiple assistant messages sharing an ordinal must stay deterministically ordered
  (by insertion/id).

### P3 — On-demand turn retrieval: user-gated `/recall` first
Correct ordering (per reviews, and card #116's user-only verbatim-expansion posture):
- **`/recall <chatID> [<ordinal>|<range>]`** first — user-invoked only, byte-capped
  (reuse `recallByteCap` envelope machinery from `/remember`), workspace-fail-closed,
  framed as untrusted delimited context with citations, verbatim *indexed prose* (never
  raw tool output / tool-call args).
- Model-callable read-only tool (search/turn) **DEFERRED** — repeated autonomous
  context expansion is the higher-risk surface; only consider after `/recall` proves
  the `GetTurns` surface.
- **Resolve the current-chat contradiction (round-2):** `/recall <chatID>` is an
  explicit targeted fetch, unlike `/remember` which excludes current chat for
  feedback-loop reasons. Allow fetching the current chat in `/recall`; rely on
  strip-before-reindex for the loop guard instead.
- **New API:** `GetTurns(ctx, chatID, workspace string, from, to int) ([]Turn, error)`:
  reject empty `workspace` up front (mirror `Search`/`Index` fail-closed); enforce the
  `JOIN sessions … WHERE sess.workspace = ?` scope so wrong-workspace is
  indistinguishable from missing; define inclusive/exclusive and zero-based semantics;
  reject negative/reversed/excessive ranges; bound `to-from` and max rows in SQL (not
  only after load); deterministic ordering (by `turns.id`, not just ordinal);
  distinguish invalid input from not-found without leaking cross-workspace existence;
  use `ctx`.
- Spell out `/recall` UX: `ShortID` prefix matching (manifests/display use `ShortID`
  everywhere) + ambiguity handling, case sensitivity, large-range confirmation.
- Run the same bounded reconciliation `/remember` uses before direct `GetTurns`
  (sessions may not be indexed yet, or handoff indexing failed).

## Security / correctness invariants (round-2 — the biggest gaps were here)
1. **Wire the feedback-loop guard (THE priority).** `stripRetrievalBlock` /
   `retrievalEnvelopes` (`sessionhistory_bridge.go`) recognize only memory + session
   envelopes. The handoff markers (`--- BEGIN HANDOFF SUMMARY ---`) are NOT in the
   list — and in proceed mode `BuildContinuationPrompt` becomes the first *user*
   message, which `sessionToIndexInput` indexes. With P1 that re-indexes a high-fidelity
   tail; chained handoffs compound (tail-of-a-tail, cross-session FTS duplication).
   **Required: register the handoff / tail / manifest / `/recall` envelopes in
   `retrievalEnvelopes` with distinct markers, and strip them in the indexing input
   path, before P1 ships.** Tests must cover repeated handoffs, not just `/remember`.
2. **Marker spoofing is now live** (a verbatim tail can contain the literal end
   marker). Apply `neutralizeSessionMarker`-style neutralization to tail, manifest
   snippets, and `/recall` output; flatten snippets to one line; strip control
   characters from TUI-visible content; reserve marker bytes before truncating;
   preserve complete markers even at tiny caps; test malicious embedded markers.
3. **Framing is mitigation, not a boundary.** All retrieved/handed-off content remains
   untrusted, byte-capped, context-not-instruction. Sanitize/quote the interpolated
   `workspace` value in prompt builders (path with control chars/newlines could spoof
   framing).
4. **Secret hygiene = risk reduction, not a complete boundary.** Indexed user/assistant
   prose can still quote secrets. Keep excluding raw tool output + tool-call args, but
   don't claim a hard secret wall.
5. **Sidecar taint/perms**: with a fuller payload the 0o644 sidecar
   (`handoff.go:258`) holds more transcript material. Review 0o600, atomic write, and
   whether the continuation prompt needs duplicating there at all.
6. **Stop-mode pinned context**: if the tail block becomes pinned in stop mode, it is
   permanently compaction-exempt. Decide whether the high-fidelity block should be
   pinned at all.

## Corrections to earlier claims (from round-2)
- "Recency-split adds one more summarizer call" — FALSE (still one call; adds context
  bytes, not a call).
- "P2/P3 stay cache-neutral until invoked" — FALSE if the manifest is embedded in every
  handoff (immediate prompt/cache-prefix growth). Only P3-on-demand is cache-neutral.
- "Full transcript is saved on handoff" — only current `Conv` is saved; after compaction
  older history may already be gone.
- "Matches the proven compact.go pattern / strictly lossless" — overstated: different
  mechanics (structured vs rendered), prose-only so lossy for tool actions.

## Open questions / decisions to confirm
1. Whether the excluded **tool output** is actually what's needed for continuation; if
   so that's a separate, larger gated decision (release it to the new session? how?).
2. Exact `handoffTailByteCap` + `totalHandoffBudget` values (10–20k absolute fallback?
   fraction-of-context like `activeThresholds`?) — need real session-size data.
3. P2 sequence: ship manifest with P3 (recommended), or not in-context at all?
4. Whether `/recall` current-chat fetch is allowed (recommended yes) and large-range
   confirmation UX.
5. Where the 30% target sits vs the hard cap for atypical (very large / tiny) sessions.

## Acceptance criteria (per oracle — each phase ships with tests)
- P1: boundary computed from rendered bytes, not tool args; existing compaction summary
  survives; entire transcript carried when it fits; only `older` reaches the summarizer;
  tail chronologically after summary; total/summary/tail/manifest each within caps;
  UTF-8 valid; oversized newest turn deterministic; latest user request never dropped;
  no-summarizer/timeout/oversized-summary behaviors; embedded end markers can't escape
  an envelope; proceed & stop modes carry equivalent payload; repeated handoffs don't
  re-index prior envelopes.
- P2: manifest ordinals == indexed ordinals for the saved snapshot; multi-assistant
  same-ordinal order preserved; snippets one-line/control-stripped/neutralized/capped;
  explicit omission; stale citations after resume/re-index fail clearly.
- P3: empty workspace → nothing/fail-closed; wrong workspace indistinguishable from
  missing; range semantics deterministic; negative/reversed/huge/out-of-range rejected;
  full & short chat-ID resolution incl. ambiguity; reconciliation before direct lookup;
  SQL + final-envelope byte caps; retrieved blocks stripped before re-indexing; no raw
  tool output/args; user-invoked recall can't chain further recalls autonomously.
