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
)

// recordedReq is one request the fake reviewer backend received.
type recordedReq struct {
	MaxTokens int
	User      string // full user message (question + "Context:" + briefing)
}

// fakeReviewer is a fake LLM backend that records EXACTLY what it receives and
// answers like a diligent reviewer: it reads the whole message, finds the last
// section heading / block label, and cites it. Behaviour is overridable.
type fakeReviewer struct {
	mu   sync.Mutex
	reqs []recordedReq
	// reply decides the HTTP body for a request (nil = diligent reviewer).
	reply func(n int, r recordedReq) (stopReason, text string)
	srv   *httptest.Server
}

func newFakeReviewer(t *testing.T) *fakeReviewer {
	t.Helper()
	f := &fakeReviewer{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			MaxTokens int `json:"max_tokens"`
			Messages  []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.Unmarshal(raw, &body); err != nil || len(body.Messages) == 0 {
			http.Error(w, "bad request", 400)
			return
		}
		rec := recordedReq{MaxTokens: body.MaxTokens, User: body.Messages[0].Content}
		f.mu.Lock()
		f.reqs = append(f.reqs, rec)
		n := len(f.reqs)
		f.mu.Unlock()
		stop, text := "end_turn", ""
		if f.reply != nil {
			stop, text = f.reply(n, rec)
		} else {
			text = "Finding: nothing blocking.\nLAST SECTION READ: " + lastSection(rec.User)
		}
		// Return SSE format (Anthropic Messages API streaming).
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":%d,\"output_tokens\":0}}}\n\n", len(rec.User)/4)
		fmt.Fprintf(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"text\"}}\n\n")
		fmt.Fprintf(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":%q}}\n\n", text)
		fmt.Fprintf(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\"}\n\n")
		fmt.Fprintf(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%q},\"usage\":{\"output_tokens\":10}}\n\n", stop)
		fmt.Fprintf(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeReviewer) requests() []recordedReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedReq(nil), f.reqs...)
}

// lastSection returns the last "# heading" text or "── label ──" label found in
// the document part of a received message (independent of the production
// parser on purpose).
func lastSection(user string) string {
	if i := strings.Index(user, critiqueMarker); i >= 0 {
		user = user[:i]
	}
	last := ""
	inFence := false
	for _, l := range strings.Split(user, "\n") {
		if strings.HasPrefix(l, "```") {
			inFence = !inFence
		}
		if inFence {
			continue
		}
		if strings.HasPrefix(l, "#") {
			last = strings.TrimSpace(strings.TrimLeft(l, "# "))
		} else if strings.HasPrefix(l, "── ") && strings.HasSuffix(l, " ──") {
			last = strings.TrimSuffix(strings.TrimPrefix(l, "── "), " ──")
		}
	}
	return last
}

// buildDoc returns a markdown document with n sections of ~bytes each; every
// section has a unique heading and a unique marker line.
func buildDoc(n, bytes int) (string, []string) {
	var b strings.Builder
	var heads []string
	b.WriteString("# Design\n\nIntro paragraph.\n\n")
	heads = append(heads, "Design")
	for i := 1; i <= n; i++ {
		h := fmt.Sprintf("Section %d: topic-%d", i, i)
		heads = append(heads, h)
		fmt.Fprintf(&b, "## %s\n\nMARKER-%d start.\n", h, i)
		line := fmt.Sprintf("Body of section %d — detail detail detail detail.\n", i)
		for b.Len()%(bytes+1) != 0 && len(line) > 0 {
			// pad roughly to `bytes` per section
			if strings.Count(b.String(), fmt.Sprintf("Body of section %d ", i))*len(line) >= bytes {
				break
			}
			b.WriteString(line)
		}
		fmt.Fprintf(&b, "MARKER-%d end.\n\n", i)
	}
	return b.String(), heads
}

func panelCfg(f *fakeReviewer, lim map[string]ModelLimits) PanelCallConfig {
	return PanelCallConfig{MaxTokens: 4096, TimeoutSeconds: 10, AnthropicEndpoint: f.srv.URL, ModelLimits: lim}
}

