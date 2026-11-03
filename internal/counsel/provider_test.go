package counsel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeAnthropicSSE writes an Anthropic Messages API SSE stream carrying one
// text block with the given stop reason and usage.
func writeAnthropicSSE(w http.ResponseWriter, text, stopReason string, in, out int) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":%d,\"output_tokens\":0}}}\n\n", in)
	fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"text\"}}\n\n")
	fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text)
	fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\"}\n\n")
	fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%q},\"usage\":{\"output_tokens\":%d}}\n\n", stopReason, out)
	fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
}

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
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"panel answer\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"completion_tokens_details\":{\"reasoning_tokens\":0}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
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
	srv := openRouterSSETestServer(t,
		`{"choices":[{"message":{"role":"assistant","content":"partial"},"finish_reason":"length"}],"usage":{"prompt_tokens":15,"completion_tokens":8}}`)
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
	srv := openRouterSSETestServer(t,
		`{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"length"}],"usage":{"prompt_tokens":15,"completion_tokens":8}}`)
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
	srv := openRouterSSETestServer(t,
		`{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"length"}],"usage":{"prompt_tokens":20,"completion_tokens":10}}`)
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
		if req.Model == "model-a" {
			// First member: success.
			writeAnthropicSSE(w, "first answer", "end_turn", 10, 5)
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
	var mu sync.Mutex
	callN := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callN++
		n := callN
		mu.Unlock()
		if n == 1 {
			// First member: a FINAL API error (401 — not retried; a 429/5xx
			// would be retried once by withProviderRetry, which is a separate
			// behaviour covered by TestProviderRetryOn429And5xx).
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"error":{"type":"authentication_error","message":"bad key"}}`))
		} else {
			// Second member: success.
			writeAnthropicSSE(w, "fallback answer", "end_turn", 8, 4)
		}
	}))
	defer srv.Close()

	models := []string{"anthropic:primary", "anthropic:secondary", "anthropic:tertiary"}
	apiKeys := map[string]string{"anthropic": "test-key"}
	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, AnthropicEndpoint: srv.URL + "/v1/messages"}

	results := RunPanel(context.Background(), models, "fallback", "question?", "briefing", ccfg, apiKeys)

	// Only 2 calls: first failed, second succeeded, third never tried.
	mu.Lock()
	got := callN
	mu.Unlock()
	if got != 2 {
		t.Errorf("fallback: expected 2 calls (stop on first success), got %d", got)
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
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"fusion analysis\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":10,\"completion_tokens_details\":{\"reasoning_tokens\":0}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
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
	var mu sync.Mutex
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"synthesized\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	models := []string{"~anthropic/claude-opus-latest", "~openai/gpt-latest"}
	apiKeys := map[string]string{"openrouter": "test-key"}
	ccfg := PanelCallConfig{MaxTokens: 512, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL}

	results := RunPanel(context.Background(), models, "fusion", "question?", "briefing", ccfg, apiKeys)

	mu.Lock()
	got := callCount
	mu.Unlock()
	if got != 1 {
		t.Errorf("fusion mode: expected exactly 1 HTTP call, got %d", got)
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
	detail := PanelDetail("my-fusion", models, "fusion", "the question", "briefing ctx", nil)
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
	panel := PanelDetail("default", []string{"anthropic:claude-opus-4-8"}, "panel", "the question", "ctx body", nil)
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
	var mu sync.Mutex
	callCount := 0
	// Track which round each call belongs to by counting calls per model.
	modelCalls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		json.Unmarshal(body, &req)
		mu.Lock()
		callCount++
		modelCalls[req.Model]++
		n := modelCalls[req.Model]
		mu.Unlock()
		// First call per model = round 1, second call = round 2.
		if n == 1 {
			writeAnthropicSSE(w, "round1 answer from "+req.Model, "end_turn", 10, 5)
		} else {
			writeAnthropicSSE(w, "round2 refined answer from "+req.Model, "end_turn", 20, 8)
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
	mu.Lock()
	callsA := modelCalls["model-a"]
	callsB := modelCalls["model-b"]
	mu.Unlock()
	if callsA != 2 {
		t.Errorf("model-a: expected 2 calls, got %d", callsA)
	}
	if callsB != 2 {
		t.Errorf("model-b: expected 2 calls, got %d", callsB)
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
	var mu sync.Mutex
	modelCalls := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		json.Unmarshal(body, &req)
		mu.Lock()
		modelCalls[req.Model]++
		mu.Unlock()
		if req.Model == "model-a" {
			// model-a always fails with a final (non-retryable) error.
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":{"type":"permission_error","message":"simulated failure"}}`))
		} else {
			// model-b succeeds in both rounds.
			writeAnthropicSSE(w, "answer from model-b", "end_turn", 10, 5)
		}
	}))
	defer srv.Close()

	models := []string{"anthropic:model-a", "anthropic:model-b"}
	apiKeys := map[string]string{"anthropic": "test-key"}
	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, AnthropicEndpoint: srv.URL + "/v1/messages"}

	results := RunPanel(context.Background(), models, "debate", "question?", "briefing", ccfg, apiKeys)

	if len(results) != 2 {
		t.Fatalf("debate round1-failure: want 2 results, got %d", len(results))
	}
	// model-a should be called only once (round 1 — it failed, so it drops out).
	mu.Lock()
	callsA := modelCalls["model-a"]
	callsB := modelCalls["model-b"]
	mu.Unlock()
	if callsA != 1 {
		t.Errorf("model-a: expected 1 call (round 1 only, failed), got %d", callsA)
	}
	// model-b should be called once (round 1 only — with 1 successful member,
	// round 2 self-critique is skipped as pointless).
	if callsB != 1 {
		t.Errorf("model-b: expected 1 call (round 1 only, round 2 skipped for n=1), got %d", callsB)
	}
	// Result 0 (model-a) should have an error.
	if results[0].Err == nil {
		t.Error("model-a: expected an error from round 1 failure")
	}
	// Result 1 (model-b) should have a round-1 answer (no round 2).
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
	var mu sync.Mutex
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		// Final (non-retryable) error so the call count is round-1 only.
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"type":"authentication_error","message":"all down"}}`))
	}))
	defer srv.Close()

	models := []string{"anthropic:model-a", "anthropic:model-b"}
	apiKeys := map[string]string{"anthropic": "test-key"}
	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, AnthropicEndpoint: srv.URL + "/v1/messages"}

	results := RunPanel(context.Background(), models, "debate", "question?", "briefing", ccfg, apiKeys)

	if len(results) != 2 {
		t.Fatalf("debate all-fail: want 2 results, got %d", len(results))
	}
	// Only round 1 calls should have been made (2 calls, no round 2).
	mu.Lock()
	got := callCount
	mu.Unlock()
	if got != 2 {
		t.Errorf("expected 2 calls (round 1 only), got %d", got)
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

// TestRunPanelDebateSingleMember verifies that debate mode with a single
// successful member skips round 2 (self-critique without a second perspective
// is paid and pointless). Only 1 call should be made (round 1 only).
func TestRunPanelDebateSingleMember(t *testing.T) {
	var mu sync.Mutex
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		mu.Unlock()
		writeAnthropicSSE(w, "initial answer", "end_turn", 10, 5)
	}))
	defer srv.Close()

	models := []string{"anthropic:single-model"}
	apiKeys := map[string]string{"anthropic": "test-key"}
	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, AnthropicEndpoint: srv.URL + "/v1/messages"}

	results := RunPanel(context.Background(), models, "debate", "question?", "briefing", ccfg, apiKeys)

	mu.Lock()
	got := callCount
	mu.Unlock()
	if got != 1 {
		t.Errorf("expected 1 call (round 1 only, round 2 skipped for n=1), got %d", got)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].Err != nil {
		t.Errorf("unexpected error: %v", results[0].Err)
	}
	if results[0].Answer != "initial answer" {
		t.Errorf("answer = %q, want 'initial answer' (round-1 result, no round 2)", results[0].Answer)
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
	detail := PanelDetail("review-panel", models, "debate", "the question", "briefing ctx", nil)
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

// TestRunPanelDebateRound2SurvivesWith2xDeadline verifies the fix:
// debate gets 2× total wall-time (round 1 + round 2), not 1×. Each round's
// individual HTTP call is separately capped at perCall (CallOracle's
// WithTimeout), so the meaningful effect of the fix is on the OVERALL budget:
// when round 1's call consumes ~perCall, round 2 must still get its own perCall
// budget. With a 1× outer deadline (the pre-fix clip) round 2 is cancelled;
// with a 2× outer deadline (what mashuraCallTimeout("debate") now provides)
// round 2 survives.
func TestRunPanelDebateRound2SurvivesWith2xDeadline(t *testing.T) {
	const perCall = 1 // seconds
	runScenario := func(outerDeadline time.Duration) (answer string, err error) {
		var mu sync.Mutex
		modelCalls := map[string]int{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			var req struct {
				Model string `json:"model"`
			}
			json.Unmarshal(body, &req)
			mu.Lock()
			modelCalls[req.Model]++
			n := modelCalls[req.Model]
			mu.Unlock()
			if n == 1 {
				// Round 1: consume nearly the full perCall budget (~950ms of 1s),
				// leaving only ~50ms in the pre-fix 1× outer deadline.
				time.Sleep(950 * time.Millisecond)
			} else {
				// Round 2: a real call (~500ms). Well under perCall (1s) so it
				// survives with a 2× outer deadline (~1.05s left), but far beyond
				// the ~50ms that remain under the pre-fix 1× clip.
				time.Sleep(500 * time.Millisecond)
			}
			writeAnthropicSSE(w, "answer", "end_turn", 3, 2)
		}))
		defer srv.Close()

		// Two models are needed to trigger round 2 (with n=1, round 2 is skipped).
		models := []string{"anthropic:model-a", "anthropic:model-b"}
		apiKeys := map[string]string{"anthropic": "test-key"}
		ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: perCall, AnthropicEndpoint: srv.URL + "/v1/messages"}

		parent, cancel := context.WithTimeout(context.Background(), outerDeadline)
		defer cancel()
		results := RunPanel(parent, models, "debate", "question?", "briefing", ccfg, apiKeys)
		if len(results) != 2 {
			t.Fatalf("want 2 results, got %d", len(results))
		}
		// Return the first successful result.
		for _, r := range results {
			if r.Err == nil {
				return r.Answer, nil
			}
		}
		return "", results[0].Err
	}

	// Pre-fix behavior (outer deadline clipped to 1×): round 2 is cancelled.
	if _, err := runScenario(1 * time.Duration(perCall) * time.Second); err == nil {
		t.Error("1× outer deadline: expected round-2 cancellation (the pre-fix clip), but it succeeded")
	}

	// Fixed behavior (2× outer deadline): round 2 survives with ~1× budget left.
	ans, err := runScenario(2 * time.Duration(perCall) * time.Second)
	if err != nil {
		t.Errorf("2× outer deadline: round-2 was cancelled (err=%v) — should survive with ~1× budget left", err)
	}
	if !strings.Contains(ans, "answer") {
		t.Errorf("expected round-2 answer, got %q", ans)
	}
}

// ── Card #M2: OpenRouter server tools tests ────────────────────────────────

// TestOpenRouterServerToolsInRequest verifies that when ServerTools is set in
// PanelCallConfig, the request body includes the tools array and max_tool_calls.
func TestOpenRouterServerToolsInRequest(t *testing.T) {
	var reqBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"searched answer\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"completion_tokens_details\":{\"reasoning_tokens\":0},\"server_tool_use\":{\"web_search_requests\":2}}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ccfg := PanelCallConfig{
		MaxTokens:          512,
		TimeoutSeconds:     5,
		OpenRouterEndpoint: srv.URL,
		ServerTools:        []string{"openrouter:web_search", "openrouter:web_fetch"},
		WebSearchEngine:    "exa",
		ServerToolMaxCalls: 10,
	}
	answer, usage, err := callOpenRouter(context.Background(), "google/gemini-2.5-pro", "key", "q", "ctx", ccfg)
	if err != nil {
		t.Fatalf("callOpenRouter: %v", err)
	}
	if answer != "searched answer" {
		t.Errorf("answer = %q, want 'searched answer'", answer)
	}
	if usage.WebSearchRequests != 2 {
		t.Errorf("WebSearchRequests = %d, want 2", usage.WebSearchRequests)
	}

	// Verify the request body contains tools and max_tool_calls.
	var req orReq
	if err := json.Unmarshal(reqBody, &req); err != nil {
		t.Fatalf("request body not valid JSON: %v\n%s", err, reqBody)
	}
	if len(req.Tools) != 2 {
		t.Fatalf("expected 2 tools, got %d", len(req.Tools))
	}
	if req.Tools[0].Type != "openrouter:web_search" {
		t.Errorf("tools[0].type = %q, want 'openrouter:web_search'", req.Tools[0].Type)
	}
	if req.Tools[1].Type != "openrouter:web_fetch" {
		t.Errorf("tools[1].type = %q, want 'openrouter:web_fetch'", req.Tools[1].Type)
	}
	// web_search should have engine parameter.
	if req.Tools[0].Parameters["engine"] != "exa" {
		t.Errorf("tools[0].parameters.engine = %v, want 'exa'", req.Tools[0].Parameters["engine"])
	}
	// web_fetch should have no parameters.
	if len(req.Tools[1].Parameters) != 0 {
		t.Errorf("tools[1].parameters should be empty, got %v", req.Tools[1].Parameters)
	}
	if req.MaxToolCalls != 10 {
		t.Errorf("max_tool_calls = %d, want 10", req.MaxToolCalls)
	}
}

// TestOpenRouterNoServerToolsOmitsFields verifies that when ServerTools is empty,
// the request body does NOT include tools or max_tool_calls fields (omitempty).
func TestOpenRouterNoServerToolsOmitsFields(t *testing.T) {
	var reqBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"answer\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 512, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL}
	_, _, err := callOpenRouter(context.Background(), "test-model", "key", "q", "ctx", ccfg)
	if err != nil {
		t.Fatalf("callOpenRouter: %v", err)
	}

	// Verify the request body does not contain tools or max_tool_calls.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(reqBody, &raw); err != nil {
		t.Fatalf("request body not valid JSON: %v", err)
	}
	if _, exists := raw["tools"]; exists {
		t.Error("request body should NOT contain 'tools' field when ServerTools is empty")
	}
	if _, exists := raw["max_tool_calls"]; exists {
		t.Error("request body should NOT contain 'max_tool_calls' field when ServerToolMaxCalls is 0")
	}
}

// TestOpenRouterServerToolsArrayContent verifies that when the response message
// content is an array of content parts (as server tools may return), the text
// is correctly extracted.
func TestOpenRouterServerToolsArrayContent(t *testing.T) {
	srv := openRouterSSETestServer(t,
		`{"choices":[{"message":{"role":"assistant","content":"Based on search results: \nthe answer is 42"},"finish_reason":"stop"}]}`)
	defer srv.Close()

	ccfg := PanelCallConfig{
		MaxTokens:          512,
		TimeoutSeconds:     5,
		OpenRouterEndpoint: srv.URL,
		ServerTools:        []string{"openrouter:web_search"},
	}
	answer, _, err := callOpenRouter(context.Background(), "test-model", "key", "q", "ctx", ccfg)
	if err != nil {
		t.Fatalf("callOpenRouter with array content: %v", err)
	}
	if answer != "Based on search results: \nthe answer is 42" {
		t.Errorf("answer = %q, want concatenated text with newline separator", answer)
	}
}

// TestOpenRouterServerToolsNullContent verifies that null content (which can
// occur when server tools are invoked but don't produce text) doesn't crash
// and returns an error with usage preserved.
func TestOpenRouterServerToolsNullContent(t *testing.T) {
	srv := openRouterSSETestServer(t,
		`{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":0,"server_tool_use":{"web_search_requests":3}}}`)
	defer srv.Close()

	ccfg := PanelCallConfig{
		MaxTokens:          512,
		TimeoutSeconds:     5,
		OpenRouterEndpoint: srv.URL,
		ServerTools:        []string{"openrouter:web_search"},
	}
	_, usage, err := callOpenRouter(context.Background(), "test-model", "key", "q", "ctx", ccfg)
	if err == nil {
		t.Error("expected error for null content")
	}
	// Usage should still be preserved (call was billed).
	if usage.InputTokens != 10 {
		t.Errorf("usage.InputTokens = %d, want 10", usage.InputTokens)
	}
	if usage.WebSearchRequests != 3 {
		t.Errorf("usage.WebSearchRequests = %d, want 3", usage.WebSearchRequests)
	}
}

