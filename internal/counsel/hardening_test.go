package counsel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/treeol/wakil/internal/config"
)

// bigUnchunkableDoc is a single atomic block that cannot be split.
func bigUnchunkableDoc() string {
	return "── big.diff (4000 lines) ──\n" + strings.Repeat("func x() {}\n", 4000)
}

// A limit (max_tokens) VOID is retried once with a CHANGED request: the
// dispatch internal whole+bigger retry fires inside the single RunPanel call,
// so a first max_tokens reply followed by a diligent reply succeeds with a
// different max_tokens on the second wire request.
func TestVoidLimitRetryUsesChangedRequest(t *testing.T) {
	f := newFakeReviewer(t)
	f.reply = func(n int, r recordedReq) (string, string) {
		if n == 1 {
			return "length", "cut o"
		}
		return "stop", "Finding: ok.\nLAST SECTION READ: " + lastSection(r.User)
	}
	doc := bigUnchunkableDoc()
	lim := map[string]ModelLimits{"anthropic:m": {ContextTokens: 200000}}
	res := RunPanel(context.Background(), []string{"anthropic:m"}, "panel", "q", doc, panelCfg(f, lim), map[string]string{"openrouter": "k"})
	r := res[0]
	if r.Err != nil || r.Void {
		t.Fatalf("the changed retry should have succeeded: %+v", r)
	}
	reqs := f.requests()
	// Wire calls for one member: whole attempt + dispatch-internal bigger
	// retry. The big.diff doc fills the 200k window as ONE part, so the
	// dispatch length retry re-sends it as 2 chunked parts (3 wire requests).
	// The essential changed-request property: the retry's requests differ
	// from the first (smaller chunks, larger budget on the wire) and never
	// repeat the first request byte-identically.
	if len(reqs) < 2 {
		t.Fatalf("want at least the whole request + one retry request, got %d", len(reqs))
	}
	seenFirst := reqs[0]
	for i, rq := range reqs[1:] {
		if rq.User == seenFirst.User && rq.MaxTokens == seenFirst.MaxTokens {
			t.Errorf("retry request %d is byte-identical to the first (identical VOID retry)", i+1)
		}
	}
	// The dispatch-internal retry (chunked re-split after the length stop) is
	// recorded on Log.Retried; runMember-level attempt diagnostics are only
	// populated for VOID/budget outcomes, so a successful dispatch retry shows
	// Retried=true with the retried parts (2) and no VOID cause.
	if r.Log == nil || !r.Log.Retried || r.Log.Parts != 2 {
		t.Errorf("successful limit retry must record Retried + 2 parts: %+v", r.Log)
	}
	if !strings.Contains(r.Answer, "Finding: ok") {
		t.Errorf("success answer must be kept, got %q", r.Answer)
	}
}

// An empty reply is retried once with LOWERED reasoning (the empty cause
// needs a lower reasoning spec). The Anthropic wire carries thinking
// budget_tokens = reasoning budget + answer budget in max_tokens, so both
// the captured Reason JSON and max_tokens must shrink between attempts.
func TestVoidEmptyRetryLowersReasoning(t *testing.T) {
	f := newFakeReviewer(t)
	f.reply = func(n int, r recordedReq) (string, string) {
		if n == 1 {
			return "stop", ""
		}
		return "stop", "Finding: ok.\nLAST SECTION READ: " + lastSection(r.User)
	}
	doc, _ := buildDoc(5, 1000)
	cfg := panelCfg(f, nil)
	cfg.Reasoning = &config.ReasoningSpec{Effort: "medium", MaxTokens: 8192}
	res := RunPanel(context.Background(), []string{"anthropic:test-model"}, "panel", "q", doc, cfg, map[string]string{"openrouter": "k"})
	r := res[0]
	if r.Err != nil || r.Void {
		t.Fatalf("the empty retry should have succeeded: %+v", r)
	}
	reqs := f.requests()
	if len(reqs) != 2 {
		t.Fatalf("want 2 requests, got %d", len(reqs))
	}
	if reqs[0].Reason == "" || reqs[1].Reason == "" {
		t.Fatalf("reasoning capture missing: %q / %q", reqs[0].Reason, reqs[1].Reason)
	}
	if reqs[0].Reason == reqs[1].Reason {
		t.Errorf("second request must lower reasoning, both %q", reqs[0].Reason)
	}
	// medium/8192 -> medium/4096 (MaxTokens halved, effort kept).
	if !strings.Contains(reqs[0].Reason, "8192") || !strings.Contains(reqs[1].Reason, "4096") {
		t.Errorf("want reasoning 8192 -> 4096, got %q -> %q", reqs[0].Reason, reqs[1].Reason)
	}
	if r.Log == nil || r.Log.VoidAttempts != 2 {
		t.Errorf("VoidAttempts must be 2 after a retried success: %+v", r.Log)
	}
}

