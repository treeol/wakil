package lsp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
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

	mu        sync.Mutex
	state     ServerState
	conn      *rpcConn
	stderr    io.ReadCloser
	stderrBuf []byte // last 4 KB of stderr for crash diagnostics
	pid       int
	caps      ServerCapabilities
	encoding  PositionEncodingKind // negotiated, stored per-instance; nil→utf-16 handled at access

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
	case "rust":
		return config.LSPServer{Command: "rust-analyzer"}
	case "python":
		return config.LSPServer{Command: "pyright-langserver", Args: []string{"--stdio"}}
	case "typescript", "javascript":
		return config.LSPServer{Command: "typescript-language-server", Args: []string{"--stdio"}}
	case "c", "cpp":
		return config.LSPServer{Command: "clangd"}
	}
	return config.LSPServer{Command: lang + "-language-server"}
}

// knownDefaultLanguages lists language IDs that have a built-in default server
// command in defaultLSPServer. It is a subset of detectLanguage's output —
// java is detected by extension but has no reliable default binary.
var knownDefaultLanguages = map[string]bool{
	"go": true, "rust": true, "python": true,
	"typescript": true, "javascript": true,
	"c": true, "cpp": true,
}

// allowedLSPToolLanguage reports whether lang is safe to pass to EnsureServer.
// It accepts built-in default languages and any key the user explicitly
// configured in lsp_servers. This prevents command injection via the language
// tool argument (which flows into defaultLSPServer → unquoted s.cmd in spawn).
func (m *Manager) allowedLSPToolLanguage(lang string) bool {
	if knownDefaultLanguages[lang] {
		return true
	}
	if _, ok := m.cfg.LSPServers[lang]; ok {
		return true
	}
	return false
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
	// Prepend configured env vars (WP-7.6: env was never applied — a dead config field).
	cmdStr := "exec"
	for k, v := range s.env {
		cmdStr += " " + k + "=" + shellQuote(v)
	}
	cmdStr += " " + s.cmd
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

	// Probe for the binary before launching it, so a missing server reads as
	// "not installed" rather than "crashed on startup". The probe runs through
	// the same executor, i.e. inside the sandbox session when there is one.
	// It runs under s.mu: spawn is the only writer of a fresh Server, and a
	// concurrent caller blocks here instead of double-launching.
	if err := s.probeBinary(ctx); err != nil {
		// Dead, so EnsureServer discards it and the next call probes again:
		// installing the binary and retrying works without a restart.
		s.state = StateDead
		s.deadErr = err
		close(s.deadCh)
		s.mu.Unlock()
		return err
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

// probeBinary checks that the server's command exists in the executor's
// environment (the sandbox session when there is one). It returns a
// user-facing error naming the missing binary and, when the backend reports
// one, the sandbox rootfs to fix. A missing binary is reported as such, not
// as a startup crash.
func (s *Server) probeBinary(ctx context.Context) error {
	if _, err := s.mgr.exec.RunShell(ctx, "command -v "+shellQuote(s.cmd)); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return &missingBinaryError{cmd: s.cmd, where: s.mgr.sandboxLabel()}
	}
	return nil
}

// missingBinaryError is returned when a language server binary is absent from
// the sandbox. Its text is the contract the LLM sees via failureContract.
type missingBinaryError struct {
	cmd   string
	where string
}

func (e *missingBinaryError) Error() string {
	return fmt.Sprintf("%s not found in the sandbox (%s); install it in the image", e.cmd, e.where)
}

// sandboxLabel names the execution environment for error messages. When the
// executor reports a sandbox rootfs, it is included, so the fix is obvious.
func (m *Manager) sandboxLabel() string {
	if r, ok := m.exec.(interface{ SandboxRootfs() string }); ok {
		if name := r.SandboxRootfs(); name != "" {
			return "rootfs " + name
		}
	}
	return "host toolchain"
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

// errIdleTimeout is a sentinel used to skip the graceful shutdown RPC
// and stderr enrichment for idle-reaping (the server is healthy, just idle).
var errIdleTimeout = errors.New("idle timeout")

// markDead transitions the server to Dead and fans out to all waiters.
func (s *Server) markDead(err error) {
	s.mu.Lock()
	if s.state == StateDead {
		s.mu.Unlock()
		return
	}
	s.state = StateDead
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
	pid := s.pid
	gen := s.generation
	stderr := s.stderr
	s.stderr = nil
	s.mu.Unlock()

	// Shutdown the connection (best-effort graceful, then stdin close).
	// Skip the 5s graceful shutdown RPC for idle-reaping — just kill.
	if conn != nil {
		if errors.Is(err, errIdleTimeout) {
			// Idle reaping: skip the RPC, just close the connection.
			conn.Close()
		} else {
			// Best-effort: send shutdown, then exit, then close.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = conn.call(shutdownCtx, "shutdown", nil)
			cancel()
			_ = conn.notify("exit", nil)
			conn.Close()
		}
	}

	// Close stderr to unblock the drainStderr goroutine (if still running).
	// This prevents the goroutine from leaking if the process hangs with the
	// stderr pipe open. Best-effort — Close may already have been called.
	if stderr != nil {
		_ = stderr.Close()
	}

	// Snapshot stderr for crash diagnostics (best-effort). Clone the slice
	// under lock to avoid aliasing — drainStderr may still be appending.
	isIdle := errors.Is(err, errIdleTimeout)
	var stderrTail []byte
	if !isIdle {
		s.mu.Lock()
		stderrTail = append([]byte(nil), s.stderrBuf...)
		s.mu.Unlock()
	}

	// Build the final error (enriched with stderr if available) before
	// publishing via deadCh so all waiters see the same error.
	finalErr := err
	if !isIdle && len(stderrTail) > 0 {
		finalErr = fmt.Errorf("%w\n--- LSP stderr (last %d bytes) ---\n%s", err, len(stderrTail), stderrTail)
	}

	s.mu.Lock()
	s.deadErr = finalErr
	s.mu.Unlock()
	// Fan out: close deadCh (unblocks waitForReady waiters).
	// All fields are final before this point so waiters see the enriched error.
	close(s.deadCh)

	// Process kill guard: if the LSP server process (pid > 0) is still alive
	// 3 seconds after stdin close, kill it via the executor. This prevents a
	// hung server from leaking as a zombie process. The PID is the host-side
	// process (docker exec or direct) — KillPgid targets the process group.
	// Uses a bounded context so the IsProcessAlive/KillPgid calls don't hang
	// indefinitely if the executor is unresponsive.
	if pid > 0 && s.mgr.exec != nil {
		go func(pid int, gen int) {
			time.Sleep(3 * time.Second)
			// Skip if the server was respawned (new generation) — the PID
			// may belong to a different process.
			s.mu.Lock()
			current := s.generation
			s.mu.Unlock()
			if current != gen {
				return
			}
			killCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if s.mgr.exec.IsProcessAlive(killCtx, pid) {
				_ = s.mgr.exec.KillPgid(killCtx, pid, 9) // SIGKILL
			}
		}(pid, gen)
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
		_ = json.Unmarshal(params, &p) //nolint:errcheck // token registered on Begin; nothing to store here
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
		s.markDead(errIdleTimeout)
	})
}

// drainStderr reads stderr and keeps a 4 KB ring buffer for crash diagnostics.
func (s *Server) drainStderr() {
	s.mu.Lock()
	stderr := s.stderr
	s.mu.Unlock()
	if stderr == nil {
		return
	}
	buf := make([]byte, 4096)
	for {
		n, err := stderr.Read(buf)
		if n > 0 {
			// Append to the ring buffer, dropping oldest data if needed.
			s.mu.Lock()
			s.stderrBuf = append(s.stderrBuf, buf[:n]...)
			if len(s.stderrBuf) > 4096 {
				s.stderrBuf = s.stderrBuf[len(s.stderrBuf)-4096:]
			}
			s.mu.Unlock()
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

// capabilityBool checks if an `any` capability field is truthy.
// LSP capability fields can be: bool, options object, or nil/absent.
// A decoded `false` must NOT count as supported (Mashūra #20 fix).
func capabilityBool(v any) bool {
	if v == nil {
		return false
	}
	if b, ok := v.(bool); ok {
		return b
	}
	// Options object (e.g. DocumentSymbolOptions) means supported.
	return true
}

// CapabilitySupported checks if the server advertises a given capability.
func (s *Server) CapabilitySupported(capName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch capName {
	case "definitionProvider":
		return capabilityBool(s.caps.DefinitionProvider)
	case "referencesProvider":
		return capabilityBool(s.caps.ReferencesProvider)
	case "hoverProvider":
		return capabilityBool(s.caps.HoverProvider)
	case "documentSymbolProvider":
		return capabilityBool(s.caps.DocumentSymbolProvider)
	case "workspaceSymbolProvider":
		return capabilityBool(s.caps.WorkspaceSymbolProvider)
	case "renameProvider":
		return capabilityBool(s.caps.RenameProvider)
	}
	return false
}

// DocumentSymbol returns the hierarchical symbols for a single file. This is
// the primitive for building a symbol map: call it per file, collect the
// results, and rank/prioritize for the outline.
//
// The file is opened (didOpen) only if not already tracked by the server,
// ensuring current content without redundant opens. Returns hierarchical
// DocumentSymbol[] (preferred) or flat SymbolInformation[] (legacy servers).
//
// This is a typed API for internal use (symbol map builder), NOT a tool handler.
// Tool handlers use HandleLSPReadOnly which returns rendered strings.
func (m *Manager) DocumentSymbol(ctx context.Context, lang, hostPath string) ([]DocumentSymbol, []SymbolInformation, error) {
	srv, err := m.EnsureServer(ctx, lang)
	if err != nil {
		return nil, nil, fmt.Errorf("ensure server for %q: %w", lang, err)
	}

	if !srv.CapabilitySupported("documentSymbolProvider") {
		return nil, nil, fmt.Errorf("server %q does not support documentSymbol", lang)
	}

	// Translate host path to container URI.
	uri, err := m.exec.HostPathToURI(hostPath)
	if err != nil {
		return nil, nil, fmt.Errorf("URI translation for %q: %w", hostPath, err)
	}

	// Open the file only if not already tracked. gopls serves workspace files
	// without didOpen, but some servers need it for reliable results.
	srv.mu.Lock()
	alreadyOpen := srv.docs[uri] > 0
	srv.mu.Unlock()
	if !alreadyOpen {
		content, err := m.exec.ReadFile(ctx, hostPath)
		if err != nil {
			return nil, nil, fmt.Errorf("read file %q: %w", hostPath, err)
		}
		if err := srv.DidOpen(ctx, uri, lang, content); err != nil {
			// Don't abort — gopls serves workspace files even without didOpen.
			_ = err
		}
	}

	params := DocumentSymbolParams{
		TextDocument: TextDocumentIdentifier{URI: uri},
	}
	raw, err := srv.Call(ctx, "textDocument/documentSymbol", params)
	if err != nil {
		return nil, nil, fmt.Errorf("documentSymbol call for %q: %w", hostPath, err)
	}

	docSyms, symInfos, err := DecodeDocumentSymbol(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("decode documentSymbol for %q: %w", hostPath, err)
	}

	return docSyms, symInfos, nil
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

// shellQuote single-quotes a string for shell safety, escaping embedded
// single quotes using the standard '\” pattern (matching exec.shQuote).
//
// Trust boundary: LSP server args originate from the user's local config
// file (~/.config/wakil/config.json), NOT from workspace content. The
// config file is user-controlled and workspace-inaccessible. However, the
// escaping is applied defensively regardless — a misconfigured args string
// with an embedded single quote must not break shell parsing.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
