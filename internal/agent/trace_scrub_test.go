package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// TestMakeTraceEntry_ScrubsAPIKeyInOutput verifies that a recognized secret
// pattern (OpenAI API key) in tool output is redacted before being distilled
// into FirstLine/LastLine/ErrorTail.
func TestMakeTraceEntry_ScrubsAPIKeyInOutput(t *testing.T) {
	secret := "sk-abcdefghijklmnopqrstuvwxyz0123456789"
	output := "configured with key: " + secret + "\nsecond line"
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"config.go"}`}}
	result := okResult(output)

	e := MakeTraceEntry(tc, result)

	if strings.Contains(e.FirstLine, secret) {
		t.Errorf("FirstLine contains raw API key: %q", e.FirstLine)
	}
	if !strings.Contains(e.FirstLine, "[REDACTED:") {
		t.Errorf("FirstLine should contain redaction marker, got: %q", e.FirstLine)
	}
	if strings.Contains(e.LastLine, secret) {
		t.Errorf("LastLine contains raw API key: %q", e.LastLine)
	}
}

// TestMakeTraceEntry_ScrubsBearerTokenInCommand verifies that a bearer token
// in a shell command is redacted from the Command field.
func TestMakeTraceEntry_ScrubsBearerTokenInCommand(t *testing.T) {
	token := "Bearer dGhpcyBpcyBhIHNlY3JldCB0b2tlbg=="
	// Use a command without embedded quotes to avoid JSON escaping complexity.
	cmd := "curl -H Authorization:" + token + " https://api.example.com"
	args, _ := json.Marshal(struct{ Command string }{Command: cmd})
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "run_shell", Arguments: string(args)}}
	result := okResult("done")

	e := MakeTraceEntry(tc, result)

	if strings.Contains(e.Command, token) {
		t.Errorf("Command contains raw bearer token: %q", e.Command)
	}
	if !strings.Contains(e.Command, "[REDACTED:") {
		t.Errorf("Command should contain redaction marker, got: %q", e.Command)
	}
}

// TestMakeTraceEntry_ScrubsPEMBlockInErrorTail verifies that a complete PEM
// private key block in failed tool output is redacted from ErrorTail.
func TestMakeTraceEntry_ScrubsPEMBlockInErrorTail(t *testing.T) {
	pemBlock := `-----BEGIN RSA PRIVATE KEY-----
MIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn/ygWyF5TkL83
k0XnW0yq3zM5AAAAB3NzaC1yc2EAAAADAQABAAABAQC5
-----END RSA PRIVATE KEY-----`
	output := "Loading key file...\n" + pemBlock + "\nfailed to connect"
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "run_shell", Arguments: `{"command":"openssl rsa -in key.pem"}`}}
	result := errResult("ERROR: exit status 1\n" + output)

	e := MakeTraceEntry(tc, result)

	if e.ErrorTail == "" {
		t.Fatal("ErrorTail should be populated for failed tool call")
	}
	if strings.Contains(e.ErrorTail, "MIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn/ygWyF5TkL83") {
		t.Errorf("ErrorTail contains raw PEM key body: %q", e.ErrorTail)
	}
	if !strings.Contains(e.ErrorTail, "[REDACTED:private_key]") {
		t.Errorf("ErrorTail should contain PEM redaction marker, got: %q", e.ErrorTail)
	}
}

// TestMakeTraceEntry_ScrubsAnthropicKeyInFirstLine verifies that an Anthropic
// API key in the first output line is redacted.
func TestMakeTraceEntry_ScrubsAnthropicKeyInFirstLine(t *testing.T) {
	secret := "sk-ant-api03-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	output := "Using key: " + secret
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"env.go"}`}}
	result := okResult(output)

	e := MakeTraceEntry(tc, result)

	if strings.Contains(e.FirstLine, secret) {
		t.Errorf("FirstLine contains raw Anthropic key: %q", e.FirstLine)
	}
	if !strings.Contains(e.FirstLine, "[REDACTED:anthropic_key]") {
		t.Errorf("FirstLine should contain anthropic_key redaction, got: %q", e.FirstLine)
	}
}

// TestMakeTraceEntry_ScrubsJWTInOutput verifies that a JWT in tool output is redacted.
func TestMakeTraceEntry_ScrubsJWTInOutput(t *testing.T) {
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	output := "Token: " + jwt
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "run_shell", Arguments: `{"command":"echo token"}`}}
	result := okResult(output)

	e := MakeTraceEntry(tc, result)

	if strings.Contains(e.FirstLine, jwt) {
		t.Errorf("FirstLine contains raw JWT: %q", e.FirstLine)
	}
	if !strings.Contains(e.FirstLine, "[REDACTED:jwt]") {
		t.Errorf("FirstLine should contain JWT redaction, got: %q", e.FirstLine)
	}
}

