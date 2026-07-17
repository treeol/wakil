package tools

import (
	"encoding/json"

	"github.com/treeol/wakil/internal/proxy"
)

const memoryDesc = `Durable, trusted, host-side memory store scoped to the current workspace. ` +
	`Survives across sessions in this workspace, but entries are NOT shared across ` +
	`different workspaces — each workspace has its own isolated memory DB. ` +
	`Two tiers: mid (TTL 1h–7d, direct active writes) and durable (no TTL, writes ` +
	`land as PROPOSED — promotion to ACTIVE requires the main agent via memory_promote). ` +
	`Every entry carries provenance (writer, tier, taint, staleness). ` +
	`memory_promote, memory_reject, memory_forget, and memory_promote_from_staging ` +
	`are MAIN AGENT ONLY — subagents calling them get an error.`

// MemoryTools returns the memory tool definitions. These are registered for ALL
// agent tiers (main, discovery, edit) — same as staging. The tier-gating
// (main-only tools rejected for subagents) is enforced in the tool handler
// via a.IsSubagent, not by excluding tools from the list.
//
// memory_put in the discovery tier is a DELIBERATE exception to "discovery
// is read-only" — same rationale as staging_put: memory writes touch no
// workspace state, and proposing durable entries is a legitimate subagent
// capability. See docs/memory.md.
func MemoryTools() []proxy.Tool {
	strProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "string", "description": desc}
	}
	intProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "integer", "description": desc}
	}
	arrProp := func(desc string) map[string]interface{} {
		return map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": desc}
	}
	obj := func(props map[string]interface{}, required ...string) json.RawMessage {
		m := map[string]interface{}{"type": "object", "properties": props}
		if len(required) > 0 {
			m["required"] = required
		}
		b, _ := json.Marshal(m)
		return b
	}
	return []proxy.Tool{
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "memory_put",
			Description: "Write to durable memory. " + memoryDesc + " ttl_seconds present (3600–604800) → mid-tier active write. ttl_seconds absent → durable-tier PROPOSED entry (for everyone including main agent — promote in a second step). Available to all agent tiers.",
			Parameters: obj(map[string]interface{}{
				"key":         strProp("Hierarchical key, e.g. 'arch/auth-flow'. Max 256 bytes."),
				"value":       strProp("Value to store. Max 64 KiB."),
				"kind":        strProp("Freeform category: 'note', 'decision', 'summary', etc."),
				"ttl_seconds": intProp("Optional TTL in seconds (3600–604800). Present → mid-tier (active, auto-expires). Absent → durable-tier (proposed, needs promotion)."),
				"anchors":     arrProp("Optional list of workspace-relative file paths. Hashes are computed at write time; staleness is checked at read time."),
			}, "key", "value", "kind"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "memory_promote",
			Description: "Promote a proposed durable entry to active. MAIN AGENT ONLY (and user). Optionally edit the value at promotion time. The original proposed value is preserved in the superseded entry.",
			Parameters: obj(map[string]interface{}{
				"id":           intProp("ID of the proposed entry to promote."),
				"edited_value": strProp("Optional: if provided, the promoted active entry gets this value instead of the proposed value."),
			}, "id"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "memory_reject",
			Description: "Reject a proposed durable entry. MAIN AGENT ONLY. The reason is recorded in the entry's note.",
			Parameters: obj(map[string]interface{}{
				"id":     intProp("ID of the proposed entry to reject."),
				"reason": strProp("Optional reason for rejection, recorded in the entry's note."),
			}, "id"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "memory_get",
			Description: "Get the active entry for a key. Returns the entry with provenance and staleness info. Memory is scoped to the current workspace — entries from other workspaces are not visible. Available to all agent tiers.",
			Parameters: obj(map[string]interface{}{
				"key": strProp("Key to look up."),
			}, "key"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "memory_search",
			Description: "Full-text search over memory entries. Returns up to 20 entries with provenance. By default only active entries; pass include_proposed=true to include proposed. Memory is scoped to the current workspace — entries from other workspaces are not visible. Available to all agent tiers.",
			Parameters: obj(map[string]interface{}{
				"query":            strProp("FTS5 search query."),
				"tier":             strProp("Optional tier filter: 'mid' or 'durable'."),
				"include_proposed": strProp("Optional: include proposed entries (default false). Pass 'true' to include."),
			}, "query"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "memory_list",
			Description: "List memory entries by prefix, tier, and/or status. Returns keys + metadata (up to 200). Memory is scoped to the current workspace — entries from other workspaces are not visible. Available to all agent tiers.",
			Parameters: obj(map[string]interface{}{
				"prefix": strProp("Optional key prefix to filter by."),
				"tier":   strProp("Optional tier filter: 'mid' or 'durable'."),
				"status": strProp("Optional status filter (default 'active'): 'active', 'proposed', 'superseded', 'rejected', 'expired'."),
			}),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "memory_forget",
			Description: "Forget (supersede with a tombstone) the active entry for a key. MAIN AGENT ONLY. Nothing is ever hard-deleted by agents.",
			Parameters: obj(map[string]interface{}{
				"key": strProp("Key to forget."),
			}, "key"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "memory_promote_from_staging",
			Description: "Read a value from staging (kvr) and write it as a durable PROPOSED entry. MAIN AGENT ONLY. The staging key's prefix is recorded as the writer (provenance flows through). Taint is always 'unknown' (staging values carry no taint metadata). Does not delete the staging key.",
			Parameters: obj(map[string]interface{}{
				"staging_key": strProp("Full staging key including prefix (e.g. 'sub-abc12345/data')."),
				"key":         strProp("Durable memory key for the proposed entry."),
				"kind":        strProp("Freeform category."),
				"anchors":     arrProp("Optional list of workspace-relative file paths."),
			}, "staging_key", "key", "kind"),
		}},
	}
}

// IsMemoryTool reports whether name is one of the memory_* tools.
func IsMemoryTool(name string) bool {
	switch name {
	case "memory_put", "memory_promote", "memory_reject", "memory_get",
		"memory_search", "memory_list", "memory_forget", "memory_promote_from_staging":
		return true
	}
	return false
}

// IsMemoryMainOnlyTool reports whether name is a memory tool restricted to
// the main agent (not available to subagents). These are rejected at dispatch
// time via a.IsSubagent, not by excluding from tool lists.
func IsMemoryMainOnlyTool(name string) bool {
	switch name {
	case "memory_promote", "memory_reject", "memory_forget", "memory_promote_from_staging":
		return true
	}
	return false
}
