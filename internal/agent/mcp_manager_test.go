package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"

	gosdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// MCP manager tests. No real subprocesses, no real network: the WP-3 seam
// (mcpSession interface + MCPManager.connectFn) injects fakes.
//
// Coverage map:
//   - IsMCPReadTool adversarial table (pins the default-read risk, L2)
//   - PrettyArgs / ExtractMCPResult / MCPToolToOpenAI pure helpers
//   - OpenAITools / OpenAIToolsForServers exposure + allowlist security boundary
//   - CallTool routing + gating matrix (decline → fake session never called)
//   - Reconnect / Close lifecycle via fast fake connectFn

// fakeMCPSession records CallTool invocations and Close calls.
type fakeMCPSession struct {
	mu        sync.Mutex
	callCount int
	lastName  string
	lastArgs  any
	closed    int
	result    *gosdkmcp.CallToolResult
	err       error
}

func (f *fakeMCPSession) CallTool(_ context.Context, params *gosdkmcp.CallToolParams) (*gosdkmcp.CallToolResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callCount++
	f.lastName = params.Name
	f.lastArgs = params.Arguments
	if f.err != nil {
		return nil, f.err
	}
	if f.result != nil {
		return f.result, nil
	}
	return &gosdkmcp.CallToolResult{
		Content: []gosdkmcp.Content{&gosdkmcp.TextContent{Text: "mcp-result"}},
	}, nil
}

func (f *fakeMCPSession) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	return nil
}

func (f *fakeMCPSession) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.callCount
}

func (f *fakeMCPSession) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// newFakeMCPManager hand-constructs a manager with the given servers already
// connected (Tools set manually — connect() normally fills them via ListTools).
// connectFn is replaced with a fast fake; it never spawns subprocesses.
func newFakeMCPManager(servers ...*MCPServer) *MCPManager {
	m := &MCPManager{servers: servers}
	m.connectFn = func(_ context.Context, srv *MCPServer) error {
		if srv.Session == nil {
			srv.Session = &fakeMCPSession{}
		}
		srv.Status = "connected"
		return nil
	}
	return m
}

func connectedServer(name string, session *fakeMCPSession, toolNames ...string) *MCPServer {
	var tools []*gosdkmcp.Tool
	for _, tn := range toolNames {
		tools = append(tools, &gosdkmcp.Tool{
			Name:        tn,
			Description: "tool " + tn,
			InputSchema: map[string]any{"type": "object"},
		})
	}
	return &MCPServer{
		Cfg:     config.MCPServerConfig{Name: name, Transport: "stdio", Command: "fake"},
		Session: session,
		Tools:   tools,
		Status:  "connected",
	}
}

// ── IsMCPReadTool ────────────────────────────────────────────────────────────

