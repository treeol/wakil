# Card #148 P0 — D10 Inventory: every TUI/headless → agent.App coupling

Status: ARTIFACT (read-only; produced from grep + targeted reads at HEAD on
`feature/wakild-daemon`). This is the P0 working list for severing the boundary.
Legend: **[R]** read, **[W]** write, **[M]** method call on App, **[FN]** agent
package function/type used.

---

## 1. The coupling surface, in one picture

Two frontends consume `*agent.App` directly, through **seven** distinct
channels — not just `EventSink`:

1. **Direct field read/write** — `m.app.X = y`, `m.app.X` (TUI + headless).
2. **`App.Out io.Writer`** — headless sets `app.Out = hw`; TUI sets it to a
   `ProgWriter` whose chunks go back through `sendEvent` (tui_cmds.go:19).
3. **`App.OnReasoning` / `App.OnTokRate`** — callbacks set by `RunTurn`
   (tui_cmds.go:21–22), invoked during streaming.
4. **`App.Confirm Confirmer`** — the *synchronous* approval callback; set to
   `tuiConfirmer` (tui_cmds.go:20) or `headlessConfirmer` (run.go:313).
5. **`App.EventSink func(interface{})`** — untyped callback; wired to
   `globalProg.Send` in main.go:225.
6. **`agent.Cmd` messages** — returned from `HandleTUICommand` /
   `HandlePlanCommand`, bridged via `adapter.go` `AdaptCmd`; some carry the
   `BatchMsg` invariant (must never transit `sendEvent`).
7. **Headless JSON events** — `emitEvent(out, map[string]any{...})` in run.go,
   a separate vocabulary (`tokens`/`done`/`error`/`declined`).

The **only** channel-bearing message is `ConfirmReqMsg.RespCh chan
ConfirmChoice` (msgs.go:21) — the sync approval block. Everything else is
already plain data, so the D6 "events are data-only" rule has exactly one
violator to fix.

---

## 2. TUI (`internal/tui`) — non-test occurrences

### tui.go
| Line | Expression | Class |
|---|---|---|
| 507 | `m.app.StartupNote` | [R] |
| 508 | `m.app.StartupNote` | [R] |
| 509 | `m.app.StartupNote = ""` | [W] |
| 991 | `m.app.Consent()` | [M] |
| 1006 | `m.app.SetAllowDestructive(false)` | [M] |
| 1027 | `m.app.RevokeAuto()` | [M] |
| 1183 | `m.app.PendingImages = reconcileImageChips(...)` | [W] |
| 1187 | `len(m.app.PendingImages)` | [R] |
| 1193 | `m.app.Cfg.MentionBase` | [R] |
| 1199–1200 | `m.app.PendingImages` | [R] |
| 1391 | `m.app.PendingImages = reconcileImageChips(...)` | [W] |
| 1393 | `len(m.app.PendingImages)` | [R] |
| 1397 | `m.app.Cfg.MentionBase` | [R] |
| 1402–1403 | `m.app.PendingImages` | [R] |
| 1481 | `m.app.StartSideQuestion(...)` | [M] |

### info_panel.go
| Line | Expression | Class |
|---|---|---|
| 36 | `m.app.SetInfoPanelOpen(...)` | [M] |
| 222 | `m.app.Client` (nil check) | [R] |
| 225 | `m.app.Client.Grounding()` | [R] |
| 241 | `m.app.Costs.SnapshotSplit()` | [R] |
| 293 | `m.app`, `m.app.Costs` (nil check) | [R] |
| 296 | `m.app.Costs.SnapshotSplit()` | [R] |

