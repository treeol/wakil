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
	"time"

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

// TestLogFooterShowsReasoningAnswerSplit verifies that the footer line
// shows "reasoning N / answer M" when the provider reports reasoning tokens
// separately (OpenRouter), and omits the split when it does not (Anthropic).
func TestLogFooterShowsReasoningAnswerSplit(t *testing.T) {
	// OpenRouter-style: ReasoningTokens > 0, AnswerTokens = Output - Reasoning.
	r1 := PanelMemberResult{
		Model:  "openrouter:test-model",
		Answer: "ok",
		Usage:  OracleUsage{InputTokens: 100, OutputTokens: 50, ReasoningTokens: 30, FinishReason: "stop"},
		Log: &ReviewLog{
			Model:           "openrouter:test-model",
			TokensSent:      100,
			Parts:           1,
			ContextTokens:   128000,
			ReasoningTokens: 30,
			AnswerTokens:    20,
			FinishReasons:   []string{"stop"},
		},
	}
	footer := logFooter(r1)
	if !strings.Contains(footer, "reasoning 30 / answer 20") {
		t.Errorf("OpenRouter footer should show reasoning/answer split, got: %s", footer)
	}

	// Anthropic-style: ReasoningTokens = 0 (included in OutputTokens).
	r2 := PanelMemberResult{
		Model:  "anthropic:test-model",
		Answer: "ok",
		Usage:  OracleUsage{InputTokens: 100, OutputTokens: 50, FinishReason: "end_turn"},
		Log: &ReviewLog{
			Model:           "anthropic:test-model",
			TokensSent:      100,
			Parts:           1,
			ContextTokens:   128000,
			ReasoningTokens: 0,
			AnswerTokens:    0,
			FinishReasons:   []string{"end_turn"},
		},
	}
	footer2 := logFooter(r2)
	if strings.Contains(footer2, "reasoning") {
		t.Errorf("Anthropic footer should NOT show reasoning/answer split, got: %s", footer2)
	}
}

// TestClampReviewerReasoning verifies that clampReviewerReasoning caps
// max_tokens at maxReviewerReasoningTokens (16384) while leaving lower
// values and nil/zero specs untouched.
func TestClampReviewerReasoning(t *testing.T) {
	// nil → nil
	if got := clampReviewerReasoning(nil); got != nil {
		t.Errorf("clamp(nil) = %v, want nil", got)
	}
	// zero spec → unchanged
	zero := &config.ReasoningSpec{}
	if got := clampReviewerReasoning(zero); got != zero {
		t.Errorf("clamp(zero) = %v, want %v", got, zero)
	}
	// under ceiling → unchanged
	small := &config.ReasoningSpec{Effort: "low", MaxTokens: 4096}
	if got := clampReviewerReasoning(small); got != small || got.MaxTokens != 4096 {
		t.Errorf("clamp(4096) = %v, want unchanged", got)
	}
	// exactly at ceiling → unchanged
	atCeiling := &config.ReasoningSpec{Effort: "max", MaxTokens: maxReviewerReasoningTokens}
	if got := clampReviewerReasoning(atCeiling); got != atCeiling || got.MaxTokens != maxReviewerReasoningTokens {
		t.Errorf("clamp(%d) = %v, want unchanged", maxReviewerReasoningTokens, got)
	}
	// above ceiling → clamped
	over := &config.ReasoningSpec{Effort: "max", MaxTokens: 32768}
	got := clampReviewerReasoning(over)
	if got == over {
		t.Fatal("clamp should return a new spec, not mutate the original")
	}
	if over.MaxTokens != 32768 {
		t.Errorf("original spec was mutated: MaxTokens = %d, want 32768", over.MaxTokens)
	}
	if got.MaxTokens != maxReviewerReasoningTokens {
		t.Errorf("clamp(32768).MaxTokens = %d, want %d", got.MaxTokens, maxReviewerReasoningTokens)
	}
	if got.Effort != "max" {
		t.Errorf("clamp should preserve Effort, got %q", got.Effort)
	}
	// effort-only (no MaxTokens) → unchanged
	effortOnly := &config.ReasoningSpec{Effort: "max"}
	if got := clampReviewerReasoning(effortOnly); got != effortOnly {
		t.Errorf("clamp(effort-only) = %v, want %v", got, effortOnly)
	}
}

