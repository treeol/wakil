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
	"time"

	"github.com/treeol/wakil/internal/config"
)

// ─── CallOracle / CallOracleURL ──────────────────────────────────────────────

// oracleTestServer returns an httptest.Server that responds with the given
// status code and response body.
func oracleTestServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			w.WriteHeader(status)
		}
		w.Write([]byte(body))
	}))
}

func TestCallOracleURL_Success(t *testing.T) {
	srv := oracleTestServer(t, http.StatusOK,
		`{"content":[{"type":"text","text":"the answer"}],"stop_reason":"end_turn","usage":{"input_tokens":42,"output_tokens":17}}`)
	defer srv.Close()

	cfg := config.Config{OracleModel: "test-model", OracleMaxTokens: 256, OracleTimeoutSeconds: 5}
	answer, usage, err := CallOracleURL(context.Background(), cfg, "test-key", "what is 2+2?", "context here", srv.URL)
	if err != nil {
		t.Fatalf("CallOracleURL: %v", err)
	}
	if answer != "the answer" {
		t.Errorf("answer = %q, want 'the answer'", answer)
	}
	if usage.InputTokens != 42 || usage.OutputTokens != 17 {
		t.Errorf("usage = %+v, want {42, 17}", usage)
	}
}

func TestCallOracleURL_HTTPError(t *testing.T) {
	srv := oracleTestServer(t, http.StatusInternalServerError,
		`{"error":{"type":"server_error","message":"internal failure"}}`)
	defer srv.Close()

	cfg := config.Config{OracleModel: "m", OracleMaxTokens: 256, OracleTimeoutSeconds: 5}
	_, _, err := CallOracleURL(context.Background(), cfg, "key", "q", "", srv.URL)
	if err == nil {
		t.Fatal("expected error for HTTP 500")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Errorf("error should mention HTTP 500, got: %v", err)
	}
	if !strings.Contains(err.Error(), "internal failure") {
		t.Errorf("error should contain API message, got: %v", err)
	}
}

func TestCallOracleURL_HTTPErrorNoBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	cfg := config.Config{OracleModel: "m", OracleMaxTokens: 256, OracleTimeoutSeconds: 5}
	_, _, err := CallOracleURL(context.Background(), cfg, "key", "q", "", srv.URL)
	if err == nil {
		t.Fatal("expected error for HTTP 502")
	}
	if !strings.Contains(err.Error(), "HTTP 502") {
		t.Errorf("error should mention HTTP 502, got: %v", err)
	}
}

func TestCallOracleURL_MaxTokens400(t *testing.T) {
	srv := oracleTestServer(t, http.StatusBadRequest,
		`{"error":{"type":"invalid_request","message":"max_tokens too large"}}`)
	defer srv.Close()

	cfg := config.Config{OracleModel: "m", OracleMaxTokens: 999999, OracleTimeoutSeconds: 5}
	_, _, err := CallOracleURL(context.Background(), cfg, "key", "q", "", srv.URL)
	if err == nil {
		t.Fatal("expected error for 400")
	}
	// The code adds a hint for 400 + max_tokens in the message.
	if !strings.Contains(err.Error(), "reduce oracle_max_tokens") {
		t.Errorf("error should mention reducing max_tokens, got: %v", err)
	}
}

func TestCallOracleURL_MaxTokensTruncation(t *testing.T) {
	srv := oracleTestServer(t, http.StatusOK,
		`{"content":[{"type":"text","text":"partial answer"}],"stop_reason":"max_tokens","usage":{"input_tokens":10,"output_tokens":5}}`)
	defer srv.Close()

	cfg := config.Config{OracleModel: "m", OracleMaxTokens: 256, OracleTimeoutSeconds: 5}
	_, usage, err := CallOracleURL(context.Background(), cfg, "key", "q", "", srv.URL)
	if err == nil {
		t.Fatal("expected max_tokens truncation error")
	}
	if !strings.Contains(err.Error(), "max_tokens") {
		t.Errorf("error should mention max_tokens, got: %v", err)
	}
	if !strings.Contains(err.Error(), "raise oracle_max_tokens") {
		t.Errorf("error should mention raising oracle_max_tokens, got: %v", err)
	}
	// Truncated calls are billed — usage must be returned even on error.
	if usage.InputTokens != 10 || usage.OutputTokens != 5 {
		t.Errorf("truncation usage = %+v, want {Input:10, Output:5}", usage)
	}
}

