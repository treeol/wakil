package agent

import (
	"io"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// TestMashuraOfferedAvailableAgree is the Group 1 unification pin: for a
// matrix of level, members, key presence and legacy shapes, "the tools are
// offered in the tool list" (BuildTools) must always equal "the tools are
// available when called" (App.mashuraAvailable). Both go through
// mashuraOffered.
func TestMashuraOfferedAvailableAgree(t *testing.T) {
	t.Setenv("TEST_OR_KEY", "x")
	t.Setenv("TEST_ANTH_KEY", "x")

	members := []string{"openrouter:test-model-a", "openrouter:test-model-b"}

	keyConfs := []struct {
		name    string
		orEnv   string // env var name that holds the OR key, "" = unset
		orSet   bool
		anthEnv string // env var name that holds the Anthropic key, "" = unset
		anthSet bool
	}{
		{"or_only", "TEST_OR_KEY", true, "", false},
		{"anthropic_only", "", false, "TEST_ANTH_KEY", true},
		{"both", "TEST_OR_KEY", true, "TEST_ANTH_KEY", true},
		{"none", "", false, "", false},
	}
	// Note: OpenRouterAPIKeyEnv / OracleAPIKeyEnv may be redirected per row;
	// the "set" columns set the env var contents via t.Setenv below.

	type shape struct {
		name string
		cfg  config.Config
		repo string
	}
	shapes := []shape{}
	for _, lv := range []string{"off", "low", "medium", "high", ""} {
		shapes = append(shapes, shape{
			name: "block_level_" + lv,
			cfg:  config.Config{Mashura: &config.MashuraConfig{Level: lv, Members: members}},
			repo: "",
		})
	}
	shapes = append(shapes,
		shape{name: "block_empty_members", cfg: config.Config{Mashura: &config.MashuraConfig{Level: "medium"}}, repo: ""},
		shape{name: "block_repo_off", cfg: config.Config{Mashura: &config.MashuraConfig{Level: "medium", Members: members}}, repo: "off"},
		shape{name: "block_repo_high_over_off", cfg: config.Config{Mashura: &config.MashuraConfig{Level: "off", Members: members}}, repo: "high"},
		shape{name: "legacy_enabled_true", cfg: config.Config{OracleEnabled: true, OracleModel: "claude-sonnet-4-6"}, repo: ""},
		shape{name: "legacy_enabled_false", cfg: config.Config{OracleEnabled: false, OracleModel: "claude-sonnet-4-6"}, repo: ""},
		shape{name: "legacy_panels_only", cfg: config.Config{MashuraPanels: map[string]config.MashuraPanelConfig{"default": {Models: []string{"anthropic:claude-opus-4-8"}}}}, repo: ""},
		shape{name: "legacy_model_only", cfg: config.Config{OracleModel: "claude-sonnet-4-6"}, repo: ""},
		shape{name: "no_keys_at_all", cfg: config.Config{}, repo: ""},
	)

	for _, kc := range keyConfs {
		for _, s := range shapes {
			t.Run(kc.name+"/"+s.name, func(t *testing.T) {
				// Wire the key env names in the config copy and set the
				// env contents for this row.
				cfg := s.cfg
				cfg.OpenRouterAPIKeyEnv = "TEST_OR_KEY"
				cfg.OracleAPIKeyEnv = "TEST_ANTH_KEY"
				if !kc.orSet {
					t.Setenv("TEST_OR_KEY", "")
				}
				if !kc.anthSet {
					t.Setenv("TEST_ANTH_KEY", "")
				}

				offered := hasMashuraTool(BuildTools(cfg, "/work", nil, s.repo))
				app := &App{Cfg: cfg, Out: io.Discard, Client: &proxy.Client{}, MashuraLevelOverride: s.repo}
				available := app.mashuraAvailable()
				if offered != available {
					t.Errorf("offered=%v available=%v: predicates disagree", offered, available)
				}
			})
		}
	}
}
