package agent

import (
	"regexp"
	"strconv"
	"strings"
)

// toolResult is the typed return value of ExecuteToolCall and handleToolCall.
// It replaces the former string protocol where errors were identified by an
// "ERROR:" prefix (and declines by "[declined by user]"). The prefix-based
// protocol had a correctness bug: legitimate tool output that happened to start
// with "ERROR:" was misclassified as a tool error, and vice-versa.
//
// WP-6.8 centralizes the prefix classification into stringToToolResult (the
// single sanctioned bridge from handler-level string returns to the typed
// boundary). Downstream code now uses result.ok instead of re-sniffing
// prefixes — eliminating duplicate classification sites. The Contains check
// was removed because it misclassified legitimate output containing ERROR:
// lines (e.g. log files). formatResult now prefixes errors at the start so
// HasPrefix catches them.
//
// ok is true for successful results (including "(no output)"); false for errors
// and user declines. text is the human-readable result string that ultimately
// reaches the model transcript and TUI — identical to what the old string
// protocol returned.
//
// Individual handlers in tool_handlers.go (and app.go) still return string and
// are wrapped at the ExecuteToolCall dispatch boundary via stringToToolResult.
// This keeps the handler bodies untouched in the initial WP-6.8 pass; a future
// pass can migrate handlers to return toolResult directly if desired.
type toolResult struct {
	ok      bool
	text    string
	endTurn bool
}

// String renders the toolResult to its string form for the transcript, TUI,
// and any consumer that still needs the raw text. This is the only sanctioned
// string boundary — callers should not inspect the text field directly to
// classify success/failure.
func (r toolResult) String() string {
	return r.text
}

// stringToToolResult wraps a handler's string return into a toolResult by
// inspecting the conventional prefixes. This is the bridge between the
// handler layer (which still returns string) and the typed toolResult boundary.
//
// The prefix checks here are authoritative: they are the ONLY place that
// pattern-matches on "ERROR:" and "[declined by user]". All other code should
// use result.ok instead of re-sniffing prefixes.
//
// execToolResult carries the tool name and (for run_shell) the command text
// so the classifier can apply the one deliberate exception: a compound
// command ending in an echo of $? whose last line prints a non-zero value
// is a masked failure and must be ok=false (see maskedExitEcho).
func stringToToolResult(s string) toolResult {
	return stringToToolResultFor("", "", s)
}

func stringToToolResultFor(toolName, command, s string) toolResult {
	if s == "[declined by user]" || strings.HasPrefix(s, "[declined by user]") {
		return toolResult{ok: false, text: s}
	}
	if strings.HasPrefix(s, "ERROR:") {
		return toolResult{ok: false, text: s}
	}
	if toolName == "run_shell" && maskedExitEcho(command, s) {
		return toolResult{ok: false, text: s}
	}
	return toolResult{ok: true, text: s}
}

// dollarQuestionEcho reports whether a shell command's text ends with an
// echo of the previous command's exit status — `; echo "exit=$?"`,
// `&& echo exit=$?`, `(echo $?)` or a bare `echo exit=$?`. No other guessing
// is done from output text: a command that merely prints "exit=1" without
// echoing $? is not a masked failure. The `exit=` marker is required in the
// command text (the shape the echo prints), so a bare `echo $?` is not
// matched — only the masked-exit pattern the task calls for.
var dollarQuestionEcho = regexp.MustCompile(`(;|&&|\|\||\()\s*echo\s+"?exit=\$?\?"?\s*\)?\s*$`)

// lastExitEcho parses the last non-empty line of output when it is exactly
// "exit=<n>" (the shape such an echo prints) and returns n.
var lastExitEcho = regexp.MustCompile(`^exit=(\d+)$`)

// maskedExitEcho reports whether command ends with an echo of $? and the
// last output line carries a non-zero value in that echo — the only case
// where a compound command's own exit status (the echo's, always 0) hides
// a real failure from the struggle trace and the model.
func maskedExitEcho(command, out string) bool {
	if !dollarQuestionEcho.MatchString(strings.TrimRight(command, " \t\n")) {
		return false
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		m := lastExitEcho.FindStringSubmatch(line)
		if m == nil {
			return false
		}
		n, err := strconv.Atoi(m[1])
		return err == nil && n != 0
	}
	return false
}

// okResult is a convenience constructor for a successful result.
func okResult(text string) toolResult {
	return toolResult{ok: true, text: text}
}

// errResult is a convenience constructor for an error result.
func errResult(text string) toolResult {
	return toolResult{ok: false, text: text}
}

// StringToToolResult is the exported form of stringToToolResult so external
// packages (e.g. cmd/wakil tests) can construct toolResult values for
// MakeTraceEntry and similar APIs without accessing unexported symbols.
func StringToToolResult(s string) toolResult {
	return stringToToolResult(s)
}