func TestCallOracleURL_MaxTokensNoTextBlocks(t *testing.T) {
	// Response with ONLY thinking blocks (no text blocks at all) + max_tokens.
	// This takes the "before emitting any text" branch, distinct from the
	// partial-text branch.
	srv := oracleTestServer(t, http.StatusOK,
		`{"content":[{"type":"thinking","text":"thinking..."}],"stop_reason":"max_tokens","usage":{"input_tokens":10,"output_tokens":5}}`)
	defer srv.Close()

	cfg := config.Config{OracleModel: "m", OracleMaxTokens: 256, OracleTimeoutSeconds: 5}
	_, _, err := CallOracleURL(context.Background(), cfg, "key", "q", "", srv.URL)
	if err == nil {
		t.Fatal("expected max_tokens no-text error")
	}
	if !strings.Contains(err.Error(), "before emitting any text") {
		t.Errorf("error should mention 'before emitting any text', got: %v", err)
	}
	if !strings.Contains(err.Error(), "non-thinking model") {
		t.Errorf("error should suggest non-thinking model for thinking-only blocks, got: %v", err)
	}
}

func TestCallOracleURL_MaxTokensWithThinking(t *testing.T) {
	srv := oracleTestServer(t, http.StatusOK,
		`{"content":[{"type":"thinking","text":"..."},{"type":"text","text":""}],"stop_reason":"max_tokens","usage":{"input_tokens":10,"output_tokens":5}}`)
	defer srv.Close()

	cfg := config.Config{OracleModel: "m", OracleMaxTokens: 256, OracleTimeoutSeconds: 5}
	_, _, err := CallOracleURL(context.Background(), cfg, "key", "q", "", srv.URL)
	if err == nil {
		t.Fatal("expected max_tokens truncation error")
	}
	if !strings.Contains(err.Error(), "non-thinking model") {
		t.Errorf("error should suggest non-thinking model, got: %v", err)
	}
}

func TestCallOracleURL_EmptyResponse(t *testing.T) {
	srv := oracleTestServer(t, http.StatusOK,
		`{"content":[],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":0}}`)
	defer srv.Close()

	cfg := config.Config{OracleModel: "m", OracleMaxTokens: 256, OracleTimeoutSeconds: 5}
	_, _, err := CallOracleURL(context.Background(), cfg, "key", "q", "", srv.URL)
	if err == nil {
		t.Fatal("expected empty-response error")
	}
	if !strings.Contains(err.Error(), "no text blocks") {
		t.Errorf("error should mention no text blocks, got: %v", err)
	}
}

func TestCallOracleURL_WhitespaceOnlyResponse(t *testing.T) {
	srv := oracleTestServer(t, http.StatusOK,
		`{"content":[{"type":"text","text":"   \n\t  "}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`)
	defer srv.Close()

	cfg := config.Config{OracleModel: "m", OracleMaxTokens: 256, OracleTimeoutSeconds: 5}
	_, _, err := CallOracleURL(context.Background(), cfg, "key", "q", "", srv.URL)
	if err == nil {
		t.Fatal("expected whitespace-only error")
	}
	if !strings.Contains(err.Error(), "empty or whitespace") {
		t.Errorf("error should mention whitespace, got: %v", err)
	}
}

func TestCallOracleURL_BadJSONResponse(t *testing.T) {
	srv := oracleTestServer(t, http.StatusOK, `not valid json`)
	defer srv.Close()

	cfg := config.Config{OracleModel: "m", OracleMaxTokens: 256, OracleTimeoutSeconds: 5}
	_, _, err := CallOracleURL(context.Background(), cfg, "key", "q", "", srv.URL)
	if err == nil {
		t.Fatal("expected parse error")
	}
	if !strings.Contains(err.Error(), "parse response") {
		t.Errorf("error should mention parse response, got: %v", err)
	}
}

