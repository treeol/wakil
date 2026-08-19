// Legacy `--plan` workflow driver (card #148 chunk 7, plan D16): MOVED BODILY
// from cmd/wakil/run.go into the bootstrap package so cmd/wakil stops importing
// *agent.App, but NOT re-routed through the session host. It still drives
// App.Send directly via agent.HandleStreamError/HandleEmptyResponse/
// HandleWorkflowTransition/HandleFinalReview — the re-route is chunk 7c.
//
// The headless confirmer here is the legacy *declinedReason-capturing form; it
// shares the decision function headlessDecision with the single-task resolver
// (D18/D19) so the two paths cannot diverge on approval policy.
package wiring

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/workflow"
)

// HeadlessConfirmer returns an agent.Confirmer that implements the headless run
// policy using the shared headlessDecision function. The legacy form captures a
// *declinedReason for the caller to read after the turn (the host path uses the
// ApprovalResolution Reason instead — D18).
func HeadlessConfirmer(app *agent.App, opts HeadlessOptions, declinedReason *string) agent.Confirmer {
	return headlessConfirmer(app, opts, declinedReason)
}

// headlessConfirmer returns an agent.Confirmer that implements the headless run
// policy using the shared headlessDecision function. The legacy form captures a
// *declinedReason for the caller to read after the turn (the host path uses the
// ApprovalResolution Reason instead — D18).
func headlessConfirmer(app *agent.App, opts HeadlessOptions, declinedReason *string) agent.Confirmer {
	return func(toolName, headline, detail string, readAction bool) bool {
		choice, reason := headlessDecision(app, opts, ApprovalRequest{
			ToolName:   toolName,
			Headline:   headline,
			Detail:     detail,
			ReadAction: readAction,
		})
		if choice == agent.ChoiceDecline {
			*declinedReason = reason
			return false
		}
		return true
	}
}

// emitBackendFailure emits a backend_failure done event with the session ID so
// a wrapper script can detect the exit code and resume automatically.
func emitBackendFailure(app *agent.App, out io.Writer, err error) {
	emitEvent(out, map[string]any{
		"type":      "done",
		"outcome":   "backend_failure",
		"message":   err.Error(),
		"resume_id": agent.ShortID(app.Client.ChatID),
	})
}

// runWorkflowLegacy drives the plan workflow state machine on the directly-held
// App (NOT the session host — chunk 7c re-routes it). Mirrors cmd/wakil's
// runWorkflowHeadless + runWorkflowLoop verbatim, adjusted to HeadlessOptions.
func runWorkflowLegacy(ctx context.Context, app *agent.App, task string, opts HeadlessOptions, out io.Writer) int {
	hw := NewHeadlessWriter(out)
	if opts.TranscriptFile != "" {
		defer app.SaveSession()
	}
	defer hw.Flush()
	defer app.StopAllAsyncOps()

	var declinedReason string
	app.Out = hw
	app.Confirm = headlessConfirmer(app, opts, &declinedReason)
	app.Client.ResetGrounding()

	if opts.NoOracle {
		app.Cfg.OracleEnabled = false
	}

	planPath := filepath.Join(app.Exec.Cwd(), ".wakil", "plan.md")
	if _, err := app.Exec.RunShell(ctx, "mkdir -p .wakil"); err != nil {
		emitEvent(out, map[string]any{"type": "error", "message": "cannot create .wakil: " + err.Error()})
		return ExitError
	}
	if _, err := app.Exec.WriteFile(ctx, planPath, workflow.WFInitPlanContent(task)); err != nil {
		emitEvent(out, map[string]any{"type": "error", "message": "cannot write plan.md: " + err.Error()})
		return ExitError
	}
	app.SetWorkflow(&workflow.WorkflowState{
		Task:     task,
		Phase:    workflow.WFGather,
		PlanPath: planPath,
	})
	return runWorkflowLoopLegacy(ctx, app, opts, out, &declinedReason)
}

// RunWorkflowLoopLegacy drives the plan workflow state machine on an
// already-initialized app.Workflow (verbatim from cmd/wakil runWorkflowLoop).
func RunWorkflowLoopLegacy(ctx context.Context, app *agent.App, opts HeadlessOptions, out io.Writer, declinedReason *string) int {
	return runWorkflowLoopLegacy(ctx, app, opts, out, declinedReason)
}

