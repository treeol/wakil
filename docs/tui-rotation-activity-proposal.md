# Proposal: Visible activity indicator for rotation operations (/handoff, /resume, /new)

> IMPLEMENTED (phase 1) — 2026-09-24. See "Resolved questions" below for the
> two UX decisions folded in during implementation, and the corrected
> acceptance criteria (AC1/AC3 reworded after the second Mashūra plan review).

## Problem

Long-running rotation operations leave the TUI looking frozen — and on the
current (wiring) path the situation is **worse than sporadic notes**:

- During rotation, `m.rotating = true` (internal/tui/tui_wiring_loop.go:111-126)
  blocks sends and suppresses domain events, but **nothing renders differently**.
- The always-present status dot (`renderStatusDot`, internal/tui/tui_view.go:~899)
  only pulses for streaming/compacting/waiting states. Rotation is not an
  `agentState` at all — during rotation the dot sits dim/static.
- **Zero feedback on the wiring path**: `HandoffConversation`
  (internal/wiring/conversation_manager.go:377-447) calls
  `agent.RunHandoffPipeline` (internal/agent/handoff.go:100), which emits
  **no progress events**. Only the legacy `performHandoff` Cmd path emitted
  `SysNoteMsg` stage notes; the TUI no longer uses that path. A `/handoff`
  runs its summarizer pipeline (potentially ~2 minutes) with a completely
  static screen.
- **Fresh-state invisibility**: `statusVisible()`
  (internal/tui/tui_view.go:349-351) returns false when there is no prior
  turn, no streaming, and no items — exactly the state of `/resume` from the
  picker or `/new` — so even a perfect indicator would not render today.

## Proposal (v2)

Phase 1 only: pulsing dot + operation label + elapsed timer. **No stage
plumbing** — see "deferred" below.

### 1. Rotation-aware status zone

- Add `rotationStart time.Time` and `rotationKind rotateKind` to `tuiModel`;
  set both where `m.rotating = true` is set (tui_wiring_loop.go:108-127,
  three sites); clear both in `applyRotation` (tui_wiring_loop.go:139).
- Extend `statusLineInput` (the struct consumed by the pure
  `statusSegments()` function) with `rotating bool`, `rotationKind`, and
  `rotationElapsed`; populate in `buildStatusInput`. Rotation takes
  **precedence over `agentState`** in rendering — `rotating && streaming`
  is a real combination (failure-path comment in applyRotation contemplates
  a turn completing mid-rotation), and the rotating label wins.
- `renderStatusDot` takes an `agentState`, not the model, and has two call
  sites — it needs a signature change (or a `busy bool` override parameter)
  so both sites pulse during rotation.

### 2. Arm the animation tick (known blocker, not a "verify")

`dotPhase`'s tick **self-terminates when idle** (dotArmed reset on
AgentDone; re-arm predicate consults turn state and active job tabs only).
Rotation happens between turns → without arming, NOTHING animates and the
elapsed timer never updates. Required:

- Arm the tick (`dotArmed = true`) when setting `m.rotating = true`
  (guard against double-arm with a sequence counter, same pattern as
  existing `armSeq`/`refreshSeq`).
- Extend the re-arm predicate to `|| m.rotating`.

### 3. Make the zone visible during rotation

Add `|| m.rotating` to `statusVisible()`. This flips status height 0→N at
rotation begin and N→0 at apply — the P3-class reflow change. Therefore:

- In `applyCommandResult`, wrap the `cr.Rotate` switch in the same
  `before := m.statusRows(); …; m = m.reflowIfStatusHeightChanged(before)`
  pattern the `cr.Submit` path already uses (synchronous reflow — the 200ms
  tick is not guaranteed to fire before the first frame).
- In `applyRotation`, move the `before := m.statusRows()` snapshot **above**
  `m.rotating = false` (currently the first line is the flag clear, so the
  failure-path snapshot would already reflect the post-rotation layout and
  the shrink would go undetected). Success path is already safe (it calls
  `m.reflow()` unconditionally).

### 4. Display format

`• handing off… 12s` / `• resuming session… 4s` / `• starting new session… 1s`

Pulsing dot (existing shades/cadence) + verb + elapsed seconds from
`rotationStart`, re-rendered by the 200ms tick. No fake percentage.

## Explicitly deferred (Mashūra concurrence)

- **Handoff stage labels** ("generating summary…"): would require a progress
  callback on `RunHandoffPipeline` → `sessionclient.ConversationManager`
  interface change → stale-stage generation guard → tests. Three packages of
  signature churn for marginal value — the summarizer step dominates wall
  time, so a generic label conveys ~90% of the value. Revisit only if users
  still report "frozen" after phase 1.
- **Rotation timeout** (pre-existing): `beginRotation` passes
  `context.Background()` and `RunHandoffPipeline` does not add the 120s
  timeout its legacy path had — a hung backend can block rotation
  indefinitely with all sends rejected. The elapsed timer will at least make
  this *visible*. Fix is a separate change.

## Acceptance criteria (as corrected in the second plan review)

1. `/handoff`, `/resume <id>`, `/new` each show a pulsing dot + verb label
   + elapsed seconds **immediately after the command result applies** (View()
   runs after every Update — no tick needed for the first frame); the
   *animation* (phase advance, elapsed increment) is tick-driven (200ms) —
   including from a fresh splash state (status zone forced visible).
2. Dot and timer animate for the full duration of the rotation.
3. Rotation-only UI disappears on success AND failure; the viewport height
   stays in sync with the rendered status zone (effective rows) on both
   paths — no drift, no stale rows. ("Height returns to pre-rotation value"
   was unsatisfiable: a fresh-state failure leaves status visible via the
   error item, and a successful `/new` legitimately collapses the zone.)
4. If a turn is somehow in flight during rotation, the rotating label wins.
5. Existing rotation tests extended: begin renders label; failure resets it.

## Resolved questions (implementation decisions)

- **Blocked sends**: plain-send during rotation now prints a deduped
  "· rotation in progress — please wait" note (one per rotation, via
  `rotationNoteShown`); slash commands keep their existing "command ignored"
  note. ctrl+c semantics unchanged (arms quit as at idle) — out of scope.
- **First frame**: tick-driven rendering accepted (≤200ms for animation;
  first frame is actually immediate since View() runs after Update). The
  synchronous `reflowIfStatusHeightChanged` around the rotation begin/apply
  paths is what keeps the viewport in sync — NOT the tick.
- **Reverse search**: the search prompt still owns the status row during
  rotation (dot pulses there, verb label not shown). Explicitly accepted —
  search is transient and user-initiated.
- **Entry points**: ALL four rotation sites now go through one `beginRotate`
  helper — including the resume picker, which previously never set
  `m.rotating` at all (pre-existing gap: sends were not blocked during
  picker-initiated resume; fixed as a side effect).
- **Reflow snapshots** use `effectiveStatusRows()` (visibility-aware), not
  `statusRows()` — `reflowIfStatusHeightChanged` compares against
  `effectiveStatusRows()`, so the visibility-blind variant would never
  detect the fresh-state 0→N flip.

## Out of scope

- Startup spinner (`loading.go`) reuse — different surface.
- Determinate progress %.
- Rotation timeout fix (separate change, noted above).
