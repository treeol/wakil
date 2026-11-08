package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/treeol/wakil/internal/browser"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/diag"
	"github.com/treeol/wakil/internal/lsp"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/safe"
	wtools "github.com/treeol/wakil/internal/tools"

	gosdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpNS is the separator between server name and tool name in the OpenAI tool
// name field: "{server}__{tool}". Double underscore is unlikely in real names.
const mcpNS = "__"

// mcpSession is the subset of *gosdkmcp.ClientSession that MCPManager uses.
// It exists so tests can inject fake sessions without spawning real MCP
// server processes. The concrete SDK type satisfies it (compile-time assert
// below); production behavior is unchanged.
type mcpSession interface {
	CallTool(ctx context.Context, params *gosdkmcp.CallToolParams) (*gosdkmcp.CallToolResult, error)
	Close() error
}

var _ mcpSession = (*gosdkmcp.ClientSession)(nil)

// MCPServer is one connected (or failed) MCP server.
type MCPServer struct {
	Cfg     config.MCPServerConfig
	Session mcpSession
	Tools   []*gosdkmcp.Tool // tools listed at connect time
	Status  string           // "connecting" | "connected" | "failed" | "closed"
	Err     error

	// ready is closed when the initial background connect for this server has
	// finished (success, failure, or shutdown). A tool call routed to a server
	// that is still "connecting" waits on its OWN ready channel, so a slow
	// server never blocks a call to an already-connected one. Always non-nil
	// (installed at construction). Closed exactly once via readyOnce.
	ready     chan struct{}
	readyOnce sync.Once

	// sessionMu serializes CallTool (RLock) against Reconnect/Close (Lock)
	// on the same session. CallTool holds RLock for the duration of
	// session.CallTool; Reconnect/Close hold Lock while closing the old
	// session, which drains in-flight calls before Close is called.
	sessionMu sync.RWMutex

	// reconnectMu serializes concurrent Reconnect calls for the same server,
	// preventing overlapping connect attempts that would leak sessions.
	reconnectMu sync.Mutex
}

// closeDrainTimeout bounds how long Close waits for in-flight CallTool calls
// on a session before abandoning it. Long enough for an ordinary RPC to
// finish, short enough that a wedged transport cannot hang teardown.
const closeDrainTimeout = 5 * time.Second

// drainAndClose waits for in-flight CallTool calls to release sessionMu,
// then closes the session. It reports whether the close actually happened;
// false means the drain timed out and the session was left untouched (the
// caller must not then force-close it).
func (s *MCPServer) drainAndClose(session mcpSession, timeout time.Duration) bool {
	acquired := make(chan struct{})
	go func() {
		s.sessionMu.Lock()
		close(acquired)
		s.sessionMu.Unlock()
	}()
	select {
	case <-acquired:
		_ = session.Close()
		return true
	case <-time.After(timeout):
		return false
	}
}

// MCPManager owns all MCP server connections for the lifetime of the App that
// built it. Each stdio server is a child process, so Close is mandatory for
// every owner: the daemon releases it via CloseResources, and the TUI's
// wiringFacade.Close releases it on close/rotation. Skipping Close orphans the
// child process.
type MCPManager struct {
	mu      sync.RWMutex
	servers []*MCPServer
	// connectFn performs the actual connect+ListTools for a server. Defaults
	// to m.connect; tests override it to inject fake sessions. Reconnect and
	// NewMCPManager both route through it. Nil-safe: falls back to m.connect
	// so hand-constructed managers (tests) don't nil-panic.
	connectFn func(ctx context.Context, srv *MCPServer) error
}

// connectOrDefault returns m.connectFn, defaulting to m.connect when unset.
func (m *MCPManager) connectOrDefault() func(ctx context.Context, srv *MCPServer) error {
	if m.connectFn != nil {
		return m.connectFn
	}
	return m.connect
}

