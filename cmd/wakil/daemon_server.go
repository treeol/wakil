package main

// server.go: server setup for wakild (card #148 P2d).
//
// Opens the SQLite event store (fail-closed unless --ephemeral), builds the
// session host with a TurnFunc, registers the Connect service handlers, and
// serves HTTP over a Unix socket with 0600 permissions.

import (
	"context"
	"crypto/tls"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/auth"
	"github.com/treeol/wakil/internal/auth/apitoken"
	"github.com/treeol/wakil/internal/auth/jointoken"
	"github.com/treeol/wakil/internal/auth/oidcresolver"
	"github.com/treeol/wakil/internal/auth/peercred"
	"github.com/treeol/wakil/internal/auth/tokenresolver"
	"github.com/treeol/wakil/internal/auth/tokenstore"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionhost"
	"github.com/treeol/wakil/internal/core/sessionhost/sqlstore"
	"github.com/treeol/wakil/internal/crypto"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/remote"
	"github.com/treeol/wakil/internal/scrub"
	connsvc "github.com/treeol/wakil/internal/server/connect"
	"github.com/treeol/wakil/internal/store/agentstore"
	"github.com/treeol/wakil/internal/store/backendstore"
	"github.com/treeol/wakil/internal/store/workspacestore"
	"github.com/treeol/wakil/internal/wiring"
	"github.com/treeol/wakil/web"
)

// daemonServer bundles the long-lived resources the daemon owns: the event
// store, the session host, the Connect server, the HTTP server(s), and the
// listener(s). The caller drives lifecycle via serve/shutdown.
type daemonServer struct {
	store      *sqlstore.SQLiteStore
	tokenStore *tokenstore.Store
	host       *sessionhost.Host
	srv        *connsvc.Server
	httpSrv    *http.Server // Unix-socket server (Connect RPC)
	httpAddr   string       // TCP address for web UI ("" = disabled)
	httpLnr    net.Listener // TCP listener for web UI
	tlsEnabled bool         // true when the TCP listener wraps TLS
	tcpSrv     *http.Server // TCP server (static files + auth RPC, P4c)
	listener   net.Listener // Unix-socket listener
	ephemeral  bool
	exe        exec.Executor
	app        *agent.App
	appRes     *wiring.AppResources
}

