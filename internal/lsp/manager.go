package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/exec"
)

// ServerState is the lifecycle state of one language server.
type ServerState int

const (
	StateUninitialized ServerState = iota
	StateSpawning
	StateInitializing // initialize sent, awaiting response
	StateIndexing     // initialized sent, awaiting $/progress end
	StateReady        // indexing complete, serving queries
	StateDraining     // idle shutdown started; no new writes
	StateDead         // terminal; must re-spawn
)

func (s ServerState) String() string {
	switch s {
	case StateUninitialized:
		return "uninitialized"
	case StateSpawning:
		return "spawning"
	case StateInitializing:
		return "initializing"
	case StateIndexing:
		return "indexing"
	case StateReady:
		return "ready"
	case StateDraining:
		return "draining"
	case StateDead:
		return "dead"
	}
	return "?"
}

// Manager owns language server processes for the session. One warm instance
// per (workspace, language). Lazy-spawned on first query for a language.
//
// Concurrency invariants (L1 + panel fold):
//   - readLoop hands notifications to a single drain goroutine (not inline)
//     to avoid head-of-line blocking on response delivery.
//   - Manager.mu is NEVER held across rpcConn.call() — this prevents the
//     lock-inversion deadlock where readLoop's notifyHandler tries to take
//     Manager.mu while a lock-holder is blocked in call().
//   - Death fans out to: rpcConn.pending (via readLoop EOF), the readiness
//     gate (closed chan struct{}), queued requests (released with error).
//   - didOpen/didChange flow during Indexing (not gated behind Ready) — gopls
//     needs document opens to drive indexing.
type Manager struct {
	exec    exec.Executor
	cfg     config.Config
	rootURI string // workspace root as a file:// URI (container-visible path)

	mu      sync.Mutex
	servers map[string]*Server // keyed by language ID ("go", "rust", ...)
}

// Server is one language server connection.
type Server struct {
	lang     string
	mgr      *Manager
	cmd      string
	args     []string
	env      map[string]string
	initOpts map[string]interface{}

	mu       sync.Mutex
	state    ServerState
	conn     *rpcConn
	stderr   io.ReadCloser
	pid      int
	caps     ServerCapabilities
	encoding PositionEncodingKind // negotiated, stored per-instance; nil→utf-16 handled at access

	// Readiness gate: closed when Ready, re-created on each spawn.
	// Waiters select on readyCh and deadCh.
	readyCh chan struct{}
	deadCh  chan struct{} // closed on death; never re-opened (re-spawn creates a new Server)
	deadErr error

	// $/progress tracking: refcount of open Begin-without-End tokens.
	// Ready when refcount reaches zero (or on timeout, or on first successful query).
	progressTokens map[any]bool // token → active
	progressTimer  *time.Timer  // no-progress watchdog

	// Open documents (for didOpen/didChange tracking)
	docs      map[string]int32 // URI → version counter
	idleTimer *time.Timer      // idle shutdown

	// File-sync manager (lazy-initialized)
	fs *fileSyncManager

	// Generation counter for re-arm: stale End from gen N must not release gen N+1.
	generation int
}

// NewManager creates a Manager. exec is used to spawn servers; rootURI is the
// container-visible workspace root (file:// URI).
func NewManager(ex exec.Executor, cfg config.Config, rootURI string) *Manager {
	return &Manager{
		exec:    ex,
		cfg:     cfg,
		rootURI: rootURI,
		servers: make(map[string]*Server),
	}
}

