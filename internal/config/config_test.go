package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------- cost helpers ----------------

func TestMashuraCost(t *testing.T) {
	c := CostsConfig{Mashura: map[string]ModelRate{
		"gpt":  {InputUSDPer1M: 10, OutputUSDPer1M: 30},
		"zero": {},
	}}
	usd, priced := c.MashuraCost("gpt", 1_000_000, 1_000_000)
	if !priced || usd != 40 {
		t.Errorf("MashuraCost = %v, %v; want 40, true", usd, priced)
	}
	if _, priced := c.MashuraCost("unknown", 1, 1); priced {
		t.Error("unknown model should be unpriced")
	}
	if _, priced := c.MashuraCost("zero", 1, 1); priced {
		t.Error("zero-rate model should be unpriced")
	}
}

func TestInferenceCost(t *testing.T) {
	var c CostsConfig
	if _, priced := c.InferenceCost(1000); priced {
		t.Error("unset inference rate should be unpriced")
	}
	c.Inference.USDPer1MTokens = 2
	usd, priced := c.InferenceCost(500_000)
	if !priced || usd != 1 {
		t.Errorf("InferenceCost = %v, %v; want 1, true", usd, priced)
	}
}

func TestExternalInferenceCost(t *testing.T) {
	c := CostsConfig{InferenceBackends: map[string]ModelRate{
		"openrouter/x/y": {InputUSDPer1M: 1, OutputUSDPer1M: 2},
	}}
	usd, priced := c.ExternalInferenceCost("openrouter/x/y", 2_000_000, 1_000_000, TokenDetail{})
	if !priced || usd != 4 {
		t.Errorf("ExternalInferenceCost = %v, %v; want 4, true", usd, priced)
	}
	if _, priced := c.ExternalInferenceCost("nope", 1, 1, TokenDetail{}); priced {
		t.Error("unknown backend/model should be unpriced")
	}
}

// TestExternalInferenceCostWithCachedTokens verifies the split-rate formula
// when a CachedInputUSDPer1M discount is configured: cached tokens bill at
// the cached rate, the remaining (uncached) input tokens at the normal input
// rate, output unaffected.
func TestExternalInferenceCostWithCachedTokens(t *testing.T) {
	c := CostsConfig{InferenceBackends: map[string]ModelRate{
		"openrouter/x/y": {InputUSDPer1M: 10, OutputUSDPer1M: 30, CachedInputUSDPer1M: 2},
	}}
	// 1M total prompt tokens, 400k of them cached, 100k output.
	// = 600k/1e6*10 + 400k/1e6*2 + 100k/1e6*30 = 6 + 0.8 + 3 = 9.8
	usd, priced := c.ExternalInferenceCost("openrouter/x/y", 1_000_000, 100_000, TokenDetail{CachedTok: 400_000})
	if !priced {
		t.Fatal("expected priced=true")
	}
	if usd != 9.8 {
		t.Errorf("usd = %v, want 9.8", usd)
	}
}

// TestExternalInferenceCostCachedTokensDefaultToInputRateWhenUnset verifies
// that passing a nonzero cachedTok with NO CachedInputUSDPer1M configured
// produces byte-identical cost arithmetic to the pre-cache-accounting
// formula (cached tokens billed at the plain input rate) — the golden
// "unconfigured stays unchanged" guarantee.
func TestExternalInferenceCostCachedTokensDefaultToInputRateWhenUnset(t *testing.T) {
	c := CostsConfig{InferenceBackends: map[string]ModelRate{
		"openrouter/x/y": {InputUSDPer1M: 10, OutputUSDPer1M: 30}, // no CachedInputUSDPer1M
	}}
	withoutCached, _ := c.ExternalInferenceCost("openrouter/x/y", 1_000_000, 100_000, TokenDetail{})
	withCached, priced := c.ExternalInferenceCost("openrouter/x/y", 1_000_000, 100_000, TokenDetail{CachedTok: 400_000})
	if !priced {
		t.Fatal("expected priced=true")
	}
	if withCached != withoutCached {
		t.Errorf("cost with cachedTok=400_000 (no cached rate configured) = %v, want %v (identical to the cache-unaware call)", withCached, withoutCached)
	}
}

