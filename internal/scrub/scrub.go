// Package scrub provides pattern-based secret detection and redaction for
// text that may contain sensitive data (API keys, tokens, passwords, private
// keys, JWTs). It is designed to run at the SQLite store write path before
// event payloads are persisted, ensuring trace data crossing tenant
// boundaries does not leak credentials.
//
// The scrubber is intentionally conservative: it prefers false negatives
// (missing a secret) over false positives (redacting legitimate code). A
// false positive in a trace destroys debugging value; a false negative is
// caught by the downstream consumer's own scrubbing if needed.
package scrub

import (
	"fmt"
	"regexp"
	"strings"
)

// Level controls scrubbing aggressiveness.
type Level int

const (
	// LevelOff disables scrubbing entirely.
	LevelOff Level = iota
	// LevelStandard covers common high-confidence secret patterns.
	LevelStandard
	// LevelAggressive adds generic high-entropy string detection (more
	// false positives, use for high-security environments).
	LevelAggressive
)

// Pattern represents a single scrubbing rule.
type pattern struct {
	name    string
	re      *regexp.Regexp
	replace string
}

// Scrubber redacts secrets from text. Implementations must be safe for
// concurrent use.
type Scrubber interface {
	Scrub(text string) string
}

// noOpScrubber returns text unchanged.
type noOpScrubber struct{}

func (noOpScrubber) Scrub(text string) string { return text }

// NoOp returns a Scrubber that does nothing (for --scrub-level=off).
func NoOp() Scrubber { return noOpScrubber{} }

// PatternScrubber redacts known secret patterns from text.
type PatternScrubber struct {
	patterns []pattern
}

// New creates a PatternScrubber at the given level.
func New(level Level) *PatternScrubber {
	if level == LevelOff {
		return &PatternScrubber{}
	}

	ps := &PatternScrubber{}

	// Standard patterns — high confidence.
	ps.add("bearer", `(?i)Bearer\s+[A-Za-z0-9\-._~+/]+=*`, "[REDACTED:bearer]")
	ps.add("openai_key", `sk-[A-Za-z0-9]{20,}`, "[REDACTED:openai_key]")
	ps.add("anthropic_key", `sk-ant-[A-Za-z0-9\-_]{20,}`, "[REDACTED:anthropic_key]")
	ps.add("github_pat", `gh[pousr]_[A-Za-z0-9]{36,}`, "[REDACTED:github_token]")
	ps.add("google_api", `AIza[A-Za-z0-9\-_]{35}`, "[REDACTED:google_api_key]")
	ps.add("aws_access", `AKIA[A-Z0-9]{16}`, "[REDACTED:aws_access_key]")
	ps.add("aws_secret", `(?i)aws_secret_access_key["'\s:=]+[A-Za-z0-9/+=]{40}`, "[REDACTED:aws_secret_key]")
	ps.add("private_key", `-----BEGIN [A-Z ]+PRIVATE KEY-----`, "[REDACTED:private_key]")
	ps.add("jwt", `eyJ[A-Za-z0-9_-]+\.eyJ[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`, "[REDACTED:jwt]")
	ps.add("connstring_pwd", `([a-z][a-z0-9+]*)://([^:\s]+):([^\s@]+)@`, "${1}://${2}:[REDACTED:password]@")

	if level >= LevelAggressive {
		// Generic high-entropy tokens — only when preceded by a secret-like keyword.
		ps.add("generic_token", `(?i)(token|secret|key|password|passwd|auth|credential)["'\s:=]+[A-Za-z0-9+/]{40,}={0,2}`, "[REDACTED:secret]")
	}

	return ps
}

func (ps *PatternScrubber) add(name, pat, replace string) {
	ps.patterns = append(ps.patterns, pattern{
		name:    name,
		re:      regexp.MustCompile(pat),
		replace: replace,
	})
}

// Scrub redacts all matching patterns from text. Patterns are applied
// sequentially; later patterns see the output of earlier ones.
func (ps *PatternScrubber) Scrub(text string) string {
	if ps == nil || len(ps.patterns) == 0 {
		return text
	}
	for _, p := range ps.patterns {
		text = p.re.ReplaceAllString(text, p.replace)
	}
	return text
}

// ParseLevel converts a string flag to a Level.
func ParseLevel(s string) (Level, error) {
	switch strings.ToLower(s) {
	case "off", "disabled", "none":
		return LevelOff, nil
	case "standard", "default", "normal":
		return LevelStandard, nil
	case "aggressive", "strict":
		return LevelAggressive, nil
	default:
		return 0, fmt.Errorf("scrub: unknown level %q (want off|standard|aggressive)", s)
	}
}
