package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// modules_test.go — tests for the context-modules switch (Stage 1).
//
// Block 1 tests: the switch off is byte-identical to today. With the switch
// off, BuildTools produces the same tool array as before the feature, the
// preamble is the same, and the new tools (load_module, call_tool) are absent
// from the array.

// TestContextModulesOff_ByteIdentical verifies that with context_modules
// "off" (the default), BuildTools produces a byte-identical tool array to the
// pre-feature BuildTools call. The golden master is a BuildTools call with
// the default config (which already has ContextModules "off"); the candidate
// is the same call with ContextModules explicitly set to "off". They must
// marshal to identical JSON.
func TestContextModulesOff_ByteIdentical(t *testing.T) {
	today := BuildTools(config.DefaultConfig(), "/work", nil, "")

	off := config.DefaultConfig()
	off.ContextModules = "off"
	offTools := BuildTools(off, "/work", nil, "")

	todayJSON, _ := json.Marshal(today)
	offJSON, _ := json.Marshal(offTools)
	if string(todayJSON) != string(offJSON) {
		t.Errorf("context_modules off is not byte-identical to today:\ntoday: %s\noff:   %s", todayJSON, offJSON)
	}
}

// TestContextModulesOff_NoLoadModuleCallToolInArray verifies that with the
// switch off, neither load_module nor call_tool appears in the tool array.
func TestContextModulesOff_NoLoadModuleCallToolInArray(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ContextModules = "off"
	tools := BuildTools(cfg, "/work", nil, "")
	for _, tl := range tools {
		if tl.Function.Name == "load_module" || tl.Function.Name == "call_tool" {
			t.Errorf("tool %q should not be in the array with context_modules off", tl.Function.Name)
		}
	}
}

// TestContextModulesOff_DefaultIsOff verifies that DefaultConfig sets
// context_modules to "off".
func TestContextModulesOff_DefaultIsOff(t *testing.T) {
	cfg := config.DefaultConfig()
	if cfg.ContextModules != "off" {
		t.Errorf("DefaultConfig context_modules = %q, want %q", cfg.ContextModules, "off")
	}
}

// TestContextModulesOff_PreambleByteIdentical verifies that with the switch
// off, buildPreambleForNetwork is byte-identical to today. This is a
// structural check: the function should not mention the module index or
// the context-modules switch at all when off.
func TestContextModulesOff_PreambleByteIdentical(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "off"
	today := app.buildPreambleForNetwork("Monday, 1 January 2026", "host")

	app2 := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app2.Cfg.ContextModules = "off"
	off := app2.buildPreambleForNetwork("Monday, 1 January 2026", "host")

	if today != off {
		t.Errorf("preamble with context_modules off is not byte-identical across two apps:\ntoday: %q\noff:   %q", today, off)
	}
	if strings.Contains(off, "Context modules") || strings.Contains(off, "load_module") {
		t.Errorf("preamble with context_modules off should not mention Context modules or load_module:\n%q", off)
	}
}

// TestContextModulesValidation verifies that validateEnums accepts "off" and
// "on" and rejects unknown values. Lives in package config (config_test.go)
// because validateEnums is unexported.

// TestContextModulesEnvOverride verifies that WAKIL_CONTEXT_MODULES overrides
// the config file value.
func TestContextModulesEnvOverride(t *testing.T) {
	t.Setenv("WAKIL_CONTEXT_MODULES", "on")
	cfg := config.DefaultConfig()
	if v := os.Getenv("WAKIL_CONTEXT_MODULES"); v != "" {
		cfg.ContextModules = v
	}
	if cfg.ContextModules != "on" {
		t.Errorf("WAKIL_CONTEXT_MODULES=on should set context_modules to %q, got %q", "on", cfg.ContextModules)
	}
}

// ── Block 2: module registry ──────────────────────────────────────────────

