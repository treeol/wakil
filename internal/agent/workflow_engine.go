package agent

import (
	"context"
	"fmt"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/counsel"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/workflow"
)

// handleWorkflowTransition detects phase-completion sentinels in the last
// assistant message and drives the state machine forward. It may call the
// oracle (through the confirm gate) for REVIEW and on-deviation checks.
// Returns a non-nil *WFStartTurnMsg when the TUI should auto-start a new turn.
func HandleWorkflowTransition(ctx context.Context, app *App) *WFStartTurnMsg {
	wf := app.Workflow
	phaseDone, stepDone, stepFailed := workflow.DetectPhaseMarkers(app.Conv)

	switch wf.Phase {
	case workflow.WFGather:
		if !phaseDone {
			return nil
		}
		wf.Phase = workflow.WFPlan
		wfProgNote(app, "· gather complete → plan phase")
		return &WFStartTurnMsg{
			Note:     "plan phase: writing implementation plan",
			UserText: "continue",
		}

	case workflow.WFPlan:
		if !phaseDone {
			// PlanFormatInvalid retry: any completed turn re-checks whether the
			// model has reformatted ## Plan into parseable N. steps.
			if wf.PlanFormatInvalid && app.Exec != nil {
				if content, err := app.Exec.ReadFile(ctx, wf.PlanPath); err == nil {
					if n := workflow.CountPlanSteps(content); n > 0 {
						wf.StepCount = n
						wf.PlanFormatInvalid = false
						wfProgNote(app, fmt.Sprintf("· plan re-parsed: %d steps — running oracle review", n))
						wf.Phase = workflow.WFReview
						return HandleReviewOracle(ctx, app)
					}
					wfProgNote(app, "⚠ plan still has no numbered steps — reformat as 'N. description' and emit %%PHASE_DONE%%")
				}
			}
			return nil
		}

		// Plan phase completed (%%PHASE_DONE%%).
		// Read plan and check for the format contract before advancing.
		content := ""
		if app.Exec != nil {
			content, _ = app.Exec.ReadFile(ctx, wf.PlanPath)
		}
		n := workflow.CountPlanSteps(content)
		if n == 0 {
			planSection := workflow.ExtractPlanSection(content, "## Plan")
			notEmpty := planSection != "" && planSection != "(pending plan phase)"
			if notEmpty {
				wfProgNote(app, "⚠ plan format error: ## Plan is non-empty but contains no numbered steps")
				wfProgNote(app, "· each step must be a top-level 'N.' line (e.g. '1. Fix the bug') — headers are not valid")
				wfWritePlanFormatError(app)
				wf.PlanFormatInvalid = true
				return nil // stay in WFPlan; reformat directive fires on next turn
			}
		}

		wf.StepCount = n
		wf.PlanFormatInvalid = false // clear any previous format error
		wf.Phase = workflow.WFReview
		wfProgNote(app, "· plan complete — running oracle review")
		return HandleReviewOracle(ctx, app)

	case workflow.WFReview:
		// Any completed turn while in WFReview re-attempts the oracle review
		// automatically — mirroring the verify remediation loop.
		wfProgNote(app, "· re-attempting oracle plan review")
		return HandleReviewOracle(ctx, app)

	case workflow.WFImplement:
		// Verify state: StepIdx > StepCount means all steps completed but the
		// final review flagged gaps (or was unavailable). Any completed turn in
		// this state is treated as a remediation attempt — re-run final review
		// automatically so the gate is re-passable, not only bypassable.
		if wf.StepIdx > wf.StepCount {
			// Append the remediation evidence block to plan.md BEFORE the final
			// review fires so the briefing carries the new receipts, not stale ones.
			wfAppendRemediationEvidence(app)
			wfProgNote(app, "· remediation turn complete — re-running final review")
			HandleFinalReview(ctx, app)
			return nil
		}

		switch {
		case stepDone:
			completedStep := wf.StepIdx

			// Extract the %%STEP_LOG: …%% entry emitted by the model and
			// append it together with the machine-written evidence trace.
			// The model cannot author or edit the [ev] lines; they are produced
			// here from WorkflowStepTrace, which is reset each turn in runTurn.
			if text := workflow.LastAssistantText(app.Conv); text != "" {
				if entry := workflow.ExtractStepLogEntry(text); entry != "" {
					// Combine model's claim with Wakil's evidence.
					combined := entry
					if ev := FormatStepEvidence(app.WorkflowStepTrace); ev != "" {
						combined = entry + "\n" + ev
					}
					wfAppendStepLogEntry(app, combined)
				} else {
					wfProgNote(app, fmt.Sprintf("⚠ step %d: no %%%%STEP_LOG%%%% entry found — log may be incomplete", completedStep))
				}
			}

			// Verify the step log now has exactly completedStep entries.
			if app.Exec != nil {
				if content, err := app.Exec.ReadFile(ctx, wf.PlanPath); err == nil {
					if got := workflow.CountStepLogEntries(content); got != completedStep {
						wfProgNote(app, fmt.Sprintf("⚠ step log count: expected %d entries, found %d", completedStep, got))
					}
				}
			}

			// every-step oracle: consult before advancing.
			// A critical response pauses (returns nil) so the user can review;
			// /plan approve then advances to the next step.
			mode := wf.EffectiveOracleMode(app.Cfg)
			if mode == "every-step" {
				stepQ := fmt.Sprintf(
					"Critique the outcome of step %d/%d before proceeding. "+
						"Is the change correct and complete? Flag any issue to address.",
					completedStep, wf.StepCount)
				oracleResult, oracleAvail := doWFOracle(ctx, app, stepQ)
				if oracleAvail {
					wfProgNote(app, fmt.Sprintf("step %d oracle:\n%s", completedStep, oracleResult))
					if workflow.WFEverystepCritical(oracleResult) {
						wfProgNote(app, fmt.Sprintf(
							"· oracle flagged issues with step %d — address them or /plan approve to advance",
							completedStep))
						return nil // pause: StepIdx stays at completedStep
					}
				} else {
					wfProgNote(app, "⚠ step oracle unavailable ("+oracleResult+")")
				}
			}

			wf.StepIdx++
			if wf.StepIdx > wf.StepCount {
				HandleFinalReview(ctx, app)
				return nil
			}
			return &WFStartTurnMsg{
				Note:     fmt.Sprintf("step %d complete → step %d/%d", completedStep, wf.StepIdx, wf.StepCount),
				UserText: "continue",
			}

		case stepFailed:
			deviationQ := fmt.Sprintf(
				"Step %d/%d failed or required a deviation. Advise on how to proceed: "+
					"should the plan be revised, or can the step be retried differently?",
				wf.StepIdx, wf.StepCount)
			oracleResult, oracleAvail := doWFOracle(ctx, app, deviationQ)
			if oracleAvail {
				wfProgNote(app, "oracle deviation advice:\n"+oracleResult)
			} else {
				wfProgNote(app, "⚠ oracle unavailable for deviation advice ("+oracleResult+") — review the failure manually")
			}
			// Do not auto-advance — let the user decide how to proceed.
			return nil
		}
	}
	return nil
}

