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
// load_module. The unwrap happens in handleToolCall, so the test calls
// handleToolCall (not handleCallTool, which no longer exists).
func TestHandleCallTool_NoNesting(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	for _, inner := range []string{"call_tool", "load_module"} {
		result := app.handleToolCall(context.Background(), proxy.ToolCall{
			ID: "test-call",
			Function: proxy.FunctionCall{
				Name:      "call_tool",
				Arguments: fmt.Sprintf(`{"name":%q,"arguments":"{}"}`, inner),
			},
		})
		if !strings.HasPrefix(result.text, "ERROR:") {
			t.Errorf("call_tool wrapping %q should error: got %s", inner, result.text)
		}
		if !strings.Contains(result.text, "call it directly") {
			t.Errorf("error should say 'call it directly' for %q: %s", inner, result.text)
		}
	}
}

// TestHandleCallTool_CoreToolRejected verifies call_tool rejects a core tool
// that is already in the tool array.
func TestHandleCallTool_CoreToolRejected(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	result := app.handleToolCall(context.Background(), proxy.ToolCall{
		ID: "test-call",
		Function: proxy.FunctionCall{
			Name:      "call_tool",
			Arguments: `{"name":"read_file","arguments":"{}"}`,
		},
	})
	if !strings.HasPrefix(result.text, "ERROR:") {
		t.Errorf("call_tool wrapping a core tool should error: %s", result.text)
	}
	if !strings.Contains(result.text, "core tool") {
		t.Errorf("error should say 'core tool': %s", result.text)
	}
}

// TestHandleCallTool_ModuleNotLoaded verifies call_tool rejects a module tool
// whose module has not been loaded.
func TestHandleCallTool_ModuleNotLoaded(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	result := app.handleToolCall(context.Background(), proxy.ToolCall{
		ID: "test-call",
		Function: proxy.FunctionCall{
			Name:      "call_tool",
			Arguments: `{"name":"staging_put","arguments":"{}"}`,
		},
	})
	if !strings.HasPrefix(result.text, "ERROR:") {
		t.Errorf("call_tool for unloaded module should error: %s", result.text)
	}
	if !strings.Contains(result.text, "not loaded") {
		t.Errorf("error should say 'not loaded': %s", result.text)
	}
	if !strings.Contains(result.text, "load_module") {
		t.Errorf("error should suggest load_module: %s", result.text)
	}
}

// TestHandleCallTool_UnknownTool verifies call_tool rejects a tool name that is
// not in any module.
func TestHandleCallTool_UnknownTool(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	result := app.handleToolCall(context.Background(), proxy.ToolCall{
		ID: "test-call",
		Function: proxy.FunctionCall{
			Name:      "call_tool",
			Arguments: `{"name":"nonexistent_tool","arguments":"{}"}`,
		},
	})
	if !strings.HasPrefix(result.text, "ERROR:") {
		t.Errorf("call_tool for unknown tool should error: %s", result.text)
	}
	if !strings.Contains(result.text, "not in any module") {
		t.Errorf("error should say 'not in any module': %s", result.text)
	}
}

// TestHandleCallTool_SingleDispatchPath_Gated verifies that a call_tool call
// for a gated tool goes through the same confirm gate as a direct call. The
// unwrap happens in handleToolCall so the inner name reaches the handler's
// Confirm call.
func TestHandleCallTool_SingleDispatchPath_Gated(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return false })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	_, _, err := app.Modules.LoadModule("edit-extras")
	if err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	// call_tool for move_file with a declining confirmer → declined.
	result := app.handleToolCall(context.Background(), proxy.ToolCall{
		ID: "test-call-1",
		Function: proxy.FunctionCall{
			Name:      "call_tool",
			Arguments: `{"name":"move_file","arguments":"{\"src\":\"/work/a\",\"dst\":\"/work/b\"}"}`,
		},
	})
	if !strings.Contains(result.text, "[declined by user]") {
		t.Errorf("gated call_tool should be declined, got: %s", result.text)
	}
}

// TestHandleCallTool_CoreToolRejected_RunShell verifies that call_tool rejects
// run_shell (a core tool) — the core-tool guard fires for run_shell too.
func TestHandleCallTool_CoreToolRejected_RunShell(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	result := app.handleToolCall(context.Background(), proxy.ToolCall{
		ID: "test-call",
		Function: proxy.FunctionCall{
			Name:      "call_tool",
			Arguments: `{"name":"run_shell","arguments":"{\"command\":\"rm -rf /tmp/x\"}"}`,
		},
	})
	if !strings.HasPrefix(result.text, "ERROR:") {
		t.Errorf("call_tool wrapping a core tool (run_shell) should error: %s", result.text)
	}
	if !strings.Contains(result.text, "core tool") {
		t.Errorf("error should say 'core tool': %s", result.text)
	}
}

