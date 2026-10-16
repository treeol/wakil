package agent

// skill_suggestion.go — Part B1+B2 of the skill system: deterministic
// workflow-repetition tracking with an in-band invitation to save a reusable
// procedure as a skill.
//
// Design (Mashūra-reviewed, Part A/B3 precedent):
//   - Tracking is deterministic: per-turn successful tool-call NAME sequences,
//     recorded in finalizeToolResult (the single bookkeeping path for
//     sequential, assist-synthesized, and parallel-block results).
//   - B2 (repetition): the current turn's sequence reaches ≥4 successful
//     calls, ≥2 distinct tool names, ≥1 non-read tool, and replays the
//     opening of a previous qualifying turn (current is a prefix of that
//     turn's sequence). B2 takes precedence over B1 when both fire.
//   - B1 (long workflow): ≥8 successful calls with ≥1 non-read tool.
//   - Delivery: the hint is appended to the tool result text that crossed the
//     threshold — the same in-band pattern as load_skill's refinement hint
//     (B3). No synthetic user message, no extra model call, no turn-end
//     timing problem. The hint is non-blocking ("when the task is done,
//     consider...") so the model doesn't interleave skill drafting into
//     in-flight work.
//   - Names-only matching is a WEAK candidate signal (both panels said so);
//     the hint wording reflects that — it invites judgment, never asserts a
//     reusable workflow exists.
//   - Hints are capped at 2 per session (in-memory, main agent only). The
//     write itself always flows through save_skill/update_skill's interactive
//     confirm gate — the hint never persists anything.
//   - Skill/memory/counsel/subagent bookkeeping tools are excluded so hint
//     machinery doesn't contaminate its own fingerprints.

import (
	"strings"
)

// skillSuggestion* thresholds and caps for the B1/B2 hint logic.
const (
	// b2MinCalls / b2MinDistinct / b1MinCalls gate the two triggers.
	b2MinCalls    = 4
	b2MinDistinct = 2
	b1MinCalls    = 8

	// skillHintMaxPerSession caps total B1/B2 hints per App lifetime (session
	// scope: the App is recreated on /new and resume).
	skillHintMaxPerSession = 2

	// skillHistoryMax bounds how many completed turn sequences are kept.
	skillHistoryMax = 20
)

// skillHintExcludedTools never qualify toward a workflow: bookkeeping around
// skills, memory, counsel, and subagent orchestration. Their presence would
// let the hint machinery fingerprint itself.
var skillHintExcludedTools = map[string]bool{
	"save_skill": true, "update_skill": true, "forget_skill": true,
	"load_skill": true, "list_skills": true, "skill_search": true, "skill_history": true,
	"memory_put": true, "memory_get": true, "memory_search": true, "memory_list": true,
	"memory_promote": true, "memory_reject": true, "memory_forget": true,
	"memory_promote_from_staging": true,
	"mashura__review":             true, "mashura__debug": true, "mashura__decide": true, "mashura__check": true,
	"dispatch_subagent": true, "dispatch_subagents": true,
	"check_pending": true, "wait_for_completion": true, "collapse_context": true, "read_process_log": true, "kill_process": true,
}

// skillHintNonReadTools are tools that (usually) change state. A qualifying
// workflow must contain at least one — pure exploration (read/grep) is normal
// agent behavior, not a skill candidate (panel feedback on B2 false positives).
var skillHintNonReadTools = map[string]bool{
	"run_shell": true, "run_background": true, "edit_file": true, "write_file": true,
	"write_binary_file": true, "delete_file": true, "move_file": true,
}

// skillSuggestionState is per-App, turn-goroutine-only state (same discipline
// as traceToolCalls). No mutex: finalizeToolResult and prepareTurn both run
// on the turn goroutine.
type skillSuggestionState struct {
	current        []string // successful qualifying tool names this turn
	history        [][]string
	hintsUsed      int
	hintedThisTurn bool
}

