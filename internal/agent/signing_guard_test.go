package agent

import (
	"os"
	"strings"
	"testing"
)

func TestCheckSigningBypass(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want bool // true = should refuse
	}{
		// --no-gpg-sign flag
		{"no-gpg-sign flag", "git commit --no-gpg-sign -m test", true},
		{"no-gpg-sign flag standalone", "git --no-gpg-sign commit", true},

		// git -c with signing keys
		{"-c commit.gpgsign=false", "git -c commit.gpgsign=false commit -m test", true},
		{"-c commit.gpgsign=true", "git -c commit.gpgsign=true commit -m test", true},
		{"-c gpg.format=ssh", "git -c gpg.format=ssh commit -m test", true},
		{"-c gpg.format=openpgp", "git -c gpg.format=openpgp commit", true},
		{"-c user.signingkey=key", "git -c user.signingkey=key::ssh-ed25519 AAAA commit", true},
		{"-c tag.gpgsign=false", "git -c tag.gpgsign=false tag v1", true},
		{"-c gpg.ssh.allowedSignersFile", "git -c gpg.ssh.allowedSignersFile=/x commit", true},

		// benign -c (must pass)
		{"benign -c core.pager=cat", "git -c core.pager=cat log", false},
		{"benign -c log.showSignature=false", "git -c log.showSignature=false log", false},

		// git config subcommand with signing keys
		{"config commit.gpgsign false", "git config commit.gpgsign false", true},
		{"config --global commit.gpgsign false", "git config --global commit.gpgsign false", true},
		{"config --local user.signingkey key", "git config --local user.signingkey key::ssh-ed25519 AAAA", true},
		{"config --system gpg.format ssh", "git config --system gpg.format ssh", true},
		{"config --unset commit.gpgsign", "git config --unset commit.gpgsign", true},
		{"config --unset-all user.signingkey", "git config --unset-all user.signingkey", true},
		{"config gpg.ssh.allowedSignersFile", "git config gpg.ssh.allowedSignersFile /x", true},

		// benign git config (must pass)
		{"config user.name", "git config user.name Ada", false},
		{"config --get core.pager", "git config --get core.pager", false},

		// GIT_CONFIG_* env vars
		{"GIT_CONFIG_GLOBAL env", "GIT_CONFIG_GLOBAL=/dev/null git commit -m test", true},
		{"GIT_CONFIG_SYSTEM env", "GIT_CONFIG_SYSTEM=/x git log", true},
		{"GIT_CONFIG_COUNT env", "GIT_CONFIG_COUNT=1 git commit", true},
		{"GIT_CONFIG_PARAMETERS env", "GIT_CONFIG_PARAMETERS='commit.gpgsign=false' git commit", true},
		{"GIT_CONFIG_NOSYSTEM env", "GIT_CONFIG_NOSYSTEM=1 git log", true},
		{"GIT_CONFIG_KEY_0 env", "GIT_CONFIG_KEY_0=commit.gpgsign git commit", true},

		// benign commands (must pass)
		{"plain echo", "echo hello", false},
		{"git log", "git log --oneline -5", false},
		{"git status", "git status --porcelain", false},
		{"make test", "make test", false},
		{"empty command", "", false},

		// joined -c form (no space after -c)
		{"joined -ccommit.gpgsign=false", "git -ccommit.gpgsign=false commit", true},
		{"joined -ccore.pager=cat", "git -ccore.pager=cat log", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkSigningBypass(tt.cmd)
			if tt.want && got == "" {
				t.Errorf("checkSigningBypass(%q): expected refusal, got pass", tt.cmd)
			}
			if !tt.want && got != "" {
				t.Errorf("checkSigningBypass(%q): expected pass, got refusal: %s", tt.cmd, got)
			}
			if tt.want && !strings.Contains(got, "signing is enforced") {
				t.Errorf("checkSigningBypass(%q): refusal message missing 'signing is enforced': %s", tt.cmd, got)
			}
		})
	}
}

// TestSigningEnforcedNotEnforced: when the executor does not implement
// SigningEnforcer, signingBypassMsg returns "" (no refusal).
func TestSigningEnforcedNotEnforced(t *testing.T) {
	app := &App{}
	// No Exec set — signingEnforced returns false, so no refusal.
	if msg := app.signingBypassMsg("git -c commit.gpgsign=false commit"); msg != "" {
		t.Errorf("signingBypassMsg with no executor should be empty, got: %s", msg)
	}
}

// TestSigningPromptRulePresent: the agent prompt carries the signing-enforcement
// rule text so the model sees it at runtime (F6, system prompt layer).
func TestSigningPromptRulePresent(t *testing.T) {
	// Read prompts/agent.txt — the rule names the enforced keys and
	// --no-gpg-sign. We check the key phrases are all present.
	// This test is in the agent package so it can use the test helpers; the
	// prompt file is loaded at runtime and its content is static.
	// The rule was added in the Shell section of prompts/agent.txt.
	// We verify via a runtime read in the test binary that would fail
	// if the text was removed. Since the prompt is not loaded into the
	// test binary, we check the file content directly.
	//
	// This is a static-file check: it verifies the rule text is present in
	// the committed prompt file, not that it was loaded at runtime.
	data, err := os.ReadFile("../../prompts/agent.txt")
	if err != nil {
		t.Skipf("could not read prompts/agent.txt: %v", err)
	}
	content := string(data)
	for _, phrase := range []string{
		"Signing is enforced",
		"commit.gpgsign",
		"gpg.format",
		"user.signingkey",
		"tag.gpgsign",
		"no-gpg-sign",
		"GIT_CONFIG",
		"not worked around",
	} {
		if !strings.Contains(content, phrase) {
			t.Errorf("prompts/agent.txt missing signing rule phrase %q", phrase)
		}
	}
}