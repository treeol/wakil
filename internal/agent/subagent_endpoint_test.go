package agent

// Tests for the per-subagent endpoint + limits + cost fold pass:
//   - golden inherit no-op (§6.1)
//   - override construction, both directions (§6.2/6.3)
//   - child limits resolution: override probe, failure floor, singleflight (§6.4)
//   - cost fold at the join point, including under -race (§6.5)
//   - /subagent command + subagent_endpoint config validation (§6.6)

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

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// --- 1. Golden inherit no-op ---

// TestInheritNoOpFieldsMatchTodaysCopy verifies that with no subagent_endpoint
// configured and no session override, resolveSubagentEndpointView("") produces
// exactly the same field values dispatchSubagent used to copy directly from
// a.Client — every field discovery §1 listed.
func TestInheritNoOpFieldsMatchTodaysCopy(t *testing.T) {
	temp := floatPtr(0.7)
	top := floatPtr(0.9)
	maxTok := intPtr(512)

	app := newTestApp("http://parent", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Client.Kind = proxy.KindOpenAI
	app.Client.ConfiguredModel = "parent-model"
	app.Client.Model = "parent-model"
	app.Client.Temperature = temp
	app.Client.TopP = top
	app.Client.MaxTokens = maxTok
	app.Client.AuthHeader = "Bearer parent-key"

	name := resolveSubagentEndpointName(app)
	if name != "" {
		t.Fatalf("resolveSubagentEndpointName = %q, want \"\" (no config, no override)", name)
	}
	view, inherited := app.resolveSubagentEndpointView(name)
	if !inherited {
		t.Fatal("expected inherited=true")
	}
	if view.kind != app.Client.Kind {
		t.Errorf("kind = %q, want %q", view.kind, app.Client.Kind)
	}
	if view.baseURL != app.Client.BaseURL {
		t.Errorf("baseURL = %q, want %q", view.baseURL, app.Client.BaseURL)
	}
	if view.model != app.Client.Model {
		t.Errorf("model = %q, want %q", view.model, app.Client.Model)
	}
	if view.configuredModel != app.Client.ConfiguredModel {
		t.Errorf("configuredModel = %q, want %q", view.configuredModel, app.Client.ConfiguredModel)
	}
	if view.authHeader != app.Client.AuthHeader {
		t.Errorf("authHeader = %q, want %q", view.authHeader, app.Client.AuthHeader)
	}
	if view.temperature != app.Client.Temperature {
		t.Errorf("temperature pointer = %p, want %p (same pointer value)", view.temperature, app.Client.Temperature)
	}
	if view.topP != app.Client.TopP {
		t.Errorf("topP pointer = %p, want %p", view.topP, app.Client.TopP)
	}
	if view.maxTokens != app.Client.MaxTokens {
		t.Errorf("maxTokens pointer = %p, want %p", view.maxTokens, app.Client.MaxTokens)
	}
}

// TestGoldenInheritRequestShape verifies the actual outgoing subagent request
// carries the parent's live Kind/BaseURL/model/auth unchanged, with NO extra
// context-limit probe request fired: the child's CtxLimit must come from
// a.CtxLimit directly (zero additional HTTP requests) when it already matches
// the endpoint the child is using.
func TestGoldenInheritRequestShape(t *testing.T) {
	var reqPaths []string
	var lastAuth, lastModel string
	summaryJSON := `{"objective":"check","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqPaths = append(reqPaths, r.URL.Path)
		if strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Model string `json:"model"`
			}
			json.Unmarshal(body, &req)
			lastModel = req.Model
			lastAuth = r.Header.Get("Authorization")
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: "+contentChunk(summaryJSON)+"\n\ndata: [DONE]\n\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Client.Kind = proxy.KindOpenAI
	app.Client.ConfiguredModel = "parent-model"
	app.Client.Model = "parent-model"
	app.Client.AuthHeader = "Bearer parent-key"
	// Parent's CtxLimit already resolved (as it would be at startup/after a
	// /model or /backend switch) — the golden no-op must reuse this directly.
	app.CtxLimit = ContextLimit{NCtx: 99999, Source: "backend"}

	_, _, _, _, _ = app.dispatchSubagent(context.Background(), "check", io.Discard, "")

	if lastModel != "parent-model" {
		t.Errorf("request model = %q, want parent-model (inherited)", lastModel)
	}
	if lastAuth != "Bearer parent-key" {
		t.Errorf("request auth = %q, want Bearer parent-key (inherited)", lastAuth)
	}
	for _, p := range reqPaths {
		if p == "/props" || p == "/v1/ilm/limits" {
			t.Errorf("inherit path fired an extra limits probe %q — must reuse a.CtxLimit with zero extra requests", p)
		}
	}
}

// TestSubagentInheritsSwitchedEndpointStillGreen re-runs the discovery-pinned
// scenario unmodified — a subagent dispatched after a /backend switch must
// still inherit the new endpoint's kind/base_url/model with no metadata sent.
// This mirrors endpoint_b_test.go's TestSubagentInheritsSwitchedEndpoint,
// duplicated here only to keep this file self-contained under the new
// 5-return-value dispatchSubagent signature; the original test is untouched
// and must also still pass (verified by the full suite run).
func TestSubagentInheritsSwitchedEndpointStillGreen(t *testing.T) {
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

	app.dispatchSubagent(context.Background(), "check", io.Discard, "")

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

// --- 2. Override: openai child from an ilm-proxy parent ---

func floatPtr(f float64) *float64 { return &f }
func intPtr(i int) *int           { return &i }

// TestOverrideOpenAIChildFromProxyParent: parent is on an ilm-proxy endpoint;
// subagent_endpoint names an openai entry. The child request must have no
// metadata key, no X-Ilm-* headers, model = the named endpoint's configured
// model, and auth = that endpoint's auth_header.
func TestOverrideOpenAIChildFromProxyParent(t *testing.T) {
	var capturedBody []byte
	var capturedHeaders http.Header
	summaryJSON := `{"objective":"check","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`
	openaiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			capturedBody, _ = io.ReadAll(r.Body)
			capturedHeaders = r.Header.Clone()
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: "+contentChunk(summaryJSON)+"\n\ndata: [DONE]\n\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer openaiSrv.Close()

	app := newTestApp("http://proxy-parent", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg = proxyCfg("http://proxy-parent")
	app.Cfg.Endpoints = map[string]config.EndpointConfig{
		"oa": {Kind: config.EndpointKindOpenAI, BaseURL: openaiSrv.URL, Model: "gpt-child", AuthHeader: "Bearer child-key"},
	}
	app.Cfg.SubagentEndpoint = "oa"
	app.Client.Kind = proxy.KindIlmProxy
	app.Client.Model = "ilm"

	_, _, _, _, _ = app.dispatchSubagent(context.Background(), "check", io.Discard, "")

	if capturedBody == nil {
		t.Fatal("subagent request did not reach the openai override endpoint")
	}
	var parsed map[string]json.RawMessage
	json.Unmarshal(capturedBody, &parsed)
	if _, ok := parsed["metadata"]; ok {
		t.Error("openai-kind child must not send metadata")
	}
	var model string
	json.Unmarshal(parsed["model"], &model)
	if model != "gpt-child" {
		t.Errorf("model = %q, want gpt-child (the named endpoint's configured model)", model)
	}
	if got := capturedHeaders.Get("Authorization"); got != "Bearer child-key" {
		t.Errorf("Authorization = %q, want Bearer child-key", got)
	}
	if capturedHeaders.Get("X-Ilm-No-Memory-Write") != "" {
		t.Error("openai-kind child must not send X-Ilm-No-Memory-Write")
	}
	if capturedHeaders.Get("X-Ilm-Backend") != "" {
		t.Error("openai-kind child must not send X-Ilm-Backend")
	}
}

// --- 3. Override: ilm-proxy child from an openai parent ---

// TestOverrideProxyChildFromOpenAIParent: reverse direction — parent is on an
// openai endpoint; subagent_endpoint names an ilm-proxy entry. The child
// request must carry metadata + X-Ilm-No-Memory-Write + the proxy alias
// model, and subagent_backend routing must apply to the child.
func TestOverrideProxyChildFromOpenAIParent(t *testing.T) {
	var capturedBody []byte
	var capturedHeaders http.Header
	summaryJSON := `{"objective":"check","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`
	proxySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			capturedBody, _ = io.ReadAll(r.Body)
			capturedHeaders = r.Header.Clone()
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: "+contentChunk(summaryJSON)+"\n\ndata: [DONE]\n\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer proxySrv.Close()

	app := newTestApp("http://openai-parent", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg = openaiCfg("http://openai-parent", "parent-model")
	app.Cfg.Endpoints = map[string]config.EndpointConfig{
		"px": {Kind: config.EndpointKindIlmProxy, BaseURL: proxySrv.URL}, // Model defaults to "ilm"
	}
	app.Cfg.SubagentEndpoint = "px"
	app.Cfg.SubagentBackend = "llama"
	app.Client.Kind = proxy.KindOpenAI
	app.Client.ConfiguredModel = "parent-model"
	app.Client.Model = "parent-model"

	epKind := app.resolvedSubagentEndpointKind()
	if epKind != config.EndpointKindIlmProxy {
		t.Fatalf("resolvedSubagentEndpointKind = %q, want ilm-proxy", epKind)
	}
	backend := app.resolveSubagentBackendForEndpoint(epKind)
	if backend != "llama" {
		t.Fatalf("resolved backend = %q, want llama (subagent_backend applies for ilm-proxy child)", backend)
	}

	_, _, _, _, _ = app.dispatchSubagent(context.Background(), "check", io.Discard, backend)

	if capturedBody == nil {
		t.Fatal("subagent request did not reach the ilm-proxy override endpoint")
	}
	var parsed map[string]json.RawMessage
	json.Unmarshal(capturedBody, &parsed)
	var meta map[string]string
	if err := json.Unmarshal(parsed["metadata"], &meta); err != nil {
		t.Fatalf("ilm-proxy child must send metadata: %v", err)
	}
	if meta["ilm-no-memory-write"] != "true" {
		t.Errorf("metadata.ilm-no-memory-write = %q, want true", meta["ilm-no-memory-write"])
	}
	var model string
	json.Unmarshal(parsed["model"], &model)
	if model != "ilm" {
		t.Errorf("model = %q, want ilm (proxy alias, endpoint default)", model)
	}
	if capturedHeaders.Get("X-Ilm-No-Memory-Write") != "true" {
		t.Errorf("X-Ilm-No-Memory-Write header = %q, want true", capturedHeaders.Get("X-Ilm-No-Memory-Write"))
	}
	if got := capturedHeaders.Get("X-Ilm-Backend"); got != "llama" {
		t.Errorf("X-Ilm-Backend = %q, want llama (subagent_backend routed for ilm-proxy child)", got)
	}
}

// TestOpenAIParentDoesNotComputeInertBackend verifies that when the child's
// resolved endpoint kind is openai (whether inherited or overridden),
// resolveSubagentBackendForEndpoint returns "" without consulting
// subagent_backend/SelectedBackend at all — no inert routing value.
func TestOpenAIParentDoesNotComputeInertBackend(t *testing.T) {
	app := newTestApp("http://parent", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.SubagentBackend = "llama"
	app.SelectedBackend = "openrouter"
	got := app.resolveSubagentBackendForEndpoint(config.EndpointKindOpenAI)
	if got != "" {
		t.Errorf("backend = %q, want \"\" for kind openai (backend resolution must be skipped entirely)", got)
	}
}

// --- 4. Child limits resolution ---

// TestChildLimitsOverrideFromMockProps: override endpoint with a mock /props
// server → child CtxLimit.NCtx comes from the mock; the PARENT's CtxLimit is
// untouched.
func TestChildLimitsOverrideFromMockProps(t *testing.T) {
	summaryJSON := `{"objective":"check","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`
	var gotNCtx int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/props" {
			fmt.Fprint(w, `{"default_generation_settings":{"n_ctx":4096}}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: "+contentChunk(summaryJSON)+"\n\ndata: [DONE]\n\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app := newTestApp("http://parent-unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.Endpoints = map[string]config.EndpointConfig{
		"oa": {Kind: config.EndpointKindOpenAI, BaseURL: srv.URL, Model: "m"},
	}
	app.Cfg.SubagentEndpoint = "oa"
	parentCtxLimit := ContextLimit{NCtx: 200000, Source: "backend"}
	app.CtxLimit = parentCtxLimit

	epName := resolveSubagentEndpointName(app)
	view, inherited := app.resolveSubagentEndpointView(epName)
	if inherited {
		t.Fatal("expected override, got inherited=true")
	}
	lim := app.resolveChildCtxLimit(context.Background(), view, "", inherited)
	gotNCtx = int64(lim.NCtx)
	if gotNCtx != 4096 {
		t.Errorf("child CtxLimit.NCtx = %d, want 4096 (from mock /props)", gotNCtx)
	}
	if app.CtxLimit != parentCtxLimit {
		t.Errorf("parent CtxLimit mutated: got %+v, want unchanged %+v", app.CtxLimit, parentCtxLimit)
	}
}

// TestChildLimitsProbeFailureFallsBackToByteConstants: probe failure → child
// CtxLimit stays zero, and activeThresholds() falls through to the
// byte-constant floor (compact_at/keep_bytes/hard_max from Cfg), never a
// silently-adopted 131072-token fallback.
func TestChildLimitsProbeFailureFallsBackToByteConstants(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r) // no /props, no /v1/ilm/limits — probe fails
	}))
	defer srv.Close()

	app := newTestApp("http://parent-unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.Endpoints = map[string]config.EndpointConfig{
		"oa": {Kind: config.EndpointKindOpenAI, BaseURL: srv.URL, Model: "m"},
	}
	app.Cfg.SubagentEndpoint = "oa"

	epName := resolveSubagentEndpointName(app)
	view, inherited := app.resolveSubagentEndpointView(epName)
	lim := app.resolveChildCtxLimit(context.Background(), view, "", inherited)
	if lim.NCtx != 0 {
		t.Errorf("child CtxLimit.NCtx = %d, want 0 (probe failed — no silent fallback adoption)", lim.NCtx)
	}

	// activeThresholds() on a child App with this zero CtxLimit must use the
	// subagent byte constants — mirrors TestActiveThresholdsFallsBackToAbsolute.
	childCfg := config.DefaultConfig()
	childCfg.HardMaxBytes = subagentHardMaxBytes
	childCfg.CompactAt = subagentCompactAt
	childCfg.KeepBytes = subagentKeepBytes
	childApp := &App{Cfg: childCfg, CtxLimit: lim}
	ca, kb, hm := childApp.activeThresholds()
	if ca != subagentCompactAt || kb != subagentKeepBytes || hm != subagentHardMaxBytes {
		t.Errorf("thresholds = (%d,%d,%d), want byte-constant floor (%d,%d,%d)",
			ca, kb, hm, subagentCompactAt, subagentKeepBytes, subagentHardMaxBytes)
	}
}

// TestChildLimitsSingleflightAcrossParallelDispatches: two parallel dispatches
// to the same overridden endpoint must fire exactly one limits probe.
func TestChildLimitsSingleflightAcrossParallelDispatches(t *testing.T) {
	var probeCount atomic.Int32
	summaryJSON := `{"objective":"check","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/props" {
			probeCount.Add(1)
			fmt.Fprint(w, `{"default_generation_settings":{"n_ctx":8192}}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: "+contentChunk(summaryJSON)+"\n\ndata: [DONE]\n\n")
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	app := newTestApp("http://parent-unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.Endpoints = map[string]config.EndpointConfig{
		"oa": {Kind: config.EndpointKindOpenAI, BaseURL: srv.URL, Model: "m"},
	}
	app.Cfg.SubagentEndpoint = "oa"
	app.ensureSubagentLimitsCache() // Phase A step — required for cross-call dedup

	done := make(chan struct{}, 2)
	for i := 0; i < 2; i++ {
		go func() {
			app.dispatchSubagent(context.Background(), "check", io.Discard, "")
			done <- struct{}{}
		}()
	}
	<-done
	<-done

	if n := probeCount.Load(); n != 1 {
		t.Errorf("probe count = %d, want exactly 1 (singleflight across parallel dispatches)", n)
	}
}

// --- 5. Cost fold ---

// TestCostFoldSequentialDispatchViaAppHandleToolCall verifies the sequential
// dispatch_subagent path: a child turn with mock usage on a differently-priced
// model increases the parent tracker by the child-rate amount ONLY after the
// join point (handleToolCall's dispatch_subagent case) has consumed the
// result — i.e. dispatchSubagent alone must not touch parent state.
func TestCostFoldSequentialDispatchViaAppHandleToolCall(t *testing.T) {
	summaryJSON := `{"objective":"check","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+contentChunk(summaryJSON)+"\n\n")
		fmt.Fprint(w, `data: {"usage":{"prompt_tokens":1000,"completion_tokens":500}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.Costs = config.CostsConfig{Inference: config.InferenceRate{USDPer1MTokens: 4.0}}
	app.Costs = proxy.NewCostTracker()

	// Before dispatch: parent tracker empty.
	totalBefore, _ := app.Costs.Snapshot()
	if totalBefore != 0 {
		t.Fatalf("parent tracker should start empty, got %v", totalBefore)
	}

	// Call dispatchSubagent directly (as a worker would) — this must NOT fold
	// into the parent tracker by itself.
	_, _, _, _, costRows := app.dispatchSubagent(context.Background(), "check", io.Discard, "")
	totalAfterDispatchOnly, _ := app.Costs.Snapshot()
	if totalAfterDispatchOnly != 0 {
		t.Errorf("parent tracker changed by dispatchSubagent alone (got %v) — fold must happen only at the join point", totalAfterDispatchOnly)
	}
	if len(costRows) == 0 {
		t.Fatal("expected child cost rows from a priced usage turn")
	}

	// Now the join-point fold, exactly as app.go's dispatch_subagent case does.
	subagentCostUSD := foldSubagentCost(app.Costs, costRows)
	// 1500 tokens / 1e6 * 4.0 = 0.006
	wantUSD := float64(1500) / 1e6 * 4.0
	if subagentCostUSD < wantUSD*0.99 || subagentCostUSD > wantUSD*1.01 {
		t.Errorf("folded cost = %v, want ~%v", subagentCostUSD, wantUSD)
	}
	totalAfter, _ := app.Costs.Snapshot()
	if totalAfter < wantUSD*0.99 || totalAfter > wantUSD*1.01 {
		t.Errorf("parent tracker total after fold = %v, want ~%v", totalAfter, wantUSD)
	}
}

// TestCostFoldParallelTwoSubagentsRace runs two parallel subagent dispatches
// each with priced usage and verifies BOTH fold correctly with the right
// total, exercising the exact Phase B/Phase C split runParallelSubagentBlock
// uses. Intended to run under -race.
func TestCostFoldParallelTwoSubagentsRace(t *testing.T) {
	summaryJSON := func(task string) string {
		return fmt.Sprintf(`{"objective":%q,"findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`, task)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		task := "A"
		if strings.Contains(string(body), "TASK-B") {
			task = "B"
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+contentChunk(summaryJSON("TASK-"+task))+"\n\n")
		fmt.Fprint(w, `data: {"usage":{"prompt_tokens":1000,"completion_tokens":500}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.Costs = config.CostsConfig{Inference: config.InferenceRate{USDPer1MTokens: 4.0}}
	app.Costs = proxy.NewCostTracker()
	app.ensureSubagentLimitsCache()

	jobs := []subagentJob{
		{Index: 0, Task: "TASK-A", ChatID: NewChatID()},
		{Index: 1, Task: "TASK-B", ChatID: NewChatID()},
	}
	results := app.runSubagentJobs(context.Background(), jobs, "")
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}

	// Phase C fold — main goroutine, after wg.Wait() inside runSubagentJobs
	// has already joined every worker.
	var total float64
	for _, r := range results {
		total += foldSubagentCost(app.Costs, r.CostRows)
	}
	// Two turns × 1500 tokens / 1e6 * 4.0 = 0.012
	want := float64(1500) / 1e6 * 4.0 * 2
	if total < want*0.99 || total > want*1.01 {
		t.Errorf("total folded cost = %v, want ~%v", total, want)
	}
	parentTotal, rows := app.Costs.Snapshot()
	if parentTotal < want*0.99 || parentTotal > want*1.01 {
		t.Errorf("parent tracker total = %v, want ~%v", parentTotal, want)
	}
	if len(rows) == 0 {
		t.Error("expected at least one row in parent tracker after fold")
	}
}

// --- 6. Command / config ---

// TestSubagentCommandBadName: /subagent <bad-name> → error naming the key,
// and the override is NOT set.
func TestSubagentCommandBadName(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.Endpoints = map[string]config.EndpointConfig{
		"real": {Kind: config.EndpointKindOpenAI, BaseURL: "http://x", Model: "m"},
	}

	_, _, cmd := HandleTUICommand("/subagent nope", app)
	msgs := runCmd(cmd)
	if len(msgs) != 1 {
		t.Fatalf("want 1 msg, got %d", len(msgs))
	}
	sn, ok := msgs[0].(SysNoteMsg)
	if !ok {
		t.Fatalf("want SysNoteMsg, got %T", msgs[0])
	}
	if !strings.Contains(sn.Text, "nope") {
		t.Errorf("error should name the bad key %q, got: %q", "nope", sn.Text)
	}
	if app.SubagentEndpointOverride != "" {
		t.Errorf("override should remain unset after a bad name, got %q", app.SubagentEndpointOverride)
	}
}

// TestSubagentCommandInheritResets: /subagent <name> then /subagent inherit
// clears the session override back to config-following.
func TestSubagentCommandInheritResets(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.Endpoints = map[string]config.EndpointConfig{
		"real": {Kind: config.EndpointKindOpenAI, BaseURL: "http://x", Model: "m"},
	}

	_, _, cmd := HandleTUICommand("/subagent real", app)
	runCmd(cmd)
	if app.SubagentEndpointOverride != "real" {
		t.Fatalf("override = %q, want real", app.SubagentEndpointOverride)
	}

	_, _, cmd2 := HandleTUICommand("/subagent inherit", app)
	msgs := runCmd(cmd2)
	if app.SubagentEndpointOverride != "" {
		t.Errorf("override after inherit = %q, want empty", app.SubagentEndpointOverride)
	}
	sn, ok := msgs[0].(SysNoteMsg)
	if !ok || !strings.Contains(sn.Text, "inherit") {
		t.Errorf("want inherit confirmation note, got %#v", msgs[0])
	}
}

// TestSubagentCommandNoArgShowsCurrent: /subagent with no arg reports the
// resolved endpoint (or "inherit" when none configured/overridden).
func TestSubagentCommandNoArgShowsCurrent(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	_, _, cmd := HandleTUICommand("/subagent", app)
	msgs := runCmd(cmd)
	sn, ok := msgs[0].(SysNoteMsg)
	if !ok || !strings.Contains(sn.Text, "inherit") {
		t.Errorf("want inherit note with no config/override, got %#v", msgs[0])
	}
}

// Config-level validation of subagent_endpoint (validateSubagentEndpoint,
// unexported in package config) is tested directly in
// internal/config/config_test.go — see TestValidateSubagentEndpoint.

// TestSubagentBackendIgnoredWhenChildEndpointOpenAI verifies subagent_backend
// has no effect at all when the child's resolved endpoint is kind openai —
// resolveSubagentBackendForEndpoint returns "" regardless of the configured
// pinned backend name.
func TestSubagentBackendIgnoredWhenChildEndpointOpenAI(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.SubagentBackend = "llama"
	app.Client.Kind = proxy.KindOpenAI // inherited child is openai

	got := app.resolveSubagentBackendForEndpoint(app.resolvedSubagentEndpointKind())
	if got != "" {
		t.Errorf("backend = %q, want \"\" (subagent_backend ignored for openai child)", got)
	}
}

// --- /submodel command tests ---

// TestSubmodelCommandSetsOverride: /submodel <name> sets the session override
// and clears the limits cache (so the next dispatch re-probes).
func TestSubmodelCommandSetsOverride(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.ensureSubagentLimitsCache()

	_, _, cmd := HandleTUICommand("/submodel qwen3-8b", app)
	msgs := runCmd(cmd)
	if app.SubagentModelOverride != "qwen3-8b" {
		t.Errorf("override = %q, want qwen3-8b", app.SubagentModelOverride)
	}
	if app.subagentLimitsCachePtr != nil {
		t.Error("limits cache should be cleared after /submodel switch")
	}
	sn, ok := msgs[0].(SysNoteMsg)
	if !ok || !strings.Contains(sn.Text, "qwen3-8b") {
		t.Errorf("want confirmation note with model name, got %#v", msgs[0])
	}
}

// TestSubmodelCommandInheritResets: /submodel inherit clears the override.
func TestSubmodelCommandInheritResets(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SubagentModelOverride = "old-model"

	_, _, cmd := HandleTUICommand("/submodel inherit", app)
	msgs := runCmd(cmd)
	if app.SubagentModelOverride != "" {
		t.Errorf("override after inherit = %q, want empty", app.SubagentModelOverride)
	}
	sn, ok := msgs[0].(SysNoteMsg)
	if !ok || !strings.Contains(sn.Text, "inherit") {
		t.Errorf("want inherit note, got %#v", msgs[0])
	}
}

// TestSubmodelCommandNoArgShowsCurrent: /submodel with no arg reports the
// resolved model (override if set, else the endpoint's model).
func TestSubmodelCommandNoArgShowsCurrent(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Client.Model = "endpoint-model"

	// No override set → shows endpoint's model.
	_, _, cmd := HandleTUICommand("/submodel", app)
	msgs := runCmd(cmd)
	sn, ok := msgs[0].(SysNoteMsg)
	if !ok || !strings.Contains(sn.Text, "endpoint-model") {
		t.Errorf("want endpoint model, got %#v", msgs[0])
	}

	// With override set → shows the override.
	app.SubagentModelOverride = "override-model"
	_, _, cmd = HandleTUICommand("/submodel", app)
	msgs = runCmd(cmd)
	sn, ok = msgs[0].(SysNoteMsg)
	if !ok || !strings.Contains(sn.Text, "override-model") {
		t.Errorf("want override model, got %#v", msgs[0])
	}
}

// TestSubmodelOverrideAppliedToInheritedView: with no endpoint override,
// /submodel patches the inherited view's model field.
func TestSubmodelOverrideAppliedToInheritedView(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Client.Kind = proxy.KindOpenAI
	app.Client.Model = "parent-model"
	app.Client.ConfiguredModel = "parent-model"
	app.SubagentModelOverride = "child-model"

	view, inherited := app.resolveSubagentEndpointView("")
	if !inherited {
		t.Fatal("expected inherited=true")
	}
	if view.model != "child-model" {
		t.Errorf("model = %q, want child-model (override applied)", view.model)
	}
	if view.configuredModel != "child-model" {
		t.Errorf("configuredModel = %q, want child-model (openai kind: ConfiguredModel follows override)", view.configuredModel)
	}
}

// TestSubmodelOverrideAppliedToEndpointOverride: /submodel also patches the
// model when a named endpoint override is active — composes with /subagent.
func TestSubmodelOverrideAppliedToEndpointOverride(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.Endpoints = map[string]config.EndpointConfig{
		"oa": {Kind: config.EndpointKindOpenAI, BaseURL: "http://x", Model: "endpoint-model"},
	}
	app.Cfg.SubagentEndpoint = "oa"
	app.SubagentModelOverride = "override-model"

	view, inherited := app.resolveSubagentEndpointView("oa")
	if inherited {
		t.Fatal("expected override, got inherited=true")
	}
	if view.model != "override-model" {
		t.Errorf("model = %q, want override-model", view.model)
	}
	if view.configuredModel != "override-model" {
		t.Errorf("configuredModel = %q, want override-model (openai)", view.configuredModel)
	}
}

// TestSubmodelOverrideProxyKindDoesNotTouchConfiguredModel: for kind=ilm-proxy,
// /submodel sets only Model (the alias/routing string), not ConfiguredModel
// (which is ignored for proxy kind).
func TestSubmodelOverrideProxyKindDoesNotTouchConfiguredModel(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Client.Kind = proxy.KindIlmProxy
	app.Client.Model = "ilm"
	app.Client.ConfiguredModel = "" // proxy kind doesn't use it
	app.SubagentModelOverride = "openrouter/anthropic/claude-opus-4-8"

	view, _ := app.resolveSubagentEndpointView("")
	if view.model != "openrouter/anthropic/claude-opus-4-8" {
		t.Errorf("model = %q, want the override string", view.model)
	}
	if view.configuredModel != "" {
		t.Errorf("configuredModel = %q, want empty (proxy kind ignores it)", view.configuredModel)
	}
}

// TestSubmodelOverrideInheritNoOp: "inherit" (or "") is a no-op — the view's
// model stays whatever the endpoint says.
func TestSubmodelOverrideInheritNoOp(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Client.Model = "parent-model"
	app.SubagentModelOverride = "inherit"

	view, _ := app.resolveSubagentEndpointView("")
	if view.model != "parent-model" {
		t.Errorf("model = %q, want parent-model (inherit is a no-op)", view.model)
	}
}
