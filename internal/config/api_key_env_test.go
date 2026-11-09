package config

import (
	"os"
	"path/filepath"
	"testing"
)

func loadWith(t *testing.T, body string, argv ...string) Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	clearEndpointEnv(t)
	cfg, err := LoadConfig(append([]string{"--config", p, "--sandbox", "direct"}, argv...))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

const epBase = `"kind":"openai","base_url":"http://x:1","model":"m"`

func TestAPIKeyEnvIsReadAtRequestTime(t *testing.T) {
	cfg := loadWith(t, `{"endpoints":{"e":{`+epBase+`,"api_key_env":"WAKIL_TEST_KEY"}},"default_endpoint":"e"}`)
	t.Setenv("WAKIL_TEST_KEY", "")
	if got := cfg.AuthHeader(); got != "" {
		t.Errorf("unset env var: header = %q, want none", got)
	}
	t.Setenv("WAKIL_TEST_KEY", "  sk-env-123  ")
	if got := cfg.AuthHeader(); got != "Bearer sk-env-123" {
		t.Errorf("header = %q", got)
	}
}

func TestExplicitAuthHeaderBeatsAPIKeyEnv(t *testing.T) {
	cfg := loadWith(t, `{"endpoints":{"e":{`+epBase+`,"api_key_env":"WAKIL_TEST_KEY","auth_header":"Token abc"}},"default_endpoint":"e"}`)
	t.Setenv("WAKIL_TEST_KEY", "from-env")
	if got := cfg.AuthHeader(); got != "Token abc" {
		t.Errorf("auth_header must win over api_key_env, got %q", got)
	}
}

func TestAPIKeyEnvBeatsLegacyAPIKey(t *testing.T) {
	cfg := loadWith(t, `{"api_key":"legacy","endpoints":{"e":{`+epBase+`,"api_key_env":"WAKIL_TEST_KEY"}},"default_endpoint":"e"}`)
	t.Setenv("WAKIL_TEST_KEY", "from-env")
	if got := cfg.AuthHeader(); got != "Bearer from-env" {
		t.Errorf("got %q", got)
	}
	t.Setenv("WAKIL_TEST_KEY", "")
	if got := cfg.AuthHeader(); got != "Bearer legacy" {
		t.Errorf("unset env var must fall back to api_key, got %q", got)
	}
}

func TestApprovalKey(t *testing.T) {
	cfg := loadWith(t, `{"approval":"auto","endpoints":{"e":{`+epBase+`}},"default_endpoint":"e"}`)
	if cfg.AutoApprove {
		t.Error("LoadConfig alone must not turn on auto approval (headless never reads it)")
	}
	ApplyConfigApproval(&cfg)
	if !cfg.AutoApprove || !cfg.AutoExplicit {
		t.Errorf("ApplyConfigApproval: auto=%v explicit=%v", cfg.AutoApprove, cfg.AutoExplicit)
	}
	ask := loadWith(t, `{"approval":"ask","endpoints":{"e":{`+epBase+`}},"default_endpoint":"e"}`)
	ApplyConfigApproval(&ask)
	if ask.AutoApprove {
		t.Error("ask must not enable auto")
	}
	p := filepath.Join(t.TempDir(), "c.json")
	_ = os.WriteFile(p, []byte(`{"approval":"yolo","endpoints":{"e":{`+epBase+`}},"default_endpoint":"e"}`), 0o600)
	if _, err := LoadConfig([]string{"--config", p, "--sandbox", "direct"}); err == nil {
		t.Error("an unknown approval value must be rejected")
	}
}
