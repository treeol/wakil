package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// newSimApp builds a test App with all ctx-management machinery wired up but
// no real proxy — the injected summarizer returns a fixed-size response.
func newSimApp(summarySize int) *App {
	fakeSum := func(_ context.Context, _ string) (string, error) {
		return strings.Repeat("S", summarySize), nil
	}
	app := &App{
		Cfg:       config.DefaultConfig(),
		Out:       io.Discard,
		Summarize: fakeSum,
		Session:   &Session{ChatID: ""},
	}
	return app
}

// simTurn appends one complete user turn to app.Conv (tool reads + assistant
// response) applying the capOrStub budget gate, then runs the full post-turn
// pipeline: compact → evict → enforceHardMax.
// Returns (sizeBefore, sizeAfter, compacted).
func simTurn(app *App, rawTools bool, toolCount, toolSize, assistantSize int) (before, after int, compacted bool) {
	ctx := context.Background()
	prevRaw := app.RawTools
	app.RawTools = rawTools
	defer func() { app.RawTools = prevRaw }()

	app.Conv = append(app.Conv, proxy.Message{Role: "user", Content: StrPtr("query")})

	var turnToolBytes int
	for i := 0; i < toolCount; i++ {
		raw := strings.Repeat("x", toolSize)
		id := fmt.Sprintf("tc%d", i)
		app.Conv = append(app.Conv, proxy.Message{
			Role:      "assistant",
			ToolCalls: []proxy.ToolCall{{ID: id, Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"/f"}`}}},
		})
		var capped string
		if rawTools {
			capped = raw
		} else {
			capped = app.CapOrStub(raw, "read_file", turnToolBytes)
		}
		turnToolBytes += len(capped)
		app.Conv = append(app.Conv, proxy.Message{
			Role: "tool", ToolCallID: id, Name: "read_file", Content: StrPtr(capped),
		})
	}
	app.Conv = append(app.Conv, proxy.Message{Role: "assistant", Content: StrPtr(strings.Repeat("r", assistantSize))})

	before = TranscriptSize(app.Conv)
	compacted, _ = app.Compact(ctx, app.summarizeFn(), false)
	app.evictStaleToolResults()
	app.enforceHardMax(ctx, app.Cfg.HardMaxBytes)
	after = TranscriptSize(app.Conv)
	return
}

// checkCoherence verifies the transcript is structurally valid after hard drops:
// starts with system or user, and every tool message has a matching tool_call
// in a preceding assistant message within the same slice.
func checkCoherence(t *testing.T, conv []proxy.Message) {
	t.Helper()
	if len(conv) == 0 {
		return
	}
	if conv[0].Role != "system" && conv[0].Role != "user" {
		t.Errorf("transcript starts with %q, want system or user", conv[0].Role)
	}
	seen := map[string]bool{}
	for _, m := range conv {
		for _, tc := range m.ToolCalls {
			seen[tc.ID] = true
		}
		if m.Role == "tool" && m.ToolCallID != "" && !seen[m.ToolCallID] {
			t.Errorf("orphaned tool message: ToolCallID %q has no preceding tool_call", m.ToolCallID)
		}
	}
}

// --- Test 1: baseline 20-file scan + 10 large turns (unchanged from part-1) ---

func TestCtxHardMaxInvariant(t *testing.T) {
	app := newSimApp(15000)

	doTurn := func(label string, toolCount, toolSize, assistantSize int) {
		before, after, compacted := simTurn(app, false, toolCount, toolSize, assistantSize)
		status := "ok"
		if compacted {
			status = "compacted"
		}
		t.Logf("%-32s  tools=%2d×%dk  before=%6dk  after=%5dk  [%s]",
			label, toolCount, toolSize/1000, before/1000, after/1000, status)
		if after > app.Cfg.HardMaxBytes {
			t.Errorf("%s: ctx %d exceeds hard max %d", label, after, app.Cfg.HardMaxBytes)
		}
	}

	doTurn("turn 1 — 20-file scan", 20, 15000, 2000)
	for i := 2; i <= 11; i++ {
		doTurn(fmt.Sprintf("turn %2d — 3 reads + 5k", i), 3, 15000, 5000)
	}
	t.Logf("final ctx: %dk / hard max %dk",
		TranscriptSize(app.Conv)/1000, app.Cfg.HardMaxBytes/1000)
}

// --- Test 2: 200-file scan showing per-turn budget hard-stop ---

func TestTurnBudgetHardStop200Files(t *testing.T) {
	app := newSimApp(15000)

	// Single turn: 200 files × 15k raw each.
	// Budget=40k: files 1-5 get 8k cap (5×8k=40k), files 6-200 get ~50-char stubs.
	before, after, _ := simTurn(app, false, 200, 15000, 2000)

	// Count how many results are stubs vs. capped.
	stubs, capped := 0, 0
	for _, m := range app.Conv {
		if m.Role != "tool" {
			continue
		}
		if strings.HasPrefix(DerefStr(m.Content), "[budget") {
			stubs++
		} else {
			capped++
		}
	}

	t.Logf("200-file turn: before=%dk  after=%dk", before/1000, after/1000)
	t.Logf("  capped (8k slice): %d files", capped)
	t.Logf("  stubbed (50-char):  %d files", stubs)
	t.Logf("  total tool output in ctx: ~%dk  (would be ~1600k without budget gate)",
		capped*8+stubs/1000) // rough

	if stubs < 190 {
		t.Errorf("expected ≥190 stub results, got %d", stubs)
	}
	if after > app.Cfg.HardMaxBytes {
		t.Errorf("ctx %d exceeds hard max %d", after, app.Cfg.HardMaxBytes)
	}
	checkCoherence(t, app.Conv)
}

// --- Test 3: force enforceHardMax to be the active layer ---

func TestEnforceHardMaxFires(t *testing.T) {
	// Scenario A: 5 normal turns to build history + summary, then one
	// RawTools turn with 15 files × 15k each (225k raw) that defeats all
	// earlier layers and forces enforceHardMax to drop the huge turn.
	t.Run("rawtools_huge_turn_after_history", func(t *testing.T) {
		var out bytes.Buffer
		app := newSimApp(15000)
		app.Out = &out

		for i := 0; i < 5; i++ {
			simTurn(app, false, 3, 15000, 3000)
		}
		sizeBefore5 := TranscriptSize(app.Conv)
		out.Reset() // only capture output of the final turn

		before, after, _ := simTurn(app, true, 15, 15000, 3000)
		warning := out.String()

		t.Logf("after 5 normal turns: ctx=%dk", sizeBefore5/1000)
		t.Logf("huge rawtools turn:   before=%dk  after=%dk  hard_max=%dk",
			before/1000, after/1000, app.Cfg.HardMaxBytes/1000)
		t.Logf("warning output:\n%s", warning)

		if after > app.Cfg.HardMaxBytes {
			t.Errorf("ctx %d exceeds hard max %d — enforceHardMax failed", after, app.Cfg.HardMaxBytes)
		}
		if !strings.Contains(warning, "hard-max shed") {
			t.Error("expected hard-max warning in output, got none")
		}
		if !strings.Contains(warning, "ctx was") {
			t.Error("expected ctx-was size in warning")
		}
		checkCoherence(t, app.Conv)
	})

	// Scenario B: single fresh RawTools turn of 200k (no prior history).
	// No summary exists; enforceHardMax drops the turn entirely.
	// Warning must note that the transcript is now empty.
	t.Run("rawtools_single_fresh_turn", func(t *testing.T) {
		var out bytes.Buffer
		app := newSimApp(15000)
		app.Out = &out

		before, after, _ := simTurn(app, true, 14, 15000, 2000) // 14×15k = 210k raw
		warning := out.String()

		t.Logf("fresh 210k rawtools turn: before=%dk  after=%dk  hard_max=%dk",
			before/1000, after/1000, app.Cfg.HardMaxBytes/1000)
		t.Logf("warning output:\n%s", warning)

		if after > app.Cfg.HardMaxBytes {
			t.Errorf("ctx %d exceeds hard max %d — enforceHardMax failed", after, app.Cfg.HardMaxBytes)
		}
		if !strings.Contains(warning, "hard-max shed") {
			t.Error("expected hard-max warning in output, got none")
		}
		if !strings.Contains(warning, "empty") && !strings.Contains(warning, "summary") {
			t.Error("expected transcript-state note (empty or summary-only) in warning")
		}
		checkCoherence(t, app.Conv)
	})
}
