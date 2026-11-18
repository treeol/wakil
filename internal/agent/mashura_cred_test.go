package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// rewriteOpenRouterToTestServer returns an *http.Client that redirects any
// OpenRouter host request (openrouter.ai / eu.openrouter.ai / us.openrouter.ai)
// to srv while preserving the original Host header in req.Header["X-Orig-Host"]
// so the test server can inspect both the target host and the Authorization header.
func rewriteOpenRouterToTestServer(srv *httptest.Server) *http.Client {
	target, _ := url.Parse(srv.URL)
	base := srv.Client().Transport
	if base == nil {
		base = http.DefaultTransport
	}
	return &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			clone := r.Clone(r.Context())
			clone.Header = r.Header.Clone()
			clone.Header.Set("X-Orig-Host", r.URL.Host)
			clone.URL.Scheme = target.Scheme
			clone.URL.Host = target.Host
			return base.RoundTrip(clone)
		}),
	}
}

// TestMashuraMatchesMainCredentialAndDataPolicy sends a main request and a
// Mashūra panel request under identical /zdr and /region settings and asserts
// both carry the exact same Authorization header (from the configured
// OpenRouter backend, NOT the shell env var) and the same data-policy fields
// (provider.zdr, provider.data_collection, and regional host).
func TestMashuraMatchesMainCredentialAndDataPolicy(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-shell-env-wrong")
	t.Setenv("BACKEND_OR_KEY", "sk-backend-right")

	type capturedReq struct {
		auth     string
		origHost string
		body     map[string]any
	}
	var mu sync.Mutex
	var captured []capturedReq

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		captured = append(captured, capturedReq{
			auth:     r.Header.Get("Authorization"),
			origHost: r.Header.Get("X-Orig-Host"),
			body:     body,
		})
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"ok\nLAST SECTION READ: Task"},"finish_reason":null}]}` + "\n\n"))
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	ep := config.EndpointConfig{
		Kind:      config.EndpointKindOpenAI,
		BaseURL:   "https://openrouter.ai/api",
		Model:     "anthropic/claude-sonnet-4",
		APIKeyEnv: "BACKEND_OR_KEY",
	}
	cfg := config.Config{
		EndpointName:        "openrouter-main",
		Endpoint:            ep,
		Endpoints:           map[string]config.EndpointConfig{"openrouter-main": ep},
		OpenRouterAPIKeyEnv: "OPENROUTER_API_KEY",
		OracleEnabled:       true,
		OracleModel:         "openrouter:anthropic/claude-sonnet-4",
		OracleMaxTokens:     4096,
		ZDR:                 "on",
		Region:              "eu",
	}

	httpClient := rewriteOpenRouterToTestServer(srv)
	mainClient := &proxy.Client{
		Kind:       proxy.KindOpenAI,
		BaseURL:    ep.BaseURL,
		Model:      ep.Model,
		AuthHeader: cfg.AuthHeader(),
		HTTP:       httpClient,
		ZDR:        cfg.ZDR,
		Region:     cfg.Region,
	}

	app := &App{
		Cfg:     cfg,
		Client:  mainClient,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}
	app.GrantBackendConsentForTest("openrouter")

	// 1. Main request.
	hi := "hi"
	if _, err := app.Client.Stream(context.Background(), []proxy.Message{{Role: "user", Content: &hi}}, nil, nil, nil); err != nil {
		t.Fatalf("main Stream: %v", err)
	}

	// 2. Mashūra request under the exact same App/settings.
	tc := proxy.ToolCall{
		Function: proxy.FunctionCall{
			Name:      "mashura__check",
			Arguments: `{"claim":"ok","text_under_review":"ok"}`,
		},
	}
	res := app.runMashuraCore(context.Background(), "mashura__check", tc, true)
	if strings.HasPrefix(res, "ERROR:") {
		t.Fatalf("mashura failed: %s", res)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("captured %d requests, want 2 (main + mashura)", len(captured))
	}
	mainReq, mashReq := captured[0], captured[1]

	if mainReq.auth != "Bearer sk-backend-right" {
		t.Fatalf("main Authorization = %q, want Bearer sk-backend-right", mainReq.auth)
	}
	if mashReq.auth != mainReq.auth {
		t.Errorf("mashura Authorization = %q, want same as main (%q)", mashReq.auth, mainReq.auth)
	}
	if mashReq.origHost != "eu.openrouter.ai" || mainReq.origHost != "eu.openrouter.ai" {
		t.Errorf("hosts main=%q mashura=%q, want eu.openrouter.ai for both", mainReq.origHost, mashReq.origHost)
	}
	mainProv, _ := mainReq.body["provider"].(map[string]any)
	mashProv, _ := mashReq.body["provider"].(map[string]any)
	if mainProv["zdr"] != true || mainProv["data_collection"] != "deny" {
		t.Fatalf("main provider = %v, want zdr:true, data_collection:deny", mainProv)
	}
	if mashProv["zdr"] != true || mashProv["data_collection"] != "deny" {
		t.Errorf("mashura provider = %v, want zdr:true, data_collection:deny (same as main)", mashProv)
	}
}

func TestMashuraStatus_CredentialSource(t *testing.T) {
	t.Run("backend config source never leaks key", func(t *testing.T) {
		const secret = "sk-secret-backend-999"
		t.Setenv("OPENROUTER_API_KEY", "sk-env-ignored")
		ep := config.EndpointConfig{
			Kind:       config.EndpointKindOpenAI,
			BaseURL:    "https://openrouter.ai/api",
			Model:      "m",
			AuthHeader: "Bearer " + secret,
		}
		app := &App{
			Cfg: config.Config{
				EndpointName:        "or-prod",
				Endpoint:            ep,
				OpenRouterAPIKeyEnv: "OPENROUTER_API_KEY",
				OracleEnabled:       true,
				OracleModel:         "openrouter:anthropic/claude-sonnet-4",
			},
		}
		out := mashuraStatus(app)
		if !strings.Contains(out, "backend config (or-prod)") {
			t.Errorf("status missing backend config source:\n%s", out)
		}
		if strings.Contains(out, secret) {
			t.Errorf("status leaked credential value:\n%s", out)
		}
	})

	t.Run("environment fallback on non-OpenRouter local endpoint", func(t *testing.T) {
		const secret = "sk-secret-env-888"
		t.Setenv("OPENROUTER_API_KEY", secret)
		app := &App{
			Cfg: config.Config{
				EndpointName: "local",
				Endpoint: config.EndpointConfig{
					Kind:    config.EndpointKindOpenAI,
					BaseURL: "http://127.0.0.1:11400",
					Model:   "local-llm",
				},
				OpenRouterAPIKeyEnv: "OPENROUTER_API_KEY",
				OracleEnabled:       true,
				OracleModel:         "openrouter:anthropic/claude-sonnet-4",
			},
		}
		out := mashuraStatus(app)
		if !strings.Contains(out, "environment (OPENROUTER_API_KEY)") {
			t.Errorf("status missing environment source:\n%s", out)
		}
		if !strings.Contains(out, "using environment fallback") {
			t.Errorf("status missing environment fallback note:\n%s", out)
		}
		if strings.Contains(out, secret) {
			t.Errorf("status leaked credential value:\n%s", out)
		}
	})

	t.Run("ilm-proxy openrouter backend shows proxy note and environment source", func(t *testing.T) {
		const secret = "sk-secret-env-777"
		t.Setenv("OPENROUTER_API_KEY", secret)
		app := &App{
			Cfg: config.Config{
				EndpointName: "proxy",
				Backend:      "openrouter",
				Endpoint: config.EndpointConfig{
					Kind:       config.EndpointKindIlmProxy,
					BaseURL:    "http://127.0.0.1:9000",
					Model:      "ilm",
					AuthHeader: "Bearer sk-proxy-key",
				},
				OpenRouterAPIKeyEnv: "OPENROUTER_API_KEY",
				OracleEnabled:       true,
				OracleModel:         "openrouter:anthropic/claude-sonnet-4",
			},
			SelectedBackend: "openrouter",
		}
		out := mashuraStatus(app)
		if !strings.Contains(out, "environment (OPENROUTER_API_KEY)") {
			t.Errorf("status missing environment source:\n%s", out)
		}
		if !strings.Contains(out, "the main route's OpenRouter key is held by the proxy") {
			t.Errorf("status missing proxy-held key line:\n%s", out)
		}
		if strings.Contains(out, secret) || strings.Contains(out, "sk-proxy-key") {
			t.Errorf("status leaked secret:\n%s", out)
		}
	})

	t.Run("OpenRouter endpoint without credential fails closed in mashuraAvailable and status", func(t *testing.T) {
		t.Setenv("OPENROUTER_API_KEY", "sk-env-must-not-be-used")
		ep := config.EndpointConfig{
			Kind:    config.EndpointKindOpenAI,
			BaseURL: "https://openrouter.ai/api",
			Model:   "m",
		}
		app := &App{
			Cfg: config.Config{
				EndpointName:        "or-blank",
				Endpoint:            ep,
				OpenRouterAPIKeyEnv: "OPENROUTER_API_KEY",
				OracleEnabled:       true,
				OracleModel:         "openrouter:anthropic/claude-sonnet-4",
			},
		}
		if app.mashuraAvailable() {
			t.Error("mashuraAvailable() = true, want false (fail closed)")
		}
		out := mashuraStatus(app)
		if !strings.Contains(out, "no credential on endpoint or-blank") {
			t.Errorf("status missing fail-closed error:\n%s", out)
		}
	})

	t.Run("proxy switched away from openrouter hides proxy note", func(t *testing.T) {
		t.Setenv("OPENROUTER_API_KEY", "fake-env-key")
		client := &proxy.Client{}
		// Simulate an earlier request that used "openrouter", followed by an
		// explicit switch to a different proxy backend ("anthropic").
		app := &App{
			Cfg: config.Config{
				EndpointName:        "proxy",
				Endpoint:            config.EndpointConfig{Kind: config.EndpointKindIlmProxy, BaseURL: "http://127.0.0.1:8080", Model: "ilm"},
				OpenRouterAPIKeyEnv: "OPENROUTER_API_KEY",
				OracleEnabled:       true,
				OracleModel:         "openrouter:anthropic/claude-sonnet-4",
			},
			Client:          client,
			SelectedBackend: "anthropic",
		}
		out := mashuraStatus(app)
		if strings.Contains(out, "held by the proxy") {
			t.Errorf("switched-away proxy backend must not print proxy OpenRouter note:\n%s", out)
		}
	})
}