// A document of ~30k tokens fits a 128k-context model and must reach the
// reviewer WHOLE: one request, every heading and marker present, byte-exact.
func TestReviewWholeDocumentArrivesComplete(t *testing.T) {
	f := newFakeReviewer(t)
	doc, heads := buildDoc(40, 3000) // ~120 KB ≈ 30k tokens (4 B/token)
	if len(doc) < 100_000 {
		t.Fatalf("test doc too small: %d", len(doc))
	}
	res := RunPanel(context.Background(), []string{"anthropic:test-model"}, "panel", "Review this.", doc, panelCfg(f, nil), map[string]string{"anthropic": "k"})
	if len(res) != 1 || res[0].Err != nil || res[0].Void {
		t.Fatalf("result: %+v", res)
	}
	reqs := f.requests()
	if len(reqs) != 1 {
		t.Fatalf("want 1 whole request, got %d", len(reqs))
	}
	if !strings.Contains(reqs[0].User, doc) {
		t.Fatal("the document did not arrive byte-exact")
	}
	for _, h := range heads {
		if !strings.Contains(reqs[0].User, h) {
			t.Errorf("heading %q missing from what the reviewer received", h)
		}
	}
	if strings.Contains(reqs[0].User, "truncated") {
		t.Error("a truncation marker reached the reviewer")
	}
	l := res[0].Log
	if l == nil || l.Parts != 1 || l.TokensSent < 30000 || l.ReadThrough != heads[len(heads)-1] {
		t.Errorf("log = %+v, want 1 part, ≥30k tokens, read through %q", l, heads[len(heads)-1])
	}
	if res[0].Answer != "Finding: nothing blocking." {
		t.Errorf("evidence line must be stripped from the answer, got %q", res[0].Answer)
	}
	// The provider's stop signal is surfaced per reviewer in the round report.
	if got := l.FinishReasons; len(got) != 1 || got[0] != "end_turn" {
		t.Errorf("FinishReasons = %v, want [end_turn]", got)
	}
	if out := FormatPanelResult(res); !strings.Contains(out, "finish_reason: end_turn") {
		t.Errorf("round report must show finish_reason, got:\n%s", out)
	}
}

// A length-truncated reply shows finish_reason=max_tokens in the VOID report,
// not just a missing citation.
func TestReviewFinishReasonShownForTruncation(t *testing.T) {
	f := newFakeReviewer(t)
	f.reply = func(int, recordedReq) (string, string) { return "max_tokens", "I found that the" }
	doc, _ := buildDoc(10, 3000)
	res := RunPanel(context.Background(), []string{"anthropic:test-model"}, "panel", "q", doc, panelCfg(f, nil), map[string]string{"anthropic": "k"})
	if !res[0].Void {
		t.Fatalf("want VOID, got %+v", res[0])
	}
	out := FormatPanelResult(res)
	if !strings.Contains(out, "finish_reason: max_tokens") || !strings.Contains(out, "NOT COMPLETED") {
		t.Errorf("VOID report must show finish_reason=max_tokens, got:\n%s", out)
	}
}

