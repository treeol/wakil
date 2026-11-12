package agent

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/counsel"
	"github.com/treeol/wakil/internal/proxy"
)

// A panel call with a provider-reported cost lands under mash with that amount.
func TestPanelCallProviderCostLandsUnderMashura(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Costs = proxy.NewCostTracker()
	app.RecordOracleCostFor("openrouter/auto", counsel.OracleUsage{
		InputTokens: 100, OutputTokens: 20, ServedModel: "anthropic/claude-fable-5.2",
		ProviderCostUSD: 0.0042, HasProviderCost: true,
	})
	ct, rows := app.Costs.SnapshotDetail()
	if ct.Mashura != 0.0042 || ct.Provider != 0.0042 {
		t.Errorf("mash = %v provider = %v, want 0.0042 each", ct.Mashura, ct.Provider)
	}
	if ct.Unpriced != 0 {
		t.Errorf("unpriced = %d, want 0 for a provider-costed call", ct.Unpriced)
	}
	if len(rows) != 1 || rows[0].Source != proxy.CostSourceMashuraPrefix+"anthropic/claude-fable-5.2" {
		t.Errorf("row key = %+v, want keyed on the served model", rows)
	}
}

// openrouter/auto is priced by the resolved model, not the router id: the
// served model's configured rate is used when there is no provider cost.
func TestPanelCallAutoPricedByServedModel(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Costs = proxy.NewCostTracker()
	app.Cfg.Costs.Mashura = map[string]config.ModelRate{
		"anthropic/claude-fable-5.2": {InputUSDPer1M: 2, OutputUSDPer1M: 10},
	}
	app.RecordOracleCostFor("openrouter/auto", counsel.OracleUsage{
		InputTokens: 1_000_000, OutputTokens: 100_000, ServedModel: "anthropic/claude-fable-5.2",
	})
	ct, rows := app.Costs.SnapshotDetail()
	// 1M in × $2 + 0.1M out × $10 = $3.00
	if ct.Mashura != 3 {
		t.Errorf("mash = %v, want 3 from the served model's rate", ct.Mashura)
	}
	if ct.Unpriced != 0 {
		t.Errorf("unpriced = %d, want 0 when the served model is priced", ct.Unpriced)
	}
	if len(rows) != 1 || rows[0].Source != proxy.CostSourceMashuraPrefix+"anthropic/claude-fable-5.2" {
		t.Errorf("row key = %+v, want the served model", rows)
	}
}

// A call with no reported cost and no price entry is unpriced: it is counted,
// shows in the unpriced figure, and never adds a $0.0000 cost.
func TestPanelCallUnpricedIsCountedNotZero(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Costs = proxy.NewCostTracker()
	app.RecordOracleCostFor("some/unknown-model", counsel.OracleUsage{InputTokens: 50, OutputTokens: 5})
	ct, rows := app.Costs.SnapshotDetail()
	if ct.Unpriced != 1 {
		t.Errorf("unpriced = %d, want 1", ct.Unpriced)
	}
	if ct.Mashura != 0 || ct.Total != 0 {
		t.Errorf("unpriced call added dollars: %+v", ct)
	}
	if len(rows) != 1 || rows[0].Priced || rows[0].UnpricedCalls != 1 {
		t.Errorf("row = %+v, want unpriced with one unpriced call", rows)
	}
	line := app.costStatusLine()
	if !strings.Contains(line, "· unpriced 1") {
		t.Errorf("status line missing unpriced count: %q", line)
	}
}

// Zero-token calls are recorded too (a failed or empty reply still counts as a
// call), so the unpriced figure is not silently short.
func TestPanelCallZeroTokensStillRecorded(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Costs = proxy.NewCostTracker()
	app.RecordOracleCostFor("some/unknown-model", counsel.OracleUsage{})
	ct, _ := app.Costs.SnapshotDetail()
	if ct.Unpriced != 1 {
		t.Errorf("zero-token call not recorded: unpriced = %d, want 1", ct.Unpriced)
	}
}

// Without any unpriced calls the status line keeps its shape, with no
// "unpriced" token at all.
func TestCostStatusLineNoUnpricedSuffixWhenZero(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Costs = proxy.NewCostTracker()
	app.RecordOracleCostFor("m", counsel.OracleUsage{
		InputTokens: 10, OutputTokens: 5, ProviderCostUSD: 0.01, HasProviderCost: true,
	})
	if line := app.costStatusLine(); strings.Contains(line, "unpriced") {
		t.Errorf("status line shows unpriced with none unpriced: %q", line)
	}
}

// Totals with an unpriced call survive save and restore, including the count.
func TestUnpricedCountSurvivesRestore(t *testing.T) {
	src := proxy.NewCostTracker()
	src.RecordCategorized("mashura·x", proxy.CostCatMashura, 5, 1, 0, false, proxy.ConfExact)
	_, rows := src.SnapshotDetail()
	dst := proxy.NewCostTracker()
	dst.RestoreRows(rows)
	ct, _ := dst.SnapshotDetail()
	if ct.Unpriced != 1 {
		t.Errorf("restored unpriced = %d, want 1", ct.Unpriced)
	}
}
