package counsel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseModelPrefix verifies that the provider prefix is correctly split from
// the model ID, and that bare names (no colon) default to "anthropic".
func TestParseModelPrefix(t *testing.T) {
	cases := []struct {
		in    string
		prov  string
		model string
	}{
		{"anthropic:claude-opus-4-8", "anthropic", "claude-opus-4-8"},
		{"openrouter:google/gemini-2.5-pro", "openrouter", "google/gemini-2.5-pro"},
		{"claude-opus-4-8", "anthropic", "claude-opus-4-8"}, // bare name → anthropic
		{"openrouter:anthropic/claude-3.7-sonnet", "openrouter", "anthropic/claude-3.7-sonnet"},
		{"claude-fable-5", "anthropic", "claude-fable-5"}, // bare name, no slash
	}
	for _, c := range cases {
		prov, model := ParseModelPrefix(c.in)
		if prov != c.prov || model != c.model {
			t.Errorf("ParseModelPrefix(%q) = (%q, %q), want (%q, %q)",
				c.in, prov, model, c.prov, c.model)
		}
	}
}

// TestOpenRouterRequestShape verifies that callOpenRouter sends a valid
// OpenAI-compatible chat completions request: correct headers, model, max_tokens,
// system message, and user message containing the question and context.
func TestOpenRouterRequestShape(t *testing.T) {
	var reqBody []byte
	var authHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		reqBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"panel answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 512, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL}
	answer, usage, err := callOpenRouter(context.Background(), "google/gemini-2.5-pro", "test-api-key", "question text?", "some context", ccfg)
	if err != nil {
		t.Fatalf("callOpenRouter: %v", err)
	}
	if answer != "panel answer" {
		t.Errorf("answer = %q, want 'panel answer'", answer)
	}
	if usage.InputTokens != 10 || usage.OutputTokens != 5 {
		t.Errorf("usage = %+v, want {10, 5}", usage)
	}

	// Authorization header uses Bearer scheme (not x-api-key like Anthropic).
	if authHeader != "Bearer test-api-key" {
		t.Errorf("Authorization = %q, want 'Bearer test-api-key'", authHeader)
	}

	// Request body must be valid OpenAI chat completions format.
	var req orReq
	if e := json.Unmarshal(reqBody, &req); e != nil {
		t.Fatalf("request body not valid JSON: %v\n%s", e, reqBody)
	}
	if req.Model != "google/gemini-2.5-pro" {
		t.Errorf("model = %q, want 'google/gemini-2.5-pro'", req.Model)
	}
	if req.MaxTokens != 512 {
		t.Errorf("max_tokens = %d, want 512", req.MaxTokens)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("messages count = %d, want 2 (system + user)", len(req.Messages))
	}
	if req.Messages[0].Role != "system" {
		t.Errorf("messages[0].role = %q, want 'system'", req.Messages[0].Role)
	}
	if req.Messages[0].Content == "" {
		t.Error("system message content must not be empty")
	}
	if req.Messages[1].Role != "user" {
		t.Errorf("messages[1].role = %q, want 'user'", req.Messages[1].Role)
	}
	if !strings.Contains(req.Messages[1].Content, "question text?") {
		t.Errorf("user message missing question; got: %q", req.Messages[1].Content)
	}
	if !strings.Contains(req.Messages[1].Content, "some context") {
		t.Errorf("user message missing context; got: %q", req.Messages[1].Content)
	}
}

// TestOpenRouterTruncationReturnsUsage verifies that a length-truncated
// OpenRouter response returns actual usage (the call was billed), and that
// the truncation check runs before the empty-content check so a truncated
// response with empty content still returns usage.
func TestOpenRouterTruncationReturnsUsage(t *testing.T) {
	// Truncated response with non-empty content.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"partial"},"finish_reason":"length"}],"usage":{"prompt_tokens":15,"completion_tokens":8}}`))
	}))
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 512, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL}
	_, usage, err := callOpenRouter(context.Background(), "test-model", "key", "q", "ctx", ccfg)
	if err == nil {
		t.Fatal("expected truncation error")
	}
	if !strings.Contains(err.Error(), "truncated at max_tokens") {
		t.Errorf("error = %v, want truncation message", err)
	}
	if usage.InputTokens != 15 || usage.OutputTokens != 8 {
		t.Errorf("truncation usage = %+v, want {15, 8}", usage)
	}
}