// A small-context model gets the document in labelled parts that together
// cover every byte, with ~200 tokens of overlap, the full question in each
// part, and merged findings.
func TestReviewChunkedCoversEverythingWithOverlap(t *testing.T) {
	f := newFakeReviewer(t)
	doc, heads := buildDoc(40, 3000)
	const q = "QUESTION: is the design sound?"
	lim := map[string]ModelLimits{"anthropic:small": {ContextTokens: 24000}}
	res := RunPanel(context.Background(), []string{"anthropic:small"}, "panel", q, doc, panelCfg(f, lim), map[string]string{"anthropic": "k"})
	if res[0].Err != nil || res[0].Void {
		t.Fatalf("result: %+v", res[0])
	}
	reqs := f.requests()
	n := len(reqs)
	if n < 3 {
		t.Fatalf("want ≥3 parts for a 30k-token doc in a 24k window, got %d", n)
	}
	if res[0].Log.Parts != n {
		t.Errorf("log parts %d != requests %d", res[0].Log.Parts, n)
	}
	for i, r := range reqs {
		if !strings.Contains(r.User, q) {
			t.Errorf("part %d lacks the full question", i+1)
		}
		if !strings.Contains(r.User, fmt.Sprintf("part %d of %d", i+1, n)) {
			t.Errorf("part %d lacks its 'part i of n' header", i+1)
		}
		if est := estTokens(len(r.User)); est > 24000*9/10 {
			t.Errorf("part %d is ~%d tokens, over the 90%% window", i+1, est)
		}
		if r.MaxTokens < 4096 {
			t.Errorf("part %d max_tokens %d below the 4096 floor", i+1, r.MaxTokens)
		}
	}
	all := ""
	for _, r := range reqs {
		all += r.User
	}
	for _, h := range heads {
		if !strings.Contains(all, h) {
			t.Errorf("heading %q lost in chunking", h)
		}
	}
	for i := 1; i <= 40; i++ {
		if !strings.Contains(all, fmt.Sprintf("MARKER-%d end.", i)) {
			t.Errorf("section %d lost in chunking", i)
		}
	}
	// Overlap: the tail of part i's own material (≈200 tokens) is repeated at
	// the start of part i+1.
	for i := 0; i+1 < n; i++ {
		cur, next := reqs[i].User, reqs[i+1].User
		tail := cur[len(cur)-120:]
		if !strings.Contains(next, "[overlap") || !strings.Contains(next, tail) {
			t.Errorf("part %d's tail not repeated as overlap in part %d", i+1, i+2)
		}
	}
	if !strings.Contains(res[0].Answer, "── part 1 of") || !strings.Contains(res[0].Answer, fmt.Sprintf("── part %d of %d ──", n, n)) {
		t.Errorf("merged answer must label every part: %q", res[0].Answer)
	}
}

// A document that cannot fit in any way is an ERROR and nothing is sent — in
// particular no prefix.
func TestReviewOverLimitIsErrorNotPrefix(t *testing.T) {
	f := newFakeReviewer(t)
	lim := map[string]ModelLimits{"anthropic:small": {ContextTokens: 24000}}
	t.Run("one huge section", func(t *testing.T) {
		doc := "# Big\n\n" + strings.Repeat("x", 400_000)
		res := RunPanel(context.Background(), []string{"anthropic:small"}, "panel", "q", doc, panelCfg(f, lim), map[string]string{"anthropic": "k"})
		if res[0].Err == nil || !strings.Contains(res[0].Err.Error(), "nothing was sent") {
			t.Fatalf("want a loud error, got %+v", res[0])
		}
		if res[0].Void {
			t.Error("a too-large document is an error, not a VOID reply")
		}
	})
	t.Run("source file is atomic", func(t *testing.T) {
		doc := "## Sources\n\n── big.go (9000 lines) ──\n" + strings.Repeat("package x // line\n", 30_000)
		res := RunPanel(context.Background(), []string{"anthropic:small"}, "panel", "q", doc, panelCfg(f, lim), map[string]string{"anthropic": "k"})
		if res[0].Err == nil || !strings.Contains(res[0].Err.Error(), "cannot be split") {
			t.Fatalf("a source file must never be cut mid-file, got %+v", res[0])
		}
	})
	t.Run("window smaller than the answer floor", func(t *testing.T) {
		res := RunPanel(context.Background(), []string{"anthropic:tiny"}, "panel", "q", "# a\n\nbody", panelCfg(f, map[string]ModelLimits{"anthropic:tiny": {ContextTokens: 3000}}), map[string]string{"anthropic": "k"})
		if res[0].Err == nil || !strings.Contains(res[0].Err.Error(), "nothing was sent") {
			t.Fatalf("floor that does not fit must error, got %+v", res[0])
		}
	})
	if got := len(f.requests()); got != 0 {
		t.Fatalf("%d request(s) reached the backend; an over-limit document must send nothing", got)
	}
}

// finish_reason=length → retry once chunked → success when the chunks fit.
func TestReviewLengthRetriesChunked(t *testing.T) {
	f := newFakeReviewer(t)
	f.reply = func(n int, r recordedReq) (string, string) {
		if !strings.Contains(r.User, "part ") || !strings.Contains(r.User, " of ") || !strings.Contains(r.User, "[This is part") {
			return "max_tokens", "partial answer cut o"
		}
		return "end_turn", "ok\nLAST SECTION READ: " + lastSection(r.User)
	}
	doc, _ := buildDoc(10, 3000)
	res := RunPanel(context.Background(), []string{"anthropic:test-model"}, "panel", "q", doc, panelCfg(f, nil), map[string]string{"anthropic": "k"})
	if res[0].Err != nil || res[0].Void {
		t.Fatalf("retry should have succeeded: %+v", res[0])
	}
	if !res[0].Log.Retried || res[0].Log.Parts < 2 {
		t.Errorf("log = %+v, want a chunked retry", res[0].Log)
	}
	reqs := f.requests()
	if len(reqs) < 3 || strings.Contains(reqs[0].User, "[This is part") {
		t.Errorf("want 1 whole request then ≥2 chunked, got %d", len(reqs))
	}
	if strings.Contains(res[0].Answer, "partial answer") {
		t.Error("the truncated reply must never be merged")
	}
}

