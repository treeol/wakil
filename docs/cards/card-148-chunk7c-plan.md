# Card #148 P0 — Chunk 7c plan v2: headless `--plan` re-route through the session host

Status: v2 — Mashura-reviewed (op-34: gpt-5.6-sol, claude-fable-5, glm-5.2).
v1 was rejected as not implementation-ready; v2 folds in all three panels'
blockers. Branch: `feature/wakild-daemon`, tree at 417fa73.

## 0. Goal — unchanged

`wakil run --plan` drives the workflow through the session host (like
`runSingleTask`), not the direct-App legacy loop. Gate #2 fully green.

## 1. Verified facts (v2, all checked against source)

- `RunHeadless` (headless.go:265-268) AND `RunHeadlessApp` (headless.go:275)
  both dispatch `--plan` → `runWorkflowLegacy`. BOTH must be re-routed.
- Host `finishTurn` (host.go:1141-1189) classifies turn errors into exactly
  three SessionError reasons: backend_failure / internal_error /
  request_error. No extension point for plan-specific reasons; a sentinel
  wrapping `ErrBackendFatal` is classified `request_error` (glm blocker 1,
  verified host.go:1146).
- `TurnInput.EnqueueInput` is ALWAYS non-nil on executor-driven turns
  (host.go:1085-1100) — the nil-guard in hostturn.go:417 is for tests only.
- `TurnCompleted.WorkflowWillContinue` is host-computed from queue state at
  finalization (host.go:1166-1172) — the correct terminal signal.
- `DriveTurnWithResilience` (resilience.go:160-177) already does
  stream-retry AND empty-recovery inside the turn; last assistant text is
  authoritative after recovery.
- `IsMashuraTool("mashura__review")` = true (tools.go:442-448) → headless
  resolver auto-approves oracle confirms in --auto; non-auto declines them.
- Legacy sends `"continue"` for EVERY turn including the first
  (workflow_legacy.go:113); the task text lives in `.wakil/plan.md`
  (`WFInitPlanContent(task)`), and the engine reads markers from assistant
  output only. The first submitted text is model-visible → parity requires
  `"continue"` (panels 7 both).
- `wfProgNote` → `SysNoteMsg` → EventSink; legacy headless had NO sink set
  → prog notes were dropped, NOT written to `out`. Legacy output = deltas +
  review-skip warning + terminal record. No tokens record in legacy workflow
  mode (unlike single-task).
- grep `RunHeadlessApp|RunWorkflowLoopLegacy|HeadlessConfirmer` (module
  root): definitions only in headless.go / workflow_legacy.go; no external
  callers. `emitBackendFailure` used by workflow_legacy.go only. Deletion is
  safe once both dispatch sites move (still gated by build + full tests).

## 2. Design v2

### D1 — runPlanTask replaces runWorkflowLegacy at BOTH dispatch sites

Pre-host setup (plan.md write, SetWorkflow, ResetGrounding, NoOracle cfg
mutation, HeadlessWriter + SaveSession/StopAllAsyncOps defers) moves
verbatim into `runPlanTask(ctx, app, task, opts, out)`. Then:
HostTurnFunc(app, WithResolver(headlessResolver), **WithPlanAutoAdvance**
(opts)) → host → CreateSession → Subscribe → **SubmitInput("continue")**
(byte parity with legacy's first Send).

### D2 — After-turn region becomes one centralized resolver (replaces
hostturn.go:417-433 wholesale; TUI/single-task keep current semantics via
the mode flag)

The hostTurn owns a **decline latch** (mutex-protected, set by
newHostConfirmer on every declined resolution — the D18 control function the
panels found missing). After `DriveTurnWithResilience` succeeds:

```
planResult := resolvePlanAfterTurn(ctx, app, in, latch, opts):
  1. latch declined? → emit WorkflowOutcome{declined, reason}; return terminal
     (legacy checked declinedReason post-Send pre-transition — parity)
  2. wfBefore := app.Workflow != nil
     wfNext := HandleWorkflowTransition(ctx, app)   // may run oracle (confirm
                                                    // gate auto-approve/decline
                                                    // per headless policy)
  3. wfAfter := app.Workflow != nil
     if wfBefore && !wfAfter → final review passed inside transition →
         emit KindWorkflowFinalReview; return terminal-pass
  4. if wfNext != nil → enqueue(wfNext.UserText); marker; return continue
     (enqueue REJECTION is terminal: return error wrapping ErrInternal →
      host SessionError{internal_error}; never silently idle — panels 5)
  5. waiting-state resolver (wfNext == nil, planAutoAdvance only):
     WFPresent → Phase=WFImplement, StepIdx=1; enqueue("continue") [legacy 145]
     WFReview  → warning; WFWriteReviewSkipForce(reason); Phase=WFPresent;
                 enqueue("continue") [legacy 149-160]
     WFImplement:
       StepIdx > StepCount → final review already ran inside transition:
         emit KindWorkflowFinalReview; map (declined-latch → declined;
         VerifyDeclined → declined; VerifyFailed → verify_failed; else gaps)
         as WorkflowOutcome; return terminal [legacy 163-185]
       else StepIdx++; if StepIdx > StepCount → run HandleFinalReview HERE
         (resolver-owned call — legacy 186-209, the no-marker crossing):
         emit KindWorkflowFinalReview; map outcome as above; return terminal
         else enqueue("continue") [legacy 186]
     default → return error wrapping ErrInternal ("unexpected waiting state")
     — WFPlan format-retry nil-return is NOT an error: engine retries on the
       next turn (engine:35-49); legacy's default-error branch was dead for
       WFPlan because transition handles it. Resolver must distinguish.
```

Invariant (gpt-5.6-sol acceptance criterion): every completed plan turn
yields exactly ONE of {terminal WorkflowOutcome event, one queued
continuation, error}. A live workflow never becomes silently idle.

### D3 — New durable event: KindWorkflowOutcome; new ephemeral:
KindWorkflowWarning

- `WorkflowOutcome{TurnID, Outcome ∈ pass|declined|verify_failed|gaps,
  Reason string}` — durable on the session emitter BEFORE the turn returns;
  ordered before TurnCompleted (emitDraft sequencing guarantees).
- `WorkflowWarning{Message string}` — ephemeral, Notify at skip time;
  consumer renders the exact legacy `{"type":"warning","message":…}` record.
