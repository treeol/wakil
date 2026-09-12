package agent

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// newBudgetApp creates an App with cost tracking and an optional budget.
func newBudgetApp(budgetUSD float64, out io.Writer) *App {
	app := &App{
		Cfg: config.DefaultConfig(),
		Out: out,
		costState: costState{
			Costs:     proxy.NewCostTracker(),
			BudgetUSD: budgetUSD,
		},
	}
	return app
}

// TestBudgetNoBudget verifies that without a budget set, checkBudgetExhausted
// always returns false and never sets the exhausted flag.
func TestBudgetNoBudget(t *testing.T) {
	app := newBudgetApp(0, io.Discard)
	app.Costs.Record(proxy.CostSourceInference, 100000, 50000, 1.50, true, proxy.ConfExact)
	if app.checkBudgetExhausted() {
		t.Error("checkBudgetExhausted should return false when BudgetUSD is 0")
	}
	if app.BudgetExhausted() {
		t.Error("BudgetExhausted should be false when no budget is set")
	}
}

// TestBudgetBelowLimit verifies that cost under the budget does not trigger
// exhaustion.
func TestBudgetBelowLimit(t *testing.T) {
	app := newBudgetApp(10.00, io.Discard)
	app.Costs.Record(proxy.CostSourceInference, 100000, 50000, 1.50, true, proxy.ConfExact)
	if app.checkBudgetExhausted() {
		t.Error("checkBudgetExhausted should return false when cost is under budget")
	}
	if app.BudgetExhausted() {
		t.Error("BudgetExhausted should be false when cost is under budget")
	}
}

// TestBudgetExceeded verifies that cost exceeding the budget triggers
// exhaustion and sets the sticky flag.
func TestBudgetExceeded(t *testing.T) {
	var out strings.Builder
	app := newBudgetApp(5.00, &out)
	app.Costs.Record(proxy.CostSourceInference, 1000000, 500000, 7.50, true, proxy.ConfExact)
	if !app.checkBudgetExhausted() {
		t.Error("checkBudgetExhausted should return true when cost exceeds budget")
	}
	if !app.BudgetExhausted() {
		t.Error("BudgetExhausted should be true after budget is exceeded")
	}
	if !strings.Contains(out.String(), "budget exhausted") {
		t.Errorf("expected budget exhausted warning in output, got: %q", out.String())
	}
}

// TestBudgetExhaustedIsSticky verifies that once exhausted, subsequent calls
// return true without re-checking the cost.
func TestBudgetExhaustedIsSticky(t *testing.T) {
	app := newBudgetApp(1.00, io.Discard)
	app.Costs.Record(proxy.CostSourceInference, 100000, 50000, 2.00, true, proxy.ConfExact)
	if !app.checkBudgetExhausted() {
		t.Fatal("first check should return true")
	}
	if !app.checkBudgetExhausted() {
		t.Error("second check should return true (sticky flag)")
	}
	if !app.BudgetExhausted() {
		t.Error("BudgetExhausted should remain true")
	}
}

// TestBudgetNilCostTracker verifies that nil CostTracker is safe.
func TestBudgetNilCostTracker(t *testing.T) {
	app := &App{
		Cfg:       config.DefaultConfig(),
		Out:       io.Discard,
		costState: costState{BudgetUSD: 5.00},
	}
	if app.checkBudgetExhausted() {
		t.Error("checkBudgetExhausted should return false when CostTracker is nil")
	}
	if app.BudgetExhausted() {
		t.Error("BudgetExhausted should be false when CostTracker is nil")
	}
}

// TestBudgetExactlyAtLimit verifies that cost exactly at the budget triggers
// exhaustion (>= comparison).
func TestBudgetExactlyAtLimit(t *testing.T) {
	app := newBudgetApp(5.00, io.Discard)
	app.Costs.Record(proxy.CostSourceInference, 100000, 50000, 5.00, true, proxy.ConfExact)
	if !app.checkBudgetExhausted() {
		t.Error("checkBudgetExhausted should return true when cost equals budget")
	}
}

// TestBudgetUnpricedCostNotCounted verifies that unpriced sources do not
// trigger budget exhaustion (Snapshot sums only priced rows).
func TestBudgetUnpricedCostNotCounted(t *testing.T) {
	app := newBudgetApp(1.00, io.Discard)
	app.Costs.Record(proxy.CostSourceInference, 1000000, 500000, 0, false, proxy.ConfApprox)
	if app.checkBudgetExhausted() {
		t.Error("unpriced cost should not trigger budget exhaustion")
	}
}

// TestSessionCostTotal verifies the SessionCostTotal helper.
func TestSessionCostTotal(t *testing.T) {
	app := newBudgetApp(0, io.Discard)
	app.Costs.Record(proxy.CostSourceInference, 100000, 50000, 1.50, true, proxy.ConfExact)
	app.Costs.Record(proxy.CostSourceMashura, 10000, 5000, 0.50, true, proxy.ConfExact)
	total := app.SessionCostTotal()
	if total < 1.99 || total > 2.01 {
		t.Errorf("SessionCostTotal = %.4f, want ~2.00", total)
	}
}