// NewMCPManager connects to every configured MCP server. Failures are
// recorded per-server; they never crash wakil.
//
// Connections run in the BACKGROUND, concurrently, so startup and the first
// prompt are not blocked on a slow or unreachable server (a stdio spawn or an
// HTTP connect can take up to 30s each). Each server starts in "connecting";
// its ready channel closes when its connect finishes, and readers that need
// the server (CallTool, /mcp) wait on that channel only for the server they
// touch. Tools become visible to the model as each server connects
// (OpenAITools reads live Status/Tools each turn — see MCPManager.ToolsSnapshot).
//
// The caller must Close the manager to stop servers that are still connecting;
// Close marks them "closed" and a late successful connect is discarded.
func NewMCPManager(ctx context.Context, cfgs []config.MCPServerConfig) *MCPManager {
	m := &MCPManager{}
	m.connectFn = m.connect
	for _, cfg := range cfgs {
		srv := &MCPServer{Cfg: cfg, Status: "connecting", ready: make(chan struct{})}
		m.servers = append(m.servers, srv)
		safe.Go("mcp-connect", func() {
			// Connect into a TEMP server: connect() mutates the *MCPServer it
			// is given, and readers (OpenAITools, CallTool, Servers) run
			// concurrently once the manager is published. Publishing under
			// m.mu.Lock keeps every read under RLock race-free, exactly like
			// Reconnect's temp-server pattern.
			tmp := &MCPServer{Cfg: cfg, Status: "connecting"}
			err := m.connectOrDefault()(ctx, tmp)
			m.mu.Lock()
			defer m.mu.Unlock()
			defer srv.readyOnce.Do(func() { close(srv.ready) })
			if srv.Status == "closed" {
				// Close ran while we were connecting: discard the session.
				if tmp.Session != nil {
					_ = tmp.Session.Close()
				}
				return
			}
			if err != nil {
				srv.Status = "failed"
				srv.Err = err
				return
			}
			srv.Session = tmp.Session
			srv.Tools = tmp.Tools
			srv.Status = tmp.Status
			if srv.Status == "" {
				srv.Status = "connected"
			}
			srv.Err = nil
		})
	}
	return m
}

func (m *MCPManager) connect(ctx context.Context, srv *MCPServer) error {
	impl := &gosdkmcp.Implementation{Name: "wakil", Version: "v0.1"}
	client := gosdkmcp.NewClient(impl, nil)

	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	transport, err := buildTransport(srv.Cfg)
	if err != nil {
		return fmt.Errorf("build transport: %w", err)
	}

	session, err := client.Connect(tctx, transport, nil)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}

	res, err := session.ListTools(tctx, nil)
	if err != nil {
		session.Close()
		return fmt.Errorf("list tools: %w", err)
	}

	srv.Session = session
	srv.Tools = res.Tools
	srv.Status = "connected"
	return nil
}

func buildTransport(cfg config.MCPServerConfig) (gosdkmcp.Transport, error) {
	switch cfg.Transport {
	case "stdio":
		if cfg.Command == "" {
			return nil, fmt.Errorf("stdio transport requires command")
		}
		cmd := exec.Command(cfg.Command, cfg.Args...)
		base := os.Environ()
		for k, v := range cfg.Env {
			base = append(base, k+"="+v)
		}
		cmd.Env = base
		return &gosdkmcp.CommandTransport{Command: cmd}, nil
	case "http", "https":
		return &gosdkmcp.StreamableClientTransport{
			Endpoint:             cfg.URL,
			HTTPClient:           &http.Client{Timeout: 60 * time.Second},
			DisableStandaloneSSE: true,
		}, nil
	default:
		return nil, fmt.Errorf("unknown transport %q (want stdio|http)", cfg.Transport)
	}
}

// Servers returns a snapshot of all servers (safe to read, do not mutate).
func (m *MCPManager) Servers() []*MCPServer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*MCPServer, len(m.servers))
	copy(out, m.servers)
	return out
}

// MCPServerSnapshot is a race-free copy of one server's display state, taken
// under the manager lock. Callers that render server status (the /mcp listing,
// the info panel) must use SnapshotServers rather than reading fields off the
// *MCPServer returned by Servers — background connects mutate Status/Tools/Err
// under m.mu, so an unlocked field read is a data race.
type MCPServerSnapshot struct {
	Name   string
	Status string
	Err    error
	Tools  []*gosdkmcp.Tool
}

// SnapshotServers returns a race-free copy of every server's display state.
func (m *MCPManager) SnapshotServers() []MCPServerSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]MCPServerSnapshot, 0, len(m.servers))
	for _, s := range m.servers {
		out = append(out, MCPServerSnapshot{
			Name:   s.Cfg.Name,
			Status: s.Status,
			Err:    s.Err,
			Tools:  s.Tools,
		})
	}
	return out
}

