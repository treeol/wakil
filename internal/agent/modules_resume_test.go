package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// newModuleApp builds an app with the context-modules switch on and a real
// registry, using the same shape as the modules tests.
func newModuleApp() *App {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	tools, reg := BuildToolsWithModules(app.Cfg, "/work", nil, "")
	if reg == nil {
		return app
	}
	app.Tools = tools
	app.Modules = reg
	return app
}

// TestLoadedModulesSurviveResume: modules loaded in a session are saved
// with it and re-delivered on resume — IsLoaded true, the synthetic
// load_module pair on the first turn, and the tool array unchanged.
func TestLoadedModulesSurviveResume(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())
	app := newModuleApp()
	if app.Modules == nil {
		t.Fatal("registry nil with switch on")
	}
	// Load two modules through the same path the agent uses.
	for _, name := range []string{"staging", "edit-extras"} {
		if _, _, err := app.Modules.LoadModule(name); err != nil {
			t.Fatalf("LoadModule(%q): %v", name, err)
		}
	}
	app.Session = &Session{ChatID: "module-resume-0001", Model: "ilm"}
	// SaveSession snapshots the live Conv, not Session.Conv.
	app.Conv = []proxy.Message{{Role: "user", Content: StrPtr("hi")}}
	app.curTurn = 1
	// Surface write errors directly (SaveSession swallows them into a.Out).
	if names := app.Modules.LoadedNames(); len(names) != 2 {
		t.Fatalf("LoadedNames probe = %v, want 2", names)
	}
	var captured strings.Builder
	app.Out = &captured
	app.SaveSession()
	if captured.Len() != 0 {
		t.Fatalf("SaveSession warning: %s", captured.String())
	}

	loaded, err := LoadSession("module-resume-0001")
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(loaded.LoadedModules) != 2 {
		t.Fatalf("saved LoadedModules = %v, want 2", loaded.LoadedModules)
	}

	// Resume into a fresh app with a fresh registry.
	app2 := newModuleApp()
	app2.InstallSession(loaded)
	if app2.Modules.IsLoaded("staging") || app2.Modules.IsLoaded("edit-extras") {
		t.Fatal("modules must not be loaded before the first turn")
	}
	// First-turn pair delivers them.
	pair := app2.mcpPreloadPair("hi")
	if len(pair) == 0 {
		t.Fatal("no resume pair delivered")
	}
	if !app2.Modules.IsLoaded("staging") || !app2.Modules.IsLoaded("edit-extras") {
		t.Fatalf("IsLoaded after pair: staging=%v edit-extras=%v",
			app2.Modules.IsLoaded("staging"), app2.Modules.IsLoaded("edit-extras"))
	}
	// Pair shape: assistant load_module call + pinned tool result per module.
	toolCalls := 0
	var toolResults []string
	for _, m := range pair {
		if m.Role == "assistant" {
			toolCalls += len(m.ToolCalls)
		}
		if m.Role == "tool" && m.Name == "load_module" {
			toolResults = append(toolResults, *m.Content)
		}
	}
	if toolCalls != 2 || len(toolResults) != 2 {
		t.Fatalf("pair tool calls=%d results=%d, want 2/2", toolCalls, len(toolResults))
	}
	joined := strings.Join(toolResults, "\n")
	if !strings.Contains(joined, "staging") || !strings.Contains(joined, "edit-extras") {
		t.Fatalf("schemas block missing modules: %s", joined)
	}
	// The tool array is unchanged (cache rule).
	if len(app2.Tools) != len(app.Tools) {
		t.Fatalf("tool array changed: %d vs %d", len(app2.Tools), len(app.Tools))
	}
	// No notice when every saved module exists.
	if note := app2.takeModuleAnnouncement(); note != "" {
		t.Fatalf("unexpected announcement: %q", note)
	}
	// The saved JSON carries the field.
	raw, err := os.ReadFile(filepath.Join(os.Getenv("WAKIL_SESSIONS_DIR"), "module-resume-0001.json"))
	if err != nil {
		t.Fatalf("read saved session: %v", err)
	}
	var probe map[string]any
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatalf("saved JSON: %v", err)
	}
	if _, ok := probe["loaded_modules"]; !ok {
		t.Errorf("saved JSON missing loaded_modules field")
	}
}

