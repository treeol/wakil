package ilm

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRedactString_PEMKeys verifies full and truncated PEM blocks are redacted.
func TestRedactString_PEMKeys(t *testing.T) {
	r := DefaultRedactor()
	full := `-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEA01234567890abcdefghijklmnop
-----END RSA PRIVATE KEY-----`
	got := r.RedactString(full)
	if !strings.Contains(got, "[REDACTED:private-key]") {
		t.Errorf("full PEM not redacted: %q", got)
	}
	if strings.Contains(got, "MIIEpA") {
		t.Errorf("key body leaked: %q", got)
	}

	// Truncated (no END marker).
	truncated := `-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEA0123456789`
	got = r.RedactString(truncated)
	if !strings.Contains(got, "[REDACTED:private-key]") {
		t.Errorf("truncated PEM not redacted: %q", got)
	}
}

// TestRedactString_APIKeys verifies API key patterns are redacted.
func TestRedactString_APIKeys(t *testing.T) {
	r := DefaultRedactor()
	cases := []string{
		"sk-abcdefghijklmnopqrstuvwxyz123456",
		"pk-abcdefghijklmnopqrstuvwxyz123456",
		"ghp_abcdefghijklmnopqrstuvwxyz123456",
		"AKIAIOSFODNN7EXAMPLE",
	}
	for _, tc := range cases {
		got := r.RedactString(tc)
		if !strings.Contains(got, "[REDACTED:api-key]") {
			t.Errorf("API key %q not redacted: %q", tc, got)
		}
	}
}

// TestRedactString_BearerToken verifies bearer tokens are redacted.
func TestRedactString_BearerToken(t *testing.T) {
	r := DefaultRedactor()
	input := `Authorization: Bearer abcdefghijklmnopqrstuvwxyz1234567890`
	got := r.RedactString(input)
	if !strings.Contains(got, "[REDACTED:bearer-token]") {
		t.Errorf("bearer token not redacted: %q", got)
	}
}

// TestRedactString_CredentialAssignment verifies credential assignments are redacted.
func TestRedactString_CredentialAssignment(t *testing.T) {
	r := DefaultRedactor()
	input := `api_key = "sk-abcdefghijklmnopqrstuvwxyz"`
	got := r.RedactString(input)
	if !strings.Contains(got, "[REDACTED:credential]") {
		t.Errorf("credential assignment not redacted: %q", got)
	}
}

// TestRedactString_MultipleMatches verifies multiple secrets in one string are all redacted.
func TestRedactString_MultipleMatches(t *testing.T) {
	r := DefaultRedactor()
	input := `key1: sk-abcdefghijklmnopqrstuvwxyz123456, key2: ghp_abcdefghijklmnopqrstuvwxyz`
	got := r.RedactString(input)
	if !strings.Contains(got, "[REDACTED:api-key]") {
		t.Errorf("first key not redacted: %q", got)
	}
	// Both should be redacted (count occurrences).
	if strings.Count(got, "[REDACTED:api-key]") < 2 {
		t.Errorf("expected 2 api-key redactions, got: %q", got)
	}
}

// TestRedactJSON_NestedObject verifies redaction recurses into nested JSON objects.
func TestRedactJSON_NestedObject(t *testing.T) {
	r := DefaultRedactor()
	input := json.RawMessage(`{"config":{"api_key":"sk-abcdefghijklmnopqrstuvwxyz123456","name":"my-key"}}`)
	got := r.RedactJSON(input)
	// The api_key field should be redacted by credential-field detection.
	out := string(got)
	if !strings.Contains(out, "[REDACTED:credential-field]") {
		t.Errorf("credential field not redacted in nested JSON: %q", out)
	}
	if strings.Contains(out, "sk-abcdefghijklmnopqrstuvwxyz123456") {
		t.Errorf("API key value leaked in nested JSON: %q", out)
	}
	// Non-credential field should be preserved.
	if !strings.Contains(out, "my-key") {
		t.Errorf("non-credential field 'name' was modified: %q", out)
	}
}