// OpenAITools returns all MCP tools converted to OpenAI function-tool format,
// namespaced as "{server}__{tool}".
func (m *MCPManager) OpenAITools() []proxy.Tool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var tools []proxy.Tool
	for _, srv := range m.servers {
		if srv.Status != "connected" {
			continue
		}
		for _, t := range srv.Tools {
			tools = append(tools, MCPToolToOpenAI(srv.Cfg.Name, t))
		}
	}
	// Sort by name for cache-prefix stability.
	return SortToolsByName(tools)
}

// OpenAIToolsForServers returns MCP tools only from servers in the allowlist,
// converted to OpenAI function-tool format. Servers not in the allowlist are
// excluded entirely — the model never sees their tools. This is the security
// boundary for subagent MCP access: the user explicitly opts in each server
// by name via SubagentMCPServers in config. An empty allowlist yields no tools.
func (m *MCPManager) OpenAIToolsForServers(allowed map[string]bool) []proxy.Tool {
	if len(allowed) == 0 {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	var tools []proxy.Tool
	for _, srv := range m.servers {
		if srv.Status != "connected" {
			continue
		}
		if !allowed[srv.Cfg.Name] {
			continue
		}
		for _, t := range srv.Tools {
			tools = append(tools, MCPToolToOpenAI(srv.Cfg.Name, t))
		}
	}
	// Sort by name for cache-prefix stability.
	return SortToolsByName(tools)
}

// MCP tool gating — read-allowlist with write-veto (policy decision
// 2026-07-19, Trello nAoZenva). History: the original write-keyword
// blacklist defaulted UNKNOWN tools to READ, so "drop_database" or
// "execute_command" auto-executed under AllowReads — a mutating call riding
// a consent the user gave for reads (L2). Under the allowlist, a
// misclassified READ tool costs one prompt; a misclassified WRITE tool can
// no longer auto-execute. Name heuristics remain friction, not a security
// boundary — but now they fail safe.
//
// mcpReadKeywords / mcpWriteKeywords are matched against WHOLE name segments
// (see IsMCPReadTool). A write segment vetoes a read segment, so
// "get_and_delete" confirms while "get_user" skips.
var mcpReadKeywords = []string{
	"search", "list", "get", "fetch", "query", "read", "resolve",
	"find", "show", "describe", "lookup", "check", "view", "inspect",
}

var mcpWriteKeywords = []string{
	"write", "create", "update", "delete", "remove", "insert",
	"put", "post", "set", "edit", "modify", "push", "send", "publish",
	"drop", "run", "execute", "truncate", "kill", "format", "apply",
	"submit", "upsert", "disable", "enable", "forget", "reset",
}

// isMCPReadTool returns true when a tool name looks read-only. Matching is
// word-boundary based: the name is lowercased and split on non-alphanumerics
// (underscores, hyphens, dots); any WRITE segment vetoes, otherwise one READ
// segment suffices — so "read_file" and "get-user" match, but
// "readonly_mode_disable", "get_and_delete", "checkpoint_create", and
// "spreadsheet_update" do not. Unknown names default to WRITE (confirm) —
// the AllowReads consent means reads, so a name must earn the skip, not
// inherit it.
func IsMCPReadTool(name string) bool {
	lower := strings.ToLower(name)
	segs := strings.FieldsFunc(lower, func(r rune) bool {
		return r < 'a' || r > 'z'
	})
	isRead := false
	for _, s := range segs {
		for _, kw := range mcpWriteKeywords {
			if s == kw {
				return false // write segment vetoes immediately
			}
		}
		for _, kw := range mcpReadKeywords {
			if s == kw {
				isRead = true
			}
		}
	}
	return isRead
}

// CallTool routes a namespaced tool call to the owning server, gates it,
// and returns the result string. allowReads mirrors App.AllowReads: when true,
// read-only MCP tools skip the confirm prompt automatically.
func (m *MCPManager) CallTool(ctx context.Context, name string, argsJSON string, confirm Confirmer, allowReads bool) string {
	serverName, toolName, ok := strings.Cut(name, mcpNS)
	if !ok {
		return fmt.Sprintf("ERROR: not an MCP tool name %q", name)
	}
	isRead := IsMCPReadTool(toolName)

	// Phase 1: brief read lock to find the server. We don't hold the session
	// lock yet — the confirm prompt may block indefinitely on user input, and
	// we don't want to block Reconnect.
	m.mu.RLock()
	var srv *MCPServer
	for _, s := range m.servers {
		if s.Cfg.Name == serverName {
			srv = s
			break
		}
	}
	m.mu.RUnlock()

	if srv == nil {
		return fmt.Sprintf("ERROR: no MCP server named %q", serverName)
	}

	// A server still connecting holds this tool call only for ITSELF: wait on
	// that server's ready signal, never on the others. The wait is bounded by
	// the call ctx (turn cancellation) and by the connect's own 30s timeout.
	if srv.ready != nil {
		select {
		case <-srv.ready:
		case <-ctx.Done():
			return fmt.Sprintf("ERROR: MCP server %q still connecting: %v", serverName, ctx.Err())
		}
	}

	m.mu.RLock()
	status, srvErrStr := srv.Status, ""
	if srv.Err != nil {
		srvErrStr = srv.Err.Error()
	}
	m.mu.RUnlock()
	if status != "connected" {
		return fmt.Sprintf("ERROR: MCP server %q is %s: %s", serverName, status, srvErrStr)
	}

	// Confirmation gate — no locks held, so Reconnect can proceed while
	// the user is at the prompt.
	if !(isRead && allowReads) {
		detail := fmt.Sprintf("server: %s\ntool:   %s\nargs:   %s", serverName, toolName, PrettyArgs(argsJSON))
		if !confirm(name, fmt.Sprintf("Call MCP tool %s?", toolName), detail, isRead) {
			return "[declined by user]"
		}
	}

	var arguments any
	if argsJSON != "" && argsJSON != "{}" {
		if err := json.Unmarshal([]byte(argsJSON), &arguments); err != nil {
			// Reject invalid args instead of falling back to a raw string.
			// A raw string bypasses schema validation and may cause the MCP
			// server to interpret the input unpredictably.
			return fmt.Sprintf("ERROR: MCP tool %q: invalid arguments JSON: %v", toolName, err)
		}
		// MCP tool arguments must be a JSON object (map). A bare string,
		// number, or array is not a valid tool call — reject it.
		if _, ok := arguments.(map[string]interface{}); !ok {
			return fmt.Sprintf("ERROR: MCP tool %q: arguments must be a JSON object, got %T", toolName, arguments)
		}
	}

	// Phase 2: re-acquire under both locks. m.mu.RLock re-checks status (it may
	// have changed during the confirm prompt, or the manager may have closed).
	// While still under m.mu.RLock, acquire sessionMu.RLock — this prevents
	// Reconnect from closing the session until CallTool finishes. Then release
	// m.mu.RLock and call session.CallTool with sessionMu.RLock held.
	m.mu.RLock()
	status, srvErrStr = srv.Status, ""
	if srv.Err != nil {
		srvErrStr = srv.Err.Error()
	}
	if status != "connected" || srv.Session == nil {
		m.mu.RUnlock()
		return fmt.Sprintf("ERROR: MCP server %q is %s: %s", serverName, status, srvErrStr)
	}
	// Acquire the session lock while still under m.mu.RLock so Reconnect
	// can't swap/close the session between our RUnlock and RLock.
	srv.sessionMu.RLock()
	session := srv.Session
	m.mu.RUnlock()
	defer srv.sessionMu.RUnlock()

	res, err := session.CallTool(ctx, &gosdkmcp.CallToolParams{
		Name:      toolName,
		Arguments: arguments,
	})
	if err != nil {
		return "ERROR: " + err.Error()
	}
	return ExtractMCPResult(res)
}

// Reconnect closes and re-connects a named server. Used by /mcp reconnect.
// The connect runs outside the manager lock so readers (OpenAITools, CallTool,
// Servers) are not blocked for up to 30s. In-flight CallTool calls on the old
// session are drained (via sessionMu) before the old session is closed.
// Concurrent Reconnect calls for the same server are serialized via
// reconnectMu to prevent overlapping connect attempts that would leak sessions.
// Returns an error if the server is closed (manager shutdown).
func (m *MCPManager) Reconnect(ctx context.Context, name string) error {
	// Phase 1: find server and acquire reconnectMu — short read lock + per-server lock.
	m.mu.RLock()
	var srv *MCPServer
	for _, s := range m.servers {
		if s.Cfg.Name == name {
			srv = s
			break
		}
	}
	m.mu.RUnlock()
	if srv == nil {
		return fmt.Errorf("no MCP server named %q", name)
	}

	srv.reconnectMu.Lock()
	defer srv.reconnectMu.Unlock()

	// Phase 2: detach old session, mark "connecting" — short write lock.
	// Bail out if the server/manager was closed.
	m.mu.Lock()
	if srv.Status == "closed" {
		m.mu.Unlock()
		return fmt.Errorf("MCP server %q is closed", name)
	}
	oldSession := srv.Session
	srv.Status = "connecting"
	srv.Err = nil
	srv.Tools = nil
	srv.Session = nil
	m.mu.Unlock()

	// Phase 3: drain in-flight calls and close old session — outside m.mu.
	// sessionMu.Lock blocks until all CallTool RLock holders release, then
	// prevents new CallTool from acquiring RLock on this session while we
	// close it. After this, no one can use oldSession.
	if oldSession != nil {
		srv.sessionMu.Lock()
		_ = oldSession.Close()
		srv.sessionMu.Unlock()
	}

	// Phase 4: build the new session into a temp MCPServer — outside m.mu.
	// connectFn mutates the *MCPServer it's given, so we use a temp to avoid
	// racing with readers that access the live srv.
	tmp := &MCPServer{Cfg: srv.Cfg, Status: "connecting"}
	if err := m.connectOrDefault()(ctx, tmp); err != nil {
		m.mu.Lock()
		// Do NOT resurrect a closed server by relabelling it "failed":
		// Close is permanent, and a later Reconnect only rejects "closed".
		// Close() can have run while we were connecting — leave the status
		// alone in that case so the manager stays shut down.
		if srv.Status != "closed" {
			srv.Status = "failed"
			srv.Err = err
		}
		m.mu.Unlock()
		return err
	}

	// Phase 5: publish the new session under a short write lock.
	// Re-check for closed: if Close() ran while we were connecting, don't
	// resurrect the server — close the new session instead.
	m.mu.Lock()
	defer m.mu.Unlock()
	if srv.Status == "closed" {
		_ = tmp.Session.Close()
		return fmt.Errorf("MCP server %q is closed", name)
	}
	srv.Session = tmp.Session
	srv.Tools = tmp.Tools
	srv.Status = tmp.Status
	if srv.Status == "" {
		srv.Status = "connected"
	}
	srv.Err = nil
	return nil
}

// Close shuts down all MCP server connections. Drains in-flight CallTool
// calls before closing each session (same drain pattern as Reconnect).
// After Close, the manager is permanently shut down — Reconnect will refuse
// to resurrect closed servers.
//
// The drain is BOUNDED by closeDrainTimeout. sessionMu.Lock waits for every
// in-flight CallTool RLock holder to return, and CallTool holds that lock for
// the whole RPC — including a long-polling bridge call that can block for
// minutes. Context cancellation is cooperative: if a transport ignores it,
// an unbounded Lock would hang the caller's teardown forever (for the TUI,
// facade.Close during a rotation). On timeout the session is abandoned rather
// than force-closed, since closing underneath a live RPC is the race
// sessionMu exists to prevent; the servers are already marked "closed", so no
// new call can reach them.
func (m *MCPManager) Close() {
	// Snapshot sessions under the lock and mark all as closed.
	// Nilling Session prevents Close from double-closing and prevents
	// Reconnect's publish phase from installing a live session.
	m.mu.Lock()
	type srvSession struct {
		srv     *MCPServer
		session mcpSession
	}
	var targets []srvSession
	for _, srv := range m.servers {
		srv.Status = "closed"
		// Unblock any CallTool waiting on a still-connecting server: after
		// Close the server will never become ready, so the waiter must not
		// block until its ctx expires. Test-constructed servers may have a nil
		// ready channel — skip them.
		if srv.ready != nil {
			srv.readyOnce.Do(func() { close(srv.ready) })
		}
		if srv.Session != nil {
			targets = append(targets, srvSession{srv: srv, session: srv.Session})
			srv.Session = nil
		}
	}
	m.mu.Unlock()

	// Drain in-flight calls and close sessions — outside m.mu.
	for _, t := range targets {
		if !t.srv.drainAndClose(t.session, closeDrainTimeout) {
			diag.Printf("mcp: drain timed out after %v closing %q; session abandoned",
				closeDrainTimeout, t.srv.Cfg.Name)
		}
	}
}

// --- helpers ---

func MCPToolToOpenAI(serverName string, t *gosdkmcp.Tool) proxy.Tool {
	params, _ := json.Marshal(t.InputSchema)
	return proxy.Tool{
		Type: "function",
		Function: proxy.ToolFunction{
			Name:        serverName + mcpNS + t.Name,
			Description: fmt.Sprintf("[%s] %s", serverName, t.Description),
			Parameters:  params,
		},
	}
}

func ExtractMCPResult(res *gosdkmcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		if tc, ok := c.(*gosdkmcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	text := strings.Join(parts, "\n")
	if res.IsError {
		if text == "" {
			return "ERROR: (no message)"
		}
		return "ERROR: " + text
	}
	if text == "" {
		return "(no output)"
	}
	return text
}

func PrettyArgs(raw string) string {
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return raw
	}
	b, _ := json.MarshalIndent(m, "        ", "  ")
	return string(b)
}

// checkPendingToolDef returns the check_pending tool definition:
// the read-only retrieval surface for async operation results.
func checkPendingToolDef() proxy.Tool {
	params, _ := json.Marshal(map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"id": map[string]interface{}{
				"type":        "string",
				"description": "Optional operation id (e.g. \"op-3\") to get the status/result of one async op. Omit to list all pending/completed ops.",
			},
		},
	})
	return proxy.Tool{Type: "function", Function: proxy.ToolFunction{
		Name: "check_pending",
		Description: "Check on async operations that are running in the background (Mashūra counsel calls, long shell commands). " +
			"Without arguments: list all ops with their state. With an id: get the status, or the full result once the op completed. " +
			"Read-only and cheap — but don't poll in a tight loop; async results are also injected into context automatically before the next model request.",
		Parameters: params,
	}}
}

