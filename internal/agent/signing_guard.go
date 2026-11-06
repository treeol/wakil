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
// Git config keys are case-insensitive (section and variable names), so
// comparisons use strings.ToLower.
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
// inject git config at runtime. Their presence in a command line as NAME=VALUE
// is a bypass vector.
var signingBypassEnvVars = []string{
	"GIT_CONFIG_PARAMETERS",
	"GIT_CONFIG_GLOBAL",
	"GIT_CONFIG_SYSTEM",
	"GIT_CONFIG_NOSYSTEM",
	"GIT_CONFIG_COUNT",
}

// signingBypassEnvKeyPrefixes match the per-index env var forms
// GIT_CONFIG_KEY_<n> and GIT_CONFIG_VALUE_<n> used by git to inject config
// via the environment.
var signingBypassEnvKeyPrefixes = []string{
	"GIT_CONFIG_KEY_",
	"GIT_CONFIG_VALUE_",
}

// signingGuardError is the refusal message returned to the agent when a command
// attempts to bypass signing. No "ERROR: " prefix — callers add it.
const signingGuardError = "signing is enforced for this session — a signing failure must be reported to the user, not worked around. Do not set or unset commit.gpgsign, gpg.format, user.signingkey, gpg.ssh.*, tag.gpgsign, --no-gpg-sign, or GIT_CONFIG_* in a signing-enforced session."