// TestRedactJSON_CredentialFieldNames verifies credential field name detection
// with various casing, separators, and suffixes.
func TestRedactJSON_CredentialFieldNames(t *testing.T) {
	r := DefaultRedactor()
	cases := []struct {
		key      string
		redacted bool
	}{
		{"password", true},
		{"api_key", true},
		{"apiKey", true},
		{"api-key", true},
		{"authToken", true},
		{"access-token", true},
		{"my_secret", true},
		{"clientSecret", true},
		{"nonSecret", true}, // suffix-matched: "secret" is a credential suffix
		{"description", false},
	}
	for _, tc := range cases {
		t.Run(tc.key, func(t *testing.T) {
			input := json.RawMessage(`{"` + tc.key + `":"somevalue123456"}`)
			got := string(r.RedactJSON(input))
			if tc.redacted {
				if !strings.Contains(got, "[REDACTED:credential-field]") {
					t.Errorf("key %q should be redacted, got: %q", tc.key, got)
				}
			} else {
				if strings.Contains(got, "[REDACTED:credential-field]") {
					t.Errorf("key %q should NOT be redacted, got: %q", tc.key, got)
				}
			}
		})
	}
}

// TestRedactJSON_EmptyAndNullCredentials verifies empty strings and null values
// under credential field names are NOT redacted (they're not secrets).
func TestRedactJSON_EmptyAndNullCredentials(t *testing.T) {
	r := DefaultRedactor()
	input := json.RawMessage(`{"password":"","secret":null,"api_key":"sk-abcdefghijklmnopqrstuvwxyz123"}`)
	got := string(r.RedactJSON(input))
	// Empty string should remain empty.
	if !strings.Contains(got, `"password":""`) {
		t.Errorf("empty password should remain empty: %q", got)
	}
	// null should remain null.
	if !strings.Contains(got, `"secret":null`) {
		t.Errorf("null secret should remain null: %q", got)
	}
	// Non-empty api_key should be redacted.
	if !strings.Contains(got, "[REDACTED:credential-field]") {
		t.Errorf("non-empty api_key should be redacted: %q", got)
	}
}

// TestRedactJSON_ArrayOfObjects verifies arrays of objects with secrets are redacted.
func TestRedactJSON_ArrayOfObjects(t *testing.T) {
	r := DefaultRedactor()
	input := json.RawMessage(`[{"name":"item1","token":"sk-abcdefghijklmnopqrstuvwxyz123456"},{"name":"item2"}]`)
	got := string(r.RedactJSON(input))
	out := string(got)
	// The token field should be redacted.
	if !strings.Contains(out, "[REDACTED:credential-field]") {
		t.Errorf("token in array object not redacted: %q", out)
	}
	// Non-credential field should be preserved.
	if !strings.Contains(out, "item1") || !strings.Contains(out, "item2") {
		t.Errorf("non-credential fields lost: %q", out)
	}
}

// TestRedactJSON_MalformedFallsBackToString verifies that malformed JSON
// falls back to string redaction.
func TestRedactJSON_MalformedFallsBackToString(t *testing.T) {
	r := DefaultRedactor()
	// Malformed JSON containing a secret pattern.
	input := json.RawMessage(`{bad json with sk-abcdefghijklmnopqrstuvwxyz123456}`)
	got := string(r.RedactJSON(input))
	if !strings.Contains(got, "[REDACTED:api-key]") {
		t.Errorf("malformed JSON should fall back to string redaction: %q", got)
	}
}

// TestRedactJSON_NilRedactor verifies nil redactor is a no-op.
func TestRedactJSON_NilRedactor(t *testing.T) {
	var r *Redactor
	input := json.RawMessage(`{"api_key":"sk-abcdefghijklmnopqrstuvwxyz123456"}`)
	got := string(r.RedactJSON(input))
	if got != string(input) {
		t.Errorf("nil redactor should be no-op, got: %q", got)
	}
}

