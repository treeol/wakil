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
	"github.com/treeol/wakil/internal/remote"
	"github.com/treeol/wakil/internal/tui"
	"github.com/treeol/wakil/internal/wiring"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	// "wakil run" subcommand: headless, non-interactive, exits with a code.
	if len(os.Args) >= 2 && os.Args[1] == "run" {
		cfg, err := config.LoadConfig(nil) // flags after "run" are for RunHeadless, not LoadConfig
		if err != nil {
			fmt.Fprintln(os.Stderr, "config error:", err)
			os.Exit(ExitError)
		}
		os.Exit(RunHeadless(cfg, os.Args[2:]))
	}

	// "wakil daemon" subcommand: run the daemon server (card #149). Listens on
	// a Unix socket, serves Connect RPCs, and optionally a web UI on TCP.
	// Previously a separate `wakild` binary; merged into `wakil` for simplicity.
	if len(os.Args) >= 2 && os.Args[1] == "daemon" {
		os.Exit(runDaemon())
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

	cfg, err := config.LoadConfig(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "config error:", err)
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "ctx limits: compactAt=%d hardMax=%d keep=%d summary=%d\n",
		cfg.CompactAt, cfg.HardMaxBytes, cfg.KeepBytes, cfg.SummaryBytes)

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

	// Daemon mode: remote TUI dials the daemon — no local executor needed.
	// Checking BEFORE NewExecutor avoids paying full Docker container startup
	// and then leaking the container via os.Exit (runDaemonMode takes no exe).
	if cfg.DaemonMode {
		if cfg.AttachImage != "" {
			fmt.Fprintln(os.Stderr, "warning: --attach-image is not supported in daemon mode — ignored")
		}
		os.Exit(runDaemonMode(cfg, resumeID))
	}

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
}

// runDaemonMode dials the wakil daemon and runs the TUI in remote mode
// (card #148 P2e). When the user runs `wakil --daemon`, the TUI dials the
// daemon over its Unix socket and drives the session remotely instead of
// embedding the agent loop. This mirrors main.go's embedded bootstrap path
// but uses the remote package.
func runDaemonMode(cfg config.Config, resumeID string) int {
	socketPath := cfg.DaemonSocket
	if socketPath == "" {
		socketPath = remote.DefaultSocketPath()
	}

	// Derive the workspace ID from the config (same derivation as the daemon).
	ws := wiring.WorkspaceIDFromConfig(cfg)

	ctx := context.Background()
	rt, cleanup, err := remote.BootstrapRemote(ctx, socketPath, ws, resumeID, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "daemon error:", err)
		return ExitError
	}
	defer cleanup()

	model := tui.NewTUIModelWithFacade(rt.Facade, rt.Manager, rt.Principal)
	prog := tea.NewProgram(model,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	tui.SetProgramSend(prog.Send)
	if err := rt.SubscribeLive(ctx, func(ev event.Event) {
		prog.Send(ev)
	}); err != nil {
		fmt.Fprintln(os.Stderr, "subscribe error:", err)
		return ExitError
	}
	rt.StartEventPump(ctx)

	// Redirect raw diagnostics to a session log file (mirrors the embedded path).
	if snap := rt.Facade.Snapshot(); snap.ChatID != "" {
		if f := diag.OpenSessionLog(snap.ChatID); f != nil {
			defer f.Close()
			defer diag.Redirect(nil)
		}
	}

	if _, err := prog.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "tui error:", err)
		return ExitError
	}
	return ExitOK
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
