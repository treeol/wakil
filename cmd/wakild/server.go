package main

// server.go: server setup for wakild (card #148 P2d).
//
// Opens the SQLite event store (fail-closed unless --ephemeral), builds the
// session host with a TurnFunc, registers the Connect service handlers, and
// serves HTTP over a Unix socket with 0600 permissions.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionhost"
	"github.com/treeol/wakil/internal/core/sessionhost/sqlstore"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/server/connect"
	"github.com/treeol/wakil/internal/wiring"
)

// daemonServer bundles the long-lived resources the daemon owns: the event
// store, the session host, the Connect server, the HTTP server, and the
// listener. The caller drives lifecycle via serve/shutdown.
type daemonServer struct {
	store     *sqlstore.SQLiteStore
	host      *sessionhost.Host
	srv       *connect.Server
	httpSrv   *http.Server
	listener  net.Listener
	ephemeral bool
	exe       exec.Executor
	app       *agent.App
	appRes    *wiring.AppResources
}

// newDaemonServer constructs the daemon's server-side resources:
//  1. Open the SQLite event store (fail-closed unless ephemeral)
//  2. Build the executor + a TurnFunc
//  3. Create the session host with the store
//  4. Build the Connect server
//  5. Listen on the Unix socket
func newDaemonServer(cfg config.Config, socketPath string, ephemeral bool, workspaceID event.WorkspaceID) (*daemonServer, error) {
	// 1. Store initialization (fail-closed unless --ephemeral).
	var store *sqlstore.SQLiteStore
	if !ephemeral {
		dbPath := agent.SessionHostDBPath(string(workspaceID))
		if dbPath == "" {
			return nil, fmt.Errorf("wakild: cannot derive session-host DB path for workspace %q (no data directory?)", workspaceID)
		}
		s, err := sqlstore.NewSQLiteStore(context.Background(), dbPath)
		if err != nil {
			return nil, fmt.Errorf("wakild: failed to open SQLite store (fail-closed): %w", err)
		}
		store = s
	}

	// 2. Executor + App + TurnFunc.
	// P2d: one App drives one session (HostTurnFunc's single-App binding).
	// The daemon serves one active session at a time; the TUI (--daemon) creates
	// a new session on connect. A per-session factory (multiple Apps/hosts) is
	// the P2e concern. WithAsyncApproval enables the async wire approval path:
	// the TUI calls RespondToApproval over RPC; the confirmer parks on
	// ParkApproval instead of an inline resolver.
	exe, err := wiring.NewExecutor(cfg)
	if err != nil {
		if store != nil {
			store.Close()
		}
		return nil, fmt.Errorf("wakild: executor: %w", err)
	}

	app, res := wiring.BuildApp(cfg, exe, wiring.BuildAppOpts{
		IsHeadless: true, // no TUI callbacks; the daemon is headless
	})
	handle, err := wiring.NewHostTurnHandle(app, wiring.WithAsyncApproval())
	if err != nil {
		wiring.CloseResources(app, res)
		exe.Close()
		if store != nil {
			store.Close()
		}
		return nil, fmt.Errorf("wakild: host turn handle: %w", err)
	}

	// 3. Host with store.
	hostOpts := []sessionhost.Option{
		sessionhost.WithAgentName("wakild"),
	}
	if store != nil {
		hostOpts = append(hostOpts, sessionhost.WithStore(store))
	}
	host := sessionhost.New(handle.Turn, hostOpts...)

	// 4. Connect server.
	srv := connect.NewServer(host, ephemeral)

	// 5. Unix socket listener.
	listener, err := listenUnix(socketPath)
	if err != nil {
		host.Close(context.Background())
		wiring.CloseResources(app, res)
		exe.Close()
		if store != nil {
			store.Close()
		}
		return nil, fmt.Errorf("wakild: listen: %w", err)
	}

	httpSrv := &http.Server{
		Handler: srv.Handler(),
	}

	return &daemonServer{
		store:     store,
		host:      host,
		srv:       srv,
		httpSrv:   httpSrv,
		listener:  listener,
		ephemeral: ephemeral,
		exe:       exe,
		app:       app,
		appRes:    res,
	}, nil
}

// serve starts the HTTP server on the Unix socket. Blocks until the server
// stops (via Shutdown or an error). Returns the serve error.
func (d *daemonServer) serve() error {
	return d.httpSrv.Serve(d.listener)
}

// shutdown performs a graceful drain:
//  1. Stop accepting new connections (http.Server.Shutdown)
//  2. Drain running turns up to the timeout (host.Close)
//  3. Close the store, executor, and App resources
func (d *daemonServer) shutdown(ctx context.Context) error {
	// Stop accepting new HTTP connections. In-flight RPC handlers continue
	// until they finish or ctx is cancelled.
	if d.httpSrv != nil {
		if err := d.httpSrv.Shutdown(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "wakild: http shutdown: %v\n", err)
		}
	}

	// Drain the host: close all sessions, wait for running turns to finish.
	// Pending approvals are auto-declined with SystemUserID (existing Host
	// behavior on ctx cancellation).
	if d.host != nil {
		if err := d.host.Close(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "wakild: host close: %v\n", err)
		}
	}

	// Stop async ops and close App resources (memory, skills, MCP, LSP, etc.).
	if d.app != nil {
		d.app.StopAllAsyncOps()
		d.app.StopAllBackgroundProcs()
		wiring.CloseResources(d.app, d.appRes)
	}

	// Close the store.
	if d.store != nil {
		if err := d.store.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "wakild: store close: %v\n", err)
		}
	}

	// Close the executor.
	if d.exe != nil {
		d.exe.Close()
	}

	return nil
}

// listenUnix creates a Unix socket listener at path with 0600 permissions.
// If a stale socket exists and no process is listening on it, it is unlinked
// and rebound. If a process IS listening, the function returns an error
// (refuses to steal the socket).
func listenUnix(path string) (net.Listener, error) {
	// Ensure the parent directory exists with 0700 permissions.
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create socket directory: %w", err)
		}
	}

	// Check for a stale socket: if the file exists, try connecting to see if
	// another process is listening. If the connection succeeds, another daemon
	// is running — refuse to steal it. If the connection fails, the socket is
	// stale — unlink and rebind.
	if _, err := os.Stat(path); err == nil {
		conn, dErr := net.Dial("unix", path)
		if dErr == nil {
			conn.Close()
			return nil, fmt.Errorf("socket %s is already in use by another process", path)
		}
		// Stale socket: unlink it.
		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("unlink stale socket %s: %w", path, err)
		}
	}

	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}

	// 0600: owner-only — security boundary until P4 adds SO_PEERCRED.
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
		os.Remove(path)
		return nil, fmt.Errorf("chmod socket %s: %w", path, err)
	}

	return l, nil
}

// socketPath returns the default Unix socket path:
// $XDG_RUNTIME_DIR/wakild.sock, or $HOME/.local/share/wakil/wakild.sock.
func defaultSocketPath() string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return filepath.Join(xdg, "wakild.sock")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".local", "share", "wakil", "wakild.sock")
	}
	return "wakild.sock"
}
