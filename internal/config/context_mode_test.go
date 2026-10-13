package config

import (
	"os"
	"testing"
)

// ---------------- ContextMode ----------------

func TestContextModeDefaults(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.ContextMode != ContextModeStandard {
		t.Errorf("DefaultConfig ContextMode = %q, want %q", cfg.ContextMode, ContextModeStandard)
	}
	if cfg.ContextCollapseMinBytes != 2000 {
		t.Errorf("DefaultConfig ContextCollapseMinBytes = %d, want 2000", cfg.ContextCollapseMinBytes)
	}
}

func TestNormalizeContextMode(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", ContextModeStandard, true},
		{"standard", ContextModeStandard, true},
		{"long", ContextModeLongSession, true},
		{"long-session", ContextModeLongSession, true},
		{"STANDARD", "", false},
		{"bogus", "", false},
	}
	for _, c := range cases {
		got, ok := NormalizeContextMode(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("NormalizeContextMode(%q) = (%q, %v), want (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestValidateContextLimitsContextMode(t *testing.T) {
	// Valid modes pass.
	for _, m := range []string{"", "standard", "long-session"} {
		c := validCfg()
		c.ContextMode = m
		if err := validateContextLimits(c); err != nil {
			t.Errorf("mode %q: unexpected error: %v", m, err)
		}
	}
	// "long" passes validation too (it's a valid shorthand), but normalization
	// to "long-session" happens in LoadConfig, not in validateContextLimits.
	c := validCfg()
	c.ContextMode = "long"
	if err := validateContextLimits(c); err != nil {
		t.Errorf("mode %q: unexpected error: %v", "long", err)
	}
	// Invalid mode rejected.
	c = validCfg()
	c.ContextMode = "bogus"
	if err := validateContextLimits(c); err == nil {
		t.Error("validateContextLimits should reject bogus context_mode")
	}
	// Negative min bytes rejected.
	c = validCfg()
	c.ContextCollapseMinBytes = -1
	if err := validateContextLimits(c); err == nil {
		t.Error("validateContextLimits should reject negative context_collapse_min_bytes")
	}
}

func TestLoadConfigContextModeEnv(t *testing.T) {
	t.Setenv("WAKIL_CONTEXT_MODE", "long-session")
	cfg, err := LoadConfig([]string{"--base-url", "http://unused"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ContextMode != "long-session" {
		t.Errorf("ContextMode = %q, want %q", cfg.ContextMode, "long-session")
	}
}

func TestLoadConfigContextModeFlagExplicit(t *testing.T) {
	// --context-mode should set ContextModeExplicit.
	cfg, err := LoadConfig([]string{"--base-url", "http://unused", "--context-mode", "long-session"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !cfg.ContextModeExplicit {
		t.Error("ContextModeExplicit should be true after --context-mode flag")
	}
	if cfg.ContextMode != "long-session" {
		t.Errorf("ContextMode = %q, want %q", cfg.ContextMode, "long-session")
	}
}

func TestLoadConfigContextModeNotExplicitByDefault(t *testing.T) {
	// No --context-mode flag: ContextModeExplicit should be false.
	os.Unsetenv("WAKIL_CONTEXT_MODE")
	cfg, err := LoadConfig([]string{"--base-url", "http://unused"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ContextModeExplicit {
		t.Error("ContextModeExplicit should be false without --context-mode flag")
	}
}

func TestLoadConfigContextModeInvalid(t *testing.T) {
	_, err := LoadConfig([]string{"--base-url", "http://unused", "--context-mode", "bogus"})
	if err == nil {
		t.Fatal("LoadConfig should reject invalid --context-mode value")
	}
}

func TestLoadConfigContextModeLongAlias(t *testing.T) {
	// "long" should be normalized to "long-session" by LoadConfig.
	cfg, err := LoadConfig([]string{"--base-url", "http://unused", "--context-mode", "long"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ContextMode != "long-session" {
		t.Errorf("ContextMode = %q, want %q (alias 'long' should be normalised)", cfg.ContextMode, "long-session")
	}
}