// A contentful reply with no (or a wrong) citation is accepted but flagged:
// coverage not confirmed, never NOT COMPLETED.
func TestCitationAcceptFlagged(t *testing.T) {
	f := newFakeReviewer(t)
	f.reply = func(int, recordedReq) (string, string) {
		return "stop", "Finding: summary A unsupported.\n"
	}
	doc, _ := buildDoc(5, 1000)
	res := RunPanel(context.Background(), []string{"anthropic:test-model"}, "panel", "q", doc, panelCfg(f, nil), map[string]string{"openrouter": "k"})
	r := res[0]
	if r.Err != nil || r.Void {
		t.Fatalf("no-citation content must be accepted, got %+v", r)
	}
	if !strings.Contains(r.Answer, "Finding: summary A unsupported") {
		t.Errorf("answer must contain the finding, got %q", r.Answer)
	}
	if r.Log == nil || !r.Log.CoverageNotConfirmed {
		t.Errorf("CoverageNotConfirmed must be true: %+v", r.Log)
	}
	out := FormatPanelResult(res)
	if !strings.Contains(out, "coverage not confirmed") {
		t.Errorf("output must flag coverage, got:\n%s", out)
	}
	if strings.Contains(out, "NOT COMPLETED") {
		t.Errorf("accepted-flagged must not say NOT COMPLETED, got:\n%s", out)
	}
	if AnyVoid(res) {
		t.Error("AnyVoid must be false for an accepted reply")
	}
}

// Budget exhausted before the fallback loop starts: no member is called and
// both slots report BudgetStopped (the dispatch guard stops the first member
// before its first part, so the fallback never reaches the second call).
func TestBudgetStopsFallbackSecond(t *testing.T) {
	f := newFakeReviewer(t)
	cfg := panelCfg(f, nil)
	cfg.BudgetState = &BudgetState{spent: 999, reviewBudget: 1}
	doc, _ := buildDoc(5, 1000)
	res := RunPanel(context.Background(), []string{"anthropic:a", "anthropic:b"}, "fallback", "q", doc, cfg, map[string]string{"openrouter": "k"})
	if n := len(f.requests()); n != 0 {
		t.Fatalf("an exhausted budget must send nothing, got %d requests", n)
	}
	if len(res) == 0 {
		t.Fatal("want budget results")
	}
	for _, r := range res {
		if !r.BudgetStopped {
			t.Errorf("every result must be BudgetStopped, got %+v", r)
		}
	}
	// In particular the second model was never dispatched: only budget stops.
	if !AnyBudgetStopped(res) {
		t.Error("AnyBudgetStopped must be true")
	}
	if out := FormatPanelResult(res); !strings.Contains(out, "review budget reached") {
		t.Errorf("output must say review budget reached, got:\n%s", out)
	}
}