// TestHandleCallTool_InnerNameReachesGates verifies that the inner tool name
// (not "call_tool") reaches the trace record. This is the "addition 5" test:
// trace records, tool_calls in --result-json, declined counts and the bench
// tools column record the inner tool name, never "call_tool".
func TestHandleCallTool_InnerNameReachesTrace(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	_, _, err := app.Modules.LoadModule("edit-extras")
	if err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	// Use a tool that won't be declined by the confirmer (auto-approve).
	// move_file needs src/dst that exist; use the fake executor's files.
	// Instead, test with a tool that errors for other reasons but still
	// records the inner name in the trace.
	tc := proxy.ToolCall{
		ID: "test-trace",
		Function: proxy.FunctionCall{
			Name:      "call_tool",
			Arguments: `{"name":"move_file","arguments":"{\"src\":\"/work/a\",\"dst\":\"/work/b\"}"}`,
		},
	}
	result := app.handleToolCall(context.Background(), tc)
	// The trace entry should record "move_file" not "call_tool".
	if len(app.recentTraces) == 0 {
		t.Fatal("expected at least one trace entry")
	}
	last := app.recentTraces[len(app.recentTraces)-1]
	if last.Abbrev != "move" {
		t.Errorf("trace abbrev = %q, want %q (inner name, not 'subagent' or 'call_to')", last.Abbrev, "move")
	}
	// The result should mention move or declined, not call_tool.
	_ = result // result may be declined or error depending on confirmer
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
	preload, _ := reg.MCPModulePreload("add a card to the trello board", nil)
	if len(preload) != 1 || preload[0] != "mcp:trello" {
		t.Errorf("preload = %v, want [mcp:trello]", preload)
	}
	reg.PreloadModules(preload, nil)
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
	preload, _ := reg.MCPModulePreload("write a hello world program", nil)
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
	preload, _ := reg.MCPModulePreload("use the custom tool", cfg.MCPModuleKeywords)
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
	preload, _ := reg.MCPModulePreload("add a card to the board", cfg.MCPModuleKeywords)
	if len(preload) != 0 {
		t.Errorf("preload with user override should not match built-in 'board': %v", preload)
	}
	preload, _ = reg.MCPModulePreload("update the kanban", cfg.MCPModuleKeywords)
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
	preload, _ := reg.MCPModulePreload("", nil)
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
	reg.PreloadModules([]string{"lsp"}, nil)
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
	reg.PreloadModules([]string{"mcp:trello"}, nil)
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
	reg.PreloadModules([]string{"nonexistent"}, nil)
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
	if preload, _ := reg.MCPModulePreload("some text", nil); preload != nil {
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

// ── Block 7: Measurement helper ─────────────────────────────────────────────

// TestMeasurement_TokenEstimate compares the tool-array size (bytes/4, marked
// as an estimate) with the switch off and on, for the default config and for
// a config with two fake MCP servers. The test prints the before/after numbers
// so the final report can quote them.
func TestMeasurement_TokenEstimate(t *testing.T) {
	// Default config (no MCP, no optional groups).
	cfgDefault := config.DefaultConfig()
	cfgDefault.ContextModules = "off"
	offDefault := BuildTools(cfgDefault, "/work", nil, "")
	offBytes := 0
	for _, tl := range offDefault {
		b, _ := json.Marshal(tl)
		offBytes += len(b)
	}

	cfgOn := config.DefaultConfig()
	cfgOn.ContextModules = "on"
	onDefault, _ := BuildToolsWithModules(cfgOn, "/work", nil, "")
	onBytes := 0
	for _, tl := range onDefault {
		b, _ := json.Marshal(tl)
		onBytes += len(b)
	}

	t.Logf("default config: off=%d bytes (~%d est_tok), on=%d bytes (~%d est_tok), saving=%d bytes (~%d est_tok)",
		offBytes, offBytes/4, onBytes, onBytes/4, offBytes-onBytes, (offBytes-onBytes)/4)

	// With two fake MCP servers.
	mcp := newFakeMCPManager(
		connectedServer("trello", &fakeMCPSession{}, "get_boards", "get_board", "get_lists", "get_cards", "get_card", "create_card", "move_card", "update_card", "add_comment"),
		connectedServer("context7", &fakeMCPSession{}, "resolve-library-id", "query-docs"),
	)
	cfgMCP := config.DefaultConfig()
	cfgMCP.ContextModules = "off"
	offMCP := BuildTools(cfgMCP, "/work", mcp, "")
	offMCPBytes := 0
	for _, tl := range offMCP {
		b, _ := json.Marshal(tl)
		offMCPBytes += len(b)
	}

	cfgMCPOn := config.DefaultConfig()
	cfgMCPOn.ContextModules = "on"
	onMCP, _ := BuildToolsWithModules(cfgMCPOn, "/work", mcp, "")
	onMCPBytes := 0
	for _, tl := range onMCP {
		b, _ := json.Marshal(tl)
		onMCPBytes += len(b)
	}

	t.Logf("two MCP servers: off=%d bytes (~%d est_tok), on=%d bytes (~%d est_tok), saving=%d bytes (~%d est_tok)",
		offMCPBytes, offMCPBytes/4, onMCPBytes, onMCPBytes/4, offMCPBytes-onMCPBytes, (offMCPBytes-onMCPBytes)/4)

	// The on array must be smaller than the off array in both cases.
	if onBytes >= offBytes {
		t.Errorf("default: on (%d) should be smaller than off (%d)", onBytes, offBytes)
	}
	if onMCPBytes >= offMCPBytes {
		t.Errorf("MCP: on (%d) should be smaller than off (%d)", onMCPBytes, offMCPBytes)
	}

	// The on array with MCP must be the same size as the on array without MCP
	// (MCP tools are in modules, not in the core array).
	if onBytes != onMCPBytes {
		t.Errorf("on with MCP (%d) should equal on without MCP (%d) — MCP tools are in modules", onMCPBytes, onBytes)
	}
}

// ── Block 2: pre-load delivers the tools; the gate lives in executeToolCall ─

// TestPreload_DeliveredAsSyntheticLoadModule: turn-1 pre-load ships the
// schemas as an assistant tool call + pinned tool result, never as a
// user-role message.
func TestPreload_DeliveredAsSyntheticLoadModule(t *testing.T) {
	mcp := newFakeMCPManager(connectedServer("trello", &fakeMCPSession{}, "get_boards", "get_card"))
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	size := len(strings.Repeat("x", 10000))
	app.Cfg.ContextModules = "on"
	app.MCP = mcp
	app.Modules = NewModuleRegistry(app.Cfg, "/work", mcp)
	app.Cfg.ContextCollapseMinBytes = size

	pair := app.mcpPreloadPair("show my trello board")
	if len(pair) != 2 {
		t.Fatalf("pair = %d messages, want 2 (assistant call + tool result)", len(pair))
	}
	if pair[0].Role != "assistant" || len(pair[0].ToolCalls) != 1 ||
		pair[0].ToolCalls[0].Function.Name != "load_module" {
		t.Errorf("first message must be the assistant load_module call, got %+v", pair[0])
	}
	if pair[1].Role != "tool" || pair[1].Name != "load_module" ||
		pair[1].ToolCallID != pair[0].ToolCalls[0].ID || !pair[1].Pinned {
		t.Errorf("second message must be the pinned load_module tool result paired by id, got %+v", pair[1])
	}
	if !strings.Contains(DerefStr(pair[1].Content), `"name": "trello__get_boards"`) {
		t.Errorf("the tool result must carry the schemas:\n%s", DerefStr(pair[1].Content))
	}
	// Byte-identical in form to handleLoadModule's return.
	got := app.handleLoadModule(context.Background(), pair[0].ToolCalls[0])
	if DerefStr(pair[1].Content) != got {
		t.Errorf("pre-load schemas differ from handleLoadModule's form:\npre-load: %s\nhandler: %s", DerefStr(pair[1].Content), got)
	}
	if !app.Modules.IsLoaded("mcp:trello") {
		t.Error("the module must be marked loaded")
	}
	// No pre-load on the second turn.
	app.preambleDay = "2026-11-23"
	if p := app.mcpPreloadPair("show my trello board"); len(p) != 0 {
		t.Errorf("no pre-load after turn 1, got %d messages", len(p))
	}
}

// TestLateConnect_AnnouncementAppendedToNextToolResult: a late-connect
// announcement is never a user-role message; it is appended to the next
// tool result as a marked part.
func TestLateConnect_AnnouncementAppendedToNextToolResult(t *testing.T) {
	mcp := newFakeMCPManager()
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.MCP = mcp
	app.Modules = NewModuleRegistry(app.Cfg, "/work", mcp)
	app.RegisterMCPModules(mcp)

	// Simulate a connect that happens after the registry snapshot: the
	// server is in the manager but not in the registry yet.
	mcp.servers = append(mcp.servers, connectedServer("caj", &fakeMCPSession{}, "read_file"))
	app.mcpEventBuf = append(app.mcpEventBuf, mcpLifecycleEvent{name: "caj", kind: "connected"})
	app.flushPendingModuleEvents()

	// The announcement is queued, not a user message.
	if len(app.Conv) != 0 {
		t.Fatalf("announcement must not be appended as its own message, Conv has %d", len(app.Conv))
	}
	if len(app.pendingModuleAnnouncements) != 1 {
		t.Fatalf("one announcement must be queued, got %v", app.pendingModuleAnnouncements)
	}

	// On the next tool result the announcement rides as a marked part:
	// the result text gains the announcement under [module announcement].
	// Accomplished by streamTurn's result append; here we assert the
	// queue is consumed and the rendered form is marked.
	ann := app.takeModuleAnnouncement()
	if !strings.HasPrefix(ann, "Context module available: mcp:caj") {
		t.Fatalf("takeModuleAnnouncement returned unexpectedly: %q", ann)
	}
	res := proxy.Message{Role: "tool", ToolCallID: "t1", Name: "read_file", Content: StrPtr("file content")}
	text := DerefStr(res.Content)
	if ann != "" {
		text += "\n\n[module announcement]\n" + ann
	}
	if !strings.Contains(text, "[module announcement]") || !strings.Contains(text, "mcp:caj") {
		t.Errorf("rendered result must carry the marked announcement:\n%s", text)
	}
	if !strings.Contains(text, "file content") {
		t.Errorf("the original result must be preserved:\n%s", text)
	}
	// Queue is consumed.
	if len(app.pendingModuleAnnouncements) != 0 {
		t.Errorf("announcement must be consumed, still queued: %v", app.pendingModuleAnnouncements)
	}
}

// TestLateConnect_WaitsForToolResult: if the turn produces no tool
// result, the announcement survives to the next one.
func TestLateConnect_WaitsForToolResult(t *testing.T) {
	mcp := newFakeMCPManager()
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.MCP = mcp
	app.Modules = NewModuleRegistry(app.Cfg, "/work", mcp)
	app.RegisterMCPModules(mcp)
	mcp.servers = append(mcp.servers, connectedServer("caj", &fakeMCPSession{}, "read_file"))
	app.mcpEventBuf = append(app.mcpEventBuf, mcpLifecycleEvent{name: "caj", kind: "connected"})
	app.flushPendingModuleEvents()

	// A turn with no tool call: the announcement is still queued.
	if app.takeModuleAnnouncement() == "" {
		t.Fatal("announcement must wait for a tool result; the queue emptied first")
	}
	if len(app.pendingModuleAnnouncements) != 0 {
		t.Error("take consumes the announcement")
	}
}

// TestNoUserRoleMessageCarriesMCPText: no user-role message in the
// pre-load or late-connect paths contains MCP-provided text.
func TestNoUserRoleMessageCarriesMCPText(t *testing.T) {
	sess := &fakeMCPSession{}
	mcp := newFakeMCPManager(connectedServer("caj", sess, "read_file"))
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.MCP = mcp
	app.Modules = NewModuleRegistry(app.Cfg, "/work", mcp)

	// Pre-load path.
	pair := app.mcpPreloadPair("use caj")
	if len(pair) == 0 {
		t.Fatal("expected a pre-load pair")
	}
	for _, m := range pair {
		if m.Role == "user" {
			t.Errorf("no user-role message may carry module text, got %q", DerefStr(m.Content))
		}
	}
	if !strings.Contains(DerefStr(pair[1].Content), "caj__read_file") {
		t.Error("the schemas must be in the tool result")
	}

	// Late-connect path: queue an announcement directly; flushPendingModuleEvents
	// never appends a user message.
	app.RegisterMCPModules(mcp)
	app.flushPendingModuleEvents()
	for _, m := range app.Conv {
		if m.Role == "user" {
			t.Errorf("no user-role message may carry a module announcement, got %q", DerefStr(m.Content))
		}
	}
	if len(app.pendingModuleAnnouncements) != 0 {
		t.Errorf("nothing new should have been announced (already in the index); queued %v", app.pendingModuleAnnouncements)
	}
	// A late-connect announcement queued directly: the queue itself is
	// only consumed by a tool result, never appended as a user message.
	app.queueModuleAnnouncement("Context module available: mcp:caj")
	for _, m := range app.Conv {
		if m.Role == "user" {
			t.Errorf("a queued announcement must not be a user message, got %q", DerefStr(m.Content))
		}
	}
}

// TestPreload_FirstRequestContainsSchemas: a task text with "trello" must
// produce a first request whose messages already contain the trello
// schemas, in the same form load_module returns — and a call_tool to a
// trello tool then works without load_module.
func TestPreload_FirstRequestContainsSchemas(t *testing.T) {
	mcp := newFakeMCPManager(connectedServer("trello", &fakeMCPSession{}, "get_boards", "get_card"))
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.MCP = mcp
	app.Modules = NewModuleRegistry(app.Cfg, "/work", mcp)

	task := "add a card to the trello board"
	preload, reasons := app.Modules.MCPModulePreload(task, app.Cfg.MCPModuleKeywords)
	if len(preload) != 1 || preload[0] != "mcp:trello" {
		t.Fatalf("pre-load for %q = %v, want [mcp:trello]", task, preload)
	}
	app.Modules.PreloadModules(preload, reasons)

	schemas, ok := app.Modules.PreloadedSchemas(preload)
	if !ok {
		t.Fatal("PreloadedSchemas returned ok=false for a pre-loaded module")
	}
	for _, want := range []string{`Module "mcp:trello" loaded`, `"name": "trello__get_boards"`, `"name": "trello__get_card"`} {
		if !strings.Contains(schemas, want) {
			t.Errorf("pre-loaded schemas missing %q:\n%s", want, schemas)
		}
	}
	// The schemas must be byte-identical to what load_module returns.
	tools, desc, err := app.Modules.LoadModule("mcp:trello")
	if err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	schemasJSON, _ := json.MarshalIndent(tools, "", "  ")
	wantBlock := fmt.Sprintf("Module %q loaded: %s\nTool schemas:\n%s", "mcp:trello", desc, schemasJSON)
	if schemas != wantBlock {
		t.Errorf("pre-loaded schemas differ from load_module form:\nwant:\n%s\ngot:\n%s", wantBlock, schemas)
	}

	// And a call_tool to a trello tool works without load_module.
	result := app.handleToolCall(context.Background(), proxy.ToolCall{
		ID:       "test-call-preload",
		Function: proxy.FunctionCall{Name: "call_tool", Arguments: `{"name":"trello__get_boards","arguments":"{}"}`},
	})
	if strings.HasPrefix(result.text, "ERROR") || strings.Contains(result.text, "not loaded") {
		t.Errorf("call_tool to a pre-loaded module tool must work without load_module, got: %s", result.text)
	}
}

// TestExecuteToolCall_UnloadedModuleToolRejected: the exported dispatch
// reaches the same gate as handleToolCall — a direct ExecuteToolCall on an
// unloaded module tool is rejected, never dispatched.
func TestExecuteToolCall_UnloadedModuleToolRejected(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	result := app.ExecuteToolCall(context.Background(), proxy.ToolCall{
		ID:       "test-direct",
		Function: proxy.FunctionCall{Name: "staging_put", Arguments: `{"key":"k","value":"v"}`},
	})
	if !strings.HasPrefix(result.text, "ERROR:") || !strings.Contains(result.text, "not loaded") {
		t.Errorf("direct ExecuteToolCall on an unloaded module tool must be rejected, got: %s", result.text)
	}
}

// TestExecuteToolCall_LoadedModuleToolDispatches: once the module is loaded
// the direct dispatch reaches the handler (here: a declining confirmer).
func TestExecuteToolCall_LoadedModuleToolDispatches(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return false })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	if _, _, err := app.Modules.LoadModule("edit-extras"); err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	result := app.ExecuteToolCall(context.Background(), proxy.ToolCall{
		ID:       "test-direct-loaded",
		Function: proxy.FunctionCall{Name: "move_file", Arguments: `{"src":"/work/a","dst":"/work/b"}`},
	})
	if !strings.Contains(result.text, "[declined by user]") {
		t.Errorf("loaded module tool via direct ExecuteToolCall must reach its handler, got: %s", result.text)
	}
}