// wfProgNote sends a SysNoteMsg to the TUI via the app's EventSink (nil-safe).
func wfProgNote(app *App, text string) {
	app.sendEvent(SysNoteMsg{Text: text})
}

// panelFlagsGaps returns true when the panel result should be treated as GAPS.
// Fail-closed: ALL responding models must return VERDICT: PASS; any GAPS (or no
// model responding at all) keeps the workflow open.
// Safety choice: one model's PASS cannot override another's GAPS.
func panelFlagsGaps(results []counsel.PanelMemberResult) bool {
	anyResponded := false
	for _, r := range results {
		if r.Err != nil {
			continue // errored member excluded from verdict; not counted as a responder
		}
		anyResponded = true
		if workflow.WFFlagsGaps(r.Answer) {
			return true // any GAPS → fail-closed
		}
	}
	return !anyResponded // no responders → fail-closed
}

// runWFPanel is the shared oracle runner for all workflow-phase consults. It
// receives a pre-validated briefing and a resolved panel config, gates the user
// with a single confirm prompt, runs the panel, records per-model costs, and
// returns (formatted string, raw results, available). available=false means the
// oracle could not run (key missing, declined, all members errored).
func runWFPanel(ctx context.Context, app *App, headline, question, briefing, panelName string, panel config.MashuraPanelConfig) (string, []counsel.PanelMemberResult, bool) {
	apiKeys, err := app.mashuraPanelKeys(panel)
	if err != nil {
		return err.Error(), nil, false
	}
	detail := counsel.PanelDetail(panelName, panel.Models, panel.Mode, question, briefing)
	if !app.Confirm("mashura__review", headline, detail, false) {
		return "declined by user", nil, false
	}
	ccfg := counsel.PanelCallConfig{
		MaxTokens:          app.Cfg.OracleMaxTokens,
		TimeoutSeconds:     app.Cfg.OracleTimeoutSeconds,
		AnthropicEndpoint:  app.Cfg.OracleEndpoint,
		FusionJudge:        panel.FusionJudge,
		FusionMaxToolCalls: panel.FusionMaxToolCalls,
	}
	results := counsel.RunPanel(ctx, panel.Models, panel.Mode, question, briefing, ccfg, apiKeys)
	for _, r := range results {
		if r.Err == nil {
			app.RecordOracleCostFor(r.Model, r.Usage)
			app.addExternalGrounding(proxy.GroundingEntry{Type: "oracle", Label: r.Model})
		}
	}
	formatted := counsel.FormatPanelResult(results)
	anyOk := false
	for _, r := range results {
		if r.Err == nil {
			anyOk = true
			break
		}
	}
	if !anyOk {
		return "oracle call failed: all panel members errored", results, false
	}
	return formatted, results, true
}

