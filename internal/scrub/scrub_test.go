package scrub

import (
	"strings"
	"testing"
)

func TestScrubBearer(t *testing.T) {
	s := New(LevelStandard)
	got := s.Scrub("Authorization: Bearer abc123def456")
	if strings.Contains(got, "abc123def456") {
		t.Fatalf("bearer token not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED:bearer]") {
		t.Fatalf("expected [REDACTED:bearer] in: %s", got)
	}
}

func TestScrubOpenAIKey(t *testing.T) {
	s := New(LevelStandard)
	key := "sk-abcdefghijklmnopqrstuvwxyz0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	got := s.Scrub("api_key=" + key)
	if strings.Contains(got, key) {
		t.Fatalf("OpenAI key not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED:openai_key]") {
		t.Fatalf("expected [REDACTED:openai_key] in: %s", got)
	}
}

func TestScrubAnthropicKey(t *testing.T) {
	s := New(LevelStandard)
	key := "sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789"
	got := s.Scrub("key: " + key)
	if strings.Contains(got, key) {
		t.Fatalf("Anthropic key not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED:anthropic_key]") {
		t.Fatalf("expected [REDACTED:anthropic_key] in: %s", got)
	}
}

func TestScrubGitHubPAT(t *testing.T) {
	s := New(LevelStandard)
	key := "ghp_" + strings.Repeat("A", 36)
	got := s.Scrub("token: " + key)
	if strings.Contains(got, key) {
		t.Fatalf("GitHub PAT not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED:github_token]") {
		t.Fatalf("expected [REDACTED:github_token] in: %s", got)
	}
}

func TestScrubGoogleAPIKey(t *testing.T) {
	s := New(LevelStandard)
	key := "AIza" + strings.Repeat("A", 35)
	got := s.Scrub("key=" + key)
	if strings.Contains(got, key) {
		t.Fatalf("Google API key not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED:google_api_key]") {
		t.Fatalf("expected [REDACTED:google_api_key] in: %s", got)
	}
}

func TestScrubAWSAccessKey(t *testing.T) {
	s := New(LevelStandard)
	key := "AKIA" + strings.Repeat("I", 16)
	got := s.Scrub("aws_access_key_id=" + key)
	if strings.Contains(got, key) {
		t.Fatalf("AWS access key not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED:aws_access_key]") {
		t.Fatalf("expected [REDACTED:aws_access_key] in: %s", got)
	}
}

func TestScrubPrivateKey(t *testing.T) {
	s := New(LevelStandard)
	block := "-----BEGIN RSA PRIVATE KEY-----\nMIIE..."
	got := s.Scrub(block)
	if strings.Contains(got, "BEGIN RSA PRIVATE KEY") {
		t.Fatalf("private key not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED:private_key]") {
		t.Fatalf("expected [REDACTED:private_key] in: %s", got)
	}
}

func TestScrubJWT(t *testing.T) {
	s := New(LevelStandard)
	jwt := "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	got := s.Scrub("Authorization: " + jwt)
	if strings.Contains(got, jwt) {
		t.Fatalf("JWT not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED:jwt]") {
		t.Fatalf("expected [REDACTED:jwt] in: %s", got)
	}
}

func TestScrubConnString(t *testing.T) {
	s := New(LevelStandard)
	conn := "postgres://user:secretpass@host:5432/db"
	got := s.Scrub(conn)
	if strings.Contains(got, "secretpass") {
		t.Fatalf("connection string password not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED:password]") {
		t.Fatalf("expected [REDACTED:password] in: %s", got)
	}
	// The rest of the connection string should survive.
	if !strings.Contains(got, "postgres://") || !strings.Contains(got, "host:5432") {
		t.Fatalf("connection string over-redacted: %s", got)
	}
}

func TestScrubNoOp(t *testing.T) {
	s := NoOp()
	text := "Authorization: Bearer secret123"
	got := s.Scrub(text)
	if got != text {
		t.Fatalf("NoOp scrubber modified text: %s", got)
	}
}

func TestScrubOffLevel(t *testing.T) {
	s := New(LevelOff)
	text := "sk-abcdefghijklmnopqrstuvwxyz0123456789"
	got := s.Scrub(text)
	if got != text {
		t.Fatalf("LevelOff scrubber modified text: %s", got)
	}
}

func TestScrubAggressive(t *testing.T) {
	s := New(LevelAggressive)
	got := s.Scrub("password=ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij1234567890+/ABCD")
	if !strings.Contains(got, "[REDACTED:secret]") {
		t.Fatalf("aggressive mode should redact generic secret: %s", got)
	}
}

func TestScrubStandardDoesNotRedactGeneric(t *testing.T) {
	s := New(LevelStandard)
	// Standard mode should NOT match generic high-entropy strings without
	// a known prefix.
	text := "password=ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghij1234567890+/ABCD"
	got := s.Scrub(text)
	if strings.Contains(got, "[REDACTED:secret]") {
		t.Fatalf("standard mode should not redact generic secrets: %s", got)
	}
}

func TestScrubFalsePositiveCode(t *testing.T) {
	s := New(LevelStandard)
	// Common code patterns that should NOT be redacted.
	tests := []string{
		"func main() { fmt.Println(\"hello\") }",
		"import \"net/http\"",
		"SELECT * FROM users WHERE id = 1",
		"git commit -m \"fix: update README\"",
		"docker run -p 8080:80 nginx",
	}
	for _, text := range tests {
		got := s.Scrub(text)
		if got != text {
			t.Fatalf("false positive on benign code: %q -> %q", text, got)
		}
	}
}

func TestScrubMultipleInOneString(t *testing.T) {
	s := New(LevelStandard)
	got := s.Scrub("key1=sk-abcdefghijklmnopqrstuvwxyz0123456789 key2=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.signatureabc")
	if strings.Contains(got, "sk-abcdef") {
		t.Fatalf("first secret not redacted: %s", got)
	}
	if !strings.Contains(got, "[REDACTED:openai_key]") {
		t.Fatalf("expected openai_key redaction: %s", got)
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		input string
		want  Level
		err   bool
	}{
		{"off", LevelOff, false},
		{"standard", LevelStandard, false},
		{"aggressive", LevelAggressive, false},
		{"", 0, true},
		{"invalid", 0, true},
	}
	for _, tt := range tests {
		got, err := ParseLevel(tt.input)
		if tt.err {
			if err == nil {
				t.Fatalf("ParseLevel(%q) should error", tt.input)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseLevel(%q) error: %v", tt.input, err)
		}
		if got != tt.want {
			t.Fatalf("ParseLevel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

func TestScrubNilSafe(t *testing.T) {
	var ps *PatternScrubber
	got := ps.Scrub("hello")
	if got != "hello" {
		t.Fatalf("nil PatternScrubber should return text unchanged")
	}
}
