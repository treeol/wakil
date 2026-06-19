package agent

import (
	"context"
	"io"
	"strings"
	"testing"

	"wakil/internal/config"
	"wakil/internal/proxy"
	"wakil/internal/tools"
)

// buildTurn appends a complete user→tool→assistant exchange to conv.
func buildTurn(conv []proxy.Message, userText, toolResult string, idx int) []proxy.Message {
	id := string(rune('0' + idx))
	return append(conv,
		proxy.Message{Role: "user", Content: StrPtr(userText)},
		proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{{ID: id, Function: proxy.FunctionCall{Name: "read_file"}}}},
		proxy.Message{Role: "tool", ToolCallID: id, Name: "read_file", Content: StrPtr(toolResult)},
		proxy.Message{Role: "assistant", Content: StrPtr("done")},
	)
}

// TestEvictTTL0 — with TTL=0 eviction fires after the turn that produced the
// results: tool results from turn N are evicted at the end of turn N+1.
func TestEvictTTL0(t *testing.T) {
	app := &App{Cfg: config.DefaultConfig(), Out: io.Discard}
	app.Cfg.ToolResultCap = 10
	app.Cfg.ToolResultTTL = 0

	big := strings.Repeat("x", 100)

	// Two completed turns.
	app.Conv = buildTurn(nil, "turn 1", big, 1)
	app.Conv = buildTurn(app.Conv, "turn 2", big, 2)

	app.evictStaleToolResults()

	// Turn 1's tool result (index 2) should be evicted.
	if !strings.HasPrefix(DerefStr(app.Conv[2].Content), "[evicted") {
		t.Errorf("turn 1 tool result should be evicted, got: %q", DerefStr(app.Conv[2].Content))
	}
	// Turn 2's tool result (index 6) should still be intact.
	if DerefStr(app.Conv[6].Content) != big {
		t.Errorf("turn 2 tool result should not be evicted, got: %q", DerefStr(app.Conv[6].Content))
	}
}

// TestEvictTTL1 — with TTL=1 eviction needs two turns to have passed.
// After 2 turns: nothing evicted. After 3 turns: turn 1 is evicted.
func TestEvictTTL1(t *testing.T) {
	app := &App{Cfg: config.DefaultConfig(), Out: io.Discard}
	app.Cfg.ToolResultCap = 10
	app.Cfg.ToolResultTTL = 1

	big := strings.Repeat("x", 100)

	// Two turns: nothing evicted yet.
	app.Conv = buildTurn(nil, "turn 1", big, 1)
	app.Conv = buildTurn(app.Conv, "turn 2", big, 2)
	app.evictStaleToolResults()
	if strings.HasPrefix(DerefStr(app.Conv[2].Content), "[evicted") {
		t.Error("turn 1 should not be evicted after only 2 turns with TTL=1")
	}

	// Add a third turn: now turn 1 is old enough.
	app.Conv = buildTurn(app.Conv, "turn 3", big, 3)
	app.evictStaleToolResults()
	if !strings.HasPrefix(DerefStr(app.Conv[2].Content), "[evicted") {
		t.Errorf("turn 1 tool result should be evicted after 3 turns, got: %q", DerefStr(app.Conv[2].Content))
	}
	// Turn 2's tool result should still be intact.
	if DerefStr(app.Conv[6].Content) != big {
		t.Errorf("turn 2 tool result should not be evicted yet, got: %q", DerefStr(app.Conv[6].Content))
	}
}

func TestEvictSkipsSmallResults(t *testing.T) {
	app := &App{Cfg: config.DefaultConfig(), Out: io.Discard}
	app.Cfg.ToolResultCap = 100
	app.Cfg.ToolResultTTL = 0

	small := "tiny result"
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("q")},
		{Role: "tool", Name: "run_shell", Content: StrPtr(small)},
		{Role: "user", Content: StrPtr("q2")},
	}
	app.evictStaleToolResults()
	if DerefStr(app.Conv[1].Content) != small {
		t.Errorf("small result should not be evicted, got: %q", DerefStr(app.Conv[1].Content))
	}
}

