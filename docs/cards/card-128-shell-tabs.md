# Card #128 — Detached shell (run_background) tabs in the TUI

Trello: https://trello.com/c/7g3R7W9X (feature).

Status: **DELIVERED** (this note reflects the shipped source at HEAD, updated
2026-08-11 for cards #132/#133/#134). Earlier versions of this doc described the
pre-implementation proposal; that content is superseded.

## What ships

Detached shells (auto-backgrounded `run_shell` at deadline/turn-cancel, and
explicit `run_background`) surface as selectable TUI async-job tabs, keyed by
OpID `job-<bgID>`, reusing the generic `subTabAsyncJob` machinery
(`AsyncJobStartMsg`/`AsyncJobDoneMsg`). The tab shows live status, a bounded
tail-preview on completion, with 30s display-only auto-close, prune/nav/dot, and
manual close — all inherited from the generic async-job tab machinery.

## Start / Done lifecycle (tool_handlers.go, internal/agent)

| Path | Start | Terminal condition | Done | OpID | origin |
|---|---|---|---|---|---|
| auto-bg `run_shell` deadline | `announceShellStart` | process group dead | `notifyDetachedShellExit` → `announceShellDone` | `job-<bgID>` | captured at creation |
| auto-bg `run_shell` turn-cancel | `announceShellStart` | process group dead | same as above | `job-<bgID>` | captured |
| explicit `run_background` | `announceShellStart` | process group dead | reaper → `announceShellDone` | `job-<bgID>` | captured |
| kill_process / generation loss | — (reaper captures entry pointer) | SIGTERM/SIGKILL | reaper `announceShellDone` | `job-<bgID>` | captured |

- `announceShellStart` (:331) and `announceShellDone` (:357) are guarded
  exactly-once under `bgMu` by `tabStarted` / `tabDoneSent`.
- `shellTailPreview` (:398) reads only the log TAIL (multi-GB safe) and returns
  the exit-status line + bounded tail; shared by the tab-Done and model-notice
  paths so both report the same exit status.
- `notifyDetachedShellExit` (:412) emits the tab Done **and** models the inbox
  completion notice.
- TUI Start/Done handlers live in `internal/tui/tui_agent_msgs.go` (AsyncJobStart~
  /AsyncJobDone~). These also apply card #133 (duplicate Done arms exactly one
  auto-close timer) and card #134 (`subTab.active` cleared on Done).

## Cards applied on top of this feature

- **Card #132 (d6db6bc)** — stranded-tab fix. The auto-bg reaper previously did a
  fresh `a.bgProcs[bgID]` lookup and gated the tab Done behind `notifyOnExit`;
  kill_process / read_process_log / generation loss deleted the entry (and
  kill/shutdown disarmed `notifyOnExit`), so an opened tab never received its
  matching Done → stranded yellow & unclosable. Fix: the reaper captures the
  `bgEntry` pointer and terminalizes a started tab on group exit independent of
  `notifyOnExit`; `notifyOnExit` now gates only the model inbox notice.
  `announceShellStart` no-ops when the tab is already finalized (`tabDoneSent`),
  closing the Done-before-Start ordering race.
- **Card #133 (d5b5ed5)** — duplicate `AsyncJobDoneMsg`/`SubagentDoneMsg` re-armed
  the 30s auto-close timer; now armed only on the `!done→done` transition.
- **Card #134 (d32c23a)** — `subTab.active` cleared on Done (semantic cleanup).

## Known open considerations (not bugs of this feature)

- A **signal-ignoring, never-exiting** child keeps its tab active (by design — the
  reaper polls *group* liveness, so the tab stays until the whole group dies).
  A killed-but-unreaped descendant can therefore keep a tab pulsing; this is
  intended group-lifecycle semantics, not a strand.
- Strict **Start-before-Done sink ordering** is not guaranteed under concurrent
  emission (the reaper can finish a tab while the Start sender is between unlock
  and `sendEvent`). This produces no stranded/unclosable tab in either order: the
  TUI's orphan-Done fallback creates a terminal tab, and a late Start neither
  resurrects nor leaves it running (verified against the TUI handlers).

## Files

- `internal/agent/tool_handlers.go` — Start/Done emission, reapers, helpers.
- `internal/agent/async_ops.go` — `publishAsyncOp` (Mashūra jobs, uiJob).
- `internal/agent/msgs.go` — `AsyncJobStartMsg`/`AsyncJobDoneMsg`/`AsyncJobChunkMsg`.
- `internal/tui/tui_agent_msgs.go` — TUI handlers.
- Tests: `internal/agent/shell_tab_test.go`, `internal/tui/asyncjob_tab_test.go`,
  `internal/tui/subtab_autoclose_test.go`.
