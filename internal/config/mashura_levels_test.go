package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMashuraBlockValidation pins the load-time rejections: unknown tool
// names and levels fail with a clear message.
func TestMashuraBlockValidation(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr string
	}{
		{"unknown tool", `{"mashura":{"level":"medium","overrides":{"revieww":{"level":"low"}}}}`, "unknown tool"},
		{"unknown block level", `{"mashura":{"level":"ultra"}}`, "mashura.level must be one of"},
		{"unknown override level", `{"mashura":{"level":"medium","overrides":{"check":{"level":"extreme"}}}}`, "must be one of"},
		{"valid full", `{"mashura":{"level":"medium","members":["openrouter:a/b"],"overrides":{"check":{"level":"low","members":["openrouter:a/b"]}}}}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			if err := json.Unmarshal([]byte(tc.raw), &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			err := validateEnums(cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestResolveEffectivePrecedence pins A3: off (any source) > per-tool
// override > per-repo level > config level.
func TestResolveEffectivePrecedence(t *testing.T) {
	block := &MashuraConfig{
		Level:   "medium",
		Members: []string{"openrouter:a/b"},
		Overrides: map[string]MashuraOverride{
			"check": {Level: "low", Members: []string{"openrouter:c/d"}},
		},
	}
	cfg := Config{Mashura: block}
	cases := []struct {
		tool      string
		repoLevel string
		wantLevel string
		wantMem   string
	}{
		{"review", "", "medium", "openrouter:a/b"},
		{"review", "high", "high", "openrouter:a/b"},
		{"check", "", "low", "openrouter:c/d"},
		{"check", "high", "low", "openrouter:c/d"}, // override beats per-repo
		{"review", "off", "off", "openrouter:a/b"}, // off from repo wins over everything
	}
	for _, tc := range cases {
		level, members, _ := cfg.ResolveEffective(tc.tool, tc.repoLevel)
		if level != tc.wantLevel {
			t.Errorf("tool %s repo %s: level = %s, want %s", tc.tool, tc.repoLevel, level, tc.wantLevel)
		}
		if len(members) != 1 || members[0] != tc.wantMem {
			t.Errorf("tool %s: members = %v, want [%s]", tc.tool, members, tc.wantMem)
		}
	}
	// Block-level off beats a per-tool override.
	offCfg := Config{Mashura: &MashuraConfig{Level: "off", Members: []string{"openrouter:a/b"}, Overrides: map[string]MashuraOverride{"check": {Level: "low"}}}}
	if level, _, _ := offCfg.ResolveEffective("check", ""); level != "off" {
		t.Errorf("block off should win: got %s", level)
	}
	if level, _, _ := offCfg.ResolveEffective("review", "high"); level != "off" {
		t.Errorf("block off should win over repo high: got %s", level)
	}
}

// TestLevelReasoning pins the per-level reasoning and the check exception
// (check stays at low under low and medium).
func TestLevelReasoning(t *testing.T) {
	cases := []struct {
		tool, level string
		wantEffort  string
	}{
		{"review", "off", "none"},
		{"review", "low", "none"},
		{"review", "medium", "medium"},
		{"review", "high", "max"},
		{"check", "low", "low"},
		{"check", "medium", "low"},
		{"check", "high", "max"},
	}
	for _, tc := range cases {
		r := LevelReasoning(tc.tool, tc.level)
		if r == nil || r.Effort != tc.wantEffort {
			t.Errorf("LevelReasoning(%s,%s) = %v, want effort %s", tc.tool, tc.level, r, tc.wantEffort)
		}
	}
}

// TestLevelDebateAndAutoCounsel pins the level table: debate only at high,
// auto-counsel ceiling off/suggest/auto.
func TestLevelDebateAndAutoCounsel(t *testing.T) {
	debate := map[string]bool{"off": false, "low": false, "medium": false, "high": true}
	ceiling := map[string]string{"off": "off", "low": "suggest", "medium": "auto", "high": "auto"}
	for lv := range debate {
		if got := LevelDebate(lv); got != debate[lv] {
			t.Errorf("LevelDebate(%s) = %v, want %v", lv, got, debate[lv])
		}
		if got := LevelAutoCounsel(lv); got != ceiling[lv] {
			t.Errorf("LevelAutoCounsel(%s) = %s, want %s", lv, got, ceiling[lv])
		}
	}
}

// TestMashuraDefaultMatchesGate pins the default when neither the new block
// nor any old key is present. Today the tools are offered once the
// OpenRouter key exists (BuildTools + mashuraAvailable), so the default
// level is medium with the level/key predicate. This test fails loudly if
// the default ever stops matching that behavior.
func TestMashuraDefaultMatchesGate(t *testing.T) {
	if MashuraLevelDefault != MashuraLevelMedium {
		t.Fatalf("MashuraLevelDefault = %s, want medium (today's gate offers tools once the OpenRouter key exists)", MashuraLevelDefault)
	}
	cfg := Config{
		Mashura: &MashuraConfig{Level: "", Members: []string{"openrouter:a/b"}},
	}
	if level, members, _ := cfg.ResolveEffective("review", ""); level != "medium" || len(members) == 0 {
		t.Fatalf("default resolve = (%s, %v), want (medium, non-empty)", level, members)
	}
}