// newDaemonServer constructs the daemon's server-side resources:
//  1. Open the SQLite event store (fail-closed unless ephemeral)
//  2. Build the executor + a TurnFunc
//  3. Create the session host with the store
//  4. Build the Connect server
//  5. Listen on the Unix socket
//  6. Optionally listen on a TCP address for the web UI
func newDaemonServer(cfg config.Config, socketPath string, ephemeral bool, workspaceID event.WorkspaceID, httpAddr string, tlsCertFile string, tlsKeyFile string, allowedOrigins string, masterKeyFile string, scrubLevel string) (*daemonServer, error) {
	// 1. Store initialization (fail-closed unless --ephemeral).
	var store *sqlstore.SQLiteStore
	if !ephemeral {
		dbPath := agent.SessionHostDBPath(string(workspaceID))
		if dbPath == "" {
			return nil, fmt.Errorf("wakil daemon: cannot derive session-host DB path for workspace %q (no data directory?)", workspaceID)
		}
		s, err := sqlstore.NewSQLiteStore(context.Background(), dbPath)
		if err != nil {
			return nil, fmt.Errorf("wakil daemon: failed to open SQLite store (fail-closed): %w", err)
		}

		// P4g: apply secret scrubbing to event payloads before persistence.
		level, err := scrub.ParseLevel(scrubLevel)
		if err != nil {
			s.Close()
			return nil, fmt.Errorf("wakil daemon: %w", err)
		}
		if level != scrub.LevelOff {
			s.WithScrubber(scrub.New(level))
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
		return nil, fmt.Errorf("wakil daemon: executor: %w", err)
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
		return nil, fmt.Errorf("wakil daemon: host turn handle: %w", err)
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
	//
	// P4c: if a token store is available (non-ephemeral mode), create an
	// auth-enabled server with join token + web session support. The auth
	// handler is mounted on both Unix and TCP listeners. The TCP listener
	// gets a transport-aware resolver (web session cookie resolver) so the
	// browser can call the API with cookie auth.
	resolver := auth.NewLocalResolver()

	var tokenStore *tokenstore.Store
	var issuer *jointoken.Issuer
	var apiIssuer *apitoken.Issuer
	var srv *connsvc.Server

	// P4g: load master key for envelope encryption (optional).
	var masterKey *crypto.MasterKey
	if masterKeyFile != "" {
		mk, err := crypto.LoadMasterKeyFromFile("v1", masterKeyFile)
		if err != nil {
			host.Close(context.Background())
			wiring.CloseResources(app, res)
			exe.Close()
			if store != nil {
				store.Close()
			}
			return nil, fmt.Errorf("wakil daemon: load master key: %w", err)
		}
		masterKey = mk
	}

	if store != nil {
		// Create the token store (shares the same DB as the session store).
		tokenStore = tokenstore.New(store.DB())
		issuer = jointoken.New(tokenStore)
		apiIssuer = apitoken.New(tokenStore)

		// P4g: create backend handler if master key is available.
		var backendHandler *connsvc.BackendHandler
		if masterKey != nil {
			backendStore := backendstore.New(store.DB())
			backendHandler = connsvc.NewBackendHandler(backendStore, masterKey, resolver)
		}

		// P5b: create workspace handler.
		workspaceStore := workspacestore.New(store.DB())
		workspaceHandler := connsvc.NewWorkspaceHandler(workspaceStore, resolver)

		// P5c: create agent handler.
		agentStore := agentstore.New(store.DB())
		agentHandler := connsvc.NewAgentHandler(agentStore, resolver)

		srv = connsvc.NewServerWithAuthSecureCookiesBackendsWorkspacesAndAgents(host, ephemeral, resolver, issuer, apiIssuer, tokenStore, false, backendHandler, workspaceHandler, agentHandler)
		// P6a: attach the SessionStateHandler (the daemon owns the *agent.App).
		// Card #149: wire ResetSessionBinding so the single App can serve
		// multiple sequential sessions across TUI reconnections.
		ssh := connsvc.NewSessionStateHandler(app, resolver)
		ssh.SetResetSessionBinding(handle.ResetSessionBinding)
		srv = srv.WithSessionState(ssh)
	} else {
		srv = connsvc.NewServer(host, ephemeral, resolver)
	}

	// 5. Unix socket listener.
	listener, err := listenUnix(socketPath)
	if err != nil {
		host.Close(context.Background())
		wiring.CloseResources(app, res)
		exe.Close()
		if store != nil {
			store.Close()
		}
		return nil, fmt.Errorf("wakil daemon: listen: %w", err)
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
				fmt.Fprintf(os.Stderr, "wakil daemon: peercred extraction failed: %v\n", err)
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
		store:      store,
		tokenStore: tokenStore,
		host:       host,
		srv:        srv,
		httpSrv:    httpSrv,
		listener:   listener,
		ephemeral:  ephemeral,
		exe:        exe,
		app:        app,
		appRes:     res,
		httpAddr:   httpAddr,
	}

	// 6. Optional TCP listener for web UI + hosted auth (P4c).
	// P4b restricted TCP to static files only because there was no auth
	// mechanism for TCP. P4c adds web session cookie auth: the TCP server
	// now serves both static files AND Connect RPCs (AuthService, plus
	// Session/Event/System services) with the web session resolver.
	//
	// P4f: when --tls-cert and --tls-key are provided, the TCP listener is
	// wrapped with crypto/tls.NewListener. Session cookies get the Secure
	// flag (so browsers never send them over plaintext HTTP). The origin
	// validator is wired with the configured allowed origins (CSRF defense).
	//
	// The TCP server uses a web session cookie resolver (not the local
	// SO_PEERCRED resolver). ExchangeJoinToken is the only unauthenticated
	// RPC — all others require a valid session cookie.
	//
	// Static files are served at "/" and Connect RPCs at their service
	// paths ("/wakil.v1alpha1.*"). The mux routes by path prefix.
	if httpAddr != "" {
		tcpLnr, err := net.Listen("tcp", httpAddr)
		if err != nil {
			listener.Close() // close the already-open Unix listener
			host.Close(context.Background())
			wiring.CloseResources(app, res)
			exe.Close()
			if store != nil {
				store.Close()
			}
			return nil, fmt.Errorf("wakil daemon: listen tcp: %w", err)
		}

		// P4f: wrap with TLS if cert+key are configured.
		tlsEnabled := false
		if tlsCertFile != "" && tlsKeyFile != "" {
			cert, err := tls.LoadX509KeyPair(tlsCertFile, tlsKeyFile)
			if err != nil {
				tcpLnr.Close()
				listener.Close() // close the already-open Unix listener
				host.Close(context.Background())
				wiring.CloseResources(app, res)
				exe.Close()
				if store != nil {
					store.Close()
				}
				return nil, fmt.Errorf("wakil daemon: load TLS keypair: %w", err)
			}
			tlsConfig := &tls.Config{
				Certificates: []tls.Certificate{cert},
				MinVersion:   tls.VersionTLS12, // TLS 1.2+ required (no SSLv3/TLS1.0/1.1)
			}
			tcpLnr = tls.NewListener(tcpLnr, tlsConfig)
			tlsEnabled = true
		}
		ds.httpLnr = tcpLnr
		ds.tlsEnabled = tlsEnabled

		// Build the TCP handler: Connect RPCs with cookie auth + static files.
		var tcpHandler http.Handler
		if tokenStore != nil {
			// Create a web session resolver for TCP (cookie auth).
			webResolver := tokenresolver.New(tokenStore)
			// Create an API token resolver for TCP (Bearer header auth).
			apiResolver := tokenresolver.NewAPIResolver(tokenStore)
			// Create an OIDC resolver for TCP (Bearer JWT auth). When no
			// validator is configured, it returns ErrCredentialAbsent
			// (disabled) and the MultiResolver chain skips it.
			oidcResolver := oidcresolver.New(tokenStore, nil, oidcresolver.Config{})
			// The TCP multi-resolver tries web session first (browsers), then
			// API token (CLI/CI), then OIDC (external IdP). An invalid
			// credential in any resolver is a hard fail (no fallthrough).
			multiResolver := auth.NewMultiResolver(webResolver, apiResolver, oidcResolver)
			// Build a TCP-specific Connect server with the multi-resolver.
			// P4f: pass secureCookies=tlsEnabled so session cookies get the
			// Secure flag when TLS is active.
			// P4g: pass the backend handler to the TCP server too.
			// P5b: pass the workspace handler to the TCP server too.
			// P5c: pass the agent handler to the TCP server too.
			tcpSrv := connsvc.NewServerWithAuthSecureCookiesBackendsWorkspacesAndAgents(host, ephemeral, multiResolver, issuer, apiIssuer, tokenStore, tlsEnabled, srv.Backend(), srv.Workspace(), srv.Agent())
			// P6a: attach the SessionStateHandler to the TCP server too.
			// Card #149: wire ResetSessionBinding here too.
			tcpSsh := connsvc.NewSessionStateHandler(app, multiResolver)
			tcpSsh.SetResetSessionBinding(handle.ResetSessionBinding)
			tcpSrv = tcpSrv.WithSessionState(tcpSsh)
			tcpConnectHandler := tcpSrv.Handler()

			// Compose: Connect RPCs at service paths, static files at "/".
			// Go's ServeMux treats patterns not ending in "/" as exact matches,
			// so "/wakil.v1alpha1." would only match that exact path — not
			// "/wakil.v1alpha1.AuthService/Foo". Use a prefix-dispatching
			// handler instead.
			staticHandler := webStaticHandler()
			tcpHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasPrefix(r.URL.Path, "/wakil.v1alpha1.") {
					tcpConnectHandler.ServeHTTP(w, r)
					return
				}
				staticHandler.ServeHTTP(w, r)
			})
		} else {
			// Ephemeral mode: no auth, static files only.
			tcpHandler = webStaticHandler()
		}

		// P4f: wrap with origin validator for CSRF protection.
		// In production (hosted mode), --allowed-origins must be set.
		// Empty = dev mode (all origins allowed, SameSite=Strict still applies).
		originsSet := parseAllowedOrigins(allowedOrigins)
		tcpHandler = connsvc.OriginValidator(originsSet)(tcpHandler)

		ds.tcpSrv = &http.Server{
			Handler:           tcpHandler,
			ReadHeaderTimeout: 10 * time.Second, // P4f: mitigate slowloris on public listener
		}
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
				fmt.Fprintf(os.Stderr, "wakil daemon: tcp server stopped: %v\n", err)
			}
			// Don't send TCP errors to errCh — only the Unix-socket
			// server error stops the daemon.
		}()
	}

	// Return the first error from the Unix-socket server only.
	return <-errCh
}

