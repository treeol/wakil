package agent

import "strings"

// toolResult is the typed return value of ExecuteToolCall and handleToolCall.
// It replaces the former string protocol where errors were identified by an
// "ERROR:" prefix (and declines by "[declined by user]"). The prefix-based
// protocol had a correctness bug: legitimate tool output that happened to start
// with "ERROR:" was misclassified as a tool error, and vice-versa.
//
// WP-6.8 centralizes the prefix classification into stringToToolResult (the
// single sanctioned bridge from handler-level string returns to the typed
// boundary). Downstream code now uses result.ok instead of re-sniffing
// prefixes — eliminating duplicate classification sites. The full fix (handlers
// returning toolResult natively via okResult/errResult, bypassing the bridge
// for cases where the output text is ambiguous) is a future pass; the typed
// boundary makes that possible.
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
	ok   bool
	text string
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
func stringToToolResult(s string) toolResult {
	if s == "[declined by user]" || strings.HasPrefix(s, "[declined by user]") {
		return toolResult{ok: false, text: s}
	}
	if strings.HasPrefix(s, "ERROR:") || strings.Contains(s, "\nERROR:") {
		return toolResult{ok: false, text: s}
	}
	return toolResult{ok: true, text: s}
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
