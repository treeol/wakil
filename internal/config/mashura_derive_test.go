package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestDeriveMashuraFromLegacy is the B3 table test: the seven required cases.
func TestDeriveMashuraFromLegacy(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantLevel  string
		wantMember string // "", "a/b", "c/d"...
		wantOvTool string
		wantOvLvl  string
		wantOvMem  string
		wantNone   bool // expect no block at all (neither present)
	}{
		{
			name:       "only new block",
			raw:        `{"mashura":{"level":"high","members":["openrouter:a/b"]}}`,
			wantLevel:  "high",
			wantMember: "openrouter:a/b",
		},
		{
			name:       "only old keys with one shared panel for all tools",
			raw:        `{"oracle_enabled":true,"mashura_panels":{"default":{"models":["openrouter:a/b"],"mode":"panel"}}}`,
			wantLevel:  "medium",
			wantMember: "openrouter:a/b",
		},
		{
			name:       "old keys with a separate panel for check",
			raw:        `{"oracle_enabled":true,"mashura_panels":{"default":{"models":["openrouter:a/b"]},"cheap":{"models":["openrouter:c/d"],"mode":"panel"}},"mashura_tool_panels":{"check":"cheap"}}`,
			wantLevel:  "medium",
			wantMember: "openrouter:a/b",
			wantOvTool: "check",
			wantOvMem:  "openrouter:c/d",
		},
		{
			name:      "old keys with oracle_enabled false",
			raw:       `{"oracle_enabled":false,"mashura_panels":{"default":{"models":["openrouter:a/b"]}}}`,
			wantLevel: "off",
		},
		{
			name:       "old keys with a fusion panel",
			raw:        `{"oracle_enabled":true,"mashura_panels":{"default":{"models":["openrouter:a/b"],"mode":"fusion"}}}`,
			wantLevel:  "medium",
			wantMember: "openrouter:a/b",
		},
		{
			name:       "only new block wins over old keys",
			raw:        `{"mashura":{"level":"low","members":["openrouter:c/d"]},"mashura_panels":{"default":{"models":["openrouter:a/b"]}}}`,
			wantLevel:  "low",
			wantMember: "openrouter:c/d",
		},
		{
			name:     "neither present",
			raw:      `{}`,
			wantNone: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := DefaultConfig()
			if err := json.Unmarshal([]byte(tc.raw), &cfg); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			// Re-run the loader's derivation step (without the notice write).
			if cfg.Mashura == nil && len(legacyMashuraKeysPresent(cfg)) > 0 {
				cfg.Mashura = deriveMashuraFromLegacy(cfg)
			}
			if tc.wantNone {
				if cfg.Mashura != nil {
					t.Fatalf("want no block, got %+v", cfg.Mashura)
				}
				return
			}
			m := cfg.Mashura
			if m == nil {
				t.Fatal("want a block, got none")
			}
			if m.Level != tc.wantLevel {
				t.Errorf("level = %s, want %s", m.Level, tc.wantLevel)
			}
			if tc.wantMember != "" {
				if len(m.Members) != 1 || m.Members[0] != tc.wantMember {
					t.Errorf("members = %v, want [%s]", m.Members, tc.wantMember)
				}
			}
			if tc.wantOvTool != "" {
				ov, ok := m.Overrides[tc.wantOvTool]
				if !ok {
					t.Fatalf("missing override for %s", tc.wantOvTool)
				}
				if tc.wantOvLvl != "" && ov.Level != tc.wantOvLvl {
					t.Errorf("override level = %s, want %s", ov.Level, tc.wantOvLvl)
				}
				if tc.wantOvMem != "" && (len(ov.Members) != 1 || ov.Members[0] != tc.wantOvMem) {
					t.Errorf("override members = %v, want [%s]", ov.Members, tc.wantOvMem)
				}
			}
			// The legacy file must never be rewritten: rewriting is not this
			// package's job (LoadConfig only reads). This documents the rule.
			_ = strings.Contains(tc.raw, `"mashura"`)
		})
	}
}

// TestDeriveMashuraFromLegacyRoleEffort pins the role → override mapping:
// none/low → low, medium → medium, high+ → high; a role with only a token
// number creates no override.
func TestDeriveMashuraFromLegacyRoleEffort(t *testing.T) {
	cases := []struct {
		effort string
		want   string
	}{
		{"none", "low"}, {"low", "low"}, {"medium", "medium"}, {"high", "high"}, {"xhigh", "high"}, {"max", "high"},
	}
	for _, tc := range cases {
		cfg := DefaultConfig()
		cfg.OracleEnabled = true
		cfg.MashuraPanels = map[string]MashuraPanelConfig{"default": {Models: []string{"openrouter:a/b"}}}
		cfg.MashuraRoles = map[string]MashuraRoleConfig{"reviewer": {Reasoning: &ReasoningSpec{Effort: tc.effort}}}
		m := deriveMashuraFromLegacy(cfg)
		ov, ok := m.Overrides["review"]
		if !ok || ov.Level != tc.want {
			t.Errorf("effort %s → review override = %+v, want level %s", tc.effort, ov, tc.want)
		}
	}
	// Token-only role creates no override.
	cfg := DefaultConfig()
	cfg.OracleEnabled = true
	cfg.MashuraPanels = map[string]MashuraPanelConfig{"default": {Models: []string{"openrouter:a/b"}}}
	cfg.MashuraRoles = map[string]MashuraRoleConfig{"reviewer": {Reasoning: &ReasoningSpec{MaxTokens: 4096}}}
	if m := deriveMashuraFromLegacy(cfg); len(m.Overrides) != 0 {
		t.Errorf("token-only role must create no override, got %+v", m.Overrides)
	}
}