// EnsureServer returns the Server for the given language, lazy-spawning if needed.
// Returns an error if the server cannot be spawned (binary not found, initialize failed).
func (m *Manager) EnsureServer(ctx context.Context, lang string) (*Server, error) {
	m.mu.Lock()
	srv, ok := m.servers[lang]
	// If the server is Dead or Draining, discard it and create a fresh one.
	// This is the State Machine Discard pattern: a fresh struct gives fresh
	// channels (deadCh, readyCh), clears deadErr, and bounds generation scope.
	if ok {
		srv.mu.Lock()
		state := srv.state
		srv.mu.Unlock()
		if state == StateDead || state == StateDraining {
			delete(m.servers, lang)
			srv = m.newServerLocked(lang)
			m.servers[lang] = srv
		}
	} else {
		srv = m.newServerLocked(lang)
		m.servers[lang] = srv
	}
	m.mu.Unlock()

	if err := srv.spawn(ctx); err != nil {
		return nil, err
	}
	return srv, nil
}

func (m *Manager) newServerLocked(lang string) *Server {
	srvCfg, ok := m.cfg.LSPServers[lang]
	if !ok {
		srvCfg = defaultLSPServer(lang)
	}
	s := &Server{
		lang:           lang,
		mgr:            m,
		cmd:            srvCfg.Command,
		args:           srvCfg.Args,
		env:            srvCfg.Env,
		initOpts:       srvCfg.InitOptions,
		readyCh:        make(chan struct{}),
		deadCh:         make(chan struct{}),
		progressTokens: make(map[any]bool),
		docs:           make(map[string]int32),
	}
	return s
}

// defaultLSPServer returns the default config for a language.
func defaultLSPServer(lang string) config.LSPServer {
	switch lang {
	case "go":
		return config.LSPServer{Command: "gopls", Args: []string{"serve"}}
	}
	return config.LSPServer{Command: lang + "-language-server"}
}

// spawn starts the server process, performs the initialize handshake, and
// transitions to Indexing (or Ready if no progress arrives within a short window).
// Called by EnsureServer on a fresh Server (the State Machine Discard pattern).
func (s *Server) spawn(ctx context.Context) error {
	s.mu.Lock()
	if s.state == StateReady || s.state == StateIndexing || s.state == StateInitializing {
		s.mu.Unlock()
		return nil // already running
	}

	// Build the command. Use 'exec' so sh doesn't linger as a separate parent.
	cmdStr := "exec " + s.cmd
	for _, a := range s.args {
		cmdStr += " " + shellQuote(a)
	}

	// Spawn via the executor.
	if s.mgr.exec == nil {
		s.state = StateDead
		s.deadErr = fmt.Errorf("no executor configured")
		close(s.deadCh)
		s.mu.Unlock()
		return s.deadErr
	}
	stdin, stdout, stderr, pid, err := s.mgr.exec.StartInteractive(ctx, cmdStr)
	if err != nil {
		s.state = StateDead
		s.deadErr = fmt.Errorf("spawn failed: %w", err)
		close(s.deadCh)
		s.mu.Unlock()
		return s.deadErr
	}

	s.conn = newRPCConn(stdin, stdout, s.handleNotification)
	s.stderr = stderr
	s.pid = pid
	s.state = StateInitializing
	s.generation++
	s.encoding = UTF16 // default until negotiated; nil from server → utf-16

	// Capture the generation for stale-callback protection.
	gen := s.generation
	s.mu.Unlock()

	// Start stderr drain (captures crash traces for error classification).
	go s.drainStderr()

	// Perform the initialize handshake.
	if err := s.initialize(ctx); err != nil {
		s.markDead(fmt.Errorf("initialize failed: %w", err))
		return err
	}

	// Send initialized notification.
	if err := s.conn.notify("initialized", map[string]interface{}{}); err != nil {
		s.markDead(fmt.Errorf("initialized notification failed: %w", err))
		return err
	}

	// Transition to Indexing. Start the no-progress watchdog.
	s.mu.Lock()
	s.state = StateIndexing
	s.startProgressWatchdogLocked(gen)
	s.mu.Unlock()

	// Start the idle timer (L3: 30 min).
	s.resetIdleTimer()

	return nil
}

