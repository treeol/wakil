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
// workflow state.
func runWorkflowLoop(ctx context.Context, app *agent.App, flags RunFlags, out io.Writer, declinedReason *string) int {
	return wiring.RunWorkflowLoopLegacy(ctx, app, headlessOpts(flags), out, declinedReason)
}

// headlessConfirmer is the old test entry for the legacy confirmer.
func headlessConfirmer(app *agent.App, flags RunFlags, declinedReason *string) agent.Confirmer {
	return wiring.HeadlessConfirmer(app, headlessOpts(flags), declinedReason)
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
