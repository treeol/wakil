# Card #129 — Cross-session async delivery/grounding

Trello: https://trello.com/c/bnkJCdoA (bug).

## Problem (verified 2026-08-10 at HEAD)
- `App.NewConversation` (app.go:652) resets `Conv`/`ChatID`/`Session`/`preambleDay` but does NOT
  touch `asyncInbox`/`asyncOps`.
- `drainAsyncInbox` (async_ops.go:1049) delivers EVERY queued completion into the CURRENT Conv and
  calls `commitAsyncEffects`→`commitAsyncGrounding`→`addExternalGrounding` on the current Client —
  no session-origin check.
- `handleCheckPending` single-op retrieval (async_ops.go:1227) does the same: `commitAsyncEffects`
  unconditionally.
- So an old-session Mashūra result completing after /new is injected into the new Conv and its
  oracle grounding is committed against the NEW session — wrong provenance.
- Card #126 already stamped `originChatID` on every asyncOp (metadata only, read by no delivery
  path). No migration needed.

## Decision (Mashūra decide op-7 — unanimous Option A)
**Deliver-with-provenance + suppress grounding.**
- Still deliver the result inline into the current Conv, but TAG the envelope so the model/user
  knows it belongs to a prior session (originChatID).
- SUPPRESS grounding for cross-session results: an old-session oracle answer must NOT be grounded
  into the new session (`commitAsyncGrounding` skips when `op.originChatID != a.chatID()`).
- Cost is always committed at worker terminal (unchanged — never lost).
- In-session completion behavior unchanged (exactly-once delivery preserved).
- Rejected alternatives: B (quarantine — degrades UX, hides a paid result), C (current buggy),
  D (ground into owning session — no mechanism; NewConversation discards old session live state).

## Implementation
1. `opIsCrossSession(op)` helper = `op.originChatID != "" && op.originChatID != a.chatID()` (empty origin →
   in-session fallback, preserving pre-fix behavior). `originChatID` documented immutable after
   registration (write-before-publish), read without op.mu.
2. `commitAsyncGrounding(op)`: set `groundingCommitted`, then if cross-session — set `touchedExternal`
   (untrusted oracle content is still delivered/consumed, so taint is preserved) but do NOT add the
   oracle grounding entry to the new session.
3. `commitAsyncSubagentEffects(op)`: for cross-session discovery-subagent batches, skip the per-child
   `addExternalGrounding` loop (still set `touchedExternal`), but keep SubagentDoneMsg events + LSP
   bookkeeping.
4. `renderAsyncLine`: tag cross-session envelopes with a `> prior-session result ` prefix.
5. Both drain and check_pending route through commitAsyncEffects/commitAsyncSubagentEffects →
   suppression applies on both paths (and shutdown).

## Files
- `internal/agent/async_ops.go` — helper, grounding suppression (oracle + subagent), render tagging.
- `internal/agent/cross_session_async_test.go` — tests.

## Review notes (op-8 folded)
- Subagent per-child grounding path was a MISSED suppression site — fixed in commitAsyncSubagentEffects.
- Taint signal (`touchedExternal`) preserved for cross-session delivery even though the grounding entry
  is suppressed.
- Empty-origin fallback tested (in-session). Mixed-drain, in-session check_pending, cross-session
  subagent, and taint tests added.
- Stale "read by nothing" comment on originChatID corrected.
- check_pending retrieval untagged on purpose: the user explicitly requests by op id, so the id IS the
  provenance; the inline drain path carries the tag.

## Test requirements
- Old-session op completing after /new → delivered inline, envelope tagged cross-session, grounding
  NOT committed to the new session.
- In-session op → delivered + grounded normally (exactly-once preserved).
- check_pending(op-N) across rotation returns the result, suppresses grounding, stays coherent.
- Cost committed exactly once regardless of rotation.

## Invariants preserved
- Cost committed at worker terminal (never lost). Workers never write Conv. Exactly-once delivery.
- API keys env-only.