// TestResumeMissingModuleNoticed: a saved module that no longer exists is
// named in one announcement, and the turn proceeds.
func TestResumeMissingModuleNoticed(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())
	app := newModuleApp()
	if app.Modules == nil {
		t.Fatal("registry nil")
	}
	app.Session = &Session{ChatID: "module-resume-0002", Model: "ilm", LoadedModules: []string{"mcp:gone", "staging"}}
	app.Conv = []proxy.Message{{Role: "user", Content: StrPtr("hi")}}
	app.curTurn = 1
	app.SaveSession()

	loaded, err := LoadSession("module-resume-0002")
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	app2 := newModuleApp()
	app2.InstallSession(loaded)
	_ = app2.mcpPreloadPair("hi")
	if !app2.Modules.IsLoaded("staging") {
		t.Fatal("existing module not delivered")
	}
	if app2.Modules.IsLoaded("mcp:gone") {
		t.Fatal("missing module must not be loaded")
	}
	note := app2.takeModuleAnnouncement()
	if !strings.Contains(note, "mcp:gone") {
		t.Fatalf("announcement should name the missing module, got %q", note)
	}
	if strings.Count(note, "mcp:gone") != 1 {
		t.Fatalf("announcement should name it once, got %q", note)
	}
}

// TestOldSessionResumesWithoutModules: a v0.8.10-shaped session (no
// loaded_modules) resumes with nothing loaded and no notice.
func TestOldSessionResumesWithoutModules(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WAKIL_SESSIONS_DIR", dir)
	// Write a v0.8.10 JSON by hand (no loaded_modules field).
	body := `{"chat_id":"old-session-0001","model":"ilm","created":"2026-11-20T00:00:00Z","updated":"2026-11-20T00:00:00Z","conv":[{"role":"user","content":"hi"}]}`
	if err := os.WriteFile(filepath.Join(dir, "old-session-0001.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSession("old-session-0001")
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(s.LoadedModules) != 0 {
		t.Fatalf("old session LoadedModules = %v, want empty", s.LoadedModules)
	}
	app := newModuleApp()
	app.InstallSession(s)
	if pair := app.mcpPreloadPair("hi"); len(pair) != 0 {
		t.Fatalf("old session: no pair expected, got %d messages", len(pair))
	}
	if note := app.takeModuleAnnouncement(); note != "" {
		t.Fatalf("old session: no notice expected, got %q", note)
	}
}

// resumedHistoryWithSchema builds the transcript a live session leaves behind
// after the model called load_module(name): user, assistant call, the
// (originally pinned) schema result, assistant answer.
func resumedHistoryWithSchema(t *testing.T, reg *ModuleRegistry, name string) []proxy.Message {
	t.Helper()
	block, ok := reg.ModuleSchemasBlock(name)
	if !ok {
		t.Fatalf("no schema block for %q", name)
	}
	return []proxy.Message{
		{Role: "user", Content: StrPtr("please stage this"), Turn: 1},
		{Role: "assistant", Turn: 1, ToolCalls: []proxy.ToolCall{{ID: "call-1", Type: "function",
			Function: proxy.FunctionCall{Name: "load_module", Arguments: `{"name":"` + name + `"}`}}}},
		// Pinned is json:"-", so a session read back from disk has it false.
		{Role: "tool", ToolCallID: "call-1", Name: "load_module", Content: StrPtr(block), Turn: 1},
		{Role: "assistant", Content: StrPtr("loaded"), Turn: 1},
	}
}

// countSchemaMessages counts load_module result messages for a module in conv.
func countSchemaMessages(conv []proxy.Message, name string) int {
	n := 0
	for _, m := range conv {
		if got, ok := moduleSchemaName(m); ok && got == name {
			n++
		}
	}
	return n
}

// A resumed session whose history already holds the original load_module
// result must not receive the schema a second time: each schema reaches the
// model once, and the surviving copy is pinned again.
func TestResumeNoDuplicateModuleSchema(t *testing.T) {
	app := newModuleApp()
	hist := resumedHistoryWithSchema(t, app.Modules, "staging")
	app.InstallSession(&Session{ChatID: "resume-dup-0001", Conv: hist, LoadedModules: []string{"staging"}})

	pair := app.mcpPreloadPair("next question")
	if len(pair) != 0 {
		t.Fatalf("pair has %d messages; the history already holds the staging schema", len(pair))
	}
	if !app.Modules.IsLoaded("staging") {
		t.Error("staging must still be marked loaded (call_tool gate)")
	}
	if n := countSchemaMessages(app.Conv, "staging"); n != 1 {
		t.Errorf("staging schema appears %d times in Conv, want 1", n)
	}
	// The kept copy is pinned again (Pinned is not persisted).
	for _, m := range app.Conv {
		if got, ok := moduleSchemaName(m); ok && got == "staging" && !m.Pinned {
			t.Error("restored staging schema is not re-pinned; eviction could drop it")
		}
	}
}

// A module saved as loaded but with NO schema in the history (the history was
// compacted away, or a v0.8.11-only fresh pre-load) still gets its pair.
func TestResumeDeliversMissingModuleSchema(t *testing.T) {
	app := newModuleApp()
	app.InstallSession(&Session{ChatID: "resume-dup-0002",
		Conv:          []proxy.Message{{Role: "user", Content: StrPtr("hi"), Turn: 1}},
		LoadedModules: []string{"staging"}})
	pair := app.mcpPreloadPair("hi")
	if len(pair) != 2 {
		t.Fatalf("pair has %d messages, want assistant call + tool result", len(pair))
	}
}

// A schema that changed since the session was saved is re-delivered; the stale
// copy is unpinned so eviction may drop it, leaving one pinned schema.
func TestResumeChangedModuleSchemaRedelivered(t *testing.T) {
	app := newModuleApp()
	hist := resumedHistoryWithSchema(t, app.Modules, "staging")
	stale := *hist[2].Content + "\n(old version of the schema)"
	hist[2].Content = &stale
	app.InstallSession(&Session{ChatID: "resume-dup-0003", Conv: hist, LoadedModules: []string{"staging"}})

	pair := app.mcpPreloadPair("hi")
	if len(pair) != 2 {
		t.Fatalf("changed schema: pair has %d messages, want 2 (re-delivered)", len(pair))
	}
	for _, m := range app.Conv {
		if got, ok := moduleSchemaName(m); ok && got == "staging" && m.Pinned {
			t.Error("the stale schema copy must stay unpinned")
		}
	}
}

// With the switch off nothing is re-pinned and nothing is skipped: behaviour
// is exactly v0.8.10's.
func TestResumeModulesOffUnchanged(t *testing.T) {
	reg := newModuleApp().Modules
	hist := resumedHistoryWithSchema(t, reg, "staging")
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	if app.Modules != nil {
		t.Fatal("test app must have the switch off")
	}
	app.InstallSession(&Session{ChatID: "resume-dup-0004", Conv: hist, LoadedModules: []string{"staging"}})
	for i, m := range app.Conv {
		if m.Pinned {
			t.Errorf("message %d pinned with modules off", i)
		}
	}
	if pair := app.mcpPreloadPair("hi"); len(pair) != 0 {
		t.Errorf("modules off: pair has %d messages, want none", len(pair))
	}
}

// Only the LAST schema result per module is re-pinned; an earlier duplicate
// stays unpinned so eviction can drop it.
func TestResumeRepinsOnlyLatestSchemaPerModule(t *testing.T) {
	app := newModuleApp()
	hist := resumedHistoryWithSchema(t, app.Modules, "staging")
	second := append([]proxy.Message(nil), hist...)
	second[1].ToolCalls = []proxy.ToolCall{{ID: "call-2", Type: "function", Function: proxy.FunctionCall{Name: "load_module", Arguments: `{"name":"staging"}`}}}
	second[2].ToolCallID = "call-2"
	conv := append(append([]proxy.Message(nil), hist...), second...)
	app.InstallSession(&Session{ChatID: "resume-dup-0005", Conv: conv, LoadedModules: []string{"staging"}})
	pinned := 0
	for _, m := range app.Conv {
		if got, ok := moduleSchemaName(m); ok && got == "staging" && m.Pinned {
			pinned++
		}
	}
	if pinned != 1 {
		t.Errorf("%d pinned staging schemas, want exactly 1 (the latest)", pinned)
	}
}