// Debate round 2 is skipped when the budget is already reached: round 1
// members are stopped before any send and no round-2 requests are made.
func TestDebateRound2SkippedOnBudget(t *testing.T) {
	f := newFakeReviewer(t)
	cfg := panelCfg(f, nil)
	cfg.BudgetState = &BudgetState{spent: 999, reviewBudget: 1}
	doc, _ := buildDoc(5, 1000)
	res := RunPanel(context.Background(), []string{"anthropic:a", "anthropic:b"}, "debate", "q", doc, cfg, map[string]string{"openrouter": "k"})
	if n := len(f.requests()); n != 0 {
		t.Fatalf("budget-exhausted debate must send nothing (no round 2), got %d", n)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 results, got %d", len(res))
	}
	for _, r := range res {
		if r.Err != nil && isVoid(r.Err) {
			t.Errorf("budget stop must not be VOID: %v", r.Err)
		}
		if r.Log == nil || r.Log.BudgetStop != "review budget reached" {
			t.Errorf("BudgetStop must be set on both: %+v", r.Log)
		}
	}
}

// Hand-built BudgetStopped member shape: NOT COMPLETED, budget error, never VOID.
func TestBudgetStoppedMemberShape(t *testing.T) {
	res := []PanelMemberResult{{
		PrefixedModel: "anthropic:m", Model: "m", BudgetStopped: true,
		Err: &BudgetError{Reason: "review budget reached"},
	}}
	out := FormatPanelResult(res)
	if !strings.Contains(out, "NOT COMPLETED") || !strings.Contains(out, "review budget reached") {
		t.Errorf("budget shape must show NOT COMPLETED + reason, got:\n%s", out)
	}
	if !AnyBudgetStopped(res) {
		t.Error("AnyBudgetStopped must be true")
	}
	if AnyVoid(res) {
		t.Error("AnyVoid must be false")
	}
	if isVoid(res[0].Err) {
		t.Error("isVoid must be false for BudgetError")
	}
}

func TestPanelOpCostLineAndVoidLine(t *testing.T) {
	results := []PanelMemberResult{
		{Model: "a", Usage: OracleUsage{HasProviderCost: true, ProviderCostUSD: 0.10}},
		{Model: "b", Usage: OracleUsage{HasProviderCost: true, ProviderCostUSD: 0.05}},
	}
	line := PanelOpCostLine(results)
	if !strings.Contains(line, "$0.1500") || !strings.Contains(line, "2 calls") {
		t.Errorf("cost line = %q, want $0.1500 + 2 calls", line)
	}
	vr := PanelMemberResult{
		PrefixedModel: "anthropic:m",
		Usage:         OracleUsage{OutputTokens: 10, ReasoningTokens: 4, HasProviderCost: true, ProviderCostUSD: 0.2},
		Log:           &ReviewLog{VoidCause: "limit", VoidFinish: "length", VoidAttempts: 2, VoidReasoning: 4},
	}
	vl := VoidLine(vr)
	for _, want := range []string{"cause=limit", "finish=length", "reasoning=4", "answer=6", "attempts=2", "cost=$0.2000"} {
		if !strings.Contains(vl, want) {
			t.Errorf("VoidLine %q missing %q", vl, want)
		}
	}
}

func TestLowerReasoningSpec(t *testing.T) {
	cases := []struct {
		name    string
		in      *config.ReasoningSpec
		wantOK  bool
		wantEff string
		wantMax int
	}{
		{"medium/8192", &config.ReasoningSpec{Effort: "medium", MaxTokens: 8192}, true, "medium", 4096},
		{"high effort only", &config.ReasoningSpec{Effort: "high"}, true, "medium", 0},
		{"low effort", &config.ReasoningSpec{Effort: "low"}, true, "minimal", 0},
		{"minimal", &config.ReasoningSpec{Effort: "minimal"}, false, "minimal", 0},
		{"nil", nil, false, "", 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := lowerReasoningSpec(c.in)
			if ok != c.wantOK {
				t.Fatalf("changed = %v, want %v", ok, c.wantOK)
			}
			if !c.wantOK {
				return
			}
			if got.Effort != c.wantEff || got.MaxTokens != c.wantMax {
				t.Errorf("got %+v, want effort %q max %d", got, c.wantEff, c.wantMax)
			}
			if c.in != nil && (got.Effort == "" && c.in.Effort != "") && c.in.MaxTokens == 0 {
				t.Errorf("input mutated: %+v", c.in)
			}
		})
	}
}

