package agent

// modules.go — Context module registry for Stage 1.
//
// When the context_modules switch is "off" (the default), this file is inert:
// BuildTools produces the same tool array as before the feature, and the
// module registry is never consulted. When the switch is "on", BuildTools
// produces a core-only tool array (load_module + call_tool + the core tools),
// and the deferred tools are reached through load_module(name) which loads a
// module's schemas, and call_tool(name, arguments) which dispatches a call to
// a deferred tool through the same handleToolCall path as a direct call.
//
// The registry is a static table of module definitions (name, tool list,
// description for the index). It is generated at package init time from the
// same tool-builder functions that BuildTools uses, so the module contents
// cannot drift from the shipped tools. MCP modules (mcp:<server>) are built
// dynamically from the connected server list at BuildTools time.
//
// See docs/plans/context-modules.md §7 (design) and the "Stage 1
// implementation" section appended to that document for the verified findings
// and block-by-block plan.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/treeol/wakil/internal/browser"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/lsp"
	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// coreToolNames is the set of tool names that are always in the core tool
// array when the switch is on. Everything not in this set is deferred to a
// module. Derived from §7.1.1 and §F1.3.4 of the design.
var coreToolNames = map[string]bool{
	// Built-in file/shell tools.
	"dispatch_subagent":  true,
	"dispatch_subagents": true,
	"run_shell":          true,
	"read_file":          true,
	"read_file_full":     true,
	"list_dir":           true,
	"search_files":       true,
	"find_files":         true,
	"edit_file":          true,
	"write_file":         true,
	"delete_file":        true,
	"run_background":     true,
	"kill_process":       true,
	"read_process_log":   true,
	"math_eval":          true,
	"open_url":           true,
	// Async / collapse tools (always present).
	"check_pending":       true,
	"wait_for_completion": true,
	"collapse_context":    true,
	// Memory tools (stay core per §F1.3.4).
	"memory_put":                  true,
	"memory_promote":              true,
	"memory_reject":               true,
	"memory_get":                  true,
	"memory_search":               true,
	"memory_list":                 true,
	"memory_forget":               true,
	"memory_promote_from_staging": true,
	// Skill read tools (stay core; only authoring tools are deferred).
	"list_skills":   true,
	"load_skill":    true,
	"skill_search":  true,
	"skill_history": true,
	// Git tools (stay core per §F1.3.4).
	"git_status": true,
	"git_diff":   true,
	"git_log":    true,
	"git_show":   true,
	"git_blame":  true,
	// Module dispatcher tools (always core when the switch is on).
	"load_module": true,
	"call_tool":   true,
}

// Module is one entry in the module registry: a name, a set of tool
// definitions, and a one-line description for the index.
type Module struct {
	// Name is the module identifier the model passes to load_module.
	Name string
	// Desc is the one-line index entry shown in the preamble index.
	Desc string
	// Tools are the tool definitions this module carries. They are NOT in
	// the core tool array; the model loads them via load_module(Name) and
	// calls them via call_tool(toolName, arguments).
	Tools []proxy.Tool
}

