package agent

// tool_handlers.go contains the per-tool handler methods extracted from
// ExecuteToolCall (WP-6.1). Each handler has the signature:
//
//	func (a *App) handleXxx(ctx context.Context, tc proxy.ToolCall) string
//
// ExecuteToolCall (app.go) is now a thin dispatch: it runs the shared
// pre-dispatch gate (workflow phase enforcement), then routes to the
// appropriate handler via a switch. Handlers that were already extracted
// before WP-6.1 (handleEditFile, handleMashura, handleStaging*,
// handleMemory*) remain in their original files.
//
// IMPORTANT: These handlers return string (not toolResult). WP-6.8 will
// change the return type to toolResult and update the handler signature.

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/safe"
	wtools "github.com/treeol/wakil/internal/tools"
	"github.com/treeol/wakil/internal/workflow"
)

// handleRunShell executes a shell command after confirmation.
func (a *App) handleRunShell(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	// Read-only commands are auto-approved once the user has allowed reads;
	// otherwise prompt, offering the "allow all reads" choice for reads.
	readAction := IsReadOnlyShell(args.Command)

	// In pre-IMPLEMENT workflow phases, route through the confirm gate —
	// the AllowReads shortcut is suppressed so the user sees the phase
	// warning. In /auto mode, read-only commands are auto-approved by
	// SuspendAuto (the phase warning still shows in the ⚡ auto note);
	// non-read-only commands suspend auto and prompt.
	preImpl := a.Workflow != nil && workflow.IsPreImplementPhase(a.Workflow.Phase)
	detail := fmt.Sprintf("$ %s\n  (%s)", args.Command, a.Exec.Describe())
	if preImpl {
		detail = fmt.Sprintf("⚠ workflow phase: %s (read-only expected — is this command investigative?)\n%s",
			a.Workflow.PhaseName(), detail)
	}
	if preImpl || !(readAction && a.Consent().AllowReads) {
		if !a.Confirm("run_shell", "Run shell command?", detail, readAction) {
			return "[declined by user]"
		}
	}

	// Auto-background path: if ShellTimeoutSec > 0, run with a soft deadline.
	// Under the deadline: blocking, full output (same as today). Over: auto-
	// background and return a pointer for read_process_log polling.
	if a.Cfg.ShellTimeoutSec > 0 {
		return a.runShellWithDeadline(ctx, args.Command, readAction)
	}

	out, err := a.Exec.RunShell(ctx, args.Command)
	// LSP file-sync: after a non-read-only run_shell, mark open files dirty
	// for lazy resync (R3). The next LSP query touching a dirty file will
	// stat it and didChange if mtime+size changed.
	if err == nil && a.LSP != nil && !readAction {
		a.LSP.MarkOpenFilesDirty()
		a.LSP.BatchNotifyWatchedFiles(ctx)
	}
	return formatResult(out, err)
}