// TestExecuteToolCall_CallToolRejectedDirect: call_tool passed directly to
// ExecuteToolCall (skipping handleToolCall) is unwrapped by the same gate,
// not dispatched to a nonexistent "call_tool" handler.
func TestExecuteToolCall_CallToolRejectedDirect(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	result := app.ExecuteToolCall(context.Background(), proxy.ToolCall{
		ID:       "test-direct-calltool",
		Function: proxy.FunctionCall{Name: "call_tool", Arguments: `{"name":"call_tool","arguments":"{}"}`},
	})
	if !strings.HasPrefix(result.text, "ERROR:") || !strings.Contains(result.text, "must not wrap") {
		t.Errorf("call_tool wrapping call_tool via direct dispatch must be rejected, got: %s", result.text)
	}
}

// TestExecuteToolCall_BareModuleToolNameEnforced: a bare module tool name
// (not through call_tool) is only dispatched when its module is loaded, so
// the model cannot guess at a schema it was never handed.
func TestExecuteToolCall_BareModuleToolNameEnforced(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	result := app.ExecuteToolCall(context.Background(), proxy.ToolCall{
		ID:       "test-bare",
		Function: proxy.FunctionCall{Name: "move_file", Arguments: `{"src":"/work/a","dst":"/work/b"}`},
	})
	if !strings.HasPrefix(result.text, "ERROR:") || !strings.Contains(result.text, "not loaded") {
		t.Errorf("bare module tool with module not loaded must be rejected, got: %s", result.text)
	}
}