// TestSessionTokenTotals verifies the SessionTokenTotals helper.
func TestSessionTokenTotals(t *testing.T) {
	app := newBudgetApp(0, io.Discard)
	app.Costs.Record(proxy.CostSourceInference, 100000, 50000, 1.50, true, proxy.ConfExact)
	app.Costs.Record(proxy.CostSourceMashura, 10000, 5000, 0.50, true, proxy.ConfExact)
	inTok, outTok := app.SessionTokenTotals()
	if inTok != 110000 {
		t.Errorf("input tokens = %d, want 110000", inTok)
	}
	if outTok != 55000 {
		t.Errorf("output tokens = %d, want 55000", outTok)
	}
}

// TestFormatCostSummary verifies the summary string includes cost and tokens.
func TestFormatCostSummary(t *testing.T) {
	app := newBudgetApp(10.00, io.Discard)
	app.Costs.Record(proxy.CostSourceInference, 100000, 50000, 1.50, true, proxy.ConfExact)
	summary := app.FormatCostSummary()
	if !strings.Contains(summary, "session cost:") {
		t.Errorf("summary should contain 'session cost:', got: %q", summary)
	}
	if !strings.Contains(summary, "tokens:") {
		t.Errorf("summary should contain 'tokens:', got: %q", summary)
	}
	if !strings.Contains(summary, "budget") {
		t.Errorf("summary should contain 'budget' when BudgetUSD is set, got: %q", summary)
	}
}

// TestFormatCostSummaryNoBudget verifies summary without a budget.
func TestFormatCostSummaryNoBudget(t *testing.T) {
	app := newBudgetApp(0, io.Discard)
	app.Costs.Record(proxy.CostSourceInference, 100000, 50000, 1.50, true, proxy.ConfExact)
	summary := app.FormatCostSummary()
	if !strings.Contains(summary, "session cost:") {
		t.Errorf("summary should contain 'session cost:', got: %q", summary)
	}
	if strings.Contains(summary, "budget") {
		t.Errorf("summary should NOT contain 'budget' when BudgetUSD is 0, got: %q", summary)
	}
}

// TestFormatCostSummaryNilCosts verifies nil-safe behavior.
func TestFormatCostSummaryNilCosts(t *testing.T) {
	app := &App{Cfg: config.DefaultConfig()}
	summary := app.FormatCostSummary()
	if summary != "" {
		t.Errorf("summary should be empty when Costs is nil, got: %q", summary)
	}
}

// TestBudgetConcurrentAccess verifies that concurrent budget checks and cost
// recordings are race-free under -race.
func TestBudgetConcurrentAccess(t *testing.T) {
	app := newBudgetApp(100.00, io.Discard)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				app.Costs.Record(proxy.CostSourceInference, 1000, 500, 0.01, true, proxy.ConfExact)
				app.checkBudgetExhausted()
			}
		}()
	}
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				_ = app.BudgetExhausted()
				_ = app.SessionCostTotal()
			}
		}()
	}
	wg.Wait()
}

// TestBudgetConcurrentReservation simulates the concurrent reservation pattern:
// multiple goroutines checking and setting the exhausted flag simultaneously.
func TestBudgetConcurrentReservation(t *testing.T) {
	app := newBudgetApp(1.00, io.Discard)
	app.Costs.Record(proxy.CostSourceInference, 100000, 50000, 2.00, true, proxy.ConfExact)

	var wg sync.WaitGroup
	var count int32
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if app.checkBudgetExhausted() {
				atomic.AddInt32(&count, 1)
			}
		}()
	}
	wg.Wait()
	if !app.BudgetExhausted() {
		t.Error("BudgetExhausted should be true after concurrent checks")
	}
}

// TestBudgetStreamTurnSkipsOnExhausted verifies that streamTurn skips inference
// when the budget is already exhausted.
func TestBudgetStreamTurnSkipsOnExhausted(t *testing.T) {
	app := &App{
		Cfg:    config.DefaultConfig(),
		Out:    io.Discard,
		Client: &proxy.Client{ChatID: "test-budget-skip"},
		costState: costState{
			Costs: proxy.NewCostTracker(),
		},
	}
	app.budgetExhausted.Store(true)

	final, suspended, err := app.streamTurn(context.Background(), "test", nil, nil)
	if err != nil {
		t.Fatalf("streamTurn returned error: %v", err)
	}
	if !strings.Contains(final, "budget exhausted") {
		t.Errorf("expected budget exhausted message, got: %q", final)
	}
	if suspended {
		t.Error("should not be suspended when budget is exhausted")
	}
}