// builtinModules returns the static module list (non-MCP modules). The tools
// are built from the same functions BuildTools uses, so they cannot drift.
// MCP modules (mcp:<server>) are built separately at BuildTools time.
func builtinModules(cfg config.Config, cwd string) []Module {
	var mods []Module

	// skills-authoring: the 6 skill authoring/template tools. list_skills,
	// load_skill, skill_search, skill_history stay core.
	mods = append(mods, Module{
		Name: "skills-authoring",
		Desc: "save_skill, update_skill, forget_skill, apply_skill_template, capture_skill_template, check_skill_template (~2.3k tok)",
		Tools: filterTools(wtools.SkillTools(), map[string]bool{
			"save_skill":             true,
			"update_skill":           true,
			"forget_skill":           true,
			"apply_skill_template":   true,
			"capture_skill_template": true,
			"check_skill_template":   true,
		}),
	})

	// edit-extras: move_file, write_binary_file.
	mods = append(mods, Module{
		Name: "edit-extras",
		Desc: "move_file, write_binary_file (~0.4k tok)",
		Tools: filterTools(wtools.DefaultTools(cwd), map[string]bool{
			"move_file":         true,
			"write_binary_file": true,
		}),
	})

	// staging: the 5 staging tools.
	mods = append(mods, Module{
		Name:  "staging",
		Desc:  "staging_put, staging_get, staging_delete, staging_list, staging_get_many (~0.6k tok)",
		Tools: wtools.StagingTools(),
	})

	// search: searxng tools (conditional — only if searxng_url is set).
	if cfg.SearXngURL != "" {
		mods = append(mods, Module{
			Name:  "search",
			Desc:  "searxng_search, searxng_url_read (~1.5k tok)",
			Tools: wtools.SearxngTools(),
		})
	}

	// browser: 8 browser tools (conditional — only if browser_enabled).
	if cfg.BrowserEnabled {
		mods = append(mods, Module{
			Name:  "browser",
			Desc:  "browser_navigate, browser_screenshot, browser_viewport, browser_click, browser_eval, browser_text, browser_html, browser_reduced_motion (~1.2k tok)",
			Tools: browser.BrowserTools(),
		})
	}

	// lsp: 4 LSP tools (conditional — only if lsp_enabled).
	if cfg.LSPEnabled {
		mods = append(mods, Module{
			Name:  "lsp",
			Desc:  "lsp_definition, lsp_references, lsp_hover, lsp_symbols (~0.6k tok)",
			Tools: lsp.LSPTools(cwd),
		})
	}

	return mods
}

// mcpModules builds one Module per connected MCP server. Each module is
// named mcp:<server> and contains exactly that server's tools.
func mcpModules(mcp *MCPManager) []Module {
	if mcp == nil {
		return nil
	}
	servers := mcp.ServerNames()
	if len(servers) == 0 {
		return nil
	}
	var mods []Module
	for _, srvName := range servers {
		tools := mcp.OpenAIToolsForServers(map[string]bool{srvName: true})
		if len(tools) == 0 {
			continue
		}
		mods = append(mods, Module{
			Name:  "mcp:" + srvName,
			Desc:  fmt.Sprintf("MCP server %q (%d tools)", srvName, len(tools)),
			Tools: tools,
		})
	}
	return mods
}

// filterTools returns only the tools whose Function.Name is in the allow set.
func filterTools(tools []proxy.Tool, allow map[string]bool) []proxy.Tool {
	var out []proxy.Tool
	for _, t := range tools {
		if allow[t.Function.Name] {
			out = append(out, t)
		}
	}
	return out
}

// loadModuleToolDef returns the load_module tool definition.
func loadModuleToolDef() proxy.Tool {
	params, _ := json.Marshal(map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"name": map[string]interface{}{
				"type":        "string",
				"description": "Module name to load (e.g. \"memory\", \"staging\", \"mcp:trello\"). Returns the module's tool schemas and a one-line description. Idempotent: loading an already-loaded module is a no-op that returns the schemas again.",
			},
		},
		"required": []string{"name"},
	})
	return proxy.Tool{Type: "function", Function: proxy.ToolFunction{
		Name:        "load_module",
		Description: "Load a context module: returns the tool schemas of the module's tools so you can call them with call_tool. The module index in your system prompt lists all available modules and their estimated token cost. Idempotent — loading an already-loaded module returns its schemas again without error.",
		Parameters:  params,
	}}
}

// callToolToolDef returns the call_tool tool definition.
func callToolToolDef() proxy.Tool {
	params, _ := json.Marshal(map[string]interface{}{
		"type": "object",
		"properties": map[string]interface{}{
			"name": map[string]interface{}{
				"type":        "string",
				"description": "The tool to call (e.g. \"save_skill\", \"move_file\", \"staging_put\"). Must be a tool from a module you have loaded.",
			},
			"arguments": map[string]interface{}{
				"type":        "string",
				"description": "JSON-encoded arguments for the tool, exactly as you would pass them to the tool directly.",
			},
		},
		"required": []string{"name", "arguments"},
	})
	return proxy.Tool{Type: "function", Function: proxy.ToolFunction{
		Name:        "call_tool",
		Description: "Call a tool from a loaded module. The tool goes through the same approval, gating, sandbox, and trace checks as a direct call. Use load_module first to load the module and see the tool's schema.",
		Parameters:  params,
	}}
}