// TestIsMCPReadTool_ReadAllowlistFailSafe pins the read-keyword ALLOWLIST
// (policy decision 2026-07-19, Trello nAoZenva): a tool skips confirmation
// under AllowReads only when its name contains a read keyword. Unknown and
// write-like names default to WRITE (confirm) — the AllowReads consent means
// reads, so a name must earn the skip.
//
// History: this test was TestIsMCPReadTool_DocumentsDefaultReadRisk (WP-3),
// which pinned the OLD write-blacklist behavior where unknown tools defaulted
// to READ and the evasions below auto-executed. The flip was decided after a
// Mashūra 3-panel review (unanimous): default-read is fail-open, default-
// write is fail-safe — a misclassified read costs one prompt, a
// misclassified write can no longer auto-execute.
func TestIsMCPReadTool_ReadAllowlistFailSafe(t *testing.T) {
	readLike := []string{
		"search", "list", "fetch", "query", "get", "resolve", "read_file",
		"find_files", "show_status", "describe_table", "lookup_user",
		"check_health", "view_log", "inspect_state",
	}
	for _, name := range readLike {
		if !IsMCPReadTool(name) {
			t.Errorf("IsMCPReadTool(%q) = false — read-keyword tool must skip prompt under AllowReads", name)
		}
	}

	writeLike := []string{
		"write_file", "create", "UPDATE", "delete", "send", "push",
		"edit_file", "modify_config", "insert_row", "remove_user",
		"publish_event", "set_value",
	}
	for _, name := range writeLike {
		if IsMCPReadTool(name) {
			t.Errorf("IsMCPReadTool(%q) = true — write tool must confirm", name)
		}
	}

	// REGRESSION (the L2 evasions): under the old blacklist these classified
	// READ and auto-executed under AllowReads. They must now confirm.
	evasions := []string{
		"drop_database", "run_script", "execute_command", "truncate_table",
		"kill_process", "format_disk", "apply", "submit", "upsert",
	}
	for _, name := range evasions {
		if IsMCPReadTool(name) {
			t.Errorf("IsMCPReadTool(%q) = true — REGRESSION: known evasion auto-executes again", name)
		}
	}

	// Word-boundary evasions: substrings that LOOK read-ish inside mutating
	// names must not match (segment-exact matching).
	wordBoundaryEvasions := []string{
		"readonly_mode_disable", "unread_marker", "checkpoint_create",
		"checkout_branch", "recheck_and_fix", "spreadsheet_update",
		"get_and_delete", "forget_user",
	}
	for _, name := range wordBoundaryEvasions {
		if IsMCPReadTool(name) {
			t.Errorf("IsMCPReadTool(%q) = true — substring evasion: segment-exact matching must reject", name)
		}
	}

	// Unknown/ambiguous names confirm — fail-safe for anything not invented yet.
	unknown := []string{"sync_state", "ping", "noop", "handle_event", "process"}
	for _, name := range unknown {
		if IsMCPReadTool(name) {
			t.Errorf("IsMCPReadTool(%q) = true — unknown names must default to confirm", name)
		}
	}
}

// ── Pure helpers ─────────────────────────────────────────────────────────────

func TestPrettyArgs(t *testing.T) {
	// Valid object JSON pretty-prints with indentation.
	got := PrettyArgs(`{"a":1,"b":[2,3]}`)
	if !strings.Contains(got, "\n") || !strings.Contains(got, `"a": 1`) {
		t.Errorf("PrettyArgs should indent object JSON, got %q", got)
	}
	// Invalid JSON returns the raw input unchanged.
	if got := PrettyArgs(`{not json`); got != `{not json` {
		t.Errorf("invalid JSON should pass through raw, got %q", got)
	}
	// Scalar/array inputs don't match map unmarshal — raw passthrough.
	if got := PrettyArgs(`[1,2]`); got != `[1,2]` {
		t.Errorf("array should pass through raw, got %q", got)
	}
	if got := PrettyArgs(`42`); got != `42` {
		t.Errorf("scalar should pass through raw, got %q", got)
	}
}

func TestExtractMCPResult(t *testing.T) {
	text := func(s string) gosdkmcp.Content { return &gosdkmcp.TextContent{Text: s} }
	image := func() gosdkmcp.Content { return &gosdkmcp.ImageContent{Data: []byte("x"), MIMEType: "image/png"} }

	cases := []struct {
		name string
		res  *gosdkmcp.CallToolResult
		want string
	}{
		{"single text", &gosdkmcp.CallToolResult{Content: []gosdkmcp.Content{text("hello")}}, "hello"},
		{"multi text joined", &gosdkmcp.CallToolResult{Content: []gosdkmcp.Content{text("a"), text("b")}}, "a\nb"},
		{"non-text only → (no output)", &gosdkmcp.CallToolResult{Content: []gosdkmcp.Content{image()}}, "(no output)"},
		{"text + non-text keeps text", &gosdkmcp.CallToolResult{Content: []gosdkmcp.Content{image(), text("kept")}}, "kept"},
		{"empty content → (no output)", &gosdkmcp.CallToolResult{}, "(no output)"},
		{"error with message", &gosdkmcp.CallToolResult{IsError: true, Content: []gosdkmcp.Content{text("boom")}}, "ERROR: boom"},
		{"error without message", &gosdkmcp.CallToolResult{IsError: true}, "ERROR: (no message)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ExtractMCPResult(c.res); got != c.want {
				t.Errorf("ExtractMCPResult = %q, want %q", got, c.want)
			}
		})
	}
}