// runShellWithDeadline runs a shell command with a soft deadline. If the
// command finishes before the deadline, it returns the full output (same as
// blocking RunShell). If the deadline is reached, the command is left running
// as a background process and a pointer is returned for read_process_log
// polling. This unblocks the agent goroutine so the tool loop can continue.
//
// Exit-code capture: the command is wrapped so its exit code lands in the log
// as a parseable marker (wrapExitMarker). Without it, a backgrounded command
// that finished within the deadline — or one announced by the completion
// notice — could fail with a non-zero exit code completely invisibly.
func (a *App) runShellWithDeadline(ctx context.Context, command string, readAction bool) string {
	deadline := time.Duration(a.Cfg.ShellTimeoutSec) * time.Second

	// Reuse the existing bg machinery: log dir, registry, 5-proc limit.
	a.bgMu.Lock()
	if a.bgProcs == nil {
		a.bgProcs = make(map[string]*bgEntry)
	}
	live := 0
	for _, e := range a.bgProcs {
		if e.generation == a.Exec.Generation() && a.Exec.IsProcessGroupAlive(ctx, e.pgid) {
			live++
		}
	}
	if live >= 5 {
		a.bgMu.Unlock()
		// Card #121: fail closed instead of silently blocking. run_background
		// shares this same 5-proc limit, so the honest remediation is to free a
		// slot (kill_process) or wait — not to block the turn.
		return "ERROR: background process limit reached (5 live) — free a slot with kill_process(bgN) or wait for a running process to finish; auto-backgrounding is unavailable until then"
	}
	a.bgCounter++
	n := a.bgCounter
	a.bgMu.Unlock()

	// Use a flat path in /tmp (no subdirectory) for the log file. In Docker
	// mode the container's /tmp is a writable tmpfs, and the shell redirect
	// creates the file directly — no directory creation needed. In Direct
	// mode /tmp is also writable. A host-side os.MkdirTemp would create a
	// directory that doesn't exist inside the container, causing the
	// redirect to fail.
	logPath := fmt.Sprintf("/tmp/wakil-bg-%d.log", n)
	bgID := fmt.Sprintf("bg%d", n)

	pid, pgid, err := a.Exec.StartBackground(ctx, exec.WrapExitMarker(command, logPath), logPath)
	if err != nil {
		// NOTE: the counter slot is deliberately NOT rolled back — IDs stay
		// monotonic so a failed start can never cause a future bgN collision
		// (pre-existing rollback bug, card #121 review).
		// Card #121: StartBackground failure is usually environmental (executor
		// or container quirk); erroring here would make EVERY shell command
		// unrunnable. Loud blocking fallback instead — never silent.
		fmt.Fprintln(a.Out, Yellow("⚠ auto-background unavailable ("+err.Error()+") — running synchronously"))
		out, runErr := a.Exec.RunShell(ctx, command)
		if runErr == nil && a.LSP != nil && !readAction {
			a.LSP.MarkOpenFilesDirty()
			a.LSP.BatchNotifyWatchedFiles(ctx)
		}
		return formatResult(out, runErr)
	}

	done := make(chan struct{})
	entry := &bgEntry{
		id:         bgID,
		pid:        pid,
		pgid:       pgid,
		label:      "auto-bg",
		logPath:    logPath,
		startedAt:  time.Now(),
		generation: a.Exec.Generation(),
		readOnly:   readAction,
		cmdDigest:  Truncate(command, 80),
		done:       done,
	}
	a.bgMu.Lock()
	a.bgProcs[bgID] = entry
	a.bgMu.Unlock()

	// Reaper goroutine: polls GROUP liveness, closes done when the whole group
	// is gone. Uses a background context — the process may outlive the turn.
	// Group check (not the leader pid): since card #121 the leader is the
	// exit-marker wrapper shell; a signal-ignoring child could outlive it and
	// must still count as running. Card #121: when the command was DETACHED
	// (deadline hit), the reaper also pushes a one-line completion notice into
	// the async inbox — the "ping". Exactly once: notifyOnExit && !notified
	// under bgMu; the sync waiter sets notifyOnExit=false when it returns
	// output itself, so a fast command never yields both a synchronous result
	// AND a notification.
	safe.Go("auto-bg-reaper", func() {
		bgCtx := context.Background()
		for {
			if !a.Exec.IsProcessGroupAlive(bgCtx, pgid) {
				close(done)
				a.bgMu.Lock()
				e := a.bgProcs[bgID]
				shouldNotify := e != nil && e.notifyOnExit && !e.notified
				if shouldNotify {
					e.notified = true
				}
				a.bgMu.Unlock()
				if shouldNotify {
					a.notifyDetachedShellExit(bgID, e)
				}
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	})

	// Wait for completion or deadline.
	timer := time.NewTimer(deadline)
	defer timer.Stop()
	select {
	case <-done:
		// Process finished within the deadline — read the full log.
		// notifyOnExit stays false: the reaper will NOT push a completion
		// notice, so the model sees exactly one result (this one).
		out, readErr := a.Exec.ReadFile(ctx, logPath)
		if readErr != nil {
			out = "(output unreadable: " + readErr.Error() + ")"
		}
		// Exit-code recovery (card #121 follow-up): the wrapper wrote a
		// sentinel marker; parse it and surface non-zero exits as ERROR so a
		// failing backgrounded command can never masquerade as "(no output)".
		// The "exit status N" wording deliberately mirrors exec.ExitError so
		// string-matching callers see identical text on both shell paths.
		// FAIL CLOSED: a completed process with no parseable marker (killed
		// before the write, shell syntax error, unreadable log) must never
		// look like success.
		var runErr error
		if readErr != nil {
			runErr = fmt.Errorf("exit code unknown (log unreadable)")
		} else if cleaned, code, ok := exec.ParseExitMarker(out); ok {
			out = cleaned
			if code != 0 {
				runErr = fmt.Errorf("exit status %d", code)
			}
		} else {
			runErr = fmt.Errorf("exit code unknown (completion marker missing — killed, shell syntax error, or marker displaced)")
		}
		// LSP file-sync for non-read-only commands that modify files. Marking
		// happens regardless of exit code: a command that ran but exited
		// non-zero may still have partially modified files (unlike the
		// blocking path's launch-failure case).
		if a.LSP != nil && !readAction {
			a.LSP.MarkOpenFilesDirty()
			a.LSP.BatchNotifyWatchedFiles(ctx)
		}
		// Clean up the bg entry — the process is done.
		a.bgMu.Lock()
		delete(a.bgProcs, bgID)
		a.bgMu.Unlock()
		return formatResult(out, runErr)
	case <-timer.C:
		// Deadline reached — the process is still running. Leave it
		// registered for read_process_log polling, and arm the push
		// notification: the reaper will announce the exit (card #121).
		// Race guard: the process may have exited between the reaper's
		// notify check and this arm (select picked timer.C while done was
		// also ready) — then the reaper already left without notifying, so
		// we notify here ourselves. The notified flag dedupes both paths.
		a.bgMu.Lock()
		notifySelf := false
		var notifyEntry *bgEntry
		if e := a.bgProcs[bgID]; e != nil {
			e.notifyOnExit = true
			select {
			case <-done: // already exited — reaper saw notifyOnExit==false
				if !e.notified {
					e.notified = true
					notifySelf = true
					notifyEntry = e
				}
			default:
			}
		}
		a.bgMu.Unlock()
		if notifySelf {
			a.notifyDetachedShellExit(bgID, notifyEntry)
		}
		return fmt.Sprintf("command still running as %s — you will be notified when it finishes; use read_process_log(%s) to poll for output, kill_process(%s) to stop", bgID, bgID, bgID)
	case <-ctx.Done():
		// Turn cancelled — leave the process running for the user to inspect;
		// arm the notification the same way (the exit still matters). Same
		// race guard as the timer branch.
		a.bgMu.Lock()
		notifySelf := false
		var notifyEntry *bgEntry
		if e := a.bgProcs[bgID]; e != nil {
			e.notifyOnExit = true
			select {
			case <-done:
				if !e.notified {
					e.notified = true
					notifySelf = true
					notifyEntry = e
				}
			default:
			}
		}
		a.bgMu.Unlock()
		if notifySelf {
			a.notifyDetachedShellExit(bgID, notifyEntry)
		}
		return fmt.Sprintf("command still running as %s (turn cancelled) — you will be notified when it finishes; use read_process_log(%s) to poll for output", bgID, bgID)
	}
}

// notifyDetachedShellExit builds the completion notice for a detached
// (auto-backgrounded at deadline) shell command and enqueues it into the async
// inbox as a shell-typed asyncOp record. The record carries the log tail so the
// model can act without an extra poll; full output stays in read_process_log.
// Called from the reaper goroutine — never touches Conv (funnel drains it).
func (a *App) notifyDetachedShellExit(bgID string, e *bgEntry) {
	tail := ""
	statusLine := exec.ExitStatusLine(0, false)
	// Read only the TAIL of the log: the marker sits at EOF and the notice
	// shows the last lines only — a multi-GB log must not be loaded in full
	// just to extract them (was ReadFile; review finding).
	if out, err := a.Exec.ReadFileTail(context.Background(), e.logPath, 8*1024); err == nil {
		// Parse the wrapper's exit marker (card #121 follow-up) so the notice
		// reports the actual exit status, not just "exited". The marker is
		// stripped from the tail shown to the model. A self-backgrounded
		// child writing after the marker can push it out of the window or
		// break end-anchoring → known=false → honest "code unknown".
		cleaned, code, known := exec.ParseExitMarker(out)
		statusLine = exec.ExitStatusLine(code, known)
		lines := strings.Split(strings.TrimRight(cleaned, "\n"), "\n")
		if n := len(lines); n > asyncShellNotifyTail {
			lines = lines[n-asyncShellNotifyTail:]
		}
		tail = strings.Join(lines, "\n")
	}
	msg := fmt.Sprintf("%s (\"%s\") %s — last output:\n%s\nuse read_process_log(%q) for the full output", bgID, e.cmdDigest, statusLine, tail, bgID)

	op := &asyncOp{
		id:        "job-" + bgID,
		toolName:  "run_shell",
		label:     e.cmdDigest,
		createdAt: e.startedAt,
		done:      make(chan struct{}),
	}
	op.mu.Lock()
	op.terminal = true
	op.result = msg
	op.shellLSPDirty = !e.readOnly
	op.mu.Unlock()
	close(op.done)

	a.asyncMu.Lock()
	defer a.asyncMu.Unlock()
	if a.asyncStopping {
		return
	}
	if a.asyncOps == nil {
		a.asyncOps = make(map[string]*asyncOp)
	}
	a.asyncOps[op.id] = op
	a.evictOldestTerminalLocked()
	a.asyncInbox = append(a.asyncInbox, op)
}

// handleOpenURL opens a URL on the host desktop after confirmation.
func (a *App) handleOpenURL(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	// Always runs on the host (not via a.Exec), so it reaches the user's
	// desktop even when shell commands are sandboxed.
	detail := fmt.Sprintf("xdg-open %s\n  (on the host desktop)", args.URL)
	if !a.Confirm("open_url", "Open in browser?", detail, false) {
		return "[declined by user]"
	}
	out, err := wtools.OpenOnHost(args.URL)
	result := formatResult(out, err)
	if !strings.HasPrefix(result, "ERROR:") {
		label := args.URL
		label = Truncate(label, 79)
		a.addExternalGrounding(proxy.GroundingEntry{Type: "web", Label: label})
	}
	return result
}

// handleReadFile reads a file with optional line-range, size guard, and
// toolcache spill-path interception.
func (a *App) handleReadFile(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	// Toolcache spill-path interception (root-cause fix for the
	// "subagent retries its own capped tool result forever" failure
	// mode): a path returned inside an earlier "[... at: PATH]" marker
	// (CapToolResult/StubToolResult/SpillFullResult, or a dispatch_subagent
	// summary spill) is a HOST-side wakil artifact — it was never part of
	// the sandboxed workspace, so Executor.ConfinePath rejects it
	// deterministically, every single time (Docker: the toolcache dir is
	// never bind-mounted; Direct: it's outside the workspace root either
	// way). Recognise it here and serve it directly from the host
	// filesystem, bypassing the executor entirely, instead of handing the
	// model a guaranteed dead end to retry until it exhausts its budget.
	if wtools.IsToolCacheHostPath(args.Path) {
		sizeLimit := int64(a.Cfg.ReadFileSizeLimit)
		if sizeLimit <= 0 {
			sizeLimit = 1 << 20
		}
		if args.Limit != 0 {
			sizeLimit = 0 // caller explicitly bounded the read; skip the guard, same as the executor path below
		}
		out, errResult := hostCacheReadResult(args.Path, sizeLimit, "read", "specify a line/byte range or use search_files.")
		if errResult != "" {
			return errResult
		}
		return formatFileView(out, args.Offset, args.Limit)
	}
	// Confine the path to the workspace (P0-3: path confinement).
	canonical, err := a.Exec.ConfinePath(ctx, args.Path)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	// Guard 1a: stat before reading — refuse oversized unbounded reads without
	// loading the file. Skip when a Limit is already set: the caller explicitly
	// bounded the read, so the size guard's advice ("specify a range") has been
	// taken and the refusal would be a dead end.
	sizeLimit := int64(a.Cfg.ReadFileSizeLimit)
	if sizeLimit <= 0 {
		sizeLimit = 1 << 20 // 1 MB safety net when config is zero
	}
	if args.Limit == 0 {
		if fileSize, serr := a.Exec.StatFile(ctx, canonical); serr == nil && fileSize > sizeLimit {
			return fmt.Sprintf(
				"ERROR: file is %.2f MB, exceeds read limit of %.2f MB — specify a line/byte range or use search_files.",
				float64(fileSize)/(1<<20), float64(sizeLimit)/(1<<20))
		}
	}
	out, err := a.Exec.ReadFile(ctx, canonical)
	// Redirect a directory read to the right tool instead of returning a raw
	// errno the model tends to retry against (a known subagent loop trigger).
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "is a directory") {
		return fmt.Sprintf("ERROR: %q is a directory, not a file — use list_dir to see its contents or search_files to search within it.", args.Path)
	}
	if err != nil {
		return formatResult(out, err)
	}
	// Guard 1b: refuse binary content detected via null-byte sniff.
	if strings.ContainsRune(out, 0) {
		return fmt.Sprintf(
			"ERROR: binary file, %.2f MB — not readable as text.",
			float64(len(out))/(1<<20))
	}
	return formatFileView(out, args.Offset, args.Limit)
}

// handleReadFileFull reads a full file with a higher size ceiling, spill-path
// interception, and post-read backstop.
func (a *App) handleReadFileFull(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	// Toolcache spill-path interception — see the matching comment in the
	// read_file handler above for the full rationale. read_file_full is, if
	// anything, the MORE common trigger in practice: it's the tool guided
	// to recover a "[full content at: PATH]" spill marker.
	if wtools.IsToolCacheHostPath(args.Path) {
		fullLimit := int64(a.Cfg.MaxFullReadBytes)
		if fullLimit <= 0 {
			fullLimit = 256 << 10
		}
		out, errResult := hostCacheReadResult(args.Path, fullLimit, "full-read", "use read_file with an offset/limit range instead.")
		if errResult != "" {
			return errResult
		}
		return formatFileView(out, 0, 0)
	}
	// Confine the path to the workspace (P0-3: path confinement).
	canonical, err := a.Exec.ConfinePath(ctx, args.Path)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	// Guard 1a: stat before reading — refuse oversized full reads without
	// loading the file. The ceiling is max_full_read_bytes (default 256 KB),
	// higher than read_file's windowed cap but well under MaxRequestBytes.
	fullLimit := int64(a.Cfg.MaxFullReadBytes)
	if fullLimit <= 0 {
		fullLimit = 256 << 10 // 256 KB safety net when config is zero
	}
	if fileSize, serr := a.Exec.StatFile(ctx, canonical); serr == nil && fileSize > fullLimit {
		return fmt.Sprintf(
			"ERROR: file is %.2f MB, exceeds full-read limit of %.2f MB — use read_file with an offset/limit range instead.",
			float64(fileSize)/(1<<20), float64(fullLimit)/(1<<20))
	}
	out, err := a.Exec.ReadFile(ctx, canonical)
	// Redirect a directory read to the right tool (same as read_file).
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "is a directory") {
		return fmt.Sprintf("ERROR: %q is a directory, not a file — use list_dir to see its contents or search_files to search within it.", args.Path)
	}
	if err != nil {
		return formatResult(out, err)
	}
	// Guard 1b: refuse binary content detected via null-byte sniff.
	if strings.ContainsRune(out, 0) {
		return fmt.Sprintf(
			"ERROR: binary file, %.2f MB — not readable as text.",
			float64(len(out))/(1<<20))
	}
	// Guard 1c: post-read size backstop — if StatFile errored (so the
	// pre-read guard was skipped), refuse if the loaded content exceeds
	// the ceiling. This prevents an un-stattable file from bypassing the
	// size guard entirely.
	if int64(len(out)) > fullLimit {
		return fmt.Sprintf(
			"ERROR: file is %.2f MB, exceeds full-read limit of %.2f MB — use read_file with an offset/limit range instead.",
			float64(len(out))/(1<<20), float64(fullLimit)/(1<<20))
	}
	return formatFileView(out, 0, 0)
}