// checkSigningBypass inspects a command line for forms that would disable or
// bypass signing. Returns a non-empty error message string when a bypass is
// detected, "" when the command is safe.
//
// It checks:
//   - `--no-gpg-sign` flag (as a whitespace-delimited token, ignoring trailing
//     punctuation like `;` or `&&`)
//   - `git -c <key>=<val>` where <key> is a signing key (benign -c uses like
//     `core.pager=cat` stay allowed)
//   - `git --config-env=<key>=<envvar>` where <key> is a signing key
//   - `git config <key> <val>` (any scope, including --global/--system/--local)
//     — but NOT read-only subcommands (--get, --get-all, --get-regexp, --list,
//     --show-origin, --show-scope)
//   - GIT_CONFIG_* environment variables in the command line (NAME=VALUE form)
//   - GIT_CONFIG_KEY_<n> and GIT_CONFIG_VALUE_<n> env var prefixes
//
// Git config keys are case-insensitive: `Commit.GpgSign` matches
// `commit.gpgsign`.
//
// A determined bypass through a script file is out of scope: the guard catches
// the direct forms; the audit trail (an unsigned commit) catches the rest.
func checkSigningBypass(cmd string) string {
	if cmd == "" {
		return ""
	}
	// --no-gpg-sign flag (only on git commands — `echo --no-gpg-sign` is
	// benign).
	if isGitCommand(cmd) && containsToken(cmd, "--no-gpg-sign") {
		return signingGuardError
	}
	// `git -c <key>=<val>` and `git --config-env=<key>=<val>` forms.
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

// isGitCommand reports whether the command starts with `git` or `<path>/git`.
func isGitCommand(cmd string) bool {
	fields := strings.Fields(cmd)
	for _, f := range fields {
		if f == "git" || strings.HasSuffix(f, "/git") {
			return true
		}
		// Skip env vars (NAME=VALUE) that precede git.
		if strings.Contains(f, "=") && !strings.HasPrefix(f, "-") {
			continue
		}
		// First non-env, non-flag token that isn't git — not a git command.
		if !strings.HasPrefix(f, "-") {
			return false
		}
	}
	return false
}

// containsToken reports whether s contains the token as a whitespace-delimited
// word (not a substring of a longer word), ignoring trailing punctuation.
func containsToken(s, token string) bool {
	for _, f := range strings.Fields(s) {
		f = strings.TrimRight(f, ";&|")
		if f == token {
			return true
		}
	}
	return false
}

// isSigningKey reports whether key is a signing-related config key.
// Case-insensitive (git config section/variable names are case-insensitive).
func isSigningKey(key string) bool {
	key = strings.ToLower(key)
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

// checkGitDashC scans for `-c <key>=<val>`, `-c<key>=<val>`, and
// `--config-env=<key>=<envvar>` forms where key is a signing key.
func checkGitDashC(cmd string) string {
	fields := strings.Fields(cmd)
	for i, f := range fields {
		var val string
		switch {
		case f == "-c" && i+1 < len(fields):
			val = fields[i+1]
		case strings.HasPrefix(f, "-c") && len(f) > 2 && f[2] != '-':
			// Joined form: -ckey=val. But not -c<dash> which could be a
			// different flag like --config.
			val = f[2:]
		case f == "-c":
			continue
		case strings.HasPrefix(f, "--config-env="):
			// --config-env=<key>=<envvar> form.
			val = strings.TrimPrefix(f, "--config-env=")
			val = strings.Trim(val, `"'`)
			key := val
			if idx := strings.IndexByte(val, '='); idx >= 0 {
				key = val[:idx]
			}
			if isSigningKey(key) {
				return signingGuardError
			}
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

// gitConfigReadOnlyFlags are `git config` flags that read config without
// modifying it. When any of these is present (and no write form is), the
// invocation is a read and allowed even if it names a signing key.
var gitConfigReadOnlyFlags = map[string]bool{
	"--get":          true,
	"--get-all":      true,
	"--get-regexp":   true,
	"--get-urlmatch": true,
	"--list":         true,
	"-l":             true,
	"--show-origin":  true,
	"--show-scope":   true,
	"--name-only":    true,
	"--null":         true,
}

// isSigningSection reports whether section is the section part of any
// signing key (e.g., "gpg" covers "gpg.format" and "gpg.ssh.*"). Used for
// --remove-section and --rename-section, whose argument is a section name.
func isSigningSection(section string) bool {
	section = strings.ToLower(section)
	prefix := section + "."
	for _, k := range signingBypassKeys {
		if strings.HasPrefix(k, prefix) {
			return true
		}
	}
	for _, p := range signingBypassKeyPrefixes {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// gitConfigWriteFlags are `git config` flags that modify config. Their presence
// makes the invocation a write regardless of any read flags.
var gitConfigWriteFlags = map[string]bool{
	"--add":            true,
	"--unset":          true,
	"--unset-all":      true,
	"--replace-all":    true,
	"--rename-section":  true,
	"--remove-section":  true,
	"--edit":            true,
	"-e":                true,
}

// gitConfigSectionFlags take a section name (not a key) as their argument.
var gitConfigSectionFlags = map[string]bool{
	"--remove-section": true,
	"--rename-section": true,
}

// checkGitConfigSub detects `git config <key> <value>` where key is a signing
// key. It handles --global, --system, --local, --file, --blob, -C, -c (with
// their arguments) and the bare `git config <key> <value>` form.
//
// F7: a git config invocation is a read, and allowed, when it contains any
// read flag (--get, --get-all, --get-regexp, --list, -l, --show-origin,
// --show-scope, --get-urlmatch) and no write form (--add, --unset,
// --unset-all, --replace-all, --rename-section, --remove-section, --edit, -e,
// or a bare key value pair). A write on a signing key is refused.
//
// All `config` tokens in the command are checked (not just the first).
func checkGitConfigSub(cmd string) string {
	fields := strings.Fields(cmd)
	if len(fields) < 2 {
		return ""
	}
	for start := 0; start < len(fields); start++ {
		if fields[start] != "config" {
			continue
		}
		j := start - 1
		if j >= 0 && !strings.HasPrefix(fields[j], "-") {
			if j-1 >= 0 {
				prev := fields[j-1]
				if prev == "-C" || prev == "-c" || prev == "--file" || prev == "--blob" || prev == "-f" {
					j -= 2
				}
			}
		}
		for j >= 0 && strings.HasPrefix(fields[j], "-") {
			f := fields[j]
			if f == "-C" || f == "-c" || f == "--file" || f == "--blob" || f == "-f" {
				j -= 2
				continue
			}
			j--
		}
		if j < 0 || (fields[j] != "git" && !strings.HasSuffix(fields[j], "/git")) {
			continue
		}
		// Found `git ... config`. Scan ALL tokens after `config`: classify
		// as read or write and capture the first non-flag key.
		hasRead := false
		hasExplicitWrite := false
		hasSectionWrite := false
		hasEdit := false
		var section string
		var key string
		i := start + 1
		for i < len(fields) {
			f := fields[i]
			if !strings.HasPrefix(f, "-") {
				if key == "" {
					key = strings.ToLower(strings.Trim(f, `"'`))
				}
				break
			}
			if gitConfigReadOnlyFlags[f] {
				hasRead = true
				i++
				continue
			}
			if gitConfigWriteFlags[f] {
				hasExplicitWrite = true
				if gitConfigSectionFlags[f] {
					hasSectionWrite = true
					// The next token is the section name.
					if i+1 < len(fields) {
						section = strings.ToLower(strings.Trim(fields[i+1], `"'`))
					}
					i += 2
					continue
				}
				if f == "--edit" || f == "-e" {
					hasEdit = true
				}
				i++
				continue
			}
			if f == "--file" || f == "--blob" || f == "-f" || f == "-C" || f == "-c" {
				i += 2
				continue
			}
			if strings.HasPrefix(f, "--type=") {
				i++
				continue
			}
			i++
		}
		// --edit / -e opens the whole config file for editing — refuse.
		if hasEdit {
			return signingGuardError
		}
		// --remove-section / --rename-section on a section that contains
		// signing keys — refuse.
		if hasSectionWrite && section != "" && isSigningSection(section) {
			return signingGuardError
		}
		// Other explicit write flag (--add, --unset, --unset-all,
		// --replace-all) → check the key.
		if hasExplicitWrite && !hasSectionWrite {
			if key != "" && isSigningKey(key) {
				return signingGuardError
			}
			continue
		}
		// Read flag and no explicit write → read; allowed.
		if hasRead && !hasExplicitWrite {
			continue
		}
		// No read flag and no explicit write: a bare key (with or without a
		// value) is a write form. Check the key.
		if key != "" && isSigningKey(key) {
			return signingGuardError
		}
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
			for _, v := range signingBypassEnvVars {
				if name == v {
					return signingGuardError
				}
			}
			for _, p := range signingBypassEnvKeyPrefixes {
				if strings.HasPrefix(name, p) {
					return signingGuardError
				}
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