// doWFOracle calls the oracle with a bounded briefing built from the plan file.
// Returns (result, true) when the oracle ran successfully, (reason, false) when
// it could not run (disabled, no key, declined, or call error). The caller must
// treat false as "oracle unavailable" and not silently advance the workflow.
func doWFOracle(ctx context.Context, app *App, question string) (string, bool) {
	if !app.Cfg.OracleEnabled {
		return "oracle not enabled in config", false
	}
	if app.Exec == nil {
		return "briefing incomplete: no executor", false
	}
	planContent, err := app.Exec.ReadFile(ctx, app.Workflow.PlanPath)
	if err != nil {
		return "briefing incomplete: plan file unreadable", false
	}
	briefing := workflow.BuildOracleBriefing(app.Workflow.Task, planContent, question)
	if reason := workflow.ValidateBriefing(briefing, false); reason != "" {
		return "briefing incomplete: " + reason, false
	}
	panelName, panel := app.defaultPanel()
	result, _, ok := runWFPanel(ctx, app, "Workflow mashūra check?", question, briefing, panelName, panel)
	return result, ok
}

// wfAppendStepLogEntry appends a step-log entry extracted from the model's output
// to plan.md. Best-effort; errors are warned once per session.
func wfAppendStepLogEntry(app *App, entry string) {
	if app.Exec == nil {
		return
	}
	content, err := app.Exec.ReadFile(context.Background(), app.Workflow.PlanPath)
	if err != nil {
		if !app.planWriteFailed {
			app.planWriteFailed = true
			fmt.Fprintln(app.Out, Yellow("⚠ failed to read plan.md for step log: "+err.Error()))
		}
		return
	}
	if _, err := app.Exec.WriteFile(context.Background(), app.Workflow.PlanPath, workflow.WFAppendToStepLog(content, entry)); err != nil {
		if !app.planWriteFailed {
			app.planWriteFailed = true
			fmt.Fprintln(app.Out, Yellow("⚠ failed to write plan.md step log: "+err.Error()))
		}
	}
}