// TestExternalInferenceCostCacheWriteTokens verifies the split-rate formula
// when a CacheWriteUSDPer1M rate is configured: write tokens bill at the write
// rate, uncached input at the input rate, cached at the cached rate, output
// at the output rate. Write tokens are treated as additive (not inside
// prompt_tokens).
func TestExternalInferenceCostCacheWriteTokens(t *testing.T) {
	c := CostsConfig{InferenceBackends: map[string]ModelRate{
		"openrouter/x/y": {InputUSDPer1M: 10, OutputUSDPer1M: 30, CachedInputUSDPer1M: 2, CacheWriteUSDPer1M: 12.5},
	}}
	// 1M prompt tokens (600k uncached + 400k cached), 200k write tokens, 100k output.
	// = 600k/1e6*10 + 400k/1e6*2 + 200k/1e6*12.5 + 100k/1e6*30
	// = 6 + 0.8 + 2.5 + 3 = 12.3
	usd, priced := c.ExternalInferenceCost("openrouter/x/y", 1_000_000, 100_000, TokenDetail{
		CachedTok:     400_000,
		CacheWriteTok: 200_000,
	})
	if !priced {
		t.Fatal("expected priced=true")
	}
	if usd != 12.3 {
		t.Errorf("usd = %v, want 12.3", usd)
	}
}

// TestExternalInferenceCostCacheWriteUnsetDefaultsToInputRate verifies that
// when CacheWriteUSDPer1M is unset (0), write tokens are billed at
// InputUSDPer1M — the golden "unconfigured stays unchanged" guarantee for
// the write side.
func TestExternalInferenceCostCacheWriteUnsetDefaultsToInputRate(t *testing.T) {
	c := CostsConfig{InferenceBackends: map[string]ModelRate{
		"openrouter/x/y": {InputUSDPer1M: 10, OutputUSDPer1M: 30}, // no CacheWriteUSDPer1M
	}}
	// Without write tokens, without cached:
	// 1M input * 10 + 100k output * 30 = 10 + 3 = 13
	withoutWrite, _ := c.ExternalInferenceCost("openrouter/x/y", 1_000_000, 100_000, TokenDetail{})
	// With 200k write tokens (billed at InputUSDPer1M=10 since unset):
	// 1M input * 10 + 200k write * 10 + 100k output * 30 = 10 + 2 + 3 = 15
	withWrite, priced := c.ExternalInferenceCost("openrouter/x/y", 1_000_000, 100_000, TokenDetail{CacheWriteTok: 200_000})
	if !priced {
		t.Fatal("expected priced=true")
	}
	// The difference should be exactly 200k/1e6*10 = 2.0
	if withWrite-withoutWrite != 2.0 {
		t.Errorf("cost delta from write tokens = %v, want 2.0 (200k/1M * InputUSDPer1M=10)", withWrite-withoutWrite)
	}
}

func TestSearchCost(t *testing.T) {
	var c CostsConfig
	if _, priced := c.SearchCost(); priced {
		t.Error("unset search rate should be unpriced")
	}
	c.Search.USDPerQuery = 0.005
	usd, priced := c.SearchCost()
	if !priced || usd != 0.005 {
		t.Errorf("SearchCost = %v, %v; want 0.005, true", usd, priced)
	}
}

// ---------------- subagent_endpoint validation ----------------

func TestValidateSubagentEndpoint(t *testing.T) {
	base := func() Config {
		c := DefaultConfig()
		c.Endpoints = map[string]EndpointConfig{
			"real": {Kind: EndpointKindOpenAI, BaseURL: "http://x", Model: "m"},
		}
		return c
	}

	if err := validateSubagentEndpoint(base()); err != nil {
		t.Errorf("no subagent_endpoint set: unexpected error %v", err)
	}

	c := base()
	c.SubagentEndpoint = "inherit"
	if err := validateSubagentEndpoint(c); err != nil {
		t.Errorf("subagent_endpoint=inherit: unexpected error %v", err)
	}

	c = base()
	c.SubagentEndpoint = "real"
	if err := validateSubagentEndpoint(c); err != nil {
		t.Errorf("subagent_endpoint naming a real key: unexpected error %v", err)
	}

	c = base()
	c.SubagentEndpoint = "missing"
	err := validateSubagentEndpoint(c)
	if err == nil {
		t.Fatal("expected error for missing subagent_endpoint key")
	}
	if !strings.Contains(err.Error(), "missing") {
		t.Errorf("error should name the missing key %q, got: %v", "missing", err)
	}
}

