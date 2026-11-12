package counsel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rawSSEServer serves the given SSE data lines, then [DONE].
func rawSSEServer(t *testing.T, events ...string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, e := range events {
			fmt.Fprintf(w, "data: %s\n\n", e)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

const servedModel = "anthropic/claude-fable-5.2"

// Normal completion: the usage chunk carries cost and the served model.
func TestPanelStreamCostAndServedModelNormalCompletion(t *testing.T) {
	srv := rawSSEServer(t,
		`{"model":"`+servedModel+`","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`,
		`{"model":"`+servedModel+`","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"model":"`+servedModel+`","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"cost":0.0042}}`,
	)
	defer srv.Close()
	ccfg := PanelCallConfig{MaxTokens: 512, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL}
	_, usage, err := callOpenRouter(context.Background(), "openrouter/auto", "key", "q", "ctx", ccfg)
	if err != nil {
		t.Fatalf("callOpenRouter: %v", err)
	}
	if !usage.HasProviderCost || usage.ProviderCostUSD != 0.0042 {
		t.Errorf("cost = %v/%v, want 0.0042 provider-reported", usage.ProviderCostUSD, usage.HasProviderCost)
	}
	if usage.ServedModel != servedModel {
		t.Errorf("ServedModel = %q, want %q (openrouter/auto resolves to the served model)", usage.ServedModel, servedModel)
	}
}

// Usage-only trailing chunk with no finish_reason and an empty choices array.
func TestPanelStreamCostOnUsageOnlyChunk(t *testing.T) {
	srv := rawSSEServer(t,
		`{"model":"`+servedModel+`","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		`{"model":"`+servedModel+`","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"cost":0.0042}}`,
	)
	defer srv.Close()
	ccfg := PanelCallConfig{MaxTokens: 512, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL}
	_, usage, err := callOpenRouter(context.Background(), "openrouter/auto", "key", "q", "ctx", ccfg)
	if err != nil {
		t.Fatalf("callOpenRouter: %v", err)
	}
	if !usage.HasProviderCost || usage.ProviderCostUSD != 0.0042 || usage.ServedModel != servedModel {
		t.Errorf("usage = cost %v/%v served %q", usage.ProviderCostUSD, usage.HasProviderCost, usage.ServedModel)
	}
}

// A stream with no usage.cost must not claim a provider cost, but must still
// report the served model and tokens. The agent prices that call from the
// table, or records it as unpriced.
func TestPanelStreamNoCostReportedIsNotProviderCost(t *testing.T) {
	srv := rawSSEServer(t,
		`{"model":"`+servedModel+`","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		`{"model":"`+servedModel+`","choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3}}`,
	)
	defer srv.Close()
	ccfg := PanelCallConfig{MaxTokens: 512, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL}
	_, usage, err := callOpenRouter(context.Background(), "openrouter/auto", "key", "q", "ctx", ccfg)
	if err != nil {
		t.Fatalf("callOpenRouter: %v", err)
	}
	if usage.HasProviderCost {
		t.Errorf("HasProviderCost set without usage.cost: %+v", usage)
	}
	if usage.InputTokens != 7 || usage.OutputTokens != 3 || usage.ServedModel != servedModel {
		t.Errorf("tokens/served = %d/%d %q", usage.InputTokens, usage.OutputTokens, usage.ServedModel)
	}
}

// The debug capture writes one 0600 file with the request body as sent, the
// served model, the HTTP status and every usage object in order, and never
// writes a header or the API key.
func TestPanelDebugCaptureWritesFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(panelDebugEnvVar, dir)
	const secret = "sk-or-SECRET-TEST-VALUE"
	srv := rawSSEServer(t,
		`{"model":"`+servedModel+`","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`,
		`{"model":"`+servedModel+`","choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"cost":0.0042}}`,
	)
	defer srv.Close()
	ccfg := PanelCallConfig{MaxTokens: 512, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL}
	if _, _, err := callOpenRouter(context.Background(), "openrouter/auto", secret, "q", "ctx", ccfg); err != nil {
		t.Fatalf("callOpenRouter: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "panel-*.json"))
	if len(matches) != 1 {
		t.Fatalf("want 1 panel-*.json file, got %v", matches)
	}
	fi, err := os.Stat(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 0600", perm)
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) {
		t.Fatal("capture file contains the API key")
	}
	var doc struct {
		Served     string   `json:"served_model"`
		HTTPStatus int      `json:"http_status"`
		Usage      []string `json:"usage_objects"`
		Last       []string `json:"last_lines_before_done"`
		Body       any      `json:"request_body"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("capture is not JSON: %v", err)
	}
	if doc.Served != servedModel || doc.HTTPStatus != http.StatusOK {
		t.Errorf("served=%q status=%d", doc.Served, doc.HTTPStatus)
	}
	if len(doc.Usage) != 1 || !strings.Contains(doc.Usage[0], `"cost":0.0042`) {
		t.Errorf("usage objects = %v, want the raw usage chunk once", doc.Usage)
	}
	if len(doc.Last) == 0 || len(doc.Last) > panelDebugTail {
		t.Errorf("last lines = %d, want 1..%d", len(doc.Last), panelDebugTail)
	}
	if doc.Body == nil {
		t.Error("request body missing from capture")
	}
}

// With the variable unset, no capture is written.
func TestPanelDebugCaptureOffWritesNothing(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(panelDebugEnvVar, "")
	srv := rawSSEServer(t, `{"model":"`+servedModel+`","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop"}]}`)
	defer srv.Close()
	ccfg := PanelCallConfig{MaxTokens: 512, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL}
	_, _, _ = callOpenRouter(context.Background(), "openrouter/auto", "key", "q", "ctx", ccfg)
	if m, _ := filepath.Glob(filepath.Join(dir, "*")); len(m) != 0 {
		t.Errorf("capture off but files written: %v", m)
	}
}
