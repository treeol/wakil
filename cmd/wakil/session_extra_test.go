package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/workflow"

	gosdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
)

// --- session listing (against an isolated WAKIL_SESSIONS_DIR) ---

func TestSessionListingAndLoad(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)

	older := &agent.Session{
		ChatID: "aaa11122", Model: "ilm", Updated: time.Now().Add(-time.Hour),
		Conv: []proxy.Message{{Role: "user", Content: agent.StrPtr("first task here")}},
	}
	newer := &agent.Session{
		ChatID: "bbb33344", Model: "ilm", Updated: time.Now(),
		Conv: []proxy.Message{
			{Role: "user", Content: agent.StrPtr("newer task")},
			{Role: "assistant", Content: agent.StrPtr("done")},
			{Role: "user", Content: agent.StrPtr("follow up")},
		},
	}
	for _, s := range []*agent.Session{older, newer} {
		if err := agent.WriteSession(s); err != nil {
			t.Fatal(err)
		}
	}

	// agent.ListSessions: newest first.
	got, err := agent.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ChatID != "bbb33344" {
		t.Fatalf("expected newest-first ordering; got %+v", got)
	}

	// sessionTurns counts user turns and returns the first user message.
	turns, first := agent.SessionTurns(*newer)
	if turns != 2 || first != "newer task" {
		t.Errorf("sessionTurns = (%d,%q), want (2,%q)", turns, first, "newer task")
	}

	// agent.LoadSession by unique prefix.
	s, err := agent.LoadSession("bbb")
	if err != nil || s.ChatID != "bbb33344" {
		t.Errorf("agent.LoadSession(prefix) = %v, %v", s, err)
	}

	// agent.LoadSession("") returns the most recent.
	s, err = agent.LoadSession("")
	if err != nil || s.ChatID != "bbb33344" {
		t.Errorf("agent.LoadSession(\"\") should return most recent; got %v, %v", s, err)
	}

	// Unknown prefix errors.
	if _, err := agent.LoadSession("zzz"); err == nil {
		t.Error("unknown prefix should error")
	}
}

func TestLoadSessionAmbiguousPrefix(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)
	for _, id := range []string{"dup-aaa", "dup-bbb"} {
		if err := agent.WriteSession(&agent.Session{ChatID: id, Updated: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	_, err := agent.LoadSession("dup-")
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("ambiguous prefix should error; got %v", err)
	}
}

func TestSessionListTextMarksCurrent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)
	if err := agent.WriteSession(&agent.Session{ChatID: "cur12345", Updated: time.Now(),
		Conv: []proxy.Message{{Role: "user", Content: agent.StrPtr("hi")}}}); err != nil {
		t.Fatal(err)
	}
	out := agent.SessionListText("cur12345")
	if !strings.Contains(out, "★") {
		t.Errorf("current session should be starred; got %q", out)
	}
	// Empty dir → friendly message.
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())
	if agent.SessionListText("x") != "no saved sessions yet" {
		t.Errorf("empty dir should report no sessions")
	}
}

func TestPrintSessionsEmptyAndPopulated(t *testing.T) {
	empty := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", empty)
	var buf bytes.Buffer
	agent.PrintSessions(&buf)
	if !strings.Contains(buf.String(), "no saved sessions") {
		t.Errorf("empty dir output: %q", buf.String())
	}

	t.Setenv("WAKIL_SESSIONS_DIR", filepath.Join(empty, "sub"))
	_ = agent.WriteSession(&agent.Session{ChatID: "id123456", Updated: time.Now(),
		Conv: []proxy.Message{{Role: "user", Content: agent.StrPtr("the task")}}})
	buf.Reset()
	agent.PrintSessions(&buf)
	if !strings.Contains(buf.String(), "id12345") || !strings.Contains(buf.String(), "the task") {
		t.Errorf("populated output should list the session; got %q", buf.String())
	}
}

func TestSessionLabel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)

	s := &agent.Session{
		ChatID:  "lab11111-0000-0000-0000-000000000000",
		Model:   "ilm",
		Label:   "my refactor",
		Updated: time.Now(),
		Conv:    []proxy.Message{{Role: "user", Content: agent.StrPtr("fix auth")}},
	}
	if err := agent.WriteSession(s); err != nil {
		t.Fatal(err)
	}

	// Label round-trips through disk.
	got, err := agent.LoadSession("lab1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != "my refactor" {
		t.Errorf("label = %q, want %q", got.Label, "my refactor")
	}

	// Label appears in SessionListText output.
	out := agent.SessionListText("")
	if !strings.Contains(out, "my refactor") {
		t.Errorf("SessionListText should show label; got %q", out)
	}

	// Label appears in PrintSessions output.
	var buf bytes.Buffer
	agent.PrintSessions(&buf)
	if !strings.Contains(buf.String(), "my refactor") {
		t.Errorf("PrintSessions should show label; got %q", buf.String())
	}
}

