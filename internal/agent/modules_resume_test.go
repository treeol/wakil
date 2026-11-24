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