// TestRedactString_NilRedactor verifies nil redactor is a no-op for strings.
func TestRedactString_NilRedactor(t *testing.T) {
	var r *Redactor
	input := "sk-abcdefghijklmnopqrstuvwxyz123456"
	got := r.RedactString(input)
	if got != input {
		t.Errorf("nil redactor should be no-op, got: %q", got)
	}
}

// TestMatchAllows_PerMatch verifies that allow patterns suppress specific
// matches, not the entire string. A custom schema with an allow pattern
// that matches one specific API key prefix should preserve that key while
// redacting others.
func TestMatchAllows_PerMatch(t *testing.T) {
	schema := RedactionSchema{
		Patterns: []RedactionPattern{
			{
				Name:        "api_key_long",
				Pattern:     `(?i)\b(sk)[A-Za-z0-9_\-]{16,}`,
				Replacement: "[REDACTED:api-key]",
			},
		},
		AllowPatterns: []AllowPattern{
			{Pattern: `^sk_test_[A-Za-z0-9]+$`}, // allow sk_test_* keys
		},
	}
	r := NewRedactor(schema)

	// A test key should be preserved (allow pattern matches the substring).
	testKey := "sk_test_abcdefghijklmnopqrstuvwxyz"
	got := r.RedactString(testKey)
	if strings.Contains(got, "[REDACTED:api-key]") {
		t.Errorf("test key should be allowed (not redacted): %q", got)
	}

	// A live key should be redacted (allow pattern does not match).
	liveKey := "sk_live_abcdefghijklmnopqrstuvwxyz"
	got = r.RedactString(liveKey)
	if !strings.Contains(got, "[REDACTED:api-key]") {
		t.Errorf("live key should be redacted: %q", got)
	}

	// Both in one string: test key preserved, live key redacted.
	combined := "test: sk_test_abcdefghijklmnopqrstuvwxyz, live: sk_live_abcdefghijklmnopqrstuvwxyz"
	got = r.RedactString(combined)
	if strings.Contains(got, "[REDACTED:api-key]") && !strings.Contains(got, "sk_test_") {
		// Good — test key preserved, live key redacted.
	} else if !strings.Contains(got, "[REDACTED:api-key]") {
		t.Errorf("live key should be redacted in combined string: %q", got)
	}
	// The test key should survive.
	if !strings.Contains(got, "sk_test_") {
		t.Errorf("test key should be preserved in combined string: %q", got)
	}
}

// TestNewRedactor_InvalidPattern verifies invalid regex patterns are skipped.
func TestNewRedactor_InvalidPattern(t *testing.T) {
	schema := RedactionSchema{
		Patterns: []RedactionPattern{
			{Name: "valid", Pattern: `sk-[a-z]+`, Replacement: "[REDACTED]"},
			{Name: "invalid", Pattern: `[`, Replacement: "[REDACTED]"}, // invalid regex
		},
	}
	r := NewRedactor(schema)
	// Should have only 1 pattern (invalid skipped).
	got := r.RedactString("sk-testkey")
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("valid pattern should still work: %q", got)
	}
}

// TestRedactJSON_BenignValuesNotRedacted verifies that non-credential values
// are not redacted.
func TestRedactJSON_BenignValuesNotRedacted(t *testing.T) {
	r := DefaultRedactor()
	input := json.RawMessage(`{"name":"my-session","title":"test session","count":42}`)
	got := string(r.RedactJSON(input))
	if !strings.Contains(got, "my-session") {
		t.Errorf("benign string 'name' value was modified: %q", got)
	}
	if !strings.Contains(got, "test session") {
		t.Errorf("benign string 'title' value was modified: %q", got)
	}
	if !strings.Contains(got, "42") {
		t.Errorf("benign number was modified: %q", got)
	}
}
