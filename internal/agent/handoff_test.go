package agent

import (
	"context"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

func testHandoffApp() *App {
	return &App{
		Cfg:                          config.DefaultConfig(),
		Client:                       &proxy.Client{ChatID: "test-chat-id-aaaa"},
		Session:                      &Session{ChatID: "test-chat-id-aaaa"},
		EffectiveCtxMaxCharsOverride: -1,
	}
}

func TestPerformHandoffEmptyConv(t *testing.T) {
	app := testHandoffApp()
	msg := performHandoff(context.Background(), app)
	hm, ok := msg.(HandoffMsg)
	if !ok {
		t.Fatalf("expected HandoffMsg, got %T", msg)
	}
	if hm.Err == nil || !strings.Contains(hm.Err.Error(), "nothing to hand off") {
		t.Errorf("empty conv should error; got %v", hm.Err)
	}
}

func TestPerformHandoffNoUserMessages(t *testing.T) {
	app := testHandoffApp()
	app.Conv = []proxy.Message{
		{Role: "system", Content: StrPtr("preamble")},
	}
	msg := performHandoff(context.Background(), app)
	hm, ok := msg.(HandoffMsg)
	if !ok {
		t.Fatalf("expected HandoffMsg, got %T", msg)
	}
	if hm.Err == nil || !strings.Contains(hm.Err.Error(), "no user messages") {
		t.Errorf("system-only conv should error; got %v", hm.Err)
	}
}

func TestBuildContinuationPromptContains(t *testing.T) {
	prompt := BuildContinuationPrompt("test summary", "abc12345-aaaa", "/workspace")
	if !strings.Contains(prompt, "abc12345") {
		t.Error("should contain short chat ID")
	}
	if !strings.Contains(prompt, "/workspace") {
		t.Error("should contain workspace")
	}
	if !strings.Contains(prompt, "test summary") {
		t.Error("should contain summary")
	}
	if !strings.Contains(prompt, "untrusted") {
		t.Error("should delimit as untrusted")
	}
}

func TestEffectiveCtxCapOverrideTakesPrecedence(t *testing.T) {
	app := testHandoffApp()
	app.EffectiveCtxMaxCharsOverride = 200000
	app.Cfg.EffectiveCtxMaxChars = 150000
	if got := app.EffectiveCtxCap(); got != 200000 {
		t.Errorf("override should take precedence; got %d", got)
	}
}

func TestEffectiveCtxCapFallsBackToConfig(t *testing.T) {
	app := testHandoffApp()
	app.EffectiveCtxMaxCharsOverride = -1 // not set
	app.Cfg.EffectiveCtxMaxChars = 150000
	if got := app.EffectiveCtxCap(); got != 150000 {
		t.Errorf("should use config value; got %d", got)
	}
}

func TestEffectiveCtxCapDisabled(t *testing.T) {
	app := testHandoffApp()
	app.EffectiveCtxMaxCharsOverride = 0 // explicitly disabled
	if got := app.EffectiveCtxCap(); got != 0 {
		t.Errorf("disabled should return 0; got %d", got)
	}
}

func TestEffectiveCtxCapNoOverrideNoConfig(t *testing.T) {
	app := testHandoffApp()
	app.EffectiveCtxMaxCharsOverride = -1 // not set
	// Cfg.EffectiveCtxMaxChars defaults to 0 (disabled)
	if got := app.EffectiveCtxCap(); got != 0 {
		t.Errorf("no override + no config should return 0; got %d", got)
	}
}

func TestActiveThresholdsAppliesCap(t *testing.T) {
	app := testHandoffApp()
	app.CtxLimit = ContextLimit{NCtx: 1000000} // 1M token model
	app.Cfg.CompactAtFrac = 0.75
	app.Cfg.KeepBytesFrac = 0.60
	app.Cfg.HardMaxFrac = 0.95
	app.Cfg.ContextCapacityFrac = 0.80
	app.Cfg.SummaryBytes = 20000

	// Without cap: effectiveChars = 1M * 0.80 * 4 = 3.2M → compactAt ~2.4M
	compactAt, _, hardMax := app.activeThresholds()
	if compactAt < 2000000 {
		t.Errorf("without cap, compactAt should be ~2.4M; got %d", compactAt)
	}

	// With cap at 200k: effectiveChars = min(3.2M, 200k) = 200k → compactAt ~150k
	app.EffectiveCtxMaxCharsOverride = 200000
	compactAt, _, hardMax = app.activeThresholds()
	if compactAt > 200000 {
		t.Errorf("with cap=200k, compactAt should be ~150k; got %d", compactAt)
	}
	if hardMax > 250000 {
		t.Errorf("with cap=200k, hardMax should be ~190k; got %d", hardMax)
	}
}
