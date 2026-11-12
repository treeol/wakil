package counsel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// ─── callMember (provider dispatch) ──────────────────────────────────────────

// httpErrorTestServer returns an httptest.Server that always answers with the
// given status and an OpenRouter-style error body.
func httpErrorTestServer(t *testing.T, status int, msg string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"error":{"message":%q}}`, msg)
	}))
}

// lengthTestServer returns an httptest.Server that answers with text cut off
// at finish_reason=length.
func lengthTestServer(t *testing.T, content string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": content}, "finish_reason": "length"}}}))
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5}}))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
}

// openRouterSSETestServer returns an httptest.Server that speaks the
// OpenRouter SSE streaming shape (chat.completion.chunk data lines ending in
// [DONE]). It is shared by every counsel test that needs a fake model host.
func openRouterSSETestServer(t *testing.T, content string, promptTokens, completionTokens int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if content != "" {
			fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": content}, "finish_reason": nil}}}))
		}
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}}))
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": promptTokens, "completion_tokens": completionTokens}}))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// transportForTest builds the single transport client for a test server:
// the policy is off (the defaults), matching the pre-policy tests.
//
// It takes the *httptest.Server and sets the chat endpoint on the client via
// the TransportEndpoint override, which orChat uses only when the injected
// client itself does not carry a non-empty BaseURL of its own. (Tests keep
// the full request URL so Exec's policy classification stays on the test
// host; production clients carry the OpenRouter origin instead.)
func transportForTest(srv *httptest.Server) *proxy.Client {
	return &proxy.Client{BaseURL: srv.URL, HTTP: srv.Client()}
}

// ─── callMember (provider dispatch) ──────────────────────────────────────────

func TestCallMember_UnknownProvider(t *testing.T) {
	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5}
	_, _, err := callMember(context.Background(), "unknown", "model", "q", "b", "", ccfg, map[string]string{})
	if err == nil {
		t.Fatal("expected unknown provider error")
	}
	if !strings.Contains(err.Error(), "unknown provider") {
		t.Errorf("error should mention unknown provider, got: %v", err)
	}
}

func TestCallMember_AnthropicProvider(t *testing.T) {
	// A configured "anthropic:<id>" member is mapped mechanically to the
	// OpenRouter slug "anthropic/<id>" and served through the OpenRouter key.
	srv := openRouterSSETestServer(t, "anthropic answer", 10, 5)
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, Transport: transportForTest(srv)}
	answer, _, err := callMember(context.Background(), "anthropic", "test-model", "q", "b", "", ccfg,
		map[string]string{"openrouter": "key"})
	if err != nil {
		t.Fatalf("callMember anthropic: %v", err)
	}
	if answer != "anthropic answer" {
		t.Errorf("answer = %q", answer)
	}
}

func TestCallMember_OpenRouterProvider(t *testing.T) {
	srv := openRouterSSETestServer(t, "or answer", 10, 5)
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, Transport: transportForTest(srv)}
	answer, _, err := callMember(context.Background(), "openrouter", "model", "q", "b", "", ccfg,
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
	srv := httpErrorTestServer(t, http.StatusBadGateway, "upstream down")
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, Transport: transportForTest(srv)}
	_, _, err := callOpenRouter(context.Background(), "model", "key", "q", "b", "", ccfg)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "upstream down") {
		t.Errorf("error should contain API message, got: %v", err)
	}
}

func TestCallOpenRouter_NoChoices(t *testing.T) {
	srv := openRouterSSETestServer(t, "", 10, 5)
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, Transport: transportForTest(srv)}
	_, _, err := callOpenRouter(context.Background(), "model", "key", "q", "b", "", ccfg)
	if err == nil {
		t.Fatal("expected no-choices error")
	}
	if !strings.Contains(err.Error(), "empty response") {
		t.Errorf("error should mention empty response, got: %v", err)
	}
}

func TestCallOpenRouter_EmptyContent(t *testing.T) {
	srv := openRouterSSETestServer(t, "  ", 10, 5)
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, Transport: transportForTest(srv)}
	_, _, err := callOpenRouter(context.Background(), "model", "key", "q", "b", "", ccfg)
	if err == nil {
		t.Fatal("expected empty-content error")
	}
	if !strings.Contains(err.Error(), "empty response") {
		t.Errorf("error should mention empty response, got: %v", err)
	}
}

func TestCallOpenRouter_FinishReasonLength(t *testing.T) {
	srv := lengthTestServer(t, "partial")
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, Transport: transportForTest(srv)}
	_, _, err := callOpenRouter(context.Background(), "model", "key", "q", "b", "", ccfg)
	if err == nil {
		t.Fatal("expected length-truncation error")
	}
	if !strings.Contains(err.Error(), "max_tokens") {
		t.Errorf("error should mention max_tokens, got: %v", err)
	}
}

// ─── callFusion error paths ──────────────────────────────────────────────────

func TestCallFusion_HTTPError(t *testing.T) {
	srv := httpErrorTestServer(t, http.StatusInternalServerError, "fusion failed")
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, Transport: transportForTest(srv)}
	_, _, err := callFusion(context.Background(), []string{"model-a"}, "key", "q", "b", ccfg)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "fusion failed") {
		t.Errorf("error should contain fusion failed, got: %v", err)
	}
}

func TestCallFusion_NoChoices(t *testing.T) {
	srv := openRouterSSETestServer(t, "", 10, 5)
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, Transport: transportForTest(srv)}
	_, _, err := callFusion(context.Background(), []string{"model-a"}, "key", "q", "b", ccfg)
	if err == nil {
		t.Fatal("expected empty-response error")
	}
	if !strings.Contains(err.Error(), "empty response") {
		t.Errorf("error should mention empty response, got: %v", err)
	}
}

func TestCallFusion_EmptyResponse(t *testing.T) {
	srv := openRouterSSETestServer(t, "", 10, 5)
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, Transport: transportForTest(srv)}
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
	srv := httpErrorTestServer(t, http.StatusInternalServerError, "down")
	defer srv.Close()

	models := []string{"anthropic:model-a", "anthropic:model-b", "anthropic:model-c"}
	apiKeys := map[string]string{"openrouter": "test-key"}
	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, Transport: transportForTest(srv)}

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
	got := FormatPanelResult([]PanelMemberResult{{PrefixedModel: "openrouter:m", Model: "m", Err: errors.New("boom")}})
	if !strings.Contains(got, "[mashūra error:") {
		t.Errorf("should contain error marker, got: %s", got)
	}
}

// TestChatRequestShape verifies the request body sent through the single
// transport: model, max_tokens, system prompt, and the skeleton user message.
func TestChatRequestShape(t *testing.T) {
	var reqBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "ok"}, "finish_reason": "stop"}}}))
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1}}))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, Transport: transportForTest(srv)}
	_, _, err := callOpenRouter(context.Background(), "model", "key", "the question?", "the context", "", ccfg)
	if err != nil {
		t.Fatalf("callOpenRouter: %v", err)
	}
	var req struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(reqBody, &req); err != nil {
		t.Fatalf("request body not valid JSON: %v", err)
	}
	if req.Model != "model" {
		t.Errorf("model = %q, want model", req.Model)
	}
	if req.MaxTokens != 256 {
		t.Errorf("max_tokens = %d, want 256", req.MaxTokens)
	}
	found := false
	for _, m := range req.Messages {
		if strings.Contains(m.Content, "the question?") {
			found = true
		}
	}
	if !found {
		t.Errorf("user message should contain the question, got %+v", req.Messages)
	}
}
