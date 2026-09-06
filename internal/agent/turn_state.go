package agent

// turn_state.go: per-turn and per-session turn-tracking state for App (WP-6.3
// extraction). Embedded in App so all field access is unchanged via Go's
// promoted-field access.

type turnState struct {
	// WorkflowStepTrace accumulates tool-call evidence during an IMPLEMENT turn.
	// Reset to nil at the start of each turn in runTurn; consumed by
	// handleWorkflowTransition when %%STEP_DONE%% is detected.
	WorkflowStepTrace []ToolTraceEntry

	// recentTraces is a rolling buffer of the most recent tool-call evidence
	// records across all turns and phases (unlike WorkflowStepTrace, which is
	// IMPLEMENT-only and resets each turn). mashura__debug reads it, and the
	// struggle detector scans it. Capped at mashuraRecentTraceCap entries.
	recentTraces []ToolTraceEntry

	// struggleSuggested dedupes the struggle-trigger hint so the same symptom is
	// offered at most once per session.
	struggleSuggested map[string]bool

	// CtxPressureWarned tracks whether the usable-budget pressure notice has
	// already been shown for the current high-occupancy stretch; it re-arms once
	// occupancy drops back under the usable budget so the warning fires once per
	// crossing rather than every turn.
	CtxPressureWarned bool

	// learnNudgePending holds the whitespace-normalised query when the current
	// turn fired the learn-candidate log; cleared by runTurn after the turn.
	learnNudgePending string

	// learnNudgedQueries is a per-session (process-lifetime) set of queries for
	// which the end-of-turn nudge has already been shown — prevents repeats.
	learnNudgedQueries map[string]bool
}
