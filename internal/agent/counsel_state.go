package agent

// counselState holds the unexported, per-turn auto-counsel bookkeeping extracted
// from the App god struct (WP-6.3). Embedded in App, so selector access is
// unchanged (a.counselCalls, a.autoCounselSkipGate).
//
// Only the two unexported fields live here. The exported counsel configuration
// fields (AutoCounsel, MaxCounsel, CounselMode) stay flat on App because they are
// set by cross-package composite literals (cmd/wakil/app_builder.go) — moving them
// into an unexported embedded struct would make those literals impossible.
type counselState struct {
	// counselCalls counts auto-counsel calls fired this turn (TUI path) or
	// this session (headless path). Reset at the start of each Send() when
	// CounselMode is set.
	counselCalls int

	// autoCounselSkipGate, when true, tells the next handleMashura call to
	// bypass the a.Confirm gate. Set immediately before an auto-counsel fire
	// when AutoApprove=true; consumed (cleared) by handleMashura.
	autoCounselSkipGate bool

	// lastStruggleEval is the read-only snapshot of the most recent struggle
	// evaluation, written by maybeSuggestDebug on every entry (including
	// silent exits) and read by the bare /counsel diagnostics. It is the
	// state the detector actually saw, not a re-evaluation.
	lastStruggleEval struggleEvaluation
}

// struggleEvaluation records one auto-counsel evaluation as the detector saw
// it, for the bare /counsel diagnostics. It is written even when the
// evaluation stays silent, so a silent run is explainable after the fact.
type struggleEvaluation struct {
	// TraceWindow is the rolling trace as the detector saw it (tool, target,
	// ok/failed, repeat marker). Empty means no tool calls had been recorded.
	TraceWindow string
	// RuleMatched is the detector rule that fired, or "none".
	RuleMatched string
	// Symptom is the detector symptom when a rule fired.
	Symptom string
	// ResolvedMode is the effective mode from resolveCounselMode (explicit
	// choice, or the level when no choice is set).
	ResolvedMode string
	// ModeSource names where the mode came from (/counsel, config
	// auto_counsel, level <name>, or the legacy AutoCounsel bool).
	ModeSource string
	// Level is the effective Mashūra level for the review tool.
	Level string
	// ActionTaken is what the turn did: hint, auto-fire, or a reason nothing
	// was shown (off, cap, ceiling, unavailable, dedup, no detection).
	ActionTaken string
}