// initialize sends the initialize request and reads the result, storing
// capabilities and the negotiated position encoding.
func (s *Server) initialize(ctx context.Context) error {
	rootURI := s.mgr.rootURI
	pid := int32(0) // we don't have our own PID meaningfully across docker
	params := InitializeParams{
		ProcessID:  &pid,
		ClientInfo: &ClientInfo{Name: "wakil"},
		RootURI:    &rootURI,
		Capabilities: ClientCapabilities{
			General: &GeneralClientCapabilities{
				PositionEncodings: []PositionEncodingKind{UTF8, UTF16},
			},
			Workspace: &WorkspaceClientCapabilities{
				WorkspaceEdit: &WorkspaceEditClientCapabilities{
					DocumentChanges:    true,
					ResourceOperations: []string{"create", "rename", "delete"},
				},
			},
			TextDocument: &TextDocumentClientCapabilities{
				Synchronization: &TextDocumentSyncClientCapabilities{DidSave: true},
				Hover:           &HoverClientCapabilities{ContentFormat: []string{"markdown", "plaintext"}},
				DocumentSymbol:  &DocumentSymbolClientCapabilities{HierarchicalDocumentSymbolSupport: true},
			},
			Window: &WindowClientCapabilities{WorkDoneProgress: true},
		},
		InitializationOptions: s.initOpts,
	}

	result, err := s.conn.call(ctx, "initialize", params)
	if err != nil {
		return err
	}

	var initResult InitializeResult
	if err := json.Unmarshal(result, &initResult); err != nil {
		return fmt.Errorf("unmarshaling initialize result: %w", err)
	}

	s.mu.Lock()
	s.caps = initResult.Capabilities
	// Negotiate encoding: store from result, default nil → utf-16.
	if initResult.Capabilities.PositionEncoding != nil {
		s.encoding = *initResult.Capabilities.PositionEncoding
	} else {
		s.encoding = UTF16
	}
	s.mu.Unlock()

	return nil
}

// Encoding returns the negotiated position encoding for this server.
// Defaults to UTF-16 if the server omitted the field.
func (s *Server) Encoding() PositionEncodingKind {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.encoding
}

// waitForReady blocks until the server is Ready, the context is done, or the
// server dies. Returns nil on Ready, an error otherwise.
func (s *Server) waitForReady(ctx context.Context) error {
	s.mu.Lock()
	readyCh := s.readyCh
	deadCh := s.deadCh
	// Fast path: already ready.
	select {
	case <-readyCh:
		s.mu.Unlock()
		return nil
	default:
	}
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-deadCh:
		s.mu.Lock()
		err := s.deadErr
		s.mu.Unlock()
		if err == nil {
			err = fmt.Errorf("server died")
		}
		return err
	case <-readyCh:
		return nil
	}
}

// markDead transitions the server to Dead and fans out to all waiters.
func (s *Server) markDead(err error) {
	s.mu.Lock()
	if s.state == StateDead {
		s.mu.Unlock()
		return
	}
	s.state = StateDead
	s.deadErr = err
	// Fan out: close deadCh (unblocks waitForReady waiters).
	// readyCh is left open-on-dead; waitForReady selects on deadCh first.
	close(s.deadCh)
	// Cancel the progress watchdog.
	if s.progressTimer != nil {
		s.progressTimer.Stop()
		s.progressTimer = nil
	}
	if s.idleTimer != nil {
		s.idleTimer.Stop()
		s.idleTimer = nil
	}
	conn := s.conn
	s.mu.Unlock()

	// Shutdown the connection (best-effort graceful, then stdin close).
	if conn != nil {
		// Best-effort: send shutdown, then exit, then close.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, _ = conn.call(shutdownCtx, "shutdown", nil)
		cancel()
		_ = conn.notify("exit", nil)
		conn.Close()
	}
}