- Resolves the sentinel-transport blocker: NO SessionError reason changes,
  sessionhost stays 100% workflow-free (D12), replay sees true outcomes,
  and an expected workflow terminal (decline/gaps) is not misclassified as a
  stream error (glm's ErrBackendFatal misclassification avoided entirely).
- D2a (v1) withdrawn — the "vocabulary leak" objection dissolved: the
  vocabulary lives in a dedicated workflow event, not SessionError.

### D4 — consumeWorkflowEvents (consumer-side)

Terminates on FIRST of:
1. `KindWorkflowOutcome` → map outcome → exit code + terminal record bytes
   (declined: reason from the event, NOT a separate latch — the event is
   authoritative; verify_failed/gaps: message parity with legacy strings).
2. `SessionError` → backend_failure (with resume_id) / request_error /
   internal_error — same records as consumeTurnEvents.
3. `TurnCompleted{cancelled}` → error record "turn cancelled" (ExitError).
4. `TurnCompleted{complete, WorkflowWillContinue=false}` with no prior
   outcome event → pass (workflow done; final turn completed normally).
Deltas → hw.Write. `KindWorkflowWarning` → warning record.
NEVER reads `app.Workflow` (boundary + race, panels 6). No tokens record
(legacy parity; noted deviation from single-task).

Precedence (panels): error > cancelled > declined > gaps > pass — realized
structurally by first-terminal-wins consumption.

### D5 — Deletion of workflow_legacy.go

Move `emitBackendFailure` (reused for backend_failure parity records —
actually superseded by D4.2 consumeTurnEvents-style records; delete only if
truly unused after re-route). Delete HeadlessConfirmer /
RunWorkflowLoopLegacy / runWorkflowLegacy / runWorkflowLoopLegacy after:
`grep -rn 'RunWorkflowLoopLegacy|HeadlessConfirmer|runWorkflowLegacy|
emitBackendFailure'` returns nothing outside the deleted file; build +
`go vet ./...` + full tests. Both dispatch sites (RunHeadless,
RunHeadlessApp) re-routed first.

### D6 — Cancellation path

runPlanTask passes ctx (with CLI signal handling unchanged from
runSingleTask — same code path). Cancellation mid-turn → cancelled outcome
(D4.3). Cancellation between transition and enqueue → enqueue fails
(closing) → ErrInternal → SessionError — consumer sees it; no hang.

## 3. Intentional deviations from legacy (documented, tested)

1. First SubmitInput is `"continue"` — byte parity kept (panels' fix).
2. Oracle-consent decline during transition: v2 terminates at the post-
   transition latch check; legacy ran one extra "continue" turn before its
   next-loop declinedReason check fired. Deviation: one fewer turn, same
   terminal bytes. (Panels required the latch; legacy's extra turn is a
   quirk, not a contract.)
3. WFReview pause → force-skip emits an explicit WorkflowWarning event; the
   warning BYTES are identical, the transport is new (required — no `out`
   writer in the adapter).
4. Per-step TurnStarted/TurnCompleted/UserMessageCommitted pairs are new
   durable events (host semantics); legacy had none. More events, better
   replay — Gate #2's "service boundary only" demands them.
5. No tokens record (legacy parity).

## 4. Tests (golden, byte-level; fakes = existing fake SSE servers)

Golden scenarios (exact exit code + exact terminal JSON + warning records +
ordering + turn-pair counts + first submitted text):
1. pass (multi-step: StepCount=2, marker crossing on final step)
2. tool decline during implement (mid-workflow, post-turn latch path)
3. oracle consent decline during review (--no-auto)
4. verification command declined (VerifyDeclined)
5. verify failure (VerifyFailed)
6. oracle gaps (final review flags gaps)
7. oracle unavailable warning (skip) + continue to completion
8. backend failure mid-workflow → backend_failure + resume_id bytes
9. fatal request error (4xx) → request_error
10. cancellation during stream / during approval
11. enqueue rejection (full queue) → internal_error, no hang (deadline)
12. resolver-owned final review (no-marker StepIdx crossing) → gaps
13. zero-step plan → WFPresent resolver StepIdx=1 → immediate final-review
    mapping (legacy quirk preserved)
14. empty response mid-workflow → recovery inside turn, transition exactly
    once after authoritative text
15. WFPlan format-invalid → next-turn retry, no "unexpected state" error
16. marker ordering: WorkflowOutcome < TurnCompleted; FinalReview marker on
    every final-review boundary (both call sites)
Unit (table-driven, no SSE): resolvePlanAfterTurn over {phase, StepIdx/
StepCount, transition result, latch, Verify flags, enqueue result, NoOracle}
— exactly-one-of invariant asserted per case.

## 5. Package homes

- `internal/wiring/headless.go`: runPlanTask, consumeWorkflowEvents.
- `internal/wiring/hostturn.go`: resolver (mode-gated), decline latch.
- `internal/wiring/workflow_legacy.go`: DELETED (post-grep).
- `internal/core/event`: +KindWorkflowOutcome, +KindWorkflowWarning,
  payload types + validation tests (bounded Reason).
- `internal/agent`: UNCHANGED (engine untouched — marker via state-diff).
- `cmd/wakil/workflow_test.go`: adapted to host path, golden assertions.

## 6. ENV note

/tmp noexec: GOTMPDIR=/mnt/wakil/.gotmp GOCACHE=/mnt/wakil/.gocache.