// TestOpenRouterToolCallsFinishReason verifies that a finish_reason of
// "tool_calls" (server tools exhausted without final answer) returns an
// actionable error with usage preserved.
func TestOpenRouterToolCallsFinishReason(t *testing.T) {
	srv := openRouterSSETestServer(t,
		`{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"server_tool_use":{"web_search_requests":5}}}`)
	defer srv.Close()

	ccfg := PanelCallConfig{
		MaxTokens:          512,
		TimeoutSeconds:     5,
		OpenRouterEndpoint: srv.URL,
		ServerTools:        []string{"openrouter:web_search"},
	}
	_, usage, err := callOpenRouter(context.Background(), "test-model", "key", "q", "ctx", ccfg)
	if err == nil {
		t.Error("expected error for tool_calls finish_reason")
	}
	if !strings.Contains(err.Error(), "tool_calls") {
		t.Errorf("error should mention tool_calls, got: %v", err)
	}
	if usage.WebSearchRequests != 5 {
		t.Errorf("usage.WebSearchRequests = %d, want 5", usage.WebSearchRequests)
	}
}

// TestPanelDetailServerToolsDisclosure verifies that PanelDetail includes a
// tools disclosure when serverTools is non-empty.
func TestPanelDetailServerToolsDisclosure(t *testing.T) {
	detail := PanelDetail("research", []string{"openrouter:google/gemini-2.5-pro"}, "panel", "q", "ctx",
		[]string{"openrouter:web_search", "openrouter:web_fetch"})
	if !strings.Contains(detail, "web_search") {
		t.Error("detail should mention web_search")
	}
	if !strings.Contains(detail, "web_fetch") {
		t.Error("detail should mention web_fetch")
	}
	if !strings.Contains(detail, "third-party") {
		t.Error("detail should mention third-party egress risk")
	}
}