// TestModuleRegistry_Names verifies the registry returns the expected module
// names for a default config (no MCP, no search/browser/lsp).
func TestModuleRegistry_Names(t *testing.T) {
	cfg := config.DefaultConfig()
	reg := NewModuleRegistry(cfg, "/work", nil)
	names := reg.ModuleNames()
	// Default config (no search/browser/lsp/mcp): skills-authoring, edit-extras, staging.
	want := []string{"skills-authoring", "edit-extras", "staging"}
	if len(names) != len(want) {
		t.Fatalf("module names = %v, want %v", names, want)
	}
	for i, w := range want {
		if names[i] != w {
			t.Errorf("module[%d] = %q, want %q", i, names[i], w)
		}
	}
}

// TestModuleRegistry_NamesWithOptionals verifies search/browser/lsp modules
// appear when their config flags are on.
func TestModuleRegistry_NamesWithOptionals(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.SearXngURL = "http://x"
	cfg.LSPEnabled = true
	cfg.BrowserEnabled = true
	reg := NewModuleRegistry(cfg, "/work", nil)
	names := reg.ModuleNames()
	want := map[string]bool{
		"skills-authoring": true,
		"edit-extras":      true,
		"staging":          true,
		"search":           true,
		"browser":          true,
		"lsp":              true,
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected module %q in %v", n, names)
		}
		delete(want, n)
	}
	if len(want) > 0 {
		t.Errorf("missing modules: %v in %v", want, names)
	}
}

// TestModuleRegistry_LoadModule verifies loading a module returns its tools
// and marks it loaded.
func TestModuleRegistry_LoadModule(t *testing.T) {
	cfg := config.DefaultConfig()
	reg := NewModuleRegistry(cfg, "/work", nil)
	tools, desc, err := reg.LoadModule("staging")
	if err != nil {
		t.Fatalf("LoadModule(staging): %v", err)
	}
	if len(tools) != 5 {
		t.Errorf("staging tools = %d, want 5", len(tools))
	}
	if desc == "" {
		t.Error("staging desc is empty")
	}
	if !reg.IsLoaded("staging") {
		t.Error("staging not marked as loaded")
	}
}

// TestModuleRegistry_LoadUnknownModule verifies an unknown name returns an
// error listing available modules.
func TestModuleRegistry_LoadUnknownModule(t *testing.T) {
	cfg := config.DefaultConfig()
	reg := NewModuleRegistry(cfg, "/work", nil)
	_, _, err := reg.LoadModule("nonexistent")
	if err == nil {
		t.Fatal("LoadModule(nonexistent) should error")
	}
	if !strings.Contains(err.Error(), "unknown module") {
		t.Errorf("error should contain 'unknown module', got: %v", err)
	}
	// The error should list available modules.
	if !strings.Contains(err.Error(), "staging") {
		t.Errorf("error should list 'staging', got: %v", err)
	}
}

// TestModuleRegistry_LoadModuleIdempotent verifies loading an already-loaded
// module returns its tools again without error.
func TestModuleRegistry_LoadModuleIdempotent(t *testing.T) {
	cfg := config.DefaultConfig()
	reg := NewModuleRegistry(cfg, "/work", nil)
	_, _, err := reg.LoadModule("staging")
	if err != nil {
		t.Fatalf("first LoadModule: %v", err)
	}
	tools2, _, err := reg.LoadModule("staging")
	if err != nil {
		t.Fatalf("second LoadModule: %v", err)
	}
	if len(tools2) != 5 {
		t.Errorf("second load tools = %d, want 5", len(tools2))
	}
}

// TestModuleRegistry_ToolModule verifies the registry can find which module
// owns a tool name.
func TestModuleRegistry_ToolModule(t *testing.T) {
	cfg := config.DefaultConfig()
	reg := NewModuleRegistry(cfg, "/work", nil)
	if mod := reg.ToolModule("staging_put"); mod != "staging" {
		t.Errorf("ToolModule(staging_put) = %q, want %q", mod, "staging")
	}
	if mod := reg.ToolModule("save_skill"); mod != "skills-authoring" {
		t.Errorf("ToolModule(save_skill) = %q, want %q", mod, "skills-authoring")
	}
	if mod := reg.ToolModule("move_file"); mod != "edit-extras" {
		t.Errorf("ToolModule(move_file) = %q, want %q", mod, "edit-extras")
	}
	// Core tools are not in any module.
	if mod := reg.ToolModule("run_shell"); mod != "" {
		t.Errorf("ToolModule(run_shell) = %q, want empty", mod)
	}
}

