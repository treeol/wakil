package config

import (
	"strings"
	"testing"
)

// TestLegacyMashuraKeysSuperseded pins the Group 2 classification: with the
// mashura block present, ONLY the superseded legacy keys have no effect
// (and are the ones the load notice names). Every other legacy key is still
// a valid setting that keeps working.
func TestLegacyMashuraKeysSuperseded(t *testing.T) {
	def := DefaultConfig()
	cfg := def
	cfg.OracleEnabled = true
	cfg.OracleAPIKeyEnv = "ANOTHER_KEY"
	cfg.MashuraRoles = map[string]MashuraRoleConfig{"reviewer": {Reasoning: &ReasoningSpec{MaxTokens: 12288}}}
	cfg.MashuraPanels = map[string]MashuraPanelConfig{"default": {Models: []string{"anthropic:claude-opus-4-8"}}}
	cfg.MashuraToolPanels = map[string]string{"review": "default"}
	// still-valid keys
	cfg.OracleModel = "claude-opus-4-8"
	cfg.OracleMaxTokens = 2048
	cfg.MashuraMaxTokens = 2048
	cfg.MashuraToolMaxTokens = map[string]int{"check": 512}
	cfg.MashuraModels = map[string]MashuraModelLimits{"anthropic/model": {ContextTokens: 1000}}
	cfg.OracleTimeoutSeconds = 120
	cfg.MashuraTimeoutSeconds = 120
	cfg.MashuraReviewBudgetUSD = 0.5
	cfg.MashuraFallbackModel = "openrouter:anthropic/claude-sonnet-4"
	cfg.MashuraMode = "stress"
	cfg.AutoCounsel = "auto"
	cfg.CounselMaxPerSession = 5
	cfg.WFOracleMode = "stress"
	cfg.WFFinalReview = !def.WFFinalReview // per-key default may already be true; deviation is how presence is detected
	cfg.WFBriefingMaxBytes = 4096

	wantSuperseded := map[string]bool{
		"oracle_enabled":      true,
		"oracle_api_key_env":  true,
		"mashura_roles":       true,
		"mashura_panels":      true,
		"mashura_tool_panels": true,
	}
	got := legacyMashuraKeysSuperseded(cfg)
	if len(got) != len(wantSuperseded) {
		t.Fatalf("superseded = %v, want exactly %v", got, wantSuperseded)
	}
	for _, n := range got {
		if !wantSuperseded[n] {
			t.Errorf("superseded contains %q (a still-valid key) — the notice would falsely call it ignored", n)
		}
		if !contains(legacyMashuraKeysPresent(cfg), n) {
			t.Errorf("superseded %q missing from legacyMashuraKeysPresent", n)
		}
	}
	// Every still-valid key is reported present (and must not be named
	// "ignored").
	for _, n := range []string{"oracle_model", "oracle_max_tokens", "mashura_max_tokens",
		"mashura_max_tokens_by_tool", "mashura_models", "oracle_timeout_seconds",
		"mashura_timeout_seconds", "mashura_review_budget_usd", "mashura_fallback_model",
		"mashura_mode", "auto_counsel", "counsel_max_per_session", "wf_oracle_mode",
		"wf_final_review", "wf_briefing_max_bytes"} {
		if !contains(legacyMashuraKeysPresent(cfg), n) {
			t.Errorf("legacyMashuraKeysPresent missing still-valid key %q", n)
		}
		if contains(got, n) {
			t.Errorf("superseded must not contain still-valid key %q", n)
		}
	}
}

// TestLegacyMashuraNoticeReasoningNumbers pins the derived-from-legacy
// notice content: it must name the reviewer reasoning budget change with
// the old and new numbers.
func TestLegacyMashuraNoticeReasoningNumbers(t *testing.T) {
	notice := legacyMashuraDerivedNoticeText()
	if !strings.Contains(notice, "8192") {
		t.Errorf("notice must name the old budget 8192: %q", notice)
	}
	if !strings.Contains(notice, "4096") {
		t.Errorf("notice must name the new budget 4096: %q", notice)
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