// handleListDir lists directory contents after path confinement.
func (a *App) handleListDir(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	canonical, err := a.Exec.ConfinePath(ctx, args.Path)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	out, err := a.Exec.ListDir(ctx, canonical)
	return formatResult(out, err)
}

// handleFindFiles finds files by name pattern under a confined path.
func (a *App) handleFindFiles(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.Pattern == "" {
		return "ERROR: pattern is required"
	}
	findPath := args.Path
	if findPath == "" {
		findPath = "."
	}
	// Confine the path to the workspace (P0-3: path confinement).
	canonical, err := a.Exec.ConfinePath(ctx, findPath)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	// Constrained find: model-supplied values are single-quoted so no shell
	// metacharacters leak. Errors (permission denied) are dropped and the
	// list is capped so a huge tree can't flood ctx.
	const findCap = 200
	cmd := "find " + shellQuote(canonical) + " -type f -name " + shellQuote(args.Pattern) +
		fmt.Sprintf(" 2>/dev/null | head -n %d", findCap)
	out, err := a.Exec.RunShell(ctx, cmd)
	if err == nil && strings.TrimSpace(out) == "" {
		return "(no files found)"
	}
	if n := strings.Count(strings.TrimRight(out, "\n"), "\n") + 1; n >= findCap {
		out = strings.TrimRight(out, "\n") + fmt.Sprintf("\n… [capped at %d files — narrow the pattern or path]", findCap)
	}
	return formatResult(out, err)
}

