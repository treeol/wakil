package ilm

import (
	"encoding/json"
	"strings"
)

// assistAllowedTools is the explicit, default-deny read-only allowlist for
// assist-proposed actions (C2). The server has its own allowlist, but Wakil
// re-checks against its OWN list — defense in depth. No run_shell, no write
// tools, no memory mutations, no skill mutations, no staging writes.
//
// The list is reviewed built-in tools only — no MCP tools (their names are
// not statically known and their safety is not guaranteed by the name).
var assistAllowedTools = map[string]bool{
	// File reads
	"read_file":      true,
	"read_file_full": true,
	"search_files":   true,
	"find_files":     true,
	"list_dir":       true,
	// LSP (read-only)
	"lsp_definition": true,
	"lsp_references": true,
	"lsp_hover":      true,
	"lsp_symbols":    true,
	// Browser (read-only — no click, no eval)
	"browser_navigate":   true,
	"browser_screenshot": true,
	"browser_text":       true,
	"browser_html":       true,
	"browser_viewport":   true,
}

// IsAssistAllowed checks whether a proposed tool action passes Wakil's
// read-only allowlist. It validates the tool name against the explicit
// allowlist and, when args are non-empty and not JSON null, verifies
// they parse as a JSON object (rejecting malformed JSON, arrays, and
// scalars). This function does NOT inspect argument contents — path
// confinement and argument safety are enforced by the executor at
// execution time. MCP tools (containing "__") are rejected — their
// safety is not guaranteed.
func IsAssistAllowed(toolName string, args json.RawMessage) bool {
	// Reject MCP tools (name contains "__" — e.g. "trello__create_card").
	if strings.Contains(toolName, "__") {
		return false
	}

	// Must be in the explicit allowlist.
	if !assistAllowedTools[toolName] {
		return false
	}

	// When args are non-empty and not JSON null, verify they parse as a
	// JSON object (rejects malformed JSON, arrays, and scalars). Empty
	// and null args are accepted — the executor rejects missing required
	// params at execution time.
	var m map[string]interface{}
	if len(args) > 0 && string(args) != "null" {
		if err := json.Unmarshal(args, &m); err != nil {
			return false // malformed args → reject
		}
	}

	return true
}
