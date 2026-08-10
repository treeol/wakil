# Card #126 — Phase 2 Implementation Plan: Live Mashūra progress/status streaming

Author: main agent. Status: plan, pre-implementation. Phase 1 done (commit f8e15a0).
Mashūra decision (2026-08-10, 3 panels unanimous): option (b) — progress/status
streaming, NOT full token SSE.

## Scope (decided with Mashūra: option b)
Full token streaming (option a) requires two provider-specific SSE parsers, is
impossible for fusion per-member (single server-side call), and rewrites the
paid-call path whose exact-usage billing is a stated invariant. It is a separate
follow-up. Phase 2 adds LIVE per-member STATUS to the async-job tabs: the tab
shows each panel member as it starts and completes ("calling claude-x…",
"claude-x done"), updating live, with the final full answer still appearing at
completion (Phase 1 behavior preserved). This is meaningful "live content"
without touching provider protocols or billing.

Forward-compatible: the callback signature allows a future "delta" event kind for
token streaming without re-plumbing.

## Verified current state
- `counsel.RunPanel` (internal/counsel/oracle.go:298) is fully blocking: does
  `http POST` + `io.ReadAll` (doJSONPost:37); results assembled only at
  `wg.Wait()` (panel), sequentially (fallback), single call (fusion), two rounds
  (debate). No streaming surface.
- `PanelCallConfig` (oracle.go:255) has no callback field.
- Async Mashūra path: `runMashuraCore` (mashura.go:235) → `enqueueAsyncOpJob`
  → worker runs `counsel.RunPanel` in a detached context → terminalizes →
  `publishAsyncOp` emits bounded `AsyncJobDoneMsg`.
- Sync sites: subagents, registry-full fallback, auto-counsel call RunPanel
  synchronously (must not regress).
- Phase 1 message family: AsyncJobStartMsg + AsyncJobDoneMsg exist in
  msgs.go. **AsyncJobChunkMsg does NOT exist yet** (Phase 1 deferred live content).
- sendEvent is goroutine-safe (Program.Send) — workers/watchdogs already call it.

## Design

### PanelMemberEvent — typed, forward-compatible (counsel)
Add to internal/counsel (oracle.go or a small events.go):
```go
type PanelMemberEventKind string

const (
    PanelMemberStart PanelMemberEventKind = "start"
    PanelMemberDone  PanelMemberEventKind = "done"
    PanelMemberError PanelMemberEventKind = "error"
    // PanelMemberDelta reserved for future token streaming (option c).
)

// PanelMemberEvent is an optional observer event fired as panel members progress.
// Round is 0 for panel/fallback/fusion, 1 or 2 for debate (debate reuses slot
// indices across rounds; round disambiguates). Text is reserved (currently empty
// or short detail) for future delta payloads.
type PanelMemberEvent struct {
    Slot  int
    Round int
    Model string // "provider:model" (or "openrouter:openrouter/fusion" for fusion)
    Kind  PanelMemberEventKind
    Text  string
}
```
Add to `PanelCallConfig`:
```go
// OnMemberEvent is an optional observer invoked as panel members progress
// (start/done/error). It may be called CONCURRENTLY from panel/debate member
// goroutines and MUST be fast (it runs on the paid-call path). It is invoked
// nil-safely and through a panic-recovering helper so UI telemetry can never
// corrupt panel results or accounting. nil at synchronous call sites (subagents,
// registry-full fallback, auto-counsel) → structurally unchanged.
OnMemberEvent func(PanelMemberEvent)
```
Event rule: emit events ONLY for actual attempted calls; once Start is emitted
for an attempt, emit exactly one Done or Error (fire the terminal event in a
defer so a member panic still yields it). Unknown-mode / oversized-debate /
empty-model are rejected before any attempt → zero events.

### 2. agent: AsyncJobChunkMsg + OriginChatID
```go
// AsyncJobChunkMsg delivers a live progress/status line for an async-job tab
// (Mashūra panel member status). OpID + OriginChatID route/reject (matching
// Start/Done). Display-only, bounded, single-line, control-sanitized; never
// written to the registry or delivered to the model.
type AsyncJobChunkMsg struct {
    OpID         string
    OriginChatID string
    Text         string
}
```

### 3. wire the callback in runMashuraCore — with a drop-capable forwarder
Fix the OpID chicken-and-egg (review blocker): change the worker `fn` signature
to receive the op id:
```go
// enqueueAsyncOpJob passes op.id into fn — no closure over the returned op.
func (a *App) enqueueAsyncOpJob(toolName, label string, fn func(opID string) (string, []counselUsageRec, []string, error)) (*asyncOp, string)
```
Inside the worker closure (mashura.go):
- build a small buffered status channel (cap e.g. 256) + ONE forwarder goroutine
  that drains it to `a.sendEvent(AsyncJobChunkMsg{...})` — best-effort, drop-on-
  full so a BUFFERED/STALLED Bubble Tea sink NEVER blocks a panel-member
  goroutine or delays the paid call / wg.Wait / slot release (mirrors the Phase 1
  publishAsyncOp "UI must not stall the async slot" invariant).
- the `OnMemberEvent` callback (set on a COPY of ccfg inside the worker) does a
  non-blocking push to the channel; the single forwarder preserves per-op FIFO
  and bounds/neutralizes/sanitizes each line (single line, strip control bytes,
  neutralizeAsyncMarker, truncateUTF8 to asyncJobChunkMaxBytes = 256).
