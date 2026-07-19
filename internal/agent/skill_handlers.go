package agent

// skill_handlers.go contains the handlers for the skill tools (list_skills,
// load_skill, skill_search, skill_history, save_skill, update_skill,
// forget_skill). They run host-side against a.SkillStore (a *skillsProfile
// wrapping memory.Store) — no sandbox round-trip, same as the memory handlers.
//
// Gating model (Mashūra-reviewed):
//   - Read tools (list/load/search/history) are ungated and available to ALL
//     tiers, including subagents.
//   - Write tools (save/update/forget) are MAIN AGENT ONLY. The a.IsSubagent
//     check MUST run before a.Confirm: a tools-tier subagent's confirmer
//     auto-approves everything (toolsConfirmer returns true unconditionally),
//     so checking IsSubagent first is the only thing that prevents a subagent
//     from writing to the global store. This mirrors handleMemoryPromote /
//     handleMemoryForget.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/memory"
	"github.com/treeol/wakil/internal/proxy"
)

// skillsUnavailable is the response for all skill tools when the store is not
// available (init failed).
const skillsUnavailable = "skills unavailable (could not open store)"

// skillMainOnlyDenied is the error returned when a subagent calls a
// main-agent-only skill write tool. The message is stable for tests.
const skillMainOnlyDenied = "ERROR: this skill tool is main-agent only — subagents can read skills (list_skills, load_skill, skill_search, skill_history) but cannot save, update, or forget them."

func (a *App) getSkillStore() *skillsProfile {
	return a.SkillStore
}

// handleListSkills lists all active skills (key + first-line description).
func (a *App) handleListSkills(ctx context.Context, tc proxy.ToolCall) string {
	s := a.getSkillStore()
	if s == nil {
		return skillsUnavailable
	}
	entries, err := s.listActiveSkills(ctx)
	if err != nil {
		return fmt.Sprintf("ERROR: list skills: %v", err)
	}
	if len(entries) == 0 {
		return "(no skills found — the global skill store is empty; use save_skill to add one)"
	}
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%s%s — %s\n", e.Key, skillProvenanceSuffix(e), firstLine(e.Value))
	}
	return strings.TrimRight(b.String(), "\n")
}

// handleLoadSkill loads the full content of an active skill by key.
// NOTE: ExecuteToolCall routes load_skill through SpillFullResult in
// CapOrStub, so large skills are not truncated to ToolResultCap.
func (a *App) handleLoadSkill(ctx context.Context, tc proxy.ToolCall) string {
	s := a.getSkillStore()
	if s == nil {
		return skillsUnavailable
	}
	var args struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	e, err := s.getActiveSkill(ctx, args.Key)
	if err == memory.ErrNotFound {
		return "not found: " + args.Key + " (no active skill with that key — use list_skills to see available skills)"
	}
	if err != nil {
		return fmt.Sprintf("ERROR: load skill: %v", err)
	}
	return fmt.Sprintf("# skill: %s\n%s\n\n%s", e.Key, renderSkillProvenance(e), e.Value)
}

// handleSkillSearch runs FTS5 search over active skills.
func (a *App) handleSkillSearch(ctx context.Context, tc proxy.ToolCall) string {
	s := a.getSkillStore()
	if s == nil {
		return skillsUnavailable
	}
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	if strings.TrimSpace(args.Query) == "" {
		return "ERROR: query is required"
	}
	entries, err := s.searchSkills(ctx, args.Query)
	if err != nil {
		return fmt.Sprintf("ERROR: skill search: %v", err)
	}
	if len(entries) == 0 {
		return "(no skills match that query)"
	}
	var b strings.Builder
	for i, e := range entries {
		if i > 0 {
			b.WriteString("\n---\n")
		}
		fmt.Fprintf(&b, "%s%s\n%s", e.Key, skillProvenanceSuffix(e), snippet(e.Value, 200))
	}
	return b.String()
}

// handleSkillHistory renders the full version chain for a skill key.
func (a *App) handleSkillHistory(ctx context.Context, tc proxy.ToolCall) string {
	s := a.getSkillStore()
	if s == nil {
		return skillsUnavailable
	}
	var args struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	entries, err := s.historyForKey(ctx, args.Key)
	if err != nil {
		return fmt.Sprintf("ERROR: skill history: %v", err)
	}
	if len(entries) == 0 {
		return "(no history for skill: " + args.Key + ")"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "history for skill: %s (%d version%s)\n", args.Key, len(entries), plural(len(entries)))
	for _, e := range entries {
		fmt.Fprintf(&b, "  #%d %s\n", e.ID, renderSkillProvenance(e))
	}
	return strings.TrimRight(b.String(), "\n")
}