func TestCallOracleURL_ContextTimeout(t *testing.T) {
	// Server that sleeps long enough for the 1s timeout to fire first.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer srv.Close()

	cfg := config.Config{OracleModel: "m", OracleMaxTokens: 256, OracleTimeoutSeconds: 1}
	_, _, err := CallOracleURL(context.Background(), cfg, "key", "q", "", srv.URL)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestCallOracle_EndpointOverride(t *testing.T) {
	srv := oracleTestServer(t, http.StatusOK,
		`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	defer srv.Close()

	// CallOracle uses cfg.OracleEndpoint when non-empty, instead of the
	// production oracleEndpoint.
	cfg := config.Config{
		OracleModel:          "m",
		OracleMaxTokens:      256,
		OracleTimeoutSeconds: 5,
		OracleEndpoint:       srv.URL,
	}
	answer, _, err := CallOracle(context.Background(), cfg, "key", "q", "")
	if err != nil {
		t.Fatalf("CallOracle: %v", err)
	}
	if answer != "ok" {
		t.Errorf("answer = %q, want 'ok'", answer)
	}
}

func TestCallOracle_ZeroTimeoutDoesNotPanic(t *testing.T) {
	// When OracleTimeoutSeconds is 0, the code applies a 300s default.
	// This test verifies the function doesn't panic with a zero timeout config
	// against a fast-responding server. It does NOT verify the 300s default
	// itself (would take too long in a test).
	srv := oracleTestServer(t, http.StatusOK,
		`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	defer srv.Close()

	cfg := config.Config{OracleModel: "m", OracleMaxTokens: 256} // TimeoutSeconds=0
	answer, _, err := CallOracleURL(context.Background(), cfg, "key", "q", "", srv.URL)
	if err != nil {
		t.Fatalf("unexpected error with zero timeout: %v", err)
	}
	if answer != "ok" {
		t.Errorf("answer = %q", answer)
	}
}

// ─── callMember (provider dispatch) ──────────────────────────────────────────

func TestCallMember_UnknownProvider(t *testing.T) {
	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5}
	_, _, err := callMember(context.Background(), "unknown", "model", "q", "b", ccfg, map[string]string{})
	if err == nil {
		t.Fatal("expected unknown provider error")
	}
	if !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("error should mention unknown provider, got: %v", err)
	}
}

func TestCallMember_AnthropicProvider(t *testing.T) {
	srv := oracleTestServer(t, http.StatusOK,
		`{"content":[{"type":"text","text":"anthropic answer"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`)
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, AnthropicEndpoint: srv.URL}
	answer, _, err := callMember(context.Background(), "anthropic", "test-model", "q", "b", ccfg,
		map[string]string{"anthropic": "key"})
	if err != nil {
		t.Fatalf("callMember anthropic: %v", err)
	}
	if answer != "anthropic answer" {
		t.Errorf("answer = %q", answer)
	}
}

