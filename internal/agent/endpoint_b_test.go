package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/orregistry"
	"github.com/treeol/wakil/internal/proxy"
)

// --- helpers ---

// requestLogServer records every request path and serves the given handler.
func requestLogServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, *[]string) {
	t.Helper()
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, &paths
}

// openaiCfg builds a config whose active endpoint is kind=openai at baseURL.
func openaiCfg(baseURL, model string) config.Config {
	cfg := config.DefaultConfig()
	cfg.BaseURL = baseURL
	cfg.Endpoint = config.EndpointConfig{
		Kind:    config.EndpointKindOpenAI,
		BaseURL: baseURL,
		Model:   model,
	}
	cfg.EndpointName = "test-openai"
	return cfg
}

// proxyCfg builds a config whose active endpoint is kind=ilm-proxy at baseURL.
func proxyCfg(baseURL string) config.Config {
	cfg := config.DefaultConfig()
	cfg.BaseURL = baseURL
	cfg.Endpoint = config.EndpointConfig{
		Kind:    config.EndpointKindIlmProxy,
		BaseURL: baseURL,
		Model:   "ilm",
	}
	cfg.EndpointName = "test-proxy"
	return cfg
}

// runCmd executes a tea.Cmd and flattens tea.BatchMsg into its member msgs.
func runCmd(cmd tea.Cmd) []tea.Msg {
	if cmd == nil {
		return nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var out []tea.Msg
		for _, c := range batch {
			out = append(out, runCmd(c)...)
		}
		return out
	}
	return []tea.Msg{msg}
}

// --- 1. Limits resolution ---

// TestLimitsOpenAIPropsOnly: openai kind + llama-shaped server → NCtx from
// /props, and /v1/ilm/limits is NEVER requested.
func TestLimitsOpenAIPropsOnly(t *testing.T) {
	srv, paths := requestLogServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/props" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"default_generation_settings":{"n_ctx":32768,"n_ctx_train":131072}}`)
	})

	cfg := openaiCfg(srv.URL, "qwen3.6-35b")
	var out strings.Builder
	lim := ResolveContextLimit(context.Background(), http.DefaultClient, cfg, &out)

	if lim.NCtx != 32768 {
		t.Errorf("NCtx = %d, want 32768 (from /props)", lim.NCtx)
	}
	if lim.Source != "backend" {
		t.Errorf("Source = %q, want backend", lim.Source)
	}
	if lim.UsableCtx != 0 {
		t.Errorf("UsableCtx = %d, want 0 (never synthesized in openai mode)", lim.UsableCtx)
	}
	for _, p := range *paths {
		if p == "/v1/ilm/limits" {
			t.Fatal("openai kind must never request /v1/ilm/limits")
		}
	}
	if len(*paths) != 1 || (*paths)[0] != "/props" {
		t.Errorf("request log = %v, want exactly [/props]", *paths)
	}
}

// TestLimitsOpenAIOpenRouterRegistry: openrouter.ai base_url → registry
// lookup, no /props probe fired.
func TestLimitsOpenAIOpenRouterRegistry(t *testing.T) {
	// Registry mock — no real network.
	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"anthropic/claude-opus-4-8","context_length":200000}]}`)
	}))
	t.Cleanup(regSrv.Close)
	restore := orregistry.SetModelsURLForTest(regSrv.URL)
	t.Cleanup(restore)
	orregistry.ResetCache()
	t.Cleanup(orregistry.ResetCache)

	// The endpoint base_url uses the real openrouter.ai host, but no request
	// may ever reach it: registry resolution short-circuits before any probe.
	// A hijacked RoundTripper proves it.
	var probes int32
	blockAll := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&probes, 1)
		return nil, fmt.Errorf("no requests allowed to %s", req.URL)
	})}

	cfg := openaiCfg("https://openrouter.ai/api/v1", "anthropic/claude-opus-4-8")
	var out strings.Builder
	lim := ResolveContextLimit(context.Background(), blockAll, cfg, &out)

	if lim.NCtx != 200000 {
		t.Errorf("NCtx = %d, want 200000 (registry context_length)", lim.NCtx)
	}
	if lim.Source != "backend" {
		t.Errorf("Source = %q, want backend", lim.Source)
	}
	if lim.NCtxTrain != 0 {
		t.Errorf("NCtxTrain = %d, want 0 (registry doesn't report it)", lim.NCtxTrain)
	}
	if n := atomic.LoadInt32(&probes); n != 0 {
		t.Errorf("%d probe request(s) fired against openrouter.ai base_url — registry must be the only source", n)
	}
}

// TestLimitsOpenAIOpenRouterUnknownModel: unknown model → fallback path with
// Source="fallback".
func TestLimitsOpenAIOpenRouterUnknownModel(t *testing.T) {
	regSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"some/other-model","context_length":8192}]}`)
	}))
	t.Cleanup(regSrv.Close)
	restore := orregistry.SetModelsURLForTest(regSrv.URL)
	t.Cleanup(restore)
	orregistry.ResetCache()
	t.Cleanup(orregistry.ResetCache)

	cfg := openaiCfg("https://openrouter.ai/api/v1", "does/not-exist")
	var out strings.Builder
	lim := ResolveContextLimit(context.Background(), http.DefaultClient, cfg, &out)

	if lim.Source != "fallback" {
		t.Errorf("Source = %q, want fallback", lim.Source)
	}
	if lim.NCtx != cfg.ContextTokensFallback {
		t.Errorf("NCtx = %d, want fallback %d", lim.NCtx, cfg.ContextTokensFallback)
	}
	if !strings.Contains(out.String(), "fallback") {
		t.Errorf("expected loud fallback warning, got: %q", out.String())
	}
}

