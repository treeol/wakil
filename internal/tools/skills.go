package tools

import (
	"github.com/treeol/wakil/internal/proxy"
)

const skillDesc = `GLOBAL, durable, host-side skills store. ` +
	`Skills are user-authored reference docs (templates, runbooks, conventions, ` +
	`specs) shared across EVERY session and EVERY project — unlike memory, they ` +
	`are NOT workspace-scoped. Always durable (no TTL), active immediately on ` +
	`save/update (no promote step for the main agent/user). Every entry carries ` +
	`provenance (writer, taint) and full version history (supersedes chain, ` +
	`including tombstones). Anchors are disabled (global store has no stable ` +
	`workspace root). save_skill, update_skill, and forget_skill are MAIN AGENT ` +
	`ONLY — subagents calling them get an error.`

// skillScopeNote is the one-line scoping reminder appended to every skill tool
// description except save_skill (which carries the full skillDesc blurb). This
// mirrors the memory precedent: the long policy blurb lives on ONE tool so it
// is not repeated 7× per request (token cost), while each tool still reminds
// the model of the global-vs-memory distinction.
const skillScopeNote = ` Global skill store (shared across all sessions/projects, unlike memory).`

// SkillTools returns the skill tool definitions. These are registered for ALL
// agent tiers (main, discovery, edit, tools) — same as staging and memory. The
// write-tool restriction (save/update/forget are main-agent-only) is enforced
// in the tool handler via a.IsSubagent, not by excluding tools from the list.
//
// Read tools (list_skills, load_skill, skill_search, skill_history) are
// ungated. Write tools (save_skill, update_skill, forget_skill) go through the
// confirm gate via GatedTool, matching the destructive-mutation policy.
func SkillTools() []proxy.Tool {
	return []proxy.Tool{
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "list_skills",
			Description: "List all active skills: key + first-line description (NOT full bodies)." + skillScopeNote + " Available to all agent tiers.",
			Parameters:  SchemaObj(map[string]interface{}{}),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "load_skill",
			Description: "Load the full content of an active skill by key, with a provenance header. " +
				"Routed through SpillFullResult so large skills are NOT truncated to ToolResultCap " +
				"(matches read_file_full)." + skillScopeNote + " Available to all agent tiers.",
			Parameters: SchemaObj(map[string]interface{}{
				"key": StrProp("Skill key to load (flat identifier, no path separators)."),
			}, "key"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "skill_search",
			Description: "Full-text search (FTS5) over active skills' keys and content. Returns matching skills with snippets (not full bodies)." + skillScopeNote + " Available to all agent tiers.",
			Parameters: SchemaObj(map[string]interface{}{
				"query": StrProp("FTS5 search query."),
			}, "query"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name:        "skill_history",
			Description: "Show the full version history for a skill key, newest first: active version, all superseded versions, and tombstones, each with provenance." + skillScopeNote + " Available to all agent tiers.",
			Parameters: SchemaObj(map[string]interface{}{
				"key": StrProp("Skill key to show history for."),
			}, "key"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "save_skill",
			Description: "Create a NEW skill, active immediately. Errors if the key already exists — use update_skill to change an existing skill. " +
				"MAIN AGENT ONLY. Requires confirmation. " + skillDesc,
			Parameters: SchemaObj(map[string]interface{}{
				"key":         StrProp("Skill key (flat identifier, no path separators). Max 256 bytes."),
				"value":       StrProp("Skill content (the reference doc). Max 256 KiB."),
				"description": StrProp("Optional one-line description shown by list_skills."),
			}, "key", "value"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "update_skill",
			Description: "Update an EXISTING skill: supersedes the current active version with new content, preserving the old version in skill_history. " +
				"MAIN AGENT ONLY. Requires confirmation." + skillScopeNote,
			Parameters: SchemaObj(map[string]interface{}{
				"key":         StrProp("Skill key to update."),
				"value":       StrProp("New skill content. Max 256 KiB."),
				"description": StrProp("Optional new one-line description."),
			}, "key", "value"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "forget_skill",
			Description: "Forget a skill: tombstones the active version. The skill disappears from list_skills, load_skill, and skill_search, but remains visible in skill_history. Nothing is hard-deleted. " +
				"MAIN AGENT ONLY. Requires confirmation." + skillScopeNote,
			Parameters: SchemaObj(map[string]interface{}{
				"key": StrProp("Skill key to forget."),
			}, "key"),
		}},
	}
}

// IsSkillTool reports whether name is one of the skill tools.
func IsSkillTool(name string) bool {
	switch name {
	case "list_skills", "load_skill", "skill_search", "skill_history",
		"save_skill", "update_skill", "forget_skill":
		return true
	}
	return false
}

// IsSkillWriteTool reports whether name is a skill tool restricted to the main
// agent (not available to subagents). These are rejected at dispatch time via
// a.IsSubagent, not by excluding from tool lists — same pattern as
// IsMemoryMainOnlyTool.
func IsSkillWriteTool(name string) bool {
	switch name {
	case "save_skill", "update_skill", "forget_skill":
		return true
	}
	return false
}