// TestMakeTraceEntry_OutputLenIsRaw verifies that OutputLen reflects the raw
// (pre-scrub) output byte count, not the scrubbed length.
func TestMakeTraceEntry_OutputLenIsRaw(t *testing.T) {
	secret := "sk-abcdefghijklmnopqrstuvwxyz0123456789"
	output := "key: " + secret
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"f"}`}}
	result := okResult(output)

	e := MakeTraceEntry(tc, result)

	if e.OutputLen != len(output) {
		t.Errorf("OutputLen = %d, want %d (raw output length)", e.OutputLen, len(output))
	}
}

// TestMakeTraceEntry_PreservesDiagnostics verifies that non-secret diagnostic
// information (paths, error messages) is preserved in trace entries.
func TestMakeTraceEntry_PreservesDiagnostics(t *testing.T) {
	output := "ERROR: permission denied: /home/user/project/config.yaml"
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"config.yaml"}`}}
	result := errResult(output)

	e := MakeTraceEntry(tc, result)

	if !strings.Contains(e.FirstLine, "permission denied") {
		t.Errorf("FirstLine should preserve diagnostic text, got: %q", e.FirstLine)
	}
	if !strings.Contains(e.FirstLine, "config.yaml") {
		t.Errorf("FirstLine should preserve file path, got: %q", e.FirstLine)
	}
}

// TestMakeTraceEntry_ScrubsGitHubPATInCommand verifies that a GitHub PAT in a
// shell command is redacted.
func TestMakeTraceEntry_ScrubsGitHubPATInCommand(t *testing.T) {
	token := "ghp_1234567890abcdefghijklmnopqrstuvwxyzABCD"
	cmd := "export GITHUB_TOKEN=" + token
	args, _ := json.Marshal(struct{ Command string }{Command: cmd})
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "run_shell", Arguments: string(args)}}
	result := okResult("set")

	e := MakeTraceEntry(tc, result)

	if strings.Contains(e.Command, token) {
		t.Errorf("Command contains raw GitHub PAT: %q", e.Command)
	}
	if !strings.Contains(e.Command, "[REDACTED:") {
		t.Errorf("Command should contain redaction marker, got: %q", e.Command)
	}
}

// TestMakeTraceEntry_ScrubsConnectionStringInOutput verifies that a database
// connection string with a password is redacted from tool output.
func TestMakeTraceEntry_ScrubsConnectionStringInOutput(t *testing.T) {
	connStr := "postgres://user:secretpass123@db.example.com:5432/mydb"
	output := "Connecting to: " + connStr
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "run_shell", Arguments: `{"command":"psql"}`}}
	result := okResult(output)

	e := MakeTraceEntry(tc, result)

	if strings.Contains(e.FirstLine, "secretpass123") {
		t.Errorf("FirstLine contains raw password from connection string: %q", e.FirstLine)
	}
	if !strings.Contains(e.FirstLine, "[REDACTED:password]") {
		t.Errorf("FirstLine should contain password redaction, got: %q", e.FirstLine)
	}
}

// TestFormatTraceEntry_NoSecretsAfterFormatting verifies that FormatTraceEntry
// output contains no raw secrets when the trace entry was scrubbed at construction.
func TestFormatTraceEntry_NoSecretsAfterFormatting(t *testing.T) {
	secret := "sk-abcdefghijklmnopqrstuvwxyz0123456789"
	output := "key: " + secret
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"f"}`}}
	result := okResult(output)

	e := MakeTraceEntry(tc, result)
	formatted := FormatTraceEntry(e)

	if strings.Contains(formatted, secret) {
		t.Errorf("FormatTraceEntry output contains raw secret: %q", formatted)
	}
}

// TestMakeTraceEntry_ScrubsAPIKeyInLastLine verifies that a secret in the
// LAST output line (not just the first) is redacted. Dedicated LastLine coverage.
func TestMakeTraceEntry_ScrubsAPIKeyInLastLine(t *testing.T) {
	secret := "sk-abcdefghijklmnopqrstuvwxyz0123456789"
	output := "first line\nconfigured with key: " + secret
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"config.go"}`}}
	result := okResult(output)

	e := MakeTraceEntry(tc, result)

	if strings.Contains(e.LastLine, secret) {
		t.Errorf("LastLine contains raw API key: %q", e.LastLine)
	}
	if !strings.Contains(e.LastLine, "[REDACTED:") {
		t.Errorf("LastLine should contain redaction marker, got: %q", e.LastLine)
	}
}

