package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/memory"
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
	msg := performHandoff(context.Background(), app, false)
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
	msg := performHandoff(context.Background(), app, false)
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

func TestBuildHandoffContextContains(t *testing.T) {
	ctx := BuildHandoffContext("test summary", "abc12345-aaaa", "/workspace")
	if !strings.Contains(ctx, "abc12345") {
		t.Error("should contain short chat ID")
	}
	if !strings.Contains(ctx, "/workspace") {
		t.Error("should contain workspace")
	}
	if !strings.Contains(ctx, "test summary") {
		t.Error("should contain summary")
	}
	if !strings.Contains(ctx, "untrusted") {
		t.Error("should delimit as untrusted")
	}
	// Stop mode must NOT instruct the model to proceed — the user drives.
	if strings.Contains(ctx, "proceed with the next action") {
		t.Error("stop-mode context should NOT contain 'proceed with the next action'")
	}
	if strings.Contains(ctx, "Continue where the previous session left off") {
		t.Error("stop-mode context should NOT instruct continuation")
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
	compactAt, _, _ := app.activeThresholds()
	if compactAt < 2000000 {
		t.Errorf("without cap, compactAt should be ~2.4M; got %d", compactAt)
	}

	// With cap at 200k: effectiveChars = min(3.2M, 200k) = 200k → compactAt ~150k
	app.EffectiveCtxMaxCharsOverride = 200000
	compactAt, _, hardMax := app.activeThresholds()
	if compactAt > 200000 {
		t.Errorf("with cap=200k, compactAt should be ~150k; got %d", compactAt)
	}
	if hardMax > 250000 {
		t.Errorf("with cap=200k, hardMax should be ~190k; got %d", hardMax)
	}
}

// TestStoreHandoffRecordRetrievable is a regression test for two bugs in
// storeHandoffRecord:
//
//  1. TTL units: the record was written with expiresAt in Unix seconds while
//     the memory store clock is Unix milliseconds, so the 7-day record was
//     filtered as already-expired the instant it was written.
//  2. Stale anchor: the workspace directory was passed as an anchor, and
//     computeAnchorHashes os.ReadFile's each anchor — a directory fails, so
//     the record carried a permanently-stale anchor.
func TestStoreHandoffRecordRetrievable(t *testing.T) {
	// Isolate the sidecar JSON that storeHandoffRecord always writes.
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())

	// Real memory store rooted at a temp workspace.
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(wsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := memory.Open(filepath.Join(dir, "memory", "test.db"), wsRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	app := testHandoffApp()
	app.Client.Model = "test-model"
	app.MemoryStore = store

	oldChatID := "chat-old-123"
	newChatID := "chat-new-456"
	warnings := storeHandoffRecord(context.Background(), app,
		"summary text", "continuation prompt", oldChatID, newChatID, wsRoot)
	for _, w := range warnings {
		t.Errorf("unexpected warning: %s", w)
	}

	// The record must be retrievable immediately — before the fix, the
	// seconds-vs-ms TTL bug made Get filter it as already expired.
	got, err := store.Get(context.Background(), "handoff/"+oldChatID)
	if err != nil {
		t.Fatalf("handoff record not retrievable immediately after write (expired instantly?): %v", err)
	}
	if got.ExpiresAt == nil {
		t.Fatal("expected a 7-day expiresAt to be set")
	}
	// The expiry must be ~7 days out in milliseconds, not a 1970-era seconds
	// value: now(ms) < expiresAt <= now(ms)+7d.
	nowMs := time.Now().UnixMilli()
	if *got.ExpiresAt <= nowMs {
		t.Fatalf("expiresAt %d is in the past (now %d) — TTL unit bug", *got.ExpiresAt, nowMs)
	}
	sevenDaysMs := int64(7 * 24 * time.Hour / time.Millisecond)
	if *got.ExpiresAt > nowMs+sevenDaysMs+int64(time.Minute/time.Millisecond) {
		t.Fatalf("expiresAt %d is beyond 7 days (now %d)", *got.ExpiresAt, nowMs)
	}

	// The record must carry no anchor at all (a workspace directory is not a
	// content anchor) — and therefore no stale anchor. Handoff records are
	// write-only audit artifacts (nothing reads them back via anchor-scoped
	// recall), so nil anchors is correct.
	if got.TotalAnchors != 0 {
		t.Fatalf("expected 0 anchors, got %d", got.TotalAnchors)
	}
	if got.StaleAnchors != 0 {
		t.Fatalf("expected 0 stale anchors, got %d of %d", got.StaleAnchors, got.TotalAnchors)
	}

	// The sidecar JSON is the always-written audit artifact — assert it exists
	// and round-trips the handoff fields.
	sidecar, err := os.ReadFile(filepath.Join(os.Getenv("WAKIL_SESSIONS_DIR"), oldChatID+".handoff.json"))
	if err != nil {
		t.Fatalf("sidecar not written: %v", err)
	}
	var rec handoffRecord
	if err := json.Unmarshal(sidecar, &rec); err != nil {
		t.Fatalf("sidecar not valid JSON: %v", err)
	}
	if rec.OldChatID != oldChatID || rec.NewChatID != newChatID {
		t.Errorf("sidecar chat IDs = %q→%q, want %q→%q", rec.OldChatID, rec.NewChatID, oldChatID, newChatID)
	}
	if rec.Summary != "summary text" || rec.Prompt != "continuation prompt" {
		t.Errorf("sidecar summary/prompt mismatch: %+v", rec)
	}
	if rec.Model != "test-model" {
		t.Errorf("sidecar model = %q, want test-model", rec.Model)
	}
}