func TestCallMember_OpenRouterProvider(t *testing.T) {
	srv := oracleTestServer(t, http.StatusOK,
		`{"choices":[{"message":{"role":"assistant","content":"or answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL}
	answer, _, err := callMember(context.Background(), "openrouter", "model", "q", "b", ccfg,
		map[string]string{"openrouter": "key"})
	if err != nil {
		t.Fatalf("callMember openrouter: %v", err)
	}
	if answer != "or answer" {
		t.Errorf("answer = %q", answer)
	}
}

// ─── callOpenRouter error paths ─────────────────────────────────────────────

func TestCallOpenRouter_HTTPError(t *testing.T) {
	srv := oracleTestServer(t, http.StatusBadGateway,
		`{"error":{"type":"server_error","message":"upstream down"}}`)
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL}
	_, _, err := callOpenRouter(context.Background(), "model", "key", "q", "b", ccfg)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "upstream down") {
		t.Errorf("error should contain API message, got: %v", err)
	}
}

func TestCallOpenRouter_NoChoices(t *testing.T) {
	srv := oracleTestServer(t, http.StatusOK, `{"choices":[]}`)
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL}
	_, _, err := callOpenRouter(context.Background(), "model", "key", "q", "b", ccfg)
	if err == nil {
		t.Fatal("expected no-choices error")
	}
	if !strings.Contains(err.Error(), "no choices") {
		t.Errorf("error should mention no choices, got: %v", err)
	}
}

func TestCallOpenRouter_EmptyContent(t *testing.T) {
	srv := oracleTestServer(t, http.StatusOK,
		`{"choices":[{"message":{"role":"assistant","content":"  "},"finish_reason":"stop"}]}`)
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL}
	_, _, err := callOpenRouter(context.Background(), "model", "key", "q", "b", ccfg)
	if err == nil {
		t.Fatal("expected empty-content error")
	}
	if !strings.Contains(err.Error(), "empty response") {
		t.Errorf("error should mention empty response, got: %v", err)
	}
}

func TestCallOpenRouter_FinishReasonLength(t *testing.T) {
	srv := oracleTestServer(t, http.StatusOK,
		`{"choices":[{"message":{"role":"assistant","content":"partial"},"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL}
	_, _, err := callOpenRouter(context.Background(), "model", "key", "q", "b", ccfg)
	if err == nil {
		t.Fatal("expected length-truncation error")
	}
	if !strings.Contains(err.Error(), "max_tokens") {
		t.Errorf("error should mention max_tokens, got: %v", err)
	}
}

// ─── callFusion error paths ──────────────────────────────────────────────────

func TestCallFusion_HTTPError(t *testing.T) {
	srv := oracleTestServer(t, http.StatusInternalServerError,
		`{"error":{"type":"server_error","message":"fusion failed"}}`)
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL}
	_, _, err := callFusion(context.Background(), []string{"model-a"}, "key", "q", "b", ccfg)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "fusion failed") {
		t.Errorf("error should contain fusion failed, got: %v", err)
	}
}

func TestCallFusion_NoChoices(t *testing.T) {
	srv := oracleTestServer(t, http.StatusOK, `{"choices":[]}`)
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL}
	_, _, err := callFusion(context.Background(), []string{"model-a"}, "key", "q", "b", ccfg)
	if err == nil {
		t.Fatal("expected no-choices error")
	}
	if !strings.Contains(err.Error(), "no choices") {
		t.Errorf("error should mention no choices, got: %v", err)
	}
}

func TestCallFusion_EmptyResponse(t *testing.T) {
	srv := oracleTestServer(t, http.StatusOK,
		`{"choices":[{"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`)
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL}
	_, _, err := callFusion(context.Background(), []string{"model-a"}, "key", "q", "b", ccfg)
	if err == nil {
		t.Fatal("expected empty-response error")
	}
	if !strings.Contains(err.Error(), "empty response") {
		t.Errorf("error should mention empty response, got: %v", err)
	}
}

// ─── RunPanel error surfaces ─────────────────────────────────────────────────

// TestRunPanelFallbackAllFail verifies that when ALL members fail in fallback
// mode, the results slice contains all failures (none succeed).
func TestRunPanelFallbackAllFail(t *testing.T) {
	srv := oracleTestServer(t, http.StatusInternalServerError,
		`{"error":{"type":"server_error","message":"down"}}`)
	defer srv.Close()

	models := []string{"anthropic:model-a", "anthropic:model-b", "anthropic:model-c"}
	apiKeys := map[string]string{"anthropic": "test-key"}
	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, AnthropicEndpoint: srv.URL + "/v1/messages"}

	results := RunPanel(context.Background(), models, "fallback", "question?", "briefing", ccfg, apiKeys)
	if len(results) != 3 {
		t.Fatalf("fallback all-fail: want 3 results (all failures), got %d", len(results))
	}
	for i, r := range results {
		if r.Err == nil {
			t.Errorf("result %d: expected error, got nil", i)
		}
		if r.Answer != "" {
			t.Errorf("result %d: answer should be empty on error, got %q", i, r.Answer)
		}
	}
}

