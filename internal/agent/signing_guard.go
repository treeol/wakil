package agent

// signing_guard.go implements the F6 signing-enforcement guard: in any session
// whose profile carries signing (session gitconfig emitted), the shell tool and
// every git tool refuse a command that sets or unsets a signing-related git
// config key — via `git -c`, `git config` (any scope), `--no-gpg-sign`, or the
// GIT_CONFIG_* environment variables in the command line.
//
// The refusal is a tool error that says signing is enforced for this session and
// that a signing failure is to be reported to the user, not worked around.
//
// A determined bypass through a script file is out of scope — the guard catches
// the direct forms; the audit trail (an unsigned commit) catches the rest. The
// guard is defense-in-depth, not a security boundary.

import (
	"strings"
)

// signingBypassKeys are the git config keys that control signing. Setting or
// unsetting any of them in a signing-enforced session is a bypass attempt.
var signingBypassKeys = []string{
	"commit.gpgsign",
	"gpg.format",
	"user.signingkey",
	"tag.gpgsign",
}

// signingBypassKeyPrefixes are key prefixes whose matches (gpg.ssh.*) are also
// signing-related and must not be set or unset in a signing-enforced session.
var signingBypassKeyPrefixes = []string{
	"gpg.ssh.",
}

// signingBypassEnvVars are the environment variables that can override or
// inject git config at runtime; their presence in a command line (as NAME=VALUE
// or `NAME VALUE`) is a bypass vector when the value targets a signing key.
var signingBypassEnvVars = []string{
	"GIT_CONFIG_PARAMETERS",
	"GIT_CONFIG_GLOBAL",
	"GIT_CONFIG_SYSTEM",
	"GIT_CONFIG_NOSYSTEM",
	"GIT_CONFIG_COUNT",
}

// signingBypassEnvKeyPrefixes matches the per-index env var form
// GIT_CONFIG_KEY_<n> used by git to inject config via the environment.
const signingBypassEnvKeyPrefix = "GIT_CONFIG_KEY_"

// signingGuardError is the refusal message returned to the agent when a command
// attempts to bypass signing.
const signingGuardError = "ERROR: signing is enforced for this session — a signing failure must be reported to the user, not worked around. Do not set or unset commit.gpgsign, gpg.format, user.signingkey, gpg.ssh.*, tag.gpgsign, --no-gpg-sign, or GIT_CONFIG_* in a signing-enforced session."

// checkSigningBypass inspects a command line for forms that would disable or
// bypass signing. Returns a non-empty error message string when a bypass is
// detected, "" when the command is safe.
//
// It checks:
//   - `git -c <key>=<val>` where <key> is a signing key (benign -c uses like
//     `core.pager=cat` stay allowed)
//   - `git config <key> <val>` (any scope, including --global/--system/--local)
//   - `--no-gpg-sign` flag
//   - GIT_CONFIG_* environment variables in the command line (NAME=VALUE form)
//   - GIT_CONFIG_KEY_<n> env vars
//
// A determined bypass through a script file is out of scope: the guard catches
// the direct forms; the audit trail (an unsigned commit) catches the rest.
func checkSigningBypass(cmd string) string {
	if cmd == "" {
		return ""
	}
	// --no-gpg-sign flag.
	if containsToken(cmd, "--no-gpg-sign") {
		return signingGuardError
	}
	// `git -c <key>=<val>` forms: scan for -c followed by a signing key.
	if msg := checkGitDashC(cmd); msg != "" {
		return msg
	}
	// `git config` subcommand: the next non-flag tokens are the key (and value).
	if msg := checkGitConfigSub(cmd); msg != "" {
		return msg
	}
	// GIT_CONFIG_* env vars in the command line.
	if msg := checkGitConfigEnv(cmd); msg != "" {
		return msg
	}
	return ""
}

// containsToken reports whether s contains the token as a whitespace-delimited
// word (not a substring of a longer word).
func containsToken(s, token string) bool {
	for _, f := range strings.Fields(s) {
		if f == token {
			return true
		}
	}
	return false
}

// isSigningKey reports whether key is a signing-related config key.
func isSigningKey(key string) bool {
	for _, k := range signingBypassKeys {
		if key == k {
			return true
		}
	}
	for _, p := range signingBypassKeyPrefixes {
		if strings.HasPrefix(key, p) {
			return true
		}
	}
	return false
}