// roundTripFunc adapts a func to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// TestLimitsIlmProxyGolden: ilm-proxy kind — request sequence and parsed
// result identical to today: /v1/ilm/limits first, usable_ctx and
// resolved:false → ModelUnresolved honored.
func TestLimitsIlmProxyGolden(t *testing.T) {
	srv, paths := requestLogServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/ilm/limits" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprint(w, `{"n_ctx":196608,"n_ctx_train":262144,"usable_ctx":188416,"context_source":"props","resolved":false,"fallback_model":"aion-labs/aion-3.0-mini"}`)
	})

	cfg := proxyCfg(srv.URL)
	var out strings.Builder
	lim := ResolveContextLimit(context.Background(), http.DefaultClient, cfg, &out)

	if len(*paths) != 1 || (*paths)[0] != "/v1/ilm/limits" {
		t.Errorf("request sequence = %v, want [/v1/ilm/limits] first-hit", *paths)
	}
	if lim.NCtx != 196608 || lim.NCtxTrain != 262144 {
		t.Errorf("NCtx/NCtxTrain = %d/%d, want 196608/262144", lim.NCtx, lim.NCtxTrain)
	}
	if lim.UsableCtx != 188416 {
		t.Errorf("UsableCtx = %d, want 188416", lim.UsableCtx)
	}
	if lim.ContextSource != "props" {
		t.Errorf("ContextSource = %q, want props", lim.ContextSource)
	}
	if !lim.ModelUnresolved {
		t.Error("ModelUnresolved should be true (proxy said resolved:false)")
	}
	if !strings.Contains(out.String(), "fallback model") {
		t.Errorf("expected unresolved-model warning, got: %q", out.String())
	}
}

// TestLimitsIlmProxyPropsFallbackGolden: proxy kind falls through to /props
// when /v1/ilm/limits is absent — the historical two-step sequence.
func TestLimitsIlmProxyPropsFallbackGolden(t *testing.T) {
	srv, paths := requestLogServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/props" {
			fmt.Fprint(w, `{"default_generation_settings":{"n_ctx":65536}}`)
			return
		}
		http.NotFound(w, r)
	})

	cfg := proxyCfg(srv.URL)
	var out strings.Builder
	lim := ResolveContextLimit(context.Background(), http.DefaultClient, cfg, &out)

	want := []string{"/v1/ilm/limits", "/props"}
	if len(*paths) != 2 || (*paths)[0] != want[0] || (*paths)[1] != want[1] {
		t.Errorf("request sequence = %v, want %v", *paths, want)
	}
	if lim.NCtx != 65536 {
		t.Errorf("NCtx = %d, want 65536", lim.NCtx)
	}
}

// --- 2. Command gating ---

// TestLearnGateOpenAI: /learn in openai mode → client-side error, zero HTTP
// requests issued.
func TestLearnGateOpenAI(t *testing.T) {
	srv, paths := requestLogServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should be issued")
	})

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg = openaiCfg(srv.URL, "m")
	app.Client.Kind = proxy.KindOpenAI
	app.Client.ConfiguredModel = "m"

	handled, quit, cmd := HandleTUICommand("/learn", app)
	if !handled || quit {
		t.Fatalf("handled=%v quit=%v", handled, quit)
	}
	msgs := runCmd(cmd)
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d", len(msgs))
	}
	sn, ok := msgs[0].(SysNoteMsg)
	if !ok {
		t.Fatalf("want SysNoteMsg (client-side error), got %T", msgs[0])
	}
	if !strings.Contains(sn.Text, "ilm-proxy") || !strings.Contains(sn.Text, "test-openai") {
		t.Errorf("error should name the requirement and the endpoint, got: %q", sn.Text)
	}
	if len(*paths) != 0 {
		t.Errorf("zero HTTP requests expected, got %v", *paths)
	}
}