// recordSkillToolCall feeds one finalized tool call into the tracker and
// returns a hint string when a trigger fires ("" otherwise). Failures and
// excluded tools are ignored.
func (s *skillSuggestionState) recordSkillToolCall(name string, ok bool) string {
	if !ok || skillHintExcludedTools[name] {
		// Loading an existing skill suggests the workflow already IS a skill —
		// suppress the hint for that turn (no point inviting a duplicate save).
		if name == "load_skill" && ok {
			s.hintedThisTurn = true
		}
		return ""
	}
	if s.hintedThisTurn || s.hintsUsed >= skillHintMaxPerSession {
		s.current = append(s.current, name)
		return ""
	}
	s.current = append(s.current, name)

	// B2 takes precedence: repetition of a previous qualifying turn is the
	// more specific signal. Checked before B1 so a matching long turn gets
	// the repetition wording.
	if skillTurnQualifies(s.current) && skillTurnRepeats(s.current, s.history) {
		s.hintedThisTurn = true
		s.hintsUsed++
		return "\n\n[app note — not from the user: this turn's tool sequence so far matches an earlier turn this session (tool names only — arguments may differ)" +
			". If the procedure behind this turn is reusable reference material, consider drafting it as a skill (skill_search first for near-duplicates, then save_skill or update_skill). This is a weak signal — skip it if the work is one-off. Finish the user's task first; the skill draft can happen after.]"
	}
	// B1: long sequence with at least one state-changing tool — length alone
	// is the signal, distinctness not required.
	if len(s.current) >= b1MinCalls && skillTurnHasNonRead(s.current) {
		s.hintedThisTurn = true
		s.hintsUsed++
		return "\n\n[app note — not from the user: this turn has run a long multi-step sequence of tool calls" +
			". If the procedure behind this turn is reusable reference material, consider drafting it as a skill (skill_search first for near-duplicates, then save_skill or update_skill). This is a weak signal — skip it if the work is one-off. Finish the user's task first; the skill draft can happen after.]"
	}
	return ""
}

// skillTurnQualifies reports whether a sequence is a B2 candidate: long
// enough, varied enough, and containing at least one state-changing tool.
func skillTurnQualifies(seq []string) bool {
	if len(seq) < b2MinCalls {
		return false
	}
	distinct := make(map[string]bool, len(seq))
	for _, n := range seq {
		distinct[n] = true
	}
	return len(distinct) >= b2MinDistinct && skillTurnHasNonRead(seq)
}

// skillTurnHasNonRead reports whether any call in the sequence changes state.
func skillTurnHasNonRead(seq []string) bool {
	for _, n := range seq {
		if skillHintNonReadTools[n] {
			return true
		}
	}
	return false
}

// skillTurnRepeats reports whether the current sequence replays the opening
// of an earlier QUALIFYING turn: seq is a prefix of prev (≥ b2MinCalls calls
// matched). Only qualifying turns are committed to history (see
// commitSkillTurn), so a pure-read turn can never match. Prefix direction
// makes the hint fire as soon as the replay is recognizable rather than only
// at the end of the previous sequence's length.
func skillTurnRepeats(seq []string, history [][]string) bool {
	if len(seq) < b2MinCalls {
		return false
	}
	for _, prev := range history {
		if len(prev) < len(seq) {
			continue
		}
		match := true
		for i := range seq {
			if prev[i] != seq[i] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// commitSkillTurn finalizes the current turn's sequence into history. Called
// from prepareTurn (start of the NEXT turn) so a completed turn's sequence is
// available for B2 matching during later turns — never during its own turn
// (no self-match). prepareTurn runs once per user turn (app.go Send entry),
// not per model iteration. Only qualifying turns are committed so trivial
// turns don't evict real workflows and pure-read history can never match.
func (s *skillSuggestionState) commitSkillTurn() {
	if len(s.current) > 0 && skillTurnQualifies(s.current) {
		s.history = append(s.history, s.current)
		if len(s.history) > skillHistoryMax {
			s.history = s.history[len(s.history)-skillHistoryMax:]
		}
	}
	s.current = nil
	s.hintedThisTurn = false
}

// appendSkillHint is called from finalizeToolResult: record the call and, if
// a trigger fired, append the hint to the result text before it enters the
// transcript.
func (a *App) appendSkillHint(tcName string, ok bool, text string) string {
	if a.IsSubagent || a.SkillStore == nil {
		return text
	}
	if a.skillSuggest == nil {
		a.skillSuggest = &skillSuggestionState{}
	}
	if hint := a.skillSuggest.recordSkillToolCall(tcName, ok); hint != "" {
		return text + hint
	}
	return text
}

// formatSkillSequence renders a sequence for diagnostics (tests/logs).
func formatSkillSequence(seq []string) string {
	return strings.Join(seq, " → ")
}