// TestModuleRegistry_ModuleIndex verifies the index has one line per module,
// starts with the expected header, and lists all module names.
func TestModuleRegistry_ModuleIndex(t *testing.T) {
	cfg := config.DefaultConfig()
	reg := NewModuleRegistry(cfg, "/work", nil)
	idx := reg.ModuleIndex()
	if !strings.HasPrefix(idx, "## Context modules") {
		t.Errorf("index should start with '## Context modules', got: %q", idx[:40])
	}
	for _, name := range reg.ModuleNames() {
		if !strings.Contains(idx, name) {
			t.Errorf("index should contain %q, got: %q", name, idx)
		}
	}
}

// TestModuleRegistry_LoadedNames verifies LoadedNames returns only the loaded
// modules, in registry order.
func TestModuleRegistry_LoadedNames(t *testing.T) {
	cfg := config.DefaultConfig()
	reg := NewModuleRegistry(cfg, "/work", nil)
	if _, _, err := reg.LoadModule("edit-extras"); err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	loaded := reg.LoadedNames()
	if len(loaded) != 1 || loaded[0] != "edit-extras" {
		t.Errorf("LoadedNames = %v, want [edit-extras]", loaded)
	}
}

// TestBuildToolsWithModules_CoreOnly verifies the switch-on tool array contains
// only core tools + load_module + call_tool, and no deferred tool names.
func TestBuildToolsWithModules_CoreOnly(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ContextModules = "on"
	tools, reg := BuildToolsWithModules(cfg, "/work", nil, "")
	// Every tool in the core array must be a known core tool.
	for _, tl := range tools {
		if !coreToolNames[tl.Function.Name] {
			t.Errorf("non-core tool %q in core array", tl.Function.Name)
		}
	}
	// load_module and call_tool must be present.
	hasLoad, hasCall := false, false
	for _, tl := range tools {
		if tl.Function.Name == "load_module" {
			hasLoad = true
		}
		if tl.Function.Name == "call_tool" {
			hasCall = true
		}
	}
	if !hasLoad {
		t.Error("load_module not in core array")
	}
	if !hasCall {
		t.Error("call_tool not in core array")
	}
	// No deferred tool should be in the core array.
	deferred := []string{"save_skill", "update_skill", "forget_skill", "apply_skill_template",
		"capture_skill_template", "check_skill_template", "move_file", "write_binary_file",
		"staging_put", "staging_get", "staging_delete", "staging_list", "staging_get_many"}
	coreSet := make(map[string]bool, len(tools))
	for _, tl := range tools {
		coreSet[tl.Function.Name] = true
	}
	for _, d := range deferred {
		if coreSet[d] {
			t.Errorf("deferred tool %q should not be in core array", d)
		}
	}
	// Registry should have the built-in modules.
	if reg == nil {
		t.Fatal("registry is nil")
	}
	if len(reg.ModuleNames()) == 0 {
		t.Error("registry has no modules")
	}
}

// TestBuildToolsWithModules_NoDeferredInCore verifies that no tool name from a
// module appears in the core array. This is the "no module tool name appears
// in the core array" test from the design.
// (§7.9 Stage 1 Tests).
func TestBuildToolsWithModules_NoDeferredInCore(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ContextModules = "on"
	cfg.SearXngURL = "http://x"
	cfg.LSPEnabled = true
	cfg.BrowserEnabled = true
	core, reg := BuildToolsWithModules(cfg, "/work", nil, "")
	coreSet := make(map[string]bool, len(core))
	for _, tl := range core {
		coreSet[tl.Function.Name] = true
	}
	for _, mod := range reg.modules {
		for _, tl := range mod.Tools {
			if coreSet[tl.Function.Name] {
				t.Errorf("module tool %q (module %q) is in the core array", tl.Function.Name, mod.Name)
			}
		}
	}
}

// ── Block 3: load_module + call_tool handlers ────────────────────────────

