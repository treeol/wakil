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
	"github.com/treeol/wakil/internal/diag"
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
// definitions and the set of loaded module names. The built-in modules are
// static; MCP modules (mcp:<server>) are added and removed dynamically as
// servers connect and disconnect (refreshMCPModules), so a server that
// connects after the first snapshot still becomes a module. Loading a
// module does not add tools to the tool array — the array is frozen after
// the first request; loaded schemas reach the model only as appended
// messages.
type ModuleRegistry struct {
	// modules is the full list of available modules (built-in + MCP), in
	// index order. MCP modules are appended as their servers connect.
	modules []Module
	// byName maps module name → Module for fast lookup.
	byName map[string]Module
	// loaded tracks which modules have been loaded via load_module or
	// pre-loaded at session start.
	loaded map[string]bool
	// disconnected marks MCP modules whose server has disconnected. Their
	// load_module and call_tool answer with a clear "server not connected"
	// error instead of silently succeeding against a dead session.
	disconnected map[string]bool
	// announcedOnce tracks MCP index lines already announced as appended
	// messages, so the reconcile never announces the same module twice.
	announcedOnce map[string]bool
	// preloadedAtConstruction is a snapshot of the loaded set taken at
	// construction time, used by moduleCapabilityLine so the preamble's
	// LSP/browser lines are frozen at session start and never change on a
	// mid-session rebuild.
	preloadedAtConstruction map[string]bool
	// preloadReason records, per module, the keyword that caused its
	// pre-load (Block 2, Stage 1 fixes round 2), so the module table
	// line can render the reason. Populated by PreloadModules; read by
	// ModuleIndexLine / ModuleIndex. Guarded by mu.
	preloadReason map[string]string
	// mu guards every field above against concurrent access.
	mu sync.Mutex
}

// NewModuleRegistry builds a registry from the given config, cwd, and MCP
// manager. Returns nil if the switch is off (the caller should not use it).
func NewModuleRegistry(cfg config.Config, cwd string, mcp *MCPManager) *ModuleRegistry {
	mods := builtinModules(cfg, cwd)
	// MCP modules are folded in by the single registry path below; the
	// manager's snapshot is taken inside mcpModules (under the manager
	// lock) BEFORE the registry lock, so the two locks never nest in
	// reverse. The late-connect reconcile (refreshMCPModules) uses the same
	// fold, so there is no legacy duplicate build left to drift.
	mods = append(mods, mcpModules(mcp)...)
	r := &ModuleRegistry{
		modules:                 mods,
		byName:                  make(map[string]Module, len(mods)),
		loaded:                  make(map[string]bool),
		disconnected:            make(map[string]bool),
		announcedOnce:           make(map[string]bool),
		preloadedAtConstruction: make(map[string]bool),
		preloadReason:           make(map[string]string),
	}
	for _, m := range mods {
		r.byName[m.Name] = m
	}
	// Modules present at construction are already in the frozen preamble
	// index; announcers must not re-announce them as appended messages.
	for _, m := range mods {
		if strings.HasPrefix(m.Name, "mcp:") {
			r.announcedOnce[m.Name] = true
		}
	}
	return r
}

// HasModule reports whether the named module exists in the registry.
func (r *ModuleRegistry) HasModule(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.byName[name]
	return ok
}

// refreshMCPModules reconciles the registry's MCP modules with the manager's
// live state. It is the turn-start safety net (and the per-event hook): the
// manager snapshot is taken inside mcpModules (under the manager lock);
// ONLY THEN is the registry lock taken, so the two locks never nest in
// reverse. Idempotent: a server already in the registry is left alone; a
// connected server with no module is added. Returns the names of modules
// whose index line has not yet been announced.
func (r *ModuleRegistry) refreshMCPModules(m *MCPManager) []string {
	return r.refreshMCPModulesSilently(m, true)
}