// handleNotification is called by rpcConn's readLoop — BUT via a single drain
// goroutine (see newRPCConn's notifyHandler). It processes server→client
// notifications and requests.
func (s *Server) handleNotification(method string, params json.RawMessage, isRequest bool) (any, error) {
	switch method {
	case "$/progress":
		s.handleProgress(params)
		return nil, nil
	case "window/workDoneProgress/create":
		// Server is creating a progress token. Accept it (result: null).
		// Register the token so we expect Begin/End for it.
		var p WorkDoneProgressCreateParams
		json.Unmarshal(params, &p)
		_ = p // token registered on Begin; nothing to store here
		return nil, nil
	case "window/showMessage":
		// Log and ignore for now.
		return nil, nil
	case "window/logMessage":
		return nil, nil
	case "workspace/configuration":
		// Return empty config; gopls uses this for settings.
		return map[string]interface{}{}, nil
	case "client/registerCapability":
		// Accept silently.
		return nil, nil
	}
	return nil, nil
}

// handleProgress processes a $/progress notification.
// Uses a refcount of open Begin-without-End tokens: when it reaches zero,
// the server is considered Ready (indexing complete).
func (s *Server) handleProgress(params json.RawMessage) {
	var p ProgressParams
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}

	// The Value is a WorkDoneProgressBegin/Report/End — discriminate by "kind".
	raw, _ := json.Marshal(p.Value)
	var v struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Already ready? Ignore stale progress.
	select {
	case <-s.readyCh:
		return
	default:
	}

	// Capture generation for the progress-watchdog reset.
	gen := s.generation

	switch v.Kind {
	case "begin":
		s.progressTokens[p.Token] = true
		// Reset the no-progress watchdog (we got activity).
		s.startProgressWatchdogLocked(gen)
	case "end":
		delete(s.progressTokens, p.Token)
		// If no more open tokens, we're ready.
		if len(s.progressTokens) == 0 {
			s.transitionToReadyLocked()
		}
	case "report":
		// Activity — reset the watchdog.
		s.startProgressWatchdogLocked(gen)
	}
}

// startProgressWatchdogLocked starts/resets the no-progress timeout.
// If no End arrives within the configured timeout, we declare Ready anyway
// (gopls may not send progress, or indexing may be instant).
// The gen parameter prevents a stale timer from generation N from firing
// after generation N+1 has started.
func (s *Server) startProgressWatchdogLocked(gen int) {
	timeout := time.Duration(s.mgr.cfg.LSPIndexTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if s.progressTimer != nil {
		s.progressTimer.Stop()
	}
	s.progressTimer = time.AfterFunc(timeout, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		// Stale-generation guard: if the server re-spawned (gen mismatch), skip.
		if s.generation != gen {
			return
		}
		// If still indexing, declare ready (timeout path — no silent empty).
		if s.state == StateIndexing {
			s.transitionToReadyLocked()
		}
	})
}

// transitionToReadyLocked moves to Ready and releases all queued waiters.
// Caller must hold s.mu.
func (s *Server) transitionToReadyLocked() {
	if s.state == StateReady {
		return
	}
	s.state = StateReady
	close(s.readyCh)
	if s.progressTimer != nil {
		s.progressTimer.Stop()
		s.progressTimer = nil
	}
}