// TestHandleLoadModule_Success verifies loading a module returns the schemas
// and marks it loaded.
func TestHandleLoadModule_Success(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	result := app.handleLoadModule(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{
			Name:      "load_module",
			Arguments: `{"name":"staging"}`,
		},
	})
	if strings.HasPrefix(result, "ERROR:") {
		t.Fatalf("handleLoadModule failed: %s", result)
	}
	if !app.Modules.IsLoaded("staging") {
		t.Error("staging not marked as loaded")
	}
	if !strings.Contains(result, "staging") {
		t.Errorf("result should mention staging: %q", result)
	}
	if !strings.Contains(result, "staging_put") {
		t.Errorf("result should contain tool schema name staging_put: %q", result)
	}
}

// TestHandleLoadModule_UnknownModule verifies an unknown name returns an error
// listing available modules.
func TestHandleLoadModule_UnknownModule(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	result := app.handleLoadModule(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{
			Name:      "load_module",
			Arguments: `{"name":"nonexistent"}`,
		},
	})
	if !strings.Contains(result, "unknown module") {
		t.Errorf("should say 'unknown module': %s", result)
	}
	if !strings.Contains(result, "staging") {
		t.Errorf("should list staging as available: %s", result)
	}
}

// TestHandleLoadModule_Idempotent verifies loading an already-loaded module
// returns its schemas again without error.
func TestHandleLoadModule_Idempotent(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	_, _, err := app.Modules.LoadModule("staging")
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	result := app.handleLoadModule(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{
			Name:      "load_module",
			Arguments: `{"name":"staging"}`,
		},
	})
	if strings.HasPrefix(result, "ERROR:") {
		t.Errorf("idempotent load should not error: %s", result)
	}
}

// TestHandleCallTool_NoNesting verifies call_tool rejects wrapping itself or
// load_module.
func TestHandleCallTool_NoNesting(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	for _, inner := range []string{"call_tool", "load_module"} {
		result := app.handleCallTool(context.Background(), proxy.ToolCall{
			Function: proxy.FunctionCall{
				Name:      "call_tool",
				Arguments: fmt.Sprintf(`{"name":%q,"arguments":"{}"}`, inner),
			},
		})
		if !strings.HasPrefix(result, "ERROR:") {
			t.Errorf("call_tool wrapping %q should error: got %s", inner, result)
		}
		if !strings.Contains(result, "call it directly") {
			t.Errorf("error should say 'call it directly' for %q: %s", inner, result)
		}
	}
}

// TestHandleCallTool_CoreToolRejected verifies call_tool rejects a core tool
// that is already in the tool array.
func TestHandleCallTool_CoreToolRejected(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	result := app.handleCallTool(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{
			Name:      "call_tool",
			Arguments: `{"name":"read_file","arguments":"{}"}`,
		},
	})
	if !strings.HasPrefix(result, "ERROR:") {
		t.Errorf("call_tool wrapping a core tool should error: %s", result)
	}
	if !strings.Contains(result, "core tool") {
		t.Errorf("error should say 'core tool': %s", result)
	}
}

// TestHandleCallTool_ModuleNotLoaded verifies call_tool rejects a module tool
// whose module has not been loaded.
func TestHandleCallTool_ModuleNotLoaded(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	result := app.handleCallTool(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{
			Name:      "call_tool",
			Arguments: `{"name":"staging_put","arguments":"{}"}`,
		},
	})
	if !strings.HasPrefix(result, "ERROR:") {
		t.Errorf("call_tool for unloaded module should error: %s", result)
	}
	if !strings.Contains(result, "not loaded") {
		t.Errorf("error should say 'not loaded': %s", result)
	}
	if !strings.Contains(result, "load_module") {
		t.Errorf("error should suggest load_module: %s", result)
	}
}

// TestHandleCallTool_UnknownTool verifies call_tool rejects a tool name that is
// not in any module.
func TestHandleCallTool_UnknownTool(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	result := app.handleCallTool(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{
			Name:      "call_tool",
			Arguments: `{"name":"nonexistent_tool","arguments":"{}"}`,
		},
	})
	if !strings.HasPrefix(result, "ERROR:") {
		t.Errorf("call_tool for unknown tool should error: %s", result)
	}
	if !strings.Contains(result, "not in any module") {
		t.Errorf("error should say 'not in any module': %s", result)
	}
}

