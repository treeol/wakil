package ilm

import (
	"encoding/json"
	"regexp"
	"strings"
)

// RedactionPattern is one regex pattern from the redaction_v1 schema.
type RedactionPattern struct {
	Name        string `json:"name"`
	Pattern     string `json:"pattern"`
	Replacement string `json:"replacement"`
}

// AllowPattern overrides a redaction pattern — if the allow pattern matches
// the same text, the redaction is suppressed.
type AllowPattern struct {
	Pattern string `json:"pattern"`
}

// RedactionSchema mirrors the redaction_v1.json schema.
type RedactionSchema struct {
	Patterns      []RedactionPattern `json:"patterns"`
	AllowPatterns []AllowPattern     `json:"allow_patterns"`
}

// Redactor applies secret redaction to event payloads before they leave the
// process. It compiles the patterns from redaction_v1.json at init time.
type Redactor struct {
	patterns []compiledPattern
	allows   []*regexp.Regexp
}

type compiledPattern struct {
	re          *regexp.Regexp
	replacement string
	name        string
}

// DefaultRedactor returns a Redactor with the patterns from the fetched
// redaction_v1.json schema. These are compiled-in so the emitter doesn't need
// to read a file at runtime.
func DefaultRedactor() *Redactor {
	schema := RedactionSchema{
		Patterns: []RedactionPattern{
			{
				Name:        "private_key",
				Pattern:     `-----BEGIN [A-Z ]*PRIVATE KEY-----`,
				Replacement: "[REDACTED:private-key]",
			},
			{
				Name:        "api_key_long",
				Pattern:     `(?i)\b(sk|pk|rk|ghp|gho|xox[baprs]|AKIA)[A-Za-z0-9_\-]{16,}`,
				Replacement: "[REDACTED:api-key]",
			},
			{
				Name:        "credential_assignment",
				Pattern:     `(?i)\b(api[_-]?key|secret|token|password|passwd|credential)\s*[:=]\s*['"][^'"]{8,}['"]`,
				Replacement: "[REDACTED:credential]",
			},
			{
				Name:        "bearer_token",
				Pattern:     `(?i)\bbearer\s+[A-Za-z0-9_\-\.]{20,}`,
				Replacement: "[REDACTED:bearer-token]",
			},
		},
		AllowPatterns: []AllowPattern{
			{Pattern: `(?i)api[_-]?key.*schema`},
			{Pattern: `(?i)api[_-]?key.*config`},
		},
	}
	return NewRedactor(schema)
}

// NewRedactor compiles a Redactor from a schema.
func NewRedactor(schema RedactionSchema) *Redactor {
	r := &Redactor{}
	for _, p := range schema.Patterns {
		re, err := regexp.Compile(p.Pattern)
		if err != nil {
			continue // skip invalid patterns
		}
		r.patterns = append(r.patterns, compiledPattern{
			re:          re,
			replacement: p.Replacement,
			name:        p.Name,
		})
	}
	for _, a := range schema.AllowPatterns {
		re, err := regexp.Compile(a.Pattern)
		if err != nil {
			continue
		}
		r.allows = append(r.allows, re)
	}
	return r
}

// RedactString applies redaction patterns to a string. Allow patterns
// suppress redaction on matching text.
func (r *Redactor) RedactString(s string) string {
	if r == nil {
		return s
	}
	for _, p := range r.patterns {
		// Check allow patterns first.
		if r.matchesAnyAllow(s) {
			continue
		}
		s = p.re.ReplaceAllString(s, p.replacement)
	}
	return s
}

// RedactJSON applies redaction to a JSON payload. It unmarshals the JSON,
// redacts all string values recursively, then re-marshals.
func (r *Redactor) RedactJSON(raw json.RawMessage) json.RawMessage {
	if r == nil {
		return raw
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		return raw
	}
	v = r.redactValue(v)
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

func (r *Redactor) redactValue(v interface{}) interface{} {
	switch val := v.(type) {
	case string:
		return r.RedactString(val)
	case []interface{}:
		for i, item := range val {
			val[i] = r.redactValue(item)
		}
		return val
	case map[string]interface{}:
		for k, item := range val {
			val[k] = r.redactValue(item)
		}
		return val
	default:
		return v
	}
}

func (r *Redactor) matchesAnyAllow(s string) bool {
	for _, re := range r.allows {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}

// RedactIfNeeded is a convenience that redacts a string only if a redactor is
// non-nil. Used in tests.
func RedactIfNeeded(r *Redactor, s string) string {
	if r == nil {
		return s
	}
	return r.RedactString(s)
}

// ensure no unused import
var _ = strings.TrimSpace