// waitForCompletionToolDef is the Phase 2 tool: it lets the model
// hand control back and suspend the turn until an async completion arrives,
// removing the check_pending spin loop as the only option. Returns immediately
// with a token; the turn loop translates it into a Suspended outcome (the
// caller awaits a real completion via WaitForAsyncCompletion and resumes).
func waitForCompletionToolDef() proxy.Tool {
	params, _ := json.Marshal(map[string]interface{}{
		"type":       "object",
		"properties": map[string]interface{}{},
	})
	return proxy.Tool{Type: "function", Function: proxy.ToolFunction{
		Name: "wait_for_completion",
		Description: "Blockingly await the next async completion (Mashūra panel, detached shell job, discovery subagent) " +
			"instead of polling check_pending. If async work is pending, the turn suspends and is automatically resumed " +
			"when a completion arrives; if nothing is pending, returns immediately. Prefer this over looping check_pending.",
		Parameters: params,
	}}
}

// collapseContextToolDef returns the collapse_context tool definition: lets
// the model proactively collapse accumulated working-set messages mid-turn (or
// sweep deferred prior turns) in long-session mode when a sub-task finishes or
// a new task begins.
func collapseContextToolDef() proxy.Tool {
	params, _ := json.Marshal(map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"summary": map[string]interface{}{
				"type":        "string",
				"description": "Cumulative summary of key findings, decisions, files modified, and state from the completed work being collapsed. Required; if called more than once in a turn, include earlier collapsed findings so nothing is lost.",
			},
			"next_steps": map[string]interface{}{
				"type":        "string",
				"description": "Optional clean instructions or plan for how to proceed next after the context is collapsed.",
			},
			"memory_key": map[string]interface{}{
				"type":        "string",
				"description": "Optional: store the summary as a named memory entry and replace the raw context with a one-line pointer. Use when this distilled knowledge will be needed later in the session but not immediately — recall it with memory_get or memory_search. The key is a bare single-segment name (letters, digits, hyphens); it is namespaced under ctx/<this-chat>/ automatically. Qualified keys must be exactly ctx/<this-chat>/<name> with a non-empty single-segment name — foreign namespaces, empty/dot/dot-dot/multi-segment suffixes are refused.",
			},
			"description": map[string]interface{}{
				"type":        "string",
				"description": "Optional one-line description of the memory_key stash, shown in listings and the checkpoint pointer.",
			},
		},
		"required": []string{"summary"},
	})
	return proxy.Tool{Type: "function", Function: proxy.ToolFunction{
		Name: "collapse_context",
		Description: "Proactively collapse accumulated tool-call working sets in long-session mode without waiting for the turn to end. " +
			"Call this when a distinct sub-task or investigation phase is complete and you are transitioning to the next phase or a new task. " +
			"Archives earlier tool rounds of the current request (and any newly-eligible prior requests) to disk and replaces them in context with your summary, next_steps, and an archive file path you can read_file back if needed.",
		Parameters: params,
	}}
}

