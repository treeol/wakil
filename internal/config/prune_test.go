package config

import (
	"strings"
	"testing"
)

// Prune-key validation: defaults OK, each broken key rejected.

func TestValidatePruneDefaultsOK(t *testing.T) {
	cfg := DefaultConfig()
	if err := validateContextLimits(cfg); err != nil {
		t.Fatalf("default prune config rejected: %v", err)
	}
	cfg.ContextPrune = "on"
	if err := validateContextLimits(cfg); err != nil {
		t.Fatalf("prune on rejected: %v", err)
	}
}

func TestValidatePruneKeys(t *testing.T) {
	for name, mut := range map[string]func(*Config){
		"bad switch":   func(c *Config) { c.ContextPrune = "bogus" },
		"at zero":      func(c *Config) { c.ContextPruneAt = 0 },
		"at one":       func(c *Config) { c.ContextPruneAt = 1 },
		"at negative":  func(c *Config) { c.ContextPruneAt = -0.5 },
		"target >= at": func(c *Config) { c.ContextPruneTarget = c.ContextPruneAt },
		"target zero":  func(c *Config) { c.ContextPruneTarget = 0 },
		"target neg":   func(c *Config) { c.ContextPruneTarget = -0.1 },
		"protect zero": func(c *Config) { c.ContextPruneProtect = 0 },
		"protect neg":  func(c *Config) { c.ContextPruneProtect = -1 },
	} {
		c := DefaultConfig()
		c.ContextPrune = "on"
		mut(&c)
		if err := validateContextLimits(c); err == nil {
			t.Errorf("%s: expected validation error, got nil", name)
		}
	}
}

func TestPruneDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.ContextPrune != "off" {
		t.Errorf("ContextPrune = %q, want off", cfg.ContextPrune)
	}
	if cfg.ContextPruneAt != 0.70 || cfg.ContextPruneTarget != 0.55 || cfg.ContextPruneProtect != 3 {
		t.Errorf("defaults = (%g, %g, %d), want (0.70, 0.55, 3)",
			cfg.ContextPruneAt, cfg.ContextPruneTarget, cfg.ContextPruneProtect)
	}
	if cfg.ContextPruneAtTokens != 0 || cfg.ContextPruneTargetTokens != 0 {
		t.Errorf("token defaults = (%d, %d), want (0, 0) (fractions only)", cfg.ContextPruneAtTokens, cfg.ContextPruneTargetTokens)
	}
	if cfg.ContextPruneRater != "off" {
		t.Errorf("ContextPruneRater = %q, want off", cfg.ContextPruneRater)
	}
}

func TestValidatePruneTokenPairs(t *testing.T) {
	// Only one token key set → invalid. Both set with target >= at →
	// invalid. Both unset / both correctly set → valid.
	cases := []struct {
		name string
		mut  func(*Config)
		want string // "" = valid, otherwise error must contain it
	}{
		{"only at set", func(c *Config) { c.ContextPruneAtTokens = 100; c.ContextPruneTargetTokens = 0 }, "must both be set or both be 0"},
		{"only target set", func(c *Config) { c.ContextPruneAtTokens = 0; c.ContextPruneTargetTokens = 50 }, "must both be set or both be 0"},
		{"target >= at", func(c *Config) { c.ContextPruneAtTokens = 100; c.ContextPruneTargetTokens = 100 }, "must be in (0,"},
		{"both unset", func(c *Config) { c.ContextPruneAtTokens = 0; c.ContextPruneTargetTokens = 0 }, ""},
		{"both set correctly", func(c *Config) { c.ContextPruneAtTokens = 90000; c.ContextPruneTargetTokens = 60000 }, ""},
	}
	for _, tc := range cases {
		c := DefaultConfig()
		c.ContextPrune = "on"
		tc.mut(&c)
		err := validateContextLimits(c)
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", tc.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want containing %q", tc.name, err, tc.want)
		}
	}
}