// TestOpenRouterTruncationEmptyContentReturnsUsage verifies that a
// length-truncated response with EMPTY content still returns usage.
// This is the ordering bug Mashūra caught: the empty-text check must not
// run before the finish_reason check.
func TestOpenRouterTruncationEmptyContentReturnsUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"length"}],"usage":{"prompt_tokens":15,"completion_tokens":8}}`))
	}))
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 512, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL}
	_, usage, err := callOpenRouter(context.Background(), "test-model", "key", "q", "ctx", ccfg)
	if err == nil {
		t.Fatal("expected truncation error")
	}
	if !strings.Contains(err.Error(), "truncated at max_tokens") {
		t.Errorf("error = %v, want truncation message (not empty-content)", err)
	}
	if usage.InputTokens != 15 || usage.OutputTokens != 8 {
		t.Errorf("truncation usage = %+v, want {15, 8}", usage)
	}
}

// TestFusionTruncationReturnsUsage verifies that a length-truncated fusion
// response returns actual usage.
func TestFusionTruncationReturnsUsage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"length"}],"usage":{"prompt_tokens":20,"completion_tokens":10}}`))
	}))
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 512, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL}
	_, usage, err := callFusion(context.Background(), []string{"~anthropic/claude"}, "key", "q", "ctx", ccfg)
	if err == nil {
		t.Fatal("expected truncation error")
	}
	if !strings.Contains(err.Error(), "truncated at max_tokens") {
		t.Errorf("error = %v, want truncation message", err)
	}
	if usage.InputTokens != 20 || usage.OutputTokens != 10 {
		t.Errorf("truncation usage = %+v, want {20, 10}", usage)
	}
}

// TestRunPanelModeCollectsAll verifies that panel mode queries all members and
// collects every result — including errors from failing members — without stopping.
// With WP-7.7 parallelization, members run concurrently; the test server
// dispatches based on the request body's model field, not call order.
func TestRunPanelModeCollectsAll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		if req.Model == "model-a" {
			// First member: success.
			w.Write([]byte(`{"content":[{"type":"text","text":"first answer"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`))
		} else {
			// Second member: API error.
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":{"type":"server_error","message":"simulated failure"}}`))
		}
	}))
	defer srv.Close()

	models := []string{"anthropic:model-a", "anthropic:model-b"}
	apiKeys := map[string]string{"anthropic": "test-key"}
	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, AnthropicEndpoint: srv.URL + "/v1/messages"}

	results := RunPanel(context.Background(), models, "panel", "question?", "briefing", ccfg, apiKeys)

	if len(results) != 2 {
		t.Fatalf("panel mode: want 2 results, got %d", len(results))
	}
	if results[0].Err != nil {
		t.Errorf("first member: unexpected error: %v", results[0].Err)
	}
	if results[0].Answer != "first answer" {
		t.Errorf("first member: answer = %q, want 'first answer'", results[0].Answer)
	}
	if results[0].Model != "model-a" {
		t.Errorf("first member: model = %q, want 'model-a'", results[0].Model)
	}
	if results[1].Err == nil {
		t.Error("second member: expected an error")
	}
	if results[1].Answer != "" {
		t.Error("second member: answer must be empty when error")
	}
}