// TestRunPanelEmptyModels verifies that an empty models list returns an empty
// results slice (no panic).
func TestRunPanelEmptyModels(t *testing.T) {
	results := RunPanel(context.Background(), nil, "panel", "q", "b",
		PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5}, map[string]string{})
	if len(results) != 0 {
		t.Errorf("empty models: want 0 results, got %d", len(results))
	}
}

// TestRunPanelFallbackEmptyModels verifies fallback mode with no models doesn't panic.
func TestRunPanelFallbackEmptyModels(t *testing.T) {
	results := RunPanel(context.Background(), nil, "fallback", "q", "b",
		PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5}, map[string]string{})
	if len(results) != 0 {
		t.Errorf("empty fallback: want 0 results, got %d", len(results))
	}
}

// ─── OracleDetail ────────────────────────────────────────────────────────────

func TestOracleDetail_Truncation(t *testing.T) {
	longQuestion := strings.Repeat("x", 3000)
	got := OracleDetail("model", longQuestion, "")
	if len(got) > 2100 {
		t.Errorf("truncated detail should be <= ~2100 chars, got %d", len(got))
	}
	if !strings.Contains(got, "(+") {
		t.Errorf("truncated detail should contain excess indicator, got: ...%s", got[len(got)-50:])
	}
}

func TestOracleDetail_NoTruncation(t *testing.T) {
	got := OracleDetail("model", "short question", "short context")
	if strings.Contains(got, "(+") {
		t.Errorf("non-truncated detail should not contain excess indicator")
	}
	if !strings.Contains(got, "model:") {
		t.Errorf("detail should contain model label, got: %s", got)
	}
	if !strings.Contains(got, "short question") {
		t.Errorf("detail should contain question")
	}
	if !strings.Contains(got, "short context") {
		t.Errorf("detail should contain context")
	}
}

func TestOracleDetail_NoContext(t *testing.T) {
	got := OracleDetail("model", "question", "")
	if strings.Contains(got, "context:") {
		t.Errorf("detail should not contain context label when oracleCtx is empty")
	}
	if !strings.Contains(got, "question") {
		t.Errorf("detail should contain question")
	}
}

// ─── FormatPanelResult edge cases ────────────────────────────────────────────

func TestFormatPanelResult_Empty(t *testing.T) {
	got := FormatPanelResult(nil)
	if !strings.Contains(got, "no members") {
		t.Errorf("empty results should mention no members, got: %s", got)
	}
}

func TestFormatPanelResult_MultiWithErrors(t *testing.T) {
	results := []PanelMemberResult{
		{Model: "model-a", Answer: "success answer"},
		{Model: "model-b", Err: fmt.Errorf("connection refused")},
	}
	got := FormatPanelResult(results)
	if !strings.Contains(got, "model-a") {
		t.Errorf("should contain model-a label")
	}
	if !strings.Contains(got, "success answer") {
		t.Errorf("should contain model-a answer")
	}
	if !strings.Contains(got, "model-b") {
		t.Errorf("should contain model-b label")
	}
	if !strings.Contains(got, "connection refused") {
		t.Errorf("should contain model-b error")
	}
	if !strings.Contains(got, "[error:") {
		t.Errorf("should contain error marker")
	}
}

// ─── doJSONPost ───────────────────────────────────────────────────────────────

func TestDoJSONPost_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		if r.Header.Get("X-Custom") != "custom-value" {
			t.Errorf("X-Custom = %q", r.Header.Get("X-Custom"))
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	raw, status, err := doJSONPost(context.Background(), srv.URL,
		map[string]string{"X-Custom": "custom-value"}, []byte(`{"hello":"world"}`))
	if err != nil {
		t.Fatalf("doJSONPost: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want %d", status, http.StatusOK)
	}
	if string(raw) != `{"ok":true}` {
		t.Errorf("raw = %q", string(raw))
	}
}