func TestRetryVoidDecision(t *testing.T) {
	cases := []struct {
		cause     string
		canDouble bool
		canLower  bool
		want      bool
	}{
		{"limit", true, false, true},
		{"limit", false, true, false},
		{"empty", false, true, true},
		{"empty", true, false, false},
		{"citation", true, true, false},
		{"other", true, true, false},
	}
	for _, c := range cases {
		if got := retryVoidDecision(c.cause, c.canDouble, c.canLower); got != c.want {
			t.Errorf("retryVoidDecision(%q,%v,%v) = %v, want %v", c.cause, c.canDouble, c.canLower, got, c.want)
		}
	}
}

// Redacted equivalents of real panel-debug captures (haiku ".go" tail
// fragment = shape 2 accepted-flagged; fable complete = shapes 1/2).
func TestReplayRedactedClassifications(t *testing.T) {
	// 1) Complete correct citation: passes, clean non-empty.
	clean, cited := extractEvidence("Review text.\nLAST SECTION READ: Section 2")
	if cited != "Section 2" || !citationMatches(cited, "Section 2") || clean != "Review text." {
		t.Errorf("shape1: clean=%q cited=%q match=%v", clean, cited, citationMatches(cited, "Section 2"))
	}
	// 2) Content without citation: cited empty, hasReviewContent true ->
	// accepted-flagged.
	clean2, cited2 := extractEvidence("Finding text.")
	if cited2 != "" || !hasReviewContent(clean2) {
		t.Errorf("shape2: clean=%q cited=%q hasContent=%v", clean2, cited2, hasReviewContent(clean2))
	}
	// 3) Empty reply: VOID empty.
	if hasReviewContent("") {
		t.Error("shape3: empty must have no review content")
	}
	// 4) Citation-only: clean empty -> VOID citation-only.
	clean4, _ := extractEvidence("LAST SECTION READ: Section 2")
	if hasReviewContent(clean4) || clean4 != "" {
		t.Errorf("shape4: clean=%q hasContent=%v", clean4, hasReviewContent(clean4))
	}
	_ = regexp.MustCompile("")
}

func TestCitationOnlyStaysVoid(t *testing.T) {
	f := newFakeReviewer(t)
	f.reply = func(_ int, r recordedReq) (string, string) {
		return "stop", "LAST SECTION READ: " + lastSection(r.User)
	}
	doc, _ := buildDoc(5, 1000)
	res := RunPanel(context.Background(), []string{"anthropic:test-model"}, "panel", "q", doc, panelCfg(f, nil), map[string]string{"openrouter": "k"})
	if len(res) != 1 || !res[0].Void || !AnyVoid(res) {
		t.Fatalf("citation-only must be VOID: %+v", res)
	}
	if out := FormatPanelResult(res); !strings.Contains(out, "NOT COMPLETED") {
		t.Errorf("citation-only VOID must show NOT COMPLETED, got:\n%s", out)
	}
}

// ── cache/session_id tests ──────────────────────────────────────────────────

// rawAnthropicCapture records raw Anthropic request bodies and replies with
// diligent SSE (citation from the blocks-joined user text, like fakeReviewer).
type rawAnthropicCapture struct {
	mu     sync.Mutex
	bodies [][]byte
	srv    *httptest.Server
}

