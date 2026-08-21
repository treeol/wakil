package main

// daemon_mode.go: the --daemon entry point (card #148 P2e).
//
// When the user runs `wakil --daemon`, the TUI dials the wakild daemon over
// its Unix socket and drives the session remotely instead of embedding the
// agent loop. This file wires the remote bootstrap into the TUI, mirroring
// main.go's embedded bootstrap path but using the remote package.

import (
	"context"
	"fmt"
	"os"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/diag"
	"github.com/treeol/wakil/internal/remote"
	"github.com/treeol/wakil/internal/tui"

	tea "github.com/charmbracelet/bubbletea"
)

// RunDaemonMode dials the daemon and runs the TUI in remote mode.
func RunDaemonMode(cfg config.Config) int {
	socketPath := cfg.DaemonSocket
	if socketPath == "" {
		socketPath = remote.DefaultSocketPath()
	}

	// Derive the workspace ID from the config's work dir (the same logic
	// the daemon uses).
	ws := event.WorkspaceID(cfg.WorkDir)
	if ws == "" {
		cwd, _ := os.Getwd()
		ws = event.WorkspaceID(cwd)
	}

	ctx := context.Background()
	rt, cleanup, err := remote.BootstrapRemote(ctx, socketPath, ws, "", nil)
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

	// Redirect raw diagnostics to a session log file (mirrors main.go).
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