// toolsForTurn returns the tool list for one turn. MCP servers connect in the
// background, so their tools appear as each server becomes ready; this replaces
// the MCP group captured at construction with the live snapshot, keeping the
// MCP tools in their original slot (so the built-in / search / oracle / LSP /
// browser cache prefix is unchanged). When no MCP tools were present at
// construction (every server was still connecting), the live set is appended
// where the MCP group would have been — but we cannot know that slot, so it is
// appended at the end; MCP tools are the only group whose membership changes
// mid-session, so this is the least-disruptive placement.
//
// MCP tools are identified by the mcpNS namespace ("server__tool") in their
// name — the same discriminator CallTool uses. Built-in tools never contain "__".
func (a *App) toolsForTurn() []proxy.Tool {
	if a.MCP == nil || len(a.MCP.Servers()) == 0 {
		return a.Tools
	}
	live := a.MCP.OpenAITools()
	// Find the construction-time MCP slot (first MCP tool).
	firstMCP := -1
	for i, t := range a.Tools {
		if strings.Contains(t.Function.Name, mcpNS) {
			firstMCP = i
			break
		}
	}
	if firstMCP < 0 {
		// No MCP tools at construction: append the live set.
		out := make([]proxy.Tool, 0, len(a.Tools)+len(live))
		out = append(out, a.Tools...)
		out = append(out, live...)
		return out
	}
	// Replace the contiguous MCP block starting at firstMCP.
	out := make([]proxy.Tool, 0, len(a.Tools)+len(live))
	out = append(out, a.Tools[:firstMCP]...)
	out = append(out, live...)
	for _, t := range a.Tools[firstMCP:] {
		if strings.Contains(t.Function.Name, mcpNS) {
			continue // drop the stale MCP block
		}
		out = append(out, t)
	}
	return out
}