// TestLearnGateIlmProxy: proxy mode unchanged — /learn yields LearnTurnMsg.
func TestLearnGateIlmProxy(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg = proxyCfg("http://unused")

	_, _, cmd := HandleTUICommand("/learn", app)
	msgs := runCmd(cmd)
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d", len(msgs))
	}
	if _, ok := msgs[0].(LearnTurnMsg); !ok {
		t.Errorf("want LearnTurnMsg (unchanged proxy behavior), got %T", msgs[0])
	}
}

// TestLearnGateLegacyConfig: hand-built config with no endpoint (legacy) maps
// to ilm-proxy via ActiveEndpoint — /learn must keep working.
func TestLearnGateLegacyConfig(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	// DefaultConfig has no Endpoint set — ActiveEndpoint synthesizes ilm-proxy.

	_, _, cmd := HandleTUICommand("/learn", app)
	msgs := runCmd(cmd)
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d", len(msgs))
	}
	if _, ok := msgs[0].(LearnTurnMsg); !ok {
		t.Errorf("legacy config must behave as ilm-proxy, got %T", msgs[0])
	}
}

// --- /model direct semantics ---

// TestModelCommandOpenAI: /model <name> in openai mode → next chat request
// carries the new model; limits re-resolution fires.
func TestModelCommandOpenAI(t *testing.T) {
	var lastModel atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Model string `json:"model"`
			}
			json.Unmarshal(body, &req)
			lastModel.Store(req.Model)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: "+contentChunk("ok")+"\n\ndata: [DONE]\n\n")
			return
		}
		if r.URL.Path == "/props" {
			fmt.Fprint(w, `{"default_generation_settings":{"n_ctx":4096}}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg = openaiCfg(srv.URL, "old-model")
	app.Client.Kind = proxy.KindOpenAI
	app.Client.ConfiguredModel = "old-model"
	app.Client.Model = "old-model"

	_, _, cmd := HandleTUICommand("/model new-model", app)
	msgs := runCmd(cmd)

	// Limits re-resolution fired: BackendCtxLimitMsg present with /props NCtx.
	var gotCtxMsg bool
	for _, m := range msgs {
		if bm, ok := m.(BackendCtxLimitMsg); ok {
			gotCtxMsg = true
			if bm.Limit.NCtx != 4096 {
				t.Errorf("re-resolved NCtx = %d, want 4096", bm.Limit.NCtx)
			}
		}
	}
	if !gotCtxMsg {
		t.Error("limits re-resolution (BackendCtxLimitMsg) not triggered by /model")
	}

	// Next chat request carries the new model.
	if _, err := app.Send(context.Background(), "hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := lastModel.Load(); got != "new-model" {
		t.Errorf("request model = %v, want new-model", got)
	}
}

// TestModelCommandIlmProxyUnchanged: proxy mode keeps raw-string behavior —
// SelectedModel set verbatim, Client.Model applied at Send time.
func TestModelCommandIlmProxyUnchanged(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg = proxyCfg("http://unused")

	_, _, _ = HandleTUICommand("/model openrouter/anthropic/claude-opus-4-8", app)
	if app.SelectedModel != "openrouter/anthropic/claude-opus-4-8" {
		t.Errorf("SelectedModel = %q, want raw string stored", app.SelectedModel)
	}
	if app.Client.ConfiguredModel != "" {
		t.Errorf("proxy mode must not touch ConfiguredModel, got %q", app.Client.ConfiguredModel)
	}
}

// --- /backend as endpoint switcher ---

// endpointsApp builds an app with two named endpoints (openai + ilm-proxy)
// and the openai one active.
func endpointsApp(t *testing.T, openaiURL, proxyURL string) *App {
	t.Helper()
	app := newTestApp(openaiURL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	cfg := openaiCfg(openaiURL, "model-a")
	cfg.EndpointName = "a"
	cfg.Endpoints = map[string]config.EndpointConfig{
		"a":  {Kind: config.EndpointKindOpenAI, BaseURL: openaiURL, Model: "model-a"},
		"b":  {Kind: config.EndpointKindOpenAI, BaseURL: proxyURL, Model: "model-b"},
		"px": {Kind: config.EndpointKindIlmProxy, BaseURL: proxyURL},
	}
	cfg.Endpoint = cfg.Endpoints["a"]
	app.Cfg = cfg
	app.Client.Kind = proxy.KindOpenAI
	app.Client.ConfiguredModel = "model-a"
	app.Client.Model = "model-a"
	return app
}

// chatCapture serves SSE and records the last request body+headers.
func chatCapture(t *testing.T) (*httptest.Server, *atomic.Value, *atomic.Value) {
	t.Helper()
	var lastBody, lastHdr atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			b, _ := io.ReadAll(r.Body)
			lastBody.Store(b)
			lastHdr.Store(r.Header.Clone())
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: "+contentChunk("ok")+"\n\ndata: [DONE]\n\n")
			return
		}
		http.NotFound(w, r) // limits probes 404 → fallback, fine
	}))
	t.Cleanup(srv.Close)
	return srv, &lastBody, &lastHdr
}

// TestBackendSwitchOpenAIToOpenAI: switch changes base_url+model on the next
// request.
func TestBackendSwitchOpenAIToOpenAI(t *testing.T) {
	srvA, _, _ := chatCapture(t)
	srvB, bodyB, _ := chatCapture(t)

	app := endpointsApp(t, srvA.URL, srvB.URL)
	_, _, cmd := HandleTUICommand("/backend b", app)
	runCmd(cmd) // limits re-resolve hits 404 → fallback; harmless

	if _, err := app.Send(context.Background(), "hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	raw, _ := bodyB.Load().([]byte)
	if raw == nil {
		t.Fatal("request did not reach endpoint b's base_url")
	}
	var req struct {
		Model string `json:"model"`
	}
	json.Unmarshal(raw, &req)
	if req.Model != "model-b" {
		t.Errorf("model = %q, want model-b", req.Model)
	}
}

// TestBackendSwitchOpenAIToProxy: switching to an ilm-proxy endpoint restores
// the proxy request shape (metadata present; X-Ilm headers when set).
func TestBackendSwitchOpenAIToProxy(t *testing.T) {
	srvA, _, _ := chatCapture(t)
	srvPx, bodyPx, hdrPx := chatCapture(t)

	app := endpointsApp(t, srvA.URL, srvPx.URL)
	_, _, cmd := HandleTUICommand("/backend px", app)
	runCmd(cmd)

	if app.Client.Kind != proxy.KindIlmProxy {
		t.Fatalf("client kind = %q, want ilm-proxy", app.Client.Kind)
	}
	if _, err := app.Send(context.Background(), "hi"); err != nil {
		t.Fatalf("Send: %v", err)
	}
	raw, _ := bodyPx.Load().([]byte)
	if raw == nil {
		t.Fatal("request did not reach the proxy endpoint")
	}
	var parsed map[string]json.RawMessage
	json.Unmarshal(raw, &parsed)
	var meta map[string]string
	if err := json.Unmarshal(parsed["metadata"], &meta); err != nil {
		t.Fatalf("proxy shape not restored — metadata missing: %v", err)
	}
	if meta["chat_id"] == "" {
		t.Error("metadata.chat_id missing after switch to proxy endpoint")
	}
	var model string
	json.Unmarshal(parsed["model"], &model)
	if model != "ilm" {
		t.Errorf("model = %q, want alias ilm", model)
	}
	_ = hdrPx // header assertions below need a Backend set; alias model is the key proxy-shape signal
}

// TestBackendSwitchNoArgLists: no-arg /backend in openai mode lists endpoints
// with the active one marked.
func TestBackendSwitchNoArgLists(t *testing.T) {
	app := endpointsApp(t, "http://a", "http://b")
	_, _, cmd := HandleTUICommand("/backend", app)
	msgs := runCmd(cmd)
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d", len(msgs))
	}
	sn, ok := msgs[0].(SysNoteMsg)
	if !ok {
		t.Fatalf("want SysNoteMsg, got %T", msgs[0])
	}
	if !strings.Contains(sn.Text, "* a") {
		t.Errorf("active endpoint 'a' should be marked, got:\n%s", sn.Text)
	}
	for _, name := range []string{"b", "px"} {
		if !strings.Contains(sn.Text, name) {
			t.Errorf("endpoint %q missing from list:\n%s", name, sn.Text)
		}
	}
}

// TestBackendSwitchUnknownEndpoint: bad name → error + list, no state change.
func TestBackendSwitchUnknownEndpoint(t *testing.T) {
	app := endpointsApp(t, "http://a", "http://b")
	before := app.Client.BaseURL
	_, _, cmd := HandleTUICommand("/backend nope", app)
	msgs := runCmd(cmd)
	sn, ok := msgs[0].(SysNoteMsg)
	if !ok || !strings.Contains(sn.Text, "not found") {
		t.Errorf("want not-found note, got %#v", msgs[0])
	}
	if app.Client.BaseURL != before {
		t.Error("client must be unchanged after failed switch")
	}
}

// TestSubagentInheritsSwitchedEndpoint: a subagent spawned AFTER an endpoint
// switch inherits the new endpoint (kind, base_url, model) — nothing is
// snapshotted at startup.
func TestSubagentInheritsSwitchedEndpoint(t *testing.T) {
	srvA, _, _ := chatCapture(t)

	summaryJSON := `{"objective":"check","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`
	var subBody atomic.Value
	srvB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			b, _ := io.ReadAll(r.Body)
			subBody.Store(b)
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: "+contentChunk(summaryJSON)+"\n\ndata: [DONE]\n\n")
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srvB.Close)

	app := endpointsApp(t, srvA.URL, srvB.URL)
	_, _, cmd := HandleTUICommand("/backend b", app)
	runCmd(cmd)

	app.dispatchSubagent(context.Background(), "check", io.Discard, "", "")

	raw, _ := subBody.Load().([]byte)
	if raw == nil {
		t.Fatal("subagent request did not reach the switched endpoint's base_url")
	}
	var parsed map[string]json.RawMessage
	json.Unmarshal(raw, &parsed)
	var model string
	json.Unmarshal(parsed["model"], &model)
	if model != "model-b" {
		t.Errorf("subagent model = %q, want model-b (switched endpoint)", model)
	}
	if _, ok := parsed["metadata"]; ok {
		t.Error("subagent to openai endpoint must not send metadata")
	}
}

// --- FetchBackendList/FetchModelList gating ---

// TestBackendListSkippedOpenAI: openai mode fires no request and falls back
// to the config external_backends list.
func TestBackendListSkippedOpenAI(t *testing.T) {
	srv, paths := requestLogServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("no request should fire in openai mode")
	})

	cfg := openaiCfg(srv.URL, "m")
	cfg.ExternalBackends = []string{"openrouter"}
	list := FetchBackendListWithFallback(context.Background(), http.DefaultClient, cfg, io.Discard)

	if len(*paths) != 0 {
		t.Errorf("requests fired: %v", *paths)
	}
	if len(list) != 1 || list[0].Name != "openrouter" || !list[0].External {
		t.Errorf("config-list fallback = %+v", list)
	}
	// IsExternalBackend lands on the same fallback semantics.
	if !IsExternalBackend(list, cfg, "openrouter") {
		t.Error("openrouter should be external via config fallback")
	}
}