// Truncated again after the retry → VOID: no answer, flagged, never "no findings".
func TestReviewLengthTwiceIsVoid(t *testing.T) {
	f := newFakeReviewer(t)
	f.reply = func(int, recordedReq) (string, string) { return "max_tokens", "I found that the" }
	doc, _ := buildDoc(10, 3000)
	res := RunPanel(context.Background(), []string{"anthropic:test-model"}, "panel", "q", doc, panelCfg(f, nil), map[string]string{"anthropic": "k"})
	r := res[0]
	if !r.Void || r.Err == nil || r.Answer != "" {
		t.Fatalf("want VOID with no answer, got %+v", r)
	}
	if !AnyVoid(res) {
		t.Error("AnyVoid must report it")
	}
	out := FormatPanelResult(res)
	if !strings.Contains(out, "NOT COMPLETED") || !strings.Contains(out, "not a review round") || strings.Contains(strings.ToLower(out), "no blocking findings") {
		t.Errorf("VOID output must say not completed, got:\n%s", out)
	}
	// Billed usage is kept even for a VOID reply.
	if r.Usage.InputTokens == 0 {
		t.Error("usage of the VOID calls must be recorded")
	}
}

// A reply that cites the wrong last section is VOID.
func TestReviewWrongLastHeadingIsVoid(t *testing.T) {
	f := newFakeReviewer(t)
	f.reply = func(int, recordedReq) (string, string) {
		return "end_turn", "all fine\nLAST SECTION READ: 4. Something I invented"
	}
	doc, heads := buildDoc(6, 800)
	res := RunPanel(context.Background(), []string{"anthropic:test-model"}, "panel", "q", doc, panelCfg(f, nil), map[string]string{"anthropic": "k"})
	if !res[0].Void || !strings.Contains(res[0].Err.Error(), heads[len(heads)-1]) {
		t.Fatalf("want VOID naming the real last heading %q, got %+v", heads[len(heads)-1], res[0])
	}
	// Missing citation is VOID too.
	f2 := newFakeReviewer(t)
	f2.reply = func(int, recordedReq) (string, string) { return "end_turn", "looks fine, no findings" }
	res = RunPanel(context.Background(), []string{"anthropic:test-model"}, "panel", "q", doc, panelCfg(f2, nil), map[string]string{"anthropic": "k"})
	if !res[0].Void {
		t.Fatalf("a reply without a citation must be VOID: %+v", res[0])
	}
}

// One VOID part voids the whole member: partial parts are never merged.
func TestReviewVoidPartVoidsMember(t *testing.T) {
	f := newFakeReviewer(t)
	f.reply = func(n int, r recordedReq) (string, string) {
		if n == 2 {
			return "end_turn", "ok\nLAST SECTION READ: Section 1"
		}
		return "end_turn", "ok\nLAST SECTION READ: " + lastSection(r.User)
	}
	doc, _ := buildDoc(40, 3000)
	lim := map[string]ModelLimits{"anthropic:small": {ContextTokens: 24000}}
	res := RunPanel(context.Background(), []string{"anthropic:small"}, "panel", "q", doc, panelCfg(f, lim), map[string]string{"anthropic": "k"})
	if !res[0].Void || res[0].Answer != "" {
		t.Fatalf("want a fully VOID member, got %+v", res[0])
	}
}

