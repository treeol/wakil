package agent

import "github.com/treeol/wakil/internal/proxy"

// cost_state.go: cost-tracking state for App (WP-6.3 extraction).
// Embedded in App so all field access (a.Costs, etc.) is unchanged via
// Go's promoted-field access.

type costState struct {
	// Costs accumulates per-source cost estimates for the session, rendered in the
	// sidebar. Nil disables tracking (subagents, headless runs, tests) — every
	// CostTracker method is nil-safe, so call sites need no guard.
	Costs *proxy.CostTracker
}