// TestModelListOpenAIUsesV1Models: openai mode populates completion from the
// standard /v1/models route; tolerates absence silently.
func TestModelListOpenAIUsesV1Models(t *testing.T) {
	srv, paths := requestLogServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"m1"},{"id":"m2"}]}`)
			return
		}
		http.NotFound(w, r)
	})

	cfg := openaiCfg(srv.URL, "m1")
	names := FetchModelListForEndpoint(context.Background(), http.DefaultClient, cfg)
	if len(names) != 2 || names[0] != "m1" || names[1] != "m2" {
		t.Errorf("names = %v, want [m1 m2]", names)
	}
	for _, p := range *paths {
		if p == "/v1/ilm/models" {
			t.Error("openai mode must not request /v1/ilm/models")
		}
	}

	// Bare server without the route → silently empty.
	srv2, _ := requestLogServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	cfg2 := openaiCfg(srv2.URL, "m")
	if names := FetchModelListForEndpoint(context.Background(), http.DefaultClient, cfg2); names != nil {
		t.Errorf("bare server should yield empty list, got %v", names)
	}
}

// TestModelListIlmProxyUnchanged: proxy mode still uses /v1/ilm/models.
func TestModelListIlmProxyUnchanged(t *testing.T) {
	srv, paths := requestLogServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/ilm/models" {
			fmt.Fprint(w, `{"models":[{"name":"ilm"},{"name":"qwen"}]}`)
			return
		}
		http.NotFound(w, r)
	})

	cfg := proxyCfg(srv.URL)
	names := FetchModelListForEndpoint(context.Background(), http.DefaultClient, cfg)
	if len(names) != 2 || names[0] != "ilm" {
		t.Errorf("names = %v, want [ilm qwen]", names)
	}
	if len(*paths) != 1 || (*paths)[0] != "/v1/ilm/models" {
		t.Errorf("request log = %v, want [/v1/ilm/models]", *paths)
	}
}
