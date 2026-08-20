package main

// headless_bridge_test.go adapts the moved headless driver (now in
// internal/wiring) back to the old cmd/wakil test-facing signatures, so the
// existing CLI tests keep exercising the re-routed driver without a large
// test-file rewrite. These wrappers are TEST-ONLY: production cmd/wakil code
// calls wiring.RunHeadless directly and does not import these.

import (
	"context"
	"io"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/wiring"
)

// runHeadlessApp is the old test entry: drive a pre-built App through single-
// task (host path) or --plan (legacy) and return the exit code.
func runHeadlessApp(ctx context.Context, app *agent.App, task string, planMode bool, flags RunFlags, out io.Writer) int {
	return wiring.RunHeadlessApp(ctx, app, task, planMode, headlessOpts(flags), out)
}

// runWorkflowLoop is the old test entry for the --plan loop over a pre-set
// workflow state. 7c: the loop is host-driven now — RunPlanLoop drives the
// session host over the caller-initialized app.Workflow.
func runWorkflowLoop(ctx context.Context, app *agent.App, flags RunFlags, out io.Writer, declinedReason *string) int {
	return wiring.RunPlanLoop(ctx, app, headlessOpts(flags), out)
}

// headlessConfirmer is the old test entry for the headless confirmer. 7c: the
// legacy wrapper is gone; the decision policy is headlessDecision (same for
// both paths), so the test entry builds an inline confirmer from it. The
// declinedReason latch keeps the old signature for the policy tests.
func headlessConfirmer(app *agent.App, flags RunFlags, declinedReason *string) agent.Confirmer {
	return func(toolName, headline, detail string, readAction bool) bool {
		choice, reason := wiring.HeadlessDecision(app, headlessOpts(flags), wiring.ApprovalRequest{
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

// closeResources is the old test entry for the resource cleanup path.
func closeResources(app *agent.App, res wiring.AppResources) {
	wiring.CloseResources(app, &res)
}

// appResources is a test alias so close_resources_test.go keeps compiling.
type appResources = wiring.AppResources

// loadAgentPrompt is the old test entry for prompt loading.
func loadAgentPrompt(cfg config.Config) string { return wiring.LoadAgentPrompt(cfg) }

// headlessOpts converts the CLI RunFlags (parse-level) into wiring options.
func headlessOpts(f RunFlags) wiring.HeadlessOptions {
	return wiring.HeadlessOptions{
		PlanMode:         false, // caller passes planMode explicitly where relevant
		Auto:             f.Auto,
		AllowDestructive: f.AllowDestructive,
		AllowExternal:    f.AllowExternal,
		NoOracle:         f.NoOracle,
		AutoCounsel:      f.AutoCounsel,
		MaxCounsel:       f.MaxCounsel,
		AttachImage:      f.AttachImage,
		PolicyPath:       f.PolicyPath,
		ProfileName:      f.ProfileName,
		Verify:           f.Verify,
		TranscriptFile:   f.TranscriptFile,
	}
}
