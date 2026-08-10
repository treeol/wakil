# Card #128 — Detached shell (run_background) tabs in the TUI

Trello: https://trello.com/c/7g3R7W9X (feature).

## Problem (verified 2026-08-10 at HEAD)
Detached shells (auto-backgrounded at deadline via runShellWithDeadline, or explicit
`run_background` via handleRunBackground) do NOT surface as selectable TUI tabs.
- `notifyDetachedShellExit` (tool_handlers.go:295) builds a `job-<bgID>` asyncOp and appends it
  DIRECTLY to asyncInbox (line 347) — no registerAsyncOp/publishAsyncOp → no AsyncJobStartMsg/
  DoneMsg. Card #126's publishAsyncOp uiJob hook fires only for Mashūra ops.
- The TUI's generic async-job tab machinery (kind `subTabAsyncJob`, keyed by OpID) already exists
  and routes generically — so shells could ride it keyed by `job-<bgID>`.

## Two detached-shell start paths (tool_handlers.go)
1. **Auto-bg** (runShellWithDeadline): at deadline hit (line 235 case) it arms
   `notifyOnExit=true`; the reaper closes `done` and calls `notifyDetachedShellExit` on exit →
   the op lands in asyncInbox.  Currently NO Start/Done events.
2. **Explicit `run_background`** (handleRunBackground): starts at line 922, entry at 938. Its
   reaper (945) closes `done` but does NOT notify → no inbox entry, no events at all.

## Design
Emit tab events via the existing generic event family (keyed by `job-<bgID>` as OpID); keep shells
OUT of the registry admission path (they have no worker goroutine and deliberately bypass
registerAsyncOp/publishAsyncOp — no admission slot to reserve or release). Route via the event
sink only, matching how notifyDetachedShellExit already delivers into the inbox.

- **Start (auto-bg):** in runShellWithDeadline, when the deadline is hit and the shell detaches
  (before returning the "still running as bgN" message, line ~262), emit
  `AsyncJobStartMsg{OpID:"job-"+bgID, Label: cmdDigest, ToolName:"run_shell"}`.
- **Start (explicit run_background):** in handleRunBackground, after a successful StartBackground,
  emit the same Start (Label=args.Label or cmd digest).
- **Done (both):** in notifyDetachedShellExit, emit `AsyncJobDoneMsg{OpID:"job-"+bgID,
  Label, ToolName:"run_shell", Result:<bounded tail preview>, OriginChatID}`. For explicit
  run_background (which doesn't call notifyDetachedShellExit today), add a Done emission from its
  reaper OR route it through a small shared helper.

Common helper: `a.announceShellStart(bgID, label, cmdDigest)` and
`a.announceShellDone(opIDOrBGID, label, tail, originChatID)` shield both paths.

## Constraints
- Do NOT break read_process_log / kill_process / bgRegistry.
- Tab cleanup display-only; process continues regardless.
- LSP dirty-marking at drain (shellLSPDirty) unaffected.
- Live output: only the bounded tail preview in Done (never read whole multi-GB logs).
- Route by OpID `job-<bgID>` like Mashūra, 30s display-only auto-close, prune/nav/dot reuse — all
  inherited from the generic subTabAsyncJob machinery (no TUI changes needed).

## Open decision (Mashūra)
- Should explicit `run_background` ops also get a Done tab (currently they produce no inbox entry)?
  Consistency suggests yes; the card names run_background explicitly.

## Files
- `internal/agent/tool_handlers.go` — Start hooks + Done emission (shared helpers).
- Possibly `internal/agent/msgs.go` — no new message types needed (reuse AsyncJobStartMsg/DoneMsg).

## Test requirements
- Auto-bg: shell detaches → Start emitted; on exit → Done emitted with tail preview; tab lifecycle
  (30s auto-close, prune) inherited (existing TUI async-job tests).
- Explicit run_background: Start on start, Done on exit.
- No regression to read_process_log/kill_process/bgRegistry.
