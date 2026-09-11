package agent

// cache_stable.go — Cache-stable prompt layout helpers (card #189).
//
// The system prompt (Conv[0]) is already day-stable via ensurePreamble, and
// Anthropic cache_control breakpoints are already computed by the proxy
// client. This file provides deterministic tool ordering within a group:
//
// Tools are sorted by name within a single group (built-ins, search, MCP,
// oracle, LSP, browser) so that non-deterministic ordering (e.g., an MCP
// server returning tools in a different order on reconnect) does not
// silently invalidate the prompt-cache prefix. Group order itself is
// determined by the caller's append chain, not by this file.

import (
	"sort"

	"github.com/treeol/wakil/internal/proxy"
)

// SortToolsByName returns a shallow copy of nonempty tools sorted by
// Function.Name; empty input is returned unchanged. Callers should sort
// each group separately and append groups in the desired order so that
// toggling a conditional group only invalidates from that group onward.
func SortToolsByName(tools []proxy.Tool) []proxy.Tool {
	if len(tools) == 0 {
		return tools
	}
	sorted := make([]proxy.Tool, len(tools))
	copy(sorted, tools)
	sort.SliceStable(sorted, func(i, j int) bool {
		return sorted[i].Function.Name < sorted[j].Function.Name
	})
	return sorted
}
