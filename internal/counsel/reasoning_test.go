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

// TestReasoningParameter verifies that when a reasoning spec is set, the
// OpenRouter reasoning parameter is sent with budget_tokens and max_tokens is
// additive (budget + answer). The answer budget must never be reduced for
// reasoning. (The direct Anthropic "thinking" parameter is gone with the
// direct path; the OpenRouter reasoning object is the surviving shape.)
func TestReasoningParameter(t *testing.T) {
	var reqBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "ok"}, "finish_reason": "stop"}}}))
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 20}}))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	ccfg := PanelCallConfig{
		MaxTokens:      4096,
		TimeoutSeconds: 5,
		Transport:      transportForTest(srv),
		Reasoning:      &config.ReasoningSpec{Effort: "medium", MaxTokens: 8192},
	}
	_, _, err := callOpenRouter(context.Background(), "test-model", "key", "q", "briefing", "", ccfg)
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
	if req.Reasoning.MaxTokens != 8192 {
		t.Errorf("reasoning.max_tokens = %d, want 8192", req.Reasoning.MaxTokens)
	}
	// max_tokens must be additive: budget + answer (4096).
	expectedMax := req.Reasoning.MaxTokens + 4096
	if req.MaxTokens != expectedMax {
		t.Errorf("max_tokens = %d, want %d (budget %d + answer 4096)", req.MaxTokens, expectedMax, req.Reasoning.MaxTokens)
	}
}

// TestNoReasoningWhenUnset verifies that when no reasoning spec is set, the
// reasoning parameter is omitted and max_tokens equals the answer budget.
func TestNoReasoningWhenUnset(t *testing.T) {
	var reqBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "ok"}, "finish_reason": "stop"}}}))
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 20}}))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 512, TimeoutSeconds: 5, Transport: transportForTest(srv)}
	_, _, err := callOpenRouter(context.Background(), "test-model", "key", "q", "briefing", "", ccfg)
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
	_, usage, err := callOpenRouter(context.Background(), "test-model", "key", "q", "briefing", "", ccfg)
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
	// When only Effort is set (MaxTokens == 0), reasoning.max_tokens must
	// not be sent — only reasoning.effort. OpenRouter rejects both together.
	if req.Reasoning.MaxTokens != 0 {
		t.Errorf("reasoning.max_tokens = %d, want 0 (effort-only spec must not send max_tokens)", req.Reasoning.MaxTokens)
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

// TestCallOpenRouter_ReasoningMaxTokensOnly verifies that when a reasoning spec
// has both Effort and MaxTokens set (the default reviewer spec: Effort="medium",
// MaxTokens=8192), only reasoning.max_tokens is sent — not reasoning.effort.
// OpenRouter rejects requests that specify both with HTTP 400.
func TestCallOpenRouter_ReasoningMaxTokensOnly(t *testing.T) {
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
		MaxTokens:          4096,
		TimeoutSeconds:     5,
		OpenRouterEndpoint: srv.URL,
		Reasoning:          &config.ReasoningSpec{Effort: "medium", MaxTokens: 8192},
	}
	_, _, err := callOpenRouter(context.Background(), "test-model", "key", "q", "briefing", "", ccfg)
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
	// When MaxTokens > 0, only reasoning.max_tokens must be sent — not effort.
	if req.Reasoning.MaxTokens != 8192 {
		t.Errorf("reasoning.max_tokens = %d, want 8192", req.Reasoning.MaxTokens)
	}
	if req.Reasoning.Effort != "" {
		t.Errorf("reasoning.effort = %q, want empty (max_tokens spec must not send effort)", req.Reasoning.Effort)
	}
	// max_tokens must be additive: budget (8192) + answer (4096).
	if req.MaxTokens != 8192+4096 {
		t.Errorf("max_tokens = %d, want %d (budget 8192 + answer 4096)", req.MaxTokens, 8192+4096)
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
	_, _, err := callOpenRouter(context.Background(), "test-model", "key", "q", "briefing", "", ccfg)
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
	_, usage, err := callOpenRouter(context.Background(), "test-model", "key", "q", "briefing", "", ccfg)
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

	send := func(ctx context.Context, q, b, ap string, maxTok int) (string, OracleUsage, error) {
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

	send := func(ctx context.Context, q, b, ap string, maxTok int) (string, OracleUsage, error) {
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

// path is now ACCEPTED with coverage flagged (content present, citation wrong:
// obtained review, not VOID). Only empty/citation-only replies stay VOID.
func TestReviewCodeOnlyCitationWrongPathVoid(t *testing.T) {
	briefing := "── main.go (10 lines) ──\npackage main\n── utils.go (15 lines) ──\npackage main\n"

	// Cite "main.go" instead of "utils.go" (the last block).
	cited := "main.go"
	answer := "Looks good.\n" + evidencePrefix + " " + cited

	send := func(ctx context.Context, q, b, ap string, maxTok int) (string, OracleUsage, error) {
		return answer, OracleUsage{InputTokens: 10, OutputTokens: 20, FinishReason: "stop"}, nil
	}

	ctx := context.Background()
	out := dispatchReview(ctx, "test-model", 128000, 4096, 4096, "Review this code.", briefing, "", send)
	if out.Err != nil {
		t.Fatalf("a wrong citation with content must be accepted (coverage flagged), got: %v", out.Err)
	}
	if out.Log == nil || !out.Log.CoverageNotConfirmed {
		t.Errorf("wrong citation must set CoverageNotConfirmed: %+v", out.Log)
	}
	if strings.TrimSpace(out.Answer) == "" {
		t.Error("accepted review must keep its content")
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
		Usage:  OracleUsage{InputTokens: 100, OutputTokens: 50, FinishReason: "stop"},
		Log: &ReviewLog{
			Model:           "anthropic:test-model",
			TokensSent:      100,
			Parts:           1,
			ContextTokens:   128000,
			ReasoningTokens: 0,
			AnswerTokens:    0,
			FinishReasons:   []string{"stop"},
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
// sent to the provider. The OpenRouter reasoning.max_tokens must not exceed
// maxReviewerReasoningTokens.
func TestRunMemberReviewerReasoningCeiling(t *testing.T) {
	var reqBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "ok"}, "finish_reason": "stop"}}}))
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 20}}))
		w.Write([]byte("data: [DONE]\n\n"))
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
		MaxTokens:       4096,
		TimeoutSeconds:  5,
		Transport:       transportForTest(srv),
		ModelLimits:     lim,
		MinAnswerTokens: 4096,
	}

	res := runMember(context.Background(), "anthropic:big-model", "anthropic", "big-model",
		"q", "briefing", "", ccfg, map[string]string{"openrouter": "key"}, nil)
	if res.Err != nil {
		t.Fatalf("unexpected error: %v", res.Err)
	}

	var req orReq
	if err := json.Unmarshal(reqBody, &req); err != nil {
		t.Fatalf("request body not valid JSON: %v", err)
	}
	if req.Reasoning == nil {
		t.Fatal("reasoning parameter not sent")
	}
	if req.Reasoning.MaxTokens > maxReviewerReasoningTokens {
		t.Errorf("reasoning.max_tokens = %d, must be <= %d (reviewer ceiling)", req.Reasoning.MaxTokens, maxReviewerReasoningTokens)
	}
	// max_tokens must be additive: clamped budget + answer (4096).
	expectedMax := req.Reasoning.MaxTokens + 4096
	if req.MaxTokens != expectedMax {
		t.Errorf("max_tokens = %d, want %d (clamped budget %d + answer 4096)", req.MaxTokens, expectedMax, req.Reasoning.MaxTokens)
	}
}