// handleSearchFiles searches file contents with a controlled grep command.
func (a *App) handleSearchFiles(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		Pattern         string `json:"pattern"`
		Path            string `json:"path"`
		FilePattern     string `json:"file_pattern"`
		CaseInsensitive bool   `json:"case_insensitive"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.Pattern == "" || args.Path == "" {
		return "ERROR: pattern and path are required"
	}
	// Confine the path to the workspace (P0-3: path confinement).
	canonical, err := a.Exec.ConfinePath(ctx, args.Path)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	// Build a controlled grep command. All model-supplied values are
	// single-quoted so the model cannot inject shell metacharacters.
	cmd := "grep -rn"
	if args.CaseInsensitive {
		cmd += " -i"
	}
	if args.FilePattern != "" {
		cmd += " --include=" + shellQuote(args.FilePattern)
	}
	cmd += " -- " + shellQuote(args.Pattern) + " " + shellQuote(canonical)
	out, err := a.Exec.RunShell(ctx, cmd)
	// grep exits 1 when it finds zero matches — not an error.
	if err != nil && strings.TrimSpace(out) == "" && strings.Contains(err.Error(), "exit status 1") {
		return "(no matches)"
	}
	return formatResult(out, err)
}

// handleWriteFile writes content to a file after confirmation and path confinement.
func (a *App) handleWriteFile(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	// Confine the path to the workspace (P0-3: path confinement).
	canonical, err := a.Exec.ConfinePath(ctx, args.Path)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	preview := args.Content
	if len(preview) > 280 {
		preview = preview[:280] + "…"
	}
	detail := fmt.Sprintf("write_file %s (%d bytes) in %s\n--- content ---\n%s",
		args.Path, len(args.Content), a.Exec.Describe(), preview)
	if !a.Confirm("write_file", "Write file?", detail, false) {
		return "[declined by user]"
	}
	// Snapshot pre-edit state for restore (copy-on-first-write, edit-tier only).
	a.captureFileOriginal(ctx, canonical)
	out, err := a.Exec.WriteFile(ctx, canonical, args.Content)
	if err == nil && a.LSP != nil {
		a.LSP.NotifyChange(ctx, canonical)
	}
	if err == nil {
		a.recordFileChanged(canonical)
	}
	return formatResult(out, err)
}

// handleWriteBinaryFile decodes a base64-encoded string to raw bytes and writes
// them to a file. Used for binary data (images, PDFs, xlsx, executables) that
// cannot be represented as a JSON string for write_file.
//
// Sequence: parse → validate → ConfinePath → size pre-check → decode → size
// backstop → Confirm → snapshot → WriteFileBytes → LSP notify → record.
// Decoding happens BEFORE confirm so invalid base64 fails fast without a prompt
// and the confirm detail shows the true decoded byte count.
func (a *App) handleWriteBinaryFile(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		Path          string `json:"path"`
		ContentBase64 string `json:"content_base64"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.Path == "" {
		return "ERROR: path is required"
	}
	if args.ContentBase64 == "" {
		return "ERROR: content_base64 is required"
	}
	// Confine the path to the workspace (P0-3: path confinement).
	canonical, err := a.Exec.ConfinePath(ctx, args.Path)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	// Size limit: pre-check encoded length before allocating decoded buffer.
	// base64 expands by ~4/3, so encoded_len * 3/4 is the upper bound on decoded size.
	limit := int64(a.Cfg.MaxBinaryWriteBytes)
	if limit <= 0 {
		limit = 10 << 20 // 10 MB default safety net
	}
	// Strip whitespace/newlines that models often insert, then validate.
	cleaned := strings.Join(strings.Fields(args.ContentBase64), "")
	if cleaned == "" {
		return "ERROR: content_base64 is empty after stripping whitespace"
	}
	encodedLen := int64(len(cleaned))
	// Pre-check: encoded length * 3/4 must not exceed limit (upper bound on decoded).
	if encodedLen*3/4 > limit {
		return fmt.Sprintf(
			"ERROR: decoded content would exceed binary write limit of %.2f MB — content_base64 is %d chars (~%.2f MB decoded).",
			float64(limit)/(1<<20), encodedLen, float64(encodedLen*3/4)/(1<<20))
	}
	// Decode base64 (standard padded encoding).
	data, err := base64.StdEncoding.DecodeString(cleaned)
	if err != nil {
		return "ERROR: invalid base64 content_base64: " + err.Error()
	}
	// Backstop: verify exact decoded length against limit.
	if int64(len(data)) > limit {
		return fmt.Sprintf(
			"ERROR: decoded content (%d bytes) exceeds binary write limit of %.2f MB.",
			len(data), float64(limit)/(1<<20))
	}
	// Confirm with decoded size + sha256 (raw bytes are not useful to display).
	sum := sha256.Sum256(data)
	detail := fmt.Sprintf("write_binary_file %s (%d decoded bytes, sha256:%x) in %s",
		args.Path, len(data), sum, a.Exec.Describe())
	if !a.Confirm("write_binary_file", "Write binary file?", detail, false) {
		return "[declined by user]"
	}
	// Snapshot pre-edit state for restore (copy-on-first-write, edit-tier only).
	a.captureFileOriginal(ctx, canonical)
	out, err := a.Exec.WriteFileBytes(ctx, canonical, data)
	if err == nil && a.LSP != nil {
		// Only notify LSP for valid UTF-8 content. Binary files (non-UTF-8)
		// are not text documents and would corrupt LSP state if sent as
		// didChange text content. detectLanguage also filters non-code
		// extensions, but this is an extra guard for code-extension paths
		// that happen to contain binary data.
		if utf8.Valid(data) {
			a.LSP.NotifyChange(ctx, canonical)
		}
	}
	if err == nil {
		a.recordFileChanged(canonical)
	}
	return formatResult(out, err)
}