func TestMCPToolToOpenAI(t *testing.T) {
	tool := &gosdkmcp.Tool{
		Name:        "read_file",
		Description: "reads a file",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
	}
	got := MCPToolToOpenAI("fs", tool)
	if got.Function.Name != "fs__read_file" {
		t.Errorf("name = %q, want fs__read_file", got.Function.Name)
	}
	if !strings.HasPrefix(got.Function.Description, "[fs] ") {
		t.Errorf("description should carry the server prefix, got %q", got.Function.Description)
	}
	if !strings.Contains(string(got.Function.Parameters), `"path"`) {
		t.Errorf("schema should marshal through, got %s", got.Function.Parameters)
	}
}

// ── Tool exposure / allowlist boundary ───────────────────────────────────────

func TestOpenAITools_ExcludesFailedServers(t *testing.T) {
	m := newFakeMCPManager(
		connectedServer("good", &fakeMCPSession{}, "search"),
		&MCPServer{Cfg: config.MCPServerConfig{Name: "bad"}, Status: "failed", Err: errors.New("boom")},
	)
	tools := m.OpenAITools()
	if len(tools) != 1 || tools[0].Function.Name != "good__search" {
		t.Fatalf("failed server tools must be excluded, got %+v", toolNames(tools))
	}
}

func TestOpenAIToolsForServers_EmptyAllowlistYieldsNil(t *testing.T) {
	// SECURITY BOUNDARY: subagents get tools only for explicitly allowlisted
	// servers. An empty allowlist must yield NO tools — not "all tools".
	m := newFakeMCPManager(connectedServer("srv", &fakeMCPSession{}, "search"))
	if got := m.OpenAIToolsForServers(nil); got != nil {
		t.Fatalf("nil allowlist must yield nil, got %+v", toolNames(got))
	}
	if got := m.OpenAIToolsForServers(map[string]bool{}); got != nil {
		t.Fatalf("empty allowlist must yield nil, got %+v", toolNames(got))
	}
}

func TestOpenAIToolsForServers_OnlyAllowlisted(t *testing.T) {
	m := newFakeMCPManager(
		connectedServer("allowed", &fakeMCPSession{}, "search"),
		connectedServer("denied", &fakeMCPSession{}, "run_query"),
	)
	tools := m.OpenAIToolsForServers(map[string]bool{"allowed": true})
	if len(tools) != 1 || tools[0].Function.Name != "allowed__search" {
		t.Fatalf("only allowlisted server tools expected, got %+v", toolNames(tools))
	}
	// Unknown server names in the allowlist are simply absent (no panic).
	tools = m.OpenAIToolsForServers(map[string]bool{"allowed": true, "ghost": true})
	if len(tools) != 1 {
		t.Fatalf("ghost allowlist entry must not add tools, got %+v", toolNames(tools))
	}
}

func TestOpenAITools_DuplicateToolNamesStayDistinct(t *testing.T) {
	m := newFakeMCPManager(
		connectedServer("a", &fakeMCPSession{}, "read"),
		connectedServer("b", &fakeMCPSession{}, "read"),
	)
	names := toolNames(m.OpenAITools())
	if len(names) != 2 || names[0] == names[1] {
		t.Fatalf("namespaced duplicates must stay distinct, got %v", names)
	}
}

func toolNames(tools []proxy.Tool) []string {
	var out []string
	for _, t := range tools {
		out = append(out, t.Function.Name)
	}
	return out
}

// ── CallTool routing + gating ────────────────────────────────────────────────

func TestMCPCallTool_BadNamespace(t *testing.T) {
	m := newFakeMCPManager(connectedServer("srv", &fakeMCPSession{}, "x"))
	got := m.CallTool(context.Background(), "no-separator", "{}", nil, false)
	if !strings.HasPrefix(got, "ERROR: not an MCP tool name") {
		t.Fatalf("bad namespace: %q", got)
	}
}