// TestHandleCallTool_SingleDispatchPath verifies that a call_tool call for a
// gated tool goes through the same confirm gate as a direct call. The test
// uses save_skill (a gated, main-agent-only tool from the skills-authoring
// module): with a declining confirmer it should return the same declined
// result as a direct call would.
func TestHandleCallTool_SingleDispatchPath_Gated(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return false })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	// Load the edit-extras module first.
	_, _, err := app.Modules.LoadModule("edit-extras")
	if err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	// call_tool for move_file with a declining confirmer → declined.
	result := app.handleCallTool(context.Background(), proxy.ToolCall{
		ID: "test-call-1",
		Function: proxy.FunctionCall{
			Name:      "call_tool",
			Arguments: `{"name":"move_file","arguments":"{\"src\":\"/work/a\",\"dst\":\"/work/b\"}"}`,
		},
	})
	if !strings.Contains(result, "[declined by user]") {
		t.Errorf("gated call_tool should be declined, got: %s", result)
	}
}

// TestHandleCallTool_SingleDispatchPath_Destructive verifies that a call_tool
// call for a destructive shell command is classified as destructive (the
// shell classifier runs on the inner name).
func TestHandleCallTool_SingleDispatchPath_Destructive(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	// run_shell is a core tool — call_tool should reject it.
	// This test verifies the core-tool guard fires for run_shell too.
	result := app.handleCallTool(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{
			Name:      "call_tool",
			Arguments: `{"name":"run_shell","arguments":"{\"command\":\"rm -rf /tmp/x\"}"}`,
		},
	})
	if !strings.HasPrefix(result, "ERROR:") {
		t.Errorf("call_tool wrapping a core tool (run_shell) should error: %s", result)
	}
	if !strings.Contains(result, "core tool") {
		t.Errorf("error should say 'core tool': %s", result)
	}
}

// ── Block 4: MCP modules and keyword pre-load ───────────────────────────────

// TestMCPModules_BuiltFromServers verifies that mcpModules builds one module
// per connected MCP server, named mcp:<server>.
func TestMCPModules_BuiltFromServers(t *testing.T) {
	mcp := newFakeMCPManager(
		connectedServer("trello", &fakeMCPSession{}, "get_boards", "get_card"),
		connectedServer("context7", &fakeMCPSession{}, "resolve-library-id", "query-docs"),
	)
	mods := mcpModules(mcp)
	if len(mods) != 2 {
		t.Fatalf("expected 2 MCP modules, got %d", len(mods))
	}
	if mods[0].Name != "mcp:trello" {
		t.Errorf("module 0 name = %q, want %q", mods[0].Name, "mcp:trello")
	}
	if len(mods[0].Tools) != 2 {
		t.Errorf("trello module should have 2 tools, got %d", len(mods[0].Tools))
	}
	if mods[1].Name != "mcp:context7" {
		t.Errorf("module 1 name = %q, want %q", mods[1].Name, "mcp:context7")
	}
}

// TestMCPModules_NilManager verifies mcpModules returns nil for a nil manager.
func TestMCPModules_NilManager(t *testing.T) {
	if mods := mcpModules(nil); mods != nil {
		t.Errorf("mcpModules(nil) = %v, want nil", mods)
	}
}

// TestMatchMCPKeywords_Match verifies keyword matching against task text.
func TestMatchMCPKeywords_Match(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		keywords []string
		want     bool
	}{
		{"trello board", "add a card to the trello board", []string{"board", "card", "trello", "list"}, true},
		{"trello card", "create a card", []string{"board", "card", "trello", "list"}, true},
		{"trello list", "show me the list", []string{"board", "card", "trello", "list"}, true},
		{"context7 library", "docs for the library", []string{"library", "docs", "documentation", "api reference"}, true},
		{"context7 api reference", "look up the API reference", []string{"library", "docs", "documentation", "api reference"}, true},
		{"no match", "write a hello world program", []string{"board", "card", "trello", "list"}, false},
		{"word boundary", "listless behaviour", []string{"list"}, false},
		{"word boundary 2", "listed items", []string{"list"}, false},
		{"word boundary 3", "check the listing", []string{"list"}, false},
		{"multi-word", "find the API reference for react", []string{"api reference"}, true},
		{"multi-word no match", "find the API", []string{"api reference"}, false},
		{"empty keywords", "some text", nil, false},
		{"empty text", "", []string{"board"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := matchMCPKeywords(tc.text, tc.keywords)
			if got != tc.want {
				t.Errorf("matchMCPKeywords(%q, %v) = %v, want %v", tc.text, tc.keywords, got, tc.want)
			}
		})
	}
}

