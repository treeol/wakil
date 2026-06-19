package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"wakil/internal/config"
)

func backendsServer(t *testing.T, statusCode int, body interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ilm/backends" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		if body != nil {
			b, _ := json.Marshal(body)
			w.Write(b)
		}
	}))
}

func TestFetchBackendListSuccess(t *testing.T) {
	srv := backendsServer(t, 200, map[string]interface{}{
		"backends": []map[string]interface{}{
			{"name": "local", "external": false},
			{"name": "openrouter", "external": true, "caps": []string{"chat"}},
		},
	})
	defer srv.Close()

	list, ok := FetchBackendList(context.Background(), srv.Client(), srv.URL, "")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 backends, got %d", len(list))
	}
	if list[0].Name != "local" || list[0].External {
		t.Errorf("first backend wrong: %+v", list[0])
	}
	if list[1].Name != "openrouter" || !list[1].External {
		t.Errorf("second backend wrong: %+v", list[1])
	}
}

func TestFetchBackendListNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, ok := FetchBackendList(context.Background(), srv.Client(), srv.URL, "")
	if ok {
		t.Fatal("expected ok=false for 404")
	}
}

func TestFetchBackendListEmpty(t *testing.T) {
	srv := backendsServer(t, 200, map[string]interface{}{"backends": []interface{}{}})
	defer srv.Close()

	_, ok := FetchBackendList(context.Background(), srv.Client(), srv.URL, "")
	if ok {
		t.Fatal("expected ok=false for empty backend list")
	}
}

func TestFetchBackendListWithFallbackUsesProxy(t *testing.T) {
	srv := backendsServer(t, 200, map[string]interface{}{
		"backends": []map[string]interface{}{
			{"name": "local", "external": false},
		},
	})
	defer srv.Close()

	cfg := config.DefaultConfig()
	cfg.BaseURL = srv.URL
	cfg.ExternalBackends = []string{"fallback-only"}

	var logBuf strings.Builder
	list := FetchBackendListWithFallback(context.Background(), srv.Client(), cfg, &logBuf)
	if len(list) != 1 || list[0].Name != "local" {
		t.Errorf("expected proxy list; got %+v", list)
	}
	if strings.Contains(logBuf.String(), "fallback") {
		t.Error("should not have mentioned fallback when proxy succeeded")
	}
}

func TestFetchBackendListWithFallbackUsesConfig(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg := config.DefaultConfig()
	cfg.BaseURL = srv.URL
	cfg.ExternalBackends = []string{"openrouter", "together"}

	var logBuf strings.Builder
	list := FetchBackendListWithFallback(context.Background(), srv.Client(), cfg, &logBuf)
	if len(list) != 2 {
		t.Fatalf("expected 2 backends from config; got %d", len(list))
	}
	if !list[0].External || !list[1].External {
		t.Error("config-list backends should all be External=true")
	}
}

func TestFetchBackendListWithFallbackNilWhenBothEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg := config.DefaultConfig()
	cfg.BaseURL = srv.URL
	// No external_backends in config either.

	list := FetchBackendListWithFallback(context.Background(), srv.Client(), cfg, io.Discard)
	if list != nil {
		t.Errorf("expected nil when both proxy and config are empty; got %+v", list)
	}
}

func TestIsExternalBackendProxyList(t *testing.T) {
	list := []BackendInfo{
		{Name: "local", External: false},
		{Name: "openrouter", External: true},
	}
	cfg := config.DefaultConfig()

	if IsExternalBackend(list, cfg, "openrouter") != true {
		t.Error("openrouter should be external")
	}
	if IsExternalBackend(list, cfg, "local") != false {
		t.Error("local should not be external")
	}
	if IsExternalBackend(list, cfg, "") != false {
		t.Error("empty name should not be external")
	}
}

func TestIsExternalBackendConfigFallback(t *testing.T) {
	// No proxy list, but config has external_backends.
	cfg := config.DefaultConfig()
	cfg.ExternalBackends = []string{"together", "groq"}

	if !IsExternalBackend(nil, cfg, "together") {
		t.Error("together should be external via config")
	}
	if !IsExternalBackend(nil, cfg, "groq") {
		t.Error("groq should be external via config")
	}
	if IsExternalBackend(nil, cfg, "local") {
		t.Error("local should not be external when not in config list")
	}
}

func TestIsExternalBackendProxyListWins(t *testing.T) {
	// Proxy says "openrouter" is NOT external (unusual but valid edge case).
	list := []BackendInfo{{Name: "openrouter", External: false}}
	cfg := config.DefaultConfig()
	cfg.ExternalBackends = []string{"openrouter"} // config says it IS external

	// Proxy list takes precedence (checked first).
	if IsExternalBackend(list, cfg, "openrouter") {
		t.Error("proxy list should win over config; proxy says openrouter is not external")
	}
}

func modelsServer(t *testing.T, statusCode int, body interface{}) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ilm/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(statusCode)
		if body != nil {
			b, _ := json.Marshal(body)
			w.Write(b)
		}
	}))
}

func TestFetchModelListSuccess(t *testing.T) {
	srv := modelsServer(t, 200, map[string]interface{}{
		"models": []map[string]interface{}{
			{"name": "claude-sonnet-4-6"},
			{"name": "claude-opus-4-8"},
		},
	})
	defer srv.Close()

	list, ok := FetchModelList(context.Background(), srv.Client(), srv.URL, "")
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(list) != 2 {
		t.Fatalf("expected 2 models, got %d", len(list))
	}
	if list[0] != "claude-sonnet-4-6" || list[1] != "claude-opus-4-8" {
		t.Errorf("unexpected model names: %v", list)
	}
}

func TestFetchModelListNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	_, ok := FetchModelList(context.Background(), srv.Client(), srv.URL, "")
	if ok {
		t.Fatal("expected ok=false for 404")
	}
}

func TestFetchModelListEmpty(t *testing.T) {
	srv := modelsServer(t, 200, map[string]interface{}{"models": []interface{}{}})
	defer srv.Close()

	_, ok := FetchModelList(context.Background(), srv.Client(), srv.URL, "")
	if ok {
		t.Fatal("expected ok=false for empty model list")
	}
}

// TestSuspendAutoExternalBackend verifies the egress gate is always suspended
// in auto mode — external_backend must never be auto-approved.
func TestSuspendAutoExternalBackend(t *testing.T) {
	app := &App{}
	reason := SuspendAuto("external_backend", app, "")
	if reason == "" {
		t.Error("SuspendAuto should return a non-empty reason for external_backend")
	}
	if !strings.Contains(strings.ToLower(reason), "external") {
		t.Errorf("reason should mention 'external'; got %q", reason)
	}
}