// TestNormalizeEndpointDefaultsAndErrors verifies NormalizeEndpoint applies
// the same defaulting rules as resolveEndpoint/handleEndpointSwitch.
func TestNormalizeEndpointDefaultsAndErrors(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Endpoints = map[string]EndpointConfig{
		"oa":         {Kind: EndpointKindOpenAI, BaseURL: "http://x", Model: "m"},
		"px-default": {Kind: EndpointKindIlmProxy, BaseURL: "http://y"}, // Model empty → defaults to "ilm"
		"oa-nomodel": {Kind: EndpointKindOpenAI, BaseURL: "http://z"},   // missing required model
		"nobase":     {Kind: EndpointKindOpenAI, Model: "m"},            // missing required base_url
		"badkind":    {Kind: "bogus", BaseURL: "http://w", Model: "m"},
	}

	if ep, err := cfg.NormalizeEndpoint("oa"); err != nil || ep.Model != "m" {
		t.Errorf("oa: ep=%+v err=%v", ep, err)
	}
	if ep, err := cfg.NormalizeEndpoint("px-default"); err != nil || ep.Model != "ilm" {
		t.Errorf("px-default: want Model=ilm default, got ep=%+v err=%v", ep, err)
	}
	if _, err := cfg.NormalizeEndpoint("oa-nomodel"); err == nil {
		t.Error("oa-nomodel: expected error (model required for openai)")
	}
	if _, err := cfg.NormalizeEndpoint("nobase"); err == nil {
		t.Error("nobase: expected error (base_url required)")
	}
	if _, err := cfg.NormalizeEndpoint("badkind"); err == nil {
		t.Error("badkind: expected error (unknown kind)")
	}
	if _, err := cfg.NormalizeEndpoint("nope"); err == nil {
		t.Error("nope: expected error (key not found)")
	}
}

// TestAuthHeaderForFallsBackToAPIKey verifies AuthHeaderFor mirrors AuthHeader
// for an arbitrary endpoint: the endpoint's own auth_header wins; otherwise
// the legacy api_key ("Bearer <key>") fallback applies.
func TestAuthHeaderForFallsBackToAPIKey(t *testing.T) {
	cfg := Config{APIKey: "k"}
	if got := cfg.AuthHeaderFor(EndpointConfig{}); got != "Bearer k" {
		t.Errorf("AuthHeaderFor(empty ep) = %q, want Bearer k (api_key fallback)", got)
	}
	if got := cfg.AuthHeaderFor(EndpointConfig{AuthHeader: "Custom xyz"}); got != "Custom xyz" {
		t.Errorf("AuthHeaderFor(ep with auth_header) = %q, want Custom xyz (endpoint wins)", got)
	}
}

// ---------------- small helpers ----------------

func TestExpandTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir in test env")
	}
	cases := map[string]string{
		"":            "",
		"~":           home,
		"~/x/y":       filepath.Join(home, "x/y"),
		"/abs/path":   "/abs/path",
		"rel/path":    "rel/path",
		"~user/notme": "~user/notme", // only bare ~ and ~/ expand
	}
	for in, want := range cases {
		if got := expandTilde(in); got != want {
			t.Errorf("expandTilde(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestEnvHelpers(t *testing.T) {
	t.Setenv("CFG_TEST_STR", "hello")
	s := "default"
	envStr(&s, "CFG_TEST_STR")
	if s != "hello" {
		t.Errorf("envStr: got %q", s)
	}
	s2 := "keep"
	envStr(&s2, "CFG_TEST_UNSET")
	if s2 != "keep" {
		t.Errorf("envStr should not overwrite on unset var; got %q", s2)
	}

	for val, want := range map[string]bool{"1": true, "true": true, "YES": true, "on": true,
		"0": false, "false": false, "No": false, "off": false} {
		t.Setenv("CFG_TEST_BOOL", val)
		b := !want // start from the opposite to prove it flips
		envBool(&b, "CFG_TEST_BOOL")
		if b != want {
			t.Errorf("envBool(%q) = %v; want %v", val, b, want)
		}
	}
	bKeep := true
	t.Setenv("CFG_TEST_BOOL", "garbage")
	envBool(&bKeep, "CFG_TEST_BOOL")
	if !bKeep {
		t.Error("envBool should ignore unrecognized values")
	}

	t.Setenv("CFG_TEST_INT", "42")
	n := 7
	envInt(&n, "CFG_TEST_INT")
	if n != 42 {
		t.Errorf("envInt: got %d", n)
	}
	t.Setenv("CFG_TEST_INT", "notanumber")
	m := 7
	envInt(&m, "CFG_TEST_INT")
	if m != 7 {
		t.Errorf("envInt should keep prior value on malformed input; got %d", m)
	}
}

func TestAuthHeader(t *testing.T) {
	if (Config{}).AuthHeader() != "" {
		t.Error("empty APIKey should yield empty header")
	}
	if got := (Config{APIKey: "k"}).AuthHeader(); got != "Bearer k" {
		t.Errorf("AuthHeader = %q", got)
	}
}

// ---------------- validateContextLimits ----------------

func validCfg() Config {
	c := DefaultConfig()
	return c
}

func TestValidateContextLimitsDefaultsOK(t *testing.T) {
	if err := validateContextLimits(validCfg()); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}
}

func TestValidateContextLimitsRejects(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantSub string
	}{
		{"compact_at zero", func(c *Config) { c.CompactAt = 0 }, "compact_at"},
		{"hard_max zero", func(c *Config) { c.HardMaxBytes = 0 }, "hard_max_bytes"},
		{"keep_bytes zero", func(c *Config) { c.KeepBytes = 0 }, "keep_bytes"},
		{"summary_bytes zero", func(c *Config) { c.SummaryBytes = 0 }, "summary_bytes"},
		{"keep+summary >= compact_at", func(c *Config) {
			c.KeepBytes = c.CompactAt
		}, "must be < compact_at"},
		{"compact_at >= hard_max", func(c *Config) {
			c.CompactAt = c.HardMaxBytes
		}, "must be < hard_max_bytes"},
		{"fallback zero", func(c *Config) { c.ContextTokensFallback = 0 }, "context_tokens_fallback"},
		{"negative reasoning budget", func(c *Config) { c.ReasoningBudgetTokens = -1 }, "reasoning_budget_tokens"},
		{"negative answer margin", func(c *Config) { c.AnswerMarginTokens = -1 }, "answer_margin_tokens"},
		{"reservations eat fallback", func(c *Config) {
			c.ReasoningBudgetTokens = c.ContextTokensFallback
		}, "no prompt budget"},
		{"bad compact_at_frac", func(c *Config) { c.CompactAtFrac = 1.5 }, "compact_at_frac"},
		{"keep_frac >= compact_frac", func(c *Config) {
			c.CompactAtFrac = 0.5
			c.KeepBytesFrac = 0.5
			c.HardMaxFrac = 0.9
		}, "keep_bytes_frac"},
		{"hard_max_frac <= compact_frac", func(c *Config) {
			c.CompactAtFrac = 0.5
			c.KeepBytesFrac = 0.2
			c.HardMaxFrac = 0.5
		}, "hard_max_frac"},
		{"capacity frac out of range", func(c *Config) { c.ContextCapacityFrac = 1.2 }, "context_capacity_frac"},
		{"subagent_max_tool_iter negative", func(c *Config) { c.SubagentMaxToolIter = -1 }, "subagent_max_tool_iterations"},
		{"subagent_turn_tool_budget negative", func(c *Config) { c.SubagentTurnToolBudget = -1 }, "subagent_turn_tool_budget"},
		{"subagent_tool_result_cap negative", func(c *Config) { c.SubagentToolResultCap = -1 }, "subagent_tool_result_cap"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validCfg()
			tc.mutate(&cfg)
			err := validateContextLimits(cfg)
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantSub)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Errorf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

func TestValidateEnums_InvalidAutoCounsel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AutoCounsel = "bogus"
	err := validateEnums(cfg)
	if err == nil || !strings.Contains(err.Error(), "auto_counsel") {
		t.Fatalf("expected auto_counsel error, got: %v", err)
	}
}

func TestValidateEnums_ValidAutoCounsel(t *testing.T) {
	for _, v := range []string{"", "suggest", "auto", "off"} {
		cfg := DefaultConfig()
		cfg.AutoCounsel = v
		if err := validateEnums(cfg); err != nil {
			t.Errorf("auto_counsel=%q: unexpected error: %v", v, err)
		}
	}
}

func TestValidateEnums_InvalidWFOracleMode(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WFOracleMode = "never"
	err := validateEnums(cfg)
	if err == nil || !strings.Contains(err.Error(), "wf_oracle_mode") {
		t.Fatalf("expected wf_oracle_mode error, got: %v", err)
	}
}

func TestValidateTimeouts_NegativeOracle(t *testing.T) {
	cfg := DefaultConfig()
	cfg.OracleTimeoutSeconds = -1
	err := validateTimeouts(cfg)
	if err == nil || !strings.Contains(err.Error(), "oracle_timeout_seconds") {
		t.Fatalf("expected oracle_timeout_seconds error, got: %v", err)
	}
}

func TestEnvInt_HalfParse(t *testing.T) {
	// "12abc" should NOT parse — envInt must use strconv.Atoi which rejects it.
	// The value should remain unchanged.
	t.Setenv("WAKIL_TEST_INT", "12abc")
	var dst int
	envInt(&dst, "WAKIL_TEST_INT")
	if dst != 0 {
		t.Errorf("envInt with half-parseable value should leave dst unchanged, got %d", dst)
	}
}

func TestEnvInt_ValidParse(t *testing.T) {
	t.Setenv("WAKIL_TEST_INT", "42")
	var dst int
	envInt(&dst, "WAKIL_TEST_INT")
	if dst != 42 {
		t.Errorf("envInt should parse 42, got %d", dst)
	}
}