// checkGitDashC scans for `-c <key>=<val>` or `-c<key>=<val>` forms where key is
// a signing key. It handles both the separated (`-c key=val`) and joined
// (`-ckey=val`) forms, and quotes.
func checkGitDashC(cmd string) string {
	fields := strings.Fields(cmd)
	for i, f := range fields {
		var val string
		switch {
		case f == "-c" && i+1 < len(fields):
			val = fields[i+1]
		case strings.HasPrefix(f, "-c") && len(f) > 2:
			val = f[2:]
		case f == "-c":
			// -c at the end with no argument — nothing to check.
			continue
		default:
			continue
		}
		// Strip quotes from the value.
		val = strings.Trim(val, `"'`)
		// The value is `key=value`; extract the key (before the first `=`).
		key := val
		if idx := strings.IndexByte(val, '='); idx >= 0 {
			key = val[:idx]
		}
		if isSigningKey(key) {
			return signingGuardError
		}
	}
	return ""
}

// checkGitConfigSub detects `git config <scope> <key> <value>` where key is a
// signing key. It handles --global, --system, --local, --file, --blob and the
// bare `git config <key> <value>` form.
func checkGitConfigSub(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) < 2 {
		return ""
	}
	// Find `git` ... `config`.
	var configIdx int = -1
	for i, f := range fields {
		if f == "config" && i > 0 && (fields[i-1] == "git" || strings.HasSuffix(fields[i-1], "/git")) {
			configIdx = i
			break
		}
		// Also match `git -c ... config` (with flags between git and config).
		if f == "config" && i > 0 {
			// Walk back past flags to find `git`.
			j := i - 1
			for j >= 0 && strings.HasPrefix(fields[j], "-") {
				j--
			}
			if j >= 0 && (fields[j] == "git" || strings.HasSuffix(fields[j], "/git")) {
				configIdx = i
				break
			}
		}
	}
	if configIdx < 0 {
		return ""
	}
	// After `config`, skip flags (--global, --system, --local, --file, --blob,
	// --get, --set, --unset, --unset-all, --replace-all, --get-all, --get-regexp,
	// --add, etc.) and the --file/--blob argument, then the key.
	i := configIdx + 1
	for i < len(fields) {
		f := fields[i]
		if !strings.HasPrefix(f, "-") {
			break
		}
		// --file and --blob take an argument.
		if f == "--file" || f == "--blob" {
			i += 2 // skip the flag and its argument
			continue
		}
		// -f <file> (short form) — but only skip if there's a next field.
		if f == "-f" {
			i += 2
			continue
		}
		i++
	}
	if i >= len(fields) {
		return ""
	}
	key := strings.Trim(fields[i], `"'`)
	if isSigningKey(key) {
		return signingGuardError
	}
	return ""
}

// checkGitConfigEnv detects GIT_CONFIG_* environment variables in the command
// line (the `NAME=VALUE` form used by sh -c).
func checkGitConfigEnv(cmd string) string {
	for _, f := range strings.Fields(cmd) {
		// NAME=VALUE form.
		if idx := strings.IndexByte(f, '='); idx > 0 {
			name := f[:idx]
			// GIT_CONFIG_COUNT, GIT_CONFIG_GLOBAL, GIT_CONFIG_SYSTEM,
			// GIT_CONFIG_NOSYSTEM, GIT_CONFIG_PARAMETERS.
			for _, v := range signingBypassEnvVars {
				if name == v {
					return signingGuardError
				}
			}
			// GIT_CONFIG_KEY_<n> form.
			if strings.HasPrefix(name, signingBypassEnvKeyPrefix) {
				return signingGuardError
			}
		}
	}
	return ""
}

// signingEnforced reports whether the executor's session carries signing. It
// checks the SigningEnforcer optional interface.
func (a *App) signingEnforced() bool {
	if a.Exec == nil {
		return false
	}
	if se, ok := a.Exec.(execSigningEnforcer); ok {
		return se.SigningEnforced()
	}
	return false
}

// execSigningEnforcer is a local alias to avoid importing exec in the
// interface assertion (the interface is defined there). It matches
// exec.SigningEnforcer.
type execSigningEnforcer interface {
	SigningEnforced() bool
}

// signingBypassMsg returns the refusal message when signing is enforced and the
// command would bypass it, or "" when the command is safe or signing is not
// enforced.
func (a *App) signingBypassMsg(cmd string) string {
	if !a.signingEnforced() {
		return ""
	}
	return checkSigningBypass(cmd)
}