// TestRunPanelFallbackStopsOnSuccess verifies that fallback mode stops querying
// after the first successful response, and records prior failures in the result.
func TestRunPanelFallbackStopsOnSuccess(t *testing.T) {
	callN := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callN++
		w.Header().Set("Content-Type", "application/json")
		if callN == 1 {
			// First member: API error.
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte(`{"error":{"type":"gateway_error","message":"upstream down"}}`))
		} else {
			// Second member: success.
			w.Write([]byte(`{"content":[{"type":"text","text":"fallback answer"}],"stop_reason":"end_turn","usage":{"input_tokens":8,"output_tokens":4}}`))
		}
	}))
	defer srv.Close()

	models := []string{"anthropic:primary", "anthropic:secondary", "anthropic:tertiary"}
	apiKeys := map[string]string{"anthropic": "test-key"}
	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, AnthropicEndpoint: srv.URL + "/v1/messages"}

	results := RunPanel(context.Background(), models, "fallback", "question?", "briefing", ccfg, apiKeys)

	// Only 2 calls: first failed, second succeeded, third never tried.
	if callN != 2 {
		t.Errorf("fallback: expected 2 calls (stop on first success), got %d", callN)
	}
	if len(results) != 2 {
		t.Fatalf("fallback: want 2 results (1 fail + 1 success), got %d", len(results))
	}
	// First result: failure recorded.
	if results[0].Err == nil {
		t.Error("fallback: first result should record the prior failure")
	}
	if results[0].Model != "primary" {
		t.Errorf("fallback: results[0].Model = %q, want 'primary'", results[0].Model)
	}
	// Second result: success.
	if results[1].Err != nil {
		t.Errorf("fallback: second result should succeed: %v", results[1].Err)
	}
	if results[1].Answer != "fallback answer" {
		t.Errorf("fallback: second answer = %q, want 'fallback answer'", results[1].Answer)
	}
	if results[1].Model != "secondary" {
		t.Errorf("fallback: results[1].Model = %q, want 'secondary'", results[1].Model)
	}
}