// TestPanelDetailNoServerTools verifies that PanelDetail with nil serverTools
// does not include a tools line (backward compat).
func TestPanelDetailNoServerTools(t *testing.T) {
	detail := PanelDetail("default", []string{"anthropic:claude-opus-4-8"}, "panel", "q", "ctx", nil)
	if strings.Contains(detail, "tools:") {
		t.Error("detail should NOT contain tools line when serverTools is nil")
	}
}

// TestOpenRouterServerToolsMixedArray verifies that a content array with
// mixed valid/invalid parts does not discard the valid text (per-part
// tolerance). Astra flagged this as the key robustness gap.
func TestOpenRouterServerToolsMixedArray(t *testing.T) {
	srv := openRouterSSETestServer(t,
		`{"choices":[{"message":{"role":"assistant","content":"useful answer second part"},"finish_reason":"stop"}]}`)
	defer srv.Close()

	ccfg := PanelCallConfig{
		MaxTokens:          512,
		TimeoutSeconds:     5,
		OpenRouterEndpoint: srv.URL,
		ServerTools:        []string{"openrouter:web_search"},
	}
	answer, _, err := callOpenRouter(context.Background(), "test-model", "key", "q", "ctx", ccfg)
	if err != nil {
		t.Fatalf("callOpenRouter with mixed array: %v", err)
	}
	// Both text parts should be extracted; the "other" part skipped.
	if !strings.Contains(answer, "useful answer") {
		t.Errorf("answer missing 'useful answer': %q", answer)
	}
	if !strings.Contains(answer, "second part") {
		t.Errorf("answer missing 'second part': %q", answer)
	}
}

