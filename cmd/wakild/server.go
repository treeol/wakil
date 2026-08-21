package main

// server.go: server setup for wakild (card #148 P2d).
//
// Opens the SQLite event store (fail-closed unless --ephemeral), builds the
// session host with a TurnFunc, registers the Connect service handlers, and
// serves HTTP over a Unix socket with 0600 permissions.

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/auth"
	"github.com/treeol/wakil/internal/auth/peercred"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionhost"
	"github.com/treeol/wakil/internal/core/sessionhost/sqlstore"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/server/connect"
	"github.com/treeol/wakil/internal/wiring"
	"github.com/treeol/wakil/web"
)

// daemonServer bundles the long-lived resources the daemon owns: the event
// store, the session host, the Connect server, the HTTP server(s), and the
// listener(s). The caller drives lifecycle via serve/shutdown.
type daemonServer struct {
	store     *sqlstore.SQLiteStore
	host      *sessionhost.Host
	srv       *connect.Server
	httpSrv   *http.Server // Unix-socket server (Connect RPC)
	httpAddr  string       // TCP address for web UI ("" = disabled)
	httpLnr   net.Listener // TCP listener for web UI
	tcpSrv    *http.Server // TCP server (static files only — no Connect RPC, P4b)
	listener  net.Listener // Unix-socket listener
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
//  6. Optionally listen on a TCP address for the web UI
func newDaemonServer(cfg config.Config, socketPath string, ephemeral bool, workspaceID event.WorkspaceID, httpAddr string) (*daemonServer, error) {
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

	// 4. Connect server with the local principal resolver (P4b).
	// The resolver maps Unix-socket peer UIDs (SO_PEERCRED) to the local
	// owner principal. It is fail-closed: connections without valid peer
	// credentials are rejected with CodeUnauthenticated.
	resolver := auth.NewLocalResolver()
	srv := connect.NewServer(host, ephemeral, resolver)

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
		// ConnContext captures peer credentials (SO_PEERCRED) at
		// connection-accept time and stores them in the context that every
		// request on that connection inherits. The principal resolver reads
		// them per-request to resolve the caller's identity (P4b).
		ConnContext: func(ctx context.Context, conn net.Conn) context.Context {
			creds, ok, err := peercred.FromConn(conn)
			if err != nil {
				// Log extraction failures — fail-closed but visible.
				// A persistent failure on a Unix socket indicates a
				// platform or configuration problem.
				fmt.Fprintf(os.Stderr, "wakild: peercred extraction failed: %v\n", err)
				return ctx
			}
			if ok {
				return auth.WithPeerCredentials(ctx, creds)
			}
			// No credentials available: leave the context empty. The
			// resolver will return ErrUnauthenticated — fail closed.
			return ctx
		},
	}

	ds := &daemonServer{
		store:     store,
		host:      host,
		srv:       srv,
		httpSrv:   httpSrv,
		listener:  listener,
		ephemeral: ephemeral,
		exe:       exe,
		app:       app,
		appRes:    res,
		httpAddr:  httpAddr,
	}

	// 6. Optional TCP listener for web UI.
	// P4b: the TCP server serves ONLY static files. Connect RPC handlers
	// are NOT mounted on TCP — that would expose the API without
	// authentication (SO_PEERCRED is Unix-only). Hosted auth (P4c) will
	// add token/OIDC-based auth for TCP; until then, the web UI can view
	// static assets but cannot call the API over TCP. The browser UI
	// will be fully functional again once P4c adds hosted auth or a
	// Unix-socket-to-TCP proxy is introduced. This is an accepted
	// trade-off for P4b: closing the authentication bypass takes priority.
	if httpAddr != "" {
		tcpLnr, err := net.Listen("tcp", httpAddr)
		if err != nil {
			host.Close(context.Background())
			wiring.CloseResources(app, res)
			exe.Close()
			if store != nil {
				store.Close()
			}
			return nil, fmt.Errorf("wakild: listen tcp: %w", err)
		}
		ds.httpLnr = tcpLnr
		ds.tcpSrv = &http.Server{Handler: webStaticHandler()}
	}

	return ds, nil
}

// serve starts the HTTP servers. Blocks until both servers stop.
// The Unix-socket server error is returned; the TCP server error is logged
// but does not stop the daemon (the Unix socket is the critical path).
func (d *daemonServer) serve() error {
	errCh := make(chan error, 2)

	go func() {
		errCh <- d.httpSrv.Serve(d.listener)
	}()

	if d.tcpSrv != nil && d.httpLnr != nil {
		go func() {
			err := d.tcpSrv.Serve(d.httpLnr)
			if err != nil {
				fmt.Fprintf(os.Stderr, "wakild: tcp server stopped: %v\n", err)
			}
			// Don't send TCP errors to errCh — only the Unix-socket
			// server error stops the daemon.
		}()
	}

	// Return the first error from the Unix-socket server only.
	return <-errCh
}

// webStaticHandler builds the HTTP handler for the web UI: static files only.
// P4b: Connect RPC handlers are NOT mounted on TCP — the TCP listener has no
// peer-credential authentication. The web UI loads static assets (HTML, CSS,
// JS) from TCP and connects to the API over the Unix socket (or a future
// hosted-auth path in P4c).
func webStaticHandler() http.Handler {
	staticFS, _ := fs.Sub(web.StaticFiles, ".")
	return http.FileServer(http.FS(staticFS))
}

// shutdown performs a graceful drain:
//  1. Stop accepting new connections (both Unix + TCP)
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
	if d.tcpSrv != nil {
		if err := d.tcpSrv.Shutdown(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "wakild: tcp shutdown: %v\n", err)
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
//
// P4b: 0600 is the first security layer (owner-only connect). SO_PEERCRED
// (via ConnContext + LocalResolver) is the second layer: it verifies the
// connecting process's UID matches the daemon owner even if the socket
// permissions are misconfigured.
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

	// 0600: owner-only — first security layer. SO_PEERCRED (ConnContext +
	// LocalResolver) is the second layer, defense-in-depth against misconfigured
	// permissions.
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
