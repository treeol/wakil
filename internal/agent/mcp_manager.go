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
	Status  string           // "connecting" | "connected" | "failed"
	Err     error
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

// mcpWriteKeywords are substrings that indicate a mutating MCP tool. Any tool
// whose name does NOT contain one of these is treated as read-only.
//
// THREAT-MODEL NOTE (L2): this is a blacklist that defaults unknown tools to
// READ — names like "drop_database", "run_script", or "execute_command"
// contain none of these substrings and are therefore classified read-only,
// skipping the confirmation prompt when App.AllowReads is on. This is pinned
// deliberately in TestIsMCPReadTool_DocumentsDefaultReadRisk (WP-3); whether
// to flip the default to write-until-proven-read is an open policy decision
// tracked as a follow-up card. Do not "fix" the test without deciding here.
var mcpWriteKeywords = []string{
	"write", "create", "update", "delete", "remove", "insert",
	"put", "post", "set", "edit", "modify", "push", "send", "publish",
}

// isMCPReadTool returns true when a tool name looks like a read-only operation
// (search, fetch, query, list, resolve …). Uses a write-keyword blacklist so
// unknown tools default to read rather than silently skipping the [a] option.
func IsMCPReadTool(name string) bool {
	lower := strings.ToLower(name)
	for _, kw := range mcpWriteKeywords {
		if strings.Contains(lower, kw) {
			return false
		}
	}
	return true
}

// CallTool routes a namespaced tool call to the owning server, gates it,
// and returns the result string. allowReads mirrors App.AllowReads: when true,
// read-only MCP tools skip the confirm prompt automatically.
func (m *MCPManager) CallTool(ctx context.Context, name string, argsJSON string, confirm Confirmer, allowReads bool) string {
	serverName, toolName, ok := strings.Cut(name, mcpNS)
	if !ok {
		return fmt.Sprintf("ERROR: not an MCP tool name %q", name)
	}

	// Copy the fields we need under the lock so Reconnect can't mutate them
	// concurrently (status/session/err are written by Reconnect under a write lock).
	m.mu.RLock()
	var status, srvErrStr string
	var session mcpSession
	found := false
	for _, s := range m.servers {
		if s.Cfg.Name == serverName {
			found = true
			status = s.Status
			session = s.Session
			if s.Err != nil {
				srvErrStr = s.Err.Error()
			}
			break
		}
	}
	m.mu.RUnlock()

	if !found {
		return fmt.Sprintf("ERROR: no MCP server named %q", serverName)
	}
	if status != "connected" {
		return fmt.Sprintf("ERROR: MCP server %q is %s: %s", serverName, status, srvErrStr)
	}

	isRead := IsMCPReadTool(toolName)
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
// Holds the write lock for the entire operation so no reader can observe a
// half-torn-down or half-rebuilt server state.
func (m *MCPManager) Reconnect(ctx context.Context, name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	var srv *MCPServer
	for _, s := range m.servers {
		if s.Cfg.Name == name {
			srv = s
			break
		}
	}
	if srv == nil {
		return fmt.Errorf("no MCP server named %q", name)
	}
	if srv.Session != nil {
		srv.Session.Close()
	}
	srv.Status = "connecting"
	srv.Err = nil
	srv.Tools = nil
	srv.Session = nil
	if err := m.connectOrDefault()(ctx, srv); err != nil {
		srv.Status = "failed"
		srv.Err = err
		return err
	}
	return nil
}

// Close shuts down all MCP server connections.
func (m *MCPManager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, srv := range m.servers {
		if srv.Session != nil {
			srv.Session.Close()
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
