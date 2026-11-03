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

	"github.com/treeol/wakil/internal/config"
)

// errorsAsVoid reports whether err or its chain is a *VoidError.
func errorsAsVoid(err error) bool {
	var ve *VoidError
	return errors.As(err, &ve)
}

// TestCallOracleURL_ThinkingParameter verifies that when a reasoning spec is
// set, the Anthropic-native thinking parameter is sent with budget_tokens and
// max_tokens is additive (budget + answer). The answer budget must never be
// reduced for reasoning.
func TestCallOracleURL_ThinkingParameter(t *testing.T) {
	var reqBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n")
		fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
		fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":20}}\n\n")
		fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	cfg := config.Config{OracleModel: "test-model", OracleMaxTokens: 4096, OracleTimeoutSeconds: 5}
	spec := &config.ReasoningSpec{Effort: "medium"}
	_, _, err := callOracleWithReasoning(context.Background(), cfg, "key", "q", "ctx", srv.URL, spec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var req oracleReq
	if err := json.Unmarshal(reqBody, &req); err != nil {
		t.Fatalf("request body not valid JSON: %v", err)
	}
	if req.Thinking == nil {
		t.Fatal("thinking parameter not sent")
	}
	if req.Thinking.Type != "enabled" {
		t.Errorf("thinking.type = %q, want %q", req.Thinking.Type, "enabled")
	}
	if req.Thinking.BudgetTokens < 1024 {
		t.Errorf("thinking.budget_tokens = %d, must be >= 1024", req.Thinking.BudgetTokens)
	}
	// max_tokens must be additive: budget + answer (4096).
	expectedMax := req.Thinking.BudgetTokens + 4096
	if req.MaxTokens != expectedMax {
		t.Errorf("max_tokens = %d, want %d (budget %d + answer 4096)", req.MaxTokens, expectedMax, req.Thinking.BudgetTokens)
	}
}

// TestCallOracleURL_NoThinkingWhenUnset verifies that when no reasoning spec
// is set, the thinking parameter is omitted and max_tokens equals the answer
// budget — byte-identical to the original CallOracleURL behavior.
func TestCallOracleURL_NoThinkingWhenUnset(t *testing.T) {
	var reqBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":0}}}\n\n")
		fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
		fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":20}}\n\n")
		fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	cfg := config.Config{OracleModel: "test-model", OracleMaxTokens: 512, OracleTimeoutSeconds: 5}
	_, _, err := callOracleWithReasoning(context.Background(), cfg, "key", "q", "ctx", srv.URL, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var req oracleReq
	if err := json.Unmarshal(reqBody, &req); err != nil {
		t.Fatalf("request body not valid JSON: %v", err)
	}
	if req.Thinking != nil {
		t.Errorf("thinking parameter sent when spec is nil: %+v", req.Thinking)
	}
	if req.MaxTokens != 512 {
		t.Errorf("max_tokens = %d, want 512 (answer budget only)", req.MaxTokens)
	}
}

// TestCallOpenRouter_ReasoningParameter verifies that when a reasoning spec is
// set, the OpenRouter reasoning parameter is sent and max_tokens is additive.
func TestCallOpenRouter_ReasoningParameter(t *testing.T) {
	var reqBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20,\"completion_tokens_details\":{\"reasoning_tokens\":15}}}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ccfg := PanelCallConfig{
		MaxTokens:          4096,
		TimeoutSeconds:     5,
		OpenRouterEndpoint: srv.URL,
		Reasoning:          &config.ReasoningSpec{Effort: "medium"},
	}
	_, usage, err := callOpenRouter(context.Background(), "test-model", "key", "q", "briefing", ccfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var req orReq
	if err := json.Unmarshal(reqBody, &req); err != nil {
		t.Fatalf("request body not valid JSON: %v", err)
	}
	if req.Reasoning == nil {
		t.Fatal("reasoning parameter not sent")
	}
	if req.Reasoning.Effort != "medium" {
		t.Errorf("reasoning.effort = %q, want %q", req.Reasoning.Effort, "medium")
	}
	// max_tokens must be additive: budget + answer.
	budget := config.ReasoningBudgetFor(&config.ReasoningSpec{Effort: "medium"})
	expectedMax := budget + 4096
	if req.MaxTokens != expectedMax {
		t.Errorf("max_tokens = %d, want %d (budget %d + answer 4096)", req.MaxTokens, expectedMax, budget)
	}
	// Verify reasoning tokens are parsed from the response usage.
	if usage.ReasoningTokens != 15 {
		t.Errorf("usage.ReasoningTokens = %d, want 15", usage.ReasoningTokens)
	}
}

// TestCallOpenRouter_NoReasoningWhenUnset verifies that when no reasoning spec
// is set, the reasoning parameter is omitted and max_tokens equals the answer
// budget.
func TestCallOpenRouter_NoReasoningWhenUnset(t *testing.T) {
	var reqBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":20}}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ccfg := PanelCallConfig{
		MaxTokens:          512,
		TimeoutSeconds:     5,
		OpenRouterEndpoint: srv.URL,
	}
	_, _, err := callOpenRouter(context.Background(), "test-model", "key", "q", "briefing", ccfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var req orReq
	if err := json.Unmarshal(reqBody, &req); err != nil {
		t.Fatalf("request body not valid JSON: %v", err)
	}
	if req.Reasoning != nil {
		t.Errorf("reasoning parameter sent when spec is nil: %+v", req.Reasoning)
	}
	if req.MaxTokens != 512 {
		t.Errorf("max_tokens = %d, want 512 (answer budget only)", req.MaxTokens)
	}
}