// Answer budget: min(configured, ctx − input − 512), at least 4096.
func TestReviewAnswerBudget(t *testing.T) {
	f := newFakeReviewer(t)
	doc, _ := buildDoc(4, 2000)
	cfg := panelCfg(f, map[string]ModelLimits{"anthropic:m": {ContextTokens: 200000, ReviewMaxTokens: 100}})
	RunPanel(context.Background(), []string{"anthropic:m"}, "panel", "q", doc, cfg, map[string]string{"anthropic": "k"})
	if got := f.requests()[0].MaxTokens; got != 4096 {
		t.Errorf("a configured 100 must be raised to the 4096 review floor, got %d", got)
	}
	f2 := newFakeReviewer(t)
	cfg = panelCfg(f2, map[string]ModelLimits{"anthropic:m": {ContextTokens: 200000, ReviewMaxTokens: 16000}})
	RunPanel(context.Background(), []string{"anthropic:m"}, "panel", "q", doc, cfg, map[string]string{"anthropic": "k"})
	if got := f2.requests()[0].MaxTokens; got != 16000 {
		t.Errorf("configured 16000 fits and must be used, got %d", got)
	}
	f3 := newFakeReviewer(t)
	big, _ := buildDoc(12, 3000) // ≈ 9k tokens in a 20k window
	cfg = panelCfg(f3, map[string]ModelLimits{"anthropic:m": {ContextTokens: 20000, ReviewMaxTokens: 30000}})
	RunPanel(context.Background(), []string{"anthropic:m"}, "panel", "q", big, cfg, map[string]string{"anthropic": "k"})
	r := f3.requests()[0]
	want := 18000 - estTokens(len(oracleSystemPrompt)+len(r.User)) - 512 // usable − input − reserve
	if r.MaxTokens > want+60 || r.MaxTokens < 4096 {
		t.Errorf("max_tokens %d must be min(configured, ctx−input−512)≈%d and ≥4096", r.MaxTokens, want)
	}
}

// Debate round 2 appends round-1 answers; the appendix is counted in the budget
// and the last-section check is about the document, not the quoted answers.
func TestReviewDebateRound2Evidence(t *testing.T) {
	f := newFakeReviewer(t)
	doc, _ := buildDoc(5, 1000)
	res := RunPanel(context.Background(), []string{"anthropic:a", "anthropic:b"}, "debate", "q", doc, panelCfg(f, nil), map[string]string{"anthropic": "k"})
	for _, r := range res {
		if r.Err != nil || r.Void {
			t.Fatalf("debate member failed: %+v", r)
		}
	}
	reqs := f.requests()
	if len(reqs) != 4 {
		t.Fatalf("want 2 round-1 + 2 round-2 requests, got %d", len(reqs))
	}
	if !strings.Contains(reqs[3].User, critiqueMarker) {
		t.Error("round 2 must carry the round-1 answers")
	}
}

func TestExtractEvidenceAndMatch(t *testing.T) {
	ans, cited := extractEvidence("Findings here.\n\n**LAST SECTION READ:** `## 4. Design`\n")
	if ans != "Findings here." || cited != "## 4. Design" {
		t.Fatalf("got %q / %q", ans, cited)
	}
	for _, c := range []struct {
		cited, actual string
		ok            bool
	}{
		{"## 4. Design", "4. Design", true},
		{"Design", "4. Design", true},
		{"§4 design", "Design", true},
		{"**Section 12: topic-12**", "Section 12: topic-12", true},
		{"13. Open questions", "4. Design", false},
		{"", "Design", false},
		{"Q", "Design", false},
		{"Safety", "Safety risks", false},        // earlier heading that is a prefix of the last
		{"Safety risks", "Safety", false},        // and the reverse
		{"big.go", "big.go (120 lines)", true},   // label with a line count
		{"`big.go (120 lines)`", "big.go", true}, // formatting is normalised
	} {
		if got := citationMatches(c.cited, c.actual); got != c.ok {
			t.Errorf("citationMatches(%q, %q) = %v, want %v", c.cited, c.actual, got, c.ok)
		}
	}
}

// Headings inside code fences are not split points; source blocks are atomic;
// the parts' bodies concatenate to the exact briefing (nothing lost).
func TestPlanPartsLossless(t *testing.T) {
	doc := "intro\n\n# A\n\n```\n# not a heading\n```\n\n" + strings.Repeat("a line\n", 600) + "\n# B\n\n" + strings.Repeat("b line\n", 600)
	parts, err := planParts(doc, 5500+overlapTokens*reviewCharsPerToken+partSlackChars)
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) < 2 {
		t.Fatalf("want a split, got %d part(s)", len(parts))
	}
	var rebuilt strings.Builder
	for _, p := range parts {
		rebuilt.WriteString(p.body)
	}
	if rebuilt.String() != doc {
		t.Fatal("parts do not concatenate to the original briefing")
	}
}