// handleFinalReview runs the closing oracle check after the last IMPLEMENT step.
// It may be called from two places:
//  1. handleWorkflowTransition (already inside the runTurn goroutine).
//  2. runFinalReview (a standalone Cmd goroutine started from WFFinalReviewMsg).
//
// Outcomes:
//   - Oracle OK, no gaps: workflow cleared (done).
//   - Oracle flags gaps: workflow stays in IMPLEMENT with StepIdx > StepCount;
//     user types /plan approve to force-close.
//   - Oracle unavailable: same loud path as REVIEW; /plan approve force-closes.
func HandleFinalReview(ctx context.Context, app *App) {
	if app.Workflow == nil {
		return
	}
	if !app.Cfg.WFFinalReview {
		app.Workflow = nil
		wfProgNote(app, "· workflow complete — all steps done (final review disabled)")
		return
	}

	// Run deterministic verification BEFORE the oracle review. Verification
	// results are appended to the step log (by RunVerification) so they
	// feed into the final-review briefing as machine evidence. If verification
	// fails, the workflow stays open (StepIdx > StepCount) — same state as
	// oracle gaps — so the user can remediate. Verification failure takes
	// precedence: even if the oracle says PASS, a failed test keeps the
	// workflow open (fail-closed: deterministic gate is authoritative).
	//
	// The outcome is recorded on WorkflowState (VerifyFailed/VerifyDeclined)
	// so the headless exit path can report the correct JSON outcome without
	// re-deriving the cause.
	app.Workflow.VerifyFailed = false
	app.Workflow.VerifyDeclined = false
	if app.VerifyEnabled {
		verifyOutcome := runFinalVerification(ctx, app)
		if verifyOutcome.HasFailures() {
			app.Workflow.VerifyFailed = true
			wfProgNote(app, "⚠ verification failed — workflow remains open")
			wfProgNote(app, "· fix the failing tests or type /plan approve to force-close")
			// Stay in WFImplement with StepIdx > StepCount (same as oracle gaps).
			return
		}
		if verifyOutcome.AnyDeclined() {
			app.Workflow.VerifyDeclined = true
			wfProgNote(app, "⚠ verification command declined — workflow remains open")
			wfProgNote(app, "· approve the verification command or type /plan approve to force-close")
			return
		}
	}

	wfProgNote(app, "· all steps complete — running final oracle review")
	reviewQ := "Does the step log demonstrate that every acceptance criterion was met? " +
		"List any criterion not verifiably satisfied and any deviation that was logged but not resolved. " +
		"End your response with exactly one of these two lines:\nVERDICT: PASS\nVERDICT: GAPS"

	if app.Exec == nil {
		wfProgNote(app, "⚠ FINAL REVIEW: oracle unavailable — briefing incomplete: no executor")
		wfWriteFinalLog(app, "FINAL REVIEW skipped: briefing incomplete (no executor) — /plan approve required to close.")
		wfProgNote(app, "· type /plan approve to force-close, or fix oracle config and retry")
		return
	}
	planContent, err := app.Exec.ReadFile(ctx, app.Workflow.PlanPath)
	if err != nil {
		wfProgNote(app, "⚠ FINAL REVIEW: oracle unavailable — briefing incomplete: plan file unreadable")
		wfWriteFinalLog(app, "FINAL REVIEW skipped: briefing incomplete (plan file unreadable) — /plan approve required to close.")
		wfProgNote(app, "· type /plan approve to force-close, or fix oracle config and retry")
		return
	}
	briefing := workflow.BuildFinalReviewBriefing(app.Workflow.Task, planContent, reviewQ, app.Cfg.WFBriefingMaxBytes)
	if reason := workflow.ValidateBriefing(briefing, true); reason != "" {
		wfProgNote(app, "⚠ FINAL REVIEW: oracle unavailable — briefing incomplete: "+reason)
		wfWriteFinalLog(app, "FINAL REVIEW skipped: briefing incomplete ("+reason+") — /plan approve required to close.")
		wfProgNote(app, "· type /plan approve to force-close, or fix oracle config and retry")
		return
	}
	panelName, panel := app.defaultPanel()
	oracleResult, panelResults, oracleAvail := doWFOracleWithBriefing(ctx, app, reviewQ, briefing, panelName, panel)
	if !oracleAvail {
		wfProgNote(app, "⚠ FINAL REVIEW: oracle unavailable — "+oracleResult)
		wfWriteFinalLog(app, "FINAL REVIEW skipped: oracle unavailable ("+oracleResult+") — /plan approve required to close.")
		wfProgNote(app, "· type /plan approve to force-close, or fix oracle config and retry")
		// Workflow stays in IMPLEMENT with StepIdx > StepCount.
		return
	}

	wfProgNote(app, "final review:\n"+oracleResult)

	// Record each model's verdict separately for multi-model panels.
	if len(panelResults) > 1 {
		for _, r := range panelResults {
			if r.Err != nil {
				wfWriteFinalLog(app, fmt.Sprintf("FINAL REVIEW [%s]: error — %v", r.Model, r.Err))
			} else if workflow.WFFlagsGaps(r.Answer) {
				wfWriteFinalLog(app, fmt.Sprintf("FINAL REVIEW [%s]: GAPS — %s", r.Model, workflow.GapGist(r.Answer)))
			} else {
				wfWriteFinalLog(app, fmt.Sprintf("FINAL REVIEW [%s]: PASS", r.Model))
			}
		}
	}

	// Fail-closed: any GAPS from any responding model keeps the workflow open.
	if panelFlagsGaps(panelResults) {
		wfWriteFinalLog(app, "FINAL REVIEW: gaps — "+workflow.GapGist(oracleResult))
		wfProgNote(app, "· gaps flagged — address them or type /plan approve to force-close")
		// Workflow stays in IMPLEMENT with StepIdx > StepCount.
		return
	}

	wfWriteFinalLog(app, "FINAL REVIEW: all acceptance criteria verified ✓")
	app.Workflow = nil
	wfProgNote(app, "· workflow complete — all acceptance criteria verified ✓")
}