// TestFusionRequestShape verifies that callFusion sends the correct OpenRouter
// Fusion request: model="openrouter/fusion", plugins block with analysis_models,
// judge, max_tool_calls, and Bearer auth.
func TestFusionRequestShape(t *testing.T) {
	var reqBody []byte
	var authHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		reqBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"fusion analysis"},"finish_reason":"stop"}],"usage":{"prompt_tokens":20,"completion_tokens":10}}`))
	}))
	defer srv.Close()

	analysisModels := []string{
		"~anthropic/claude-opus-latest",
		"~openai/gpt-latest",
		"~google/gemini-pro-latest",
	}
	ccfg := PanelCallConfig{
		MaxTokens:          1024,
		TimeoutSeconds:     5,
		OpenRouterEndpoint: srv.URL,
		FusionJudge:        "~anthropic/claude-opus-latest",
		FusionMaxToolCalls: 4,
	}

	answer, usage, err := callFusion(context.Background(), analysisModels, "test-key", "question?", "ctx", ccfg)
	if err != nil {
		t.Fatalf("callFusion: %v", err)
	}
	if answer != "fusion analysis" {
		t.Errorf("answer = %q", answer)
	}
	if usage.InputTokens != 20 || usage.OutputTokens != 10 {
		t.Errorf("usage = %+v", usage)
	}
	if authHeader != "Bearer test-key" {
		t.Errorf("Authorization = %q", authHeader)
	}

	// Decode and verify the request body.
	var req orReq
	if e := json.Unmarshal(reqBody, &req); e != nil {
		t.Fatalf("request body not valid JSON: %v\n%s", e, reqBody)
	}
	if req.Model != "openrouter/fusion" {
		t.Errorf("model = %q, want openrouter/fusion", req.Model)
	}
	if req.MaxTokens != 1024 {
		t.Errorf("max_tokens = %d, want 1024", req.MaxTokens)
	}
	if len(req.Plugins) != 1 {
		t.Fatalf("plugins count = %d, want 1", len(req.Plugins))
	}
	p := req.Plugins[0]
	if p.ID != "fusion" {
		t.Errorf("plugin id = %q, want fusion", p.ID)
	}
	if len(p.AnalysisModels) != 3 {
		t.Errorf("analysis_models count = %d, want 3", len(p.AnalysisModels))
	}
	if p.AnalysisModels[0] != "~anthropic/claude-opus-latest" {
		t.Errorf("analysis_models[0] = %q", p.AnalysisModels[0])
	}
	if p.Model != "~anthropic/claude-opus-latest" {
		t.Errorf("judge model = %q, want ~anthropic/claude-opus-latest", p.Model)
	}
	if p.MaxToolCalls != 4 {
		t.Errorf("max_tool_calls = %d, want 4", p.MaxToolCalls)
	}
	// Messages must be system + user.
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
		t.Errorf("messages = %+v", req.Messages)
	}
}

// TestRunPanelFusionMode verifies that RunPanel with mode="fusion" makes exactly
// one call and returns a single result labeled "openrouter/fusion".
func TestRunPanelFusionMode(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"synthesized"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	models := []string{"~anthropic/claude-opus-latest", "~openai/gpt-latest"}
	apiKeys := map[string]string{"openrouter": "test-key"}
	ccfg := PanelCallConfig{MaxTokens: 512, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL}

	results := RunPanel(context.Background(), models, "fusion", "question?", "briefing", ccfg, apiKeys)

	if callCount != 1 {
		t.Errorf("fusion mode: expected exactly 1 HTTP call, got %d", callCount)
	}
	if len(results) != 1 {
		t.Fatalf("fusion mode: expected 1 result, got %d", len(results))
	}
	if results[0].Model != "openrouter/fusion" {
		t.Errorf("result model = %q, want openrouter/fusion", results[0].Model)
	}
	if results[0].Answer != "synthesized" {
		t.Errorf("result answer = %q", results[0].Answer)
	}
	if results[0].Err != nil {
		t.Errorf("result error = %v", results[0].Err)
	}
}

// TestPanelDetailFusionMode verifies the gate detail for fusion panels.
func TestPanelDetailFusionMode(t *testing.T) {
	models := []string{"~anthropic/claude-opus-latest", "~openai/gpt-latest"}
	detail := PanelDetail("my-fusion", models, "fusion", "the question", "briefing ctx")
	if !strings.Contains(detail, "fusion") {
		t.Error("fusion detail should mention fusion mode")
	}
	if !strings.Contains(detail, "~anthropic/claude-opus-latest") {
		t.Error("fusion detail should list analysis models")
	}
	if !strings.Contains(detail, "the question") {
		t.Error("fusion detail should include question")
	}
}

// TestPanelDetailSingleModel verifies that PanelDetail for a 1-model panel
// produces the same format as OracleDetail (backward compat).
func TestPanelDetailSingleModel(t *testing.T) {
	oracle := OracleDetail("claude-opus-4-8", "the question", "ctx body")
	panel := PanelDetail("default", []string{"anthropic:claude-opus-4-8"}, "panel", "the question", "ctx body")
	if oracle != panel {
		t.Errorf("single-model PanelDetail differs from OracleDetail:\noracle: %q\npanel:  %q", oracle, panel)
	}
}

// TestFormatPanelResultSingle verifies single-member results are returned verbatim.
func TestFormatPanelResultSingle(t *testing.T) {
	r := FormatPanelResult([]PanelMemberResult{{Model: "m", Answer: "the answer"}})
	if r != "the answer" {
		t.Errorf("single result = %q, want 'the answer'", r)
	}
	errR := FormatPanelResult([]PanelMemberResult{{Model: "m", Err: fmt.Errorf("boom")}})
	if !strings.Contains(errR, "mashūra error") {
		t.Errorf("single error result = %q, missing 'mashūra error'", errR)
	}
}

// TestFormatPanelResultMulti verifies that multi-member results get labeled sections.
func TestFormatPanelResultMulti(t *testing.T) {
	results := []PanelMemberResult{
		{Model: "claude-opus-4-8", Answer: "model a says yes"},
		{Model: "gemini-2.5-pro", Answer: "model b says no"},
	}
	out := FormatPanelResult(results)
	if !strings.Contains(out, "── claude-opus-4-8 ──") {
		t.Errorf("missing label for first model; got: %q", out)
	}
	if !strings.Contains(out, "── gemini-2.5-pro ──") {
		t.Errorf("missing label for second model; got: %q", out)
	}
	if !strings.Contains(out, "model a says yes") {
		t.Error("missing first model's answer")
	}
	if !strings.Contains(out, "model b says no") {
		t.Error("missing second model's answer")
	}
}

// ── Debate mode tests ────────────────────────────────────────────────────────

// TestRunPanelDebateBasic verifies that debate mode executes two rounds:
// round 1 (independent) then round 2 (critique). Each member should be
// called twice — once per round.
func TestRunPanelDebateBasic(t *testing.T) {
	callCount := 0
	// Track which round each call belongs to by counting calls per model.
	modelCalls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		json.Unmarshal(body, &req)
		modelCalls[req.Model]++
		w.Header().Set("Content-Type", "application/json")
		// First call per model = round 1, second call = round 2.
		if modelCalls[req.Model] == 1 {
			w.Write([]byte(`{"content":[{"type":"text","text":"round1 answer from ` + req.Model + `"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`))
		} else {
			w.Write([]byte(`{"content":[{"type":"text","text":"round2 refined answer from ` + req.Model + `"}],"stop_reason":"end_turn","usage":{"input_tokens":20,"output_tokens":8}}`))
		}
	}))
	defer srv.Close()

	models := []string{"anthropic:model-a", "anthropic:model-b"}
	apiKeys := map[string]string{"anthropic": "test-key"}
	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, AnthropicEndpoint: srv.URL + "/v1/messages"}

	results := RunPanel(context.Background(), models, "debate", "question?", "briefing", ccfg, apiKeys)

	if len(results) != 2 {
		t.Fatalf("debate mode: want 2 results, got %d", len(results))
	}
	// Each model should be called twice (round 1 + round 2).
	if modelCalls["model-a"] != 2 {
		t.Errorf("model-a: expected 2 calls, got %d", modelCalls["model-a"])
	}
	if modelCalls["model-b"] != 2 {
		t.Errorf("model-b: expected 2 calls, got %d", modelCalls["model-b"])
	}
	// Results should contain round-2 answers.
	for i, r := range results {
		if r.Err != nil {
			t.Errorf("result %d: unexpected error: %v", i, r.Err)
		}
		if !strings.Contains(r.Answer, "round2 refined") {
			t.Errorf("result %d: answer = %q, want round2 refined answer", i, r.Answer)
		}
		// Usage should be accumulated from both rounds (10+20 input, 5+8 output).
		if r.Usage.InputTokens != 30 {
			t.Errorf("result %d: input tokens = %d, want 30", i, r.Usage.InputTokens)
		}
		if r.Usage.OutputTokens != 13 {
			t.Errorf("result %d: output tokens = %d, want 13", i, r.Usage.OutputTokens)
		}
	}
}

// TestRunPanelDebateRound1FailureDropsMember verifies that a member that
// fails in round 1 drops out of round 2 (is not called again).
func TestRunPanelDebateRound1FailureDropsMember(t *testing.T) {
	modelCalls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		json.Unmarshal(body, &req)
		modelCalls[req.Model]++
		w.Header().Set("Content-Type", "application/json")
		if req.Model == "model-a" {
			// model-a always fails.
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":{"type":"server_error","message":"simulated failure"}}`))
		} else {
			// model-b succeeds in both rounds.
			w.Write([]byte(`{"content":[{"type":"text","text":"answer from model-b"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`))
		}
	}))
	defer srv.Close()

	models := []string{"anthropic:model-a", "anthropic:model-b"}
	apiKeys := map[string]string{"anthropic": "test-key"}
	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, AnthropicEndpoint: srv.URL + "/v1/messages"}

	results := RunPanel(context.Background(), models, "debate", "question?", "briefing", ccfg, apiKeys)

	// model-a should be called only once (round 1 — it failed, so it drops out).
	if modelCalls["model-a"] != 1 {
		t.Errorf("model-a: expected 1 call (round 1 only, failed), got %d", modelCalls["model-a"])
	}
	// model-b should be called twice (round 1 + round 2).
	if modelCalls["model-b"] != 2 {
		t.Errorf("model-b: expected 2 calls, got %d", modelCalls["model-b"])
	}
	// Result 0 (model-a) should have an error.
	if results[0].Err == nil {
		t.Error("model-a: expected an error from round 1 failure")
	}
	// Result 1 (model-b) should have a round-2 answer.
	if results[1].Err != nil {
		t.Errorf("model-b: unexpected error: %v", results[1].Err)
	}
	if !strings.Contains(results[1].Answer, "answer from model-b") {
		t.Errorf("model-b: answer = %q", results[1].Answer)
	}
}