func TestMCPCallTool_UnknownServer(t *testing.T) {
	m := newFakeMCPManager(connectedServer("srv", &fakeMCPSession{}, "x"))
	got := m.CallTool(context.Background(), "ghost__x", "{}", nil, false)
	if !strings.Contains(got, `no MCP server named "ghost"`) {
		t.Fatalf("unknown server: %q", got)
	}
}

func TestMCPCallTool_NotConnected(t *testing.T) {
	m := newFakeMCPManager(&MCPServer{
		Cfg:    config.MCPServerConfig{Name: "down"},
		Status: "failed",
		Err:    errors.New("connection refused"),
	})
	got := m.CallTool(context.Background(), "down__x", "{}", nil, false)
	if !strings.Contains(got, "is failed") || !strings.Contains(got, "connection refused") {
		t.Fatalf("not-connected error should carry status+cause: %q", got)
	}
}

func TestMCPCallTool_ReadToolSkipsConfirmWithAllowReads(t *testing.T) {
	sess := &fakeMCPSession{}
	m := newFakeMCPManager(connectedServer("srv", sess, "search"))
	confirmCalled := 0
	confirm := func(_, _, _ string, _ bool) bool { confirmCalled++; return true }

	got := m.CallTool(context.Background(), "srv__search", `{"q":"x"}`, confirm, true)
	if got != "mcp-result" {
		t.Fatalf("read tool result: %q", got)
	}
	if confirmCalled != 0 {
		t.Fatalf("read tool + AllowReads must skip confirm, prompted %d times", confirmCalled)
	}
	if sess.calls() != 1 {
		t.Fatalf("vacuity: session must be called exactly once, got %d", sess.calls())
	}
}

func TestMCPCallTool_ReadToolConfirmsWithoutAllowReads(t *testing.T) {
	sess := &fakeMCPSession{}
	m := newFakeMCPManager(connectedServer("srv", sess, "search"))
	confirmCalled := 0
	confirm := func(_, _, _ string, _ bool) bool { confirmCalled++; return true }

	m.CallTool(context.Background(), "srv__search", `{"q":"x"}`, confirm, false)
	if confirmCalled != 1 {
		t.Fatalf("read tool without AllowReads must confirm, prompted %d", confirmCalled)
	}
	if sess.calls() != 1 {
		t.Fatalf("accepted call must execute once, got %d", sess.calls())
	}
}

func TestMCPCallTool_WriteToolAlwaysConfirms(t *testing.T) {
	sess := &fakeMCPSession{}
	m := newFakeMCPManager(connectedServer("srv", sess, "write_file"))
	confirmCalled := 0
	var sawReadAction bool
	confirm := func(_, _, _ string, readAction bool) bool {
		confirmCalled++
		sawReadAction = readAction
		return true
	}

	// Even WITH AllowReads, a write-keyword tool must prompt.
	m.CallTool(context.Background(), "srv__write_file", `{"path":"x"}`, confirm, true)
	if confirmCalled != 1 {
		t.Fatalf("write tool must confirm even with AllowReads, prompted %d", confirmCalled)
	}
	if sawReadAction {
		t.Errorf("write tool prompt must not offer the read-only fast path")
	}
}

func TestMCPCallTool_DeclineNeverTouchesSession(t *testing.T) {
	sess := &fakeMCPSession{}
	m := newFakeMCPManager(connectedServer("srv", sess, "delete_thing"))
	confirm := func(_, _, _ string, _ bool) bool { return false }

	got := m.CallTool(context.Background(), "srv__delete_thing", `{}`, confirm, false)
	if got != "[declined by user]" {
		t.Fatalf("declined call: %q", got)
	}
	if sess.calls() != 0 {
		t.Fatalf("declined MCP call reached the session: %d calls", sess.calls())
	}
}