// refreshMCPModulesSilently is the reconcile used before the preamble
// exists (at BuildApp registration and at turn 1 before the first
// ensurePreamble). When announce is false, new modules are registered but
// NOT returned for announcement — the frozen preamble index is their one
// announcement, so an appended announcement would duplicate it and, worse,
// land before messages[0]. After ensurePreamble, announce=true and new
// modules are returned for the appended announcement path.
func (r *ModuleRegistry) refreshMCPModulesSilently(m *MCPManager, announce bool) []string {
	if r == nil || m == nil {
		return nil
	}
	current := mcpModules(m)
	r.mu.Lock()
	defer r.mu.Unlock()
	live := make(map[string]Module, len(current))
	for _, mod := range current {
		live[mod.Name] = mod
	}
	var announced []string
	for _, mod := range current {
		existing, ok := r.byName[mod.Name]
		if !ok {
			r.modules = append(r.modules, mod)
			r.byName[mod.Name] = mod
			if announce {
				if !r.announcedOnce[mod.Name] {
					announced = append(announced, mod.Name)
					r.announcedOnce[mod.Name] = true
				}
			} else {
				// Before the preamble: the frozen index will name this
				// module, so the late-announcement path must never fire.
				r.announcedOnce[mod.Name] = true
			}
			continue
		}
		// Refresh a reconnected server whose tool set changed. The loaded
		// flag is cleared so the gate cannot pass for tools the model
		// never saw schemas for; the model is told through the returned
		// announcement (a reconnect updates the module and announces it
		// again, whether or not it was announced before). A schema- or
		// description-only change (same names) is refreshed too: the
		// announced line is the index line, never the schemas, so the
		// model re-loads and sees current text rather than stale copies.
		if len(existing.Tools) != len(mod.Tools) || toolNamesDiffer(existing.Tools, mod.Tools) || toolsTextDiffer(existing.Tools, mod.Tools) {
			existing.Tools = mod.Tools
			existing.Desc = mod.Desc
			for i, rm := range r.modules {
				if rm.Name == mod.Name {
					r.modules[i] = existing
					break
				}
			}
			r.byName[mod.Name] = existing
			delete(r.loaded, mod.Name)
			if announce {
				announced = append(announced, mod.Name)
				r.announcedOnce[mod.Name] = true
			}
		}
		delete(r.disconnected, mod.Name)
	}
	for _, mod := range r.modules {
		if !strings.HasPrefix(mod.Name, "mcp:") {
			continue
		}
		if _, ok := live[mod.Name]; !ok {
			r.disconnected[mod.Name] = true
		}
	}
	return announced
}

// toolsTextDiffer reports whether two tool slices with the same names carry
// different descriptions or different parameter JSON (schema-only drift).
// Same length is assumed by the caller.
func toolsTextDiffer(a, b []proxy.Tool) bool {
	am := make(map[string]proxy.Tool, len(a))
	for _, t := range a {
		am[t.Function.Name] = t
	}
	for _, t := range b {
		old, ok := am[t.Function.Name]
		if !ok {
			return true
		}
		if old.Function.Description != t.Function.Description {
			return true
		}
		aj, _ := json.Marshal(old.Function.Parameters)
		bj, _ := json.Marshal(t.Function.Parameters)
		if string(aj) != string(bj) {
			return true
		}
	}
	return false
}

// toolNamesDiffer reports whether two tool slices carry different names
// (same length assumed by the caller).
func toolNamesDiffer(a, b []proxy.Tool) bool {
	an := make(map[string]bool, len(a))
	for _, t := range a {
		an[t.Function.Name] = true
	}
	for _, t := range b {
		if !an[t.Function.Name] {
			return true
		}
	}
	return false
}

// IsDisconnected reports whether the named MCP module's server is currently
// recorded as disconnected.
func (r *ModuleRegistry) IsDisconnected(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.disconnected[name]
}