// doWFOracleWithBriefing is the low-level variant used by HandleFinalReview: it
// receives a pre-built briefing and the resolved panel rather than rebuilding
// them. Returns (formatted result, raw results, available); available=false
// means the oracle could not run.
func doWFOracleWithBriefing(ctx context.Context, app *App, question, briefing, panelName string, panel config.MashuraPanelConfig) (string, []counsel.PanelMemberResult, bool) {
	if !app.Cfg.OracleEnabled {
		return "oracle not enabled in config", nil, false
	}
	return runWFPanel(ctx, app, "Workflow final review?", question, briefing, panelName, panel)
}

// wfWriteFinalLog appends a final-review log entry to plan.md. Best-effort.
func wfWriteFinalLog(app *App, entry string) {
	if app.Exec == nil || app.Workflow == nil {
		return
	}
	content, err := app.Exec.ReadFile(context.Background(), app.Workflow.PlanPath)
	if err != nil {
		return
	}
	if _, err := app.Exec.WriteFile(context.Background(), app.Workflow.PlanPath, workflow.WFAppendToStepLog(content, entry)); err != nil {
		wfProgNote(app, "⚠ step log write failed: "+err.Error())
	}
}

// handleReviewOracle runs the mandatory oracle plan review (WFReview phase).
// Assumes wf.Phase == WFReview. On success: advances to WFPresent — or, in
// auto mode (AutoApprove) with a fresh successful review, straight to
// WFImplement, returning the auto-turn message for step 1 (mirrors the
// headless auto-advance in runWorkflowLoop).
// On failure: stays in WFReview, stores reason, waits for /plan review or
// /plan approve — auto mode never skips a failed or unavailable review; only
// a successful review can be auto-approved.
// This is the single entry point for the review oracle — called both from initial
// plan completion and from the WFReview auto-retry path.
func HandleReviewOracle(ctx context.Context, app *App) *WFStartTurnMsg {
	wf := app.Workflow
	reviewQ := "Critically review this implementation plan. Identify missing steps, " +
		"incorrect assumptions, unclear acceptance criteria, risks, and improvements."
	oracleResult, oracleAvail := doWFOracle(ctx, app, reviewQ)

	if !oracleAvail {
		wf.OracleReview = ""
		wf.ReviewSkipReason = oracleResult
		wfProgNote(app, "⚠ REVIEW: oracle unavailable — "+oracleResult)
		wfWriteReviewSkip(app, oracleResult)
		wfProgNote(app, "· type /plan review to retry, or /plan approve to skip (reason will be logged)")
		return nil
	}

	wf.OracleReview = oracleResult
	wf.ReviewSkipReason = ""     // cleared on success
	wf.ReviewStaleWarned = false // a fresh review clears any pending stale warning

	// Fingerprint the ## Plan section so a later edit can be detected at approve time.
	if app.Exec != nil {
		if content, err := app.Exec.ReadFile(ctx, wf.PlanPath); err == nil {
			wf.ReviewPlanHash = workflow.HashPlanSection(content)
		}
	}

	wfProgNote(app, "oracle review:\n"+oracleResult)
	wf.Phase = workflow.WFPresent

	// Auto mode: a successful review with a parseable plan auto-approves and
	// starts implementation. The plan hash was fingerprinted moments ago, so
	// the stale-plan check the manual /plan approve performs cannot trip here.
	// StepCount==0 (empty plan section) never auto-advances — that state needs
	// a human decision.
	if app.Consent().AutoApprove && wf.StepCount > 0 {
		wf.Phase = workflow.WFImplement
		wf.StepIdx = 1
		wfProgNote(app, fmt.Sprintf("⚡ auto: plan approved after successful review (%d steps) — starting implementation", wf.StepCount))
		return &WFStartTurnMsg{
			Note:     fmt.Sprintf("auto-approved — starting implementation: step 1/%d", wf.StepCount),
			UserText: "continue",
		}
	}

	wfProgNote(app, fmt.Sprintf("· plan ready (%d steps) — type /plan approve to begin implementation", wf.StepCount))
	return nil
}

