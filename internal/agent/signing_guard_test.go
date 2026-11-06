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
		{"no-gpg-sign with semicolon", "git commit --no-gpg-sign; true", true},

		// git -c with signing keys
		{"-c commit.gpgsign=false", "git -c commit.gpgsign=false commit -m test", true},
		{"-c commit.gpgsign=true", "git -c commit.gpgsign=true commit -m test", true},
		{"-c gpg.format=ssh", "git -c gpg.format=ssh commit -m test", true},
		{"-c gpg.format=openpgp", "git -c gpg.format=openpgp commit", true},
		{"-c user.signingkey=key", "git -c user.signingkey=key::ssh-ed25519 AAAA commit", true},
		{"-c tag.gpgsign=false", "git -c tag.gpgsign=false tag v1", true},
		{"-c gpg.ssh.allowedSignersFile", "git -c gpg.ssh.allowedSignersFile=/x commit", true},
		{"-c case-insensitive", "git -c Commit.GpgSign=false commit", true},
		{"-c GPG.FORMAT=ssh", "git -c GPG.FORMAT=ssh commit", true},

		// --config-env
		{"--config-env commit.gpgsign", "git --config-env=commit.gpgsign=MY_VAR commit", true},

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
		{"config case-insensitive", "git config COMMIT.GPGSIGN false", true},

		// git -C . config (flags between git and config with args)
		{"-C . config commit.gpgsign", "git -C . config commit.gpgsign false", true},
		{"-c core.pager=cat config commit.gpgsign", "git -c core.pager=cat config commit.gpgsign false", true},

		// multiple config subcommands (second one is a bypass)
		{"two configs second bypass", "git config user.name Alice; git config commit.gpgsign false", true},

		// benign git config (must pass)
		{"config user.name", "git config user.name Ada", false},
		{"config --get core.pager", "git config --get core.pager", false},
		{"config --get commit.gpgsign", "git config --get commit.gpgsign", false},
		{"config --get user.signingkey", "git config --get user.signingkey", false},
		{"config --list", "git config --list", false},

		// F7: read flags in any position (not just first after config) are allowed.
		{"config --show-origin --get user.signingkey", "git config --show-origin --get user.signingkey", false},
		{"config --show-origin --get-all commit.gpgsign", "git config --show-origin --get-all commit.gpgsign", false},
		{"config --show-scope --get-regexp gpg", "git config --show-scope --get-regexp gpg", false},
		{"config -l", "git config -l", false},
		{"config --get-urlmatch gpg.ssh", "git config --get-urlmatch gpg.ssh", false},

		// F7: bare key (one non-flag token, no value) is a get (read) — allowed.
		{"config --show-origin --show-scope user.signingkey", "git config --show-origin --show-scope user.signingkey", false},
		{"bare config user.signingkey", "git config user.signingkey", false},

		// F7: write forms on a signing key still refused.
		{"config --add commit.gpgsign true", "git config --add commit.gpgsign true", true},
		{"config --replace-all gpg.format ssh", "git config --replace-all gpg.format ssh", true},
		{"config --rename-section gpg", "git config --rename-section gpg", true},
		{"config --remove-section gpg", "git config --remove-section gpg", true},
		{"config -e", "git config -e", true},

		// F7: --no-gpg-sign on a git command still refused.
		{"-c commit.gpgsign=false log", "git -c commit.gpgsign=false log", true},

		// F7 review: output modifiers do NOT make a write into a read.
		{"config --null commit.gpgsign false", "git config --null commit.gpgsign false", true},
		{"config -z commit.gpgsign false", "git config -z commit.gpgsign false", true},
		{"config --name-only commit.gpgsign false", "git config --name-only commit.gpgsign false", true},
		{"config --show-origin commit.gpgsign false", "git config --show-origin commit.gpgsign false", true},

		// F7 review: --type/--default space form does not leak the value as key.
		{"config --type bool commit.gpgsign false", "git config --type bool commit.gpgsign false", true},
		{"config --default x commit.gpgsign false", "git config --default x commit.gpgsign false", true},

		// F7 review: key=value joined in a bare write.
		{"bare config commit.gpgsign=false", "git config commit.gpgsign=false", true},

		// F7 review: --git-dir space form does not leak the path as key.
		{"--git-dir . config commit.gpgsign false", "git --git-dir . config commit.gpgsign false", true},

		// F7 review: benign writes that must pass.
		{"config --unset core.pager", "git config --unset core.pager", false},
		{"config --remove-section core", "git config --remove-section core", false},

		// GIT_CONFIG_* env vars
		{"GIT_CONFIG_GLOBAL env", "GIT_CONFIG_GLOBAL=/dev/null git commit -m test", true},
		{"GIT_CONFIG_SYSTEM env", "GIT_CONFIG_SYSTEM=/x git log", true},
		{"GIT_CONFIG_COUNT env", "GIT_CONFIG_COUNT=1 git commit", true},
		{"GIT_CONFIG_PARAMETERS env", "GIT_CONFIG_PARAMETERS='commit.gpgsign=false' git commit", true},
		{"GIT_CONFIG_NOSYSTEM env", "GIT_CONFIG_NOSYSTEM=1 git log", true},
		{"GIT_CONFIG_KEY_0 env", "GIT_CONFIG_KEY_0=commit.gpgsign git commit", true},
		{"GIT_CONFIG_VALUE_0 env", "GIT_CONFIG_VALUE_0=false git commit", true},

		// benign commands (must pass)
		{"plain echo", "echo hello", false},
		{"git log", "git log --oneline -5", false},
		{"git status", "git status --porcelain", false},
		{"make test", "make test", false},
		{"empty command", "", false},
		{"echo --no-gpg-sign", "echo --no-gpg-sign", false},

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
			if tt.want && strings.HasPrefix(got, "ERROR:") {
				t.Errorf("checkSigningBypass(%q): should not start with ERROR: (callers add it): %s", tt.cmd, got)
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

// F9: Wakil's own GIT_CONFIG_* env (process environment, not command text)
// passes the F6 guard, while a user-typed GIT_CONFIG_COUNT=… prefix in command
// text is still refused. The guard inspects command text only; Wakil's env is
// never in command text, so both hold at once.
func TestSigningGuardF9Compat(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want bool // true = should refuse
	}{
		{"user-typed GIT_CONFIG_COUNT", "GIT_CONFIG_COUNT=1 git commit -m test", true},
		{"user-typed GIT_CONFIG_KEY_0", "GIT_CONFIG_KEY_0=commit.gpgsign git commit", true},
		{"user-typed GIT_CONFIG_VALUE_0", "GIT_CONFIG_VALUE_0=false git commit", true},
		{"user-typed GIT_CONFIG_GLOBAL", "GIT_CONFIG_GLOBAL=/dev/null git commit", true},
		{"benign git log", "git log --oneline", false},
		{"benign git config --get", "git config --get user.signingkey", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkSigningBypass(tt.cmd)
			if tt.want && got == "" {
				t.Errorf("checkSigningBypass(%q): expected refusal", tt.cmd)
			}
			if !tt.want && got != "" {
				t.Errorf("checkSigningBypass(%q): expected pass, got: %s", tt.cmd, got)
			}
		})
	}
}