- close the channel when RunPanel returns; forwarder exhausts it before the
  worker terminalizes → every Chunk send completes before publishAsyncOp's Done.
- The registry-full synchronous fallback uses a SEPARATE ccfg copy WITHOUT the
  callback (or sets OnMemberEvent=nil) → it emits zero chunks (sync criterion).

### 4. counsel: invoke the observer in all four RunPanel branches
- panel: in each per-slot goroutine, notify(Start) before callMember and
  notify(Done|Error) in a defer after it (so panic still yields terminal).
- fallback (mode=="fallback"): same around each sequential callMember.
- fusion: in the early-return branch, notify(Start) before callFusion and
  notify(Done|Error) after (Round 0, Model "openrouter:openrouter/fusion").
- debate (runDebate): notify per member in round 1 (Round 1); round 2 (Round 2)
  only for round-1 survivors. Same Start/defer-terminal pattern per round.
- All invocations via a helper `notifyMemberFunc(ccfg, ev)` that nil-checks,
  recovers panics, and never holds any counsel lock.

### 5. TUI: route Chunk into the async-job tab buffer (guarded)
```go
case agent.AsyncJobChunkMsg:
    if msg.OriginChatID != "" && m.app.Client.ChatID != "" && msg.OriginChatID != m.app.Client.ChatID {
        break // stale-session chunk
    }
    for _, t := range m.subTabs {
        if t.kind == subTabAsyncJob && t.opID == msg.OpID {
            if !t.done { // defense-in-depth: late chunk after a watchdog/forced Done is dropped
                appendTabLine(t.buf, msg.Text)
            }
            break
        }
    }
```
- `appendTabLine(buf, s)` writes `s` with a `\n` prefix only when buf is non-empty
  (no leading blank line). Store status lines with a per-tab total cap
  (e.g. 32 status lines or 8KB) so a huge panel can't bloat the buf unboundedly.
- Before appending the final result in AsyncJobDoneMsg, add a blank-line
  separator when status text exists.
- Chunk never changes done/dot/auto-close state; missing tab (orphan / cleared
  on rotation) → no-op, NO resurrection (only Dones may create tabs).

### 6. Do NOT change
- publishAsyncOp / AsyncJobDoneMsg / exactly-once / cost / grounding / inbox.
- Any sync call site: nil callback → structurally unchanged (no chunk).
- The main agent's own streaming loop.

## Ordering guarantees (revised, precise)
- AsyncJobStartMsg is invoked before the worker goroutine is launched.
- For each attempted member call, Start precedes exactly one Done/Error.
- In debate, all round-1 terminal events precede every round-2 Start.
- All Chunk sends the sink ACCEPTED complete before publishAsyncOp sends Done:
  the worker closes the status channel and waits for the forwarder only under a
  bound (asyncJobChunkDrainMax = 2s). If the sink is wedged (Program.Send
  blocks), the worker abandons the drain and terminalizes anyway — a stuck UI
  can never stall terminalization or the async slot. Late stragglers are dropped
  by the TUI's done guard.
- Chunk OriginChatID is the op's REGISTERED origin (passed to the worker with
  opID), never a worker-time a.chatID() re-read.
- TUI processing order is FIFO per the Bubble Tea Program.Send channel; ordering
  among PARALLEL members is intentionally nondeterministic (only each member's
  own start→terminal is asserted).
- A Chunk for a done, missing, or stale-session tab is ignored (t.done guard +
  OriginChatID guard).

## Acceptance criteria (revised, precise)
- Start is invoked before the async worker is launched.
- For each attempted member call, Start precedes exactly one Done/Error (terminal
  fired in a defer so a panic still yields it).
- In debate, all round-1 terminal events precede every round-2 Start; round-2
  events only for round-1 survivors.
- All Chunk sends the sink accepted complete before publishAsyncOp's Done
  (bounded drain); if the sink is wedged the worker abandons the drain and
  terminalizes (never stalls the async slot). Ordering among parallel members
  unspecified.
- A Chunk for a done, missing, or stale-session tab is ignored (OriginChatID +
  t.done guards); NONE resurrects a tab.
- Sync Mashūra sites (subagents, registry-full fallback, auto-counsel) emit no
  Chunk and behave identically.
- Chunk emission never blocks a panel-member goroutine or delays the paid call /
  slot release (drop-on-full forwarder). A panicking observer does not change
  results or accounting.
- Exactly-once result delivery, cost, grounding, inbox unchanged.
- Status text is single-line, control-sanitized, marker-neutralized, UTF-8-safe,
  bounded per event (asyncJobChunkMaxBytes=256) and per tab (status-line cap).
- Final answer still appears at completion (Phase 1 behavior), separated from
  status lines; no leading blank line.
- Full repo: go build ./..., go vet ./..., go test ./...,
  go test -race ./internal/counsel ./internal/agent ./internal/tui.

## Out of scope (roadmap; option c)
- Full token streaming (option a) behind a flag later — PanelCallConfig already
  has a PanelMemberDelta-kind slot. Requires SSE parsers + usage reassembly +
  fusion/debate handling. Separate card.
- Detached-shell tabs, cross-session delivery, Mashūra watchdog (Phase 1
  follow-ups, unchanged).