// TestTransportExecIdleTimeout verifies that a stalled stream is aborted by
// the transport's total timeout. The transport's idle reader (120s) is the
// production stall detector; this test uses a 5s total cap so the stall is
// caught promptly instead of waiting out the idle window.
func TestTransportExecIdleTimeout(t *testing.T) {
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
	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, Transport: transportForTest(srv)}
	_, _, err := callOpenRouter(context.Background(), "test-model", "key", "q", "ctx", "", ccfg)
	if err == nil {
		t.Fatal("expected an error from the stalled stream, got nil")
	}
	if elapsed := time.Since(start); elapsed > 8*time.Second {
		t.Errorf("stall was not aborted promptly: %v elapsed (want ≤ total cap + slack)", elapsed)
	}
}

// TestTransportExecContextCancelled verifies the transport surfaces a
// context cancellation (not a retryable stall) when the caller cancels.
func TestTransportExecContextCancelled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Stall forever — the context will cancel first.
		<-time.After(10 * time.Second)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, Transport: transportForTest(srv)}
	_, _, err := callOpenRouter(ctx, "test-model", "key", "q", "ctx", "", ccfg)
	if err == nil {
		t.Fatal("expected a context-cancelled error, got nil")
	}
}

// TestTransportExecNoTerminalEvent verifies that a stream that sends content
// but no [DONE] is rejected as an error (not accepted as a complete answer).
func TestTransportExecNoTerminalEvent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Send a content line but NO [DONE] — the stream ends cleanly (EOF)
		// without a terminal event.
		fmt.Fprintf(w, "data: %s\n\n", mustJSON(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "ok"}}}}))
	}))
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, Transport: transportForTest(srv)}
	_, _, err := callOpenRouter(context.Background(), "test-model", "key", "q", "ctx", "", ccfg)
	if err == nil {
		t.Fatal("expected an error for stream without terminal event, got nil")
	}
}