// A length truncation on an ALREADY chunked review is retried once with
// smaller parts (not straight to VOID); a second truncation is VOID.
func TestReviewLengthRetryWhenAlreadyChunked(t *testing.T) {
	doc, _ := buildDoc(40, 3000)
	lim := map[string]ModelLimits{"anthropic:small": {ContextTokens: 24000}}

	f := newFakeReviewer(t)
	first := 0
	f.reply = func(n int, r recordedReq) (string, string) {
		if n == 2 && first == 0 { // 2nd request of the initial chunked run is cut
			first = len(r.User)
			return "max_tokens", "cut o"
		}
		return "end_turn", "ok\nLAST SECTION READ: " + lastSection(r.User)
	}
	res := RunPanel(context.Background(), []string{"anthropic:small"}, "panel", "q", doc, panelCfg(f, lim), map[string]string{"anthropic": "k"})
	if res[0].Err != nil || res[0].Void || !res[0].Log.Retried {
		t.Fatalf("a chunked review must get its one smaller retry: %+v", res[0])
	}
	reqs := f.requests()
	initial := 0
	for _, r := range reqs {
		if strings.Contains(r.User, "part 1 of ") && initial == 0 {
			initial = 1
		}
	}
	// The retry's parts are smaller than the first attempt's.
	if last := reqs[len(reqs)-1]; len(last.User) >= first+first/3 {
		t.Errorf("retry parts (%d bytes) must be smaller than the truncated part (%d)", len(last.User), first)
	}

	f2 := newFakeReviewer(t)
	f2.reply = func(int, recordedReq) (string, string) { return "max_tokens", "cut" }
	res = RunPanel(context.Background(), []string{"anthropic:small"}, "panel", "q", doc, panelCfg(f2, lim), map[string]string{"anthropic": "k"})
	if !res[0].Void || res[0].Answer != "" {
		t.Fatalf("truncated twice must be VOID: %+v", res[0])
	}
}

// TestAppendixMarkerQuotedInDocumentDoesNotMoveEvidenceTarget: a document that
// quotes (here: inside a code fence) the debate-appendix marker must be
// reviewed against ITS OWN last heading. The appendix is carried explicitly,
// so the quoted marker is just text; a reply citing an earlier heading is VOID
// and the correct citation passes — in round 1 and in the debate round 2.
func TestAppendixMarkerQuotedInDocumentDoesNotMoveEvidenceTarget(t *testing.T) {
	doc := "# Intro\n\nSome text.\n\n```\n" + critiqueMarker + "\n── fake ──\n```\n\n## Final Section\n\nEnd.\n"
	models := []string{"anthropic:a", "anthropic:b"}

	good := newFakeReviewer(t)
	good.reply = func(_ int, _ recordedReq) (string, string) {
		return "end_turn", "Finding: ok.\nLAST SECTION READ: Final Section"
	}
	res := RunPanel(context.Background(), models, "debate", "q", doc, panelCfg(good, nil), map[string]string{"anthropic": "k"})
	for _, r := range res {
		if r.Void || r.Err != nil {
			t.Fatalf("correct citation must pass: %+v", r)
		}
	}
	if n := len(good.requests()); n != 4 {
		t.Fatalf("want 4 requests (2 rounds x 2 members), got %d", n)
	}

	bad := newFakeReviewer(t)
	bad.reply = func(_ int, _ recordedReq) (string, string) {
		return "end_turn", "Finding: ok.\nLAST SECTION READ: Intro"
	}
	res = RunPanel(context.Background(), models, "panel", "q", doc, panelCfg(bad, nil), map[string]string{"anthropic": "k"})
	for _, r := range res {
		if !r.Void {
			t.Fatalf("citing a heading before the document end must be VOID: %+v", r)
		}
	}
}

