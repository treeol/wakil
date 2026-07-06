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
	usd, priced := c.ExternalInferenceCost("openrouter/x/y", 2_000_000, 1_000_000)
	if !priced || usd != 4 {
		t.Errorf("ExternalInferenceCost = %v, %v; want 4, true", usd, priced)
	}
	if _, priced := c.ExternalInferenceCost("nope", 1, 1); priced {
		t.Error("unknown backend/model should be unpriced")
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
