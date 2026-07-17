package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/tools"
	"github.com/treeol/wakil/internal/workflow"

	"github.com/charmbracelet/x/ansi"
)

// Exit codes for wakil run.
const (
	ExitOK             = 0 // task completed / VERDICT: PASS
	ExitDeclined       = 1 // a tool call was declined by the headless confirmer
	ExitGaps           = 2 // workflow final review flagged unresolved gaps
	ExitError          = 3 // runtime error or fatal (4xx) request error
	ExitBackendFailure = 4 // retryable backend error exhausted retries; session saved, resumable
)

// RunFlags holds the policy options for wakil run.
type RunFlags struct {
	Auto             bool   // approve non-destructive tool calls automatically
	AllowDestructive bool   // allow destructive shell commands (rm, mv, etc.)
	NoOracle         bool   // skip oracle review entirely; log "oracle disabled by flag"
	TranscriptFile   string // write JSON-lines events here instead of stdout
	// AllowExternal pre-authorises all external backends for the egress consent
	// gate. Without this flag, a headless run that would route to an external
	// backend aborts the task (failure_mode=declined) rather than silently sending
	// session context to a cloud provider. Set only when the caller has already
	// verified egress is acceptable for this run (e.g. a benchmark harness with
	// explicit OR credentials).
	AllowExternal bool

	// AutoCounsel fires mashura__debug automatically when the struggle detector
	// triggers, instead of printing a hint that no human is present to read.
	// Requires --auto and oracle_enabled=true + an API key. Bounded by MaxCounsel.
	//
	// Benchmark usage:
	//   --auto              bare-agent run (default; zero mashūra calls)
	//   --auto --auto-counsel --max-counsel 3
	//                       full-stack run (up to 3 diagnoses per task)
	// The two modes produce different scores AND different costs — a full-stack
	// run with OR models can easily be 5-10× more expensive per task.
	AutoCounsel bool

	// MaxCounsel caps auto-counsel calls per task. Default 3 when --auto-counsel
	// is set without an explicit --max-counsel. 0 disables auto-counsel entirely.
	MaxCounsel int
}

// parseRunArgs parses the args that follow "run":
//
//	[--plan] [--auto] [--allow-destructive] [--allow-external]
//	[--auto-counsel] [--max-counsel N] [--no-oracle] [--transcript <file>] "<task>"
func parseRunArgs(args []string) (task string, planMode bool, flags RunFlags, err error) {
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--plan":
			planMode = true
		case "--auto":
			flags.Auto = true
		case "--allow-destructive":
			flags.AllowDestructive = true
		case "--allow-external":
			flags.AllowExternal = true
		case "--auto-counsel":
			flags.AutoCounsel = true
		case "--max-counsel":
			i++
			if i >= len(args) {
				return "", false, flags, fmt.Errorf("--max-counsel requires an integer")
			}
			if n, sErr := fmt.Sscanf(args[i], "%d", &flags.MaxCounsel); n != 1 || sErr != nil {
				return "", false, flags, fmt.Errorf("--max-counsel requires an integer, got %q", args[i])
			}
		case "--no-oracle":
			flags.NoOracle = true
		case "--transcript":
			i++
			if i >= len(args) {
				return "", false, flags, fmt.Errorf("--transcript requires a file path")
			}
			flags.TranscriptFile = args[i]
		default:
			if strings.HasPrefix(args[i], "-") {
				return "", false, flags, fmt.Errorf("unknown flag: %s", args[i])
			}
			if task != "" {
				return "", false, flags, fmt.Errorf("unexpected argument: %s", args[i])
			}
			task = args[i]
		}
	}
	if task == "" {
		return "", false, flags, fmt.Errorf(
			"usage: wakil run [--plan] [--auto] [--allow-destructive] [--allow-external] [--auto-counsel [--max-counsel N]] \"<task>\"")
	}
	// Default cap: 3 auto-counsel calls when --auto-counsel is set without --max-counsel.
	if flags.AutoCounsel && flags.MaxCounsel == 0 {
		flags.MaxCounsel = 3
	}
	return task, planMode, flags, nil
}

// emitEvent writes one JSON-lines event to w. Errors are swallowed — output is
// best-effort; a broken event stream must not mask the real exit code.
func emitEvent(w io.Writer, event map[string]any) {
	b, _ := json.Marshal(event)
	fmt.Fprintf(w, "%s\n", b)
}

