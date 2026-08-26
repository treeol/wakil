// Headless driver (card #148 chunk 7, plan D20): re-routes the single-task
// `wakil run "<task>"` path through the session host + adapter, projecting
// domain events onto the existing JSON-lines transcript and exit codes.
//
// Chunk 7c: `wakil run --plan` is re-routed here too (runPlanTask) — the
// legacy direct-App workflow loop is gone.
package wiring

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/charmbracelet/x/ansi"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionhost"
	"github.com/treeol/wakil/internal/core/sessionhost/sqlstore"
	"github.com/treeol/wakil/internal/policy"
	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
	"github.com/treeol/wakil/internal/workflow"
)

// Exit codes for wakil run.
const (
	ExitOK             = 0 // task completed / VERDICT: PASS
	ExitDeclined       = 1 // a tool call was declined by the headless resolver
	ExitGaps           = 2 // workflow final review flagged unresolved gaps (--plan only)
	ExitError          = 3 // runtime error or fatal (4xx) request error
	ExitBackendFailure = 4 // retryable backend error exhausted retries; session saved, resumable
)

// HeadlessOptions is the transport-neutral parameter set for a headless run.
// cmd/wakil owns argument parsing and stderr usage; this struct carries only
// bootstrap/runtime policy. Loading of images/policy happens inside RunHeadless.
type HeadlessOptions struct {
	PlanMode         bool
	Auto             bool // approve non-destructive tool calls automatically
	AllowDestructive bool
	// AllowExternal pre-authorises all external backends for the egress consent
	// gate. Without it, a headless run that would route to an external backend
	// aborts the task (outcome=declined) rather than silently sending session
	// context to a cloud provider.
	AllowExternal bool
	NoOracle      bool // skip oracle review entirely; log "oracle disabled by flag"
	AutoCounsel   bool
	MaxCounsel    int
	// AttachImage is the path(s) to an image file attached to the first user
	// message. Multiple paths can be comma-separated (--attach-image).
	AttachImage string
	PolicyPath  string
	ProfileName string
	Verify      bool
	// TranscriptFile writes JSON-lines events here instead of stdout.
	TranscriptFile string
}

// emitEvent writes one JSON-lines event to w. Errors are swallowed — output is
// best-effort; a broken event stream must not mask the real exit code.
func emitEvent(w io.Writer, ev map[string]any) {
	b, _ := json.Marshal(ev)
	fmt.Fprintf(w, "%s\n", b)
}

// EmitEvent writes one JSON-lines event to w (exported wrapper for emitEvent,
// used by CLI integration tests).
func EmitEvent(w io.Writer, ev map[string]any) { emitEvent(w, ev) }

// headlessWriter adapts the free-form io.Writer interface (used by app.Out) to a
// JSON-lines event stream. Text is accumulated until a newline, then flushed as
// {"type":"output","line":"..."}. ANSI escape codes are stripped.
//
// In the host re-route the writer is fed from MessageDelta events (via its Write
// method), not directly from App.Out.
type HeadlessWriter struct {
	mu  sync.Mutex
	buf strings.Builder
	w   io.Writer
}

// NewHeadlessWriter returns a HeadlessWriter writing JSON-lines events to w.
func NewHeadlessWriter(w io.Writer) *HeadlessWriter { return &HeadlessWriter{w: w} }

func (h *HeadlessWriter) Write(p []byte) (int, error) {
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

// Flush emits any partial (newline-free) buffered content. Called on exit.
func (h *HeadlessWriter) Flush() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if line := strings.TrimSpace(h.buf.String()); line != "" {
		b, _ := json.Marshal(map[string]any{"type": "output", "line": line})
		fmt.Fprintf(h.w, "%s\n", b)
	}
	h.buf.Reset()
}

// headlessResolver builds the wiring ApprovalResolver for a single-task headless
// run. The decision is computed ONCE in headlessDecision (D18/D19): the same
// policy the legacy --auto/--allow-destructive/--allow-external/--policy flags
// produced, with the decline Reason carried in ApprovalResolved.
func headlessResolver(app *agent.App, opts HeadlessOptions) ApprovalResolver {
	return func(ctx context.Context, req ApprovalRequest) ApprovalResolution {
		choice, reason := headlessDecision(app, opts, req)
		return ApprovalResolution{Choice: choice, Reason: reason}
	}
}