func TestMCPCallTool_InvalidArgsJSONRejected(t *testing.T) {
	sess := &fakeMCPSession{}
	m := newFakeMCPManager(connectedServer("srv", sess, "search"))
	got := m.CallTool(context.Background(), "srv__search", `{broken`, nil, true)
	if !strings.HasPrefix(got, "ERROR:") {
		t.Fatalf("invalid args JSON should be rejected with an error, got %q", got)
	}
	// The call should NOT reach the session.
	if sess.calls() != 0 {
		t.Fatalf("invalid args JSON should not reach the MCP session: %d calls", sess.calls())
	}
}

func TestMCPCallTool_NonObjectArgsRejected(t *testing.T) {
	sess := &fakeMCPSession{}
	m := newFakeMCPManager(connectedServer("srv", sess, "search"))
	// Valid JSON but not an object — a bare string.
	got := m.CallTool(context.Background(), "srv__search", `"just a string"`, nil, true)
	if !strings.HasPrefix(got, "ERROR:") {
		t.Fatalf("non-object args should be rejected with an error, got %q", got)
	}
	if sess.calls() != 0 {
		t.Fatalf("non-object args should not reach the MCP session: %d calls", sess.calls())
	}
}

func TestMCPCallTool_SessionErrorSurfaces(t *testing.T) {
	sess := &fakeMCPSession{err: errors.New("tool exploded")}
	m := newFakeMCPManager(connectedServer("srv", sess, "search"))
	got := m.CallTool(context.Background(), "srv__search", `{}`, nil, true)
	if !strings.HasPrefix(got, "ERROR: tool exploded") {
		t.Fatalf("session error: %q", got)
	}
}

func TestMCPCallTool_ToolResultErrorExtracted(t *testing.T) {
	sess := &fakeMCPSession{result: &gosdkmcp.CallToolResult{
		IsError: true,
		Content: []gosdkmcp.Content{&gosdkmcp.TextContent{Text: "bad input"}},
	}}
	m := newFakeMCPManager(connectedServer("srv", sess, "search"))
	got := m.CallTool(context.Background(), "srv__search", `{}`, nil, true)
	if got != "ERROR: bad input" {
		t.Fatalf("tool-level error result: %q", got)
	}
}

// ── Reconnect / Close ────────────────────────────────────────────────────────

func TestMCPReconnect_UnknownServer(t *testing.T) {
	m := newFakeMCPManager(connectedServer("srv", &fakeMCPSession{}, "x"))
	if err := m.Reconnect(context.Background(), "ghost"); err == nil {
		t.Fatal("reconnect of unknown server must error")
	}
}

func TestMCPReconnect_ClosesOldSessionAndInstallsNew(t *testing.T) {
	old := &fakeMCPSession{}
	m := newFakeMCPManager(connectedServer("srv", old, "x"))
	fresh := &fakeMCPSession{}
	m.connectFn = func(_ context.Context, srv *MCPServer) error {
		srv.Session = fresh
		srv.Status = "connected"
		return nil
	}
	if err := m.Reconnect(context.Background(), "srv"); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if old.closeCount() != 1 {
		t.Fatalf("old session must be closed exactly once, got %d", old.closeCount())
	}
	for _, srv := range m.Servers() {
		if srv.Cfg.Name == "srv" {
			if srv.Session != fresh {
				t.Fatalf("new session not installed")
			}
			if srv.Status != "connected" {
				t.Fatalf("status = %q", srv.Status)
			}
		}
	}
}

func TestMCPReconnect_FailureMarksFailed(t *testing.T) {
	old := &fakeMCPSession{}
	m := newFakeMCPManager(connectedServer("srv", old, "x"))
	m.connectFn = func(_ context.Context, srv *MCPServer) error {
		return fmt.Errorf("server binary missing")
	}
	err := m.Reconnect(context.Background(), "srv")
	if err == nil || !strings.Contains(err.Error(), "server binary missing") {
		t.Fatalf("reconnect failure must surface: %v", err)
	}
	for _, srv := range m.Servers() {
		if srv.Cfg.Name == "srv" {
			if srv.Status != "failed" {
				t.Fatalf("status = %q, want failed", srv.Status)
			}
			if srv.Err == nil {
				t.Fatal("Err must be recorded")
			}
		}
	}
	// Failed server drops out of the tool list.
	if tools := m.OpenAITools(); len(tools) != 0 {
		t.Fatalf("failed server must not expose tools, got %+v", toolNames(tools))
	}
}

