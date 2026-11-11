package config

import "testing"

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
}