// ModuleRegistry holds the session-scoped module state: the module
// definitions and the set of loaded module names. It is created at BuildTools
// time and is immutable for the rest of the session (the tool array is
// frozen; loading a module does not add tools to the array).
type ModuleRegistry struct {
	// modules is the full list of available modules (built-in + MCP), in
	// index order.
	modules []Module
	// byName maps module name → Module for fast lookup.
	byName map[string]Module
	// loaded tracks which modules have been loaded via load_module or
	// pre-loaded at session start.
	loaded map[string]bool
	// preloadedAtConstruction is a snapshot of the loaded set taken at
	// construction time, used by moduleCapabilityLine so the preamble's
	// LSP/browser lines are frozen at session start and never change on a
	// mid-session rebuild.
	preloadedAtConstruction map[string]bool
	// mu guards the loaded map against concurrent access.
	mu sync.Mutex
}

// NewModuleRegistry builds a registry from the given config, cwd, and MCP
// manager. Returns nil if the switch is off (the caller should not use it).
func NewModuleRegistry(cfg config.Config, cwd string, mcp *MCPManager) *ModuleRegistry {
	mods := builtinModules(cfg, cwd)
	mods = append(mods, mcpModules(mcp)...)
	r := &ModuleRegistry{
		modules:                 mods,
		byName:                  make(map[string]Module, len(mods)),
		loaded:                  make(map[string]bool),
		preloadedAtConstruction: make(map[string]bool),
	}
	for _, m := range mods {
		r.byName[m.Name] = m
	}
	return r
}

// ModuleNames returns the names of all available modules, in index order.
func (r *ModuleRegistry) ModuleNames() []string {
	names := make([]string, 0, len(r.modules))
	for _, m := range r.modules {
		names = append(names, m.Name)
	}
	return names
}

// IsLoaded reports whether the named module has been loaded.
func (r *ModuleRegistry) IsLoaded(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.loaded[name]
}

// LoadModule marks a module as loaded and returns its tool schemas and
// description. Returns an error if the module name is unknown.
func (r *ModuleRegistry) LoadModule(name string) ([]proxy.Tool, string, error) {
	m, ok := r.byName[name]
	if !ok {
		return nil, "", fmt.Errorf("unknown module %q — available: %s", name, strings.Join(r.ModuleNames(), ", "))
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.loaded[name] = true
	return m.Tools, m.Desc, nil
}

// ToolModule returns the module name that owns the given tool name, or ""
// if the tool is not in any module (it is core).
func (r *ModuleRegistry) ToolModule(toolName string) string {
	for _, m := range r.modules {
		for _, t := range m.Tools {
			if t.Function.Name == toolName {
				return m.Name
			}
		}
	}
	return ""
}

// ModuleIndex returns the one-line-per-module index text appended to the
// preamble when the switch is on. One line per module, always loaded.
func (r *ModuleRegistry) ModuleIndex() string {
	var b strings.Builder
	b.WriteString("## Context modules\n")
	b.WriteString("Not loaded by default. Call load_module(name) to load one; unknown name lists all.\n")
	for _, m := range r.modules {
		fmt.Fprintf(&b, "  %-16s — %s\n", m.Name, m.Desc)
	}
	return strings.TrimRight(b.String(), "\n")
}

// LoadedNames returns the names of all loaded modules, sorted for
// deterministic output.
func (r *ModuleRegistry) LoadedNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var names []string
	for _, m := range r.modules {
		if r.loaded[m.Name] {
			names = append(names, m.Name)
		}
	}
	return names
}

// IsModuleTool reports whether the given tool name belongs to any module
// (i.e. it is deferred, not core). Used by the unknown-tool error path to
// tell the model which module to load.
func (r *ModuleRegistry) IsModuleTool(toolName string) (string, bool) {
	name := r.ToolModule(toolName)
	return name, name != ""
}

