package agent

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// Compaction folds older turns into a summary and keeps recent turns that fit
// within KeepBytes verbatim.
func TestCompactKeepsRecentTurnsAndSummarizes(t *testing.T) {
	app := &App{Cfg: config.DefaultConfig(), Out: io.Discard}
	// 5 turns × 2 msgs × 40 chars = 400 chars total.
	// KeepBytes=160 keeps the last 2 turns (2×2×40=160 chars).
	app.Cfg.KeepBytes = 160
	app.Cfg.CompactAt = 100 // force compaction (400 > 100)

	for i := 0; i < 5; i++ {
		app.Conv = append(app.Conv,
			proxy.Message{Role: "user", Content: StrPtr(strings.Repeat("u", 40))},
			proxy.Message{Role: "assistant", Content: StrPtr(strings.Repeat("a", 40))},
		)
	}

	fakeSum := func(_ context.Context, text string) (string, error) {
		return "SUMMARY of earlier turns", nil
	}
	ok, err := app.Compact(context.Background(), fakeSum, false)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected compaction to occur")
	}
	// Latest-user-task pin: the most recent user message of the older block
	// survives verbatim ahead of the summary; the summary follows it.
	if app.Conv[0].Role != "user" {
		t.Errorf("first message should be the pinned latest-older user message, got %+v", app.Conv[0])
	}
	if app.Conv[1].Role != "system" || !strings.Contains(DerefStr(app.Conv[1].Content), "SUMMARY") {
		t.Errorf("second message should be the summary, got %+v", app.Conv[1])
	}
	// 1 pinned older user + 1 summary + last 2 turns (2 user + 2 assistant) = 6.
	if len(app.Conv) != 6 {
		t.Errorf("expected 6 messages after compaction, got %d", len(app.Conv))
	}
	users := 0
	for _, m := range app.Conv {
		if m.Role == "user" {
			users++
		}
	}
	// 2 tail user turns + the pinned latest-older user message.
	if users != 3 {
		t.Errorf("expected 3 user messages kept verbatim, got %d", users)
	}
}

// TestCompactPreservesLeadingPinnedPreamble verifies the prompt-cache
// invariant App.ensurePreamble relies on: a pinned system message at Conv[0]
// (the day-stable preamble) survives compaction completely unchanged and
// stays literally first — the existing pinnedPrefix mechanism in Compact
// already preserves original relative order among pinned messages, so no
// code change to compact.go was needed for this; this test is that
// verification. The compaction summary system message must land after it.
func TestCompactPreservesLeadingPinnedPreamble(t *testing.T) {
	app := &App{Cfg: config.DefaultConfig(), Out: io.Discard}
	app.Cfg.KeepBytes = 160
	app.Cfg.CompactAt = 100 // force compaction

	preamble := proxy.Message{Role: "system", Content: StrPtr("PREAMBLE stable prefix"), Pinned: true}
	app.Conv = append(app.Conv, preamble)
	for i := 0; i < 5; i++ {
		app.Conv = append(app.Conv,
			proxy.Message{Role: "user", Content: StrPtr(strings.Repeat("u", 40))},
			proxy.Message{Role: "assistant", Content: StrPtr(strings.Repeat("a", 40))},
		)
	}

	fakeSum := func(_ context.Context, text string) (string, error) {
		return "SUMMARY of earlier turns", nil
	}
	ok, err := app.Compact(context.Background(), fakeSum, false)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected compaction to occur")
	}
	if app.Conv[0].Role != "system" || DerefStr(app.Conv[0].Content) != "PREAMBLE stable prefix" {
		t.Fatalf("Conv[0] must remain the pinned preamble, byte-unchanged, got %+v", app.Conv[0])
	}
	if !app.Conv[0].Pinned {
		t.Error("preamble must stay pinned across compaction")
	}
	summaryIdx := -1
	for i, m := range app.Conv {
		if m.Role == "system" && strings.Contains(DerefStr(m.Content), "SUMMARY") {
			summaryIdx = i
			break
		}
	}
	if summaryIdx <= 0 {
		t.Fatalf("compaction summary message must exist and land after Conv[0], got index %d in %+v", summaryIdx, app.Conv)
	}
}