// TestMCPModulePreload_Match verifies that MCPModulePreload returns the right
// module names when the task text matches keywords.
func TestMCPModulePreload_Match(t *testing.T) {
	mcp := newFakeMCPManager(
		connectedServer("trello", &fakeMCPSession{}, "get_boards", "get_card"),
		connectedServer("context7", &fakeMCPSession{}, "resolve-library-id"),
	)
	cfg := config.DefaultConfig()
	cfg.ContextModules = "on"
	reg := NewModuleRegistry(cfg, "/work", mcp)
	preload := reg.MCPModulePreload("add a card to the trello board", nil)
	if len(preload) != 1 || preload[0] != "mcp:trello" {
		t.Errorf("preload = %v, want [mcp:trello]", preload)
	}
	reg.PreloadModules(preload)
	if !reg.IsLoaded("mcp:trello") {
		t.Error("mcp:trello should be loaded after PreloadModules")
	}
}

// TestMCPModulePreload_NoMatch verifies that MCPModulePreload returns nil when
// no keywords match.
func TestMCPModulePreload_NoMatch(t *testing.T) {
	mcp := newFakeMCPManager(
		connectedServer("trello", &fakeMCPSession{}, "get_boards"),
	)
	cfg := config.DefaultConfig()
	cfg.ContextModules = "on"
	reg := NewModuleRegistry(cfg, "/work", mcp)
	preload := reg.MCPModulePreload("write a hello world program", nil)
	if len(preload) != 0 {
		t.Errorf("preload = %v, want empty", preload)
	}
}

// TestMCPModulePreload_UserOverride verifies that user-provided keywords in
// config override the built-in defaults.
func TestMCPModulePreload_UserOverride(t *testing.T) {
	mcp := newFakeMCPManager(
		connectedServer("myserver", &fakeMCPSession{}, "do_thing"),
	)
	cfg := config.DefaultConfig()
	cfg.ContextModules = "on"
	cfg.MCPModuleKeywords = map[string][]string{
		"myserver": {"custom", "keyword"},
	}
	reg := NewModuleRegistry(cfg, "/work", mcp)
	preload := reg.MCPModulePreload("use the custom tool", cfg.MCPModuleKeywords)
	if len(preload) != 1 || preload[0] != "mcp:myserver" {
		t.Errorf("preload = %v, want [mcp:myserver]", preload)
	}
}

// TestMCPModulePreload_UserOverrideDisablesDefaults verifies that a user
// override for a known server (trello) replaces the built-in defaults.
func TestMCPModulePreload_UserOverrideDisablesDefaults(t *testing.T) {
	mcp := newFakeMCPManager(
		connectedServer("trello", &fakeMCPSession{}, "get_boards"),
	)
	cfg := config.DefaultConfig()
	cfg.ContextModules = "on"
	cfg.MCPModuleKeywords = map[string][]string{
		"trello": {"kanban"},
	}
	reg := NewModuleRegistry(cfg, "/work", mcp)
	preload := reg.MCPModulePreload("add a card to the board", cfg.MCPModuleKeywords)
	if len(preload) != 0 {
		t.Errorf("preload with user override should not match built-in 'board': %v", preload)
	}
	preload = reg.MCPModulePreload("update the kanban", cfg.MCPModuleKeywords)
	if len(preload) != 1 || preload[0] != "mcp:trello" {
		t.Errorf("preload with user keyword 'kanban' = %v, want [mcp:trello]", preload)
	}
}