// TestDebateAppendixSpanningPartsIsIdenticalInEveryPart: when round 2 needs
// several parts, EVERY part carries the same appendix exactly once (counted in
// the budget), the evidence target comes from the part's own document text,
// and no reviewer is voided because of quoted model labels.
func TestDebateAppendixSpanningPartsIsIdenticalInEveryPart(t *testing.T) {
	f := newFakeReviewer(t)
	f.reply = func(_ int, r recordedReq) (string, string) {
		return "end_turn", "Finding " + strings.Repeat("y", 1500) + "\nLAST SECTION READ: " + lastSection(r.User)
	}
	doc, _ := buildDoc(40, 3000)
	models := []string{"anthropic:s1", "anthropic:s2"}
	lim := map[string]ModelLimits{"anthropic:s1": {ContextTokens: 24000}, "anthropic:s2": {ContextTokens: 24000}}
	res := RunPanel(context.Background(), models, "debate", "q", doc, panelCfg(f, lim), map[string]string{"anthropic": "k"})
	for _, r := range res {
		if r.Void || r.Err != nil {
			t.Fatalf("debate member failed: %+v", r)
		}
		if r.Log == nil || r.Log.Parts < 2 {
			t.Fatalf("round 2 should be chunked: %+v", r.Log)
		}
	}
	var appendix string
	r2 := 0
	for _, rq := range f.requests() {
		i := strings.Index(rq.User, "\n\n"+critiqueMarker+"\n")
		if i < 0 {
			continue // a round-1 request
		}
		r2++
		if strings.Count(rq.User, critiqueMarker) != 1 {
			t.Fatalf("appendix must appear exactly once per part")
		}
		ap := rq.User[i:]
		if appendix == "" {
			appendix = ap
		} else if ap != appendix {
			t.Fatalf("appendix differs between parts")
		}
	}
	if r2 < 4 {
		t.Fatalf("want ≥2 round-2 parts per member, got %d requests with an appendix", r2)
	}
}

// A truncated review whose single retry then FAILS (provider error) is still
// VOID — the first reply was cut off, so the member must never look merely
// "failed" (banner, auto-counsel refund and workflow availability depend on it).
func TestReviewTruncatedThenRetryFailsStaysVoid(t *testing.T) {
	cases := []struct {
		name string
		doc  func() string
		lim  map[string]ModelLimits
		want string
	}{
		{"whole-document retry", func() string { return "── big.diff (4000 lines) ──\n" + strings.Repeat("func x() {}\n", 4000) },
			map[string]ModelLimits{"anthropic:m": {ContextTokens: 200000}}, "whole-document retry failed"},
		{"chunked retry", func() string { d, _ := buildDoc(10, 3000); return d }, nil, "chunked retry failed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFakeReviewer(t)
			f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				var body struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				_ = json.Unmarshal(raw, &body)
				f.mu.Lock()
				f.reqs = append(f.reqs, recordedReq{User: body.Messages[0].Content})
				n := len(f.reqs)
				f.mu.Unlock()
				if n >= 2 {
					http.Error(w, `{"error":{"message":"overloaded"}}`, 529)
					return
				}
				writeAnthropicSSE(w, "cut o", "max_tokens", 1, 1)
			})
			model := "anthropic:test-model"
			if c.lim != nil {
				model = "anthropic:m"
			}
			res := RunPanel(context.Background(), []string{model}, "panel", "q", c.doc(), panelCfg(f, c.lim), map[string]string{"anthropic": "k"})
			r := res[0]
			if !r.Void || r.Answer != "" || r.Err == nil || !strings.Contains(r.Err.Error(), c.want) {
				t.Fatalf("want VOID mentioning %q, got %+v", c.want, r)
			}
			if !AnyVoid(res) {
				t.Error("AnyVoid must report it (refund / workflow unavailable depend on it)")
			}
			out := FormatPanelResult(res)
			if !strings.Contains(out, "NOT COMPLETED") || strings.Contains(strings.ToLower(out), "no blocking findings") {
				t.Errorf("banner missing:\n%s", out)
			}
		})
	}
}

