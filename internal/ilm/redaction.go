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
				Name:        "private_key_full",
				Pattern:     `-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`,
				Replacement: "[REDACTED:private-key]",
			},
			{
				// Fallback for truncated PEM blocks: if the END marker is
				// missing (log truncation, line-length limits), redact from
				// BEGIN to end-of-string. Without this, a truncated key body
				// would leak entirely.
				Name:        "private_key_truncated",
				Pattern:     `-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*$`,
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
		// Allow patterns removed: in per-match mode, the shipped allow patterns
		// (api_key.*schema, api_key.*config) are both ineffective and a bypass
		// vector — a secret containing "config" would escape redaction. The
		// allow mechanism is retained in the Redactor struct for future use
		// with properly anchored patterns, but no allow patterns ship by
		// default.
		AllowPatterns: nil,
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
// suppress redaction on a per-match basis: an allow pattern only suppresses
// a specific redaction pattern hit when the allow pattern matches the same
// text region — NOT the entire string. This prevents a broad allow pattern
// (e.g. "api_key.*config") from suppressing unrelated redactions (e.g.
// "bearer_token") elsewhere in the same string.
func (r *Redactor) RedactString(s string) string {
	if r == nil {
		return s
	}
	for _, p := range r.patterns {
		s = r.redactOnePattern(s, p)
	}
	return s
}

// redactOnePattern applies a single redaction pattern to s, checking allow
// patterns against each individual match (not the whole string). Matches are
// processed right-to-left so byte offsets aren't invalidated by earlier
// replacements.
func (r *Redactor) redactOnePattern(s string, p compiledPattern) string {
	matches := p.re.FindAllStringIndex(s, -1)
	if len(matches) == 0 {
		return s
	}
	for i := len(matches) - 1; i >= 0; i-- {
		start, end := matches[i][0], matches[i][1]
		matched := s[start:end]
		if r.matchAllows(matched) {
			continue // allow pattern matches this specific hit — skip
		}
		s = s[:start] + p.replacement + s[end:]
	}
	return s
}

// RedactJSON applies redaction to a JSON payload. It unmarshals the JSON,
// redacts all string values recursively, then re-marshals. If the JSON is
// malformed, it falls back to RedactString on the raw text so secrets in
// invalid JSON are still redacted.
func (r *Redactor) RedactJSON(raw json.RawMessage) json.RawMessage {
	if r == nil {
		return raw
	}
	var v interface{}
	if err := json.Unmarshal(raw, &v); err != nil {
		// Malformed JSON — fall back to string redaction so secrets are
		// still caught.
		return json.RawMessage(r.RedactString(string(raw)))
	}
	v = r.redactValue(v)
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	return out
}

// credentialFieldNames maps JSON field names that commonly hold secrets.
// Keys are normalized: lowercased with _ and - removed before lookup.
// When a JSON object has a key matching one of these, the value is redacted
// even if the value itself doesn't match a regex pattern. This catches
// {"password":"short"} where the value is not long enough or formatted to
// match the credential_assignment regex.
var credentialFieldNames = map[string]bool{
	"password":      true,
	"passwd":        true,
	"pwd":           true,
	"passphrase":    true,
	"secret":        true,
	"secretkey":     true,
	"token":         true,
	"authtoken":     true,
	"accesstoken":   true,
	"refreshtoken":  true,
	"credential":    true,
	"apikey":        true,
	"accesskey":     true,
	"privatekey":    true,
	"clientsecret":  true,
	"authorization": true,
	"awssecretaccesskey": true,
}

// isCredentialField reports whether a JSON key name indicates a credential
// field. The key is normalized (lowercased, _ and - removed) before lookup,
// and also matched by suffix (*token, *secret, *password, *key).
func isCredentialField(key string) bool {
	normalized := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
	if credentialFieldNames[normalized] {
		return true
	}
	// Suffix matching for common credential field name patterns.
	for _, suffix := range credentialFieldSuffixes {
		if strings.HasSuffix(normalized, suffix) {
			return true
		}
	}
	return false
}

// credentialFieldSuffixes matches credential field names by suffix.
// Normalized keys (lowercased, _ and - removed) ending in these suffixes
// are treated as credential fields.
var credentialFieldSuffixes = []string{
	"token",
	"tokens",
	"secret",
	"secrets",
	"password",
	"passwd",
	"apikey",
	"apikeys",
	"privatekey",
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
			if isCredentialField(k) && item != nil {
				// Redact the value of a known credential field, regardless
				// of the value type (string, number, object, array).
				// Empty strings are left as-is (not a secret).
				if s, ok := item.(string); ok && s == "" {
					val[k] = s
					continue
				}
				val[k] = "[REDACTED:credential-field]"
				continue
			}
			val[k] = r.redactValue(item)
		}
		return val
	default:
		return v
	}
}

// matchAllows checks whether any allow pattern matches the given text.
// Used per-match (not per-string) so a broad allow pattern only suppresses
// the specific redaction hit it overlaps with.
func (r *Redactor) matchAllows(s string) bool {
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
