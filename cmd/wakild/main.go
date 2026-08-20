package main

// cmd/wakild/main.go: the wakild daemon entry point (card #148 P2d).
//
// wakild runs the Connect server over a Unix socket. It opens a fail-closed
// SQLite event store (or --ephemeral for in-memory), builds the session host,
// and serves RPCs until SIGTERM/SIGINT. The TUI connects via --daemon.
//
// Usage:
//
//	wakild [--socket <path>] [--ephemeral] [--shutdown-timeout <dur>]
//
// The daemon reads the same wakil.yaml config as the TUI for backend/model
// credentials. It derives the workspace ID from the config's working directory.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core/event"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "wakild:", err)
		os.Exit(1)
	}
}

// daemonFlags holds the parsed command-line flags.
type daemonFlags struct {
	socketPath      string
	ephemeral       bool
	shutdownTimeout time.Duration
}

func parseFlags(args []string) (daemonFlags, error) {
	f := daemonFlags{
		socketPath:      defaultSocketPath(),
		shutdownTimeout: 10 * time.Second,
	}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--socket", "-socket":
			i++
			if i >= len(args) {
				return f, fmt.Errorf("--socket requires a path")
			}
			f.socketPath = args[i]
		case "--ephemeral", "-ephemeral":
			f.ephemeral = true
		case "--shutdown-timeout", "-shutdown-timeout":
			i++
			if i >= len(args) {
				return f, fmt.Errorf("--shutdown-timeout requires a duration")
			}
			d, err := time.ParseDuration(args[i])
			if err != nil {
				return f, fmt.Errorf("--shutdown-timeout: %w", err)
			}
			f.shutdownTimeout = d
		case "--help", "-help", "-h":
			fmt.Fprint(os.Stderr, usage)
			os.Exit(0)
		default:
			if len(args[i]) > 0 && args[i][0] == '-' {
				return f, fmt.Errorf("unknown flag: %s", args[i])
			}
			return f, fmt.Errorf("unexpected argument: %s (wakild takes flags only)", args[i])
		}
	}
	return f, nil
}

const usage = `wakild — wakil daemon (card #148 P2d)

Usage:
  wakild [flags]

Flags:
  --socket <path>           Unix socket path (default: $XDG_RUNTIME_DIR/wakild.sock)
  --ephemeral               Use in-memory store (no durability; GetServerInfo.ephemeral=true)
  --shutdown-timeout <dur>   Graceful drain deadline (default: 10s)
  --help                    Show this help

The daemon reads wakil.yaml for backend/model credentials. The workspace ID
is derived from the config's working directory (same as the TUI).
`

func run() error {
	flags, err := parseFlags(os.Args[1:])
	if err != nil {
		return err
	}

	// Load the same config the TUI reads (wakil.yaml).
	cfg, err := config.LoadConfig(nil)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	// Derive the workspace ID (same derivation as wiring.workspaceIDFromConfig).
	wsID := workspaceIDFromConfig(cfg)

	ds, err := newDaemonServer(cfg, flags.socketPath, flags.ephemeral, wsID)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "wakild: listening on %s (ephemeral=%v, workspace=%s)\n",
		flags.socketPath, flags.ephemeral, wsID)

	// Serve until signal.
	ctx := waitForSignal(context.Background())
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- ds.serve()
	}()

	select {
	case err := <-serveErr:
		// Serve exited (error or clean stop).
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintf(os.Stderr, "wakild: serve error: %v\n", err)
		}
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "wakild: shutting down...")
	}

	// Graceful shutdown with deadline.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), flags.shutdownTimeout)
	defer cancel()
	ds.shutdown(shutdownCtx)

	// Remove the socket file if it still exists.
	if err := os.Remove(flags.socketPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "wakild: remove socket: %v\n", err)
	}

	fmt.Fprintln(os.Stderr, "wakild: stopped")
	return nil
}

// workspaceIDFromConfig mirrors wiring.workspaceIDFromConfig: "wsp_" + the
// first 16 hex chars of the SHA-256 of the effective workdir.
func workspaceIDFromConfig(cfg config.Config) event.WorkspaceID {
	ws := cfg.WorkDir
	if cfg.ExecMode != "direct" {
		ws = cfg.HostWorkDir
	}
	sum := sha256.Sum256([]byte(ws))
	return event.WorkspaceID("wsp_" + hex.EncodeToString(sum[:8]))
}