// headlessDecision is the single headless approval decision function (D19/D18).
// It returns the choice and, on decline, a human-readable reason. It reads
// App.Policy() and agent.SuspendAuto, so it needs the App — fine, wiring holds it.
// HeadlessDecision is the exported headless approval policy (7c): one
// decision function shared by the single-task resolver, the plan path, and
// test-facing confirmer wrappers. Exposed so cmd/wakil test bridges can build
// confirmers with identical policy without duplicating the carve-outs.
func HeadlessDecision(app *agent.App, opts HeadlessOptions, req ApprovalRequest) (agent.ConfirmChoice, string) {
	return headlessDecision(app, opts, req)
}

func headlessDecision(app *agent.App, opts HeadlessOptions, req ApprovalRequest) (agent.ConfirmChoice, string) {
	if pol := app.Policy(); pol != nil {
		input := agent.BuildPolicyInput(req.ToolName, req.Detail, req.ReadAction)
		result := pol.Evaluate(input)
		switch result.Decision {
		case policy.Deny:
			return agent.ChoiceDecline, "blocked by policy: " + result.Reason + " (rule: " + result.RuleName + ")"
		case policy.Allow:
			if reason := agent.SuspendAuto(req.ToolName, app, req.Detail); reason != "" {
				switch {
				case reason == "destructive command" && opts.AllowDestructive:
					// Destructive shell allowed by --allow-destructive.
				case reason == "external backend egress (privacy gate)" && opts.AllowExternal:
					// External backend allowed by --allow-external.
				default:
					return agent.ChoiceDecline, "policy allowed but safety gate triggered: " + reason
				}
			}
			return agent.ChoiceApprove, ""
		case policy.Ask:
			return agent.ChoiceDecline, "policy requires confirmation (ask): " + result.Reason
		}
	}

	if !opts.Auto {
		return agent.ChoiceDecline, "confirmation required (rerun with --auto)"
	}
	if wtools.IsMashuraTool(req.ToolName) {
		return agent.ChoiceApprove, ""
	}
	switch req.ToolName {
	case "external_backend":
		if opts.AllowExternal {
			return agent.ChoiceApprove, ""
		}
		return agent.ChoiceDecline, "external backend requires --allow-external in headless mode"
	case "run_shell", "run_background":
		if agent.IsDestructiveShell(agent.ShellCmdFromDetail(req.Detail)) && !opts.AllowDestructive {
			cmd := agent.Truncate(agent.ShellCmdFromDetail(req.Detail), 80)
			return agent.ChoiceDecline, "destructive command declined: " + cmd + " (rerun with --allow-destructive)"
		}
		return agent.ChoiceApprove, ""
	default:
		return agent.ChoiceApprove, ""
	}
}

// RunHeadless is the bootstrap entrypoint for `wakil run`. It is exported so
// cmd/wakil can call it from a thin shim. task is the single task string;
// HeadlessOptions carries the flag-derived policy. Returns the process exit code.
func RunHeadless(cfg config.Config, task string, opts HeadlessOptions) int {
	ctx := context.Background()

	exe, err := NewExecutor(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "executor error:", err)
		return ExitError
	}
	defer exe.Close()

	app, res := BuildApp(cfg, exe, BuildAppOpts{
		IsHeadless:  true,
		AutoCounsel: opts.AutoCounsel,
		MaxCounsel:  opts.MaxCounsel,
	})
	// Register resource cleanup immediately after BuildApp so every error path
	// below is covered (LIFO: resources close before exe).
	defer func() {
		app.StopAllBackgroundProcs()
		CloseResources(app, res)
	}()

	if opts.Verify {
		app.VerifyEnabled = true
	}

	// Headless session: construct inline (no resume support in headless mode).
	// Use cfg.WorkspacePath() (not exe.WorkspaceRoot()) so the session's
	// recorded workspace matches the storage key derivation — they share the
	// same source of truth via Config.WorkspacePath().
	app.Session = &agent.Session{
		ChatID:    app.Client.ChatID,
		Model:     app.Client.Model,
		Workspace: cfg.WorkspacePath(),
	}

	// Load --attach-image into PendingImages so the first Send attaches them.
	if opts.AttachImage != "" {
		for _, p := range strings.Split(opts.AttachImage, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			img, err := proxy.LoadImage(p)
			if err != nil {
				fmt.Fprintln(os.Stderr, "attach-image:", err)
				return ExitError
			}
			app.PendingImages = append(app.PendingImages, img)
		}
	}

	// Load --policy file or --profile built-in, and install on the app.
	if opts.PolicyPath != "" {
		p, err := policy.LoadFile(opts.PolicyPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "policy:", err)
			return ExitError
		}
		app.SetPolicy(p)
	} else if opts.ProfileName != "" {
		p := policy.Profile(opts.ProfileName)
		if p == nil {
			fmt.Fprintf(os.Stderr, "policy: unknown profile %q — available: %v\n",
				opts.ProfileName, policy.ProfileNames())
			return ExitError
		}
		app.SetPolicy(p)
	}

	out := io.Writer(os.Stdout)
	if opts.TranscriptFile != "" {
		f, ferr := os.Create(opts.TranscriptFile)
		if ferr != nil {
			fmt.Fprintln(os.Stderr, "cannot create transcript:", ferr)
			return ExitError
		}
		defer f.Close()
		out = f
	}

	if !opts.PlanMode {
		return runSingleTask(ctx, app, task, opts, out)
	}
	return runPlanTask(ctx, app, task, opts, out)
}

