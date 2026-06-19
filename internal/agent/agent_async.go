package agent

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"wakil/internal/config"
	"wakil/internal/counsel"
	"wakil/internal/proxy"
	"wakil/internal/tools"
	"wakil/internal/workflow"

	tea "github.com/charmbracelet/bubbletea"
)

// runTurn launches the agent turn in a goroutine and returns a no-op tea.Cmd.
// All progress is posted into the TUI via globalProg.Send — safe because it
// runs in its own goroutine, outside the Bubble Tea event loop.
func RunTurn(app *App, ctx context.Context, userText string) tea.Cmd {
	return func() tea.Msg {
		app.Out = NewProgWriter(func(m StreamChunkMsg) { app.sendEvent(m) })
		app.Confirm = tuiConfirmer(app)
		app.OnTokRate = func(tps float64) { app.sendEvent(TokRateMsg{Tps: tps}) }
		app.OnReasoning = func(s string) { app.sendEvent(ReasoningChunkMsg{Text: s}) }
		// Dedup cache is for subagents only (dispatchSubagent sets it). The main
		// agent is interactive — the user can cancel loops via Ctrl+C, and
		// legitimate re-reads of the same file must not be silently suppressed.
		app.ToolCache = nil
		// Reset per-turn step evidence so each IMPLEMENT turn starts with a
		// clean trace. Consumed by handleWorkflowTransition on %%STEP_DONE%%.
		app.WorkflowStepTrace = nil
		// Clear grounding so proxy entries from the previous turn don't persist.
		// Header entries for this turn land at first Stream; client-side entries
		// (web/oracle) accumulate during tool execution.
		app.Client.ResetGrounding()

		_, err := app.Send(ctx, userText)

		// Retry transient backend failures in auto mode; surface fatal errors and
		// exhausted-retry state as tidy ⚠ lines rather than raw error traces.
		err = HandleStreamError(ctx, app, err)
		streamWarn := ""
		if errors.Is(err, proxy.ErrBackendStream) {
			streamWarn = "⚠ backend unreachable"
			if app.NearContextLimit() {
				streamWarn += " (near context limit — try /compact)"
			}
			if id := ShortID(app.Client.ChatID); id != "" {
				streamWarn += " — session saved, /resume " + id + " to continue"
			}
		} else if errors.Is(err, proxy.ErrBackendFatal) {
			// 4xx: surface the full message so the user can diagnose (bad request,
			// auth failure, etc.) — not a stream warn, it needs the detail.
			streamWarn = "⚠ " + err.Error()
			err = nil // don't double-render as AgentDoneMsg.Err
		}

		// Detect empty completions (no content, no tool calls) — likely token-limit
		// truncation. In IMPLEMENT phases retry once automatically; otherwise just
		// warn. Must run before handleWorkflowTransition so the transition sees the
		// retry response rather than the empty one.
		if err == nil {
			HandleEmptyResponse(ctx, app)
		}

		// Workflow phase detection: check for completion sentinels in the last
		// assistant message. All transitions run here in the goroutine so that
		// oracle calls can use the normal confirm gate before AgentDoneMsg fires.
		var wfNext *WFStartTurnMsg
		if app.Workflow != nil && err == nil {
			wfNext = HandleWorkflowTransition(ctx, app)
		}

		// Build end-of-turn nudge when: (a) the learn-candidate log fired this
		// turn, (b) at least one web or oracle grounding entry was added client-
		// side, and (c) this query hasn't been nudged already this session.
		nudge := ""
		pendingQuery := app.learnNudgePending
		app.learnNudgePending = "" // always clear
		if pendingQuery != "" && err == nil {
			for _, e := range app.Client.Grounding() {
				if e.Type == "web" || e.Type == "oracle" {
					if app.learnNudgedQueries == nil {
						app.learnNudgedQueries = make(map[string]bool)
					}
					if !app.learnNudgedQueries[pendingQuery] {
						app.learnNudgedQueries[pendingQuery] = true
						nudge = "· low grounding + external sources used — /learn to save this for next time"
					}
					break
				}
			}
		}
		// A surfaced stream error renders as the tidy warn line, not a raw err.
		doneErr := err
		if streamWarn != "" {
			doneErr = nil
		}
		app.sendEvent(AgentDoneMsg{Err: doneErr, LearnNudge: nudge, Warn: streamWarn})
		if wfNext != nil {
			app.sendEvent(*wfNext)
		}
		return nil
	}
}

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
				if content, err := app.Exec.ReadFile(wf.PlanPath); err == nil {
					if n := workflow.CountPlanSteps(content); n > 0 {
						wf.StepCount = n
						wf.PlanFormatInvalid = false
						wfProgNote(app, fmt.Sprintf("· plan re-parsed: %d steps — running oracle review", n))
						wf.Phase = workflow.WFReview
						HandleReviewOracle(ctx, app)
						return nil
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
			content, _ = app.Exec.ReadFile(wf.PlanPath)
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
		HandleReviewOracle(ctx, app)
		return nil

	case workflow.WFReview:
		// Any completed turn while in WFReview re-attempts the oracle review
		// automatically — mirroring the verify remediation loop.
		wfProgNote(app, "· re-attempting oracle plan review")
		HandleReviewOracle(ctx, app)
		return nil

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
				if content, err := app.Exec.ReadFile(wf.PlanPath); err == nil {
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
					wfProgNote(app, "⚠ step oracle unavailable (" + oracleResult + ")")
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
				wfProgNote(app, "oracle deviation advice:\n" + oracleResult)
			} else {
				wfProgNote(app, "⚠ oracle unavailable for deviation advice (" + oracleResult + ") — review the failure manually")
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

// isEmptyTurn reports whether the last assistant message in conv has no text
// content and no tool calls — an empty completion that typically indicates the
// model hit a token limit rather than producing a deliberate empty reply.
func IsEmptyTurn(conv []proxy.Message) bool {
	for i := len(conv) - 1; i >= 0; i-- {
		if conv[i].Role == "assistant" {
			return strings.TrimSpace(DerefStr(conv[i].Content)) == "" &&
				len(conv[i].ToolCalls) == 0
		}
	}
	return false
}

// handleEmptyResponse detects an empty-completion turn and, in IMPLEMENT phases,
// retries exactly once with a directive noting the truncation. A second empty
// response surfaces the condition to the user without touching workflow state
// (no phase or step transition fires from an empty response).
func HandleEmptyResponse(ctx context.Context, app *App) {
	if !IsEmptyTurn(app.Conv) {
		return
	}
	wfProgNote(app, "⚠ empty response (likely token-limit truncation)")

	if app.Workflow == nil || app.Workflow.Phase != workflow.WFImplement {
		return
	}

	// Single automatic retry: reset the step-evidence trace so it reflects
	// the retry turn, not the empty one.
	app.WorkflowStepTrace = nil
	const retryHint = "The previous response was empty — likely hit the token limit. " +
		"Please resume and complete the current implementation step."
	_, _ = app.Send(ctx, retryHint)

	if IsEmptyTurn(app.Conv) {
		// Retry also empty — surface without advancing state.
		wfProgNote(app, "⚠ retry also returned empty — "+
			"check token budget; workflow state unchanged")
	}
}

// streamRetryHint is sent as the user turn on each automatic retry after a
// backend error, so the model can resume the interrupted work.
const streamRetryHint = "The previous response was interrupted by a backend error. " +
	"Please resume and complete the current task."

// HandleStreamError handles errors from app.Send with retry logic for transient
// backend failures.
//
// Classification:
//   - nil / non-backend error → returned unchanged.
//   - ErrBackendFatal (4xx) → returned immediately, never retried.
//   - ErrBackendStream (5xx, reset, timeout) → retried in unattended runs
//     (AutoApprove or IsHeadless); passed through immediately in interactive
//     non-auto sessions so the human can decide to re-send.
//
// Retry loop: up to cfg.BackendMaxRetries attempts with exponential backoff
// (1s/2s/4s base + jitter). Each attempt is logged. When all retries fail with
// connection-reset symptoms, a "possibly deterministic" note is added — this
// distinguishes a transient infrastructure outage from a request that can never
// succeed (e.g. context-overflow resetting the connection every time).
func HandleStreamError(ctx context.Context, app *App, err error) error {
	if err == nil {
		return nil
	}
	// Fatal (4xx): bad request, auth — retrying is pointless.
	if errors.Is(err, proxy.ErrBackendFatal) {
		return err
	}
	// Non-stream errors pass through unchanged.
	if !errors.Is(err, proxy.ErrBackendStream) {
		return err
	}
	// Interactive non-auto: surface immediately; a human is present to re-send.
	if !app.AutoApprove && !app.IsHeadless {
		return err
	}

	maxRetries := app.Cfg.BackendMaxRetries
	if maxRetries <= 0 {
		maxRetries = 3
	}

	allStreamErrors := true // false if any retry produces a non-stream error
	var lastErr error
	for attempt := 1; attempt <= maxRetries; attempt++ {
		suffix := ""
		if app.NearContextLimit() {
			suffix = " (near context limit)"
		}
		backendNote(app, fmt.Sprintf("⚠ backend error%s — retry %d/%d", suffix, attempt, maxRetries))

		delay := retryBackoff(attempt-1, app.RetryDelay)
		if delay > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
		}

		app.WorkflowStepTrace = nil
		_, rerr := app.Send(ctx, streamRetryHint)
		if rerr == nil {
			return nil // recovered
		}
		if !errors.Is(rerr, proxy.ErrBackendStream) {
			allStreamErrors = false
		}
		lastErr = rerr
	}

	// All retries exhausted — annotate based on failure pattern.
	if allStreamErrors && app.NearContextLimit() {
		backendNote(app, "⚠ persistent stream errors near context limit — possible request-size issue; try /compact")
	} else if allStreamErrors {
		backendNote(app, "⚠ persistent stream errors — possible deterministic backend failure (context overflow?)")
	} else {
		backendNote(app, fmt.Sprintf("⚠ backend unreachable after %d retries", maxRetries))
	}
	return lastErr
}

// retryBackoff returns the wait duration before retry attempt n (0-based).
// The override function (App.RetryDelay) is used when set (tests); otherwise
// the standard 1s·2^n + jitter schedule.
func retryBackoff(n int, override func(int) time.Duration) time.Duration {
	if override != nil {
		return override(n)
	}
	base := time.Duration(1<<uint(n)) * time.Second
	jitter := time.Duration(rand.Int63n(int64(base / 2)))
	return base + jitter
}

// backendNote logs a backend-resilience message to the appropriate sink.
// In a workflow it uses the workflow progress channel; otherwise it writes to
// app.Out so headless and free-chat sessions see the line.
func backendNote(app *App, text string) {
	if app.Workflow != nil {
		wfProgNote(app, text)
		return
	}
	fmt.Fprintln(app.Out, text)
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
			app.Client.AddGrounding(proxy.GroundingEntry{Type: "oracle", Label: r.Model})
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
	planContent, err := app.Exec.ReadFile(app.Workflow.PlanPath)
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
// to plan.md. Best-effort; errors are swallowed.
func wfAppendStepLogEntry(app *App, entry string) {
	if app.Exec == nil {
		return
	}
	content, err := app.Exec.ReadFile(app.Workflow.PlanPath)
	if err != nil {
		return
	}
	_, _ = app.Exec.WriteFile(app.Workflow.PlanPath, workflow.WFAppendToStepLog(content, entry))
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
	planContent, err := app.Exec.ReadFile(app.Workflow.PlanPath)
	if err != nil {
		wfProgNote(app, "⚠ FINAL REVIEW: oracle unavailable — briefing incomplete: plan file unreadable")
		wfWriteFinalLog(app, "FINAL REVIEW skipped: briefing incomplete (plan file unreadable) — /plan approve required to close.")
		wfProgNote(app, "· type /plan approve to force-close, or fix oracle config and retry")
		return
	}
	briefing := workflow.BuildFinalReviewBriefing(app.Workflow.Task, planContent, reviewQ, app.Cfg.WFBriefingMaxBytes)
	if reason := workflow.ValidateBriefing(briefing, true); reason != "" {
		wfProgNote(app, "⚠ FINAL REVIEW: oracle unavailable — briefing incomplete: " + reason)
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

// runFinalReview is the Cmd that the TUI fires when WFFinalReviewMsg arrives.
// It sets up the oracle/confirm infrastructure, runs handleFinalReview, then
// signals completion via AgentDoneMsg.
func RunFinalReview(app *App, ctx context.Context) tea.Cmd {
	return func() tea.Msg {
		app.Out = NewProgWriter(func(m StreamChunkMsg) { app.sendEvent(m) })
		app.Confirm = tuiConfirmer(app)
		app.Client.ResetGrounding()
		HandleFinalReview(ctx, app)
		app.sendEvent(AgentDoneMsg{Err: nil})
		return nil
	}
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
	content, err := app.Exec.ReadFile(app.Workflow.PlanPath)
	if err != nil {
		return
	}
	_, _ = app.Exec.WriteFile(app.Workflow.PlanPath, workflow.WFAppendToStepLog(content, entry))
}

// handleReviewOracle runs the mandatory oracle plan review (WFReview phase).
// Assumes wf.Phase == WFReview. On success: advances to WFPresent.
// On failure: stays in WFReview, stores reason, waits for /plan review or /plan approve.
// This is the single entry point for the review oracle — called both from initial
// plan completion and from the WFReview auto-retry path.
func HandleReviewOracle(ctx context.Context, app *App) {
	wf := app.Workflow
	reviewQ := "Critically review this implementation plan. Identify missing steps, " +
		"incorrect assumptions, unclear acceptance criteria, risks, and improvements."
	oracleResult, oracleAvail := doWFOracle(ctx, app, reviewQ)

	if !oracleAvail {
		wf.OracleReview = ""
		wf.ReviewSkipReason = oracleResult
		wfProgNote(app, "⚠ REVIEW: oracle unavailable — " + oracleResult)
		wfWriteReviewSkip(app, oracleResult)
		wfProgNote(app, "· type /plan review to retry, or /plan approve to skip (reason will be logged)")
		return
	}

	wf.OracleReview = oracleResult
	wf.ReviewSkipReason = ""     // cleared on success
	wf.ReviewStaleWarned = false // a fresh review clears any pending stale warning

	// Fingerprint the ## Plan section so a later edit can be detected at approve time.
	if app.Exec != nil {
		if content, err := app.Exec.ReadFile(wf.PlanPath); err == nil {
			wf.ReviewPlanHash = workflow.HashPlanSection(content)
		}
	}

	wfProgNote(app, "oracle review:\n" + oracleResult)
	wf.Phase = workflow.WFPresent
	wfProgNote(app, fmt.Sprintf("· plan ready (%d steps) — type /plan approve to begin implementation", wf.StepCount))
}

// wfWriteReviewSkip appends a REVIEW-skipped log entry to plan.md. Best-effort:
// errors are swallowed — the visible warning and the user gate are the real guards.
func wfWriteReviewSkip(app *App, reason string) {
	if app.Exec == nil {
		return
	}
	content, err := app.Exec.ReadFile(app.Workflow.PlanPath)
	if err != nil {
		return
	}
	entry := "REVIEW skipped: oracle unavailable (" + reason + ") — /plan approve required to proceed."
	_, _ = app.Exec.WriteFile(app.Workflow.PlanPath, workflow.WFAppendToStepLog(content, entry))
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
	content, err := app.Exec.ReadFile(app.Workflow.PlanPath)
	if err != nil {
		return
	}
	entry := "REVIEW skipped with reason: " + reason + " (/plan approve used to force-skip)"
	_, _ = app.Exec.WriteFile(app.Workflow.PlanPath, workflow.WFAppendToStepLog(content, entry))
}

// wfWritePlanFormatError appends a format-error log entry to plan.md. Best-effort.
func wfWritePlanFormatError(app *App) {
	if app.Exec == nil {
		return
	}
	content, err := app.Exec.ReadFile(app.Workflow.PlanPath)
	if err != nil {
		return
	}
	entry := "PLAN FORMAT ERROR: ## Plan is non-empty but contains no numbered steps — model must reformat."
	_, _ = app.Exec.WriteFile(app.Workflow.PlanPath, workflow.WFAppendToStepLog(content, entry))
}

// tuiConfirmer pauses the agent goroutine and posts a ConfirmReqMsg into the
// TUI event loop. It blocks on the response channel until the user answers.
// Picking "allow all reads" flips app.AllowReads so later read-only commands
// skip the gate. Safe: runs in the agent goroutine, not in the event loop.
// suspendAuto returns a human-readable reason string when auto mode must be
// suspended for this tool call and the interactive gate must fire instead.
// Returns "" when auto mode may proceed without gating.
//
// Every carve-out routes through here so no fall-through can occur without
// a reason, making all auto-suspensions visible and auditable.
func SuspendAuto(toolName string, app *App, detail string) string {
	// Mashūra calls (and the legacy oracle__ask alias) send data to an external
	// AI; cost and payload always need human review, even in auto mode.
	if tools.IsMashuraTool(toolName) {
		return "mashūra call (cost + payload review)"
	}
	switch toolName {
	case "external_backend":
		// Egress consent gate: session context would be sent to an external backend.
		// Always requires explicit approval — never auto-approved, even in /auto.
		return "external backend egress (privacy gate)"
	case "run_shell":
		if IsDestructiveShell(ShellCmdFromDetail(detail)) {
			return "destructive command"
		}
		if app.Workflow != nil && workflow.IsPreImplementPhase(app.Workflow.Phase) {
			return "pre-implementation phase (" + app.Workflow.PhaseName() + ")"
		}
	}
	return ""
}

// shouldGateEvenWithAutoApprove is a thin predicate wrapper around suspendAuto
// for callers that only need a boolean.
func ShouldGateEvenWithAutoApprove(toolName string, app *App, detail string) bool {
	return SuspendAuto(toolName, app, detail) != ""
}

func tuiConfirmer(app *App) Confirmer {
	return func(toolName, headline, detail string, readAction bool) bool {
		if app.AutoApprove {
			reason := SuspendAuto(toolName, app, detail)
			if reason == "" {
				app.sendEvent(SysNoteMsg{Text: "⚡ auto: " + headline + "\n" + Indent(detail)})
				return true
			}
			// Auto suspended — prefix the headline so the first line of the
			// confirm prompt states the cause. headline is a local copy; the
			// tool name and detail passed to the gate are unchanged.
			headline = "⚡ auto suspended: " + reason + " — " + headline
		}
		ch := make(chan ConfirmChoice, 1)
		app.sendEvent(ConfirmReqMsg{
			ToolName:   toolName,
			Headline:   headline,
			Detail:     detail,
			ReadAction: readAction,
			RespCh:     ch,
		})
		switch <-ch {
		case ChoiceAllowReads:
			app.AllowReads = true
			return true
		case ChoiceApprove:
			return true
		default:
			return false
		}
	}
}

// resolveBackendCtxCmd returns a tea.Cmd that probes the new backend's context
// window in a goroutine and delivers the result as a BackendCtxLimitMsg. The
// TUI event loop applies the update to app.CtxLimit when the msg is handled,
// keeping it out of the goroutine to avoid races with concurrent agent turns.
func resolveBackendCtxCmd(app *App, backend string) tea.Cmd {
	return func() tea.Msg {
		var buf strings.Builder
		lim := ResolveContextLimitForBackend(context.Background(), app.Client.HTTP, app.Cfg, backend, &buf)
		return BackendCtxLimitMsg{Limit: lim, Note: strings.TrimSpace(buf.String())}
	}
}

// shellCmdFromDetail extracts the raw shell command from the detail string that
// app.go passes to Confirmer for run_shell calls. The format is:
//
//	"$ <command>\n  (<exec>, cwd=<path>)"
func ShellCmdFromDetail(detail string) string {
	line, _, _ := strings.Cut(detail, "\n")
	return strings.TrimPrefix(strings.TrimSpace(line), "$ ")
}

// handleTUICommand processes slash commands locally without touching the agent.
// Returns (handled, quit, cmd) where cmd is a tea.Cmd that produces the
// response message. All messages are returned as Cmds — never via
// globalProg.Send — because this function is called from within Update, and
// calling Send from inside the event loop risks a deadlock.
func HandleTUICommand(line string, app *App) (handled, quit bool, cmd tea.Cmd) {
	line = strings.TrimSpace(line)
	if !strings.HasPrefix(line, "/") {
		return false, false, nil
	}
	fields := strings.Fields(line)

	note := func(text string) tea.Cmd {
		return func() tea.Msg { return SysNoteMsg{Text: text} }
	}

	switch fields[0] {
	case "/new", "/reset":
		app.NewConversation(NewChatID())
		chatID := ShortID(app.Client.ChatID)
		return true, false, func() tea.Msg {
			return NewConvMsg{Note: "fresh conversation: " + chatID}
		}

	case "/cwd":
		return true, false, note("cwd: " + app.Exec.Cwd())

	case "/mode":
		return true, false, note("exec: " + app.Exec.Describe())

	case "/history":
		return true, false, note(fmt.Sprintf("%d messages, ~%d chars (max %d)",
			len(app.Conv), TranscriptSize(app.Conv), app.Cfg.MaxChars))

	case "/auto":
		app.AutoApprove = !app.AutoApprove
		if app.AutoApprove {
			return true, false, note("auto mode: ON — all tool calls approved without prompting")
		}
		return true, false, note("auto mode: OFF — tool calls require confirmation")

	case "/rawtools":
		app.RawTools = !app.RawTools
		if app.RawTools {
			return true, false, note("raw tool results: ON — full output kept in context (cap disabled)")
		}
		cap := app.Cfg.ToolResultCap
		if cap <= 0 {
			return true, false, note("raw tool results: OFF — cap is set to unlimited in config")
		}
		return true, false, note(fmt.Sprintf("raw tool results: OFF — results capped at %d chars", cap))

	case "/compact":
		return true, false, func() tea.Msg {
			ok, err := app.Compact(context.Background(), app.summarizeFn(), true)
			if err != nil {
				return SysNoteMsg{Text: "compact: " + err.Error()}
			}
			if !ok {
				return SysNoteMsg{Text: "nothing to compact (transcript fits within keep_bytes window)"}
			}
			app.SaveSession()
			return CompactedMsg{}
		}

	case "/sessions":
		return true, false, note(SessionListText(app.Client.ChatID))

	case "/resume":
		prefix := ""
		if len(fields) > 1 {
			prefix = fields[1]
		}
		return true, false, func() tea.Msg {
			s, err := LoadSession(prefix)
			if err != nil {
				return SysNoteMsg{Text: "resume: " + err.Error()}
			}
			app.Conv = s.Conv
			app.Client.ChatID = s.ChatID
			app.Session = s
			app.Workflow = s.SavedWorkflow
			msg := "resumed session " + ShortID(s.ChatID)
			if s.Label != "" {
				msg += " [" + s.Label + "]"
			}
			msg += fmt.Sprintf(" — %d messages", len(s.Conv))
			if s.SavedWorkflow != nil {
				msg += " · workflow restored: " + s.SavedWorkflow.PhaseName()
			}
			return NewConvMsg{Note: msg, RebuildConv: true}
		}

	case "/session":
		if len(fields) >= 3 && fields[1] == "name" {
			label := strings.Join(fields[2:], " ")
			label = strings.Trim(label, `"'`)
			if app.Session == nil {
				return true, false, note("no active session")
			}
			app.Session.Label = label
			app.SaveSession()
			return true, false, note("session labeled: " + label)
		}
		return true, false, note(`usage: /session name "<label>"`)

	case "/mcp":
		args := fields[1:]
		// /mcp reconnect NAME — blocking network call; run in the Cmd goroutine.
		if len(args) >= 2 && args[0] == "reconnect" {
			name := strings.Join(args[1:], " ")
			return true, false, func() tea.Msg {
				if app.MCP == nil {
					return SysNoteMsg{Text: "no MCP servers configured"}
				}
				if err := app.MCP.Reconnect(context.Background(), name); err != nil {
					return SysNoteMsg{Text: fmt.Sprintf("reconnect %q: %v", name, err)}
				}
				toolList := BuildTools(app.Cfg, app.Exec.Cwd(), app.MCP)
				return MCPReconnectedMsg{Name: name, Tools: toolList}
			}
		}
		// /mcp list — fast, compute in-line but still return as Cmd.
		return true, false, note(mcpStatus(args, app))

	case "/backend":
		// /backend [<name>[/<model-path>]] — set or show the current backend selection.
		// When <name> contains a slash (e.g. "openrouter/anthropic/claude-opus-4-8"),
		// the part before the first slash is the backend name and the full string is
		// sent as the model field so the proxy can route by model prefix.
		if len(fields) >= 2 {
			arg := fields[1]
			if idx := strings.Index(arg, "/"); idx >= 0 {
				app.SelectedBackend = arg[:idx]
				app.SelectedModel = arg
			} else {
				app.SelectedBackend = arg
				app.SelectedModel = ""
			}
			selected := app.SelectedBackend
			msg := "backend: set to " + selected
			if app.SelectedModel != "" {
				msg += " · model: " + app.SelectedModel
			}
			// Re-probe context limits for the new backend so dynamic thresholds
			// (compact_at_frac etc.) scale to the new window. The result arrives
			// as BackendCtxLimitMsg and is applied safely in the TUI event loop.
			return true, false, tea.Batch(note(msg), resolveBackendCtxCmd(app, selected))
		}
		// No arg: report current selection and last-used.
		cur := app.SelectedBackend
		if cur == "" {
			cur = "(proxy default)"
		}
		used := ""
		if app.Client != nil {
			used = app.Client.LastUsedBackend()
		}
		if used == "" {
			used = "(none yet)"
		}
		msg := "backend: selected=" + cur + " · last-used=" + used
		if app.SelectedModel != "" {
			msg += " · model=" + app.SelectedModel
		}
		return true, false, note(msg)

	case "/model":
		// /model [<name>] — set or show the model override for this session.
		// Unlike /backend <name/model>, /model acts on the model field only,
		// leaving the current backend selection unchanged.
		if len(fields) >= 2 {
			app.SelectedModel = fields[1]
			return true, false, note("model: set to " + fields[1])
		}
		cur := app.SelectedModel
		if cur == "" {
			cur = app.Client.Model
		}
		return true, false, note("model: " + cur)

	case "/counsel":
		// /counsel [auto|suggest|off] — set or show the auto-counsel mode.
		if len(fields) < 2 {
			mode := app.CounselMode
			if mode == "" {
				mode = "suggest"
			}
			msg := "counsel mode: " + mode
			if mode == "auto" {
				msg += fmt.Sprintf(" (cap: %d/turn)", app.MaxCounsel)
			}
			return true, false, note(msg)
		}
		switch fields[1] {
		case "auto":
			cap := app.MaxCounsel
			if cap <= 0 {
				cap = app.Cfg.CounselMaxPerSession
				if cap <= 0 {
					cap = 3
				}
			}
			app.CounselMode = "auto"
			app.MaxCounsel = cap
			return true, false, note(fmt.Sprintf("counsel mode: auto (cap: %d/turn)", cap))
		case "suggest":
			app.CounselMode = "suggest"
			return true, false, note("counsel mode: suggest (hint only, no auto-fire)")
		case "off":
			app.CounselMode = "off"
			return true, false, note("counsel mode: off (struggle detected silently)")
		default:
			return true, false, note("usage: /counsel auto|suggest|off")
		}

	case "/plan":
		return HandlePlanCommand(fields, app)

	case "/learn":
		return true, false, func() tea.Msg { return LearnTurnMsg{} }

	case "/help":
		return true, false, note(helpTextTUI)

	case "/quit", "/exit":
		return true, true, nil

	default:
		return true, false, note("unknown command — /help for the list")
	}
}

// handlePlanCommand processes all /plan subcommands. Called from handleTUICommand.
func HandlePlanCommand(fields []string, app *App) (handled, quit bool, cmd tea.Cmd) {
	note := func(text string) tea.Cmd {
		return func() tea.Msg { return SysNoteMsg{Text: text} }
	}

	if len(fields) < 2 {
		return true, false, note("usage: /plan <task> | /plan status | /plan abort | /plan approve")
	}

	switch fields[1] {
	case "status":
		if app.Workflow == nil {
			return true, false, note("no active workflow")
		}
		return true, false, note("workflow:\n" + app.Workflow.StatusString())

	case "abort":
		app.Workflow = nil
		return true, false, note("workflow aborted")

	case "verify":
		if app.Workflow == nil ||
			app.Workflow.Phase != workflow.WFImplement ||
			app.Workflow.StepIdx <= app.Workflow.StepCount {
			return true, false, note("no active workflow in verify state (/plan verify is for after all steps complete)")
		}
		return true, false, func() tea.Msg { return WFFinalReviewMsg{} }

	case "review":
		// Phase acknowledgments (/plan approve, /plan review) are ONLY ever
		// user-typed commands — handlePlanCommand is only called from handleKey
		// which requires a physical tea.KeyMsg. No auto-mode path can trigger them.
		if app.Workflow == nil ||
			(app.Workflow.Phase != workflow.WFReview && app.Workflow.Phase != workflow.WFPresent) {
			return true, false, note("no active workflow in review state (/plan review works from WFReview or WFPresent)")
		}
		// Transition to WFReview so the WFReview auto-retry path in
		// handleWorkflowTransition picks up and calls handleReviewOracle when
		// the turn completes. For WFReview this is a no-op; for WFPresent it
		// enables the voluntary re-review that refreshes ReviewPlanHash.
		app.Workflow.Phase = workflow.WFReview
		return true, false, func() tea.Msg {
			return WFStartTurnMsg{Note: "running oracle plan review", UserText: "continue"}
		}

	case "approve":
		// Phase acknowledgments are only ever user-typed — see /plan review above.
		if app.Workflow == nil {
			return true, false, note("no active workflow (use /plan <task> to start one)")
		}
		switch app.Workflow.Phase {
		case workflow.WFReview:
			// User force-skips the oracle review. Log the reason so the plan file
			// is an honest record: "REVIEW skipped with reason: <why oracle failed>".
			reason := app.Workflow.ReviewSkipReason
			if reason == "" {
				reason = "oracle review was unavailable"
			}
			app.Workflow.ReviewSkipReason = ""
			WFWriteReviewSkipForce(app, reason)
			app.Workflow.Phase = workflow.WFPresent
			stepLabel := strconv.Itoa(app.Workflow.StepCount)
			return true, false, note(fmt.Sprintf(
				"· oracle review skipped (logged) — plan ready (%s steps)\n"+
					"  type /plan approve again to begin step-by-step implementation", stepLabel))

		case workflow.WFPresent:
			// Stale-review detection: if ## Plan changed since the oracle reviewed it,
			// warn and require a second approve. Phase acknowledgments are user-only
			// (see /plan review comment above) so this gate cannot be auto-bypassed.
			stepLabel := strconv.Itoa(app.Workflow.StepCount)
			return true, false, func() tea.Msg {
				wf := app.Workflow
				if wf == nil {
					return SysNoteMsg{Text: "no active workflow"}
				}
				// Check for plan modification since last review.
				if wf.ReviewPlanHash != "" && app.Exec != nil {
					if content, err := app.Exec.ReadFile(wf.PlanPath); err == nil {
						if workflow.HashPlanSection(content) != wf.ReviewPlanHash && !wf.ReviewStaleWarned {
							wf.ReviewStaleWarned = true
							return SysNoteMsg{Text: "⚠ plan modified since last review — " +
								"/plan review recommended (approve again to proceed anyway)"}
						}
					}
				}
				// Second approve (warned) or no hash stored — proceed.
				wf.ReviewStaleWarned = false
				wf.Phase = workflow.WFImplement
				wf.StepIdx = 1
				return WFStartTurnMsg{
					Note:     "approved — starting implementation: step 1/" + stepLabel,
					UserText: "continue",
				}
			}

		case workflow.WFImplement:
			wf := app.Workflow
			if wf.StepIdx > wf.StepCount {
				// Force-close from verify state. Log that flags were not resolved
				// so the step log is an honest record of the workflow outcome.
				wfWriteFinalLog(app, "FINAL REVIEW: workflow force-closed with unresolved flags.")
				app.Workflow = nil
				return true, false, note("· workflow force-closed (unresolved flags logged to step log)")
			}
			// Paused by every-step oracle critique — advance to next step.
			wf.StepIdx++
			if wf.StepIdx > wf.StepCount {
				// The paused step was the last — run the final review now.
				if app.Cfg.WFFinalReview {
					return true, false, func() tea.Msg { return WFFinalReviewMsg{} }
				}
				app.Workflow = nil
				return true, false, note("· workflow complete — all steps done")
			}
			stepLabel := strconv.Itoa(wf.StepCount)
			return true, false, func() tea.Msg {
				return WFStartTurnMsg{
					Note:     fmt.Sprintf("oracle critique acknowledged — step %d/%s", wf.StepIdx, stepLabel),
					UserText: "continue",
				}
			}

		default:
			return true, false, note("no workflow awaiting approval (use /plan status)")
		}

	default:
		// /plan [--oracle=MODE] <task text>
		// Parse optional --oracle=VALUE flag; remaining tokens form the task.
		var oracleMode string
		var taskParts []string
		for _, f := range fields[1:] {
			if strings.HasPrefix(f, "--oracle=") {
				oracleMode = strings.TrimPrefix(f, "--oracle=")
			} else {
				taskParts = append(taskParts, f)
			}
		}
		if oracleMode != "" {
			switch oracleMode {
			case "every-step", "on-deviation", "phases-only":
				// valid
			default:
				return true, false, note("unknown oracle mode " + strconv.Quote(oracleMode) +
					" — use every-step, on-deviation, or phases-only")
			}
		}
		if len(taskParts) == 0 {
			return true, false, note("usage: /plan [--oracle=MODE] <task>")
		}
		task := strings.Join(taskParts, " ")
		capturedOracleMode := oracleMode
		return true, false, func() tea.Msg {
			content := workflow.WFInitPlanContent(task)
			// Resolve the plan path to absolute once, using the executor's cwd at
			// workflow start. All subsequent readers use this absolute path so that
			// later cwd changes inside the executor cannot misroute a read or write.
			planPath := ".wakil/plan.md"
			if app.Exec != nil {
				planPath = filepath.Join(app.Exec.Cwd(), ".wakil", "plan.md")
				if _, err := app.Exec.RunShell(context.Background(), "mkdir -p .wakil"); err != nil {
					return SysNoteMsg{Text: "workflow: could not create .wakil dir: " + err.Error()}
				}
				if _, err := app.Exec.WriteFile(planPath, content); err != nil {
					return SysNoteMsg{Text: "workflow: could not write plan.md: " + err.Error()}
				}
			}
			app.Workflow = &workflow.WorkflowState{
				Task:       task,
				Phase:      workflow.WFGather,
				PlanPath:   planPath,
				OracleMode: capturedOracleMode,
			}
			note := "workflow started: gather — " + Truncate(task, 60)
			if capturedOracleMode != "" {
				note += " (oracle: " + capturedOracleMode + ")"
			}
			return WFStartTurnMsg{Note: note, UserText: "continue"}
		}
	}
}

// mcpStatus builds the /mcp listing string.
func mcpStatus(args []string, app *App) string {
	hasSomething := (app.MCP != nil && len(app.MCP.Servers()) > 0) || app.Cfg.SearXngURL != ""
	if !hasSomething {
		return "no tool servers configured (add mcp_servers or searxng_url to config)"
	}

	var sb strings.Builder
	if app.MCP != nil {
		for _, srv := range app.MCP.Servers() {
			icon := "✓"
			if srv.Status == "failed" {
				icon = "✗"
			} else if srv.Status == "connecting" {
				icon = "…"
			}
			sb.WriteString(fmt.Sprintf("%s %s [%s] (mcp)", icon, srv.Cfg.Name, srv.Status))
			if srv.Err != nil {
				sb.WriteString(": " + srv.Err.Error())
			}
			sb.WriteByte('\n')
			for _, t := range srv.Tools {
				sb.WriteString(fmt.Sprintf("    • %s%s%s: %s\n", srv.Cfg.Name, mcpNS, t.Name, t.Description))
			}
		}
	}
	if app.Cfg.SearXngURL != "" {
		sb.WriteString("✓ searxng [connected] (native)\n")
		for _, t := range tools.SearxngTools() {
			sb.WriteString(fmt.Sprintf("    • %s: %s\n", t.Function.Name, Truncate(t.Function.Description, 70)))
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

const helpTextTUI = `/new, /reset         fresh conversation (new chat_id, clears viewport)
/backend <name>      set the backend for this session (X-Ilm-Backend header)
/backend <name/model> set backend + model (e.g. openrouter/anthropic/claude-opus-4-8)
/backend             show current backend selection and last-used backend
/model <name>        set the model for this session (overrides backend default)
/model               show current model
/plan <task>         start a gather→plan→review→implement workflow for <task>
/plan --oracle=MODE  set per-run oracle schedule (every-step|on-deviation|phases-only)
/plan status         show current workflow phase and step
/plan approve        approve the plan; force-skip review (logged); advance past pauses
/plan review         retry the oracle plan review (when review is pending/unavailable)
/plan verify         re-run the final oracle review (in verify state after gaps flagged)
/plan abort          cancel the active workflow
/compact             summarize older turns now (frees context, improves performance)
/learn               send "learn this for next time" — proxy synthesises a fact to save
/counsel auto|suggest|off  auto-counsel mode: auto=fire mashura__debug on struggle, suggest=hint, off=silent
/counsel                   show current counsel mode and per-turn cap
/auto                toggle: auto-approve all tool calls without prompting (shown as AUTO in status)
/rawtools            toggle: include full tool output in context (default: capped at 8k chars)
/cwd                 show executor working directory
/mode                show execution backend
/history             transcript size
/sessions            list saved sessions (★ = current)
/resume [<id>]       resume a saved session by id prefix; omit for most recent
/session name "..."  label the current session (shown in /sessions listing)
/mcp                 list tool servers and tools
/mcp reconnect NAME  reconnect a named MCP server
/help                this help
/quit, /exit         leave (ctrl+c in idle also quits)

sessions are saved automatically; resume with: wakil --resume  (or --resume-id <id>)
or switch sessions mid-run with /resume inside the TUI

@path                attach a file/folder for context (picker pops up after "@")
                     reads host files for context; editing them needs --exec direct

scroll the conversation with the mouse wheel or PgUp/PgDn
drag with the mouse to select text — it's copied to the clipboard on release`