// handleSearxngSearch performs a SearXNG search and records grounding/cost.
func (a *App) handleSearxngSearch(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		Query      string `json:"query"`
		Categories string `json:"categories"`
		TimeRange  string `json:"time_range"`
		Engines    string `json:"engines"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	result, urls := wtools.CallSearxng(a.Cfg.SearXngURL, args.Query, args.Categories, args.TimeRange, args.Engines)
	a.RecordSearchCost()
	for i, u := range urls {
		if i >= 5 {
			break
		}
		label := u
		label = Truncate(label, 79)
		a.addExternalGrounding(proxy.GroundingEntry{Type: "web", Label: label})
	}
	return result
}

// handleSearxngURLRead fetches a URL's content via SearXNG and records grounding.
func (a *App) handleSearxngURLRead(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	result := wtools.FetchURL(args.URL)
	if !strings.HasPrefix(result, "ERROR:") {
		label := args.URL
		label = Truncate(label, 79)
		a.addExternalGrounding(proxy.GroundingEntry{Type: "web", Label: label})
	}
	return result
}

// handleGoogleSearch performs a Google Custom Search and records grounding/cost.
func (a *App) handleGoogleSearch(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		Query  string `json:"query"`
		Num    int    `json:"num"`
		Start  int    `json:"start"`
		After  string `json:"after"`
		Before string `json:"before"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	result, urls := wtools.CallGoogle(a.Cfg.GoogleAPIKey, a.Cfg.GoogleCX, args.Query, args.Num, args.Start, args.After, args.Before)
	a.RecordSearchCost()
	for i, u := range urls {
		if i >= 5 {
			break
		}
		label := Truncate(u, 79)
		a.addExternalGrounding(proxy.GroundingEntry{Type: "web", Label: label})
	}
	return result
}

// handleGoogleFetchURL fetches a URL's readable text content and records grounding.
func (a *App) handleGoogleFetchURL(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		URL      string `json:"url"`
		MaxChars int    `json:"max_chars"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	result := wtools.GoogleFetchURL(args.URL, args.MaxChars)
	if !strings.HasPrefix(result, "ERROR:") {
		label := Truncate(args.URL, 79)
		a.addExternalGrounding(proxy.GroundingEntry{Type: "web", Label: label})
	}
	return result
}

// handleDeleteFile deletes a file after confirmation and path confinement.
func (a *App) handleDeleteFile(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.Path == "" {
		return "ERROR: path is required"
	}
	canonical, err := a.Exec.ConfinePath(ctx, args.Path)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	rel, _ := filepath.Rel(a.Exec.WorkspaceRoot(), canonical)
	detail := fmt.Sprintf("Delete file: %s\n  (%s)", rel, a.Exec.Describe())
	if !a.Confirm("delete_file", "Delete file?", detail, false) {
		return "[declined by user]"
	}
	// Snapshot pre-edit state for restore (copy-on-first-write, edit-tier only).
	a.captureFileOriginal(ctx, canonical)
	if err := a.Exec.DeletePath(ctx, canonical); err != nil {
		return "ERROR: " + err.Error()
	}
	a.recordFileChanged(canonical)
	return "deleted: " + rel
}

// handleMoveFile moves/renames a file after confirmation and dual path confinement.
func (a *App) handleMoveFile(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		Src string `json:"src"`
		Dst string `json:"dst"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.Src == "" || args.Dst == "" {
		return "ERROR: src and dst are required"
	}
	canonSrc, err := a.Exec.ConfinePath(ctx, args.Src)
	if err != nil {
		return "ERROR: src — " + err.Error()
	}
	canonDst, err := a.Exec.ConfinePath(ctx, args.Dst)
	if err != nil {
		return "ERROR: dst — " + err.Error()
	}
	root := a.Exec.WorkspaceRoot()
	relSrc, _ := filepath.Rel(root, canonSrc)
	relDst, _ := filepath.Rel(root, canonDst)
	detail := fmt.Sprintf("Move: %s → %s\n  (%s)", relSrc, relDst, a.Exec.Describe())
	if !a.Confirm("move_file", "Move file?", detail, false) {
		return "[declined by user]"
	}
	// Snapshot pre-edit state for restore (both source and destination).
	a.captureFileOriginal(ctx, canonSrc)
	a.captureFileOriginal(ctx, canonDst)
	if err := a.Exec.MovePath(ctx, canonSrc, canonDst); err != nil {
		return "ERROR: " + err.Error()
	}
	a.recordFileChanged(canonSrc)
	a.recordFileChanged(canonDst)
	return fmt.Sprintf("moved: %s → %s", relSrc, relDst)
}