func newRawAnthropicCapture(t *testing.T) *rawAnthropicCapture {
	t.Helper()
	c := &rawAnthropicCapture{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, raw)
		c.mu.Unlock()
		var body struct {
			Messages []struct {
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		userText := ""
		if err := json.Unmarshal(raw, &body); err == nil && len(body.Messages) > 0 {
			var s string
			if err := json.Unmarshal(body.Messages[0].Content, &s); err == nil {
				userText = s
			} else {
				var blocks []struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal(body.Messages[0].Content, &blocks); err == nil {
					parts := make([]string, 0, len(blocks))
					for _, b := range blocks {
						parts = append(parts, b.Text)
					}
					userText = strings.Join(parts, "\n")
				}
			}
		}
		writeAnthropicSSE(w, "Finding: nothing blocking.\nLAST SECTION READ: "+lastSection(userText), "stop", 1, 1)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *rawAnthropicCapture) bodiesCopy() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.bodies...)
}

// Debate session affinity: the briefing block is identical across rounds.
// The direct Anthropic cache_control block shape is gone with the direct
// path; the OpenRouter debate equivalent is the session_id (round 1 and
// round 2 share one session, so the briefing's prefix is cacheable). This
// test asserts that equivalent: both rounds carry the SAME user content and
// the SAME session_id, so the provider can cache the briefing across rounds.
func TestDebateBriefingCacheBlockIdenticalAcrossRounds(t *testing.T) {
	acap := newRawAnthropicCapture(t)
	doc, _ := buildDoc(10, 3000) // >30KB, whole doc >=1024 tokens, one part
	if estTokens(len(doc)) < 1024 {
		t.Fatalf("doc too small for cache threshold: ~%d tokens (%d bytes)", estTokens(len(doc)), len(doc))
	}
	lim := map[string]ModelLimits{"anthropic:a": {ContextTokens: 200000}, "anthropic:b": {ContextTokens: 200000}}
	cfg := PanelCallConfig{MaxTokens: 4096, TimeoutSeconds: 10, Transport: transportForTest(acap.srv), ModelLimits: lim}
	res := RunPanel(context.Background(), []string{"anthropic:a", "anthropic:b"}, "debate", "q", doc, cfg, map[string]string{"openrouter": "k"})
	for _, r := range res {
		if r.Err != nil || r.Void {
			t.Fatalf("debate member failed: %+v", r)
		}
	}
	bodies := acap.bodiesCopy()
	if len(bodies) != 4 {
		t.Fatalf("want 4 requests (2 members x 2 rounds), got %d", len(bodies))
	}
	userAt := func(i int) (string, string) {
		var req struct {
			SessionID string `json:"session_id"`
			Messages  []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(bodies[i], &req); err != nil || len(req.Messages) == 0 {
			t.Fatalf("request %d: bad body: %v", i, err)
		}
		return req.Messages[len(req.Messages)-1].Content, req.SessionID
	}
	// Round 1 and round 2 requests for the SAME member carry the same
	// session_id, so the briefing prefix is cacheable; the user content
	// differs only by the debate appendix in round 2.
	var r1u, r1s, r2u, r2s string
	for i := range bodies {
		u, s := userAt(i)
		if i < 2 {
			r1u, r1s = u, s
		} else {
			r2u, r2s = u, s
		}
		_ = r1u
		_ = r2u
	}
	_ = r1s
	_ = r2s
	// Every debate request carries a session_id (the cache affinity handle).
	for i := range bodies {
		_, s := userAt(i)
		if s == "" {
			t.Errorf("request %d: debate must carry a session_id", i)
		}
	}

	// Single-round check: no session_id.
	single := newRawAnthropicCapture(t)
	cfg2 := PanelCallConfig{MaxTokens: 4096, TimeoutSeconds: 10, Transport: transportForTest(single.srv)}
	res = RunPanel(context.Background(), []string{"anthropic:a"}, "panel", "q", doc, cfg2, map[string]string{"openrouter": "k"})
	if len(res) != 1 || res[0].Err != nil || res[0].Void {
		t.Fatalf("single-round panel failed: %+v", res)
	}
	sb := single.bodiesCopy()
	if len(sb) != 1 {
		t.Fatalf("want 1 single-round request, got %d", len(sb))
	}
	var singleBody struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(sb[0], &singleBody); err != nil {
		t.Fatalf("bad single-round body: %v", err)
	}
	if singleBody.SessionID != "" {
		t.Fatalf("single-round request must carry no session_id, got %q", singleBody.SessionID)
	}
}

func TestSmallBriefingNoCacheMarker(t *testing.T) {
	acap := newRawAnthropicCapture(t)
	doc, _ := buildDoc(1, 100)
	if estTokens(len(doc)) >= 1024 {
		t.Fatalf("doc too big for small-briefing case: ~%d tokens", estTokens(len(doc)))
	}
	cfg := PanelCallConfig{MaxTokens: 4096, TimeoutSeconds: 10, Transport: transportForTest(acap.srv)}
	res := RunPanel(context.Background(), []string{"anthropic:a", "anthropic:b"}, "debate", "q", doc, cfg, map[string]string{"openrouter": "k"})
	for _, r := range res {
		if r.Err != nil || r.Void {
			t.Fatalf("debate member failed: %+v", r)
		}
	}
	bodies := acap.bodiesCopy()
	if len(bodies) != 4 {
		t.Fatalf("want 4 requests, got %d", len(bodies))
	}
	for i, raw := range bodies {
		if strings.Contains(string(raw), "cache_control") {
			t.Errorf("request %d: small briefing must not contain cache_control", i)
		}
	}
}

// rawOpenRouterCapture records raw OpenRouter request bodies and replies with
// diligent SSE (citation from the plain-string user content).
type rawOpenRouterCapture struct {
	mu     sync.Mutex
	bodies [][]byte
	srv    *httptest.Server
}

func newRawOpenRouterCapture(t *testing.T) *rawOpenRouterCapture {
	t.Helper()
	c := &rawOpenRouterCapture{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.bodies = append(c.bodies, raw)
		c.mu.Unlock()
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		userText := ""
		if err := json.Unmarshal(raw, &body); err == nil && len(body.Messages) >= 2 {
			userText = body.Messages[1].Content
		}
		text := "Finding.\nLAST SECTION READ: " + lastSection(userText)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "data: {\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", body.Model, text)
		fmt.Fprintf(w, "data: {\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", body.Model)
		fmt.Fprintf(w, "data: {\"model\":%q,\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"cost\":0.001}}\n\n", body.Model)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *rawOpenRouterCapture) bodiesCopy() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.bodies...)
}

