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
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

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
	if errors.Is(err, memory.ErrNotFound) {
		return "not found: " + args.Key + " (no active skill with that key — use list_skills to see available skills)"
	}
	if err != nil {
		return fmt.Sprintf("ERROR: load skill: %v", err)
	}
	out := fmt.Sprintf("# skill: %s\n%s\n\n%s", e.Key, renderSkillProvenance(e), e.Value)
	// Refinement invitation (skill system Part B3): ask the model to propose
	// an update when it finds the content wrong or outdated. The write itself
	// always goes through update_skill's interactive confirm gate — this note
	// only surfaces the option; it never auto-triggers anything.
	if !a.IsSubagent {
		out += "\n\n[If you find this skill content is wrong, outdated, or missing something you had to figure out yourself, propose the correction via update_skill with the improved content — the user will review the diff.]"
	}
	return out
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
	} else if !errors.Is(err, memory.ErrNotFound) {
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
	// Secret screening: refuse to persist embedded secrets to the global store.
	if msg := screenSkillSecrets(value); msg != "" {
		return "ERROR: " + msg
	}
	if !a.Confirm("save_skill", fmt.Sprintf("Save new skill %q to the global store?", args.Key), fmt.Sprintf("Size: %d bytes\nTainted: %s\n\nContent preview:\n%s", len(value), taintLabel(a.computeTainted()), previewSkillValue(value)), false) {
		return "[declined by user]"
	}
	e, err := s.putActiveSkill(ctx, args.Key, value, a.AgentPrefix, a.chatID(), a.computeTainted(), false, "")
	if errors.Is(err, memory.ErrSkillExists) {
		// Race-safe backstop: a concurrent save won between our pre-check and
		// the write transaction. The store enforced create-only atomically.
		return "ERROR: skill already exists: " + args.Key + " — use update_skill to change it"
	}
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
	// Fetch the existing content for the diff preview.
	existingEntry, err := s.getActiveSkill(ctx, args.Key)
	if errors.Is(err, memory.ErrNotFound) {
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
	// No-op guard: identical content must not prompt or create a history
	// version. An update that changes nothing is a model mistake, not work.
	if value == existingEntry.Value {
		return fmt.Sprintf("no-op: skill %q already has this exact content — nothing updated", args.Key)
	}
	// Secret screening: refuse to persist embedded secrets to the global store.
	if msg := screenSkillSecrets(value); msg != "" {
		return "ERROR: " + msg
	}
	if !a.Confirm("update_skill", fmt.Sprintf("Update skill %q in the global store (old version kept in history)?", args.Key), fmt.Sprintf("Old: %d bytes\nNew: %d bytes\nTainted: %s\n\nDiff (old → new):\n%s\n\nNew content preview:\n%s", len(existingEntry.Value), len(value), taintLabel(a.computeTainted()), skillLineDiff(existingEntry.Value, value), previewSkillValue(value)), false) {
		return "[declined by user]"
	}
	e, err := s.putActiveSkill(ctx, args.Key, value, a.AgentPrefix, a.chatID(), a.computeTainted(), true, "")
	if errors.Is(err, memory.ErrSkillNotFound) {
		// Race-safe backstop: a concurrent forget won between our pre-check and
		// the write transaction. The store enforced requires-existing atomically.
		return "ERROR: skill not found: " + args.Key + " — use save_skill to create it"
	}
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
	existingEntry, err := s.getActiveSkill(ctx, args.Key)
	if errors.Is(err, memory.ErrNotFound) {
		return "ERROR: skill not found: " + args.Key
	} else if err != nil {
		return fmt.Sprintf("ERROR: forget skill: %v", err)
	}
	if !a.Confirm("forget_skill", fmt.Sprintf("Forget skill %q from the global store (tombstone — still in history)?", args.Key), fmt.Sprintf("Size: %d bytes\nTainted: %s", len(existingEntry.Value), taintLabel(existingEntry.Tainted)), false) {
		return "[declined by user]"
	}
	if err := s.forgetSkill(ctx, args.Key); errors.Is(err, memory.ErrNotFound) {
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

// previewSkillValue returns a truncated preview of skill content for use in
// confirm dialogs. Shows the first 500 bytes (UTF-8 safe) with an ellipsis
// if longer.
func previewSkillValue(value string) string {
	const previewMax = 500
	if len(value) <= previewMax {
		return value
	}
	// Find the last rune boundary at or before previewMax.
	end := previewMax
	for end > 0 && (value[end]&0xC0) == 0x80 {
		end--
	}
	return value[:end] + "…[" + fmt.Sprintf("%d", len(value)-end) + " more bytes]"
}

// taintLabel returns a human-readable string for the tainted state.
func taintLabel(tainted int) string {
	switch tainted {
	case memory.TaintTrue:
		return "yes (external content)"
	case memory.TaintFalse:
		return "no"
	default:
		return "unknown"
	}
}

// skillDiffMaxBytes caps the rendered diff body in the update confirm. When
// exceeded, the diff is truncated at a rune boundary and a note points at a
// spilled file holding the FULL proposed content (the preview is only 500
// bytes, so for large updates the spill is the only complete view).
const skillDiffMaxBytes = 2000

// skillLineDiff renders a compact line-level diff (old vs new) for the
// update_skill confirm dialog. Hand-rolled LCS on lines — no external diff
// dependency. Dangerous invisible characters (controls, bidi overrides) are
// stripped so terminal renderers cannot be misled; huge single lines are
// truncated per-line.
func skillLineDiff(oldVal, newVal string) string {
	oldLines := diffLines(oldVal)
	newLines := diffLines(newVal)

	// LCS table. Bounded: diff input is capped by validateSkillValue's 256KiB,
	// but a pathological all-different case would allocate a huge table, so
	// guard on line count and fall back to a summary.
	const maxDiffLines = 2000
	if len(oldLines) > maxDiffLines || len(newLines) > maxDiffLines {
		return fmt.Sprintf("(too many lines to diff — old %d lines, new %d lines; byte counts above, full new content in the spilled file if present)", len(oldLines), len(newLines))
	}
	lcs := make([][]int, len(oldLines)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(newLines)+1)
	}
	for i := len(oldLines) - 1; i >= 0; i-- {
		for j := len(newLines) - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else if lcs[i+1][j] >= lcs[i][j+1] {
				lcs[i][j] = lcs[i+1][j]
			} else {
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}

	var b strings.Builder
	i, j := 0, 0
	for i < len(oldLines) && j < len(newLines) {
		switch {
		case oldLines[i] == newLines[j]:
			i++
			j++
		case lcs[i+1][j] >= lcs[i][j+1]:
			fmt.Fprintf(&b, "- %s\n", sanitizeDiffLine(oldLines[i]))
			i++
		default:
			fmt.Fprintf(&b, "+ %s\n", sanitizeDiffLine(newLines[j]))
			j++
		}
	}
	for ; i < len(oldLines); i++ {
		fmt.Fprintf(&b, "- %s\n", sanitizeDiffLine(oldLines[i]))
	}
	for ; j < len(newLines); j++ {
		fmt.Fprintf(&b, "+ %s\n", sanitizeDiffLine(newLines[j]))
	}

	out := strings.TrimRight(b.String(), "\n")
	if out == "" {
		// No line-level changes after CRLF/trailing-newline normalization —
		// but the no-op guard does byte equality, so this may still be a
		// newline-convention-only change. Say so instead of an empty diff.
		return "(no line-level changes — differs only in line endings/trailing newline; byte counts above are exact)"
	}
	if len(out) > skillDiffMaxBytes {
		// Cut at a rune boundary and note the truncation.
		cut := out[:skillDiffMaxBytes]
		for len(cut) > 0 && !utf8.ValidString(cut) {
			_, size := utf8.DecodeLastRuneInString(cut)
			if size == 0 {
				break
			}
			cut = cut[:len(cut)-size]
		}
		out = cut + "\n… (diff truncated — the full NEW content is in the spilled file if one was written; sizes are exact)"
	}
	return out
}

// diffLines splits s into lines, tolerating CRLF and a missing final newline.
func diffLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// sanitizeDiffLine strips characters that can mislead terminal renderers:
// C0 controls (except tab), DEL, C1 controls, and Unicode bidi overrides
// (which can make "- old / + new" read backwards). Escaping would be more
// informative but these should never appear in skill content anyway.
func sanitizeDiffLine(line string) string {
	line = strings.Map(func(r rune) rune {
		if (r < 32 && r != '\t') || r == 0x7F || (r >= 0x80 && r <= 0x9F) ||
			r == 0x202A || r == 0x202B || r == 0x202C || r == 0x202D || r == 0x202E ||
			r == 0x2066 || r == 0x2067 || r == 0x2068 || r == 0x2069 {
			return -1
		}
		return r
	}, line)
	const maxLine = 200
	if len(line) > maxLine {
		cut := line[:maxLine]
		for len(cut) > 0 && !utf8.ValidString(cut) {
			_, size := utf8.DecodeLastRuneInString(cut)
			if size == 0 {
				break
			}
			cut = cut[:len(cut)-size]
		}
		line = cut + "…"
	}
	return line
}