// TestCallOpenRouter_ReasoningTokensParsed verifies that reasoning_tokens from
// the OpenRouter completion_tokens_details are surfaced in OracleUsage.
func TestCallOpenRouter_ReasoningTokensParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50,\"completion_tokens_details\":{\"reasoning_tokens\":30}}}\n\n")
		fmt.Fprintf(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	ccfg := PanelCallConfig{
		MaxTokens:          4096,
		TimeoutSeconds:     5,
		OpenRouterEndpoint: srv.URL,
	}
	_, usage, err := callOpenRouter(context.Background(), "test-model", "key", "q", "briefing", ccfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.ReasoningTokens != 30 {
		t.Errorf("ReasoningTokens = %d, want 30", usage.ReasoningTokens)
	}
	if usage.OutputTokens != 50 {
		t.Errorf("OutputTokens = %d, want 50", usage.OutputTokens)
	}
}

// TestReviewCodeOnlyCitationNotVoid verifies that a review of two .go files
// where the reviewer cites the correct file path is not VOID. This is the
// evidence-of-reading test for code-only inputs required by the spec.
func TestReviewCodeOnlyCitationNotVoid(t *testing.T) {
	briefing := "── main.go (10 lines) ──\npackage main\n\nfunc main() {}\n── utils.go (15 lines) ──\npackage main\n\nfunc helper() int {\n    return 42\n}\n"

	// The last block label is "utils.go (15 lines)". The reviewer must cite
	// "utils.go" (after trailingParenRe strips "(15 lines)").
	cited := "utils.go"
	answer := "Looks good.\n" + evidencePrefix + " " + cited

	send := func(ctx context.Context, q, b string, maxTok int) (string, OracleUsage, error) {
		return answer, OracleUsage{InputTokens: 10, OutputTokens: 20, FinishReason: "stop"}, nil
	}

	ctx := context.Background()
	out := dispatchReview(ctx, "test-model", 128000, 4096, 4096, "Review this code.", briefing, "", send)
	if out.Err != nil {
		t.Fatalf("expected no error, got: %v (answer=%q)", out.Err, out.Answer)
	}
	if strings.TrimSpace(out.Answer) == "" {
		t.Fatal("expected non-empty answer")
	}
}

// TestReviewCodeOnlyRangeLabelCitation verifies that a range-labelled source
// file block (e.g. "foo.go (lines 10–20) (15 lines)") is correctly matched when
// the reviewer cites the file path. trailingParenRe must strip ALL trailing
// parenthetical groups, not just one.
func TestReviewCodeOnlyRangeLabelCitation(t *testing.T) {
	briefing := "── main.go (lines 1–10) (10 lines) ──\npackage main\n── utils.go (lines 3–40) (120 lines) ──\npackage main\n"

	// The last block label is "utils.go (lines 3–40) (120 lines)". The
	// reviewer must cite "utils.go" — both parenthetical groups must be
	// stripped for the match.
	cited := "utils.go"
	answer := "Looks good.\n" + evidencePrefix + " " + cited

	send := func(ctx context.Context, q, b string, maxTok int) (string, OracleUsage, error) {
		return answer, OracleUsage{InputTokens: 10, OutputTokens: 20, FinishReason: "stop"}, nil
	}

	ctx := context.Background()
	out := dispatchReview(ctx, "test-model", 128000, 4096, 4096, "Review this code.", briefing, "", send)
	if out.Err != nil {
		t.Fatalf("expected no error for range-label citation, got: %v", out.Err)
	}
	if strings.TrimSpace(out.Answer) == "" {
		t.Fatal("expected non-empty answer")
	}
}

// path produces a VOID result — the evidence check catches it.
func TestReviewCodeOnlyCitationWrongPathVoid(t *testing.T) {
	briefing := "── main.go (10 lines) ──\npackage main\n── utils.go (15 lines) ──\npackage main\n"

	// Cite "main.go" instead of "utils.go" (the last block).
	cited := "main.go"
	answer := "Looks good.\n" + evidencePrefix + " " + cited

	send := func(ctx context.Context, q, b string, maxTok int) (string, OracleUsage, error) {
		return answer, OracleUsage{InputTokens: 10, OutputTokens: 20, FinishReason: "stop"}, nil
	}

	ctx := context.Background()
	out := dispatchReview(ctx, "test-model", 128000, 4096, 4096, "Review this code.", briefing, "", send)
	if out.Err == nil {
		t.Fatal("expected VOID error for wrong citation, got nil")
	}
	if !errorsAsVoid(out.Err) {
		t.Fatalf("expected *VoidError, got %T: %v", out.Err, out.Err)
	}
	if out.Answer != "" {
		t.Errorf("expected empty answer on VOID, got %q", out.Answer)
	}
}
