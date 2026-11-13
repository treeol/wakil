package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// hasMashuraTool reports whether tools contains mashura__review.
func hasMashuraTool(tools []proxy.Tool) bool {
	for _, t := range tools {
		if t.Function.Name == "mashura__review" {
			return true
		}
	}
	return false
}

// TestBuildToolsMashuraLevelGate pins Group C4's unified predicate: with the
// new block, tools are offered iff effective level != off AND effective member
// list non-empty (fallback member counts) AND the OpenRouter key is present.
// oracle_api_key_env is not part of it.
func TestBuildToolsMashuraLevelGate(t *testing.T) {
	// The gate reads the OpenRouter key named by the config's
	// openrouter_api_key_env — the test configs below must name it, or the
	// "" name reads nothing. Set the named var.
	t.Setenv("OPENROUTER_API_KEY", "x")

	members := []string{"openrouter:a/b"}
	keyEnvCfg := func(c config.Config) config.Config { c.OpenRouterAPIKeyEnv = "OPENROUTER_API_KEY"; return c }
	cases := []struct {
		name string
		cfg  config.Config
		repo string
		want bool
	}{
		{"level medium with key", keyEnvCfg(config.Config{Mashura: &config.MashuraConfig{Level: "medium", Members: members}}), "", true},
		{"level off", keyEnvCfg(config.Config{Mashura: &config.MashuraConfig{Level: "off", Members: members}}), "", false},
		{"repo off overrides config medium", keyEnvCfg(config.Config{Mashura: &config.MashuraConfig{Level: "medium", Members: members}}), "off", false},
		// Empty members falls back to the lazy fallback member (A4) — tools
		// ARE offered, matching today's fallback-based behavior.
		{"empty members (fallback counts)", keyEnvCfg(config.Config{Mashura: &config.MashuraConfig{Level: "medium"}}), "", true},
		{"empty level defaults medium", keyEnvCfg(config.Config{Mashura: &config.MashuraConfig{Members: members}}), "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hasMashuraTool(BuildTools(tc.cfg, "/work", nil, tc.repo))
			if got != tc.want {
				t.Errorf("BuildTools mashura present = %v, want %v", got, tc.want)
			}
		})
	}

	// Without the key, no tools even at medium (unified predicate).
	t.Setenv("OPENROUTER_API_KEY", "")
	if got := hasMashuraTool(BuildTools(keyEnvCfg(config.Config{Mashura: &config.MashuraConfig{Level: "medium", Members: members}}), "/work", nil, "")); got {
		t.Error("tools must not appear without the OpenRouter key")
	}
}

// TestMashuraLevelStatusShowsCheckException pins that the per-tool table in
// /mashura states check's exception explicitly (user requirement #3).
func TestMashuraLevelStatusShowsCheckException(t *testing.T) {
	app, _ := mashuraTestApp(t, "", "", "")
	app.Cfg.Mashura = &config.MashuraConfig{Level: "medium", Members: []string{"openrouter:a/b"}}
	s := mashuraStatus(app)
	if !strings.Contains(s, "level:") || !strings.Contains(s, "effective per tool") {
		t.Fatalf("status missing level survey:\n%s", s)
	}
	if !strings.Contains(s, "check stays at low below high") {
		t.Fatalf("status must state check stays at low below high:\n%s", s)
	}
	if !strings.Contains(s, "\"mashura\":") {
		t.Fatalf("status must print the copyable config block:\n%s", s)
	}
}

// TestRunMashuraCoreLevelOffRefuses pins the run-time refusal when the
// effective level is off (before any source reads).
func TestRunMashuraCoreLevelOffRefuses(t *testing.T) {
	app, _ := mashuraTestApp(t, "", "", "")
	t.Setenv("MASHURA_TEST_OR_KEY", "x")
	app.Cfg = config.Config{OpenRouterAPIKeyEnv: "MASHURA_TEST_OR_KEY", OracleModel: "z"}
	app.Cfg.Mashura = &config.MashuraConfig{Level: "off", Members: []string{"openrouter:a/b"}}
	res := app.runMashuraCore(context.Background(), "mashura__review", tcArgs("mashura__review", `{"focus":"x"}`), true)
	if !strings.Contains(res, "Mashūra is off") {
		t.Fatalf("want level-off refusal, got: %s", res)
	}
}

// TestMashuraReasoningForLevel pins that the level owns reasoning when the
// new block is present, and roles own it otherwise.
func TestMashuraReasoningForLevel(t *testing.T) {
	app, _ := mashuraTestApp(t, "", "", "")
	app.Cfg = config.Config{}
	app.Cfg.Mashura = &config.MashuraConfig{Level: "medium"}
	r := app.mashuraReasoningFor("mashura__review")
	if r == nil || r.Effort != "medium" {
		t.Fatalf("review at medium = %v, want effort medium", r)
	}
	r = app.mashuraReasoningFor("mashura__check")
	if r == nil || r.Effort != "low" {
		t.Fatalf("check at medium = %v, want effort low", r)
	}
	// Override check -> high follows the level.
	app.Cfg.Mashura.Overrides = map[string]config.MashuraOverride{"check": {Level: "high"}}
	r = app.mashuraReasoningFor("mashura__check")
	if r == nil || r.Effort != "max" {
		t.Fatalf("check override high = %v, want effort max", r)
	}
}
