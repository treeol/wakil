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
				"(matches read_file_full). For template skills (skills carrying files) the file bodies are " +
				"replaced by a compact summary of params/files/verify steps — use apply_skill_template to " +
				"write the files instead of retyping them; pass include_files=true only to inspect or edit the raw template." +
				skillScopeNote + " Available to all agent tiers.",
			Parameters: SchemaObj(map[string]interface{}{
				"key":           StrProp("Skill key to load (flat identifier, no path separators)."),
				"include_files": BoolProp("Return the raw value including template file bodies (default false)."),
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
				"key":           StrProp("Skill key to update."),
				"value":         StrProp("New skill content. Max 256 KiB. For template skills, omit the template section to keep the existing files unchanged."),
				"description":   StrProp("Optional new one-line description."),
				"drop_template": BoolProp("Remove the skill's template files (default false: an existing template section is preserved when value has none)."),
			}, "key", "value"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "apply_skill_template",
			Description: "Write a template skill's files into the workspace exactly as stored, substituting @@NAME@@ placeholders " +
				"with params — instead of retyping files from load_skill. Refuses to overwrite differing files unless overwrite=true; " +
				"never runs the skill's verify steps (they are returned for you to propose separately). Records provenance in " +
				".wakil/skill-templates.json. MAIN AGENT ONLY. Always requires confirmation (even in /auto)." + skillScopeNote,
			Parameters: SchemaObj(map[string]interface{}{
				"key":        StrProp("Template skill key."),
				"params":     map[string]interface{}{"type": "object", "description": "Placeholder values by name, e.g. {\"STACK_NAME\":\"shop\"}. Required params without defaults must be given.", "additionalProperties": map[string]interface{}{"type": "string"}},
				"target_dir": StrProp("Directory (workspace-relative) to write into. Default \".\"."),
				"overwrite":  BoolProp("Replace existing files whose content or mode differs (default false)."),
			}, "key"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "capture_skill_template",
			Description: "Read workspace files and store them verbatim as a skill's template files (no retyping). Literal param values " +
				"in paths/bodies become @@NAME@@ placeholders; the captured values themselves are not stored. Creates the skill " +
				"(description required) or replaces the template section of an existing one (its prose is kept). Never put secrets in " +
				"templates — reference .env files / docker secrets instead. MAIN AGENT ONLY. Always requires confirmation." + skillScopeNote,
			Parameters: SchemaObj(map[string]interface{}{
				"key":      StrProp("Skill key to create or update."),
				"paths":    map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Files to capture, relative to base_dir (max 50, 64 KiB each, text only)."},
				"base_dir": StrProp("Directory the stored paths are relative to. Default \".\"."),
				"params": map[string]interface{}{"type": "array", "description": "Placeholders: each literal value (≥3 bytes) is replaced by @@NAME@@. Without default the param is required at apply time.",
					"items": map[string]interface{}{"type": "object", "properties": map[string]interface{}{
						"name":        map[string]interface{}{"type": "string", "description": "UPPER_SNAKE name."},
						"value":       map[string]interface{}{"type": "string", "description": "Literal text in the captured files to replace."},
						"description": map[string]interface{}{"type": "string"},
						"default":     map[string]interface{}{"type": "string", "description": "Optional default (makes the param optional)."},
						"pattern":     map[string]interface{}{"type": "string", "description": "Optional full-match regexp for values."},
					}, "required": []string{"name", "value"}}},
				"modes":       map[string]interface{}{"type": "object", "description": "Optional file mode overrides by path, e.g. {\"deploy.sh\":\"0755\"}.", "additionalProperties": map[string]interface{}{"type": "string"}},
				"verify":      map[string]interface{}{"type": "array", "items": map[string]interface{}{"type": "string"}, "description": "Optional verification commands to suggest after apply (never auto-run)."},
				"description": StrProp("One-line description — required when creating a new skill."),
			}, "key", "paths"),
		}},
		{Type: "function", Function: proxy.ToolFunction{
			Name: "check_skill_template",
			Description: "Read-only: for files recorded in .wakil/skill-templates.json, report whether each is unchanged, locally modified, " +
				"or missing, and whether its template skill has a newer version." + skillScopeNote + " Available to all agent tiers.",
			Parameters: SchemaObj(map[string]interface{}{
				"path": StrProp("Optional: only this workspace file."),
				"key":  StrProp("Optional: only files from this skill."),
			}),
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
		"save_skill", "update_skill", "forget_skill",
		"apply_skill_template", "capture_skill_template", "check_skill_template":
		return true
	}
	return false
}

// IsSkillTemplateMainOnlyTool reports whether name is a skill-template tool
// restricted to the main agent. Kept separate from IsSkillWriteTool, whose
// meaning is "writes the global store": apply_skill_template writes the
// workspace, not the store.
func IsSkillTemplateMainOnlyTool(name string) bool {
	switch name {
	case "apply_skill_template", "capture_skill_template":
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