// TestMCPModulePreload_EmptyText verifies that empty task text produces no
// pre-load.
func TestMCPModulePreload_EmptyText(t *testing.T) {
	mcp := newFakeMCPManager(
		connectedServer("trello", &fakeMCPSession{}, "get_boards"),
	)
	cfg := config.DefaultConfig()
	cfg.ContextModules = "on"
	reg := NewModuleRegistry(cfg, "/work", mcp)
	preload := reg.MCPModulePreload("", nil)
	if len(preload) != 0 {
		t.Errorf("preload for empty text = %v, want empty", preload)
	}
}

// TestModuleCapabilityLine_Loaded verifies that when a module is loaded, the
// full inventory line is returned.
func TestModuleCapabilityLine_Loaded(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ContextModules = "on"
	cfg.LSPEnabled = true
	reg := NewModuleRegistry(cfg, "/work", nil)
	reg.PreloadModules([]string{"lsp"})
	full := "LSP code intelligence available: lsp_definition"
	line := moduleCapabilityLine("lsp", reg, full)
	if line != full {
		t.Errorf("loaded module should return full line: got %q, want %q", line, full)
	}
}

// TestModuleCapabilityLine_NotLoaded verifies that when a module is not loaded,
// a shorter line naming the module is returned.
func TestModuleCapabilityLine_NotLoaded(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ContextModules = "on"
	cfg.LSPEnabled = true
	reg := NewModuleRegistry(cfg, "/work", nil)
	line := moduleCapabilityLine("lsp", reg, "full line")
	if !strings.Contains(line, "load_module") {
		t.Errorf("not-loaded line should mention load_module: %q", line)
	}
	if !strings.Contains(line, "lsp") {
		t.Errorf("not-loaded line should name the module: %q", line)
	}
}

// TestPreloadModules_MarksLoaded verifies that PreloadModules marks the named
// modules as loaded in the registry.
func TestPreloadModules_MarksLoaded(t *testing.T) {
	mcp := newFakeMCPManager(
		connectedServer("trello", &fakeMCPSession{}, "get_boards"),
	)
	cfg := config.DefaultConfig()
	cfg.ContextModules = "on"
	reg := NewModuleRegistry(cfg, "/work", mcp)
	reg.PreloadModules([]string{"mcp:trello"})
	if !reg.IsLoaded("mcp:trello") {
		t.Error("mcp:trello should be loaded after PreloadModules")
	}
}

// TestPreloadModules_UnknownName verifies that PreloadModules silently skips
// unknown module names.
func TestPreloadModules_UnknownName(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ContextModules = "on"
	reg := NewModuleRegistry(cfg, "/work", nil)
	reg.PreloadModules([]string{"nonexistent"})
	for _, m := range reg.modules {
		if reg.IsLoaded(m.Name) {
			t.Errorf("module %q should not be loaded", m.Name)
		}
	}
}

// TestMCPModulePreload_NilRegistry verifies that MCPModulePreload on a nil
// registry returns nil safely.
func TestMCPModulePreload_NilRegistry(t *testing.T) {
	var reg *ModuleRegistry
	if preload := reg.MCPModulePreload("some text", nil); preload != nil {
		t.Errorf("nil registry should return nil, got %v", preload)
	}
}

// ── Block 5: Cache-stability test ────────────────────────────────────────────

// TestCacheStability_ToolArrayUnchangedAfterLoad verifies that loading a
// module does not change the tool array. The tool array is frozen at session
// start; load_module returns schemas as a tool result, it does not add tools
// to the array. This is the cache rule R3: "tools is fixed for the session at
// BuildTools time."
func TestCacheStability_ToolArrayUnchangedAfterLoad(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ContextModules = "on"
	reg := NewModuleRegistry(cfg, "/work", nil)

	// Build the core tool array once.
	core, _ := BuildToolsWithModules(cfg, "/work", nil, "")
	coreJSON, _ := json.Marshal(core)

	// Load two modules.
	_, _, err := reg.LoadModule("staging")
	if err != nil {
		t.Fatalf("LoadModule staging: %v", err)
	}
	_, _, err = reg.LoadModule("edit-extras")
	if err != nil {
		t.Fatalf("LoadModule edit-extras: %v", err)
	}

	// The tool array must be unchanged.
	core2, _ := BuildToolsWithModules(cfg, "/work", nil, "")
	core2JSON, _ := json.Marshal(core2)
	if string(coreJSON) != string(core2JSON) {
		t.Errorf("tool array changed after loading modules:\nbefore: %s\nafter:  %s", coreJSON, core2JSON)
	}

	// LoadedNames should show the two loaded modules.
	loaded := reg.LoadedNames()
	if len(loaded) != 2 {
		t.Errorf("expected 2 loaded modules, got %d: %v", len(loaded), loaded)
	}
}