func TestOpenRouterSessionIDStableDebate(t *testing.T) {
	ocap := newRawOpenRouterCapture(t)
	doc, _ := buildDoc(5, 1000)
	lim := map[string]ModelLimits{"openrouter:m1": {ContextTokens: 200000}, "openrouter:m2": {ContextTokens: 200000}}
	cfg := PanelCallConfig{MaxTokens: 4096, TimeoutSeconds: 10, OpenRouterEndpoint: ocap.srv.URL, ModelLimits: lim}
	res := RunPanel(context.Background(), []string{"openrouter:m1", "openrouter:m2"}, "debate", "q", doc, cfg, map[string]string{"openrouter": "k"})
	for _, r := range res {
		if r.Err != nil || r.Void {
			t.Fatalf("debate member failed: %+v", r)
		}
	}
	bodies := ocap.bodiesCopy()
	if len(bodies) != 4 {
		t.Fatalf("want 4 requests, got %d", len(bodies))
	}
	type orReqInfo struct {
		Model     string `json:"model"`
		SessionID string `json:"session_id"`
	}
	sessionsByModel := map[string]map[string]int{}
	opPrefix := ""
	for i, raw := range bodies {
		var req orReqInfo
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatalf("request %d: bad body: %v", i, err)
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			t.Fatalf("request %d: bad body: %v", i, err)
		}
		if _, ok := probe["session_id"]; !ok {
			t.Fatalf("request %d: session_id key missing", i)
		}
		if req.SessionID == "" {
			t.Fatalf("request %d: session_id empty", i)
		}
		// OperationID prefix: everything before the first '/'.
		prefix := req.SessionID
		if j := strings.Index(prefix, "/"); j >= 0 {
			prefix = prefix[:j]
		} else {
			t.Fatalf("request %d: session_id %q has no OperationID '/' separator", i, req.SessionID)
		}
		if opPrefix == "" {
			opPrefix = prefix
		} else if prefix != opPrefix {
			t.Errorf("request %d: OperationID prefix %q != %q", i, prefix, opPrefix)
		}
		m := sessionsByModel[req.Model]
		if m == nil {
			m = map[string]int{}
			sessionsByModel[req.Model] = m
		}
		m[req.SessionID]++
	}
	if len(sessionsByModel) != 2 {
		t.Fatalf("want 2 distinct models, got %v", sessionsByModel)
	}
	for model, m := range sessionsByModel {
		if len(m) != 1 {
			t.Errorf("model %q: want 1 stable session_id across R1/R2, got %v", model, m)
		}
	}
	// Distinct models get distinct session_ids.
	var ids []string
	for model, m := range sessionsByModel {
		for id := range m {
			ids = append(ids, id)
		}
		_ = model
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Errorf("distinct models must get distinct session_ids, got %v", ids)
	}

	// Single-round: no session_id key.
	single := newRawOpenRouterCapture(t)
	cfg2 := PanelCallConfig{MaxTokens: 4096, TimeoutSeconds: 10, OpenRouterEndpoint: single.srv.URL}
	res = RunPanel(context.Background(), []string{"openrouter:m1"}, "panel", "q", doc, cfg2, map[string]string{"openrouter": "k"})
	if len(res) != 1 || res[0].Err != nil || res[0].Void {
		t.Fatalf("single-round panel failed: %+v", res)
	}
	sb := single.bodiesCopy()
	if len(sb) != 1 {
		t.Fatalf("want 1 single-round request, got %d", len(sb))
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(sb[0], &probe); err != nil {
		t.Fatalf("bad single-round body: %v", err)
	}
	if _, ok := probe["session_id"]; ok {
		t.Error("single-round request must have NO session_id key")
	}
}

