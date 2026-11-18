package config

import (
	"strings"
	"testing"
)

func TestResolveOpenRouterCredential_Order(t *testing.T) {
	t.Run("active OpenRouter endpoint wins over env var", func(t *testing.T) {
		t.Setenv("OPENROUTER_API_KEY", "sk-env-should-not-win")
		t.Setenv("EP_KEY", "sk-active-ep")
		cfg := Config{
			EndpointName: "or-main",
			Endpoint: EndpointConfig{
				Kind:      EndpointKindOpenAI,
				BaseURL:   "https://openrouter.ai/api",
				Model:     "anthropic/claude-sonnet-4",
				APIKeyEnv: "EP_KEY",
			},
			Endpoints: map[string]EndpointConfig{
				"a-other-or": {
					Kind:       EndpointKindOpenAI,
					BaseURL:    "https://eu.openrouter.ai/api",
					Model:      "m",
					AuthHeader: "Bearer sk-other-ep",
				},
			},
			OpenRouterAPIKeyEnv: "OPENROUTER_API_KEY",
		}
		cred, err := cfg.ResolveOpenRouterCredential()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cred.AuthHeader != "Bearer sk-active-ep" {
			t.Errorf("AuthHeader = %q, want Bearer sk-active-ep", cred.AuthHeader)
		}
		if cred.Source != OpenRouterSourceBackendConfig || cred.EndpointName != "or-main" {
			t.Errorf("source = %q (%q), want backend config (or-main)", cred.Source, cred.EndpointName)
		}
		if got := cred.SourceLabel(); got != "backend config (or-main)" {
			t.Errorf("SourceLabel = %q, want %q", got, "backend config (or-main)")
		}
	})

	t.Run("active OpenRouter endpoint with verbatim auth_header wins over env var", func(t *testing.T) {
		t.Setenv("OPENROUTER_API_KEY", "sk-env-ignored")
		cfg := Config{
			EndpointName: "or-direct",
			Endpoint: EndpointConfig{
				Kind:       EndpointKindOpenAI,
				BaseURL:    "https://openrouter.ai/api",
				Model:      "m",
				AuthHeader: "Bearer sk-verbatim-ep",
			},
			OpenRouterAPIKeyEnv: "OPENROUTER_API_KEY",
		}
		cred, err := cfg.ResolveOpenRouterCredential()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cred.AuthHeader != "Bearer sk-verbatim-ep" {
			t.Errorf("AuthHeader = %q, want Bearer sk-verbatim-ep", cred.AuthHeader)
		}
		if got := cred.SourceLabel(); got != "backend config (or-direct)" {
			t.Errorf("SourceLabel = %q", got)
		}
	})

	t.Run("non-active configured OpenRouter endpoint (sorted) wins over env var", func(t *testing.T) {
		t.Setenv("OPENROUTER_API_KEY", "sk-env-ignored")
		cfg := Config{
			EndpointName: "local",
			Endpoint: EndpointConfig{
				Kind:    EndpointKindOpenAI,
				BaseURL: "http://127.0.0.1:11400",
				Model:   "local-llm",
			},
			Endpoints: map[string]EndpointConfig{
				"local": {
					Kind:    EndpointKindOpenAI,
					BaseURL: "http://127.0.0.1:11400",
					Model:   "local-llm",
				},
				"z-or": {
					Kind:       EndpointKindOpenAI,
					BaseURL:    "https://openrouter.ai/api",
					Model:      "m",
					AuthHeader: "Bearer sk-z",
				},
				"b-or": {
					Kind:       EndpointKindOpenAI,
					BaseURL:    "https://eu.openrouter.ai/api",
					Model:      "m",
					AuthHeader: "Bearer sk-b",
				},
			},
			OpenRouterAPIKeyEnv: "OPENROUTER_API_KEY",
		}
		cred, err := cfg.ResolveOpenRouterCredential()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cred.AuthHeader != "Bearer sk-b" {
			t.Errorf("AuthHeader = %q, want Bearer sk-b (first sorted)", cred.AuthHeader)
		}
		if got := cred.SourceLabel(); got != "backend config (b-or)" {
			t.Errorf("SourceLabel = %q, want backend config (b-or)", got)
		}
	})

	t.Run("env var used only when no OpenRouter endpoint exists", func(t *testing.T) {
		t.Setenv("OPENROUTER_API_KEY", "sk-env-fallback")
		cfg := Config{
			EndpointName: "local",
			Endpoint: EndpointConfig{
				Kind:    EndpointKindOpenAI,
				BaseURL: "http://127.0.0.1:11400",
				Model:   "local-llm",
			},
			Endpoints: map[string]EndpointConfig{
				"local": {
					Kind:    EndpointKindOpenAI,
					BaseURL: "http://127.0.0.1:11400",
					Model:   "local-llm",
				},
			},
			OpenRouterAPIKeyEnv: "OPENROUTER_API_KEY",
		}
		cred, err := cfg.ResolveOpenRouterCredential()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cred.AuthHeader != "Bearer sk-env-fallback" {
			t.Errorf("AuthHeader = %q, want Bearer sk-env-fallback", cred.AuthHeader)
		}
		if cred.Source != OpenRouterSourceEnvironment || cred.EnvVar != "OPENROUTER_API_KEY" {
			t.Errorf("source = %q (%q), want environment (OPENROUTER_API_KEY)", cred.Source, cred.EnvVar)
		}
		if got := cred.SourceLabel(); got != "environment (OPENROUTER_API_KEY)" {
			t.Errorf("SourceLabel = %q", got)
		}
	})

	t.Run("active OpenRouter endpoint without credential fails closed even when env var is set", func(t *testing.T) {
		t.Setenv("OPENROUTER_API_KEY", "sk-env-must-not-be-used")
		cfg := Config{
			EndpointName: "or-empty",
			Endpoint: EndpointConfig{
				Kind:    EndpointKindOpenAI,
				BaseURL: "https://openrouter.ai/api",
				Model:   "m",
			},
			OpenRouterAPIKeyEnv: "OPENROUTER_API_KEY",
		}
		cred, err := cfg.ResolveOpenRouterCredential()
		if err == nil {
			t.Fatalf("expected fail-closed error, got cred=%+v", cred)
		}
		if !strings.Contains(err.Error(), "no credential on endpoint or-empty") {
			t.Errorf("error = %q, want 'no credential on endpoint or-empty'", err.Error())
		}
		if cred.AuthHeader != "" {
			t.Errorf("AuthHeader must be empty on error, got %q", cred.AuthHeader)
		}
	})

	t.Run("non-active OpenRouter endpoint without credential fails closed even when env var is set", func(t *testing.T) {
		t.Setenv("OPENROUTER_API_KEY", "sk-env-must-not-be-used")
		cfg := Config{
			EndpointName: "local",
			Endpoint: EndpointConfig{
				Kind:    EndpointKindOpenAI,
				BaseURL: "http://127.0.0.1:11400",
				Model:   "local-llm",
			},
			Endpoints: map[string]EndpointConfig{
				"or-nocred": {
					Kind:    EndpointKindOpenAI,
					BaseURL: "https://openrouter.ai/api",
					Model:   "m",
				},
			},
			OpenRouterAPIKeyEnv: "OPENROUTER_API_KEY",
		}
		_, err := cfg.ResolveOpenRouterCredential()
		if err == nil {
			t.Fatal("expected fail-closed error")
		}
		if !strings.Contains(err.Error(), "no credential on endpoint or-nocred") {
			t.Errorf("error = %q, want 'no credential on endpoint or-nocred'", err.Error())
		}
	})

	t.Run("ilm-proxy with backend openrouter is skipped as non-OpenRouter endpoint and falls back to env", func(t *testing.T) {
		t.Setenv("OPENROUTER_API_KEY", "sk-env-proxy-fallback")
		cfg := Config{
			EndpointName: "proxy",
			Backend:      "openrouter",
			Endpoint: EndpointConfig{
				Kind:       EndpointKindIlmProxy,
				BaseURL:    "http://127.0.0.1:9000",
				Model:      "ilm",
				AuthHeader: "Bearer sk-proxy-token-not-openrouter",
			},
			OpenRouterAPIKeyEnv: "OPENROUTER_API_KEY",
		}
		cred, err := cfg.ResolveOpenRouterCredential()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cred.AuthHeader != "Bearer sk-env-proxy-fallback" {
			t.Errorf("AuthHeader = %q, want Bearer sk-env-proxy-fallback", cred.AuthHeader)
		}
		if cred.Source != OpenRouterSourceEnvironment {
			t.Errorf("Source = %q, want environment", cred.Source)
		}
	})

	t.Run("SourceLabel never leaks credential value", func(t *testing.T) {
		const secret = "sk-super-secret-value-xyz"
		cfg := Config{
			EndpointName: "or",
			Endpoint: EndpointConfig{
				Kind:       EndpointKindOpenAI,
				BaseURL:    "https://openrouter.ai/api",
				Model:      "m",
				AuthHeader: "Bearer " + secret,
			},
		}
		cred, err := cfg.ResolveOpenRouterCredential()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if strings.Contains(cred.SourceLabel(), secret) {
			t.Errorf("SourceLabel leaked secret: %q", cred.SourceLabel())
		}
	})
}