// headlessWriter adapts the free-form io.Writer interface (used by app.Out) to a
// JSON-lines event stream. Text is accumulated until a newline, then flushed as
// {"type":"output","line":"..."}. ANSI escape codes are stripped.
type headlessWriter struct {
	mu  sync.Mutex
	buf strings.Builder
	w   io.Writer
}

func (h *headlessWriter) Write(p []byte) (int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := ansi.Strip(string(p))
	h.buf.WriteString(s)
	for {
		idx := strings.IndexByte(h.buf.String(), '\n')
		if idx < 0 {
			break
		}
		line := h.buf.String()[:idx]
		remaining := h.buf.String()[idx+1:]
		h.buf.Reset()
		h.buf.WriteString(remaining)
		if strings.TrimSpace(line) != "" {
			b, _ := json.Marshal(map[string]any{"type": "output", "line": line})
			fmt.Fprintf(h.w, "%s\n", b)
		}
	}
	return len(p), nil
}

// flush emits any partial (newline-free) buffered content. Called on exit.
func (h *headlessWriter) flush() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if line := strings.TrimSpace(h.buf.String()); line != "" {
		b, _ := json.Marshal(map[string]any{"type": "output", "line": line})
		fmt.Fprintf(h.w, "%s\n", b)
	}
	h.buf.Reset()
}

// headlessConfirmer returns a agent.Confirmer that implements the headless run policy:
//
//   - --auto: approve most tool calls. Oracle is explicitly allowed — unlike TUI
//     auto mode (which always gates oracle for user review), headless --auto has
//     no user present so the opt-in is sufficient. Destructive shell commands are
//     declined unless --allow-destructive is set.
//   - no --auto: decline every confirmation-required call.
//
// When a call is declined, *declinedReason is set with a human-readable explanation
// that is included in the final exit event.
func headlessConfirmer(flags RunFlags, declinedReason *string) agent.Confirmer {
	return func(toolName, headline, detail string, readAction bool) bool {
		if !flags.Auto {
			*declinedReason = "confirmation required (rerun with --auto)"
			return false
		}
		// Mashūra counsel (and the legacy oracle__ask alias) is allowed in headless
		// --auto; the user opted in by passing the flag.
		if tools.IsMashuraTool(toolName) {
			return true
		}
		switch toolName {
		case "external_backend":
			// The P29 cloud-egress consent gate cannot prompt interactively in
			// headless mode. --allow-external pre-authorises external backends;
			// without it, the task is aborted with a clear failure reason rather
			// than silently sending session context to a cloud provider.
			if flags.AllowExternal {
				return true
			}
			*declinedReason = "external backend requires --allow-external in headless mode"
			return false
		case "run_shell", "run_background":
			if agent.IsDestructiveShell(agent.ShellCmdFromDetail(detail)) && !flags.AllowDestructive {
				cmd := agent.Truncate(agent.ShellCmdFromDetail(detail), 80)
				*declinedReason = "destructive command declined: " + cmd +
					" (rerun with --allow-destructive)"
				return false
			}
			return true
		default:
			return true
		}
	}
}