// resetIdleTimer (re)starts the idle shutdown timer (L3: 30 min).
func (s *Server) resetIdleTimer() {
	timeout := time.Duration(s.mgr.cfg.LSPIdleTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Minute
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.idleTimer != nil {
		s.idleTimer.Stop()
	}
	s.idleTimer = time.AfterFunc(timeout, func() {
		s.mu.Lock()
		if s.state != StateReady && s.state != StateIndexing {
			s.mu.Unlock()
			return
		}
		s.state = StateDraining
		s.mu.Unlock()
		// Graceful shutdown.
		s.markDead(fmt.Errorf("idle timeout"))
	})
}

// drainStderr reads stderr (crash traces for error classification).
func (s *Server) drainStderr() {
	if s.stderr == nil {
		return
	}
	buf := make([]byte, 4096)
	for {
		n, err := s.stderr.Read(buf)
		if n > 0 {
			// Could log or store for crash classification; for now, discard.
			_ = buf[:n]
		}
		if err != nil {
			return
		}
	}
}

// DidOpen notifies the server of a file's content. Flows during Indexing (not gated).
func (s *Server) DidOpen(ctx context.Context, uri, languageID, content string) error {
	s.mu.Lock()
	version := s.docs[uri]
	if version == 0 {
		version = 1
	} else {
		version++
	}
	s.docs[uri] = version
	conn := s.conn
	state := s.state
	s.mu.Unlock()

	if conn == nil || state == StateDead || state == StateDraining {
		return fmt.Errorf("server not running (state: %s)", state)
	}

	params := DidOpenTextDocumentParams{
		TextDocument: TextDocumentItem{
			URI:        uri,
			LanguageID: languageID,
			Version:    version,
			Text:       content,
		},
	}
	return conn.notify("textDocument/didOpen", params)
}

// DidChange notifies the server of a full-content change. Flows during Indexing.
func (s *Server) DidChange(ctx context.Context, uri, content string) error {
	s.mu.Lock()
	version := s.docs[uri] + 1
	s.docs[uri] = version
	conn := s.conn
	state := s.state
	s.mu.Unlock()

	if conn == nil || state == StateDead || state == StateDraining {
		return fmt.Errorf("server not running (state: %s)", state)
	}

	params := DidChangeTextDocumentParams{
		TextDocument: VersionedTextDocumentIdentifier{
			Version:                version,
			TextDocumentIdentifier: TextDocumentIdentifier{URI: uri},
		},
		ContentChanges: []TextDocumentContentChangeEvent{{Text: content}},
	}
	return conn.notify("textDocument/didChange", params)
}

// Call sends a feature request (definition, hover, references, etc.) and waits
// for the response. Blocks until Ready (or error). Manager.mu is NOT held
// during the call (prevents lock-inversion deadlock).
func (s *Server) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	// Wait for readiness (notifications flow during indexing; requests queue).
	if err := s.waitForReady(ctx); err != nil {
		return nil, err
	}

	// Reset the idle timer on activity.
	s.resetIdleTimer()

	// Snapshot the conn under lock (prevents data race with spawn/markDead).
	s.mu.Lock()
	conn := s.conn
	state := s.state
	s.mu.Unlock()

	if conn == nil || state == StateDead || state == StateDraining {
		return nil, fmt.Errorf("server not running (state: %s)", state)
	}

	// Make the call WITHOUT holding s.mu (critical: prevents lock-inversion).
	return conn.call(ctx, method, params)
}

// CapabilitySupported checks if the server advertises a given capability.
func (s *Server) CapabilitySupported(capName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch capName {
	case "definitionProvider":
		return s.caps.DefinitionProvider != nil
	case "referencesProvider":
		return s.caps.ReferencesProvider != nil
	case "hoverProvider":
		return s.caps.HoverProvider != nil
	case "documentSymbolProvider":
		return s.caps.DocumentSymbolProvider != nil
	case "workspaceSymbolProvider":
		return s.caps.WorkspaceSymbolProvider != nil
	case "renameProvider":
		return s.caps.RenameProvider != nil
	}
	return false
}

// Shutdown gracefully stops all servers.
func (m *Manager) Shutdown() {
	m.mu.Lock()
	servers := make([]*Server, 0, len(m.servers))
	for _, s := range m.servers {
		servers = append(servers, s)
	}
	m.mu.Unlock()

	for _, s := range servers {
		s.markDead(fmt.Errorf("manager shutdown"))
	}
}

// shellQuote single-quotes a string for shell safety.
func shellQuote(s string) string {
	return "'" + s + "'"
}