// handleSaveSkill creates a new skill, active immediately. MAIN AGENT ONLY.
func (a *App) handleSaveSkill(ctx context.Context, tc proxy.ToolCall) string {
	if a.IsSubagent {
		return skillMainOnlyDenied
	}
	s := a.getSkillStore()
	if s == nil {
		return skillsUnavailable
	}
	var args struct {
		Key         string `json:"key"`
		Value       string `json:"value"`
		Description string `json:"description,omitempty"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	// Reject if an active skill already exists — save is create-only.
	if _, err := s.getActiveSkill(ctx, args.Key); err == nil {
		return "ERROR: skill already exists: " + args.Key + " — use update_skill to change it"
	} else if err != memory.ErrNotFound {
		return fmt.Sprintf("ERROR: save skill: %v", err)
	}

	value := args.Value
	if args.Description != "" {
		value = args.Description + "\n" + value
	}
	// Validate BEFORE confirming so a doomed write never prompts.
	if err := validateSkillKey(args.Key); err != nil {
		return "ERROR: " + err.Error()
	}
	if err := validateSkillValue(value); err != nil {
		return "ERROR: " + err.Error()
	}
	if !a.Confirm("save_skill", "Save new skill to the global store?", args.Key, false) {
		return "[declined by user]"
	}
	e, err := s.putActiveSkill(ctx, args.Key, value, a.AgentPrefix, a.chatID(), a.computeTainted(), "")
	if err != nil {
		return fmt.Sprintf("ERROR: save skill: %v", err)
	}
	return fmt.Sprintf("saved skill: %s [id: %d]\n%s", e.Key, e.ID, renderSkillProvenance(e))
}

// handleUpdateSkill supersedes an existing skill with new content. MAIN AGENT ONLY.
func (a *App) handleUpdateSkill(ctx context.Context, tc proxy.ToolCall) string {
	if a.IsSubagent {
		return skillMainOnlyDenied
	}
	s := a.getSkillStore()
	if s == nil {
		return skillsUnavailable
	}
	var args struct {
		Key         string `json:"key"`
		Value       string `json:"value"`
		Description string `json:"description,omitempty"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	// Must already exist — update is not create.
	if _, err := s.getActiveSkill(ctx, args.Key); err == memory.ErrNotFound {
		return "ERROR: skill not found: " + args.Key + " — use save_skill to create it"
	} else if err != nil {
		return fmt.Sprintf("ERROR: update skill: %v", err)
	}

	value := args.Value
	if args.Description != "" {
		value = args.Description + "\n" + value
	}
	if err := validateSkillKey(args.Key); err != nil {
		return "ERROR: " + err.Error()
	}
	if err := validateSkillValue(value); err != nil {
		return "ERROR: " + err.Error()
	}
	if !a.Confirm("update_skill", "Update skill in the global store (old version kept in history)?", args.Key, false) {
		return "[declined by user]"
	}
	e, err := s.putActiveSkill(ctx, args.Key, value, a.AgentPrefix, a.chatID(), a.computeTainted(), "")
	if err != nil {
		return fmt.Sprintf("ERROR: update skill: %v", err)
	}
	return fmt.Sprintf("updated skill: %s [id: %d]\n%s", e.Key, e.ID, renderSkillProvenance(e))
}

// handleForgetSkill tombstones the active skill. MAIN AGENT ONLY.
func (a *App) handleForgetSkill(ctx context.Context, tc proxy.ToolCall) string {
	if a.IsSubagent {
		return skillMainOnlyDenied
	}
	s := a.getSkillStore()
	if s == nil {
		return skillsUnavailable
	}
	var args struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
		return fmt.Sprintf("ERROR: could not parse arguments: %v", err)
	}
	// Validate the key and confirm existence BEFORE prompting, matching
	// save/update: a doomed forget must never prompt the user.
	if err := validateSkillKey(args.Key); err != nil {
		return "ERROR: " + err.Error()
	}
	if _, err := s.getActiveSkill(ctx, args.Key); err == memory.ErrNotFound {
		return "ERROR: skill not found: " + args.Key
	} else if err != nil {
		return fmt.Sprintf("ERROR: forget skill: %v", err)
	}
	if !a.Confirm("forget_skill", "Forget skill from the global store (tombstone — still in history)?", args.Key, false) {
		return "[declined by user]"
	}
	if err := s.forgetSkill(ctx, args.Key); err == memory.ErrNotFound {
		return "ERROR: skill not found: " + args.Key
	} else if err != nil {
		return fmt.Sprintf("ERROR: forget skill: %v", err)
	}
	return "forgotten skill: " + args.Key + " (tombstoned — still visible in skill_history)"
}

// ─── Rendering ──────────────────────────────────────────────────────────────

// renderSkillProvenance renders the one-line provenance header for a skill
// entry. Skill-specific — NOT the memory renderProvenance (which says
// "durable-tier", "taint-unknown", etc.). Format:
//
//	[skill | active | 2026-07-19 | by main | tainted]
func renderSkillProvenance(e *memory.Entry) string {
	var parts []string
	parts = append(parts, "skill")
	parts = append(parts, e.Status)
	parts = append(parts, time.UnixMilli(e.CreatedAt).UTC().Format("2006-01-02"))
	parts = append(parts, "by "+e.Writer)
	if e.Tainted == memory.TaintTrue {
		parts = append(parts, "tainted")
	}
	return "[" + strings.Join(parts, " | ") + "]"
}

// skillProvenanceSuffix renders a compact inline provenance tag for list
// lines, e.g. " [main, 2026-07-19]".
func skillProvenanceSuffix(e *memory.Entry) string {
	return fmt.Sprintf(" [%s, %s]", e.Writer, time.UnixMilli(e.CreatedAt).UTC().Format("2006-01-02"))
}

// snippet returns the first n chars of s with an ellipsis if truncated.
func snippet(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// plural returns "s" when n != 1.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