func TestSessionWorkflowPersistence(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)

	app := &agent.App{
		Cfg:    config.Config{ExecMode: "direct", WorkDir: "/tmp/wftest"},
		Client: &proxy.Client{ChatID: "wf111111-0000-0000-0000-000000000000", Model: "ilm"},
		Session: &agent.Session{
			ChatID: "wf111111-0000-0000-0000-000000000000",
			Model:  "ilm",
		},
		Conv: []proxy.Message{{Role: "user", Content: agent.StrPtr("plan task")}},
	}

	// Simulate an active mid-plan workflow state.
	app.Workflow = &workflow.WorkflowState{
		Task:      "plan task",
		Phase:     workflow.WFImplement,
		StepCount: 2,
		StepIdx:   1,
		PlanPath:  "/tmp/wftest/.wakil/plan.md",
	}
	app.SaveSession()

	got, err := agent.LoadSession("wf1")
	if err != nil {
		t.Fatal(err)
	}
	if got.SavedWorkflow == nil {
		t.Fatal("SavedWorkflow should be set after saving an active workflow")
	}
	if got.SavedWorkflow.Task != "plan task" {
		t.Errorf("task = %q, want %q", got.SavedWorkflow.Task, "plan task")
	}
	if got.SavedWorkflow.StepCount != 2 || got.SavedWorkflow.StepIdx != 1 {
		t.Errorf("steps = %d/%d, want 1/2", got.SavedWorkflow.StepIdx, got.SavedWorkflow.StepCount)
	}

	// Saving with no active workflow clears SavedWorkflow.
	app.Workflow = nil
	app.SaveSession()
	got2, err := agent.LoadSession("wf1")
	if err != nil {
		t.Fatal(err)
	}
	if got2.SavedWorkflow != nil {
		t.Errorf("SavedWorkflow should be nil when workflow is nil; got %+v", got2.SavedWorkflow)
	}
}

func TestIsMCPReadTool(t *testing.T) {
	reads := []string{"search", "fetch_url", "list_dir", "resolve-library-id", "query-docs"}
	writes := []string{"write_file", "create_issue", "delete_item", "send_message", "update_record", "post_comment"}
	for _, n := range reads {
		if !agent.IsMCPReadTool(n) {
			t.Errorf("%q should be read-only", n)
		}
	}
	for _, n := range writes {
		if agent.IsMCPReadTool(n) {
			t.Errorf("%q should be treated as a write", n)
		}
	}
}

func TestMCPToolToOpenAI(t *testing.T) {
	tl := agent.MCPToolToOpenAI("srv", &gosdkmcp.Tool{Name: "do_thing", Description: "does a thing"})
	if tl.Function.Name != "srv__do_thing" {
		t.Errorf("name = %q, want srv__do_thing", tl.Function.Name)
	}
	if !strings.Contains(tl.Function.Description, "[srv]") || !strings.Contains(tl.Function.Description, "does a thing") {
		t.Errorf("description should be namespaced; got %q", tl.Function.Description)
	}
	if tl.Type != "function" {
		t.Errorf("type = %q", tl.Type)
	}
}

func TestExtractMCPResult(t *testing.T) {
	ok := &gosdkmcp.CallToolResult{Content: []gosdkmcp.Content{
		&gosdkmcp.TextContent{Text: "line1"},
		&gosdkmcp.TextContent{Text: "line2"},
	}}
	if got := agent.ExtractMCPResult(ok); got != "line1\nline2" {
		t.Errorf("joined text = %q", got)
	}

	errRes := &gosdkmcp.CallToolResult{IsError: true, Content: []gosdkmcp.Content{
		&gosdkmcp.TextContent{Text: "boom"},
	}}
	if got := agent.ExtractMCPResult(errRes); got != "ERROR: boom" {
		t.Errorf("error result = %q, want 'ERROR: boom'", got)
	}

	empty := &gosdkmcp.CallToolResult{}
	if got := agent.ExtractMCPResult(empty); got != "(no output)" {
		t.Errorf("empty result = %q, want '(no output)'", got)
	}
}

func TestPrettyArgs(t *testing.T) {
	out := agent.PrettyArgs(`{"a":1,"b":"two"}`)
	if !strings.Contains(out, `"a"`) || !strings.Contains(out, `"two"`) {
		t.Errorf("prettyArgs should pretty-print JSON; got %q", out)
	}
	// Non-JSON falls back to the raw string.
	if agent.PrettyArgs("not json") != "not json" {
		t.Errorf("non-JSON should pass through unchanged")
	}
}

// --- main helpers ---

func TestLoadAgentPromptFallbackAndFile(t *testing.T) {
	// Missing path → built-in fallback.
	if got := loadAgentPrompt(config.Config{AgentPromptPath: ""}); got != defaultAgentPrompt {
		t.Errorf("empty path should yield the built-in fallback")
	}
	if got := loadAgentPrompt(config.Config{AgentPromptPath: "/no/such/file.txt"}); got != defaultAgentPrompt {
		t.Errorf("unreadable path should yield the built-in fallback")
	}
	// Real file → its contents (trailing newline trimmed).
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.txt")
	if err := os.WriteFile(p, []byte("custom instructions\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := loadAgentPrompt(config.Config{AgentPromptPath: p}); got != "custom instructions" {
		t.Errorf("loadAgentPrompt = %q, want %q", got, "custom instructions")
	}
}

func TestBuildToolsComposition(t *testing.T) {
	// OPENROUTER_API_KEY must not leak from the host env into the test — if it
	// is set, counsel tools appear unconditionally and the "no key" sub-test
	// below fails (P0-5 in the improvement plan).
	t.Setenv("OPENROUTER_API_KEY", "")

	base := agent.BuildTools(config.Config{}, "/work", nil)
	baseCount := len(base)
	if baseCount == 0 {
		t.Fatal("expected default tools")
	}
	// SearXNG adds its two tools.
	withSearch := agent.BuildTools(config.Config{SearXngURL: "http://localhost:8888"}, "/work", nil)
	if len(withSearch) != baseCount+2 {
		t.Errorf("searxng should add 2 tools; got %d vs base %d", len(withSearch), baseCount)
	}
	// Counsel tools require both the flag and the key env; flag alone adds nothing.
	withOracleNoKey := agent.BuildTools(config.Config{OracleEnabled: true, OracleAPIKeyEnv: "WAKIL_TEST_NO_SUCH_KEY"}, "/work", nil)
	if len(withOracleNoKey) != baseCount {
		t.Errorf("counsel tools must not appear without the API key set; got %d", len(withOracleNoKey))
	}
}