// TestRunPanelDebateAllFailRound1 verifies that when all members fail in
// round 1, no round 2 calls are made and all errors are returned.
func TestRunPanelDebateAllFailRound1(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.WriteHeader(http.StatusBadGateway)
		w.Write([]byte(`{"error":{"type":"gateway_error","message":"all down"}}`))
	}))
	defer srv.Close()

	models := []string{"anthropic:model-a", "anthropic:model-b"}
	apiKeys := map[string]string{"anthropic": "test-key"}
	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, AnthropicEndpoint: srv.URL + "/v1/messages"}

	results := RunPanel(context.Background(), models, "debate", "question?", "briefing", ccfg, apiKeys)

	// Only round 1 calls should have been made (2 calls, no round 2).
	if callCount != 2 {
		t.Errorf("expected 2 calls (round 1 only), got %d", callCount)
	}
	for i, r := range results {
		if r.Err == nil {
			t.Errorf("result %d: expected error, got nil", i)
		}
	}
}

// TestRunPanelDebateMaxParticipants verifies that exceeding the max
// participants returns an error without making any HTTP calls.
func TestRunPanelDebateMaxParticipants(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no HTTP calls should be made when max participants exceeded")
	}))
	defer srv.Close()

	// Create maxDebateParticipants + 1 models.
	models := make([]string, maxDebateParticipants+1)
	for i := range models {
		models[i] = "anthropic:model-" + string(rune('a'+i))
	}
	apiKeys := map[string]string{"anthropic": "test-key"}
	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, AnthropicEndpoint: srv.URL + "/v1/messages"}

	results := RunPanel(context.Background(), models, "debate", "question?", "briefing", ccfg, apiKeys)

	for i, r := range results {
		if r.Err == nil {
			t.Errorf("result %d: expected error for exceeding max participants", i)
		}
		if !strings.Contains(r.Err.Error(), "exceeds max") {
			t.Errorf("result %d: error = %q, want 'exceeds max'", i, r.Err.Error())
		}
	}
}