// TestRunMemberReviewerReasoningCeiling verifies that a per-model reasoning
// spec with max_tokens above the 16384 ceiling is clamped in the request
// sent to the provider. The Anthropic thinking budget_tokens must not exceed
// maxReviewerReasoningTokens.
func TestRunMemberReviewerReasoningCeiling(t *testing.T) {
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

	// Per-model reasoning with max_tokens=32768, above the 16384 ceiling.
	lim := map[string]ModelLimits{
		"anthropic:big-model": {
			ContextTokens: 200000,
			Reasoning:     &config.ReasoningSpec{Effort: "max", MaxTokens: 32768},
		},
	}
	ccfg := PanelCallConfig{
		MaxTokens:         4096,
		TimeoutSeconds:    5,
		AnthropicEndpoint: srv.URL,
		ModelLimits:       lim,
		MinAnswerTokens:   4096,
	}

	res := runMember(context.Background(), "anthropic:big-model", "anthropic", "big-model",
		"q", "briefing", "", ccfg, map[string]string{"anthropic": "key"}, nil)
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}

	var req oracleReq
	if err := json.Unmarshal(reqBody, &req); err != nil {
		t.Fatalf("request body not valid JSON: %v", err)
	}
	if req.Thinking == nil {
		t.Fatal("thinking parameter not sent")
	}
	if req.Thinking.BudgetTokens > maxReviewerReasoningTokens {
		t.Errorf("thinking.budget_tokens = %d, must be <= %d (reviewer ceiling)", req.Thinking.BudgetTokens, maxReviewerReasoningTokens)
	}
	// max_tokens must be additive: clamped budget + answer (4096).
	expectedMax := req.Thinking.BudgetTokens + 4096
	if req.MaxTokens != expectedMax {
		t.Errorf("max_tokens = %d, want %d (clamped budget %d + answer 4096)", req.MaxTokens, expectedMax, req.Thinking.BudgetTokens)
	}
}

// TestSSEMultiLineDataEvent verifies that the SSE parser correctly assembles
// multi-line data: events per the SSE spec. A single event whose data spans
// two data: lines must be joined with \n before dispatching to the parser.
// The Anthropic SSE stream sometimes sends JSON payloads that span multiple
// data: lines (e.g. large usage objects).
func TestSSEMultiLineDataEvent(t *testing.T) {
	// We test streamRequest directly with a custom server that sends a
	// two-line data: event. The parser must join them with \n and parse
	// the resulting JSON.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// A two-line data: event. The JSON is split across two lines.
		// The parser joins them with \n and must parse the complete JSON.
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":\n")
		fmt.Fprint(w, "data: {\"usage\":{\"input_tokens\":42,\"output_tokens\":0}}}\n\n")
		// content + stop to make it a complete stream
		fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n")
		fmt.Fprint(w, "data: {\"type\":\"message_stop\"}\n\n")
	}))
	defer srv.Close()

	content, usage, status, err := streamRequest(
		context.Background(),
		srv.URL,
		map[string]string{"x-api-key": "key", "anthropic-version": "2023-06-01"},
		[]byte(`{"model":"m","max_tokens":256,"messages":[{"role":"user","content":"q"}],"stream":true}`),
		5*time.Second,
		10*time.Second,
		parseAnthropicSSE,
	)
	if err != nil {
		t.Fatalf("streamRequest error: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want %d", status, http.StatusOK)
	}
	if content != "ok" {
		t.Errorf("content = %q, want %q", content, "ok")
	}
	if usage == nil {
		t.Fatal("usage is nil")
	}
	if usage.InputTokens != 42 {
		t.Errorf("InputTokens = %d, want 42 (from multi-line data: event)", usage.InputTokens)
	}
}

// TestStreamRequestIdleTimeout verifies that a server that accepts the
// connection, sends SSE headers, and then stalls forever is aborted by the
// streamIdleReader's idle timeout, surfacing a *streamIdleErr (retryable via
// ErrBackendStream) rather than hanging until the total cap.
func TestStreamRequestIdleTimeout(t *testing.T) {
	stalled := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Send SSE headers, then never write a body: stall forever.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-stalled
	}))
	defer srv.Close()
	defer close(stalled)

	start := time.Now()
	_, _, _, err := streamRequest(
		context.Background(),
		srv.URL,
		map[string]string{"x-api-key": "key"},
		[]byte(`{"model":"m","max_tokens":256,"messages":[{"role":"user","content":"q"}],"stream":true}`),
		100*time.Millisecond,
		5*time.Second,
		parseAnthropicSSE,
	)
	if err == nil {
		t.Fatal("expected an error from the idle timeout, got nil")
	}
	var idleErr *streamIdleErr
	if !errors.As(err, &idleErr) {
		t.Fatalf("expected *streamIdleErr in the chain, got %T: %v", err, err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("idle timeout fired late: %v elapsed (want ~100ms)", elapsed)
	}
}

// TestStreamIdleReaderDoubleClose verifies that a second Close on an
// already-closed streamIdleReader is a no-op (the already-closed branch).
func TestStreamIdleReaderDoubleClose(t *testing.T) {
	rc := &closeOnlyReader{}
	r := newStreamIdleReader(rc, time.Hour)
	if err := r.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if rc.closes != 1 {
		t.Errorf("underlying Close calls = %d, want 1", rc.closes)
	}
	// Second Close hits the already-closed branch: no-op, no panic.
	if err := r.Close(); err != nil {
		t.Errorf("second Close should return nil, got %v", err)
	}
	if rc.closes != 1 {
		t.Errorf("underlying Close calls after double Close = %d, want 1", rc.closes)
	}
}

type closeOnlyReader struct{ closes int }

func (c *closeOnlyReader) Read(p []byte) (int, error) { return 0, io.EOF }
func (c *closeOnlyReader) Close() error {
	c.closes++
	return nil
}

