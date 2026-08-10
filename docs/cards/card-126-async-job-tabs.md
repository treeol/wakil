# Card #126 — Phase 1 Implementation Plan: Async-job tabs (Mashūra) in TUI

Author: main agent. Status: plan, pre-implementation. Reviews: 2026-08-10 Mashūra
feasibility review (attached on the Trello card).

## Scope decision (VERIFIED against current code)
- The unified `asyncOps` registry (`internal/agent/async_ops.go`) carries async
  operations. CONFIRMED: Mashūra counsel ops AND detached-shell completions both
  flow through it:
  - Mashūra: `runMashuraCore` → `enqueueAsyncOp(name, panelName, ...)` (mashura.go:235).
  - Detached shells: `notifyDetachedShellExit` (tool_handlers.go:295) builds a
    `job-<bgID>` asyncOp and appends it to `asyncInbox` directly (not via
    enqueueAsyncOp). So shells DO create asyncOps — the earlier "shells NOT in
    asyncOps" premise was WRONG and is corrected.
- **Phase 1 covers Mashūra async ops as tabs.** Detached-shell completions bypass
  publishAsyncOp (they append to asyncInbox directly in notifyDetachedShellExit),
  so they need their own emission path — **shell tabs are a separate follow-up**,
  NOT implemented in Phase 1. Subagent-batch ops (uiJob=false) keep their
  per-child subagent tabs (no double tab).

## Session-rotation policy (decided with Mashūra: option (a) + (c))
- (a) TUI clears `subTabs` on `NewConvMsg`/`HandoffMsg`. Delivery untouched.
- (c) Stamp `originChatID` on each asyncOp at registration (metadata-only, read
  by nothing in the delivery path) so the separately-filed cross-session issue
  can be fixed without a registry migration.
- CONFIRMED pre-existing behavior (out of scope, filed separately): App.NewConversation()
  does NOT clear asyncInbox, so an old-session op completing after rotation
  drains into the new conversation and grounds there. This card does NOT change
  that. Cross-session async delivery/grounding is filed as a separate issue.
- The earlier "Start re-creates a fresh tab after rotation" claim is FALSE —
  Start is emitted once at enqueue. After rotation a still-running op's later
  Done finds no tab AND no new Start → no tab (accepted policy; display-only).

## Design: reuse the existing subTab machinery (lowest risk)
Do NOT build a second parallel tab bar or a generic task-tab refactor. Extend the
existing `subTab` (internal/tui/tui.go:289) with two optional fields for async
jobs and route new async-job events into the SAME `subTabs` slice, reusing
prune/nav/tab-bar/30s-auto-close entirely.

### New/changed messages (internal/agent/msgs.go)
```go
// AsyncJobStartMsg opens a generic async-job tab (Mashūra panel or detached shell).
type AsyncJobStartMsg struct {
    OpID     string // op-N or job-<bgID> (NOT chatID; async ops have no chatID)
    Label    string // human label (panel name / shell command digest)
    ToolName string
    OriginChatID string // for TUI provenance; "" for detached-shell reaper path
}
// AsyncJobDoneMsg terminalizes an async-job tab.
type AsyncJobDoneMsg struct {
    OpID      string
    Label     string
    ToolName  string
    Result    string // bounded preview (≤ asyncJobTabPreviewMaxBytes), marker-neutralized
    Err       string
}
```

### subTab extension (tui.go) — explicit kind, not opID != ""
```go
type subTabKind uint8
const (
    subTabSubagent subTabKind = iota
    subTabAsyncJob
)
```
- Add `kind subTabKind` (default 0 = subagent) and `opID string`.
- Identity: router matches by `chatID` for subagent tabs, `opID` for async-job
  tabs. A tab has exactly one identity (kind drives which key is used).

### subTabCloseMsg generalization (tui_msgs.go)
- Add `OpID string` alongside `ChatID`. Handler matches
  `(ChatID != "" && t.chatID == ChatID) || (OpID != "" && t.opID == OpID)`.
- Existing SubagentDoneMsg path still sends `subTabCloseMsg{ChatID}` — no
  behavior change for subagent tabs; existing tests unaffected.

### Agent side — race-free Start-before-Done (blocker 1 fix)
- **Start emission seam:** add a `uiJob bool` flag on asyncOp, set true ONLY by
  the Mashūra path (explicit metadata, NOT a toolName prefix check). In
  `runMashuraCore`, after `enqueueAsyncOp` returns, `a.sendEvent(AsyncJobStartMsg{...})`.
  To make Start-before-Done structural (not timing-dependent), `enqueueAsyncOp`
  is split: register + send Start happen BEFORE the worker goroutine is
  launched. Belt-and-suspenders: the TUI `Done` handler CREATES a terminal tab
  when none exists (handles Done-before-Start even if ordering slips).
