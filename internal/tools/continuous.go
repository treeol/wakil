package tools

import "github.com/treeol/wakil/internal/proxy"

// ContinuousTools returns the experimental continuous-mode finalization tool.
// It is appended only by the headless --continue coordinator and is never part
// of DefaultTools, subagent tiers, or ordinary one-shot runs.
func ContinuousTools() []proxy.Tool {
	return []proxy.Tool{{Type: "function", Function: proxy.ToolFunction{
		Name: "finalize_goal",
		Description: "Record a structured proposal for the continuous coordinator. " +
			"This ends the current agent invocation. The coordinator alone decides whether " +
			"the goal is complete, needs another turn, or is blocked; prose is not completion.",
		Parameters: SchemaObj(map[string]interface{}{
			"status":  EnumProp("Proposed outcome.", "complete", "continue", "blocked"),
			"summary": StrProp("Concise summary of the current state (required)."),
			"remaining_work": map[string]interface{}{
				"type":        "array",
				"items":       StrProp("One bounded item of remaining work."),
				"description": "Required for continue; must be empty for complete.",
			},
			"requires_user":        BoolProp("Whether explicit user input is required. Required true for blocked."),
			"required_user_action": StrProp("Concrete user action required; set only when requires_user=true."),
		}, "status", "summary", "requires_user"),
	}}}
}