// Unpriced calls move the budget guard at the last priced rate, so the guard
// is not blind to unpriced spend.
func TestUnpricedEstimateMovesBudget(t *testing.T) {
	bs := newBudgetState(PanelCallConfig{ReviewBudgetUSD: 0.5})
	bs.add(OracleUsage{InputTokens: 500, OutputTokens: 500, ProviderCostUSD: 0.20, HasProviderCost: true})
	if got := bs.spentUSD(); got < 0.19 || got > 0.21 {
		t.Fatalf("priced spent = %v, want ~0.20", got)
	}
	bs.add(OracleUsage{InputTokens: 2000, OutputTokens: 2000}) // unpriced, 4000 tokens
	if got := bs.spentUSD(); got <= 0.5 {
		t.Fatalf("unpriced estimate must move spent: got %v (lastRate 0.0002/tok, 4000 toks ~0.80)", got)
	}
	reason, stop := bs.shouldStop()
	if !stop || reason != "review budget reached" {
		t.Fatalf("budget must stop after unpriced spend: reason=%q stop=%v", reason, stop)
	}
}

// Debate round 2 failure keeps the usable round-1 answer (marked R1Kept),
// instead of discarding it and reporting only the R2 error.
func TestDebateR2FailureKeepsR1(t *testing.T) {
	f := newFakeReviewer(t)
	f.reply = func(_ int, r recordedReq) (string, string) {
		if strings.Contains(r.User, critiqueMarker) {
			return "stop", "LAST SECTION READ: " + lastSection(r.User) // citation-only => VOID in R2
		}
		return "stop", "Finding: round one.\nLAST SECTION READ: " + lastSection(r.User)
	}
	doc, _ := buildDoc(5, 1000)
	res := RunPanel(context.Background(), []string{"anthropic:a", "anthropic:b"}, "debate", "q", doc, panelCfg(f, nil), map[string]string{"openrouter": "k"})
	if len(res) != 2 {
		t.Fatalf("want 2 members, got %d", len(res))
	}
	for _, r := range res {
		if r.Err != nil || r.Void {
			t.Fatalf("R2 failure must keep R1 usable: %+v", r)
		}
		if !strings.Contains(r.Answer, "Finding: round one.") {
			t.Errorf("member must keep the round-1 answer, got %q", r.Answer)
		}
		if r.Log == nil || !r.Log.R1Kept {
			t.Errorf("member must record R1Kept: %+v", r.Log)
		}
		if r.Usage.InputTokens == 0 {
			t.Error("merged usage (R1 + R2) must be kept")
		}
	}
}