// RunHeadlessApp drives a pre-built *agent.App through either the single-task
// host path (no --plan) or the plan-mode host path (--plan). It is the
// test-friendly entry point equivalent of cmd/wakil's old runHeadlessApp: the
// caller owns App construction. Returns the exit code.
func RunHeadlessApp(ctx context.Context, app *agent.App, task string, planMode bool, opts HeadlessOptions, out io.Writer) int {
	if !planMode {
		return runSingleTask(ctx, app, task, opts, out)
	}
	return runPlanTask(ctx, app, task, opts, out)
}

// runSingleTask drives one task through the session host (D20). Returns the
// exit code. It is the single-task re-route: CreateSession → Subscribe → Submit
// → project events → one terminal JSON record → tokens last.
func runSingleTask(ctx context.Context, app *agent.App, task string, opts HeadlessOptions, out io.Writer) int {
	app.WorkflowStepTrace = nil

	hw := NewHeadlessWriter(out)
	// Session save: defer before hw.flush() so flush runs first (LIFO order).
	if opts.TranscriptFile != "" {
		defer app.SaveSession()
	}
	defer hw.Flush()
	defer app.StopAllAsyncOps()

	// Build the host turn function with the shared resolver.
	turnFn, err := HostTurnFunc(app, WithResolver(headlessResolver(app, opts)))
	if err != nil {
		emitEvent(out, map[string]any{"type": "error", "message": err.Error()})
		return ExitError
	}
	h := sessionhost.New(turnFn, headlessStoreOpts(app.SessionWorkspace())...)
	defer h.Close(ctx)
	p := core.EmbeddedPrincipal()

	sess, err := h.CreateSession(ctx, p, core.CreateSessionRequest{
		Workspace: event.WorkspaceID("wsp_local"),
		Title:     "headless",
	})
	if err != nil {
		emitEvent(out, map[string]any{"type": "error", "message": err.Error()})
		return ExitError
	}

	sub, err := h.Subscribe(ctx, p, sess.ID, 0)
	if err != nil {
		emitEvent(out, map[string]any{"type": "error", "message": err.Error()})
		return ExitError
	}
	defer sub.Close()

	if _, err := h.SubmitInput(ctx, p, core.SubmitInputRequest{SessionID: sess.ID, Text: task}); err != nil {
		emitEvent(out, map[string]any{"type": "error", "message": err.Error()})
		return ExitError
	}

	// Projection state: consume events until the correlated turn terminal.
	// Returns (exit code, message) where message is the decline reason or error
	// text for the single terminal record.
	code, msg := consumeTurnEvents(ctx, sub, hw)

	// Emit the single terminal record based on outcome + decline latch.
	// Precedence: backend_failure > request_error > declined > pass.
	switch code {
	case ExitBackendFailure:
		emitEvent(out, map[string]any{
			"type":      "done",
			"outcome":   "backend_failure",
			"message":   msg,
			"resume_id": agent.ShortID(app.Client.ChatID),
		})
	case ExitError:
		emitEvent(out, map[string]any{"type": "error", "message": msg})
	case ExitDeclined:
		emitEvent(out, map[string]any{"type": "done", "outcome": "declined", "reason": msg})
	default:
		emitEvent(out, map[string]any{"type": "done", "outcome": "pass"})
	}

	// Token summary, last (matches legacy ordering).
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

// runPlanTask drives the plan workflow through the session host (7c). Pre-host
// setup (plan.md, SetWorkflow, writer/teardown) mirrors the legacy driver
// verbatim; the multi-turn workflow loop becomes host turns with the adapter's
// plan-auto-advance resolver (see resolveAfterTurn) driving phase transitions.
// Byte parity with the legacy driver: first submitted text is "continue" (the
// task text lives in .wakil/plan.md, exactly as legacy), terminal records and
// exit codes are produced by consumeWorkflowEvents from durable events only —
// the consumer never reads app state directly.
func runPlanTask(ctx context.Context, app *agent.App, task string, opts HeadlessOptions, out io.Writer) int {
	hw := NewHeadlessWriter(out)
	if opts.TranscriptFile != "" {
		defer app.SaveSession()
	}
	defer hw.Flush()
	defer app.StopAllAsyncOps()

	app.Out = hw
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
	return runPlanSession(ctx, app, opts, out, hw)
}

// RunPlanLoop drives the host-path plan loop over an ALREADY-INITIALIZED
// app.Workflow (7c test entry — the old RunWorkflowLoopLegacy equivalent).
// The caller owns plan.md and WorkflowState setup; this runs the host session
// to a workflow terminal and emits the terminal record.
func RunPlanLoop(ctx context.Context, app *agent.App, opts HeadlessOptions, out io.Writer) int {
	hw := NewHeadlessWriter(out)
	if opts.TranscriptFile != "" {
		defer app.SaveSession()
	}
	defer hw.Flush()
	defer app.StopAllAsyncOps()

	app.Out = hw
	return runPlanSession(ctx, app, opts, out, hw)
}

// runPlanSession builds the host session for a plan run and consumes it to a
// workflow terminal.
func runPlanSession(ctx context.Context, app *agent.App, opts HeadlessOptions, out io.Writer, hw *HeadlessWriter) int {
	turnFn, err := HostTurnFunc(app, WithResolver(headlessResolver(app, opts)), WithPlanAutoAdvance(opts.NoOracle))
	if err != nil {
		emitEvent(out, map[string]any{"type": "error", "message": err.Error()})
		return ExitError
	}
	h := sessionhost.New(turnFn, headlessStoreOpts(app.SessionWorkspace())...)
	defer h.Close(ctx)
	p := core.EmbeddedPrincipal()

	sess, err := h.CreateSession(ctx, p, core.CreateSessionRequest{
		Workspace: event.WorkspaceID("wsp_local"),
		Title:     "headless-plan",
	})
	if err != nil {
		emitEvent(out, map[string]any{"type": "error", "message": err.Error()})
		return ExitError
	}

	sub, err := h.Subscribe(ctx, p, sess.ID, 0)
	if err != nil {
		emitEvent(out, map[string]any{"type": "error", "message": err.Error()})
		return ExitError
	}
	defer sub.Close()

	// Legacy parity: the loop sent "continue" for every step including the
	// first; the task text lives in plan.md.
	if _, err := h.SubmitInput(ctx, p, core.SubmitInputRequest{SessionID: sess.ID, Text: "continue"}); err != nil {
		emitEvent(out, map[string]any{"type": "error", "message": err.Error()})
		return ExitError
	}

	code, msg := consumeWorkflowEvents(ctx, sub, hw, out)

	// Terminal record — byte parity with the legacy records.
	switch code {
	case ExitBackendFailure:
		emitEvent(out, map[string]any{
			"type":      "done",
			"outcome":   "backend_failure",
			"message":   msg,
			"resume_id": agent.ShortID(app.Client.ChatID),
		})
	case ExitError:
		emitEvent(out, map[string]any{"type": "error", "message": msg})
	case ExitDeclined:
		emitEvent(out, map[string]any{"type": "done", "outcome": "declined", "reason": msg})
	case ExitGaps:
		emitEvent(out, map[string]any{"type": "done", "outcome": msg, "message": gapsMessage(msg)})
	default:
		emitEvent(out, map[string]any{"type": "done", "outcome": "pass"})
	}

	// No tokens record in plan mode — legacy parity.
	return code
}

// gapsMessage renders the terminal message for gaps/verify_failed outcomes.
// msg carries the outcome ("gaps" | "verify_failed"); the message text matches
// the legacy strings exactly.
func gapsMessage(outcome string) string {
	if outcome == "verify_failed" {
		return "verification failed (see step log for details)"
	}
	return "final review flagged unresolved gaps"
}

// consumeWorkflowEvents drains the subscription until a WORKFLOW-level
// terminal: the durable WorkflowOutcome event (declined/verify_failed/gaps),
// a SessionError (backend/request/internal), a cancelled turn, or the final
// turn completing with WorkflowWillContinue=false (pass). Deltas stream to hw
// (the JSON-lines HeadlessWriter); WorkflowWarning renders the legacy warning
// record on the RAW writer (a standalone JSON line, never interleaved into an
// output chunk). Returns (exit code, message): for ExitDeclined the decline
// reason; for ExitGaps the outcome string ("gaps"|"verify_failed"); for
// errors the error text.
func consumeWorkflowEvents(ctx context.Context, sub core.EventSubscription, hw *HeadlessWriter, rawOut io.Writer) (int, string) {
	for {
		ev, err := sub.Next(ctx)
		if err != nil {
			return ExitError, err.Error()
		}
		switch ev.Kind {
		case event.KindMessageDelta:
			hw.Write([]byte(ev.Payload.(event.MessageDelta).Text))
		case event.KindWorkflowWarning:
			emitEvent(rawOut, map[string]any{
				"type":    "warning",
				"message": ev.Payload.(event.WorkflowWarning).Message,
			})
		case event.KindWorkflowOutcome:
			wo := ev.Payload.(event.WorkflowOutcome)
			switch wo.Outcome {
			case "declined":
				return ExitDeclined, wo.Reason
			case "verify_failed":
				return ExitGaps, "verify_failed"
			default:
				return ExitGaps, "gaps"
			}
		case event.KindSessionError:
			se := ev.Payload.(event.SessionError)
			switch se.Reason {
			case "backend_failure":
				return ExitBackendFailure, se.Err
			case "request_error":
				return ExitError, strings.TrimPrefix(se.Err, "sessionhost: fatal backend request error: ")
			default:
				return ExitError, se.Err
			}
		case event.KindTurnCompleted:
			tc := ev.Payload.(event.TurnCompleted)
			switch tc.Outcome {
			case "cancelled":
				return ExitError, "turn cancelled"
			case "complete", "empty":
				if !tc.WorkflowWillContinue {
					// Workflow done, final turn completed normally → pass.
					return ExitOK, ""
				}
			case "stream_error":
				// SessionError follows; keep consuming.
			}
		}
	}
}

// consumeTurnEvents drains the subscription until the session reaches a terminal
// outcome, projecting deltas to the writer and latching the decline reason. It
// returns (exit code, message): the exit code for the turn's final outcome and
// the decline reason / error text for the terminal record.
//
// Precedence matches legacy runSingleTaskHeadless: an error wins over decline,
// decline wins over pass, and multiple declines → last reason wins.
func consumeTurnEvents(ctx context.Context, sub core.EventSubscription, hw *HeadlessWriter) (int, string) {
	var declinedReason string
	for {
		ev, err := sub.Next(ctx)
		if err != nil {
			return ExitError, err.Error()
		}
		switch ev.Kind {
		case event.KindMessageDelta:
			hw.Write([]byte(ev.Payload.(event.MessageDelta).Text))
		case event.KindApprovalResolved:
			ar := ev.Payload.(event.ApprovalResolved)
			if ar.Outcome == "declined" {
				// last reason wins (matches legacy *declinedReason overwrite).
				declinedReason = ar.Reason
			}
		case event.KindTurnCompleted:
			tc := ev.Payload.(event.TurnCompleted)
			switch tc.Outcome {
			case "complete":
				if declinedReason != "" {
					return ExitDeclined, declinedReason
				}
				return ExitOK, ""
			case "cancelled":
				return ExitError, "turn cancelled"
			case "empty":
				// Not reachable after D17 (empties are recovered inside the
				// adapter); treat as complete for safety.
				if declinedReason != "" {
					return ExitDeclined, declinedReason
				}
				return ExitOK, ""
			case "stream_error":
				// SessionError follows (host emits TurnCompleted{stream_error} then
				// SessionError); keep consuming.
			}
		case event.KindSessionError:
			se := ev.Payload.(event.SessionError)
			switch se.Reason {
			case "backend_failure":
				return ExitBackendFailure, se.Err
			case "request_error":
				// Strip the adapter's sentinel prefix so the projected message
				// is the raw backend error (byte-parity with the legacy 4xx
				// path, which emitted err.Error() unwrapped).
				return ExitError, strings.TrimPrefix(se.Err, "sessionhost: fatal backend request error: ")
			default: // internal_error, daemon_restart
				return ExitError, se.Err
			}
		}
	}
}

// headlessStoreOpts returns sessionhost options for the headless path: a
// workspace-keyed SQLiteStore if the DB path can be derived, else nil (the
// host falls back to MemLog). Best-effort — a failure to open is logged to
// stderr and the run proceeds with in-memory storage.
//
// Passes the raw workspace path to SessionHostDBPath — the storage functions
// hash the path internally via workspaceKey, so passing the wsp_ ID or a
// sentinel like "wsp_local" would double-hash and produce a cwd-dependent key.
func headlessStoreOpts(wsPath string) []sessionhost.Option {
	dbPath := agent.SessionHostDBPath(wsPath)
	if dbPath == "" {
		return nil
	}
	store, err := sqlstore.NewSQLiteStore(context.Background(), dbPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sessionhost: failed to open SQLite store for headless, using in-memory:", err)
		return nil
	}
	return []sessionhost.Option{sessionhost.WithStore(store)}
}
