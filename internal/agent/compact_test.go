package agent

import (
	"context"
	"io"
	"strings"
	"testing"

	"wakil/internal/config"
	"wakil/internal/proxy"
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
	if app.Conv[0].Role != "system" || !strings.Contains(DerefStr(app.Conv[0].Content), "SUMMARY") {
		t.Errorf("first message should be the summary, got %+v", app.Conv[0])
	}
	// 1 summary + last 2 turns (2 user + 2 assistant) = 5 messages.
	if len(app.Conv) != 5 {
		t.Errorf("expected 5 messages after compaction, got %d", len(app.Conv))
	}
	users := 0
	for _, m := range app.Conv {
		if m.Role == "user" {
			users++
		}
	}
	if users != 2 {
		t.Errorf("expected 2 user turns kept verbatim, got %d", users)
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
		name             string
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