### tui_agent_msgs.go
| Line | Expression | Class |
|---|---|---|
| 184 | `m.app.SetAutoApprove(true)` | [M] |
| 185 | `m.app.SaveRepoState(func(s *agent.RepoState){...})` | [M] |
| 193 | `m.app.Consent().AutoApprove` | [R] |
| 194 | `m.app.SetAllowDestructive(true)` | [M] |
| 344,392,415 | `m.app.Client.ChatID` | [R] |
| 550–551 | `m.app.Client.ChatID`, `m.app.SessionWorkspace()` | [R]/[M] |
| 574 | `m.app.Workflow` | [R] |
| 605–606, 624 | `m.app.Client.ChatID` / `m.app.SessionWorkspace()` / `m.app.Workflow` | [R]/[M] |
| 680 | `m.app.CtxLimit = msg.Limit` | [W] |
| 681 | `m.app.CtxPressureWarned = false` | [W] |
| 691 | `m.app.ModelList = msg.Models` | [W] |
| 725 | `m.app.PendingImages = append(...)` | [W] |
| 764–765 | `m.app.Conv`, `convItemsFrom(m.app.Conv)` | [R] |
| 818 | `m.app.PendingImages = nil` | [W] |
| 823 | `m.app.Workflow = nil` | [W] |
| 826 | `m.app.NewConversation(msg.NewChatID)` | [M] |
| 882–883 | `agent.BuildHandoffContext(...)`, `m.app.Conv = append(...)` | [FN]/[W] |
| 889 | `m.app.SaveSession()` | [M] |
| 903 | `m.app.Tools = msg.Tools` | [W] |
| 909, 932 | `m.app.Workflow` (nil check) | [R] |

### tui_view.go
| Line | Expression | Class |
|---|---|---|
| 446 | `m.app.ContextLimit()` | [M] |
| 447 | `m.app.ContextUsage()` | [M] |
| 479 | `m.app.Conv`, `agent.TranscriptSize(m.app.Conv)` | [R]/[FN] |
| 552–553 | `m.app.Workflow`, `.SidebarLabel()` | [R] |
| 558–559 | `m.app.Client`, `.LastUsedBackend()` | [R] |
| 561 | `m.app.SelectedBackend` | [R] |
| 562 | `m.app.Cfg.Backend` | [R] |
| 563 | `m.app.EffectiveModel()` | [M] |
| 564 | `m.app.EffectiveSubagentModel()` | [M] |
| 568 | `m.app.Consent()` | [M] |
| 592 | `m.app`, `m.app.RawTools` | [R] |