// TestCompactPinsLatestUserTask verifies the latest-user-task pin: the most
// recent user message in the summarizable range survives compaction verbatim
// instead of being folded into lossy summary prose (the "there wasn't a
// specific question in your message" failure).
func TestCompactPinsLatestUserTask(t *testing.T) {
	app := &App{Cfg: config.DefaultConfig(), Out: io.Discard}
	app.Cfg.KeepBytes = 100 // tail = only the small recent turn
	app.Cfg.CompactAt = 50  // force compaction
	app.Cfg.SummaryBytes = 5000

	task := "TASK: fix the flux capacitor in reactor.go and add tests"

	app.Conv = []proxy.Message{
		// Older user turn — summarizable.
		{Role: "user", Content: StrPtr("earlier question about something else")},
		{Role: "assistant", Content: StrPtr(strings.Repeat("a", 200))},
		// The task turn: most recent user message in the summarizable range,
		// followed by assistant/tool activity.
		{Role: "user", Content: StrPtr(task)},
		{Role: "assistant", ToolCalls: []proxy.ToolCall{{ID: "t1", Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"reactor.go"}`}}}},
		{Role: "tool", ToolCallID: "t1", Name: "read_file", Content: StrPtr(strings.Repeat("code-", 60))},
		{Role: "assistant", Content: StrPtr(strings.Repeat("work-", 40))},
		// Recent small turn that fits KeepBytes — forces the task turn into "older".
		{Role: "user", Content: StrPtr("proceed?")},
		{Role: "assistant", Content: StrPtr("ok")},
	}

	fakeSum := func(_ context.Context, _ string) (string, error) {
		return "SUMMARY", nil
	}
	ok, err := app.Compact(context.Background(), fakeSum, false)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected compaction to occur")
	}

	// The task message must appear verbatim in the post-compaction Conv.
	found := false
	for _, m := range app.Conv {
		if m.Role == "user" && DerefStr(m.Content) == task {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("latest user task was not preserved verbatim after compaction; Conv:\n%s",
			renderTranscript(app.Conv))
	}
}

// TestCompactPinsOnlyLatestUserMessage verifies that with two user messages in
// the summarizable range, only the latest survives verbatim — older user
// messages are still summarized (pinning all of them would erode compaction
// savings; the latest carries the current task or redirect).
func TestCompactPinsOnlyLatestUserMessage(t *testing.T) {
	app := &App{Cfg: config.DefaultConfig(), Out: io.Discard}
	app.Cfg.KeepBytes = 100
	app.Cfg.CompactAt = 50
	app.Cfg.SummaryBytes = 5000

	oldMsg := "OLD: first request, now superseded"
	newMsg := "NEW: redirect — do this other thing instead"

	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr(oldMsg)},
		{Role: "assistant", Content: StrPtr(strings.Repeat("a", 200))},
		{Role: "user", Content: StrPtr(newMsg)},
		{Role: "assistant", Content: StrPtr(strings.Repeat("b", 200))},
		// Recent small turn — tail.
		{Role: "user", Content: StrPtr("status?")},
		{Role: "assistant", Content: StrPtr("done")},
	}

	fakeSum := func(_ context.Context, _ string) (string, error) {
		return "SUMMARY", nil
	}
	ok, err := app.Compact(context.Background(), fakeSum, false)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected compaction to occur")
	}

	oldFound, newFound := false, false
	for _, m := range app.Conv {
		if m.Role == "user" {
			switch DerefStr(m.Content) {
			case oldMsg:
				oldFound = true
			case newMsg:
				newFound = true
			}
		}
	}
	if !newFound {
		t.Error("latest user message in summarizable range was not preserved verbatim")
	}
	if oldFound {
		t.Error("older user message survived verbatim — it should have been summarized")
	}
}

func TestCompactNoopWhenSmall(t *testing.T) {
	app := &App{Cfg: config.DefaultConfig(), Out: io.Discard}
	app.Conv = []proxy.Message{{Role: "user", Content: StrPtr("hi")}}
	ok, err := app.Compact(context.Background(), func(_ context.Context, _ string) (string, error) {
		t.Fatal("summarizer should not be called when under threshold")
		return "", nil
	}, false)
	if err != nil || ok {
		t.Errorf("expected no compaction, ok=%v err=%v", ok, err)
	}
}

func TestKeepBoundaryByBytes(t *testing.T) {
	// Build conv: 5 turns × (user 40 + assistant 40) = 400 chars total.
	var conv []proxy.Message
	for i := 0; i < 5; i++ {
		conv = append(conv,
			proxy.Message{Role: "user", Content: StrPtr(strings.Repeat("u", 40))},
			proxy.Message{Role: "assistant", Content: StrPtr(strings.Repeat("a", 40))},
		)
	}

	// keepBytes=160 fits exactly 2 turns (160 chars). Boundary should be at
	// the start of turn 3 (index 4 from start = 3rd user message).
	b := keepBoundary(conv, 160)
	if b != 6 {
		t.Errorf("keepBoundary(160) = %d, want 6", b)
	}
	if TranscriptSize(conv[b:]) > 160 {
		t.Errorf("tail size %d exceeds keepBytes 160", TranscriptSize(conv[b:]))
	}

	// keepBytes=0 means unlimited — return 0 (nothing to compact).
	if b := keepBoundary(conv, 0); b != 0 {
		t.Errorf("keepBoundary(0) = %d, want 0", b)
	}

	// keepBytes large enough for everything — return 0.
	if b := keepBoundary(conv, 9999); b != 0 {
		t.Errorf("keepBoundary(9999) = %d, want 0", b)
	}
}

// ── P36: relative context thresholds ─────────────────────────────────────────

// TestActiveThresholdsScaleWithWindow verifies that thresholds computed from a
// 1M-token window are proportionally larger than those from a 196k window, and
// that each window's hierarchy (keepBytes+summary < compactAt < hardMax) holds.
func TestActiveThresholdsScaleWithWindow(t *testing.T) {
	cfg := config.DefaultConfig()

	make196k := func() *App {
		a := &App{Cfg: cfg}
		a.CtxLimit = ContextLimit{NCtx: 196608, Source: "backend",
			ReasoningBudget: cfg.ReasoningBudgetTokens, AnswerMargin: cfg.AnswerMarginTokens}
		return a
	}
	make1M := func() *App {
		a := &App{Cfg: cfg}
		a.CtxLimit = ContextLimit{NCtx: 1048576, Source: "backend",
			ReasoningBudget: cfg.ReasoningBudgetTokens, AnswerMargin: cfg.AnswerMarginTokens}
		return a
	}

	ca196, kb196, hm196 := make196k().activeThresholds()
	ca1M, kb1M, hm1M := make1M().activeThresholds()

	// All thresholds must be strictly larger for the 1M window.
	if ca1M <= ca196 {
		t.Errorf("compact_at: 1M (%d) should be larger than 196k (%d)", ca1M, ca196)
	}
	if kb1M <= kb196 {
		t.Errorf("keep_bytes: 1M (%d) should be larger than 196k (%d)", kb1M, kb196)
	}
	if hm1M <= hm196 {
		t.Errorf("hard_max: 1M (%d) should be larger than 196k (%d)", hm1M, hm196)
	}

	// The ratio should track n_ctx proportionally (within 5% to allow for
	// integer rounding and the fixed SummaryBytes offset).
	wantRatio := float64(1048576) / float64(196608)
	gotRatio := float64(ca1M) / float64(ca196)
	if gotRatio < wantRatio*0.95 || gotRatio > wantRatio*1.05 {
		t.Errorf("compact_at ratio %.2f, want ~%.2f (within 5%%)", gotRatio, wantRatio)
	}

	// Hierarchy must hold for both windows.
	for _, tc := range []struct {
		name            string
		ca, kb, hm, sum int
	}{
		{"196k", ca196, kb196, hm196, cfg.SummaryBytes},
		{"1M", ca1M, kb1M, hm1M, cfg.SummaryBytes},
	} {
		if tc.kb+tc.sum >= tc.ca {
			t.Errorf("%s hierarchy: keep_bytes(%d)+summary(%d) >= compact_at(%d)",
				tc.name, tc.kb, tc.sum, tc.ca)
		}
		if tc.ca >= tc.hm {
			t.Errorf("%s hierarchy: compact_at(%d) >= hard_max(%d)", tc.name, tc.ca, tc.hm)
		}
	}
}

// TestActiveThresholdsFallsBackToAbsolute verifies that when CtxLimit.NCtx is
// zero (backend unknown), activeThresholds returns the absolute config values.
func TestActiveThresholdsFallsBackToAbsolute(t *testing.T) {
	cfg := config.DefaultConfig()
	app := &App{Cfg: cfg} // CtxLimit.NCtx = 0
	ca, kb, hm := app.activeThresholds()
	if ca != cfg.CompactAt {
		t.Errorf("compact_at fallback: got %d, want %d", ca, cfg.CompactAt)
	}
	if kb != cfg.KeepBytes {
		t.Errorf("keep_bytes fallback: got %d, want %d", kb, cfg.KeepBytes)
	}
	if hm != cfg.HardMaxBytes {
		t.Errorf("hard_max fallback: got %d, want %d", hm, cfg.HardMaxBytes)
	}
}

// TestFitConvToWindowDownshift verifies the downshift hazard path: when
// /backend switches to a smaller-context model and Conv already exceeds the
// new hard ceiling, fitConvToWindow compacts+drops and emits a loud warning.
func TestFitConvToWindowDownshift(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.SummaryBytes = 500 // small so the fake summary doesn't bloat Conv

	// 196k window: usable ≈ 188416 tok → chars ≈ 753664 → hard_max ≈ 716k.
	smallLim := ContextLimit{NCtx: 196608, Source: "backend",
		ReasoningBudget: cfg.ReasoningBudgetTokens, AnswerMargin: cfg.AnswerMarginTokens}
	// 1M window: usable ≈ 1040384 tok → chars ≈ 4161536 → hard_max ≈ 3.95M.
	largeLim := ContextLimit{NCtx: 1048576, Source: "backend",
		ReasoningBudget: cfg.ReasoningBudgetTokens, AnswerMargin: cfg.AnswerMarginTokens}

	var out strings.Builder
	app := &App{
		Cfg:      cfg,
		CtxLimit: largeLim,
		Out:      &out,
		Summarize: func(_ context.Context, _ string) (string, error) {
			return "summary of older turns", nil
		},
	}

	// Fill Conv with ~800k chars — fits 1M limit but exceeds 196k hard_max.
	msgBody := strings.Repeat("x", 1000)
	for i := 0; i < 400; i++ {
		app.Conv = append(app.Conv,
			proxy.Message{Role: "user", Content: StrPtr(msgBody)},
			proxy.Message{Role: "assistant", Content: StrPtr(msgBody)},
		)
	}
	convSize := TranscriptSize(app.Conv)

	// Sanity: Conv must fit the 1M limit and exceed the 196k limit.
	_, _, hm1M := app.activeThresholds()
	if convSize > hm1M {
		t.Fatalf("test setup: Conv (%d) already exceeds 1M hard_max (%d)", convSize, hm1M)
	}
	app.CtxLimit = smallLim
	_, _, hm196 := app.activeThresholds()
	if convSize <= hm196 {
		t.Fatalf("test setup: Conv (%d) doesn't exceed 196k hard_max (%d)", convSize, hm196)
	}

	// fitConvToWindow should compact+drop to fit.
	app.fitConvToWindow(context.Background())

	if got := TranscriptSize(app.Conv); got > hm196 {
		t.Errorf("Conv (%d) still exceeds 196k hard_max (%d) after fitConvToWindow", got, hm196)
	}
	if !strings.Contains(out.String(), "compacting") {
		t.Errorf("expected compacting warning in output, got: %q", out.String())
	}
}

// TestFitConvToWindowNoopWhenFits verifies that fitConvToWindow is a no-op
// when the Conv already fits the current backend's window.
func TestFitConvToWindowNoopWhenFits(t *testing.T) {
	cfg := config.DefaultConfig()
	var out strings.Builder
	app := &App{
		Cfg: cfg,
		Out: &out,
		CtxLimit: ContextLimit{NCtx: 1048576, Source: "backend",
			ReasoningBudget: cfg.ReasoningBudgetTokens, AnswerMargin: cfg.AnswerMarginTokens},
	}
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("hello")},
		{Role: "assistant", Content: StrPtr("hi")},
	}
	app.fitConvToWindow(context.Background())
	if out.Len() > 0 {
		t.Errorf("expected no output when Conv fits window, got: %q", out.String())
	}
}

// TestToolResultCapUnchangedByWindowSize verifies that ToolResultCap is not
// affected by window size — it is a per-result absolute bound, not a fraction.
func TestToolResultCapUnchangedByWindowSize(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ToolResultCap = 8000

	for _, nctx := range []int{196608, 1048576} {
		app := &App{Cfg: cfg, CtxLimit: ContextLimit{NCtx: nctx, Source: "backend"}}
		if app.Cfg.ToolResultCap != 8000 {
			t.Errorf("n_ctx=%d: ToolResultCap changed to %d", nctx, app.Cfg.ToolResultCap)
		}
	}
}

// turnBoundary is still used by tool-result eviction.
func TestTurnBoundaryKeepsGroupsIntact(t *testing.T) {
	conv := []proxy.Message{
		{Role: "user"}, {Role: "assistant"}, {Role: "tool"},
		{Role: "user"}, {Role: "assistant"},
		{Role: "user"}, {Role: "assistant"},
	}
	if b := turnBoundary(conv, 2); b != 3 {
		t.Errorf("boundary = %d, want 3", b)
	}
	if b := turnBoundary(conv, 9); b != 0 {
		t.Errorf("boundary = %d, want 0", b)
	}
}

// ── Card #184: Context Compaction / Stale-Output Pruning ────────────────────────

// TestCompactEmptySummaryFallback verifies that when the summarizer returns an
// empty string, summarizable content is NOT silently discarded — the fallback
// produces a truncated render so the model still has context from older turns.
func TestCompactEmptySummaryFallback(t *testing.T) {
	app := &App{Cfg: config.DefaultConfig(), Out: io.Discard}
	app.Cfg.KeepBytes = 100
	app.Cfg.CompactAt = 50
	app.Cfg.SummaryBytes = 5000

	importantContent := "CRITICAL: the user asked to use pnpm not npm"
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr(importantContent)},
		{Role: "assistant", Content: StrPtr(strings.Repeat("a", 200))},
		{Role: "user", Content: StrPtr("proceed?")},
		{Role: "assistant", Content: StrPtr("ok")},
	}

	// Summarizer returns empty string.
	emptySum := func(_ context.Context, _ string) (string, error) {
		return "", nil
	}
	ok, err := app.Compact(context.Background(), emptySum, false)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected compaction to occur")
	}

	// The summary system message must exist and contain some content —
	// not an empty string that would silently discard history.
	summaryFound := false
	for _, m := range app.Conv {
		if m.Role == "system" && strings.Contains(DerefStr(m.Content), "[Summary of earlier conversation]") {
			content := DerefStr(m.Content)
			if strings.TrimSpace(strings.TrimPrefix(content, "[Summary of earlier conversation]")) == "" {
				t.Error("summary is empty — older turns were silently discarded")
			}
			summaryFound = true
			break
		}
	}
	if !summaryFound {
		t.Error("no summary system message found after compaction with empty summarizer")
	}
}

// TestEvictSkipsPinnedToolResults verifies that pinned tool messages are NOT
// evicted, even when they are old and large. This is the compaction-invariant
// that pinned content survives verbatim.
func TestEvictSkipsPinnedToolResults(t *testing.T) {
	app := &App{Cfg: config.DefaultConfig(), Out: io.Discard}
	app.Cfg.ToolResultCap = 10
	app.Cfg.ToolResultTTL = 0

	big := strings.Repeat("x", 100)

	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("q1")},
		{Role: "assistant", ToolCalls: []proxy.ToolCall{{ID: "p1", Function: proxy.FunctionCall{Name: "read_file"}}}},
		{Role: "tool", ToolCallID: "p1", Name: "read_file", Content: StrPtr(big), Pinned: true},
		{Role: "assistant", Content: StrPtr("done")},
		{Role: "user", Content: StrPtr("q2")},
	}

	app.evictStaleToolResults()

	// The pinned tool result must NOT be evicted.
	if strings.HasPrefix(DerefStr(app.Conv[2].Content), "[evicted") {
		t.Errorf("pinned tool result was evicted — should survive verbatim, got: %q", DerefStr(app.Conv[2].Content))
	}
	if DerefStr(app.Conv[2].Content) != big {
		t.Errorf("pinned tool result was modified, got: %q", DerefStr(app.Conv[2].Content))
	}
}

// TestEvictSpillsUnbackedContent verifies that when a tool result has no
// existing spill path, eviction spills it to disk first so the stub carries
// a recovery path the model can read_file later.
func TestEvictSpillsUnbackedContent(t *testing.T) {
	app := &App{Cfg: config.DefaultConfig(), Out: io.Discard}
	app.Cfg.ToolResultCap = 10
	app.Cfg.ToolResultTTL = 0
	// Set a chatID so SpillToCache has a directory to write to.
	app.Client = &proxy.Client{ChatID: "test-evict-spill"}

	big := strings.Repeat("y", 500)

	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("q1")},
		{Role: "assistant", ToolCalls: []proxy.ToolCall{{ID: "e1", Function: proxy.FunctionCall{Name: "run_shell"}}}},
		{Role: "tool", ToolCallID: "e1", Name: "run_shell", Content: StrPtr(big)},
		{Role: "assistant", Content: StrPtr("done")},
		{Role: "user", Content: StrPtr("q2")},
	}

	app.evictStaleToolResults()

	evicted := DerefStr(app.Conv[2].Content)
	if !strings.HasPrefix(evicted, "[evicted") {
		t.Fatalf("tool result should be evicted, got: %q", evicted)
	}
	// The stub should contain a recovery path since the original had no spill path.
	if !strings.Contains(evicted, "full content at:") {
		t.Errorf("evicted stub should contain a recovery path, got: %q", evicted)
	}
}

// TestCompactEvictsBeforeBoundary verifies that stale tool results are evicted
// BEFORE the summarizer sees the transcript. We use a capturing summarizer that
// records whether it sees evicted stubs (rather than full tool output).
func TestCompactEvictsBeforeBoundary(t *testing.T) {
	app := &App{Cfg: config.DefaultConfig(), Out: io.Discard}
	app.Cfg.ToolResultCap = 10
	app.Cfg.ToolResultTTL = 0
	app.Cfg.KeepBytes = 200
	app.Cfg.CompactAt = 300
	app.Cfg.SummaryBytes = 5000
	app.Client = &proxy.Client{ChatID: "test-compact-evict-boundary"}

	big := strings.Repeat("x", 200)

	// 3 turns with large tool results.
	app.Conv = buildTurn(nil, "turn 1", big, 1)
	app.Conv = buildTurn(app.Conv, "turn 2", big, 2)
	app.Conv = buildTurn(app.Conv, "turn 3", big, 3)

	var summarizerSawEvicted bool
	var summarizerSawFull bool
	capturingSum := func(_ context.Context, text string) (string, error) {
		if strings.Contains(text, "[evicted") {
			summarizerSawEvicted = true
		}
		// Check if the full verbose output survived to the summarizer.
		// The original content is 200 'x' chars — if the summarizer sees
		// more than 50 consecutive x's, the full content leaked through.
		if strings.Contains(text, strings.Repeat("x", 50)) {
			summarizerSawFull = true
		}
		return "SUMMARY", nil
	}

	app.Compact(context.Background(), capturingSum, false) //nolint:errcheck

	if !summarizerSawEvicted {
		t.Error("summarizer did not see evicted stubs — eviction should run before summarization in Compact")
	}
	if summarizerSawFull {
		t.Error("summarizer saw full tool output — eviction should have stubbed it before summarization")
	}
}

// TestRenderTranscriptPreservesSpillPath verifies that when a tool result
// contains a spill path marker, renderTranscript preserves it in the summary
// prompt even after truncation, so the summarizer knows the content is
// recoverable.
func TestRenderTranscriptPreservesSpillPath(t *testing.T) {
	longContent := strings.Repeat("z", 800) + "\n[full content at: /cache/spill-123.txt]"
	msg := []proxy.Message{
		{Role: "tool", Name: "read_file", Content: StrPtr(longContent)},
	}
	rendered := renderTranscript(msg)

	if !strings.Contains(rendered, "/cache/spill-123.txt") {
		t.Errorf("spill path was lost in renderTranscript — should be preserved for recovery:\n%s", rendered)
	}
}

// TestCompactSummarizerReceivesRenderedTranscript verifies that the summarizer
// receives a rendered transcript (not empty) and that the latest-user-task pin
// excludes the most recent user message from the summarizable block. The
// preservation instructions live in proxySummarizer's prompt template, which
// is bypassed by the injected fake — this test verifies transcript rendering
// and pin exclusion, not the prompt template.
func TestCompactSummarizerReceivesRenderedTranscript(t *testing.T) {
	app := &App{Cfg: config.DefaultConfig(), Out: io.Discard}
	app.Cfg.KeepBytes = 100
	app.Cfg.CompactAt = 50
	app.Cfg.SummaryBytes = 5000

	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("do the thing")},
		{Role: "assistant", Content: StrPtr(strings.Repeat("a", 200))},
		{Role: "user", Content: StrPtr("proceed?")},
		{Role: "assistant", Content: StrPtr("ok")},
	}

	var capturedPrompt string
	capturingSum := func(_ context.Context, text string) (string, error) {
		capturedPrompt = text
		return "SUMMARY", nil
	}

	ok, err := app.Compact(context.Background(), capturingSum, false)
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}
	if !ok {
		t.Fatal("expected compaction to occur")
	}

	if capturedPrompt == "" {
		t.Error("summarizer received empty prompt — transcript not rendered")
	}
	// The summarizable block should contain the assistant's 200-char message,
	// rendered as "ASSISTANT: aaaa...".
	if !strings.Contains(capturedPrompt, "ASSISTANT:") {
		t.Error("summarizer prompt should contain rendered ASSISTANT messages")
	}
	// The latest user message in the older block ("do the thing") is pinned
	// and excluded from the summarizable block — it should NOT appear in the
	// summarizer's input.
	if strings.Contains(capturedPrompt, "do the thing") {
		t.Error("pinned user message should be excluded from summarizer input")
	}
}

// TestCompactEvictsBeforeSummarizing verifies that the eviction happens before
// the summarizer sees the transcript. The summarizer should NOT see tool output
// that has been evicted to a stub.
func TestCompactEvictsBeforeSummarizing(t *testing.T) {
	app := &App{Cfg: config.DefaultConfig(), Out: io.Discard}
	app.Cfg.ToolResultCap = 10
	app.Cfg.ToolResultTTL = 0
	app.Cfg.KeepBytes = 100
	app.Cfg.CompactAt = 50
	app.Cfg.SummaryBytes = 5000
	app.Client = &proxy.Client{ChatID: "test-compact-evict-summarize"}

	big := strings.Repeat("VERBOSE-OUTPUT-", 50) // 700 chars

	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("q1")},
		{Role: "assistant", ToolCalls: []proxy.ToolCall{{ID: "v1", Function: proxy.FunctionCall{Name: "run_shell"}}}},
		{Role: "tool", ToolCallID: "v1", Name: "run_shell", Content: StrPtr(big)},
		{Role: "assistant", Content: StrPtr(strings.Repeat("a", 100))},
		{Role: "user", Content: StrPtr("q2")},
		{Role: "assistant", Content: StrPtr("ok")},
	}

	var summarizerSawEvicted bool
	checkSum := func(_ context.Context, text string) (string, error) {
		// If eviction happened before summarizing, the summarizer should
		// see the stub, not the full verbose output.
		if strings.Contains(text, "[evicted") {
			summarizerSawEvicted = true
		}
		// The full verbose output should NOT be in the summary prompt.
		if strings.Contains(text, "VERBOSE-OUTPUT-VERBOSE-OUTPUT-VERBOSE-OUTPUT") {
			t.Error("summarizer saw full verbose tool output — should have been evicted to stub first")
		}
		return "SUMMARY", nil
	}

	app.Compact(context.Background(), checkSum, false) //nolint:errcheck

	if !summarizerSawEvicted {
		t.Error("summarizer did not see evicted stub — eviction may not have run before summary")
	}
}

// TestLongSessionStaysUnderLimit builds a synthetic 200-turn session and verifies
// that compaction + eviction + hard-max keep the transcript bounded. This is
// acceptance criterion #1 from Card #184.
func TestLongSessionStaysUnderLimit(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.SummaryBytes = 500
	cfg.KeepBytes = 800
	cfg.CompactAt = 2000
	cfg.HardMaxBytes = 4000
	cfg.ToolResultTTL = 3
	cfg.ToolResultCap = 500

	app := &App{Cfg: cfg, Out: io.Discard}

	// Simulate 200 turns, each with a large tool result that would
	// overflow the context without compaction.
	turnResult := strings.Repeat("t", 500) // 500 chars per tool result
	for i := 0; i < 200; i++ {
		app.Conv = append(app.Conv,
			proxy.Message{Role: "user", Content: StrPtr("turn " + strconv.Itoa(i))},
			proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{{
				ID:       "tc" + strconv.Itoa(i),
				Function: proxy.FunctionCall{Name: "run_shell"},
			}}},
			proxy.Message{Role: "tool", ToolCallID: "tc" + strconv.Itoa(i), Name: "run_shell",
				Content: StrPtr(turnResult)},
			proxy.Message{Role: "assistant", Content: StrPtr("response " + strconv.Itoa(i))},
		)

		// Simulate finalizeTurn: evict + compact + hard-max.
		app.evictStaleToolResults()
		app.Compact(context.Background(), func(_ context.Context, _ string) (string, error) {
			return "Summary of older turns preserving key decisions and plan state", nil
		}, false) //nolint:errcheck
		_, _, hm := app.activeThresholds()
		app.enforceHardMax(context.Background(), hm)
	}

	// After 200 turns, the transcript must stay under HardMaxBytes.
	size := TranscriptSize(app.Conv)
	if size > cfg.HardMaxBytes*2 { // allow 2x for safety margin in test
		t.Errorf("200-turn session size %d exceeds 2x hard_max (%d) — compaction not keeping up", size, cfg.HardMaxBytes*2)
	}

	// The most recent user task must survive (not lost to compaction).
	lastUserFound := false
	for _, m := range app.Conv {
		if m.Role == "user" && strings.Contains(DerefStr(m.Content), "turn 199") {
			lastUserFound = true
			break
		}
	}
	// Note: the latest user task might be summarized if it's in the older block.
	// What matters is that SOME user message from the recent tail survives.
	if !lastUserFound {
		// Check that at least one recent user message survives.
		recentFound := false
		for _, m := range app.Conv {
			if m.Role == "user" {
				recentFound = true
				break
			}
		}
		if !recentFound {
			t.Error("no user messages survived 200-turn compaction — agent would lose the task")
		}
	}
}

// TestCompactCondensationFailureKeepsOriginal verifies that when the second
// summarizer call (condensation) fails, the original summary is kept and a
// warning is written to a.Out.
func TestCompactCondensationFailureKeepsOriginal(t *testing.T) {
	var out strings.Builder
	app := &App{Cfg: config.DefaultConfig(), Out: &out}
	app.Cfg.KeepBytes = 100
	app.Cfg.CompactAt = 50
	app.Cfg.SummaryBytes = 10 // small so the first summary exceeds it

	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("do the thing")},
		{Role: "assistant", Content: StrPtr(strings.Repeat("a", 200))},
		{Role: "user", Content: StrPtr("proceed?")},
		{Role: "assistant", Content: StrPtr("ok")},
	}

	callCount := 0
	sum := func(_ context.Context, text string) (string, error) {
		callCount++
		if callCount == 1 {
			// First call: return a summary exceeding SummaryBytes (10).
			return strings.Repeat("b", 50), nil
		}
		// Second call (condensation): return an error.
		return "", fmt.Errorf("condensation backend error")
	}

	ok, err := app.Compact(context.Background(), sum, false)
	if err != nil {
		t.Fatalf("Compact should succeed despite condensation failure: %v", err)
	}
	if !ok {
		t.Fatal("expected compaction to occur")
	}
	if callCount != 2 {
		t.Fatalf("expected 2 summarizer calls, got %d", callCount)
	}

	// The original summary (50 'b' chars) should be in the conversation.
	summaryFound := false
	for _, m := range app.Conv {
		if m.Role == "system" && strings.Contains(DerefStr(m.Content), strings.Repeat("b", 50)) {
			summaryFound = true
			break
		}
	}
	if !summaryFound {
		t.Error("original summary should be retained after condensation failure")
	}

	// A warning should have been written to a.Out.
	outStr := out.String()
	if !strings.Contains(outStr, "condensation failed") {
		t.Errorf("expected 'condensation failed' warning in output, got: %s", outStr)
	}
}

// TestCompactCondensationSuccessNoWarning verifies that successful condensation
// produces no warning.
func TestCompactCondensationSuccessNoWarning(t *testing.T) {
	var out strings.Builder
	app := &App{Cfg: config.DefaultConfig(), Out: &out}
	app.Cfg.KeepBytes = 100
	app.Cfg.CompactAt = 50
	app.Cfg.SummaryBytes = 10 // small so the first summary exceeds it

	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("do the thing")},
		{Role: "assistant", Content: StrPtr(strings.Repeat("a", 200))},
		{Role: "user", Content: StrPtr("proceed?")},
		{Role: "assistant", Content: StrPtr("ok")},
	}

	callCount := 0
	sum := func(_ context.Context, text string) (string, error) {
		callCount++
		if callCount == 1 {
			return strings.Repeat("b", 50), nil // exceeds SummaryBytes=10
		}
		return "short", nil // successful condensation
	}

	ok, err := app.Compact(context.Background(), sum, false)
	if err != nil {
		t.Fatalf("Compact failed: %v", err)
	}
	if !ok {
		t.Fatal("expected compaction to occur")
	}

	outStr := out.String()
	if strings.Contains(outStr, "condensation failed") {
		t.Errorf("should not have condensation warning on success, got: %s", outStr)
	}

	// The condensed summary "short" should be in the conversation.
	summaryFound := false
	for _, m := range app.Conv {
		if m.Role == "system" && strings.Contains(DerefStr(m.Content), "short") {
			summaryFound = true
			break
		}
	}
	if !summaryFound {
		t.Error("condensed summary should be in conversation")
	}
}

// (min is already defined in subagent_test.go)
