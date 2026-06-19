package agent

import (
	"crypto/rand"
	"fmt"
	"strings"
)

// strPtr returns a pointer to s. Used when a *string field must hold a non-nil value.
func StrPtr(s string) *string { return &s }

// derefStr returns the string pointed to by p, or "" if p is nil.
func DerefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// --- UUID v4 ---

func NewChatID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func ShortID(s string) string {
	if len(s) >= 8 {
		return s[:8]
	}
	return s
}

// --- text utils ---

func Indent(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		out = append(out, "  "+l)
	}
	return strings.Join(out, "\n")
}

// --- ANSI helpers (used by app.go status lines written to progWriter) ---
// These emit raw ANSI; they're fine for text that goes into the viewport
// since Lip Gloss's renderer understands ANSI sequences.

func wrap(code, s string) string { return "\x1b[" + code + "m" + s + "\x1b[0m" }
func Bold(s string) string       { return wrap("1", s) }
func Dim(s string) string        { return wrap("2", s) }
func Red(s string) string        { return wrap("31", s) }
func Yellow(s string) string     { return wrap("33", s) }

// shellQuote wraps s in single quotes with internal single-quotes escaped.
// Use this to build shell commands from structured (model-supplied) arguments
// so the model can't inject metacharacters even if it tries.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