// BuildToolsWithModules is the switch-on variant of BuildTools. It returns
// the core tool array (including load_module and call_tool) and the
// ModuleRegistry. The caller stores the registry on the App for use by
// handleLoadModule and handleCallTool.
func BuildToolsWithModules(cfg config.Config, cwd string, mcp *MCPManager, repoLevel string) ([]proxy.Tool, *ModuleRegistry) {
	reg := NewModuleRegistry(cfg, cwd, mcp)

	// Start from the full DefaultTools set (which includes staging, memory,
	// skills, git, math_eval, open_url, and the built-in file/shell tools).
	// Then filter to core-only + add load_module + call_tool.
	full := wtools.DefaultTools(cwd)

	// Add the always-present async/collapse tools (same as BuildTools).
	full = append(full, checkPendingToolDef())
	full = append(full, waitForCompletionToolDef())
	full = append(full, collapseContextToolDef())

	// Filter to core tools only.
	var core []proxy.Tool
	for _, t := range full {
		if coreToolNames[t.Function.Name] {
			core = append(core, t)
		}
	}

	// Add Mashūra tools (config-gated, not deferred — they stay in the core
	// array when the switch is on, matching the plan's "stays config-gated,
	// not deferred" decision).
	if mashuraOffered(cfg, repoLevel) {
		core = append(core, mashuraToolDefs()...)
	}

	// Add load_module and call_tool.
	core = append(core, loadModuleToolDef())
	core = append(core, callToolToolDef())

	// Sort the core array by name for cache stability (same as BuildTools
	// groups — the tool array is part of the cache prefix).
	core = SortToolsByName(core)

	return core, reg
}

// ContextModulesEnabled reports whether the context-modules switch is on for
// this config. When false, BuildTools and the preamble are byte-identical to
// today.
func ContextModulesEnabled(cfg config.Config) bool {
	return cfg.ContextModules == "on"
}

// ModuleRegistryForApp returns a ModuleRegistry when the switch is on, or nil
// when off. Called by wiring/bootstrap.go at App construction to set the
// Modules field alongside Tools.
func ModuleRegistryForApp(cfg config.Config, cwd string, mcp *MCPManager) *ModuleRegistry {
	if !ContextModulesEnabled(cfg) {
		return nil
	}
	_, reg := BuildToolsWithModules(cfg, cwd, mcp, "")
	return reg
}

// defaultMCPKeywords returns the built-in keyword list for a server name.
// Used when the user has not provided an explicit keyword list in config.
func defaultMCPKeywords(serverName string) []string {
	switch strings.ToLower(serverName) {
	case "trello":
		return []string{"board", "card", "trello", "list"}
	case "context7":
		return []string{"library", "docs", "documentation", "api reference"}
	default:
		return nil
	}
}

// mcpKeywordsForServer returns the keyword list for a server: user config if
// present, else the built-in defaults.
func mcpKeywordsForServer(serverName string, userKeywords map[string][]string) []string {
	if kws, ok := userKeywords[serverName]; ok && len(kws) > 0 {
		return kws
	}
	return defaultMCPKeywords(serverName)
}

// matchMCPKeywords reports whether the task text contains any of the server's
// keywords (word-boundary, case-insensitive). A keyword with a space (e.g.
// "API reference") matches as a substring (case-insensitive) since the space
// is the boundary.
func matchMCPKeywords(taskText string, keywords []string) bool {
	if len(keywords) == 0 {
		return false
	}
	lower := strings.ToLower(taskText)
	for _, kw := range keywords {
		lkw := strings.ToLower(kw)
		if strings.Contains(lkw, " ") {
			if strings.Contains(lower, lkw) {
				return true
			}
			continue
		}
		// Single-word keyword: match on a word boundary so "list" does not
		// match "listed" or "listing". A word boundary is a transition
		// between a word char [a-z0-9_] and a non-word char (or string edge).
		if containsWord(lower, lkw) {
			return true
		}
	}
	return false
}

