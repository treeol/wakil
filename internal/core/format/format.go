// Package format provides neutral text utility functions extracted from
// internal/agent so the TUI and other agent-free packages (sessionclient,
// wiring) can use them without importing internal/agent.
//
// These are pure string/slice utilities with no dependencies beyond the
// standard library and internal/proxy (for TranscriptSize). They are safe
// for concurrent use.
package format

import (
	"strings"

	"github.com/treeol/wakil/internal/proxy"
)

// DerefStr returns the string pointed to by p, or "" if p is nil.
func DerefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// Indent prefixes every line of s with two spaces.
func Indent(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		out = append(out, "  "+l)
	}
	return strings.Join(out, "\n")
}

// Truncate returns s truncated to n runes with an ellipsis appended if s
// exceeds n runes. If s is n runes or shorter, it is returned unchanged.
func Truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// TranscriptSize returns a cheap proxy for context size: the total bytes of
// content + tool-call arguments across all messages in conv.
func TranscriptSize(conv []proxy.Message) int {
	n := 0
	for _, m := range conv {
		n += len(DerefStr(m.Content))
		for _, tc := range m.ToolCalls {
			n += len(tc.Function.Arguments)
		}
	}
	return n
}
