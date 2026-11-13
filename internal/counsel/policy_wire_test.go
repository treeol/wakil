package counsel

// Wire test: a Mashūra call under zdr on carries the provider ZDR fields,
// and a Mashūra call with both switches off sends byte-identical bytes to
// the pre-policy path. All other counsel tests run with both switches off.
// (Also covers the small review helpers the wire shape exercises:
// voidCauseFrom classifications, budget-state arithmetic, and the panel debug
// capture write paths.)

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// TestVoidCaptureWritesFile drives a VOID member with the debug variable set
// and asserts the panel-void-*.json diagnostics file lands.
func TestVoidCaptureWritesFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(panelDebugEnvVar, dir)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Length stop: a VOID (no citation, truncated).
		w.Write([]byte("data: " + mustJSON(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "cut o"}, "finish_reason": "length"}}}) + "\n\n"))
		w.Write([]byte("data: " + mustJSON(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5}}) + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 512, TimeoutSeconds: 5, Transport: transportForTest(srv)}
	results := RunPanel(context.Background(), []string{"anthropic:test"}, "panel", "q", "## Section\nbody\n", ccfg, map[string]string{"openrouter": "k"})
	if len(results) != 1 || !results[0].Void {
		t.Fatalf("want VOID, got %+v", results)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "panel-void-*.json"))
	if len(matches) == 0 {
		t.Fatal("no panel-void capture file written")
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"finish_reason"`) {
		t.Errorf("void capture missing finish_reason: %s", raw)
	}
}

// TestMashuraCallUnderZDROnCarriesFields runs one panel call with zdr on
// against a fake OpenRouter-classified host and asserts the provider fields
// arrive on the wire, merged with the request's other fields.
func TestMashuraCallUnderZDROnCarriesFields(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		gotBody = b
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// A diligent cited answer.
		w.Write([]byte("data: " + mustJSON(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "ok\nLAST SECTION READ: Section"}, "finish_reason": nil}}}) + "\n\n"))
		w.Write([]byte("data: " + mustJSON(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}}) + "\n\n"))
		w.Write([]byte("data: " + mustJSON(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5}}) + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := &proxy.Client{
		BaseURL: "https://openrouter.ai/api/v1/chat/completions",
		HTTP:    rewriteToServer(srv),
		ZDR:     "on",
	}
	ccfg := PanelCallConfig{MaxTokens: 512, TimeoutSeconds: 5, Transport: client}

	// Drive the OpenRouter member path with a citation-bearing reply.
	results := RunPanel(context.Background(), []string{"anthropic:test"}, "panel", "q", "## Section\nbody\n", ccfg, map[string]string{"openrouter": "k"})
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("panel error: %v", results[0].Err)
	}
	var top map[string]any
	if err := json.Unmarshal(gotBody, &top); err != nil {
		t.Fatalf("wire body not JSON: %v\n%s", err, gotBody)
	}
	prov, ok := top["provider"].(map[string]any)
	if !ok {
		t.Fatalf("provider object missing from wire body: %v", top)
	}
	if prov["zdr"] != true || prov["data_collection"] != "deny" {
		t.Errorf("provider = %v, want zdr:true and data_collection:deny", prov)
	}
}

// TestMashuraCallDefaultsOffByteEquality verifies that a Mashūra call with
// both switches off carries no provider fields (byte-identical to the
// pre-policy request for the same member).
// It is the Mashūra half of DO NOT ALTER 4.
func TestMashuraCallDefaultsOffByteEquality(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		gotBody = b
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: " + mustJSON(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "ok\nLAST SECTION READ: Section"}, "finish_reason": nil}}}) + "\n\n"))
		w.Write([]byte("data: " + mustJSON(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}}) + "\n\n"))
		w.Write([]byte("data: " + mustJSON(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5}}) + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := &proxy.Client{BaseURL: srv.URL, HTTP: srv.Client(), ZDR: "off", Region: "off"}
	ccfg := PanelCallConfig{MaxTokens: 512, TimeoutSeconds: 5, Transport: client}
	results := RunPanel(context.Background(), []string{"anthropic:test"}, "panel", "q", "## Section\nbody\n", ccfg, map[string]string{"openrouter": "k"})
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("panel error: %+v", results)
	}
	var top map[string]any
	if err := json.Unmarshal(gotBody, &top); err != nil {
		t.Fatalf("wire body not JSON: %v\n%s", err, gotBody)
	}
	if _, ok := top["provider"]; ok {
		t.Errorf("provider object present with switches off: %v", top)
	}
}

// rewriteToServer rewrites the request URL host to srv, so the OpenRouter
// policy (enforced on the original BaseURL, which classifies as OpenRouter)
// is exercised against the fake server.
//
// The endpoint orChat derives is the transport client's own BaseURL when the
// client carries a non-empty one (the test seam for httptest URLs); so a
// Mashūra call under zdr on exercised through RunPanel with an
// OpenRouter-classified client exercises the OpenRouter branch end to end.
func rewriteToServer(srv *httptest.Server) *http.Client {
	host := srv.URL[len("http://"):]
	return &http.Client{Transport: rewriteTransport{host: host}}
}

type rewriteTransport struct{ host string }

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	u.Scheme = "http"
	u.Host = rt.host
	req = req.Clone(req.Context())
	req.URL = &u
	return http.DefaultTransport.RoundTrip(req)
}

// TestSmallReviewHelpers covers the small review helpers the wire shape
// exercises: firstLine, tailOverlap, splitByLines edges, unitAtomic, and
// the panel-debug safeLabel + firstErr helpers.
func TestSmallReviewHelpers(t *testing.T) {
	if got := firstLine("hello\nworld"); got != "hello" {
		t.Errorf("firstLine = %q", got)
	}
	if got := firstLine(strings.Repeat("x", 100)); len(got) != 63 || !strings.HasSuffix(got, "…") {
		t.Errorf("long firstLine = %q (len %d)", got, len(got))
	}
	if got := tailOverlap("abc\ndef", 100); got != "abc\ndef" {
		t.Errorf("short tailOverlap = %q", got)
	}
	if got := tailOverlap("aaa\nbbb\nccc", 3); got != "ccc" {
		t.Errorf("tailOverlap trim = %q", got)
	}
	if _, err := splitByLines("line\n", 300); err != nil {
		t.Errorf("single line must fit: %v", err)
	}
	if _, err := splitByLines(strings.Repeat("x", 301), 300); err == nil || !strings.Contains(err.Error(), "line exceeds") {
		t.Errorf("oversize line must fail: %v", err)
	}
	if _, err := splitByLines(strings.Repeat("x", 301), 300); err == nil || !strings.Contains(err.Error(), "line exceeds") {
		t.Errorf("oversize lone line must fail: %v", err)
	}
	if unitAtomic(unit{label: "a.go"}) != true {
		t.Error("a.go must be atomic")
	}
	if unitAtomic(unit{label: "notes.md"}) != false {
		t.Error("a document file must be splittable")
	}
	if unitAtomic(unit{label: ""}) != false {
		t.Error("unlabelled preamble is splittable")
	}
	// panel-debug helpers.
	if got := safeLabel("anthropic:test-model"); got != "anthropic_test-model" {
		t.Errorf("safeLabel = %q", got)
	}
	if got := safeLabel(""); got != "call" {
		t.Errorf("safeLabel empty = %q", got)
	}
	if firstErr(nil, errors.New("a"), errors.New("b")) == nil || firstErr(nil, nil) != nil {
		t.Error("firstErr must return the first non-nil, nil when all nil")
	}
	// PanelOpCostLine branches.
	if got := PanelOpCostLine(nil); !strings.Contains(got, "unpriced") {
		t.Errorf("unpriced line = %q", got)
	}
	// VoidHeader with and without VoidNotObtained log.
	if got := VoidHeader([]PanelMemberResult{{PrefixedModel: "a:x", Void: true}}); !strings.Contains(got, "was not reviewed by every reviewer") {
		t.Errorf("VoidHeader plain = %q", got)
	}
	if got := VoidHeader([]PanelMemberResult{{PrefixedModel: "a:x", Void: true, Log: &ReviewLog{VoidNotObtained: true}}}); !strings.Contains(got, "NOT obtained") {
		t.Errorf("VoidHeader refresh = %q", got)
	}
}

// TestPanelDetailShapes pins PanelDetail's mode branches without a server.
func TestPanelDetailShapes(t *testing.T) {
	single := PanelDetail("default", []string{"openrouter:m"}, "panel", "q", "ctx", nil)
	if !strings.Contains(single, "model:") {
		t.Errorf("single-member detail = %q", single)
	}
	if got := PanelDetail("p", []string{"openrouter:a", "openrouter:b"}, "fallback", "q", "", nil); !strings.Contains(got, "fallback") {
		t.Errorf("multi detail = %q", got)
	}
	if got := PanelDetail("p", []string{"openrouter:a"}, "debate", "q", "", nil); !strings.Contains(got, "debate") {
		t.Errorf("debate detail = %q", got)
	}
	if got := PanelDetail("p", []string{"openrouter:a"}, "fusion", "q", "", nil); !strings.Contains(got, "fusion") {
		t.Errorf("fusion detail = %q", got)
	}
	// Truncation branch.
	long := PanelDetail("p", []string{"openrouter:a", "openrouter:b"}, "panel", strings.Repeat("q", 3000), "", nil)
	if !strings.Contains(long, "(+") {
		t.Errorf("truncated detail must carry the suffix: %q...", long[:80])
	}
}

// TestCallMemberSnakeCase covers the openrouter-passthrough mapping and the
// failure mode for an unmappable provider.
func TestCallMemberSnakeCase(t *testing.T) {
	srv := openRouterSSETestServer(t, "ok", 1, 1)
	defer srv.Close()
	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, Transport: transportForTest(srv)}
	// openrouter: passes through unchanged.
	ans, _, err := callMember(context.Background(), "openrouter", "google/gemini-2.5-pro", "q", "b", "", ccfg, map[string]string{"openrouter": "key"})
	if err != nil || ans != "ok" {
		t.Errorf("openrouter passthrough: %q %v", ans, err)
	}
	// anthropic: maps to anthropic/<id>.
	ans, _, err = callMember(context.Background(), "anthropic", "claude-x", "q", "b", "", ccfg, map[string]string{"openrouter": "key"})
	if err != nil || ans != "ok" {
		t.Errorf("anthropic mapping: %q %v", ans, err)
	}
	// Unknown provider fails closed.
	if _, _, err := callMember(context.Background(), "unknown", "m", "q", "b", "", ccfg, map[string]string{}); err == nil {
		t.Error("unknown provider must fail")
	}
}

// TestBudgetStateSessionCost exercises the session-cost-func branch.
func TestBudgetStateSessionCost(t *testing.T) {
	bs := newBudgetState(PanelCallConfig{SessionBudgetUSD: 10, SessionCostFunc: func() float64 { return 5 }})
	bs.add(OracleUsage{HasProviderCost: true, ProviderCostUSD: 1.0, InputTokens: 1, OutputTokens: 1})
	if got := bs.spentUSD(); got != 1.0 {
		t.Errorf("spentUSD = %v, want 1.0", got)
	}
	if _, stop := bs.shouldStop(); stop {
		t.Error("session base 5 + spent 1 < 10 must not stop")
	}
}
func TestBudgetStateArithmetic(t *testing.T) {
	// Nil-state no-ops.
	var nilState *BudgetState
	nilState.add(OracleUsage{InputTokens: 1})
	if nilState.spentUSD() != 0 {
		t.Error("nil spentUSD must be 0")
	}
	if _, stop := nilState.shouldStop(); stop {
		t.Error("nil shouldStop must not stop")
	}

	// Priced add sets the per-token rate; the budget then trips at 1.0.
	bs := newBudgetState(PanelCallConfig{ReviewBudgetUSD: 1.0})
	bs.add(OracleUsage{InputTokens: 100, OutputTokens: 100, HasProviderCost: true, ProviderCostUSD: 1.0})
	if got := bs.spentUSD(); got != 1.0 {
		t.Errorf("spentUSD = %v, want 1.0", got)
	}
	if _, stop := bs.shouldStop(); !stop {
		t.Error("budget 1.0 with spend 1.0 must stop")
	}
	if _, stop := bs.shouldStop(); !stop {
		t.Error("shouldStop must be sticky once stopped")
	}
	// Session budget with no cost func: spend against the session budget.
	bs2 := &BudgetState{spent: 5, sessionBudget: 10}
	if _, stop := bs2.shouldStop(); stop {
		t.Error("5 < 10 must not stop")
	}
	bs2.spent = 10
	if _, stop := bs2.shouldStop(); !stop {
		t.Error("session spend 10 >= 10 must stop")
	}
	// Unpriced estimation path: no rate known, counted unpriced, no stop.
	bs3 := newBudgetState(PanelCallConfig{})
	bs3.add(OracleUsage{InputTokens: 50, OutputTokens: 50})
	if got := bs3.spentUSD(); got != 0 {
		t.Errorf("unpriced spend without a rate = %v, want 0", got)
	}
	if _, stop := bs3.shouldStop(); stop {
		t.Error("no budget configured must not stop")
	}
}

// TestVoidCauseClassifier covers voidCauseFrom's classes directly, plus the
// split error-type Error() lines and the reasoningSpecString shapes.
func TestVoidCauseClassifier(t *testing.T) {
	// Error type contracts.
	if (&VoidError{Reason: "x"}).Error() != "VOID: x" {
		t.Error("VoidError.Error")
	}
	if (&BudgetError{}).Error() != "NOT COMPLETED: review budget reached" {
		t.Error("BudgetError.Error empty reason")
	}
	if (&BudgetError{Reason: "r"}).Error() != "NOT COMPLETED: r" {
		t.Error("BudgetError.Error with reason")
	}
	if firstFinishReasons(reviewOutcome{Log: &ReviewLog{FinishReasons: []string{"length"}}}) == nil {
		t.Error("firstFinishReasons with a log")
	}
	if got := reasoningSpecString(&config.ReasoningSpec{Effort: "medium", MaxTokens: 2048}); got != "medium/2048" {
		t.Errorf("both set = %q", got)
	}
	incLimit := &IncompleteReplyError{Msg: "reply hit max_tokens (4096) on part 1 of 1", Length: true}
	if voidCauseFrom(incLimit, OracleUsage{}, nil) != "limit" {
		t.Error("limit IncompleteReplyError")
	}
	if voidCauseFrom(nil, OracleUsage{FinishReason: "length"}, nil) != "limit" {
		t.Error("finish limit")
	}
	if voidCauseFrom(nil, OracleUsage{}, &ReviewLog{FinishReasons: []string{"?", "max_tokens"}}) != "limit" {
		t.Error("log limit")
	}
	if voidCauseFrom(errors.New("VOID: empty reply on part 1"), OracleUsage{}, nil) != "empty" {
		t.Error("empty classify")
	}
	if voidCauseFrom(errors.New("VOID: part 1 of 1: the reply contained only the last-section citation"), OracleUsage{}, nil) != "citation" {
		t.Error("citation classify")
	}
	if voidCauseFrom(errors.New("VOID: something odd"), OracleUsage{}, nil) != "other" {
		t.Error("other classify")
	}
	if got := voidFinishFrom(OracleUsage{FinishReason: "stop"}, &ReviewLog{FinishReasons: []string{"?", ""}}); got != "stop" {
		t.Errorf("voidFinishFrom = %q", got)
	}
	if got := reasoningSpecString(nil); got != "none" {
		t.Errorf("nil spec = %q", got)
	}
	if got := reasoningSpecString(&config.ReasoningSpec{Effort: "medium"}); got != "medium" {
		t.Errorf("effort-only = %q", got)
	}
	if got := reasoningSpecString(&config.ReasoningSpec{MaxTokens: 2048}); got != "2048" {
		t.Errorf("max-only = %q", got)
	}
	if out, ok := lowerReasoningSpec(&config.ReasoningSpec{Effort: "medium", MaxTokens: 8192}); !ok || out.MaxTokens != 4096 {
		t.Errorf("halve = %+v %v", out, ok)
	}
	if out, ok := lowerReasoningSpec(&config.ReasoningSpec{Effort: "high"}); !ok || out.Effort != "medium" {
		t.Errorf("step = %+v %v", out, ok)
	}
	if _, ok := lowerReasoningSpec(&config.ReasoningSpec{Effort: "minimal"}); ok {
		t.Error("minimal lowest")
	}
}
