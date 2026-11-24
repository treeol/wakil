package agent

import (
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// mashuraModuleCfg returns a config that offers the Mashūra tools: a mashura
// block at level medium and an OpenRouter key present in the environment.
func mashuraModuleCfg(t *testing.T, modules string) config.Config {
	t.Helper()
	t.Setenv("WAKIL_TEST_OR_KEY", "x")
	cfg := config.DefaultConfig()
	cfg.OpenRouterAPIKeyEnv = "WAKIL_TEST_OR_KEY"
	cfg.Mashura = &config.MashuraConfig{Level: "medium", Members: []string{"openrouter:a/b"}}
	cfg.ContextModules = modules
	return cfg
}

// mashuraDefs returns the Mashūra tool definitions found in tools, by name.
func mashuraDefs(tools []proxy.Tool) map[string]proxy.Tool {
	out := map[string]proxy.Tool{}
	for _, tl := range tools {
		switch tl.Function.Name {
		case "mashura__review", "mashura__debug", "mashura__decide", "mashura__check":
			out[tl.Function.Name] = tl
		}
	}
	return out
}

// TestMashuraInCoreToolsModulesOn: with context_modules on, the four Mashūra
// tools are in the tool array at session start (core), never behind
// load_module, and no module owns them.
func TestMashuraInCoreToolsModulesOn(t *testing.T) {
	cfg := mashuraModuleCfg(t, "on")
	core, reg := BuildToolsWithModules(cfg, "/work", nil, "")
	got := mashuraDefs(core)
	for _, name := range []string{"mashura__review", "mashura__debug", "mashura__decide", "mashura__check"} {
		if _, ok := got[name]; !ok {
			t.Errorf("%s missing from the core tool array with context_modules on", name)
		}
		if mod := reg.ToolModule(name); mod != "" {
			t.Errorf("%s is owned by module %q; Mashūra must never be a module", name, mod)
		}
	}
	// BuildTools (the wiring entry point) must agree with the builder.
	if viaBuildTools := mashuraDefs(BuildTools(cfg, "/work", nil, "")); len(viaBuildTools) != 4 {
		t.Errorf("BuildTools with modules on carries %d Mashūra tools, want 4", len(viaBuildTools))
	}
	// The app's tool array served each turn is the frozen core array.
	app := &App{Cfg: cfg, Tools: core}
	if n := len(mashuraDefs(app.toolsForTurn())); n != 4 {
		t.Errorf("toolsForTurn carries %d Mashūra tools, want 4", n)
	}
}

// TestMashuraToolsSameBothModes: the Mashūra tool definitions are
// byte-identical with modules on and off.
func TestMashuraToolsSameBothModes(t *testing.T) {
	off := mashuraDefs(BuildTools(mashuraModuleCfg(t, "off"), "/work", nil, ""))
	on := mashuraDefs(BuildTools(mashuraModuleCfg(t, "on"), "/work", nil, ""))
	if len(off) != 4 || len(on) != 4 {
		t.Fatalf("Mashūra tool count off=%d on=%d, want 4 and 4", len(off), len(on))
	}
	for name, o := range off {
		offJSON, _ := json.Marshal(o)
		onJSON, _ := json.Marshal(on[name])
		if string(offJSON) != string(onJSON) {
			t.Errorf("%s differs between modes:\noff: %s\non:  %s", name, offJSON, onJSON)
		}
	}
}

// TestMashuraOffWhenNotOffered: the predicate, not the mode, decides. With the
// level off there is no Mashūra tool in either mode.
func TestMashuraOffWhenNotOffered(t *testing.T) {
	for _, mode := range []string{"off", "on"} {
		cfg := mashuraModuleCfg(t, mode)
		cfg.Mashura.Level = "off"
		if n := len(mashuraDefs(BuildTools(cfg, "/work", nil, ""))); n != 0 {
			t.Errorf("modules %s, level off: %d Mashūra tools offered, want 0", mode, n)
		}
	}
}

// TestEveryFullSetToolReachableWithModulesOn: every tool in the v0.8.10 full
// array (modules off) is either in the core array or inside a module when
// modules are on. A tool in neither is silently lost to the model.
func TestEveryFullSetToolReachableWithModulesOn(t *testing.T) {
	mk := func(mode string) config.Config {
		cfg := mashuraModuleCfg(t, mode)
		cfg.SearXngURL = "http://searx.invalid"
		cfg.LSPEnabled = true
		cfg.BrowserEnabled = true
		return cfg
	}
	mcp := newFakeMCPManager(
		connectedServer("trello", &fakeMCPSession{}, "get_boards", "get_card"),
		connectedServer("context7", &fakeMCPSession{}, "query-docs"),
	)
	full := BuildTools(mk("off"), "/work", mcp, "")
	core, reg := BuildToolsWithModules(mk("on"), "/work", mcp, "")

	reachable := map[string]bool{}
	for _, tl := range core {
		reachable[tl.Function.Name] = true
	}
	for _, name := range reg.ModuleNames() {
		tools, _, err := reg.LoadModule(name)
		if err != nil {
			t.Fatalf("LoadModule(%q): %v", name, err)
		}
		for _, tl := range tools {
			reachable[tl.Function.Name] = true
		}
	}
	var lost []string
	for _, tl := range full {
		if !reachable[tl.Function.Name] {
			lost = append(lost, tl.Function.Name)
		}
	}
	sort.Strings(lost)
	if len(lost) > 0 {
		t.Errorf("tools in the full set but in neither the core array nor a module: %v", lost)
	}
}

// TestCallToolRejectsWrappedMashuraAsCore: call_tool wrapping a tool that IS
// in the tool array (Mashūra) must say so, not "not in any module".
func TestCallToolRejectsWrappedMashuraAsCore(t *testing.T) {
	cfg := mashuraModuleCfg(t, "on")
	core, reg := BuildToolsWithModules(cfg, "/work", nil, "")
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg = cfg
	app.Tools = core
	app.Modules = reg
	tc := proxy.ToolCall{ID: "c1", Type: "function", Function: proxy.FunctionCall{
		Name:      "call_tool",
		Arguments: `{"name":"mashura__review","arguments":"{}"}`,
	}}
	_, res := app.callToolGate(tc)
	if res == nil {
		t.Fatal("call_tool wrapping mashura__review was not rejected")
	}
	if !strings.Contains(res.text, "already in the tool array") {
		t.Errorf("error = %q, want the core-tool message (not \"not in any module\")", res.text)
	}
}

// TestMashuraNotOfferedReason: the diag reason names the failing predicate
// half and never carries a credential value.
func TestMashuraNotOfferedReason(t *testing.T) {
	t.Setenv("WAKIL_TEST_OR_KEY", "")
	cfg := config.DefaultConfig()
	cfg.OpenRouterAPIKeyEnv = "WAKIL_TEST_OR_KEY"
	cfg.Mashura = &config.MashuraConfig{Level: "medium", Members: []string{"openrouter:a/b"}}
	if got := mashuraNotOfferedReason(cfg, ""); !strings.Contains(got, "no OpenRouter credential") {
		t.Errorf("no key: reason = %q", got)
	}
	cfg.Mashura.Level = "off"
	if got := mashuraNotOfferedReason(cfg, ""); got != "effective mashura level is off" {
		t.Errorf("level off: reason = %q", got)
	}
	t.Setenv("WAKIL_TEST_OR_KEY", "sk-secret-value")
	cfg.Mashura.Level = "medium"
	if got := mashuraNotOfferedReason(cfg, ""); got != "" {
		t.Errorf("offered: reason = %q, want empty", got)
	}
	legacy := config.DefaultConfig()
	legacy.OpenRouterAPIKeyEnv = "WAKIL_TEST_OR_KEY"
	if got := mashuraNotOfferedReason(legacy, ""); !strings.Contains(got, "oracle_enabled is false") {
		t.Errorf("legacy off: reason = %q", got)
	}
}