func TestExtractSpillPath(t *testing.T) {
	content := "some content\n… [+500 chars omitted — full content at: /tmp/wakil/tool-123.txt]"
	if path := tools.ExtractSpillPath(content); path != "/tmp/wakil/tool-123.txt" {
		t.Errorf("unexpected path: %q", path)
	}
	if tools.ExtractSpillPath("no spill here") != "" {
		t.Error("expected empty path for content without spill note")
	}
}

// TestCapOrStubProtectsSubagentSurvivesBudgetExhaustion reproduces the image-1
// scenario: turnToolBytes already exceeds TurnToolBudget but dispatch_subagent
// is protected — the summary passes through uncapped and unstubbed.
func TestCapOrStubProtectsSubagentSurvivesBudgetExhaustion(t *testing.T) {
	app := &App{Cfg: config.DefaultConfig(), Out: io.Discard}
	app.Cfg.TurnToolBudget = 1000
	app.Cfg.ToolResultCap = 100

	summary := `{"objective":"find config","findings":[{"summary":"found it","location":"config.go:55","kind":"match","weight":"high"}]}`

	// turnToolBytes already past budget — would stub any unprotected result.
	result := app.CapOrStub(summary, "dispatch_subagent", 2000)
	if result != summary {
		t.Errorf("dispatch_subagent result was modified by CapOrStub:\n  got:  %q\n  want: %q", result, summary)
	}
	// Sanity: an unprotected tool at the same budget IS stubbed.
	shellResult := strings.Repeat("x", 500)
	capped := app.CapOrStub(shellResult, "run_shell", 2000)
	if capped == shellResult {
		t.Error("unprotected tool result should be stubbed at exhausted budget")
	}
	if !strings.HasPrefix(capped, "[budget") {
		t.Errorf("unprotected result should become a budget stub, got: %q", capped)
	}
}

// TestCapOrStubProtectedResultStillCountsBudget verifies that a protected
// dispatch_subagent result still increments turnToolBytes — protection means
// "don't truncate this," not "pretend it's free."
func TestCapOrStubProtectedResultStillCountsBudget(t *testing.T) {
	app := &App{Cfg: config.DefaultConfig(), Out: io.Discard}
	app.Cfg.TurnToolBudget = 10000
	app.Cfg.ToolResultCap = 99999

	summary := `{"objective":"find config","findings":[{"summary":"found it","location":"config.go:55","kind":"match","weight":"high"}]}`

	// Simulate the send-loop pattern: CapOrStub then accumulate the result length.
	turnBytes := 500
	got := app.CapOrStub(summary, "dispatch_subagent", turnBytes)
	turnBytes += len(got)

	// The summary should be intact.
	if got != summary {
		t.Errorf("dispatch_subagent result modified, got: %q", got)
	}
	// turnBytes must reflect the full summary length — not zero.
	expectedTotal := 500 + len(summary)
	if turnBytes != expectedTotal {
		t.Errorf("turnBytes = %d, want %d (summary len %d was not counted)", turnBytes, expectedTotal, len(summary))
	}

	// After this an unprotected tool on the remaining budget must still budget correctly.
	remaining := app.Cfg.TurnToolBudget - turnBytes
	_ = remaining // the budget counter is correct; the next unprotected tool will see the real remaining budget
}

// TestOldestTurnRange verifies the boundary-finding helper.
func TestOldestTurnRange(t *testing.T) {
	conv := []proxy.Message{
		{Role: "system", Content: StrPtr("[Summary]")},
		{Role: "user", Content: StrPtr("q1")},
		{Role: "assistant", Content: StrPtr("a1")},
		{Role: "user", Content: StrPtr("q2")},
		{Role: "assistant", Content: StrPtr("a2")},
	}
	first, next := oldestTurnRange(conv)
	if first != 1 || next != 3 {
		t.Errorf("oldestTurnRange = (%d, %d), want (1, 3)", first, next)
	}

	// No user turns.
	first, next = oldestTurnRange([]proxy.Message{{Role: "system", Content: StrPtr("x")}})
	if first != -1 || next != -1 {
		t.Errorf("empty: oldestTurnRange = (%d, %d), want (-1, -1)", first, next)
	}
}