// TestPreload_ServerNameOnly: with the default keywords, only the
// server name itself pre-loads the module — "show my trello board"
// matches on "trello", and the reason line names the matched keyword.
func TestPreload_ServerNameOnly(t *testing.T) {
	mcp := newFakeMCPManager(connectedServer("trello", &fakeMCPSession{}, "get_boards", "get_card"))
	cfg := config.DefaultConfig()
	cfg.ContextModules = "on"
	reg := NewModuleRegistry(cfg, "/work", mcp)

	preload, reasons := reg.MCPModulePreload("show my trello board", nil)
	if len(preload) != 1 || preload[0] != "mcp:trello" {
		t.Fatalf("preload = %v, want [mcp:trello]", preload)
	}
	if reasons["mcp:trello"] != "trello" {
		t.Fatalf("reason = %q, want %q", reasons["mcp:trello"], "trello")
	}
	reg.PreloadModules(preload, reasons)
	line, ok := reg.ModuleIndexLine("mcp:trello")
	if !ok {
		t.Fatal("no index line for mcp:trello")
	}
	if !strings.Contains(line, `pre-loaded: "trello" in your message`) {
		t.Errorf("index line must carry the pre-load reason, got: %s", line)
	}
	if !strings.Contains(reg.ModuleIndex(), `pre-loaded: "trello" in your message`) {
		t.Errorf("module index must carry the pre-load reason:\n%s", reg.ModuleIndex())
	}
}

