package exec

import (
	"fmt"
	"regexp"
	"strconv"
)

// ─── Backgrounded-shell exit-code marker (card #121 follow-up) ─────────────
//
// Backgrounded shell commands are not waited on directly: their exit status
// lives only in the process table, and the poll-based liveness check
// (IsProcessAlive) cannot recover it. Without the marker, a failing command
// is indistinguishable from a successful one — the inline within-deadline
// result and the detached completion notice both looked like success
// ("(no output)") even for `exit 3`.
//
// WrapExitMarker appends a sentinel to the command so the exit code lands at
// the very end of the log file, where ParseExitMarker recovers it. The
// sentinel is POSIX-sh-only (no bash-isms), so it works in both the direct
// and docker executors.
//
// HONEST SECURITY NOTE: the sentinel is not authenticatable — a command can
// write to the (predictable) log path itself, and a spoofed marker that
// ends up last WILL parse as genuine. End-anchoring plus the required
// leading newline narrows the spoof window but does not close it. This is
// accepted: the marker's job is surfacing honest exit codes for the model,
// not defending against a hostile command that already runs with the user's
// consent. Callers fail CLOSED (code unknown) when no marker parses.

// exitMarkerRe matches the sentinel at the END of log content, preceded by a
// line boundary (the wrapper's printf always emits a leading newline).
// End-anchoring is deliberate: a command that merely echoes a similar string
// mid-output does not match — only a marker at EOF counts.
var exitMarkerRe = regexp.MustCompile(`(?:^|\n)\[wakil-exit:(\d+)\]\s*$`)

// WrapExitMarker wraps a command so its exit code is appended to logPath as
// a parseable sentinel line, and the process exits with the original code.
// logPath must be writable from the command's environment (the background
// machinery already guarantees this: container /tmp in docker mode, host
// /tmp in direct mode) and, by contract of every caller in this codebase,
// contains no single quotes (generated paths are digit-only /tmp names).
//
// The parentheses create a SUBSHELL deliberately: a command ending in
// `exit N` (e.g. `sleep 1; exit 3` — the smoke-test case) must exit the
// subshell only, so the wrapper still runs and captures N. A brace group
// `{ ...; }` would not — `exit` would kill the wrapper itself.
//
// A command ending in a trailing backslash has been verified safe by
// construction in E2E tests on dash and bash: the backslash+newline merges
// the closing paren into the command line (`( cmd)` — still a valid
// subshell), and the wrapper captures the subshell's code. (This is a
// tested-scope claim, not a universal grammar guarantee.)
//
// Limitations (documented, not silently hidden):
//   - A command SIGKILLed before the marker write leaves no marker —
//     ParseExitMarker reports ok=false and callers fail closed (ERROR /
//     "code unknown"), never silent success.
//   - A shell syntax error in the user command fails the whole wrapped
//     script's parse — the marker is never written; callers fail closed.
//   - A command that backgrounds its OWN children (`some_server & echo done`)
//     keeps the process GROUP alive until those children exit — the reaper
//     and liveness checks wait for the whole group (the wrapper only exits
//     after the subshell returns). A child may also write to the log AFTER
//     the marker, displacing it so ParseExitMarker reports ok=false. The
//     reported code, when the marker parses, is always the subshell's.
func WrapExitMarker(command, logPath string) string {
	return fmt.Sprintf("( %s\n)\n__wakil_rc=$?\nprintf '\\n[wakil-exit:%%d]\\n' \"$__wakil_rc\" >> '%s'\nexit $__wakil_rc",
		command, logPath)
}

// ParseExitMarker extracts the wrapper's exit code from the end of log
// content and returns the content with the marker removed. ok=false when no
// marker is present (process killed before the marker write, a shell syntax
// error, a self-backgrounded child writing after the marker, log not yet
// flushed, or a command started without WrapExitMarker) — callers must treat
// that as "code unknown" and fail closed where success/failure matters.
func ParseExitMarker(out string) (cleaned string, code int, ok bool) {
	m := exitMarkerRe.FindStringSubmatch(out)
	if m == nil {
		return out, 0, false
	}
	code, err := strconv.Atoi(m[1])
	if err != nil {
		return out, 0, false
	}
	// Strip the marker match — the boundary newline the wrapper's printf
	// emits before the marker is part of the match ((?:^|\n)), so cleaned is
	// exactly the command's original output, trailing blank lines preserved.
	cleaned = out[:len(out)-len(m[0])]
	return cleaned, code, true
}

// ExitStatusLine renders a human-readable exit status for notifications and
// log headers: "exited OK", "exited with code N", or the unknown variant.
func ExitStatusLine(code int, known bool) string {
	if !known {
		return "exit code unknown (completion marker missing or log unreadable)"
	}
	if code == 0 {
		return "exited OK"
	}
	return fmt.Sprintf("exited with code %d", code)
}