func TestMCPClose_ClosesAllSessions(t *testing.T) {
	s1, s2 := &fakeMCPSession{}, &fakeMCPSession{}
	m := newFakeMCPManager(
		connectedServer("a", s1, "x"),
		connectedServer("b", s2, "y"),
	)
	m.Close()
	if s1.closeCount() != 1 || s2.closeCount() != 1 {
		t.Fatalf("Close must close every session: %d, %d", s1.closeCount(), s2.closeCount())
	}
	// Second Close is a no-op: sessions are nilled after the first Close,
	// so they are not closed again.
	m.Close()
	if s1.closeCount() != 1 {
		t.Fatalf("second Close must not re-close nilled sessions, got %d", s1.closeCount())
	}
}

// ── buildTransport validation (pure, no processes spawned) ──────────────────

func TestBuildTransport_Validation(t *testing.T) {
	if _, err := buildTransport(config.MCPServerConfig{Transport: "stdio"}); err == nil {
		t.Error("stdio without command must error")
	}
	if _, err := buildTransport(config.MCPServerConfig{Transport: "carrier-pigeon"}); err == nil {
		t.Error("unknown transport must error")
	}
	tr, err := buildTransport(config.MCPServerConfig{Transport: "stdio", Command: "echo", Args: []string{"hi"}})
	if err != nil {
		t.Fatalf("valid stdio config: %v", err)
	}
	if tr == nil {
		t.Fatal("transport must not be nil")
	}
	// http/https transport builds without URL validation (pinning current
	// behavior — empty URL fails later at connect time, not here).
	if _, err := buildTransport(config.MCPServerConfig{Transport: "http"}); err != nil {
		t.Fatalf("pinning: http transport builds without URL: %v", err)
	}
}

// ── Typed-nil guard documentation ────────────────────────────────────────────

// TestMCPServer_TypedNilSessionIsNonNil documents the interface footgun the
// WP-3 plan calls out: assigning a typed-nil *fakeMCPSession to the
// mcpSession field makes `srv.Session != nil` true. Test authors must not do
// this; the test pins the Go semantics so a future "fix" (nil-check removal)
// is a conscious decision.
func TestMCPServer_TypedNilSessionIsNonNil(t *testing.T) {
	var typedNil *fakeMCPSession
	srv := &MCPServer{Session: typedNil}
	if srv.Session == nil {
		t.Fatal("typed-nil interface compares non-nil — do not assign typed-nil fakes in tests")
	}
}

// ── Concurrency: CallTool vs Reconnect drain ────────────────────────────────

// blockingSession is a fake mcpSession whose CallTool blocks until a release
// channel is closed. Used to prove that Reconnect drains in-flight calls
// (waits for CallTool to finish) before closing the old session.
type blockingSession struct {
	callEntered chan struct{}
	release     chan struct{}
	closeCount  atomic.Int32
}

func newBlockingSession() *blockingSession {
	return &blockingSession{
		callEntered: make(chan struct{}),
		release:     make(chan struct{}),
	}
}

func (s *blockingSession) CallTool(_ context.Context, _ *gosdkmcp.CallToolParams) (*gosdkmcp.CallToolResult, error) {
	close(s.callEntered)
	<-s.release
	return &gosdkmcp.CallToolResult{
		Content: []gosdkmcp.Content{&gosdkmcp.TextContent{Text: "old-result"}},
	}, nil
}

func (s *blockingSession) Close() error {
	s.closeCount.Add(1)
	return nil
}