// runWorkflowLoopLegacy drives the plan workflow state machine on an
// already-initialized app.Workflow (verbatim from cmd/wakil runWorkflowLoop).
func runWorkflowLoopLegacy(ctx context.Context, app *agent.App, opts HeadlessOptions, out io.Writer, declinedReason *string) int {
	for app.Workflow != nil {
		app.WorkflowStepTrace = nil
		app.Client.ResetGrounding()

		_, err := app.Send(ctx, "continue")
		if err = agent.HandleStreamError(ctx, app, err); err != nil {
			if errors.Is(err, proxy.ErrBackendStream) {
				emitBackendFailure(app, out, err)
				return ExitBackendFailure
			}
			emitEvent(out, map[string]any{"type": "error", "message": err.Error()})
			return ExitError
		}
		agent.HandleEmptyResponse(ctx, app)

		if *declinedReason != "" {
			emitEvent(out, map[string]any{
				"type": "done", "outcome": "declined", "reason": *declinedReason,
			})
			return ExitDeclined
		}
		if app.Workflow == nil {
			break
		}

		next := agent.HandleWorkflowTransition(ctx, app)
		if app.Workflow == nil {
			break // completed inside transition
		}
		if next != nil {
			continue // auto-turn requested
		}

		// Waiting for user action — auto-handle based on phase.
		switch app.Workflow.Phase {
		case workflow.WFPresent:
			// Auto-approve: skip stale-review check, advance to IMPLEMENT.
			app.Workflow.Phase = workflow.WFImplement
			app.Workflow.StepIdx = 1

		case workflow.WFReview:
			var reason, logReason string
			if opts.NoOracle {
				reason = "oracle disabled by flag (--no-oracle)"
				logReason = "oracle disabled by --no-oracle flag"
			} else {
				reason = "oracle review unavailable — " + app.Workflow.ReviewSkipReason
				logReason = "headless: oracle unavailable"
			}
			emitEvent(out, map[string]any{"type": "warning", "message": reason})
			agent.WFWriteReviewSkipForce(app, logReason)
			app.Workflow.Phase = workflow.WFPresent

		case workflow.WFImplement:
			if app.Workflow.StepIdx > app.Workflow.StepCount {
				if app.Workflow.VerifyDeclined || *declinedReason != "" {
					reason := *declinedReason
					if reason == "" {
						reason = "verification command declined by consent gate"
					}
					emitEvent(out, map[string]any{
						"type": "done", "outcome": "declined", "reason": reason,
					})
					return ExitDeclined
				}
				outcome := "gaps"
				msg := "final review flagged unresolved gaps"
				if app.Workflow.VerifyFailed {
					outcome = "verify_failed"
					msg = "verification failed (see step log for details)"
				}
				emitEvent(out, map[string]any{
					"type": "done", "outcome": outcome,
					"message": msg,
				})
				return ExitGaps
			}
			app.Workflow.StepIdx++
			if app.Workflow.StepIdx > app.Workflow.StepCount {
				agent.HandleFinalReview(ctx, app)
				if app.Workflow != nil {
					if app.Workflow.VerifyDeclined || *declinedReason != "" {
						reason := *declinedReason
						if reason == "" {
							reason = "verification command declined by consent gate"
						}
						emitEvent(out, map[string]any{
							"type": "done", "outcome": "declined", "reason": reason,
						})
						return ExitDeclined
					}
					outcome := "gaps"
					msg := "final review flagged unresolved gaps"
					if app.Workflow.VerifyFailed {
						outcome = "verify_failed"
						msg = "verification failed (see step log for details)"
					}
					emitEvent(out, map[string]any{"type": "done", "outcome": outcome, "message": msg})
					return ExitGaps
				}
			}

		default:
			emitEvent(out, map[string]any{
				"type":    "error",
				"message": fmt.Sprintf("unexpected waiting state: %v", app.Workflow.PhaseName()),
			})
			return ExitError
		}
	}

	emitEvent(out, map[string]any{"type": "done", "outcome": "pass"})
	return ExitOK
}