// TestProviderHTTP400IsHardErrorNotVoid: a provider 400 (e.g. the OpenRouter
// "Only one of reasoning.effort and reasoning.max_tokens" refusal) is a hard,
// non-VOID error stated in the footer, and is not retried.
func TestProviderHTTP400IsHardErrorNotVoid(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"Only one of \"reasoning.effort\" and \"reasoning.max_tokens\" can be specified"}}`)
	}))
	defer srv.Close()
	ccfg := PanelCallConfig{MaxTokens: 4096, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL,
		ModelLimits: map[string]ModelLimits{"openrouter:m": {ContextTokens: 200000}}}
	res := runMember(context.Background(), "openrouter:m", "openrouter", "m", "q", "doc", "", ccfg, map[string]string{"openrouter": "k"}, nil)
	if res.Err == nil || res.Void {
		t.Fatalf("want hard non-VOID error, got err=%v void=%v", res.Err, res.Void)
	}
	var pe *ProviderHTTPError
	if !errors.As(res.Err, &pe) || pe.Status != 400 {
		t.Fatalf("want *ProviderHTTPError 400, got %T %v", res.Err, res.Err)
	}
	if calls != 1 {
		t.Errorf("400 must not be retried: %d calls", calls)
	}
	out := FormatPanelResult([]PanelMemberResult{res})
	if !strings.Contains(out, "PROVIDER ERROR (not a review outcome): HTTP 400") || strings.Contains(out, "VOID") {
		t.Errorf("footer must state the provider error, not VOID: %s", out)
	}
}

// TestProviderRetryOn429And5xx: a 429/5xx is retried exactly once after the
// backoff; success on the retry is a normal reply; two failures stay a hard
// error. The footer records the retry.
func TestProviderRetryOn429And5xx(t *testing.T) {
	old := retryBackoff
	retryBackoff = func() time.Duration { return time.Millisecond }
	defer func() { retryBackoff = old }()

	for _, status := range []int{429, 502} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if calls == 1 {
					w.WriteHeader(status)
					fmt.Fprint(w, `{"error":{"message":"busy"}}`)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"fine\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer srv.Close()
			ccfg := PanelCallConfig{MaxTokens: 4096, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL,
				ModelLimits: map[string]ModelLimits{"openrouter:m": {ContextTokens: 200000}}}
			res := runMember(context.Background(), "openrouter:m", "openrouter", "m", "q", "doc", "", ccfg, map[string]string{"openrouter": "k"}, nil)
			if res.Err != nil || res.Answer != "fine" || calls != 2 {
				t.Fatalf("err=%v answer=%q calls=%d", res.Err, res.Answer, calls)
			}
			if !res.Log.ProviderRetried {
				t.Error("retry not recorded in the log")
			}
		})
	}

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(503)
	}))
	defer srv.Close()
	ccfg := PanelCallConfig{MaxTokens: 4096, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL,
		ModelLimits: map[string]ModelLimits{"openrouter:m": {ContextTokens: 200000}}}
	res := runMember(context.Background(), "openrouter:m", "openrouter", "m", "q", "doc", "", ccfg, map[string]string{"openrouter": "k"}, nil)
	if res.Err == nil || res.Void || calls != 2 {
		t.Fatalf("two 503s: err=%v void=%v calls=%d (want hard error after exactly 2 calls)", res.Err, res.Void, calls)
	}
}

// TestBackoffRange: the retry pause is 5–15 s.
func TestBackoffRange(t *testing.T) {
	for i := 0; i < 200; i++ {
		if d := prodRetryBackoff(); d < 5*time.Second || d >= 15*time.Second {
			t.Fatalf("backoff %v outside [5s,15s)", d)
		}
	}
}

// TestRunMemberDefaultReviewerSpecOpenRouterBody drives the full reviewer
// path (runMember → clamp → callOpenRouter) with the built-in default
// reviewer spec {effort medium, max_tokens 8192} and asserts the wire body:
// reasoning has max_tokens 8192 and NO effort key (OpenRouter 400s on both).
func TestRunMemberDefaultReviewerSpecOpenRouterBody(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	ccfg := PanelCallConfig{MaxTokens: 4096, TimeoutSeconds: 5, Transport: transportForTest(srv),
		Reasoning:   &config.ReasoningSpec{Effort: "medium", MaxTokens: 8192},
		ModelLimits: map[string]ModelLimits{"openrouter:m": {ContextTokens: 200000}}}
	res := runMember(context.Background(), "openrouter:m", "openrouter", "m", "q", "doc", "", ccfg, map[string]string{"openrouter": "k"}, nil)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	r, ok := body["reasoning"].(map[string]any)
	if !ok {
		t.Fatalf("no reasoning object in body: %v", body)
	}
	if _, has := r["effort"]; has {
		t.Errorf("reasoning.effort must be absent: %v", r)
	}
	if r["max_tokens"] != float64(8192) {
		t.Errorf("reasoning.max_tokens = %v, want 8192", r["max_tokens"])
	}
}