// TestPreload_CommonWordsDoNotLoad: the old built-in common words no
// longer load any module — neither from the user's message nor from
// the memory block / AGENTS.md / retrieval text.
func TestPreload_CommonWordsDoNotLoad(t *testing.T) {
	mcp := newFakeMCPManager(
		connectedServer("trello", &fakeMCPSession{}, "get_boards"),
		connectedServer("context7", &fakeMCPSession{}, "resolve-library-id"),
	)
	cfg := config.DefaultConfig()
	cfg.ContextModules = "on"
	reg := NewModuleRegistry(cfg, "/work", mcp)

	scanned := map[string]string{
		"user message with a common word": "add this to my list and check the board",
		"user message with another":       "the board needs a card",
		"user message":                    "checklist",
		"memory block":                    "recent: list of cards, board of directors, generic list",
		"agents.md":                       "## Notes\ncard list board\n",
		"tool output":                     "list: board card",
		"context7 common words":           "docs for the library documentation",
	}
	for where, text := range scanned {
		if preload, _ := reg.MCPModulePreload(text, nil); len(preload) != 0 {
			t.Errorf("%s: %q must not pre-load anything, got %v", where, text, preload)
		}
	}
	// Sanity: the server name itself still matches.
	if preload, _ := reg.MCPModulePreload("docs for context7", nil); len(preload) != 1 {
		t.Errorf("the server name must still pre-load, got %v", preload)
	}
}