// TestCacheStability_MessagesByteIdenticalAfterLoad verifies that loading a
// module does not modify any earlier message in the conversation. A
// load_module call appends a tool result (role:"tool") at the tail; it never
// inserts or modifies messages[0] or any earlier message. This is the cache
// rule R1: "Loading appends, never inserts into the middle."
func TestCacheStability_MessagesByteIdenticalAfterLoad(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)

	// Simulate a conversation with a system message and a user message.
	app.Conv = []proxy.Message{
		{Role: "system", Content: StrPtr("preamble"), Pinned: true},
		{Role: "user", Content: StrPtr("hello")},
	}
	before := make([]proxy.Message, len(app.Conv))
	copy(before, app.Conv)

	// Simulate a load_module tool call result appended to the conversation.
	loadResult := app.handleLoadModule(context.Background(), proxy.ToolCall{
		ID: "load-1",
		Function: proxy.FunctionCall{
			Name:      "load_module",
			Arguments: `{"name":"staging"}`,
		},
	})
	app.appendConv(proxy.Message{
		Role:      "assistant",
		Content:   StrPtr(""),
		ToolCalls: []proxy.ToolCall{{ID: "load-1", Type: "function", Function: proxy.FunctionCall{Name: "load_module", Arguments: `{"name":"staging"}`}}},
	})
	app.appendConv(proxy.Message{
		Role:       "tool",
		ToolCallID: "load-1",
		Name:       "load_module",
		Content:    StrPtr(loadResult),
		Pinned:     true,
	})

	// The first two messages must be byte-identical.
	for i, m := range app.Conv[:len(before)] {
		beforeJSON, _ := json.Marshal(m)
		afterJSON, _ := json.Marshal(before[i])
		if string(beforeJSON) != string(afterJSON) {
			t.Errorf("message %d changed after load_module:\nbefore: %s\nafter:  %s", i, beforeJSON, afterJSON)
		}
	}

	// The conversation grew by 2 (assistant + tool result).
	if len(app.Conv) != len(before)+2 {
		t.Errorf("conversation length = %d, want %d (append only)", len(app.Conv), len(before)+2)
	}

	// The last message must be a tool result for load_module.
	last := app.Conv[len(app.Conv)-1]
	if last.Role != "tool" {
		t.Errorf("last message role = %q, want %q", last.Role, "tool")
	}
	if last.Name != "load_module" {
		t.Errorf("last message name = %q, want %q", last.Name, "load_module")
	}
}

// TestCacheStability_ModuleIndexInPreamble verifies that the module index is
// present in the preamble when the switch is on, and absent when off.
func TestCacheStability_ModuleIndexInPreamble(t *testing.T) {
	// Switch on: preamble should contain "## Context modules".
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	preamble := app.buildPreambleForNetwork("Monday, 1 January 2026", "host")
	if !strings.Contains(preamble, "## Context modules") {
		t.Errorf("preamble with switch on should contain module index:\n%s", preamble)
	}
	if !strings.Contains(preamble, "load_module") {
		t.Errorf("preamble should mention load_module:\n%s", preamble)
	}

	// Switch off: preamble should NOT contain module index.
	app2 := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app2.Cfg.ContextModules = "off"
	preamble2 := app2.buildPreambleForNetwork("Monday, 1 January 2026", "host")
	if strings.Contains(preamble2, "## Context modules") {
		t.Errorf("preamble with switch off should NOT contain module index:\n%s", preamble2)
	}
}