// A reply that is only the citation line is not a review: VOID, not success.
func TestReviewCitationOnlyReplyIsVoid(t *testing.T) {
	for _, lim := range []map[string]ModelLimits{nil, {"anthropic:small": {ContextTokens: 24000}}} {
		f := newFakeReviewer(t)
		f.reply = func(_ int, r recordedReq) (string, string) {
			return "end_turn", "LAST SECTION READ: " + lastSection(r.User)
		}
		doc, _ := buildDoc(40, 3000)
		model := "anthropic:test-model"
		if lim != nil {
			model = "anthropic:small"
		}
		res := RunPanel(context.Background(), []string{model}, "panel", "q", doc, panelCfg(f, lim), map[string]string{"anthropic": "k"})
		if !res[0].Void || res[0].Err == nil || res[0].Answer != "" {
			t.Fatalf("limits=%v: a citation-only reply must be VOID, got %+v", lim, res[0])
		}
		if !AnyVoid(res) || !strings.Contains(FormatPanelResult(res), "NOT COMPLETED") {
			t.Error("citation-only VOID must show NOT COMPLETED")
		}
	}
}

// A later part failing with an ordinary error (timeout, provider error) after
// earlier parts succeeded voids the whole member; partial answers are never merged.
func TestReviewInterruptedChunkedReviewIsVoid(t *testing.T) {
	f := newFakeReviewer(t)
	f.srv.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(raw, &body)
		user := body.Messages[0].Content
		f.mu.Lock()
		f.reqs = append(f.reqs, recordedReq{User: user})
		n := len(f.reqs)
		f.mu.Unlock()
		if n >= 2 {
			http.Error(w, `{"error":{"message":"overloaded"}}`, 529)
			return
		}
		writeAnthropicSSE(w, "finding from part one\nLAST SECTION READ: "+lastSection(user), "end_turn", 1, 1)
	})
	doc, _ := buildDoc(40, 3000)
	lim := map[string]ModelLimits{"anthropic:small": {ContextTokens: 24000}}
	res := RunPanel(context.Background(), []string{"anthropic:small"}, "panel", "q", doc, panelCfg(f, lim), map[string]string{"anthropic": "k"})
	r := res[0]
	if !r.Void || r.Answer != "" || strings.Contains(FormatPanelResult(res), "finding from part one") {
		t.Fatalf("an interrupted chunked review must be VOID with nothing merged, got %+v", r)
	}
	if !strings.Contains(r.Err.Error(), "interrupted at part 2") {
		t.Errorf("reason should say where it was interrupted: %v", r.Err)
	}
}

// finish_reason=length on a document that cannot be chunked (one source block)
// is retried ONCE whole with a larger answer budget — nothing is cut.
func TestReviewLengthOnAtomicSourceRetriesWithLargerBudget(t *testing.T) {
	f := newFakeReviewer(t)
	f.reply = func(_ int, r recordedReq) (string, string) {
		if r.MaxTokens <= 4096 {
			return "max_tokens", "I found that the"
		}
		return "end_turn", "real finding\nLAST SECTION READ: " + lastSection(r.User)
	}
	body := strings.Repeat("func x() {}\n", 4000)
	doc := "── big.diff (4000 lines) ──\n" + body
	lim := map[string]ModelLimits{"anthropic:m": {ContextTokens: 200000}}
	res := RunPanel(context.Background(), []string{"anthropic:m"}, "panel", "q", doc, panelCfg(f, lim), map[string]string{"anthropic": "k"})
	if res[0].Void || res[0].Err != nil || res[0].Answer != "real finding" {
		t.Fatalf("retry with a larger budget should succeed: %+v", res[0])
	}
	reqs := f.requests()
	if len(reqs) != 2 || !strings.Contains(reqs[1].User, body) || reqs[1].MaxTokens <= 4096 {
		t.Fatalf("want 2 whole requests, second with a bigger budget: %d reqs", len(reqs))
	}
	out := FormatPanelResult(res)
	if !strings.Contains(out, "finish_reason: max_tokens,end_turn") && !strings.Contains(out, "finish_reason: end_turn") {
		t.Errorf("report should show finish reasons: %s", out)
	}
	// Still truncated at the larger budget → VOID.
	f2 := newFakeReviewer(t)
	f2.reply = func(int, recordedReq) (string, string) { return "max_tokens", "cut" }
	res = RunPanel(context.Background(), []string{"anthropic:m"}, "panel", "q", doc, panelCfg(f2, lim), map[string]string{"anthropic": "k"})
	if !res[0].Void || len(f2.requests()) != 2 {
		t.Fatalf("still truncated must be VOID after exactly one retry: %+v reqs=%d", res[0], len(f2.requests()))
	}
}