// wfWriteReviewSkip appends a REVIEW-skipped log entry to plan.md. Best-effort:
// errors are swallowed — the visible warning and the user gate are the real guards.
func wfWriteReviewSkip(app *App, reason string) {
	if app.Exec == nil {
		return
	}
	content, err := app.Exec.ReadFile(context.Background(), app.Workflow.PlanPath)
	if err != nil {
		return
	}
	entry := "REVIEW skipped: oracle unavailable (" + reason + ") — /plan approve required to proceed."
	if _, err := app.Exec.WriteFile(context.Background(), app.Workflow.PlanPath, workflow.WFAppendToStepLog(content, entry)); err != nil {
		wfProgNote(app, "⚠ step log write failed: "+err.Error())
	}
}

// wfAppendRemediationEvidence records one remediation turn's evidence in plan.md.
// It combines the model's %%STEP_LOG: Remediation: …%% summary (claim) with the
// deterministic tool-call trace (receipt) into a single step-log paragraph — the
// same claim-and-receipt structure used for normal IMPLEMENT steps. The append
// happens before handleFinalReview so the briefing carries up-to-date evidence.
func wfAppendRemediationEvidence(app *App) {
	if app.Exec == nil || app.Workflow == nil {
		return
	}
	// Extract model's reconciliation summary from the %%STEP_LOG: Remediation:…%% sentinel.
	var modelSummary string
	if text := workflow.LastAssistantText(app.Conv); text != "" {
		if entry := workflow.ExtractStepLogEntry(text); entry != "" {
			modelSummary = entry
		}
	}
	ev := FormatStepEvidence(app.WorkflowStepTrace)
	if modelSummary == "" && ev == "" {
		return // nothing to record
	}
	combined := modelSummary
	if ev != "" {
		if combined != "" {
			combined += "\n"
		}
		combined += ev
	}
	wfAppendStepLogEntry(app, combined)
}

// wfWriteReviewSkipForce appends "REVIEW skipped with reason: …" when the user
// explicitly force-skips the oracle review via /plan approve. Best-effort.
func WFWriteReviewSkipForce(app *App, reason string) {
	if app.Exec == nil || app.Workflow == nil {
		return
	}
	content, err := app.Exec.ReadFile(context.Background(), app.Workflow.PlanPath)
	if err != nil {
		return
	}
	entry := "REVIEW skipped with reason: " + reason + " (/plan approve used to force-skip)"
	if _, err := app.Exec.WriteFile(context.Background(), app.Workflow.PlanPath, workflow.WFAppendToStepLog(content, entry)); err != nil {
		wfProgNote(app, "⚠ step log write failed: "+err.Error())
	}
}

// wfWritePlanFormatError appends a format-error log entry to plan.md. Best-effort.
func wfWritePlanFormatError(app *App) {
	if app.Exec == nil {
		return
	}
	content, err := app.Exec.ReadFile(context.Background(), app.Workflow.PlanPath)
	if err != nil {
		return
	}
	entry := "PLAN FORMAT ERROR: ## Plan is non-empty but contains no numbered steps — model must reformat."
	if _, err := app.Exec.WriteFile(context.Background(), app.Workflow.PlanPath, workflow.WFAppendToStepLog(content, entry)); err != nil {
		wfProgNote(app, "⚠ step log write failed: "+err.Error())
	}
}
