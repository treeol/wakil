package main

import (
	"context"
	"fmt"
	"os"
	"strings"
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
	// "wakil run" subcommand: headless, non-interactive, exits with a code.
	if len(os.Args) >= 2 && os.Args[1] == "run" {
		cfg, err := config.LoadConfig(nil) // flags after "run" are for RunHeadless, not LoadConfig
		if err != nil {
			fmt.Fprintln(os.Stderr, "config error:", err)
			os.Exit(ExitError)
		}
		os.Exit(RunHeadless(cfg, os.Args[2:]))
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

	exe, err := wiring.NewExecutor(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "executor error:", err)
		os.Exit(1)
	}

	if cfg.DaemonMode {
		os.Exit(RunDaemonMode(cfg))
	}

	// --attach-image: load into pending images for the first message.
	attach := []proxy.ImagePart{}
	if cfg.AttachImage != "" {
		for _, p := range strings.Split(cfg.AttachImage, ",") {
			p = strings.TrimSpace(p)
			if p == "" {
				continue
			}
			img, err := proxy.LoadImage(p)
			if err != nil {
				fmt.Fprintln(os.Stderr, "attach-image:", err)
				exe.Close()
				os.Exit(1)
			}
			attach = append(attach, img)
		}
	}

	// Prime the OpenRouter model-context cache in the background when any
	// mashura panel routes through OpenRouter. ResolveContextLength never
	// fetches on its own (oracle calls must not block on cold-cache network
	// I/O), so without this warm-up OpenRouter models silently get the
	// conservative fallback context length. Best-effort; errors are ignored.
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

	counselMode := cfg.AutoCounsel
	if counselMode == "" {
		counselMode = "suggest"
	}
	counselMax := cfg.CounselMaxPerSession
	if counselMode == "auto" && counselMax == 0 {
		counselMax = 3
	}

	// m4c: the TUI runs through the session host. BootstrapTUI builds the
	// ConversationManager + first conversation (fresh or resumed), runs the
	// TUI-specific startup steps (repo-state restore, counsel, attach-images,
	// startup notes), and subscribes the event stream. Event delivery is
	// prog.Send once the program exists (the pump is armed, not started).
	rt, cleanup, err := wiring.BootstrapTUI(cfg, exe, resumeID, nil, wiring.BootstrapTUIOpts{
		AttachImages:        attach,
		RestoreRepoState:    true,
		CounselMode:         counselMode,
		CounselMax:          counselMax,
		ComposeStartupNotes: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "bootstrap error:", err)
		exe.Close()
		os.Exit(1)
	}

	model := tui.NewTUIModelWithFacade(rt.Facade, rt.Manager, rt.Principal)
	prog := tea.NewProgram(model,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	tui.SetProgramSend(prog.Send)
	// Subscribe the facade's event stream now that prog.Send exists, then
	// start delivery. BootstrapTUI could not subscribe at construction (prog
	// did not exist yet), so the deliver callback is bound here. Without this
	// the facade has no subscription → no pump → turns run server-side but
	// their events never reach the TUI (the UI stays stuck on "streaming").
	if err := rt.SubscribeLive(context.Background(), func(ev event.Event) {
		prog.Send(ev)
	}); err != nil {
		fmt.Fprintln(os.Stderr, "subscribe error:", err)
		cleanup()
		exe.Close()
		os.Exit(1)
	}
	rt.StartEventPump(context.Background())

	// Redirect raw diagnostics to a session log file BEFORE prog.Run() so a
	// diagnostic written while the alt-screen is active can never interleave
	// with Bubble Tea's renderer and garble the terminal. Headless mode
	// (wakil run) never reaches this point and keeps stderr. The path is
	// surfaced to stderr now (the alt-screen isn't up yet) so the user can
	// find diagnostics later.
	if snap := rt.Facade.Snapshot(); snap.ChatID != "" {
		if f := diag.OpenSessionLog(wiring.ShortID(snap.ChatID)); f != nil {
			// Register f.Close() FIRST so it runs LAST (defers are LIFO): the
			// sink is restored to stderr before the file closes, so late
			// cleanup writes never target a closed file.
			defer f.Close()
			defer diag.Redirect(nil)
		} else if p := diag.LogPath(wiring.ShortID(snap.ChatID)); p != "" {
			fmt.Fprintf(os.Stderr, "diagnostics: cannot open log at %s — session diagnostics stay on stderr\n", p)
		}
	}

	if _, err := prog.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "tui error:", err)
		cleanup()
		exe.Close()
		os.Exit(1)
	}

	// Teardown: close the facade (host session, pump, detached jobs) then
	// the executor-owned resources.
	cleanup()
	exe.Close()
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
