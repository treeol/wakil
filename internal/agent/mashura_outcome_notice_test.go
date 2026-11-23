package agent

// Block 2 (0.8.11): the Mashura panel outcome lines (VOID / NOT COMPLETED /
// cost) must go through the notice path, never onto the streamed output —
// they are printed by the async worker while the turn is still streaming.

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/counsel"
	"github.com/treeol/wakil/internal/proxy"
)

// TestMashuraPanelOutcomeThroughNoticePath: with an event sink attached,
// every outcome line arrives as SysNoteMsg and nothing lands on a.Out
// (the streamed answer).
func TestMashuraPanelOutcomeThroughNoticePath(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	var out strings.Builder
	app.Out = &out
	var notes []SysNoteMsg
	app.EventSink = func(msg interface{}) {
		if sn, ok := msg.(SysNoteMsg); ok {
			notes = append(notes, sn)
		}
	}
	results := []counsel.PanelMemberResult{
		{Model: "m1", PrefixedModel: "p/m1", Void: true},
		{Model: "m2", PrefixedModel: "p/m2", BudgetStopped: true},
	}
	printMashuraPanelOutcome(app, results)
	if len(notes) != 3 {
		t.Fatalf("want 3 SysNoteMsg notices (VOID, NOT COMPLETED, cost), got %d: %+v", len(notes), notes)
	}
	if !strings.Contains(notes[0].Text, "VOID") {
		t.Errorf("first notice should be the VOID line: %q", notes[0].Text)
	}
	if !strings.Contains(notes[1].Text, "NOT COMPLETED") {
		t.Errorf("second notice should be the NOT COMPLETED line: %q", notes[1].Text)
	}
	if !strings.Contains(notes[2].Text, "mashūra panel:") {
		t.Errorf("third notice should be the cost line: %q", notes[2].Text)
	}
	if strings.Contains(out.String(), "mashūra panel:") || strings.Contains(out.String(), "VOID") {
		t.Errorf("outcome line leaked to a.Out (streamed answer): %q", out.String())
	}
}

// TestMashuraPanelCostLineZeroSinkFallsBack: with no sink the cost line is
// printed as one plain line on a.Out (headless).
func TestMashuraPanelCostLineZeroSinkFallsBack(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	var out strings.Builder
	app.Out = &out
	results := []counsel.PanelMemberResult{
		{Model: "m1", PrefixedModel: "p/m1", Usage: counsel.OracleUsage{
			InputTokens: 100, OutputTokens: 20, ProviderCostUSD: 0.01, HasProviderCost: true,
		}},
	}
	printMashuraPanelOutcome(app, results)
	if !strings.Contains(out.String(), "mashūra panel: $0.0100 total (1 calls)") {
		t.Errorf("cost line missing from the fallback output: %q", out.String())
	}
	if strings.Contains(out.String(), "\x1b") {
		t.Errorf("fallback in a non-terminal must carry no escape sequence: %q", out.String())
	}
	_ = proxy.CostTracker{}
}
