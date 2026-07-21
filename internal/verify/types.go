// Package verify implements deterministic verification for the workflow:
// it detects test/build/lint commands from project manifests and runs them,
// producing a structured pass/fail result. This is the deterministic gate
// that complements (not replaces) the advisory oracle final review.
//
// This package is PURE logic: detection rules, result types, and
// formatting. It does not import internal/agent or internal/exec and does
// not perform I/O. Execution and consent gating belong in the agent layer
// (internal/agent/verify.go) — verification commands are repo-derived
// (detected from files), so a malicious repository could plant a malicious
// "test" script. Routing through app.Confirm (policy + SuspendAuto) is
// mandatory before any command is executed.
package verify

// Status is the outcome of running a single verification command.
type Status string

const (
	// StatusPass: command exited 0.
	StatusPass Status = "pass"
	// StatusFail: command exited non-zero.
	StatusFail Status = "fail"
	// StatusTimeout: command exceeded its timeout.
	StatusTimeout Status = "timeout"
	// StatusDeclined: user or policy declined the command (consent gate).
	StatusDeclined Status = "declined"
	// StatusError: runner/internal error (executor unavailable, etc.).
	StatusError Status = "error"
	// StatusSkipped: no commands configured or detected, or disabled.
	StatusSkipped Status = "skipped"
)

// Command is one verification command to run.
type Command struct {
	// Cmd is the shell command string, e.g. "go test ./...".
	Cmd string
	// Source is how the command was determined: "config", "detect", or
	// "default". Used in the log so the origin is auditable.
	Source string
}

// Result is the outcome of running a single Command.
type Result struct {
	Command Command
	Status  Status
	// Output is the combined stdout+stderr, truncated to OutputCap bytes.
	Output string
	// DurationMs is the wall-clock time the command took.
	DurationMs int64
	// ExitCode is the process exit code (meaningful for StatusFail).
	ExitCode int
	// Reason is a human-readable explanation for non-pass statuses.
	Reason string
}

// Outcome summarizes a full verification run (one or more commands).
type Outcome struct {
	Results []Result
}

// Passed reports whether all commands in the outcome passed.
// An empty outcome (no commands ran) is treated as passed=false with
// status skipped — "no tests detected" is never a silent pass.
func (o Outcome) Passed() bool {
	if len(o.Results) == 0 {
		return false
	}
	for _, r := range o.Results {
		if r.Status != StatusPass {
			return false
		}
	}
	return true
}

// HasFailures reports whether any command failed (non-zero exit).
// Timeouts and errors are also treated as failures (fail-closed).
func (o Outcome) HasFailures() bool {
	for _, r := range o.Results {
		switch r.Status {
		case StatusFail, StatusTimeout, StatusError:
			return true
		}
	}
	return false
}

// WasSkipped reports whether verification was skipped entirely (no
// commands ran — either disabled or nothing detected).
func (o Outcome) WasSkipped() bool {
	return len(o.Results) == 0
}

// AnyDeclined reports whether any command was declined by the consent gate.
func (o Outcome) AnyDeclined() bool {
	for _, r := range o.Results {
		if r.Status == StatusDeclined {
			return true
		}
	}
	return false
}