// TestMCPReconnect_DrainsInFlightCallBeforeClosingOldSession proves the fix
// for the CallTool/Reconnect use-after-close race: when CallTool is in-flight
// on the old session, Reconnect must NOT close that session until CallTool
// finishes. The old session's Close() is deferred until the RLock is released.
func TestMCPReconnect_DrainsInFlightCallBeforeClosingOldSession(t *testing.T) {
	old := newBlockingSession()
	fresh := &fakeMCPSession{}
	m := newFakeMCPManager(&MCPServer{
		Cfg:     config.MCPServerConfig{Name: "srv", Transport: "stdio", Command: "fake"},
		Session: old,
		Tools:   []*gosdkmcp.Tool{{Name: "search", Description: "search", InputSchema: map[string]any{"type": "object"}}},
		Status:  "connected",
	})
	m.connectFn = func(_ context.Context, srv *MCPServer) error {
		srv.Session = fresh
		srv.Status = "connected"
		return nil
	}

	// Start CallTool in a goroutine — it will block inside old.CallTool.
	callDone := make(chan string, 1)
	go func() {
		callDone <- m.CallTool(context.Background(), "srv__search", `{}`, nil, true)
	}()

	// Wait until CallTool has entered the old session.
	<-old.callEntered

	// Start Reconnect in a goroutine — it should block trying to close old
	// (because CallTool holds sessionMu.RLock).
	reconnectDone := make(chan error, 1)
	go func() {
		reconnectDone <- m.Reconnect(context.Background(), "srv")
	}()

	// Give Reconnect a moment to reach the sessionMu.Lock() call.
	// The old session must NOT be closed while CallTool is in-flight.
	time.Sleep(50 * time.Millisecond)
	if got := old.closeCount.Load(); got != 0 {
		t.Fatalf("old session closed while CallTool in-flight (closeCount=%d) — drain not working", got)
	}

	// Release CallTool — it should complete with the old session's result.
	close(old.release)
	if got := <-callDone; got != "old-result" {
		t.Fatalf("CallTool result = %q, want old-result", got)
	}

	// Now Reconnect can proceed: close old, connect fresh, publish.
	if err := <-reconnectDone; err != nil {
		t.Fatalf("Reconnect: %v", err)
	}

	// Old session must be closed exactly once after drain.
	if got := old.closeCount.Load(); got != 1 {
		t.Fatalf("old session closeCount = %d, want 1", got)
	}

	// New session installed and connected.
	for _, srv := range m.Servers() {
		if srv.Cfg.Name == "srv" {
			if srv.Session != fresh {
				t.Fatal("new session not installed")
			}
			if srv.Status != "connected" {
				t.Fatalf("status = %q, want connected", srv.Status)
			}
		}
	}
}

// TestMCPReconnect_ReadersNotBlockedDuringConnect proves the fix for ISSUE 1:
// Reconnect no longer holds the manager write lock during the 30s connect.
// Readers (OpenAITools, Servers) must return promptly while connect is blocked.
func TestMCPReconnect_ReadersNotBlockedDuringConnect(t *testing.T) {
	old := &fakeMCPSession{}
	m := newFakeMCPManager(connectedServer("srv", old, "search"))

	connectStarted := make(chan struct{})
	releaseConnect := make(chan struct{})
	m.connectFn = func(_ context.Context, srv *MCPServer) error {
		close(connectStarted)
		<-releaseConnect
		srv.Session = &fakeMCPSession{}
		srv.Status = "connected"
		return nil
	}

	// Start Reconnect in a goroutine — connectFn will block.
	reconnectDone := make(chan error, 1)
	go func() {
		reconnectDone <- m.Reconnect(context.Background(), "srv")
	}()

	// Wait for connect to start (old session already closed+drained at this point).
	<-connectStarted

	// While connect is blocked, OpenAITools must return promptly (not blocked).
	// The server is in "connecting" state, so it yields no tools — but it
	// returns immediately rather than hanging.
	done := make(chan struct{})
	go func() {
		_ = m.OpenAITools()
		close(done)
	}()
	select {
	case <-done:
		// Good — readers are not blocked.
	case <-time.After(time.Second):
		t.Fatal("OpenAITools blocked during Reconnect connect — write lock held too long")
	}

	// Let Reconnect finish.
	close(releaseConnect)
	if err := <-reconnectDone; err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
}
