package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/verify"
)

// verify.go: orchestration layer for the workflow verification runner.
//
// This file is the bridge between the pure internal/verify/ package (detection,
// result types, formatting) and the agent's execution + consent infrastructure.
//
// SECURITY: Verification commands are repo-derived — detected from project
// manifest files (go.mod, package.json, …). A malicious repository could plant
// a manifest whose "test" script does anything. Therefore every command MUST
// route through app.Confirm (policy + SuspendAuto), the same gate used for
// run_shell. Direct app.Exec.RunShell calls here would bypass policy and are
// forbidden. See the Mashūra plan review for the analysis.

// verifyTimeout is the per-command timeout for verification runs. Verification
// commands (especially test suites) can hang in watch mode or on network calls;
// a timeout prevents an unattended run from hanging forever.
const verifyTimeout = 5 * time.Minute

// ResolveVerifyCommands determines which verification commands to run.
// Explicit config (cfg.Verify) always wins over auto-detection.
// When config is empty, detects from project manifests.
func ResolveVerifyCommands(app *App) []verify.Command {
	// Explicit config wins.
	if len(app.Cfg.Verify) > 0 {
		cmds := make([]verify.Command, len(app.Cfg.Verify))
		for i, c := range app.Cfg.Verify {
			cmds[i] = verify.Command{Cmd: c, Source: "config"}
		}
		return cmds
	}

	// Auto-detect from project manifests.
	if app.Exec == nil {
		return nil
	}
	files := detectManifestFiles(app)
	return verify.DetectCommands(files)
}

// detectManifestFiles lists the project root files relevant to test detection.
// We only need the manifest filenames, not the full directory listing — but
// ListDir is the cheapest available call and returns one line per entry.
func detectManifestFiles(app *App) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := app.Exec.ListDir(ctx, ".")
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(out, "\n") {
		// ListDir returns entries with trailing / for directories; strip it.
		name := strings.TrimRight(strings.TrimSpace(line), "/")
		if name != "" {
			files = append(files, name)
		}
	}
	return files
}

// RunVerification runs the given verification commands through the consent gate
// and executor, returning a structured Outcome. Each command is confirmed via
// app.Confirm (policy + SuspendAuto) before execution — this is the security
// boundary that prevents a malicious repo from running arbitrary commands.
//
// When a workflow is active, the outcome is appended to the step log via
// wfAppendStepLogEntry so it feeds into the final-review oracle briefing as
// machine evidence.
func RunVerification(ctx context.Context, app *App, cmds []verify.Command) verify.Outcome {
	if len(cmds) == 0 {
		return verify.Outcome{} // WasSkipped() = true
	}

	outcome := verify.Outcome{Results: make([]verify.Result, 0, len(cmds))}
	for _, cmd := range cmds {
		result := runOneVerifyCommand(ctx, app, cmd)
		outcome.Results = append(outcome.Results, result)
		// Stop on first decline: once a verification command is declined by
		// the consent gate, remaining commands should not run — the user
		// denied consent for this verification batch.
		if result.Status == verify.StatusDeclined {
			break
		}
	}

	// Append to step log when a workflow is active so the final-review briefing
	// carries the verification receipts as machine evidence. Summarize() already
	// prefixes each line with "VERIFY:" so no additional wrapping is needed.
	if app.Workflow != nil {
		wfAppendStepLogEntry(app, strings.TrimRight(outcome.Summarize(), "\n"))
	}

	return outcome
}

// runFinalVerification is the entry point from HandleFinalReview. It resolves
// the verification commands (config or auto-detect), runs them through the
// consent gate, and returns the outcome. The outcome's summary is appended
// to the step log by RunVerification so it feeds the oracle briefing.
func runFinalVerification(ctx context.Context, app *App) verify.Outcome {
	cmds := ResolveVerifyCommands(app)
	if len(cmds) == 0 {
		wfProgNote(app, "· verification: no commands configured or detected — skipped")
		// Still append to step log so the final-review briefing knows
		// verification was attempted but had no commands.
		if app.Workflow != nil {
			wfAppendStepLogEntry(app, "VERIFY: skipped — no commands configured or detected")
		}
		return verify.Outcome{}
	}
	wfProgNote(app, fmt.Sprintf("· running verification (%d commands)...", len(cmds)))
	return RunVerification(ctx, app, cmds)
}

// gate. Returns a Result with the command's status, output, and timing.
func runOneVerifyCommand(ctx context.Context, app *App, cmd verify.Command) verify.Result {
	if app.Exec == nil {
		return verify.Result{
			Command: cmd,
			Status:  verify.StatusError,
			Reason:  "no executor available",
		}
	}

	// Route through the consent gate — same path as run_shell.
	// This is the security boundary: policy deny blocks, policy ask prompts,
	// SuspendAuto carve-outs (destructive, egress) still fire on top.
	// The detail includes the command source (config/detect) so the user
	// knows where the command came from — detected commands execute
	// repo-controlled code (package.json scripts, conftest.py, etc.).
	headline := "verification [" + cmd.Source + "]: " + cmd.Cmd
	detail := "$ " + cmd.Cmd + "\n  (source: " + cmd.Source + ")"
	if !app.Confirm("run_shell", headline, detail, false) {
		return verify.Result{
			Command: cmd,
			Status:  verify.StatusDeclined,
			Reason:  "declined by user or policy",
		}
	}

	// Execute with a timeout to prevent hanging test runners.
	runCtx, cancel := context.WithTimeout(ctx, verifyTimeout)
	defer cancel()

	start := time.Now()
	out, err := app.Exec.RunShell(runCtx, cmd.Cmd)
	duration := time.Since(start)

	// Context deadline = timeout.
	if runCtx.Err() == context.DeadlineExceeded {
		return verify.Result{
			Command:    cmd,
			Status:     verify.StatusTimeout,
			Output:     verify.CapOutput(out, verify.OutputCap),
			DurationMs: duration.Milliseconds(),
			Reason:     fmt.Sprintf("timed out after %s", verifyTimeout),
		}
	}
	// Parent context cancellation (not our timeout) = error, not test failure.
	if ctx.Err() == context.Canceled {
		return verify.Result{
			Command:    cmd,
			Status:     verify.StatusError,
			Output:     verify.CapOutput(out, verify.OutputCap),
			DurationMs: duration.Milliseconds(),
			Reason:     "context canceled",
		}
	}

	// Non-nil error from RunShell = non-zero exit code. The Executor interface
	// (RunShell returns (string, error)) does not expose the exact exit code —
	// CombinedOutput returns a non-nil error for any non-zero exit. We set
	// ExitCode=1 as a convention; the actual code is in the output if the
	// command prints it.
	if err != nil {
		return verify.Result{
			Command:    cmd,
			Status:     verify.StatusFail,
			Output:     verify.CapOutput(out, verify.OutputCap),
			DurationMs: duration.Milliseconds(),
			ExitCode:   1,
			Reason:     err.Error(),
		}
	}

	return verify.Result{
		Command:    cmd,
		Status:     verify.StatusPass,
		Output:     verify.CapOutput(out, verify.OutputCap),
		DurationMs: duration.Milliseconds(),
	}
}
