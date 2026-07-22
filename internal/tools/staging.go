package tools

import (
	"encoding/json"

	"github.com/treeol/wakil/internal/proxy"
)

const stagingDesc = `Fast ephemeral scratch/staging KV store inside the sandbox. ` +
	`Repo-scoped, survives sandbox restarts via snapshot. NOT durable memory — ` +
	`TTL strongly encouraged. Cross-prefix reads are allowed (subagent handoffs). ` +
	`The tool layer prepends your agent prefix to keys on put/delete; ` +
	`you cannot write outside your prefix.`

// StagingTools returns the staging tool definitions. These are registered
// for ALL agent tiers (main, discovery, edit) — staging is ungated by
// design (the gate lives at promotion, in a later ticket).
//
// staging_put in the discovery tier is a DELIBERATE exception to "discovery
// is read-only": staging writes touch no workspace state, and handoff-
// writing is the tier's purpose. See docs/staging.md.
func StagingTools() []proxy.Tool {
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
			Name:        "staging_put",
			Description: "Store a value in the " + stagingDesc + " The key is prefixed with your agent identity; supply the key without the prefix.",
			Parameters: obj(map[string]interface{}{
				"key":         strProp("Key (without prefix — the tool layer adds your agent prefix automatically)"),
				"value":       strProp("Value to store"),
				"ttl_seconds": intProp("Optional TTL in seconds. Strongly encouraged — staging is ephemeral."),
			}, "key", "value"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "staging_get",
			Description: "Retrieve a value from staging. Cross-prefix reads are allowed — pass the full key including prefix (e.g. 'main/result' or 'sub-abc12345/data').",
			Parameters: obj(map[string]interface{}{
				"key": strProp("Full key including prefix"),
			}, "key"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "staging_delete",
			Description: "Delete a key from staging. Can only delete under your own prefix — supply the key without prefix.",
			Parameters: obj(map[string]interface{}{
				"key": strProp("Key (without prefix — the tool layer adds your agent prefix automatically)"),
			}, "key"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "staging_list",
			Description: "List keys in staging, optionally filtered by prefix. Returns up to 200 keys with a truncation marker. Cross-prefix listing is allowed.",
			Parameters: obj(map[string]interface{}{
				"prefix": strProp("Optional key prefix to filter by (e.g. 'main/' or 'sub-')"),
			}),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "staging_get_many",
			Description: "Retrieve multiple values from staging. Cross-prefix reads are allowed. Pass full keys including prefixes.",
			Parameters: obj(map[string]interface{}{
				"keys": arrProp("Array of full keys including prefixes (e.g. [\"main/result\", \"sub-abc12345/data\"])"),
			}, "keys"),
		}},
	}
}

// IsStagingTool reports whether name is one of the staging_* tools.
func IsStagingTool(name string) bool {
	switch name {
	case "staging_put", "staging_get", "staging_delete", "staging_list", "staging_get_many":
		return true
	}
	return false
}