// TestMakeTraceEntry_ScrubsLongPEMInErrorTail verifies that a PEM block
// spanning many lines (longer than the 15-line ErrorTail cap) is fully
// redacted, not partially retained due to line selection.
func TestMakeTraceEntry_ScrubsLongPEMInErrorTail(t *testing.T) {
	// Build a PEM block with 20 body lines — exceeds the 15-line ErrorTail cap.
	var pemLines []string
	pemLines = append(pemLines, "-----BEGIN RSA PRIVATE KEY-----")
	for i := 0; i < 20; i++ {
		pemLines = append(pemLines, "MIIEpAIBAAKCAQEA0Z3VS5JJcds3xfn"+string(rune('A'+i))+"ygWyF5TkL83")
	}
	pemLines = append(pemLines, "-----END RSA PRIVATE KEY-----")
	pemBlock := strings.Join(pemLines, "\n")
	output := "Loading key file...\n" + pemBlock + "\nfailed to connect"
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "run_shell", Arguments: `{"command":"openssl rsa -in key.pem"}`}}
	result := errResult("ERROR: exit status 1\n" + output)

	e := MakeTraceEntry(tc, result)

	if e.ErrorTail == "" {
		t.Fatal("ErrorTail should be populated for failed tool call")
	}
	// The entire PEM block should be replaced — no body lines should survive.
	for _, bodyLine := range pemLines[1 : len(pemLines)-1] {
		if strings.Contains(e.ErrorTail, bodyLine) {
			t.Errorf("ErrorTail contains raw PEM body line: %q", bodyLine)
		}
	}
	if !strings.Contains(e.ErrorTail, "[REDACTED:private_key]") {
		t.Errorf("ErrorTail should contain PEM redaction marker, got: %q", e.ErrorTail)
	}
}

// TestMakeTraceEntry_ScrubsSecretInRunShellLastLine verifies that for run_shell
// (which has a 4-line LastLine tail), secrets in any of those lines are redacted.
func TestMakeTraceEntry_ScrubsSecretInRunShellLastLine(t *testing.T) {
	secret := "sk-abcdefghijklmnopqrstuvwxyz0123456789"
	output := "line 1\nline 2\nline 3\nkey: " + secret
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "run_shell", Arguments: `{"command":"test"}`}}
	result := okResult(output)

	e := MakeTraceEntry(tc, result)

	if strings.Contains(e.LastLine, secret) {
		t.Errorf("LastLine (run_shell multi-line) contains raw API key: %q", e.LastLine)
	}
	if !strings.Contains(e.LastLine, "[REDACTED:") {
		t.Errorf("LastLine should contain redaction marker, got: %q", e.LastLine)
	}
}

// TestMakeTraceEntry_ScrubsMultipleSecretsInErrorTail verifies that multiple
// different secret patterns in failed tool output are all redacted.
func TestMakeTraceEntry_ScrubsMultipleSecretsInErrorTail(t *testing.T) {
	openaiKey := "sk-abcdefghijklmnopqrstuvwxyz0123456789"
	githubToken := "ghp_1234567890abcdefghijklmnopqrstuvwxyzABCD"
	output := "config check\nopenai: " + openaiKey + "\ngithub: " + githubToken + "\nfailed"
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "run_shell", Arguments: `{"command":"check"}`}}
	result := errResult("ERROR: exit status 1\n" + output)

	e := MakeTraceEntry(tc, result)

	if strings.Contains(e.ErrorTail, openaiKey) {
		t.Errorf("ErrorTail contains raw OpenAI key: %q", e.ErrorTail)
	}
	if strings.Contains(e.ErrorTail, githubToken) {
		t.Errorf("ErrorTail contains raw GitHub token: %q", e.ErrorTail)
	}
	if !strings.Contains(e.ErrorTail, "[REDACTED:openai_key]") {
		t.Errorf("ErrorTail should contain openai_key redaction, got: %q", e.ErrorTail)
	}
	if !strings.Contains(e.ErrorTail, "[REDACTED:github_token]") {
		t.Errorf("ErrorTail should contain github_token redaction, got: %q", e.ErrorTail)
	}
}