func TestDoJSONPost_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":"unavailable"}`))
	}))
	defer srv.Close()

	raw, status, err := doJSONPost(context.Background(), srv.URL, nil, []byte(`{}`))
	if err != nil {
		t.Fatalf("doJSONPost should not error on HTTP 503, just return status")
	}
	if status != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", status, http.StatusServiceUnavailable)
	}
	if string(raw) != `{"error":"unavailable"}` {
		t.Errorf("raw = %q", string(raw))
	}
}

func TestDoJSONPost_CancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := doJSONPost(ctx, "http://localhost:1", nil, []byte(`{}`))
	if err == nil {
		t.Fatal("expected error with cancelled context")
	}
}

func TestDoJSONPost_InvalidURL(t *testing.T) {
	// A URL with an invalid port should cause the request to fail.
	_, _, err := doJSONPost(context.Background(), "http://[::1]:namedport", nil, []byte(`{}`))
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

// ─── Anthropic request shape verification ───────────────────────────────────

// TestCallOracleURL_RequestShape verifies the request body sent to the Anthropic
// API: model, max_tokens, system prompt, and user message containing question
// and context.
func TestCallOracleURL_RequestShape(t *testing.T) {
	var reqBody []byte
	var apiKeyHeader, anthropicVersion string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKeyHeader = r.Header.Get("x-api-key")
		anthropicVersion = r.Header.Get("anthropic-version")
		reqBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	cfg := config.Config{OracleModel: "test-model", OracleMaxTokens: 512, OracleTimeoutSeconds: 5}
	_, _, err := CallOracleURL(context.Background(), cfg, "my-api-key", "the question?", "the context", srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if apiKeyHeader != "my-api-key" {
		t.Errorf("x-api-key = %q, want 'my-api-key'", apiKeyHeader)
	}
	if anthropicVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want '2023-06-01'", anthropicVersion)
	}

	var req oracleReq
	if e := json.Unmarshal(reqBody, &req); e != nil {
		t.Fatalf("request body not valid JSON: %v", e)
	}
	if req.Model != "test-model" {
		t.Errorf("model = %q, want 'test-model'", req.Model)
	}
	if req.MaxTokens != 512 {
		t.Errorf("max_tokens = %d, want 512", req.MaxTokens)
	}
	if req.System == "" {
		t.Error("system prompt should not be empty")
	}
	if len(req.Messages) != 1 {
		t.Fatalf("messages count = %d, want 1", len(req.Messages))
	}
	if req.Messages[0].Role != "user" {
		t.Errorf("messages[0].role = %q, want 'user'", req.Messages[0].Role)
	}
	content := req.Messages[0].Content
	if !strings.Contains(content, "the question?") {
		t.Errorf("user message missing question: %q", content)
	}
	if !strings.Contains(content, "the context") {
		t.Errorf("user message missing context: %q", content)
	}
	if !strings.Contains(content, "Context:") {
		t.Errorf("user message should contain 'Context:' separator when oracleCtx is non-empty: %q", content)
	}
}

// TestCallOracleURL_NoContext verifies the user message is just the question
// when oracleCtx is empty.
func TestCallOracleURL_NoContext(t *testing.T) {
	var reqBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	cfg := config.Config{OracleModel: "m", OracleMaxTokens: 256, OracleTimeoutSeconds: 5}
	_, _, err := CallOracleURL(context.Background(), cfg, "key", "just a question", "", srv.URL)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var req oracleReq
	if e := json.Unmarshal(reqBody, &req); e != nil {
		t.Fatalf("request body not valid JSON: %v", e)
	}
	content := req.Messages[0].Content
	if content != "just a question" {
		t.Errorf("user message should be just the question, got: %q", content)
	}
	if strings.Contains(content, "Context:") {
		t.Error("user message should NOT contain 'Context:' separator when oracleCtx is empty")
	}
}