// handleRunBackground starts a background process with logging and a reaper goroutine.
func (a *App) handleRunBackground(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		Command string `json:"command"`
		Label   string `json:"label"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.Command == "" {
		return "ERROR: command is required"
	}
	if args.Label == "" {
		args.Label = "bg"
	}
	a.bgMu.Lock()
	if a.bgProcs == nil {
		a.bgProcs = make(map[string]*bgEntry)
	}
	// Count live processes (those matching the current generation).
	live := 0
	for _, e := range a.bgProcs {
		if e.generation == a.Exec.Generation() && a.Exec.IsProcessGroupAlive(ctx, e.pgid) {
			live++
		}
	}
	if live >= 5 {
		a.bgMu.Unlock()
		return "ERROR: maximum of 5 concurrent background processes reached — kill one first"
	}
	a.bgCounter++
	n := a.bgCounter
	a.bgMu.Unlock()
	// Use a flat path in /tmp (no subdirectory) for the log file. In Docker
	// mode the container's /tmp is a writable tmpfs, and the shell redirect
	// creates the file directly — no directory creation needed. A host-side
	// os.MkdirTemp would create a directory that doesn't exist inside the
	// container, causing the redirect to fail silently.
	logPath := fmt.Sprintf("/tmp/wakil-bg-%d.log", n)
	bgID := fmt.Sprintf("bg%d", n)
	detail := fmt.Sprintf("$ %s (background)\n  label=%s, log=%s\n  (%s)",
		args.Command, args.Label, logPath, a.Exec.Describe())
	if !a.Confirm("run_background", "Start background process?", detail, false) {
		// No counter rollback: IDs stay monotonic (no future bgN collision).
		return "[declined by user]"
	}
	pid, pgid, err := a.Exec.StartBackground(ctx, exec.WrapExitMarker(args.Command, logPath), logPath)
	if err != nil {
		// No counter rollback: IDs stay monotonic (no future bgN collision).
		return "ERROR: " + err.Error()
	}
	done := make(chan struct{})
	entry := &bgEntry{
		id:         bgID,
		pid:        pid,
		pgid:       pgid,
		label:      args.Label,
		logPath:    logPath,
		startedAt:  time.Now(),
		generation: a.Exec.Generation(),
		done:       done,
	}
	a.bgMu.Lock()
	a.bgProcs[bgID] = entry
	a.bgMu.Unlock()
	// Reaper goroutine: poll GROUP liveness and close done when the group
	// exits. This lets StopAllBackgroundProcs wait for clean shutdown without
	// a fixed sleep. Uses a background context — the process may outlive the
	// turn context that started it.
	safe.Go("bg-reaper", func() {
		bgCtx := context.Background()
		for {
			if !a.Exec.IsProcessGroupAlive(bgCtx, pgid) {
				close(done)
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	})
	return fmt.Sprintf("id: %s\npid: %d\nlog: %s\nlabel: %s", bgID, pid, logPath, args.Label)
}

// handleKillProcess kills a background process by ID with SIGTERM→SIGKILL escalation.
func (a *App) handleKillProcess(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	a.bgMu.RLock()
	entry, ok := a.bgProcs[args.ID]
	a.bgMu.RUnlock()
	if !ok {
		return fmt.Sprintf("ERROR: no background process with id %q", args.ID)
	}
	if entry.generation != a.Exec.Generation() {
		a.bgMu.Lock()
		delete(a.bgProcs, args.ID)
		a.bgMu.Unlock()
		return fmt.Sprintf("[%s] process lost (container restarted)", args.ID)
	}
	detail := fmt.Sprintf("kill_process %s (%s) pgid=%d\n  (%s)", args.ID, entry.label, entry.pgid, a.Exec.Describe())
	if !a.Confirm("kill_process", "Kill background process?", detail, false) {
		return "[declined by user]"
	}
	if !a.Exec.IsProcessGroupAlive(ctx, entry.pgid) {
		a.bgMu.Lock()
		delete(a.bgProcs, args.ID)
		a.bgMu.Unlock()
		return fmt.Sprintf("[%s] already exited", args.ID)
	}
	// Card #121: an intentional kill must not produce a completion notice —
	// disarm before signalling so the reaper (which races this path) stays
	// silent even if it observes the exit first.
	a.bgMu.Lock()
	entry.notifyOnExit = false
	a.bgMu.Unlock()
	_ = a.Exec.KillPgid(ctx, entry.pgid, 15) // SIGTERM
	// Wait up to 5s for the GROUP to exit, then SIGKILL. Group check (not the
	// leader pid): since card #121 the leader is the exit-marker wrapper shell,
	// which can die to SIGTERM while a signal-ignoring child survives.
	// The lock is NOT held during this wait — see bgMu comment.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "ERROR: kill_process cancelled: " + ctx.Err().Error()
		case <-time.After(200 * time.Millisecond):
		}
		if !a.Exec.IsProcessGroupAlive(ctx, entry.pgid) {
			a.bgMu.Lock()
			delete(a.bgProcs, args.ID)
			a.bgMu.Unlock()
			return fmt.Sprintf("[%s] terminated (SIGTERM)", args.ID)
		}
	}
	_ = a.Exec.KillPgid(ctx, entry.pgid, 9) // SIGKILL
	a.bgMu.Lock()
	delete(a.bgProcs, args.ID)
	a.bgMu.Unlock()
	return fmt.Sprintf("[%s] killed (SIGKILL after 5s timeout)", args.ID)
}

// handleReadProcessLog reads the tail of a background process's log file.
func (a *App) handleReadProcessLog(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	a.bgMu.RLock()
	entry, ok := a.bgProcs[args.ID]
	a.bgMu.RUnlock()
	if !ok {
		return fmt.Sprintf("ERROR: no background process with id %q", args.ID)
	}
	if entry.generation != a.Exec.Generation() {
		a.bgMu.Lock()
		delete(a.bgProcs, args.ID)
		a.bgMu.Unlock()
		return fmt.Sprintf("[%s] process lost (container restarted)", args.ID)
	}
	alive := a.Exec.IsProcessGroupAlive(ctx, entry.pgid)
	status := "running"
	if !alive {
		status = "exited"
	}
	const maxLogBytes = 8 * 1024
	tail, err := a.Exec.ReadFileTail(ctx, entry.logPath, maxLogBytes)
	if err != nil {
		header := fmt.Sprintf("[%s %s] %s pid=%d\n", args.ID, entry.label, status, entry.pid)
		return header + "(log not yet available)"
	}
	// Exit-code recovery (card #121 follow-up): exited processes carry the
	// wrapper's marker at the end of the log — parse it, strip it from the
	// shown tail, and surface the code in the status line.
	if !alive {
		if cleaned, code, known := exec.ParseExitMarker(tail); known {
			tail = cleaned
			status = fmt.Sprintf("exited (code %d)", code)
		} else {
			status = "exited (code unknown)"
		}
	}
	header := fmt.Sprintf("[%s %s] %s pid=%d\n", args.ID, entry.label, status, entry.pid)
	// Enforce hard cap: header + tail must not exceed 8KB + small overhead.
	result := header + tail
	if len(result) > maxLogBytes+256 {
		result = result[:maxLogBytes+256]
	}
	return result
}

// handleDispatchSubagent dispatches a single subagent sequentially.
func (a *App) handleDispatchSubagent(ctx context.Context, tc proxy.ToolCall) string {
	var args struct {
		Task       string `json:"task"`
		Capability string `json:"capability"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.Task == "" {
		return "ERROR: task is required"
	}
	// Normalize capability: empty = discovery (the default, golden no-op).
	capability := args.Capability
	if capability == "" {
		capability = wtools.CapabilityDiscovery
	}
	if !wtools.ValidCapability(capability) {
		return fmt.Sprintf("ERROR: unknown capability %q — valid values: %q (default), %q, %q",
			args.Capability, wtools.CapabilityDiscovery, wtools.CapabilityEdit, wtools.CapabilityTools)
	}
	// Consent gate: edit capability requires the session to have write consent.
	// This deliberately mirrors the parent's own write predicate: in /auto
	// mode (AutoApprove=true), write_file/edit_file/delete_file/move_file
	// auto-approve via SuspendAuto (they are not classified as "destructive"
	// by IsDestructiveShell, which only covers run_shell/run_background).
	// AllowDestructive and AllowReads are NOT consulted for these tools —
	// the parent's write gate is AutoApprove alone (see tuiConfirmer in
	// commands.go, headlessConfirmer at run.go, and SuspendAuto in commands.go
	// which returns "" for all four
	// edit-category tools). Without AutoApprove the parent's confirmer
	// would prompt for each write; a child cannot prompt, so edit dispatch
	// is rejected. Do not silently downgrade to discovery — a silent
	// downgrade produces a child that "completes" without doing the work,
	// which is a fabricated success.
	// INVARIANT: child may write iff parent may write. If the parent's write
	// predicate ever changes (e.g. a new consent bool is added), this gate
	// MUST be updated to match — the two must move together.
	if capability == wtools.CapabilityEdit && !a.Consent().AutoApprove {
		return "ERROR: edit capability requires /auto or --auto (session write consent). " +
			"Re-dispatch with capability \"discovery\" (the default) for read-only research."
	}
	// Consent gate: tools capability also requires /auto or --auto. The tools
	// tier exposes MCP/LSP/web search to unattended children with no per-call
	// confirm gate. The user's config (SubagentMCPServers allowlist) is the
	// consent surface for which servers are exposed; /auto is the session-level
	// trust that the agent may call them without prompting. This mirrors the
	// edit tier pattern: consent at dispatch, not per-call.
	if capability == wtools.CapabilityTools && !a.Consent().AutoApprove {
		return "ERROR: tools capability requires /auto or --auto (session consent for external tool access). " +
			"Re-dispatch with capability \"discovery\" (the default) for read-only research."
	}
	fmt.Fprintln(a.Out, Dim("· subagent: "+Truncate(args.Task, 60)))
	subChatID := NewChatID()
	// Backend resolution only applies when the child's resolved endpoint is
	// kind ilm-proxy; for kind openai there is no backend-routing concept to
	// resolve, so skip it entirely rather than compute an inert value.
	subBackend := a.resolveSubagentBackendForEndpoint(a.resolvedSubagentEndpointKind())
	a.ensureSubagentLimitsCache()
	// Consent gate BEFORE the start event: a declined dispatch never opens
	// a TUI tab. This is the sequential path; the parallel block path runs
	// ensureSubagentConsent once for the whole block in its prepare phase.
	if !a.ensureSubagentConsent(subBackend) {
		return declinedSubagentSummary(args.Task, subBackend).Render()
	}
	a.sendEvent(SubagentStartMsg{
		Task:       args.Task,
		ChatID:     subChatID,
		Backend:    subBackend,
		Capability: capability,
		Model:      a.resolvedSubagentDisplayModel(),
		ToolNames:  a.subagentToolNames(capability),
	})
	// Sequential path: the dispatch begins immediately, no queue wait.
	a.sendEvent(SubagentActiveMsg{ChatID: subChatID})
	summary, grounding, ctxSize, usedBackend, costRows, filesChanged := a.dispatchSubagent(ctx, args.Task, subagentProgressOut(a, subChatID), subBackend, capability, subChatID)
	// Early display-only completion event: emitted at child return, before
	// the cost fold, keeping the parallel and sequential paths symmetric.
	// SubagentDoneMsg below remains the authoritative event carrying the
	// folded state; the TUI treats it as idempotent finalization of a tab
	// that may already be visually done via this earlier event.
	a.sendSubagentFinished(subChatID, subagentJobResult{
		Summary:      summary,
		CostRows:     costRows,
		FilesChanged: filesChanged,
	})
	// Cost fold happens HERE — the caller's side of the goroutine boundary,
	// where parent-state mutation is already safe — never inside
	// dispatchSubagent. The child's fresh CostTracker never touches a.Costs
	// directly; this is the only place its rows are merged in.
	subagentCostUSD := foldSubagentCost(a.Costs, costRows)
	a.sendEvent(SubagentDoneMsg{
		ChatID:       subChatID,
		Grounding:    grounding,
		CtxSize:      ctxSize,
		HardMaxBytes: subagentHardMaxBytes,
		UsedBackend:  usedBackend,
		CostUSD:      subagentCostUSD,
		FilesChanged: filesChanged,
	})

	// Part C: durable summary persistence. Write the full structured summary
	// to disk under the PARENT's chatID (which is valid, unlike the subagent's
	// ephemeral chatID). The in-context result carries a breadcrumb path so
	// the parent's compaction can dissolve the body without losing the
	// findings — the main agent can read_file the path to recover detail.
	fullJSON := summary.Render()
	result := fullJSON
	if spillPath := wtools.SpillToCache(a.chatID(), "dispatch_subagent", fullJSON); spillPath != "" {
		result = fullJSON + fmt.Sprintf("\n[subagent summary at: %s]", spillPath)
	}
	// Append the mechanical files_changed list (ground truth) for edit-tier
	// children. When the model's self-reported files_changed disagrees with
	// the mechanical record, render both with the discrepancy noted.
	if len(filesChanged) > 0 {
		result += renderFilesChanged(summary.FilesChanged, filesChanged)
	}

	// Loud warning on exhaustion — not a dim tool-line. The main agent must
	// know the subagent ran out of budget so it can re-dispatch narrower or
	// take over, rather than trusting potentially-incomplete findings.
	if summary.Status == "incomplete" {
		fmt.Fprintln(a.Out, Yellow("⚠ subagent ran out of budget on task: "+Truncate(args.Task, 80)))
		fmt.Fprintln(a.Out, Yellow("  partial findings returned — consider re-dispatching narrower or taking over"))
	}
	return result
}

