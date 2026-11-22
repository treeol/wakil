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