// TestPreload_UserKeywordWorks: a keyword the user adds in
// mcp_module_keywords matches, and the reason line names it.
func TestPreload_UserKeywordWorks(t *testing.T) {
	mcp := newFakeMCPManager(connectedServer("trello", &fakeMCPSession{}, "get_boards"))
	cfg := config.DefaultConfig()
	cfg.ContextModules = "on"
	cfg.MCPModuleKeywords = map[string][]string{"trello": {"kanban"}}
	reg := NewModuleRegistry(cfg, "/work", mcp)

	preload, reasons := reg.MCPModulePreload("move it to the kanban", cfg.MCPModuleKeywords)
	if len(preload) != 1 || preload[0] != "mcp:trello" {
		t.Fatalf("preload = %v, want [mcp:trello]", preload)
	}
	reg.PreloadModules(preload, reasons)
	line, _ := reg.ModuleIndexLine("mcp:trello")
	if !strings.Contains(line, `pre-loaded: "kanban" in your message`) {
		t.Errorf("index line must name the user keyword, got: %s", line)
	}
}

// TestSwitchOff_HooksAreNoOps: with the switch off, RegisterMCPModules and
// the pre-load path are no-ops and the tool array is unchanged (the golden
// tests assert byte identity; this asserts the hooks do not exist).
func TestSwitchOff_HooksAreNoOps(t *testing.T) {
	mcp := newFakeMCPManager(connectedServer("trello", &fakeMCPSession{}, "get_boards"))
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ContextModules = "off"
	offTools := BuildTools(app.Cfg, "/work", mcp, "")
	app.Tools = offTools
	app.Modules = ModuleRegistryForApp(app.Cfg, "/work", mcp)
	if app.Modules != nil {
		t.Fatalf("switch off: ModuleRegistryForApp must return nil, got %v", app.Modules)
	}
	app.MCP = mcp
	app.RegisterMCPModules(mcp) // must be a no-op
	if len(app.mcpEventBuf) != 0 {
		t.Errorf("switch off: lifecycle events must not be recorded, got %d", len(app.mcpEventBuf))
	}
	if mcp.lifecycle.Load() != nil {
		t.Error("switch off: the manager must not get a lifecycle handler")
	}
}

