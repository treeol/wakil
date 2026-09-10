package agent

// cache_stable.go — Cache-stable prompt layout helpers (card #189).
//
// The system prompt (Conv[0]) is already day-stable via ensurePreamble, and
// Anthropic cache_control breakpoints are already computed by the proxy
// client. This file fills the remaining gaps:
//
//   - Deterministic tool ordering within groups: tools are sorted by name
//     *within each group* (built-ins, search, MCP, oracle, LSP, browser),
//     preserving the group order so toggling a conditional group only
//     invalidates the suffix, not the entire prefix. This is important
//     because tool schemas are part of the prompt-cache prefix; non-
//     deterministic ordering within a group (e.g., MCP server returning
//     tools in a different order on reconnect) would silently invalidate
//     the cache.
//   - Cache-hit ratio: surfaces cached-token statistics for the /info panel
//     and cost display.

import (
	"sort"

	"github.com/treeol/wakil/internal/proxy"
)

// SortToolsByName returns a copy of tools sorted by Function.Name.
// Used within a single tool group (not across groups) so that group ordering
// is preserved — toggling a conditional group only invalidates the suffix.
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