// BuildTools assembles the full tool list in stable groups:// built-ins → searxng → google → MCP → oracle → LSP → browser.
// Within each group, tools are sorted by name for prompt-cache stability
// : tool schemas are part of the cache prefix, so non-deterministic
// ordering within a group (e.g., MCP servers returning tools in a different
// order on reconnect) would silently invalidate the prefix. Group order is
// preserved so toggling a conditional group only invalidates the suffix,
// not the entire prefix.
func BuildTools(cfg config.Config, cwd string, mcp *MCPManager) []proxy.Tool {
	// Group 1: built-ins (DefaultTools already has a fixed order).
	t := wtools.DefaultTools(cwd)
	// check_pending is always available — it is the read-only
	// retrieval surface for async results (mashūra panels, detached shell
	// jobs). Cheap, ungated, and excluded from subagent toolsets.
	t = append(t, checkPendingToolDef())
	// Phase 2: wait_for_completion lets the model explicitly hand
	// control and suspend the turn until an async completion arrives, removing
	// the check_pending spin loop as the only option. Ungated and always
	// available (like check_pending).
	t = append(t, waitForCompletionToolDef())
	// collapse_context lets the model trigger mid-turn or task-boundary
	// working-set collapse in long-session mode. Ungated and excluded from
	// subagent toolsets.
	t = append(t, collapseContextToolDef())

	// Group 2: search (conditional — appended at the end to preserve the
	// built-in prefix when absent).
	if cfg.SearXngURL != "" {
		t = append(t, wtools.SearxngTools()...)
	}
	if cfg.GoogleAPIKey != "" && cfg.GoogleCX != "" {
		t = append(t, wtools.GoogleTools()...)
	}
	// Group 3: MCP (conditional). OpenAITools already sorts by name.
	if mcp != nil {
		t = append(t, mcp.OpenAITools()...)
	}
	// Group 4: oracle (conditional).
	if cfg.OracleEnabled && (os.Getenv(cfg.OracleAPIKeyEnv) != "" || os.Getenv(cfg.OpenRouterAPIKeyEnv) != "") {
		t = append(t, mashuraToolDefs()...)
	}
	// Group 5: LSP (conditional).
	if cfg.LSPEnabled {
		t = append(t, lsp.LSPTools(cwd)...)
	}
	// Group 6: browser (conditional).
	if cfg.BrowserEnabled {
		t = append(t, browser.BrowserTools()...)
	}
	return t
}