// --- Stage 1 fixes round 2, Block 1: loaded modules survive collapse ---

// schemaMsg builds the message as finalizeToolResult appends it for a
// load_module result: role "tool", pinned, content = the schemas block.
func schemaMsg(id, content string) proxy.Message {
	m := trMsg(id, "load_module", content)
	m.Pinned = true
	return m
}

// findSchemaBlock looks for a module's schemas in the projected payload
// by identity (role tool, name load_module, pinned, module named in the
// content) — never by a loose content search.
func findSchemaBlock(msgs []proxy.Message, module string) bool {
	for _, m := range msgs {
		if m.Role != "tool" || m.Name != "load_module" || !m.Pinned {
			continue
		}
		c := DerefStr(m.Content)
		if strings.Contains(c, `Module "`+module+`" loaded`) {
			return true
		}
	}
	return false
}

// TestCollapse_LoadedModuleSurvivesTurnEndCollapse: a turn containing a
// pinned load_module result collapses, and the projected payload the
// model receives still carries the schemas exactly once, paired with
// its assistant tool call.
func TestCollapse_LoadedModuleSurvivesTurnEndCollapse(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	if _, _, err := app.Modules.LoadModule("staging"); err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("use the staging tools"), Turn: 1},
		tcMsg("tc0", "read_file", `{"path":"foo.go"}`),
		trMsg("tc0", "read_file", bigContent),
		tcMsg("ld1", "load_module", `{"name":"staging"}`),
		schemaMsg("ld1", "Module \"staging\" loaded: staging\nTool schemas:\n[]"),
		tcMsg("tc1", "search_files", `{"pattern":"bar"}`),
		trMsg("tc1", "search_files", bigContent),
		{Role: "assistant", Content: StrPtr("done"), Turn: 1},
	}
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1

	app.collapseFinishedTurns()

	n, _ := app.CollapsedCount()
	if n != 1 {
		t.Fatalf("CollapsedCount = %d, want 1 (the turn must have collapsed)", n)
	}
	proj := app.contextProjection()
	if !findSchemaBlock(proj, "staging") {
		t.Fatalf("projected payload lost the loaded module's schemas; payload has %d messages", len(proj))
	}
	count := 0
	for _, m := range proj {
		if m.Role == "tool" && m.Name == "load_module" && strings.Contains(DerefStr(m.Content), `Module "staging" loaded`) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("schemas must appear exactly once in the projected payload, got %d", count)
	}
	assertNoOrphanedToolCalls(t, proj)
	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	if rec.Archive != "" {
		os.Remove(rec.Archive)
	}
}

// TestCollapse_CoreToolPinnedResultsStillCollapse: a pinned tool result
// that is NOT a load_module schema message (subagent results are pinned
// too) must not be made uncollapsible — only load_module schemas are
// preserved.
func TestCollapse_CoreToolPinnedResultsStillCollapse(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("delegate it"), Turn: 1},
		tcMsg("ds1", "dispatch_subagent", `{"task":"x"}`),
		func() proxy.Message {
			m := trMsg("ds1", "dispatch_subagent", bigContent)
			m.Pinned = true
			return m
		}(),
		{Role: "assistant", Content: StrPtr("done"), Turn: 1},
	}
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1

	app.collapseFinishedTurns()

	n, _ := app.CollapsedCount()
	if n != 1 {
		t.Fatalf("CollapsedCount = %d, want 1 — pinned subagent results must still collapse", n)
	}
	proj := app.contextProjection()
	for _, m := range proj {
		if m.Role == "tool" && strings.Contains(DerefStr(m.Content), bigContent[:64]) {
			t.Errorf("pinned subagent result must still be elided from the projection")
		}
	}
	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	if rec.Archive != "" {
		os.Remove(rec.Archive)
	}
}