// webStaticHandler builds the HTTP handler for the web UI: static files.
// P4c: when a token store is available, the TCP server also mounts Connect
// RPC handlers with web session cookie auth (see newDaemonServer). When no
// token store is available (ephemeral mode), TCP serves static files only.
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
			fmt.Fprintf(os.Stderr, "wakil daemon: http shutdown: %v\n", err)
		}
	}
	if d.tcpSrv != nil {
		if err := d.tcpSrv.Shutdown(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "wakil daemon: tcp shutdown: %v\n", err)
		}
	}

	// Drain the host: close all sessions, wait for running turns to finish.
	// Pending approvals are auto-declined with SystemUserID (existing Host
	// behavior on ctx cancellation).
	if d.host != nil {
		if err := d.host.Close(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "wakil daemon: host close: %v\n", err)
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
			fmt.Fprintf(os.Stderr, "wakil daemon: store close: %v\n", err)
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

// defaultSocketPath returns the default Unix socket path, mirroring
// remote.DefaultSocketPath() (card #149: deduplicated — was previously
// duplicated in the old cmd/wakild binary and internal/remote/dialer.go).
func defaultSocketPath() string {
	return remote.DefaultSocketPath()
}

// parseAllowedOrigins parses a comma-separated list of origin URLs into a set.
// Returns nil if the input is empty (dev mode — all origins allowed).
func parseAllowedOrigins(s string) map[string]bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	m := make(map[string]bool, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			m[p] = true
		}
	}
	if len(m) == 0 {
		return nil
	}
	return m
}