// TestStreamRequestJSONFallback covers the application/json content-type path
// (a server that returns JSON instead of SSE) and the non-data: line branch.
func TestStreamRequestJSONFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"choices":[{"delta":{"content":"ok"}}]}`)
	}))
	defer srv.Close()

	content, _, status, err := streamRequest(
		context.Background(),
		srv.URL,
		map[string]string{"x-api-key": "key"},
		[]byte(`{"model":"m","max_tokens":256,"stream":true}`),
		5*time.Second,
		10*time.Second,
		parseOpenRouterSSE,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status != http.StatusOK {
		t.Errorf("status = %d, want 200", status)
	}
	if content != "ok" {
		t.Errorf("content = %q, want 'ok'", content)
	}
}

// TestStreamRequestJSONFallbackReadError covers the application/json path where
// the body read fails (a server that sets application/json and then drops the
// connection).
func TestStreamRequestJSONFallbackReadError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Hijack and close immediately to cause a read error on the body.
		if hijacker, ok := w.(http.Hijacker); ok {
			conn, _, _ := hijacker.Hijack()
			conn.Close()
		}
	}))
	defer srv.Close()

	_, _, _, err := streamRequest(
		context.Background(),
		srv.URL,
		map[string]string{"x-api-key": "key"},
		[]byte(`{"model":"m","max_tokens":256,"stream":true}`),
		5*time.Second,
		10*time.Second,
		parseAnthropicSSE,
	)
	if err == nil {
		t.Fatal("expected an error for a dropped JSON response, got nil")
	}
}

// TestParseAnthropicSSEErrorEvent verifies the mid-stream error event is
// surfaced as an error finish reason with the error message.
func TestParseAnthropicSSEErrorEvent(t *testing.T) {
	_, u, done := parseAnthropicSSE([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`))
	if !done {
		t.Error("expected done=true for error event")
	}
	if u == nil {
		t.Fatal("usage is nil")
	}
	if u.FinishReason != "error" {
		t.Errorf("FinishReason = %q, want 'error'", u.FinishReason)
	}
	if u.ErrorMsg != "overloaded" {
		t.Errorf("ErrorMsg = %q, want 'overloaded'", u.ErrorMsg)
	}
}

// TestParseAnthropicSSEBadJSON verifies the JSON-unmarshal-failure path returns
// no content and not-done.
func TestParseAnthropicSSEBadJSON(t *testing.T) {
	content, u, done := parseAnthropicSSE([]byte(`not valid json`))
	if content != "" {
		t.Errorf("content = %q, want empty", content)
	}
	if u != nil {
		t.Errorf("usage = %v, want nil", u)
	}
	if done {
		t.Error("done = true, want false")
	}
}

// TestStreamRequestNoTerminalEvent verifies that a stream that sends content
// but no [DONE] or terminal event is rejected as an error (not accepted as a
// complete answer).
func TestStreamRequestNoTerminalEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Send a content line but NO [DONE] — the stream ends cleanly (EOF)
		// without a terminal event.
		fmt.Fprintf(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n")
	}))
	defer srv.Close()

	_, _, _, err := streamRequest(
		context.Background(),
		srv.URL,
		map[string]string{"x-api-key": "key"},
		[]byte(`{"model":"m","max_tokens":256,"messages":[{"role":"user","content":"q"}],"stream":true}`),
		5*time.Second,
		10*time.Second,
		parseAnthropicSSE,
	)
	if err == nil {
		t.Fatal("expected an error for stream without terminal event, got nil")
	}
	if !strings.Contains(err.Error(), "without a terminal event") {
		t.Errorf("error should mention 'without a terminal event', got: %v", err)
	}
}

// TestStreamRequestContextCancelled covers the streamRequest path where the
// context is cancelled (not idle timeout) — the scanner.Err() + tctx.Err()
// branch at line 247-249.
func TestStreamRequestContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Stall forever — the context will cancel before the idle timeout.
		<-time.After(10 * time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, _, _, err := streamRequest(
		ctx,
		srv.URL,
		map[string]string{"x-api-key": "key"},
		[]byte(`{"model":"m","max_tokens":256,"stream":true}`),
		5*time.Second,
		2*time.Second,
		parseAnthropicSSE,
	)
	if err == nil {
		t.Fatal("expected a context-cancelled error, got nil")
	}
}

// TestStreamIdleErrErrorUnwrap verifies the streamIdleErr error text and its
// unwrap to ErrBackendStream (the retryable sentinel).
func TestStreamIdleErrErrorUnwrap(t *testing.T) {
	e := &streamIdleErr{d: 5 * time.Second}
	if msg := e.Error(); !strings.Contains(msg, "counsel stream idle") {
		t.Errorf("Error() = %q, want it to contain %q", msg, "counsel stream idle")
	}
	if got := e.Unwrap(); !errors.Is(got, ErrBackendStream) {
		t.Errorf("Unwrap() = %v, want ErrBackendStream", got)
	}
	if !errors.Is(e, ErrBackendStream) {
		t.Errorf("errors.Is(streamIdleErr, ErrBackendStream) = false, want true")
	}
}