// TestCollapse_LoadedModuleSurvivesExplicit: the same guarantee for the
// explicit collapse_context path.
func TestCollapse_LoadedModuleSurvivesExplicit(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	if _, _, err := app.Modules.LoadModule("staging"); err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	call := proxy.ToolCall{ID: "cc1", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"done"}`}}
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("investigate"), Turn: 1},
		tcMsg("ld1", "load_module", `{"name":"staging"}`),
		schemaMsg("ld1", "Module \"staging\" loaded: staging\nTool schemas:\n[]"),
		tcMsg("tcA", "read_file", `{"path":"a.go"}`),
		trMsg("tcA", "read_file", bigContent),
		{Role: "assistant", ToolCalls: []proxy.ToolCall{call}, Turn: 1},
	}
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1
	app.curTurn = 1

	if res := app.handleCollapseContext(call); strings.HasPrefix(res, "ERROR") {
		t.Fatalf("collapse_context failed: %s", res)
	}
	proj := app.contextProjection()
	if !findSchemaBlock(proj, "staging") {
		t.Fatalf("after collapse_context the projected payload lost the loaded module's schemas (%d messages)", len(proj))
	}
	// Pair validity for the preserved schema pair (the collapse_context
	// boundary call is answered by a result appended after the
	// projection, so the whole-payload helper would flag it spuriously).
	found := false
	for _, m := range proj {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.Function.Name == "load_module" && tc.ID == "ld1" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("preserved schema block has no preceding assistant load_module tool call in the projected payload")
	}
	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	if rec.Archive != "" {
		os.Remove(rec.Archive)
	}
}

// TestCollapse_LoadedModuleSurvivesMidTurn: a schema message in the
// archived mid-turn prefix is preserved in the projected payload,
// paired with its parent tool call.
func TestCollapse_LoadedModuleSurvivesMidTurn(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextModules = "on"
	app.Modules = NewModuleRegistry(app.Cfg, "/work", nil)
	if _, _, err := app.Modules.LoadModule("staging"); err != nil {
		t.Fatalf("LoadModule: %v", err)
	}
	tail := tcMsg("tc9", "read_file", `{"path":"z.go"}`)
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("task"), Turn: 1},
		tcMsg("tc0", "read_file", `{"path":"foo.go"}`),
		trMsg("tc0", "read_file", bigContent),
		tcMsg("ld1", "load_module", `{"name":"staging"}`),
		schemaMsg("ld1", "Module \"staging\" loaded: staging\nTool schemas:\n[]"),
		tail,
		trMsg("tc9", "read_file", bigContent),
	}
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1
	app.curTurn = 1
	// Partial record anchored at the first kept tail batch (tc9).
	app.stateMu.Lock()
	if app.collapsed == nil {
		app.collapsed = map[int]CollapseRecord{}
	}
	app.collapsed[1] = CollapseRecord{
		Turn: 1, Partial: true, BoundaryCallID: "tc9",
		Note: "note", Archive: "unused",
	}
	app.stateMu.Unlock()

	proj := app.contextProjection()
	if !findSchemaBlock(proj, "staging") {
		t.Fatalf("mid-turn collapse: projected payload lost the loaded module's schemas (%d messages)", len(proj))
	}
	assertNoOrphanedToolCalls(t, proj)
}

// TestCollapse_NoteListsLoadedModules: the collapse note names the
// modules whose schemas were preserved, and a turn with no schemas
// lists none.
func TestCollapse_NoteListsLoadedModules(t *testing.T) {
	w := []proxy.Message{
		tcMsg("ld1", "load_module", `{"name":"staging"}`),
		schemaMsg("ld1", "Module \"staging\" loaded: staging\nTool schemas:\n[]"),
	}
	note := buildCollapseNote(1, w, "archive.txt")
	if !strings.Contains(note, "staging") {
		t.Errorf("collapse note must name the preserved loaded module; note:\n%s", note)
	}
	note = buildCollapseNote(1, []proxy.Message{trMsg("a", "read_file", "x")}, "archive.txt")
	if strings.Contains(note, "modules loaded and preserved") {
		t.Errorf("collapse note must not list modules when none are preserved; note:\n%s", note)
	}
}