// runHeadlessApp is the core headless driver. It wires the headless confirmer and
// output writer into a pre-built App and then runs either a single task or the
// full /plan workflow. Returns one of the Exit* constants.
//
// A {"type":"tokens","input":N,"output":N} event is always emitted after the
// done/error event so benchmark harnesses (e.g. Terminal-Bench) can record real
// per-task token usage without instrumenting the proxy separately.
//
// Exported for tests; callers should use RunHeadless for the CLI entry point.
func runHeadlessApp(ctx context.Context, app *agent.App, task string, planMode bool, flags RunFlags, out io.Writer) int {
	hw := &headlessWriter{w: out}
	// Session save: defer before hw.flush() so flush runs first (LIFO order).
	// Only persist when --transcript is set: benchmark runs without tracing
	// don't need entries in the session store.
	if flags.TranscriptFile != "" {
		defer app.SaveSession()
	}
	defer hw.flush()

	var declinedReason string
	app.Out = hw
	app.Confirm = headlessConfirmer(flags, &declinedReason)
	app.Client.ResetGrounding()

	var code int
	if !planMode {
		code = runSingleTaskHeadless(ctx, app, task, out, &declinedReason)
	} else {
		code = runWorkflowHeadless(ctx, app, task, flags, out, &declinedReason)
	}

	// Emit a token-summary event for adapters that need cost accounting.
	// Summed across all sources so the adapter gets a single consolidated figure.
	if app.Costs != nil {
		_, rows := app.Costs.Snapshot()
		var inTok, outTok int64
		for _, r := range rows {
			inTok += r.InputTok
			outTok += r.OutputTok
		}
		emitEvent(out, map[string]any{"type": "tokens", "input": inTok, "output": outTok})
	}

	return code
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

func runSingleTaskHeadless(ctx context.Context, app *agent.App, task string, out io.Writer, declinedReason *string) int {
	app.WorkflowStepTrace = nil
	_, err := app.Send(ctx, task)
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
	emitEvent(out, map[string]any{"type": "done", "outcome": "pass"})
	return ExitOK
}

func runWorkflowHeadless(ctx context.Context, app *agent.App, task string, flags RunFlags, out io.Writer, declinedReason *string) int {
	// --no-oracle: disable before any oracle call can be issued, so handleReviewOracle
	// returns "oracle not enabled" immediately and the WFReview switch case can log the
	// correct "disabled by flag" reason rather than the generic "unavailable" one.
	if flags.NoOracle {
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
	app.Workflow = &workflow.WorkflowState{
		Task:     task,
		Phase:    workflow.WFGather,
		PlanPath: planPath,
	}
	return runWorkflowLoop(ctx, app, flags, out, declinedReason)
}

// runWorkflowLoop drives the plan workflow state machine on an already-initialized
// app.Workflow. Separated from runWorkflowHeadless so tests can inject a pre-set
// workflow without repeating the scaffold-write and phase-init.
func runWorkflowLoop(ctx context.Context, app *agent.App, flags RunFlags, out io.Writer, declinedReason *string) int {
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
			// handleReviewOracle already ran (inside handleWorkflowTransition) and the
			// oracle was unavailable. This is the skip+warn fallback, not an initial skip.
			// Distinguish two sub-cases so the transcript carries the right reason:
			//   --no-oracle: oracle was deliberately disabled by the caller.
			//   default:     oracle was tried (confirm gate auto-approved it) but failed.
			var reason, logReason string
			if flags.NoOracle {
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
				// Final review flagged gaps (or oracle unavailable) — exit nonzero.
				emitEvent(out, map[string]any{
					"type": "done", "outcome": "gaps",
					"message": "final review flagged unresolved gaps",
				})
				return ExitGaps
			}
			// Paused by every-step oracle critique — auto-continue.
			app.Workflow.StepIdx++
			if app.Workflow.StepIdx > app.Workflow.StepCount {
				// Last step was paused; run final review now.
				agent.HandleFinalReview(ctx, app)
				if app.Workflow != nil {
					emitEvent(out, map[string]any{"type": "done", "outcome": "gaps"})
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

// RunHeadless is the CLI entry point for "wakil run". It builds the App from cfg
// and dispatches to runHeadlessApp. Returns the process exit code.
func RunHeadless(cfg config.Config, args []string) int {
	task, planMode, flags, err := parseRunArgs(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return ExitError
	}

	exe, err := newExecutor(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "executor error:", err)
		return ExitError
	}
	defer exe.Close()

	app, res := buildApp(cfg, exe, buildAppOpts{
		IsHeadless:  true,
		AutoCounsel: flags.AutoCounsel,
		MaxCounsel:  flags.MaxCounsel,
	})

	// Headless session: construct inline (no resume support in headless mode).
	app.Session = &agent.Session{
		ChatID:    app.Client.ChatID,
		Model:     app.Client.Model,
		Workspace: exe.WorkspaceRoot(),
	}

	// Defer resource cleanup.
	if res.mcpMgr != nil {
		defer res.mcpMgr.Close()
	}
	if res.lspMgr != nil {
		defer res.lspMgr.Shutdown()
	}
	if res.browserMgr != nil {
		defer res.browserMgr.Close()
	}
	if res.traceStore != nil {
		defer res.traceStore.Close()
	}
	if res.memStore != nil {
		defer res.memStore.Close()
	}

	out := io.Writer(os.Stdout)
	if flags.TranscriptFile != "" {
		f, ferr := os.Create(flags.TranscriptFile)
		if ferr != nil {
			fmt.Fprintln(os.Stderr, "cannot create transcript:", ferr)
			return ExitError
		}
		defer f.Close()
		out = f
	}

	return runHeadlessApp(context.Background(), app, task, planMode, flags, out)
}
