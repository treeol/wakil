package agent

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// --- A2: stale partial-collapse records across resume ----------------------
//
// A mid-turn partial collapse whose turn never finished normally (crash,
// restart) is restored verbatim by RestoreTurnState. The design self-heals:
// a restored partial whose turn HAS a clean answer is upgraded to a full
// collapse by the next collapseFinishedTurns sweep (partial records bypass
// the Rule-3 size check precisely so they always upgrade when eligible).
// A turn that ended dirty (tool calls with no results) can NEVER be
// collapsed to U+A — that would orphan tool calls and violate the
// projection invariant — so it passes through carrying its checkpoint
// block by design.

// TestResumePartialUpgradesOnNextSweep: a partial record restored from a
// saved session upgrades to a full collapse at the next sweep, ending the
// checkpoint-block carryover.
func TestResumePartialUpgradesOnNextSweep(t *testing.T) {
	// Session 1: turn 1 gets a real partial collapse mid-turn.
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)

	// Build a turn with a mid-turn tool-call boundary (assistant w/ tool
	// calls + result + clean final answer), like handleCollapseContext sees.
	conv := oneCollapsedTurn(1)
	for i := range conv {
		conv[i].Turn = 1
	}
	app.Conv = conv
	app.turnSeq = 1

	// Simulate the mid-turn partial: boundary after the first tool result.
	call := proxy.ToolCall{ID: "ccA2", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"mid-turn checkpoint"}`}}
	buildStashTurn(t, app, call)
	if res := app.handleCollapseContext(call); strings.HasPrefix(res, "ERROR") {
		t.Fatalf("partial collapse failed: %s", res)
	}
	app.stateMu.RLock()
	partRec, ok := app.collapsed[1]
	app.stateMu.RUnlock()
	if !ok || !partRec.Partial {
		t.Fatal("precondition: turn 1 should hold a partial record")
	}

	// Session 2 (resume): fresh app, state restored from the "saved" record.
	app2 := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app2.SetContextMode("long-session")
	app2.MemoryStore = app.MemoryStore
	app2.Conv = append([]proxy.Message{}, conv...)
	saved := &Session{Collapsed: []CollapseRecord{partRec}}
	app2.RestoreTurnState(saved)

	// The next sweep (end of the first new request) must upgrade the
	// restored partial to a full collapse.
	app2.collapseFinishedTurns()

	app2.stateMu.RLock()
	rec, ok := app2.collapsed[1]
	app2.stateMu.RUnlock()
	if !ok {
		t.Fatal("turn 1 record vanished after sweep")
	}
	if rec.Partial {
		t.Error("restored partial was NOT upgraded to a full collapse — checkpoint block carries forever")
	}
}

// TestResumePartialDirtyEndNeverCollapses: a partial record whose turn
// ended dirty (last message is an unanswered tool-call batch) must
// never upgrade — collapsing it would orphan tool calls. The partial
// elision + checkpoint block pass-through is the correct steady state.
func TestResumePartialDirtyEndNeverCollapses(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.MemoryStore = testStore(t)

	// Turn 1: U → tool round → collapse_context boundary assistant →
	// (tools continue) → dirty end: an assistant tool-call batch whose
	// results never arrived (crash/interrupt mid-tools).
	call := proxy.ToolCall{ID: "ccA2d", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"mid-turn checkpoint"}`}}
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("investigate the flow"), Turn: 1},
		tcMsg("tcA", "read_file", `{"path":"a.go"}`),
		trMsg("tcA", "read_file", bigContent),
		{Role: "assistant", ToolCalls: []proxy.ToolCall{call}, Turn: 1},
		tcMsg("tcB", "search_files", `{"pattern":"x","path":"."}`),
		trMsg("tcB", "search_files", bigContent),
		{Role: "assistant", Turn: 1, ToolCalls: []proxy.ToolCall{
			{ID: "tcX", Type: "function", Function: proxy.FunctionCall{Name: "run_shell", Arguments: `{"command":"x"}`}},
		}},
	}
	app.turnSeq = 1
	app.curTurn = 1

	if res := app.handleCollapseContext(call); strings.HasPrefix(res, "ERROR") {
		t.Fatalf("partial collapse failed: %s", res)
	}
	app.stateMu.RLock()
	rec, hasRec := app.collapsed[1]
	app.stateMu.RUnlock()
	if !hasRec || !rec.Partial {
		t.Fatal("precondition: turn 1 should hold a partial record")
	}

	// Resume: fresh app, state restored from the "saved" record.
	app2 := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app2.SetContextMode("long-session")
	app2.MemoryStore = app.MemoryStore
	app2.Conv = append([]proxy.Message{}, app.Conv...)
	saved := &Session{Collapsed: []CollapseRecord{rec}}
	app2.RestoreTurnState(saved)

	// Sweeps must never upgrade the dirty-ended partial.
	app2.collapseFinishedTurns()
	app2.collapseFinishedTurns()

	app2.stateMu.RLock()
	rec2, ok := app2.collapsed[1]
	app2.stateMu.RUnlock()
	if !ok || !rec2.Partial {
		t.Fatal("dirty-ended partial record must stay partial (collapsing would orphan tool calls)")
	}

	// The projection must pass the dirty tail through verbatim (the
	// orphaned tool-call batch pre-exists in the crashed conversation — the
	// projection must not drop or add anything), the archived mid-turn
	// prefix stays elided, and U must carry the checkpoint block so the
	// elided prefix remains recoverable.
	proj := app2.contextProjection()
	foundCall := false
	hasCheckpoint := false
	for _, m := range proj {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				if tc.ID == "tcX" {
					foundCall = true
				}
			}
		}
		if m.Role == "user" && m.Content != nil && strings.Contains(*m.Content, "[assistant checkpoint") {
			hasCheckpoint = true
		}
	}
	if !foundCall {
		t.Error("projection dropped the unanswered tool-call batch (dirty tail must pass through)")
	}
	if !hasCheckpoint {
		t.Error("checkpoint block missing from U in projection")
	}
}