// containsWord reports whether s contains word as a whole word
// (case-sensitive; caller lowercases both). A word boundary is a transition
// between a word char [a-z0-9_] and a non-word char (or string edge).
func containsWord(s, word string) bool {
	if word == "" {
		return false
	}
	start := 0
	for {
		idx := strings.Index(s[start:], word)
		if idx < 0 {
			return false
		}
		idx += start
		// Check the char before the match.
		beforeOK := idx == 0 || !isWordChar(s[idx-1])
		// Check the char after the match.
		afterIdx := idx + len(word)
		afterOK := afterIdx >= len(s) || !isWordChar(s[afterIdx])
		if beforeOK && afterOK {
			return true
		}
		start = idx + 1
	}
}

// isWordChar reports whether r is a word character (letter, digit, or
// underscore).
func isWordChar(r byte) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
}

// MCPModulePreload returns the list of MCP module names to pre-load on the
// first request, based on keyword matching against the task text. Only
// modules whose server has keywords and whose keywords match the task text
// are returned. A server with no keywords (and no user override) is not
// pre-loaded — it stays reachable by load_module.
func (r *ModuleRegistry) MCPModulePreload(taskText string, userKeywords map[string][]string) []string {
	if r == nil || taskText == "" {
		return nil
	}
	var preload []string
	for _, m := range r.modules {
		if !strings.HasPrefix(m.Name, "mcp:") {
			continue
		}
		serverName := strings.TrimPrefix(m.Name, "mcp:")
		kws := mcpKeywordsForServer(serverName, userKeywords)
		if len(kws) == 0 {
			continue
		}
		if matchMCPKeywords(taskText, kws) {
			preload = append(preload, m.Name)
		}
	}
	return preload
}

// PreloadModules marks the given module names as loaded (pre-loaded at session
// start, before the first request). This is used by the MCP keyword pre-load
// so the module's tools are available on the first request without a
// load_module round trip.
func (r *ModuleRegistry) PreloadModules(names []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range names {
		if _, ok := r.byName[n]; ok {
			r.loaded[n] = true
			r.preloadedAtConstruction[n] = true
		}
	}
}

// moduleCapabilityLine returns the preamble inventory line for a conditional
// tool group (LSP, browser) when context modules are on. If the module was
// pre-loaded at construction time, the full line stays as today. If not, a
// shorter line names the module to load. The line is frozen at session
// start — it depends only on construction-time state, never on mid-session
// loads. This prevents a mid-session preamble rebuild from changing the
// LSP/browser line after the model loads the module.
func moduleCapabilityLine(moduleName string, reg *ModuleRegistry, fullLine string) string {
	// Use the construction-time snapshot, not the live loaded set.
	if reg.preloadedAtConstruction[moduleName] {
		return fullLine
	}
	return fmt.Sprintf("%s tools are available but not loaded — call load_module(%q) to load them.",
		moduleName, moduleName)
}

// handleLoadModule implements the load_module tool. It loads a module's
// schemas from the registry and returns them as a pinned tool result (so
// they survive eviction). Idempotent: loading an already-loaded module
// returns the schemas again without error. Exempt from CapOrStub.
func (a *App) handleLoadModule(ctx context.Context, tc proxy.ToolCall) string {
	if a.Modules == nil {
		return "ERROR: context modules are not enabled. Set context_modules to \"on\" in config."
	}
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if args.Name == "" {
		return "ERROR: name is required. Available modules: " + strings.Join(a.Modules.ModuleNames(), ", ")
	}
	tools, desc, err := a.Modules.LoadModule(args.Name)
	if err != nil {
		return err.Error()
	}
	// Return the tool schemas as JSON so the model can call them via call_tool.
	schemasJSON, _ := json.MarshalIndent(tools, "", "  ")
	return fmt.Sprintf("Module %q loaded: %s\nTool schemas:\n%s", args.Name, desc, schemasJSON)
}

// (handleCallTool was removed — call_tool is now unwrapped at the top of
// handleToolCall in app.go, so all gates (dedup, hooks, trace, cache
// invalidation, CapOrStub, result-json) see the inner tool name. The
// executeToolCall switch no longer has a "call_tool" case.)
