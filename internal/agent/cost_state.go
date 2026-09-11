package agent

import (
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/treeol/wakil/internal/proxy"
)

// cost_state.go: cost-tracking state for App. Embedded in App so all field
// access (a.Costs, etc.) is unchanged via Go's promoted-field access.

type costState struct {
	// Costs accumulates per-source cost estimates for the session, rendered in the
	// sidebar. Nil disables tracking (subagents, headless runs, tests) — every
	// CostTracker method is nil-safe, so call sites need no guard.
	Costs *proxy.CostTracker

	// BudgetUSD is the per-session spending ceiling. When > 0, the agent checks
	// after each inference call whether the session's total priced cost has
	// exceeded this amount. If so, the current turn is force-finished (no further
	// tool calls) and budgetExhausted is set to prevent subsequent turns from
	// issuing more inference calls. This is a SOFT cutoff: the check runs after
	// RecordInferenceCost, so the call that breaches the budget has already
	// incurred its cost. Overshoot is bounded by one turn's inference spend.
	// Zero (default) = no budget enforcement.
	BudgetUSD float64

	// budgetExhausted is a sticky session-scoped flag set when the budget is
	// breached. Once set, streamTurn force-finishes every subsequent turn
	// immediately (no inference, no tools) and prints a budget warning. It is
	// NOT reset by prepareTurn (it's session-scoped, not per-turn). Atomic
	// for concurrent read access from the TUI sidebar rendering.
	budgetExhausted atomic.Bool
}

// checkBudgetExhausted returns true if the session budget has been breached.
// When the budget is set (BudgetUSD > 0) and the session's total priced cost
// exceeds it, the budgetExhausted flag is set (sticky, session-scoped) and
// a visible warning is printed. Subsequent turns are force-finished by the
// early check in streamTurn.
func (a *App) checkBudgetExhausted() bool {
	if a.BudgetUSD <= 0 || a.Costs == nil {
		return false
	}
	if a.budgetExhausted.Load() {
		return true
	}
	total, _ := a.Costs.Snapshot()
	if total >= a.BudgetUSD {
		a.budgetExhausted.Store(true)
		fmt.Fprintln(a.Out, Yellow(fmt.Sprintf(
			"⚠ session budget exhausted: $%.4f spent (budget $%.2f) — further inference will be blocked",
			total, a.BudgetUSD)))
		return true
	}
	return false
}

// BudgetExhausted returns whether the session budget has been breached.
// Exported for the TUI/sidebar and headless exit summary. Thread-safe.
func (a *App) BudgetExhausted() bool {
	return a.budgetExhausted.Load()
}

// SessionCostTotal returns the session's total priced cost in USD.
// Returns 0 when cost tracking is disabled. Thread-safe.
func (a *App) SessionCostTotal() float64 {
	if a.Costs == nil {
		return 0
	}
	total, _ := a.Costs.Snapshot()
	return total
}

// SessionTokenTotals returns the session's total input and output tokens
// across all sources. Thread-safe.
func (a *App) SessionTokenTotals() (input, output int64) {
	if a.Costs == nil {
		return 0, 0
	}
	_, rows := a.Costs.Snapshot()
	return sumTokenTotals(rows)
}

// sumTokenTotals sums InputTok and OutputTok across all snapshot rows.
func sumTokenTotals(rows []proxy.CostRow) (input, output int64) {
	for _, r := range rows {
		input += r.InputTok
		output += r.OutputTok
	}
	return
}

// FormatCostSummary returns a human-readable session cost summary string
// suitable for end-of-session output (headless terminal record or TUI quit
// message). Includes total tokens and priced cost; notes unpriced sources
// and budget status.
func (a *App) FormatCostSummary() string {
	if a.Costs == nil {
		return ""
	}
	total, rows := a.Costs.Snapshot()
	var hasUnpriced bool
	for _, r := range rows {
		if !r.Priced {
			hasUnpriced = true
		}
	}
	inTok, outTok := sumTokenTotals(rows)

	var b strings.Builder
	fmt.Fprintf(&b, "session cost: %s", proxy.FmtUSDCompact(total))
	if a.BudgetUSD > 0 {
		fmt.Fprintf(&b, " / $%.2f budget", a.BudgetUSD)
		if a.budgetExhausted.Load() {
			b.WriteString(" (exhausted)")
		}
	}
	fmt.Fprintf(&b, " | tokens: %dk in, %dk out", inTok/1000, outTok/1000)
	if hasUnpriced {
		b.WriteString(" (some sources unpriced — actual cost may be higher)")
	}
	return b.String()
}
