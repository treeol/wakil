package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/counsel"
	"github.com/treeol/wakil/internal/diag"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/tui"
	"github.com/treeol/wakil/internal/wiring"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	// "wakil version" / "wakil doctor": no config load, no side effects.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case "version", "--version", "-version":
			printVersion(os.Stdout)
			return
		case "doctor":
			os.Exit(runDoctor(os.Stdout, os.Args[2:], execRunner))
		}
	}

	// "wakil sandbox trust [dir]": the user approves the project's
	// .wakil/sandbox.json. Interactive only (reads /dev/tty); never reachable
	// with a flag that skips the question.
	if len(os.Args) >= 3 && os.Args[1] == "sandbox" && os.Args[2] == "trust" {
		dir := ""
		if len(os.Args) > 3 {
			dir = os.Args[3]
		}
		if err := wiring.SandboxTrust(os.Stdout, dir, wiring.OpenControllingTTY); err != nil {
			fmt.Fprintln(os.Stderr, "sandbox trust:", err)
			os.Exit(1)
		}
		return
	}

	// "wakil run" subcommand: headless, non-interactive, exits with a code.
	if len(os.Args) >= 2 && os.Args[1] == "run" {
		cfg, err := config.LoadConfig(nil) // flags after "run" are for RunHeadless, not LoadConfig
		if err != nil {
			fmt.Fprintln(os.Stderr, "config error:", err)
			os.Exit(ExitError)
		}
		cfg, err = wiring.ResolveSandbox(context.Background(), cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "startup error:", err)
			os.Exit(ExitError)
		}
		fmt.Fprintln(os.Stderr, cfg.StartupBanner())
		os.Exit(RunHeadless(cfg, os.Args[2:]))
	}

	// "wakil plan gc" subcommand: list and optionally delete dead plan/current
	// (and plan/tainted/) memory entries whose owning session no longer exists.
	if len(os.Args) >= 2 && os.Args[1] == "plan" && len(os.Args) >= 3 && os.Args[2] == "gc" {
		os.Exit(runPlanGC(os.Args[3:]))
	}

	// "wakil state migrate --from-legacy [--apply]" (0.5.0, C3): copy legacy
	// shared-store memory entries into their project's store. Dry-run by default.
	if len(os.Args) >= 2 && os.Args[1] == "state" {
		os.Exit(wiring.RunState(os.Args[2:], os.Stdout, os.Stderr))
	}

	// --list-sessions short-circuits before config resolution so it works even
	// without a configured proxy. Scoped to the launch cwd by default (no config
	// has been loaded yet, so cwd is the only workspace identity available);
	// --all lists every session regardless of folder.
	listAll := false
	wantList := false
	for _, a := range os.Args[1:] {
		switch a {
		case "--list-sessions", "-list-sessions":
			wantList = true
		case "--all", "-all":
			listAll = true
		}
	}
	if wantList {
		cwd, _ := os.Getwd()
		wiring.PrintSessions(os.Stdout, cwd, listAll)
		return
	}

	// --print-workspace-key: report the store keys this launch directory and
	// config would use, then exit (documented in docker-compose.yml). Read-only:
	// no executor, no stores, no network. The flag is removed from argv before
	// the normal flag parse, which does not know it.
	if args, printKeys := stripFlag(os.Args[1:], "print-workspace-key"); printKeys {
		cfg, err := config.LoadConfig(args)
		if err != nil {
			fmt.Fprintln(os.Stderr, "config error:", err)
			os.Exit(2)
		}
		fmt.Print(wiring.WorkspaceKeysReport(cfg))
		return
	}

	cfg, err := config.LoadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "ctx limits: compactAt=%d hardMax=%d keep=%d summary=%d\n",
		cfg.CompactAt, cfg.HardMaxBytes, cfg.KeepBytes, cfg.SummaryBytes)
	// Settle the backend (the default may flip to hisn) BEFORE anything else
	// reads it, and state it with its reason. A broken or unapproved
	// .wakil/sandbox.json is a startup error — no fallback, no session.
	cfg, rerr := wiring.ResolveSandbox(context.Background(), cfg)
	if rerr != nil {
		fmt.Fprintln(os.Stderr, "startup error:", rerr)
		os.Exit(ExitError)
	}
	// First-run acknowledgement (interactive TUI only): a missing or mismatched
	// risk ack is offered inline. y writes and approves exactly as
	// `wakil sandbox trust`; N exits with the error. Any other run keeps the
	// hard error from NewExecutor.
	if wiring.IsInteractiveTUI() && cfg.ExecMode != "direct" &&
		cfg.Sandbox.Backend == config.SandboxBackendHisn {
		if ferr := wiring.AckOffer(cfg, os.Stdin, os.Stdout); ferr != nil {
			fmt.Fprintln(os.Stderr, "startup error:", ferr)
			os.Exit(ExitError)
		}
	}
	fmt.Fprintln(os.Stderr, cfg.StartupBanner())
	// D4 (0.5.0): once per project, point at the read-only legacy store and the
	// migrate command. Then the WAKIL_WORKSPACE_PATH line (warning only, since
	// 0.5.0 the variable no longer changes store keys).
	if line := wiring.LegacyNotice(cfg); line != "" {
		fmt.Fprintln(os.Stderr, "notice:", line)
	}
	if line := wiring.WorkspaceEnvOverrideLine(cfg); line != "" {
		fmt.Fprintln(os.Stderr, line)
	}

	// Resolve --resume / --resume-id to a session id/prefix (global search —
	// the same rule the TUI's /resume <id> follows). Bare --resume (no id)
	// defaults to the most recent session in the CURRENT workspace; --all
	// overrides this to search every folder. The manager's ResumeConversation
	// resolves the prefix and restores the transcript.
	resumeID := ""
	if cfg.Resume || cfg.ResumeID != "" {
		id := cfg.ResumeID
		if id == "" {
			// Most recent session in the current workspace (or everywhere
			// with --all), resolved the same way App.SessionWorkspace() would
			// (host path in docker mode, work dir in direct mode).
			ws := cfg.WorkDir
			if cfg.ExecMode != "direct" {
				ws = cfg.HostWorkDir
			}
			resolved, err := wiring.ResolveRecentSession(ws, cfg.AllSessions)
			if err != nil {
				fmt.Fprintln(os.Stderr, "resume error:", err)
				os.Exit(1)
			}
			if resolved != "" {
				id = resolved
			}
		}
		resumeID = id
	}

	// Background tool-cache retention sweep (once per process; 0 = off).
	// A resumed chat (exact id) is exempt even if its spill files are older
	// than the retention window — its transcript still points at them.
	wiring.SweepToolCacheOnce(cfg.ToolCacheRetentionDays, resumeID)

	// Redirect os.Stderr to a temp file so any fmt.Fprintln(os.Stderr, ...)
	// during the async bootstrap (NewExecutor, BuildApp, BootstrapTUI) cannot
	// garble the alt-screen terminal. The temp file contents are copied into
	// the session log after bootstrap completes, then removed.
	stderrFile, err := os.CreateTemp("", "wakil-stderr-*.log")
	if err != nil {
		fmt.Fprintln(os.Stderr, "startup error: cannot create stderr redirect:", err)
		os.Exit(1)
	}
	origStderr := os.Stderr
	os.Stderr = stderrFile
	restoreStderr := func() {
		os.Stderr = origStderr
		_ = stderrFile.Close()
	}

	// Build the bootstrap tea.Cmd: runs NewExecutor + attach-image + BootstrapTUI
	// as one unit. SubscribeLive + StartEventPump run in the swap function
	// (they need prog.Send which is only live after prog.Run() starts).
	counselMode := cfg.AutoCounsel
	if counselMode == "" {
		counselMode = "suggest"
	}
	counselMax := cfg.CounselMaxPerSession
	if counselMode == "auto" && counselMax == 0 {
		counselMax = 3
	}

	// bootstrapDoneMsg carries the result of the async bootstrap.
	// Defined at package level (see bootstrap_done.go) so it can implement
	// the tui.BootstrapDone interface.

	// postRunCleanup holds the composed cleanup function (runtime + session
	// log) set by the swap function and called after prog.Run() returns.
	var postRunCleanup func()
	var postRunExeClose func() error

	// bsResult stores the bootstrap goroutine's result for cleanup if the
	// user quits before the swap function runs.
	var bsResult *bootstrapDoneMsg
	var bsResultMu sync.Mutex
	bsDone := make(chan struct{}) // closed when the bootstrap goroutine finishes

	// swapHandled is set to true when swapFn handles cleanup itself (error
	// paths). main() checks this to avoid double-cleanup.
	var swapHandled bool

	bootstrapCmd := func() tea.Msg {
		// NewExecutor (container startup — the biggest cost).
		if cfg.ExecMode == "direct" {
			tui.SendLoadingProgress("starting executor…")
		} else {
			tui.SendLoadingProgress("starting container…")
		}
		exe, err := wiring.NewExecutor(cfg)
		if err != nil {
			r := bootstrapDoneMsg{err: fmt.Errorf("executor: %w", err)}
			bsResultMu.Lock()
			bsResult = &r
			bsResultMu.Unlock()
			close(bsDone)
			return r
		}

		// --attach-image: load into pending images for the first message.
		var attach []proxy.ImagePart
		if cfg.AttachImage != "" {
			tui.SendLoadingProgress("loading images…")
			for _, p := range strings.Split(cfg.AttachImage, ",") {
				p = strings.TrimSpace(p)
				if p == "" {
					continue
				}
				img, err := proxy.LoadImage(p)
				if err != nil {
					exe.Close()
					r := bootstrapDoneMsg{err: fmt.Errorf("attach-image: %w", err)}
					bsResultMu.Lock()
					bsResult = &r
					bsResultMu.Unlock()
					close(bsDone)
					return r
				}
				attach = append(attach, img)
			}
		}

		// BootstrapTUI builds the ConversationManager + first conversation.
		if resumeID != "" {
			tui.SendLoadingProgress("resuming session…")
		} else {
			tui.SendLoadingProgress("creating session…")
		}
		rt, cleanup, err := wiring.BootstrapTUI(cfg, exe, resumeID, nil, wiring.BootstrapTUIOpts{
			AttachImages:        attach,
			RestoreRepoState:    true,
			CounselMode:         counselMode,
			CounselMax:          counselMax,
			ComposeStartupNotes: true,
		})
		if err != nil {
			exe.Close()
			r := bootstrapDoneMsg{err: fmt.Errorf("bootstrap: %w", err)}
			bsResultMu.Lock()
			bsResult = &r
			bsResultMu.Unlock()
			close(bsDone)
			return r
		}

		tui.SendLoadingProgress("setting up…")
		r := bootstrapDoneMsg{rt: rt, cleanup: cleanup, exe: exe}
		bsResultMu.Lock()
		bsResult = &r
		bsResultMu.Unlock()
		close(bsDone)
		return r
	}

	// swapFn is called when the bootstrap Cmd completes. It installs the
	// real TUI model, wires up event delivery, and sets up cleanup.
	// On error it returns nil + tea.Quit so prog.Run() unwinds normally
	// (terminal is restored by Bubble Tea) before main() prints the error.
	var progRef *tea.Program // set before prog.Run
	swapFn := func(msg tea.Msg) (tea.Model, tea.Cmd) {
		done := msg.(bootstrapDoneMsg)

		// Restore os.Stderr so post-bootstrap output is visible.
		restoreStderr()

		if done.err != nil {
			// Don't os.Exit — let tea.Quit unwind so the terminal is restored.
			// The error is stored in bsResult for main() to print after
			// prog.Run() returns.
			swapHandled = true
			_ = os.Remove(stderrFile.Name())
			if done.exe != nil {
				done.exe.Close()
			}
			return nil, tea.Quit
		}

		// Prime the OpenRouter model-context cache in the background.
		if panelsUseOpenRouter(cfg) {
			go func() {
				defer func() {
					if r := recover(); r != nil {
						diag.Printf("cache priming panic (non-fatal): %v\n", r)
					}
				}()
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				_, _ = counsel.FetchModelContextLimits(ctx)
			}()
		}

		// Build the real TUI model.
		model := tui.NewTUIModelWithFacade(done.rt.Facade, done.rt.Manager, done.rt.Principal)

		// Wire up event delivery. progRef is set before prog.Run() so this
		// is never nil. SubscribeLive registers the deliver callback; the
		// pump is started immediately after.
		if err := done.rt.SubscribeLive(context.Background(), func(ev event.Event) {
			progRef.Send(ev)
		}); err != nil {
			// Subscription failed — return nil + Quit so the terminal is
			// restored before main() prints the error.
			swapHandled = true
			done.cleanup()
			done.exe.Close()
			_ = os.Remove(stderrFile.Name())
			return nil, tea.Quit
		}
		done.rt.StartEventPump(context.Background())

		// Compose cleanup: runtime cleanup + session log teardown.
		// Both run after prog.Run() returns, in the right order
		// (runtime first, then diag redirect restore + log close).
		var sessionLogClose func()
		if snap := done.rt.Facade.Snapshot(); snap.ChatID != "" {
			if f := diag.OpenSessionLog(wiring.ShortID(snap.ChatID)); f != nil {
				sessionLogClose = func() {
					diag.Redirect(nil)
					f.Close()
				}
			}
		}

		// Copy early diagnostics (from the stderr temp file) into the
		// session log before removing the temp file.
		if data, err := os.ReadFile(stderrFile.Name()); err == nil && len(data) > 0 {
			_, _ = diag.Write(data)
		}
		_ = os.Remove(stderrFile.Name())

		// Store composed cleanup for after prog.Run() returns.
		postRunCleanup = func() {
			done.cleanup()
			if sessionLogClose != nil {
				sessionLogClose()
			}
		}
		postRunExeClose = done.exe.Close

		return model, model.Init()
	}

	loadingStatus := "starting container…"
	if resumeID != "" {
		loadingStatus = "resuming session…"
	}
	loading := tui.NewLoadingModel(loadingStatus, bootstrapCmd, swapFn)
	prog := tea.NewProgram(loading,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	progRef = prog
	tui.SetProgramSend(prog.Send)

	if _, err := prog.Run(); err != nil {
		restoreStderr()
		fmt.Fprintln(os.Stderr, "tui error:", err)
		if postRunCleanup != nil {
			postRunCleanup()
		}
		if postRunExeClose != nil {
			_ = postRunExeClose()
		}
		_ = os.Remove(stderrFile.Name())
		os.Exit(1)
	}

	// prog.Run() returned. If the swap function ran, postRunCleanup is set.
	// If the user quit during bootstrap, wait briefly for the bootstrap
	// goroutine to finish so we can clean up any resources it created.
	if postRunCleanup == nil {
		// Wait up to 3s for the bootstrap goroutine to complete.
		select {
		case <-bsDone:
		case <-time.After(3 * time.Second):
			// Bootstrap is still running (e.g., Docker is slow). We can't
			// cancel it (NewExecutor takes no context), so warn and exit.
			// The container may be left running.
		}
		bsResultMu.Lock()
		r := bsResult
		bsResultMu.Unlock()
		restoreStderr()
		if swapHandled {
			// swapFn already cleaned up — just print the error if any.
			if r != nil && r.err != nil {
				fmt.Fprintln(os.Stderr, "startup error:", r.err)
				if data, _ := os.ReadFile(stderrFile.Name()); len(data) > 0 {
					fmt.Fprintln(os.Stderr, "startup diagnostics:")
					_, _ = os.Stderr.Write(data)
				}
				_ = os.Remove(stderrFile.Name())
			}
		} else if r != nil {
			if r.err != nil {
				fmt.Fprintln(os.Stderr, "startup aborted:", r.err)
			} else {
				// Bootstrap completed but the user already quit — clean up.
				if r.cleanup != nil {
					r.cleanup()
				}
				if r.exe != nil {
					r.exe.Close()
				}
			}
		} else {
			fmt.Fprintln(os.Stderr, "startup aborted — a container may still be starting (Docker cannot be cancelled)")
		}
		_ = os.Remove(stderrFile.Name())
		os.Exit(1)
	}

	// Normal teardown: close the facade (host session, pump, detached jobs)
	// then the executor-owned resources.
	if postRunCleanup != nil {
		postRunCleanup()
	}
	if postRunExeClose != nil {
		_ = postRunExeClose()
	}

	// End-of-session cost summary (printed to stderr after terminal is restored).
	// The facade's Snapshot gives us the CostTracker, which is still valid
	// after Close (it's a heap pointer; Close doesn't nil it).
	if bsResult != nil && bsResult.rt != nil {
		snap := bsResult.rt.Facade.Snapshot()
		if snap.Costs != nil {
			total, rows := snap.Costs.Snapshot()
			var inTok, outTok int64
			for _, r := range rows {
				inTok += r.InputTok
				outTok += r.OutputTok
			}
			if total > 0 || inTok > 0 || outTok > 0 {
				fmt.Fprintf(os.Stderr, "session cost: %s | tokens: %dk in, %dk out\n",
					proxy.FmtUSDCompact(total), inTok/1000, outTok/1000)
			}
		}
	}
}

// panelsUseOpenRouter reports whether any configured mashura panel routes
// at least one model through OpenRouter ("openrouter:..." prefix or "~..." fusion
// syntax). Used to decide whether priming the OpenRouter model-context cache
// is worthwhile at startup.
func panelsUseOpenRouter(cfg config.Config) bool {
	for _, panel := range cfg.MashuraPanels {
		if panel.Mode == "fusion" {
			return true
		}
		for _, m := range panel.Models {
			if strings.HasPrefix(m, "openrouter:") || strings.HasPrefix(m, "~") {
				return true
			}
		}
	}
	return false
}

// stripFlag removes every occurrence of --name / -name (a boolean flag) from
// args and reports whether it was present.
func stripFlag(args []string, name string) ([]string, bool) {
	var out []string
	found := false
	for _, a := range args {
		if a == "--"+name || a == "-"+name {
			found = true
			continue
		}
		out = append(out, a)
	}
	return out, found
}