- **Done emission:** in `publishAsyncOp`, on winning the `op.published` CAS,
  snapshot id/label/toolName/result-preview/err under op.mu, then AFTER
  releasing asyncMu send `AsyncJobDoneMsg` (only when `op.uiJob` OR
  `op.toolName == "run_shell"` — detached-shell reaper ops need Done too, and
  they don't set uiJob). Never sendEvent while holding a lock.
  - Subagent-batch ops: `uiJob=false`, toolName != run_shell → no AsyncJobDone
    (they keep per-child SubagentDoneMsg; no stray async tab).
- **Preview bound:** `asyncJobTabPreviewMaxBytes = 8000` BYTES (not runes),
  UTF-8-safe truncation via existing `truncateUTF8`, `neutralizeAsyncMarker`
  applied, no large work under op.mu (snapshot strings under lock, format
  after unlock). Result shown whether or not Err is set (error + diagnostics
  both surfaced, matching renderAsyncLine behavior).

### TUI side (tui_agent_msgs.go)
- `case agent.AsyncJobStartMsg`: upsert by opID (idempotent — duplicate Start
  ignored); build `subTab{kind: subTabAsyncJob, n: subSeq, task: msg.Label,
  opID: msg.OpID, active: true, buf}`; append, prune, remap subCur, reflow if
  first tab.
- `case agent.AsyncJobDoneMsg`: find by opID; if found → done/finished, set
  finErr on Err AND append bounded Result to buf; arm 30s auto-close via
  `subTabCloseMsg{OpID}` (same one-shot skip-if-focused semantics). If NOT
  found → create a terminal tab (Done-before-Start safety).
- **Pulsing dot while idle (review finding 3):** change `dotTickMsg` re-arm
  condition to `state != stateIdle || hasActiveJobTab()`. `hasActiveJobTab()`
  checks any tab with `kind==subTabAsyncJob && !done`. AsyncJobStartMsg starts
  the tick (or restarts it) so detached jobs pulse while main is idle; tick
  self-terminates when neither condition holds.
- Tab bar / prune / nav / dot glyph / mouse-close: reused as-is (async tab
  renders dot + label + task; × closes when done).

### Session rotation (option a) — clear tabs + reflow
- In `NewConvMsg` and successful `HandoffMsg` handlers: set
  `m.subTabs = nil; m.subCur = -1` and `m = m.reflow()` (reclaims the tab-bar
  row when the old list was non-empty). Mirror the existing clearing pattern in
  those handlers.

### Done-before-Start ordering (test #1)
- Agent: Start is sent before the worker goroutine launches. TUI: Done with no
  matching tab creates a terminal tab. Both directions covered → no permanent
  running tab under any ordering.

## Acceptance criteria mapping
- Mashūra async op → one tab within one event-loop cycle (Start → handler).
- Running tab = pulsing dot; done = ✓/✗ (reused dot spec).
- Done tab shows the bounded result (buf), independent of model drain.
- 30s after Done, unfocused tab removed; focused skipped (matches subagent).
- `check_pending(op-N)` unaffected — tab close never touches the registry.
- Exactly-once model delivery / grounding / cost unchanged (events are
  display-only; no `envelopeDelivered`/effects touched).
- Session rotation clears tabs.
- Shell tabs: explicitly out of Phase 1 scope.

## Tests
- TUI: async start → chunk(none) → done → final content; focused-skip at 30s;
  close-by-opID; no-op when Done arrives with no matching tab (subagent batch /
  stale); NewConv clears tabs. Mirror subtab_autoclose_test.go structure, with
  an injectable/shortened timer or direct subTabCloseMsg dispatch (no 30s sleep).
- Agent: Start emitted before worker launch (structural ordering); Done emitted
  exactly once from publishAsyncOp for uiJob / run_shell ops.
- Regression: existing subtab tests still pass (close handler keyed by ChatID).

## Acceptance criteria mapping
- Mashūra/detached-shell async op → one tab within one event-loop cycle (Start
  → handler; Done-before-Start also creates a terminal tab).
- Running tab = pulsing dot even while main TUI idle (tick driven by active tabs).
- Done tab shows the bounded result (and error diagnostics on failure),
  independent of model drain.
- 30s after Done, unfocused tab removed; focused skipped — one-shot (matches
  subagent).
- Closing a tab does not mutate delivery flags or directly remove the op from
  the registry; normal retention/retrieval (check_pending / eviction) unchanged.
- Exactly-once model delivery / grounding / cost unchanged (events display-only).
- Session rotation clears tabs (option a); cross-session async delivery left
  pre-existing and filed separately (option c stamps originChatID metadata).
- Shell tabs: OUT of Phase 1 (reaper bypasses publishAsyncOp); follow-up.

## Out of scope (Phase 2, separate)
- Live Mashūra streaming (counsel.RunPanel chunk callback + SSE) — requires
  counsel-layer changes; independent.
- Shell (detached run_background) tabs — reaper bypasses publishAsyncOp; needs
  its own Start/Done emission path; follow-up.
- Cross-session async delivery/grounding (option (b): drop/quarantine old ops
  on rotation) — pre-existing, filed as separate issue; this card is display-only.
- Cancellation of a running job by closing its tab (defined as display-only close;
  op continues).
