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

// Failure is a single parsed test failure. The struct is deliberately minimal:
// v1 records only the top-level test identifier (the name `go test -run`
// targets). Subtest failures roll up to their top-level parent, because
// `-run '^Parent$'` reruns the whole parent including its subtests.
//
// File/line association is intentionally absent: go test output does not
// reliably pair test names with locations under -v/parallel/panic output, and
// adding a stateful association pass is a v2 concern (the known upgrade path
// is `go test -json`).
type Failure struct {
	Test string // top-level test function name, e.g. "TestFailingOne"
}

// Result is the outcome of running a single Command.
type Result struct {
	Command Command
	Status  Status
	// Output is the combined stdout+stderr, truncated to OutputCap bytes.
	// CapOutput keeps the HEAD of the output (the first OutputCap bytes); the
	// tail is dropped. Failure summaries commonly appear near the end, which
	// is why Failures is parsed from the raw output before this cap is applied.
	Output string
	// Failures is the structured list of parsed test failures. Empty (nil) for
	// pass/declined/error-with-no-output, and for runners whose output the
	// parser does not understand. Populated from the RAW output before it is
	// capped, so truncation never loses a failure line. Non-nil only for
	// StatusFail (a timed-out command may print partial failures, but rerun
	// semantics for timeouts are undefined — see ParseFailures).
	Failures []Failure
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