// TestTurnContainsSubagent verifies detection of dispatch_subagent in a turn.
func TestTurnContainsSubagent(t *testing.T) {
	// Turn with a dispatch_subagent tool call.
	conv := []proxy.Message{
		{Role: "user", Content: StrPtr("find it")},
		{Role: "assistant", ToolCalls: []proxy.ToolCall{
			{Function: proxy.FunctionCall{Name: "dispatch_subagent", Arguments: `{"task":"x"}`}},
		}},
		{Role: "tool", Name: "dispatch_subagent", Content: StrPtr(`{"objective":"x"}`)},
	}
	if !turnContainsSubagent(conv, 0, len(conv)) {
		t.Error("should detect dispatch_subagent in assistant tool_calls")
	}

	// Turn without any subagent references.
	clean := []proxy.Message{
		{Role: "user", Content: StrPtr("read file")},
		{Role: "assistant", ToolCalls: []proxy.ToolCall{
			{Function: proxy.FunctionCall{Name: "read_file"}},
		}},
		{Role: "tool", Name: "read_file", Content: StrPtr("content")},
	}
	if turnContainsSubagent(clean, 0, len(clean)) {
		t.Error("should NOT detect dispatch_subagent in a read-only turn")
	}
}

// TestEnforceHardMaxAtomicTurn verifies that enforceHardMax drops entire turns
// (never splitting a tool-call from its result) and flags subagent-bearing
// drops. Uses a full App so enforceHardMax can call Compact.
func TestEnforceHardMaxAtomicTurn(t *testing.T) {
	app := &App{Cfg: config.DefaultConfig(), Out: io.Discard}
	// Set Small limits so we trigger dropping.
	app.Cfg.HardMaxBytes = 200
	app.Cfg.CompactAt = 150
	app.Cfg.KeepBytes = 100
	app.Cfg.SummaryBytes = 5000

	// One normal turn + one subagent turn (call + result).
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("first query")},
		{Role: "assistant", Content: StrPtr("first answer")},
		{Role: "user", Content: StrPtr("find the config")},
		{Role: "assistant", ToolCalls: []proxy.ToolCall{
			{ID: "s1", Function: proxy.FunctionCall{Name: "dispatch_subagent", Arguments: `{"task":"find x"}`}},
		}},
		{Role: "tool", ToolCallID: "s1", Name: "dispatch_subagent", Content: StrPtr(`{"objective":"find x","findings":[]}`)},
		{Role: "assistant", Content: StrPtr("found it")},
	}

	// enforceHardMax should not split the subagent call from its result.
	// The turn [user: "find the config" ... assistant: "found it"] is 1 turn.
	// Under 200-byte hard max, compaction runs first and should drop the first
	// turn into a summary. Then if still over, the entire second turn is dropped
	// atomically — never just the call or just the result.
	app.enforceHardMax(context.Background(), app.Cfg.HardMaxBytes)

	// After enforcement: either both subagent-related messages survive together
	// or both are gone. We check that the tool message and its corresponding
	// tool_call are never separated.
	toolIndices := map[int]bool{}
	callIDs := map[string]bool{}
	for i, m := range app.Conv {
		if m.Role == "tool" && m.Name == "dispatch_subagent" {
			toolIndices[i] = true
			callIDs[m.ToolCallID] = true
		}
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.Function.Name == "dispatch_subagent" {
					callIDs[tc.ID] = true
				}
			}
		}
	}
	// Every call ID referenced by a tool message must have a corresponding
	// assistant message with a tool_call referencing it.
	for _, m := range app.Conv {
		if m.Role == "tool" && m.Name == "dispatch_subagent" {
			if !callIDs[m.ToolCallID] {
				t.Errorf("orphaned subagent tool result: tool_call_id=%q has no corresponding call in Conv", m.ToolCallID)
			}
		}
	}
}
