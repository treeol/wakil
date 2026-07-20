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
	"github.com/treeol/wakil/internal/lsp"
	"github.com/treeol/wakil/internal/proxy"
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

	// sessionMu serializes CallTool (RLock) against Reconnect/Close (Lock)
	// on the same session. CallTool holds RLock for the duration of
	// session.CallTool; Reconnect/Close hold Lock while closing the old
	// session, which drains in-flight calls before Close is called.
	sessionMu sync.RWMutex

	// reconnectMu serializes concurrent Reconnect calls for the same server,
	// preventing overlapping connect attempts that would leak sessions.
	reconnectMu sync.Mutex
}

// MCPManager owns all MCP server connections for the process lifetime.
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
func NewMCPManager(ctx context.Context, cfgs []config.MCPServerConfig) *MCPManager {
	m := &MCPManager{}
	m.connectFn = m.connect
	for _, cfg := range cfgs {
		srv := &MCPServer{Cfg: cfg, Status: "connecting"}
		m.servers = append(m.servers, srv)
		// Connect synchronously so tools are available before first user turn.
		if err := m.connectOrDefault()(ctx, srv); err != nil {
			srv.Status = "failed"
			srv.Err = err
		}
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
	return tools
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
	return tools
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

	// Phase 1: brief read lock to check server exists and is connected.
	// We don't hold the session lock yet — the confirm prompt may block
	// indefinitely on user input, and we don't want to block Reconnect.
	m.mu.RLock()
	var status, srvErrStr string
	for _, s := range m.servers {
		if s.Cfg.Name == serverName {
			status = s.Status
			if s.Err != nil {
				srvErrStr = s.Err.Error()
			}
			break
		}
	}
	m.mu.RUnlock()

	if status == "" {
		return fmt.Sprintf("ERROR: no MCP server named %q", serverName)
	}
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
			arguments = argsJSON // fall back to raw string
		}
	}

	// Phase 2: re-acquire under both locks. m.mu.RLock finds the server and
	// re-checks status (may have changed during the confirm prompt). While
	// still under m.mu.RLock, acquire sessionMu.RLock — this prevents
	// Reconnect from closing the session until CallTool finishes. Then
	// release m.mu.RLock and call session.CallTool with sessionMu.RLock held.
	m.mu.RLock()
	var srv *MCPServer
	found := false
	for _, s := range m.servers {
		if s.Cfg.Name == serverName {
			found = true
			srv = s
			status = s.Status
			if s.Err != nil {
				srvErrStr = s.Err.Error()
			}
			break
		}
	}
	if !found || srv.Status != "connected" || srv.Session == nil {
		m.mu.RUnlock()
		if !found {
			return fmt.Sprintf("ERROR: no MCP server named %q", serverName)
		}
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
		srv.Status = "failed"
		srv.Err = err
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
		if srv.Session != nil {
			targets = append(targets, srvSession{srv: srv, session: srv.Session})
			srv.Session = nil
		}
	}
	m.mu.Unlock()

	// Drain in-flight calls and close sessions — outside m.mu.
	for _, t := range targets {
		t.srv.sessionMu.Lock()
		_ = t.session.Close()
		t.srv.sessionMu.Unlock()
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

// BuildTools assembles the full tool list: built-ins → searxng → google → MCP → oracle → LSP.
func BuildTools(cfg config.Config, cwd string, mcp *MCPManager) []proxy.Tool {
	t := wtools.DefaultTools(cwd)
	if cfg.SearXngURL != "" {
		t = append(t, wtools.SearxngTools()...)
	}
	if cfg.GoogleAPIKey != "" && cfg.GoogleCX != "" {
		t = append(t, wtools.GoogleTools()...)
	}
	if mcp != nil {
		t = append(t, mcp.OpenAITools()...)
	}
	if cfg.OracleEnabled && (os.Getenv(cfg.OracleAPIKeyEnv) != "" || os.Getenv(cfg.OpenRouterAPIKeyEnv) != "") {
		t = append(t, mashuraToolDefs()...)
	}
	if cfg.LSPEnabled {
		t = append(t, lsp.LSPTools(cwd)...)
	}
	if cfg.BrowserEnabled {
		t = append(t, browser.BrowserTools()...)
	}
	return t
}