// TestRunPanelDebateSingleMember verifies that debate mode works with a
// single member (round 2 still runs, the member critiques itself).
func TestRunPanelDebateSingleMember(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		if callCount == 1 {
			w.Write([]byte(`{"content":[{"type":"text","text":"initial answer"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`))
		} else {
			w.Write([]byte(`{"content":[{"type":"text","text":"refined answer"}],"stop_reason":"end_turn","usage":{"input_tokens":15,"output_tokens":7}}`))
		}
	}))
	defer srv.Close()

	models := []string{"anthropic:single-model"}
	apiKeys := map[string]string{"anthropic": "test-key"}
	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, AnthropicEndpoint: srv.URL + "/v1/messages"}

	results := RunPanel(context.Background(), models, "debate", "question?", "briefing", ccfg, apiKeys)

	if callCount != 2 {
		t.Errorf("expected 2 calls (round 1 + round 2), got %d", callCount)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].Err != nil {
		t.Errorf("unexpected error: %v", results[0].Err)
	}
	if results[0].Answer != "refined answer" {
		t.Errorf("answer = %q, want 'refined answer'", results[0].Answer)
	}
}

// ── Unknown mode validation test ─────────────────────────────────────────────

// TestRunPanelUnknownModeFailsClosed verifies that an unrecognized mode
// produces per-member errors without making any HTTP calls.
func TestRunPanelUnknownModeFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("no HTTP calls should be made for unknown mode")
	}))
	defer srv.Close()

	models := []string{"anthropic:model-a", "anthropic:model-b"}
	apiKeys := map[string]string{"anthropic": "test-key"}
	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, AnthropicEndpoint: srv.URL + "/v1/messages"}

	results := RunPanel(context.Background(), models, "debtae", "question?", "briefing", ccfg, apiKeys)

	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	for i, r := range results {
		if r.Err == nil {
			t.Errorf("result %d: expected error for unknown mode", i)
		}
		if !strings.Contains(r.Err.Error(), "unknown panel mode") {
			t.Errorf("result %d: error = %q, want 'unknown panel mode'", i, r.Err.Error())
		}
	}
}

// ── PanelDetail debate mode test ─────────────────────────────────────────────

// TestPanelDetailDebateMode verifies the gate detail for debate panels
// includes the debate metadata and cross-provider sharing disclosure.
func TestPanelDetailDebateMode(t *testing.T) {
	models := []string{"anthropic:claude-opus-4-8", "openrouter:google/gemini-2.5-pro"}
	detail := PanelDetail("review-panel", models, "debate", "the question", "briefing ctx")
	if !strings.Contains(detail, "debate") {
		t.Error("debate detail should mention debate mode")
	}
	if !strings.Contains(detail, "2 rounds") {
		t.Error("debate detail should mention 2 rounds")
	}
	if !strings.Contains(detail, "shared across providers") {
		t.Error("debate detail should disclose cross-provider sharing")
	}
	if !strings.Contains(detail, "the question") {
		t.Error("debate detail should include the question")
	}
}