**TUI classification outcome:** the TUI reads far more than it writes. Writes
cluster on a few *control/session* fields (`PendingImages`, `Workflow`,
`CtxLimit`, `ModelList`, `Tools`, `StartupNote`, `Conv`) plus consent methods.
Pure display state (`InfoPanelOpen`, cursor/scroll/tabs — not listed because
they're TUI-owned) stays in the TUI.

---

## 3. Headless (`cmd/wakil`) — non-test occurrences

### main.go
| Line | Expression | Class |
|---|---|---|
| 114 | `app.PendingImages = append(...)` | [W] |
| 120 | `app.Client.ChatID = resumed.ChatID` | [W] |
| 149 | `app.SetCounselMode(...)` | [M] |
| 150 | `app.MaxCounsel = ...` | [W] |
| 153 | `app.Conv = resumed.Conv` | [W] |
| 154 | `app.Session = resumed` | [W] |
| 155 | `app.SetWorkflow(resumed.SavedWorkflow)` | [M] |
| 157–161 | `app.Session = &agent.Session{...}` | [W] |
| 170 | `app.StartupNote = result.Note` | [W] |
| 177 | `app.CtxLimit = agent.ResolveContextLimitForBackendModel(...)` | [W]/[FN] |
| 188–193 | `app.StagingClient.Scan(...)`, `app.StartupNote` | [R]/[W] |
| 200–209 | `app.MemoryStore.Stats(...)`, `app.StartupNote` | [R]/[W] |
| 225 | `app.EventSink = func(msg interface{}) { globalProg.Send(msg) }` | [W] |
| 250–251 | `app.StopAllAsyncOps()`, `app.StopAllBackgroundProcs()` | [M] |

### app_builder.go
| Line | Expression | Class |
|---|---|---|
| 76 | `buildApp(...) (*agent.App, appResources)` | [FN] — composition |
| 162 | `app.ApplyOptions(...)` | [M] |
| 175 | `app.SetConsent(...)` | [M] |
| 179 | `app.StagingClient = staging.NewClient(...)` | [W] |
| 185–194 | `app.MemoryStore = memStore` | [W] |
| 211 | `app.SkillStore = agent.NewSkillsProfile(...)` | [W]/[FN] |
| 225 | `app.SessionHistory = shStore` | [W] |
| 232–236 | `app.Trace = ts` (trace.Open) | [W] |
| 257–258 | `app.StopAllAsyncOps()`, `app.StopAllBackgroundProcs()` | [M] |
| 256 | `closeResources(app *agent.App, res appResources)` | [FN] |

`buildApp` is the **composition/bootstrap** package — per D12 it is *allowed*
to touch `*agent.App`. Everything here stays. This is the sanctioned
construction site.

### run.go (headless driver — must be severed)
| Line | Expression | Class |
|---|---|---|
| 297 | `runHeadlessApp(ctx, app *agent.App, ...)` | [FN] signature |
| 303 | `app.SaveSession()` | [M] |
| 309 | `app.StopAllAsyncOps()` | [M] |
| 312 | `app.Out = hw` | [W] |
| 313 | `app.Confirm = headlessConfirmer(...)` | [W] |
| 314 | `app.Client.ResetGrounding()` | [M] |
| 325–332 | `app.Costs.Snapshot()`, `emitEvent(tokens)` | [R]/[FN] |
| 340–347 | `emitBackendFailure`: `agent.ShortID(app.Client.ChatID)` | [R]/[FN] |
| 350 | `app.WorkflowStepTrace = nil` | [W] |
| 381 | `app.SendOutcome(ctx, task)` | [M] |
| 386 | `app.WaitForAsyncCompletion(ctx)` | [M] |
| 393 | `app.Resume(ctx)` | [M] |
| 409–418 | `app.Exec.Cwd()`, `app.Exec.RunShell`, `app.Exec.WriteFile`, `app.SetWorkflow(...)` | [R]/[M] |
| 430–548 | `app.Workflow` extensive [R]/[W] (Phase/StepIdx/StepCount/ReviewPlanHash...), `app.Send(ctx,"continue")` | [R]/[W]/[M] |
| 590 | `app.StopAllBackgroundProcs()` | [M] |
| 618 | `app.VerifyEnabled = true` | [W] |
| 622–640 | `app.Session = &agent.Session{...}`, `app.PendingImages = append(...)` | [W] |
| 651, 659 | `app.SetPolicy(p)` | [M] |

`run.go` is the headless driver and is exactly the code the P0 exit gate says
must stop holding `*agent.App`. Its methods (`runHeadlessApp`,
`runSingleTaskHeadless`, `driveHeadlessTurn`, `runWorkflowHeadless`,
`runWorkflowLoop`, `headlessConfirmer`, `emitBackendFailure`) all take
`*agent.App`.

---

## 4. Agent → client output surface (`sendEvent` call sites, non-test)

| File:line | Message type |
|---|---|
| app.go:1468 | `ToolStartMsg` |
| commands.go:83,91,106 | `SysNoteMsg` |
| commands.go:115 | `ConfirmReqMsg` (**channel-bearing**) |
| handoff.go:128,155,194 | `SysNoteMsg` |
| mashura.go:279 | `AsyncJobChunkMsg` |
| async_ops.go:421,982 | `SubagentDoneMsg` |
| async_ops.go:605 | `AsyncJobDoneMsg` (via boundAsyncJobDone) |
| async_ops.go:720 | `AsyncJobStartMsg` |
| async_subagents.go:77,154 | `SubagentDoneMsg` |
| side_question.go:45,52 | `SideQuestionDoneMsg` |
| side_question.go:62 | `SideQuestionChunkMsg` |
| side_question.go:65 | `SideQuestionDoneMsg` |
| subagent.go:368 | `SubagentChunkMsg` |
| subagent_parallel.go:146 | `SubagentActiveMsg` |
| subagent_parallel.go:287 | `SubagentStartMsg` |
| subagent_parallel.go:316 | `SubagentDoneMsg` |
| subagent_parallel.go:372 | `SubagentFinishedMsg` |
| tool_handlers.go:345 | `AsyncJobStartMsg` |
| tool_handlers.go:375 | `AsyncJobDoneMsg` |
| tool_handlers.go:1258 | `SubagentStartMsg` |
| tool_handlers.go:1281 | `SubagentActiveMsg` |
| tool_handlers.go:1302 | `SubagentDoneMsg` |
| turn_phases.go:233 | `ToolResultMsg` |
| tui_cmds.go:98 | `AgentDoneMsg` |
| tui_cmds.go:100 | `*wfNext` (WFStartTurnMsg) |
| tui_cmds.go:141 | `AgentDoneMsg` |
| workflow_engine.go:180 | `SysNoteMsg` |

Plus `tui_cmds.go:19–22` — `app.Out`/`OnTokRate`/`OnReasoning` wired to
`StreamChunkMsg`/`TokRateMsg`/`ReasoningChunkMsg` via closures (these are the
**callback** channels, distinct from `sendEvent`).

### Message types with channels/callbacks
- `ConfirmReqMsg.RespCh chan ConfirmChoice` (msgs.go:21) — **the only one**.
- `ProgWriter.send func(StreamChunkMsg)` (msgs.go:298) — writer mechanism, not
  an event, but a callback nonetheless.
- `App.OnTokRate func(float64)`, `App.OnReasoning func(string)` — callback
  fields (app.go:257,262).

---

## 5. Confirmer (approval) flow — the sync block

- **Assigned:** `tuiConfirmer(app)` at tui_cmds.go:20,138; `headlessConfirmer`
  at run.go:313; tests assign closures directly.
- **Called (approval gates):** tool_handlers.go:59 (run_shell), 469 (open_url),
  733 (write_file), 808 (write_binary_file), 929 (delete_file), 965 (move_file),
  1021 (run_background), 1101 (kill_process), 1440 (MCP CallTool);
  app.go:2183 (edit_file); mashura.go:211; subagent.go:576 (external_backend);
  turn_phases.go:81 (external_backend); workflow_engine.go:212; verify.go:143;
  skill_handlers.go:179,228,268.
- **The block:** `tuiConfirmer` (commands.go:70–132) — policy eval → auto/suspend
  → else `ch := make(chan ConfirmChoice, 1)`; `sendEvent(ConfirmReqMsg{...,
  RespCh: ch})`; `switch <-ch`. This is the synchronous pause in the middle of
  the agent goroutine, and the one thing that cannot cross a wire in P2. Per D5:
  shim in P0 (emit ApprovalRequested/Resolved notifications around it), full
  async rework in P2.

---

## 6. `agent.Cmd` bridge + invariants (adapter.go)

- `agent.Cmd = func() any`, `tea.Cmd = func() tea.Msg`; `AdaptCmd` is the only
  meeting point of the two type systems (adapter.go:33–61).
- **Invariant:** `agent.BatchMsg` must never transit `sendEvent` (would arrive
  unhandled; sub-Cmds never run). It is only produced by `agent.Batch()`,
  returned from `HandleTUICommand`/`HandlePlanCommand`, always wrapped by
  `AdaptCmd`.
- **Clipboard sentinel:** `agent.ClipboardImageRequest` is substituted with the
  TUI's own `readClipboardCmd()` in `AdaptCmd` (adapter.go:45–47).
- These invariants must be preserved (or eliminated deliberately) when retyping
  `EventSink` — they are exactly the kind of implicit contract that breaks in a
  "mechanical" refactor.

---

## 7. Headless JSON event vocabulary (separate from everything above)

`emitEvent(out, map[string]any{...})` in run.go emits `type` ∈
{tokens, done, error, declined} with fields input/output, outcome, message,
reason, resume_id. This is a **third** output protocol (besides EventSink and
`agent.Cmd`). Per the plan: decide in P0 whether it becomes a projection of
domain events or stays a compatibility protocol (flagged, not silently merged).

---

## 8. Open question carried into P0 step 5/6 (needs a decision)

The D12 acceptance criterion ("neither `internal/tui` nor `cmd/wakil` may hold
or import `*agent.App`") **cannot literally apply to tests**: `cmd/wakil`'s
tests (command_test.go, client_test.go, resilience_test.go, workflow_test.go,
session_test.go, misc_test.go, handoff_test.go) and `internal/tui`'s tests all
construct `*agent.App` directly and must keep doing so to exercise the core.
Proposed resolution to confirm at step 5/6: **the criterion scopes to non-test
code**; tests may construct App via a test helper, but the *driver* paths they
exercise must go through the Service. Flagging here so it is decided
explicitly, not discovered mid-refactor.

---

## Counts (for progress tracking)

- TUI non-test direct App accesses: **~70** (R/W/M/FN).
- Headless non-test direct App accesses: **~60** (mostly run.go + main.go;
  app_builder.go is the sanctioned composition site and stays).
- `sendEvent` call sites: **27**.
- Approval gate `Confirm` calls: **15**.
- Channel-bearing message: **1** (`ConfirmReqMsg.RespCh`).
