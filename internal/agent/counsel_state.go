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
}
