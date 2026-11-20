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

// orEP builds an OpenRouter-host endpoint for the table tests.
func orEP(host, authHeader, keyEnv string) EndpointConfig {
	return EndpointConfig{
		Kind: EndpointKindOpenAI, BaseURL: "https://" + host + "/api", Model: "m",
		AuthHeader: authHeader, APIKeyEnv: keyEnv,
	}
}

func localEP() EndpointConfig {
	return EndpointConfig{Kind: EndpointKindOpenAI, BaseURL: "http://127.0.0.1:11400", Model: "local"}
}

// TestResolveOpenRouterCredential_Rule is the table for the credential rule:
// active OpenRouter endpoint → use it, fail closed without a credential;
// otherwise the first OpenRouter endpoint by name that HAS a credential;
// fail closed only when OpenRouter endpoints exist and none has one; the
// environment variable only when no OpenRouter endpoint exists.
func TestResolveOpenRouterCredential_Rule(t *testing.T) {
	cases := []struct {
		name       string
		env        map[string]string // set via t.Setenv
		active     string
		endpoints  map[string]EndpointConfig
		globalKey  string
		wantHeader string
		wantLabel  string // SourceLabel when no error
		wantErr    string // substring; empty = no error
	}{
		{
			name:   "skips a no-credential OR endpoint and picks a later one with a credential",
			env:    map[string]string{"OPENROUTER_API_KEY": "sk-env-no"},
			active: "local",
			endpoints: map[string]EndpointConfig{
				"local": localEP(),
				"a-or":  orEP("openrouter.ai", "", ""),
				"b-or":  orEP("eu.openrouter.ai", "Bearer sk-b", ""),
			},
			wantHeader: "Bearer sk-b",
			wantLabel:  "backend config (b-or)",
		},
		{
			name:   "empty api_key_env variable counts as no credential and is skipped",
			env:    map[string]string{"A_KEY": "", "C_KEY": "sk-c"},
			active: "local",
			endpoints: map[string]EndpointConfig{
				"local": localEP(),
				"a-or":  orEP("openrouter.ai", "", "A_KEY"),
				"c-or":  orEP("us.openrouter.ai", "", "C_KEY"),
			},
			wantHeader: "Bearer sk-c",
			wantLabel:  "backend config (c-or)",
		},
		{
			name:   "sorted order, not insertion order, decides the pick",
			active: "local",
			endpoints: map[string]EndpointConfig{
				"local": localEP(),
				"z-or":  orEP("openrouter.ai", "Bearer sk-z", ""),
				"m-or":  orEP("openrouter.ai", "Bearer sk-m", ""),
				"a-or":  orEP("openrouter.ai", "Bearer sk-a", ""),
			},
			wantHeader: "Bearer sk-a",
			wantLabel:  "backend config (a-or)",
		},
		{
			name:   "several OR endpoints, none with a credential: fail closed naming all, env not used",
			env:    map[string]string{"OPENROUTER_API_KEY": "sk-env-must-not-be-used"},
			active: "local",
			endpoints: map[string]EndpointConfig{
				"local": localEP(),
				"a-or":  orEP("openrouter.ai", "", ""),
				"b-or":  orEP("eu.openrouter.ai", "", "UNSET_B_KEY"),
			},
			wantErr: "no credential on endpoint a-or, b-or",
		},
		{
			name:      "global api_key is not sent to a non-active OR endpoint",
			env:       map[string]string{"OPENROUTER_API_KEY": "sk-env-must-not-be-used"},
			active:    "local",
			globalKey: "sk-global-main-route",
			endpoints: map[string]EndpointConfig{
				"local": localEP(),
				"a-or":  orEP("openrouter.ai", "", ""),
			},
			wantErr: "no credential on endpoint a-or",
		},
		{
			name:      "global api_key is skipped on an OR endpoint in favour of a later own credential",
			active:    "local",
			globalKey: "sk-global-main-route",
			endpoints: map[string]EndpointConfig{
				"local": localEP(),
				"a-or":  orEP("openrouter.ai", "", ""),
				"b-or":  orEP("openrouter.ai", "Bearer sk-b", ""),
			},
			wantHeader: "Bearer sk-b",
			wantLabel:  "backend config (b-or)",
		},
		{
			name:      "active OR endpoint still uses the global api_key like a main request",
			active:    "main-or",
			globalKey: "sk-global",
			endpoints: map[string]EndpointConfig{
				"main-or": orEP("openrouter.ai", "", ""),
				"b-or":    orEP("openrouter.ai", "Bearer sk-b", ""),
			},
			wantHeader: "Bearer sk-global",
			wantLabel:  "backend config (main-or)",
		},
		{
			name:   "active OR endpoint without credential fails closed even when another OR endpoint has one",
			env:    map[string]string{"OPENROUTER_API_KEY": "sk-env-must-not-be-used"},
			active: "main-or",
			endpoints: map[string]EndpointConfig{
				"main-or": orEP("openrouter.ai", "", ""),
				"b-or":    orEP("openrouter.ai", "Bearer sk-b", ""),
			},
			wantErr: "no credential on endpoint main-or",
		},
		{
			name:   "no OR endpoint at all: environment variable is used",
			env:    map[string]string{"OPENROUTER_API_KEY": "sk-env"},
			active: "local",
			endpoints: map[string]EndpointConfig{
				"local": localEP(),
			},
			wantHeader: "Bearer sk-env",
			wantLabel:  "environment (OPENROUTER_API_KEY)",
		},
		{
			name:   "look-alike host is not an OpenRouter endpoint",
			env:    map[string]string{"OPENROUTER_API_KEY": "sk-env"},
			active: "local",
			endpoints: map[string]EndpointConfig{
				"local": localEP(),
				"evil":  {Kind: EndpointKindOpenAI, BaseURL: "https://openrouter.ai.evil.example/api", Model: "m", AuthHeader: "Bearer sk-evil"},
			},
			wantHeader: "Bearer sk-env",
			wantLabel:  "environment (OPENROUTER_API_KEY)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			// Environment variables the table does not set must be absent.
			for _, k := range []string{"OPENROUTER_API_KEY", "UNSET_B_KEY"} {
				if _, ok := tc.env[k]; !ok {
					t.Setenv(k, "")
				}
			}
			cfg := Config{
				EndpointName:        tc.active,
				Endpoint:            tc.endpoints[tc.active],
				Endpoints:           tc.endpoints,
				APIKey:              tc.globalKey,
				OpenRouterAPIKeyEnv: "OPENROUTER_API_KEY",
			}
			cred, err := cfg.ResolveOpenRouterCredential()
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got cred=%+v", tc.wantErr, cred)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantErr)
				}
				if cred.AuthHeader != "" {
					t.Errorf("AuthHeader must be empty on error, got %q", cred.AuthHeader)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cred.AuthHeader != tc.wantHeader {
				t.Errorf("AuthHeader = %q, want %q", cred.AuthHeader, tc.wantHeader)
			}
			if got := cred.SourceLabel(); got != tc.wantLabel {
				t.Errorf("SourceLabel = %q, want %q", got, tc.wantLabel)
			}
		})
	}
}