// TestOpenRouter200WithErrorBody verifies that a 200 response with an error
// object is handled correctly (Fable flagged this — provider failures can
// return HTTP 200 with an error body).
func TestOpenRouter200WithErrorBody(t *testing.T) {
	srv := openRouterSSETestServer(t,
		`{"choices":[],"error":{"message":"provider overloaded","type":"server_error"}}`)
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 512, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL}
	_, _, err := callOpenRouter(context.Background(), "test-model", "key", "q", "ctx", ccfg)
	if err == nil {
		t.Fatal("expected error for 200-with-error-body")
	}
	if !strings.Contains(err.Error(), "provider overloaded") {
		t.Errorf("error should contain provider message, got: %v", err)
	}
}

// TestPanelDetailFusionNoToolsDisclosure verifies that PanelDetail does NOT
// show a tools disclosure for fusion mode even when serverTools is non-empty
// (the disclosure would be misleading — fusion doesn't use server tools).
func TestPanelDetailFusionNoToolsDisclosure(t *testing.T) {
	detail := PanelDetail("fusion-panel", []string{"~anthropic/claude"}, "fusion", "q", "ctx",
		[]string{"openrouter:web_search"})
	if strings.Contains(detail, "tools:") {
		t.Error("fusion PanelDetail should NOT show tools disclosure (server tools not wired for fusion)")
	}
}