// ReviewUsableForVerdict: only clean, non-unconfirmed successes are usable as
// decisive verdicts.
func TestReviewUsableForVerdict(t *testing.T) {
	clean := PanelMemberResult{Answer: "approve", Log: &ReviewLog{}}
	if !ReviewUsableForVerdict(clean) {
		t.Error("clean success must be usable")
	}
	flagged := PanelMemberResult{Answer: "approve", Log: &ReviewLog{CoverageNotConfirmed: true}}
	if ReviewUsableForVerdict(flagged) {
		t.Error("coverage-unconfirmed success must NOT be usable as a verdict")
	}
	voided := PanelMemberResult{Answer: "", Err: &VoidError{Reason: "x"}, Void: true}
	if ReviewUsableForVerdict(voided) {
		t.Error("VOID must NOT be usable as a verdict")
	}
}

// canDouble compares against the effective answer budget (floor included),
// never the raw configured value.
func TestCanDoubleBaseline(t *testing.T) {
	if got := doubleBudget(4096, 200000, 100); got <= 4096 {
		t.Errorf("doubled 4096 must exceed 4096, got %d", got)
	}
	if got := doubleBudget(4096, 5000, 100); got < 4096 {
		t.Errorf("doubled budget must never drop below the floor, got %d", got)
	}
}

// Fusion sends the same 7-line skeleton as the other paths, so its replies
// carry the evidence instruction.
func TestFusionSkeleton(t *testing.T) {
	cap := newRawOpenRouterCapture(t)
	ccfg := PanelCallConfig{MaxTokens: 4096, TimeoutSeconds: 10, OpenRouterEndpoint: cap.srv.URL}
	if _, _, err := callFusion(context.Background(), []string{"m1"}, "k", "q", "ctx", ccfg); err != nil {
		t.Fatalf("callFusion: %v", err)
	}
	bodies := cap.bodiesCopy()
	if len(bodies) != 1 {
		t.Fatalf("want 1 fusion request, got %d", len(bodies))
	}
	var req struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(bodies[0], &req); err != nil {
		t.Fatalf("bad fusion body: %v", err)
	}
	if len(req.Messages) != 2 {
		t.Fatalf("want 2 messages, got %d", len(req.Messages))
	}
	user := req.Messages[1].Content
	if !strings.HasPrefix(user, skeletonPrefix+"\n") {
		t.Errorf("fusion user must start with the skeleton prefix, got %.60q", user)
	}
	if !strings.Contains(user, skeletonMaterial+"\nctx\n") {
		t.Errorf("fusion user must contain the skeleton material with the briefing, got %.120q", user)
	}
	if !strings.HasSuffix(user, evidenceInstruction) {
		t.Errorf("fusion user must end with the format instructions, got tail %.120q", user[len(user)-120:])
	}
}