// handleDispatchSubagents dispatches multiple subagents concurrently via the
// parallel block runner.
func (a *App) handleDispatchSubagents(ctx context.Context, tc proxy.ToolCall) string {
	// Batch front-end onto the same fan-out core as the per-turn contiguous
	// block path: explicit, observable parallelism under one tool_call_id.
	var args struct {
		Tasks      []string `json:"tasks"`
		Capability string   `json:"capability"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if len(args.Tasks) == 0 {
		return "ERROR: tasks is required (1–8 discovery objectives)"
	}
	const maxBatchTasks = 8
	if len(args.Tasks) > maxBatchTasks {
		return fmt.Sprintf("ERROR: too many tasks (%d) — maximum is %d per batch", len(args.Tasks), maxBatchTasks)
	}
	for i, task := range args.Tasks {
		if strings.TrimSpace(task) == "" {
			return fmt.Sprintf("ERROR: task %d is empty", i+1)
		}
	}
	// Normalize capability: empty = discovery (the default, golden no-op).
	capability := args.Capability
	if capability == "" {
		capability = wtools.CapabilityDiscovery
	}
	if !wtools.ValidCapability(capability) {
		return fmt.Sprintf("ERROR: unknown capability %q — valid values: %q (default), %q, %q",
			args.Capability, wtools.CapabilityDiscovery, wtools.CapabilityEdit, wtools.CapabilityTools)
	}
	// Consent gate (same as sequential path).
	if capability == wtools.CapabilityEdit && !a.Consent().AutoApprove {
		return "ERROR: edit capability requires /auto or --auto (session write consent). " +
			"Re-dispatch with capability \"discovery\" (the default) for read-only research."
	}
	// Consent gate: tools capability also requires /auto (same as edit).
	if capability == wtools.CapabilityTools && !a.Consent().AutoApprove {
		return "ERROR: tools capability requires /auto or --auto (session consent for external tool access). " +
			"Re-dispatch with capability \"discovery\" (the default) for read-only research."
	}
	// Reuse the block runner: build synthetic single-task calls so Phase
	// A→B→C runs identically; then aggregate into one JSON array result.
	block := make([]proxy.ToolCall, len(args.Tasks))
	for i, task := range args.Tasks {
		taskJSON, _ := json.Marshal(struct {
			Task       string `json:"task"`
			Capability string `json:"capability"`
		}{task, capability})
		block[i] = proxy.ToolCall{
			ID:       fmt.Sprintf("%s-b%d", tc.ID, i),
			Function: proxy.FunctionCall{Name: "dispatch_subagent", Arguments: string(taskJSON)},
		}
	}
	results := a.runParallelSubagentBlock(ctx, block)
	agg, err := json.Marshal(results)
	if err != nil {
		return fmt.Sprintf("ERROR: could not aggregate results: %v", err)
	}
	return string(agg)
}

// handleLSPReadOnly dispatches read-only LSP code-intelligence tools.
func (a *App) handleLSPReadOnly(ctx context.Context, tc proxy.ToolCall) string {
	if a.LSP == nil {
		return "[lsp: LSP is not enabled. Configure lsp_enabled in config.]"
	}
	return a.LSP.HandleLSPReadOnly(ctx, tc.Function.Name, tc.Function.Arguments)
}

// handleBrowserTool dispatches headless browser tools (navigate, screenshot,
// viewport, click, eval, text, html, reduced_motion). Read-only/interactive —
// no confirmation needed (the browser runs as a child of the wakil process).
func (a *App) handleBrowserTool(ctx context.Context, tc proxy.ToolCall) string {
	if a.Browser == nil {
		return "[browser: browser tools are not enabled. Configure browser_enabled in config.]"
	}
	return a.Browser.HandleToolCall(ctx, tc.Function.Name, tc.Function.Arguments)
}

// handleMCPTool routes an MCP namespaced tool call through the MCP manager.
func (a *App) handleMCPTool(ctx context.Context, tc proxy.ToolCall) string {
	name := tc.Function.Name
	// MCP tool — namespaced as "{server}__{tool}".
	// URL extraction from arbitrary MCP result payloads would require a fragile
	// scraper; emit one opaque provenance entry per successful call instead.
	if a.MCP != nil && strings.Contains(name, mcpNS) {
		serverName, toolName, _ := strings.Cut(name, mcpNS)
		// Serialize mutating MCP calls across parallel children: acquire
		// subagentMCPMu when the tool looks mutating (not classified as read
		// by IsMCPReadTool). This is defense-in-depth against parallel races
		// on external APIs (e.g. two children creating duplicate cards).
		// IsMCPReadTool is a hint here, not a security boundary — the
		// allowlist (SubagentMCPServers) is the security boundary.
		if !IsMCPReadTool(toolName) {
			subagentMCPMu.Lock()
			defer subagentMCPMu.Unlock()
		}
		result := a.MCP.CallTool(ctx, name, tc.Function.Arguments, a.Confirm, a.Consent().AllowReads)
		// Record the external action for audit (tools-tier children only;
		// nil-safe for the parent and other tiers).
		status := "ok"
		if strings.HasPrefix(result, "ERROR:") || result == "[declined by user]" {
			status = "error"
		}
		a.recordExternalAction(serverName, toolName, status)
		if !strings.HasPrefix(result, "ERROR:") && result != "[declined by user]" {
			label := Truncate(name+" result", 79)
			a.addExternalGrounding(proxy.GroundingEntry{Type: "web", Label: label})
		}
		return result
	}
	return fmt.Sprintf("ERROR: unknown tool %q", name)
}