// ModuleNames returns the names of all available modules, in index order.
func (r *ModuleRegistry) ModuleNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
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
// description. Returns an error if the module name is unknown, or a clear
// "server not connected" error if the module's MCP server has disconnected.
func (r *ModuleRegistry) LoadModule(name string) ([]proxy.Tool, string, error) {
	r.mu.Lock()
	m, ok := r.byName[name]
	if !ok {
		r.mu.Unlock()
		return nil, "", fmt.Errorf("unknown module %q — available: %s", name, strings.Join(r.ModuleNames(), ", "))
	}
	if r.disconnected[name] {
		r.mu.Unlock()
		return nil, "", fmt.Errorf("module %q: MCP server is not connected", name)
	}
	r.loaded[name] = true
	tools, desc := m.Tools, m.Desc
	r.mu.Unlock()
	return tools, desc, nil
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

// ModuleIndexLine returns the one-line index entry for a single module, in
// the same format ModuleIndex uses. Long names (mcp:<server>) exceed the
// %-16s column; the format adapts so the line stays a single index entry.
func (r *ModuleRegistry) ModuleIndexLine(name string) (string, bool) {
	r.mu.Lock()
	m, ok := r.byName[name]
	reason := r.preloadReason[name]
	r.mu.Unlock()
	if !ok {
		return "", false
	}
	line := fmt.Sprintf("  %-16s — %s", m.Name, m.Desc)
	if reason != "" {
		line += fmt.Sprintf(" (pre-loaded: %q in your message)", reason)
	}
	return line, true
}

// ModuleIndex returns the one-line-per-module index text appended to the
// preamble when the switch is on. One line per module, always loaded.
func (r *ModuleRegistry) ModuleIndex() string {
	var b strings.Builder
	b.WriteString("## Context modules\n")
	b.WriteString("Not loaded by default. Call load_module(name) to load one; unknown name lists all.\n")
	r.mu.Lock()
	mods := append([]Module(nil), r.modules...)
	reasons := make(map[string]string, len(r.preloadReason))
	for k := range r.preloadReason {
		reasons[k] = r.preloadReason[k]
	}
	r.mu.Unlock()
	for _, m := range mods {
		fmt.Fprintf(&b, "  %-16s — %s", m.Name, m.Desc)
		if reason := reasons[m.Name]; reason != "" {
			fmt.Fprintf(&b, " (pre-loaded: %q in your message)", reason)
		}
		b.WriteByte('\n')
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

// defaultMCPKeywords returns the built-in keyword list for a server
// name. Used when the user has not provided an explicit keyword list
// in config. Only the server name itself: a common word like "list"
// or "card" would pre-load the module on unrelated tasks (the host
// false positive that made mcp:trello load for a session with no
// Trello content).
func defaultMCPKeywords(serverName string) []string {
	if serverName == "" {
		return nil
	}
	return []string{strings.ToLower(serverName)}
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
	return matchedKeyword(taskText, keywords) != ""
}

// matchedKeyword returns the first keyword that matches the task text, or
// "" when none does. Block 2 of the Stage 1 fixes round 2 uses it so the
// module table can name the keyword that caused a pre-load.
func matchedKeyword(taskText string, keywords []string) string {
	if len(keywords) == 0 {
		return ""
	}
	lower := strings.ToLower(taskText)
	for _, kw := range keywords {
		lkw := strings.ToLower(kw)
		if strings.Contains(lkw, " ") {
			if strings.Contains(lower, lkw) {
				return kw
			}
			continue
		}
		// Single-word keyword: match on a word boundary so "list" does not
		// match "listed" or "listing". A word boundary is a transition
		// between a word char [a-z0-9_] and a non-word char (or string edge).
		if containsWord(lower, lkw) {
			return kw
		}
	}
	return ""
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
//
// It also returns the keyword that matched each returned module, so the
// module table can render the reason (pre-loaded: "trello" in your
// message).
func (r *ModuleRegistry) MCPModulePreload(taskText string, userKeywords map[string][]string) ([]string, map[string]string) {
	if r == nil || taskText == "" {
		return nil, nil
	}
	var preload []string
	reasons := map[string]string{}
	for _, m := range r.modules {
		if !strings.HasPrefix(m.Name, "mcp:") {
			continue
		}
		serverName := strings.TrimPrefix(m.Name, "mcp:")
		kws := mcpKeywordsForServer(serverName, userKeywords)
		if len(kws) == 0 {
			continue
		}
		if kw := matchedKeyword(taskText, kws); kw != "" {
			preload = append(preload, m.Name)
			reasons[m.Name] = kw
		}
	}
	return preload, reasons
}

// PreloadModules marks the given module names as loaded (pre-loaded at session
// start, before the first request). Disconnected modules are skipped: marking
// them would leave `loaded = true` with nothing delivered and the gate
// passing for nothing.
//
// reasons optionally records the keyword that caused each pre-load (the
// module table renders it); a nil or missing entry records no reason.
func (r *ModuleRegistry) PreloadModules(names []string, reasons map[string]string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range names {
		if _, ok := r.byName[n]; ok && !r.disconnected[n] {
			r.loaded[n] = true
			r.preloadedAtConstruction[n] = true
			if kw := reasons[n]; kw != "" {
				r.preloadReason[n] = kw
			}
		}
	}
}

// ModuleSchemasBlock renders one module's tool schemas in the exact form
// handleLoadModule returns, so a pre-loaded module needs no load_module
// round trip: the model already has the schemas on the first request, and a
// call_tool to any of its tools passes the loaded check. The block is
// delivered as an appended user-role message, never by editing an earlier
// message and never by changing the tool array.
func (r *ModuleRegistry) ModuleSchemasBlock(name string) (string, bool) {
	r.mu.Lock()
	m, ok := r.byName[name]
	r.mu.Unlock()
	if !ok {
		return "", false
	}
	schemasJSON, _ := json.MarshalIndent(m.Tools, "", "  ")
	return fmt.Sprintf("Module %q loaded: %s\nTool schemas:\n%s", name, m.Desc, schemasJSON), true
}

// PreloadedSchemas renders the schemas block for every module in names that
// exists and is not disconnected, joined by a blank line, in the order
// given. Names that do not resolve are skipped.
func (r *ModuleRegistry) PreloadedSchemas(names []string) (string, bool) {
	if r == nil || len(names) == 0 {
		return "", false
	}
	var blocks []string
	for _, n := range names {
		if r.IsDisconnected(n) {
			continue
		}
		if block, ok := r.ModuleSchemasBlock(n); ok {
			blocks = append(blocks, block)
		}
	}
	if len(blocks) == 0 {
		return "", false
	}
	return strings.Join(blocks, "\n\n"), true
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
	// Reconcile-on-miss: if the named module is unknown, the registry may
	// simply be behind a connect that has not been flushed yet (the flush
	// runs at turn start). Reconciling here — same reconcile, same
	// appended-announcement path — makes a server that connected after the
	// last flush reachable in this very request, not at the next user turn.
	if !a.Modules.HasModule(args.Name) && a.MCP != nil {
		a.flushPendingModuleEvents()
	}
	tools, desc, err := a.Modules.LoadModule(args.Name)
	if err != nil {
		return err.Error()
	}
	// Return the tool schemas as JSON so the model can call them via call_tool.
	schemasJSON, _ := json.MarshalIndent(tools, "", "  ")
	return fmt.Sprintf("Module %q loaded: %s\nTool schemas:\n%s", args.Name, desc, schemasJSON)
}

// RegisterMCPModules installs the MCP lifecycle consumer on the manager.
// Call once at App construction when the switch is on and a manager exists;
// when the switch is off (or there is no manager) it is a no-op, so
// switch-off sessions pay nothing and their tool array is unchanged.
// The reconcile below runs only after ensurePreamble has frozen
// messages[0]: announcing before that would put a user message at Conv[0]
// and duplicate the module in the preamble index.
func (a *App) RegisterMCPModules(m *MCPManager) {
	if a.Modules == nil || m == nil {
		return
	}
	m.SetLifecycleHandler(func(ev mcpLifecycleEvent) {
		a.stateMu.Lock()
		a.mcpEventBuf = append(a.mcpEventBuf, ev)
		a.stateMu.Unlock()
	})
	// Fold the manager state that already exists (a server may have
	// connected before this registration) WITHOUT announcing: the preamble
	// index — built once, at first turn — is the only announcement for
	// modules present then.
	a.Modules.refreshMCPModulesSilently(a.MCP, false)
}

// flushPendingModuleEvents runs on the agent goroutine at turn start. It
// drains every queued lifecycle event, reconciles the registry against the
// manager's live state (so a connect that completed before the handler was
// registered is still picked up — the safety net), and announces newly
// available modules as appended user-role messages. The tool array is never
// touched here; earlier messages are never edited.
func (a *App) flushPendingModuleEvents() {
	if a.Modules == nil || a.MCP == nil {
		return
	}
	a.stateMu.Lock()
	pending := a.mcpEventBuf
	a.mcpEventBuf = nil
	a.stateMu.Unlock()
	_ = pending // kinds are folded by the reconcile below; the queue bounds
	// how often the reconcile runs, not what it observes.

	announced := a.Modules.refreshMCPModules(a.MCP)
	for _, name := range announced {
		line, ok := a.Modules.ModuleIndexLine(name)
		if !ok {
			continue
		}
		body := "Context module available: " + name + "\n" + line +
			"\nCall load_module(\"" + name + "\") to load its tools, then call_tool(name, arguments) to use them."
		a.appendConv(proxy.Message{Role: "user", Content: StrPtr(body)})
		diag.Printf("wakil: late MCP module announced: %s", name)
	}
	// Safety net: a connected server with tools must have a module. If the
	// reconcile still left one out (e.g. an event raced startup), warn
	// loudly and reconcile again; its tools are announced through the same
	// path as a late connect, never silently dropped. (The tool array is
	// frozen, so no inline tool array is ever synthesized mid-session.)
	for _, srv := range a.MCP.ServerNames() {
		modName := "mcp:" + srv
		if a.Modules.HasModule(modName) {
			continue
		}
		// A server with zero tools is excluded by mcpModules; that is not
		// a dropped tool set, so no warning.
		if len(a.MCP.OpenAIToolsForServers(map[string]bool{srv: true})) == 0 {
			continue
		}
		diag.Printf("wakil: WARNING: connected MCP server %q has no module in the registry — reconciling", srv)
		for _, name := range a.Modules.refreshMCPModules(a.MCP) {
			line, ok := a.Modules.ModuleIndexLine(name)
			if !ok {
				continue
			}
			a.appendConv(proxy.Message{Role: "user", Content: StrPtr(
				"Context module available: " + name + "\n" + line +
					"\nCall load_module(\"" + name + "\") to load its tools, then call_tool(name, arguments) to use them.")})
		}
	}
}
