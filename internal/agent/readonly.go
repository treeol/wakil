// Package agent — readonly.go: shell-command classification for the auto-mode gate.
//
// isDestructiveShell is friction against accidental destruction in auto mode,
// NOT a security boundary. It is bypassable by construction: a model that wants
// to run `rm -rf /` can wrap it in a variable, a here-doc, or any other shell
// construct that defeats first-token analysis. The goal is to catch common
// accidental cases, not adversarial ones.
//
// Known limitations (inherent to first-token analysis, not fixable without a
// real shell parser):
//   - Env-var wrappers like `VAR=$(rm foo)` are caught by the $() check, but
//     deeply nested constructs could still evade detection.
//   - This is friction, not a security boundary. A model that wants to run
//     `rm -rf /` can wrap it in constructs that defeat first-token analysis.
//     The goal is to catch common accidental cases, not adversarial ones.
package agent

import "strings"

// readOnlyCmds is a conservative allowlist of shell binaries that only read.
// Commands with easy write vectors via positional output files or common flags
// (sort -o, uniq out, tee, dd, sed -i, awk 'print>') are deliberately excluded —
// misclassifying a write as a read is the dangerous direction, so when in doubt
// the command falls through to a normal confirm prompt rather than auto-approve.
//
// git is included because its read-only subcommands (diff, status, log, show,
// blame, etc.) are common investigative operations the agent runs before edits.
// The mutating subcommands (reset, clean, checkout --, push --force, stash drop,
// branch -D) are caught by IsDestructiveShell; readFlagsOK gates the remaining
// git subcommands to a read-only allowlist so non-recognized subcommands (e.g.
// "git add", "git commit") are NOT auto-approved.
//
// NB: "command" is deliberately NOT allowlisted — POSIX command executes
// its argument ("command rm -rf x" would be arbitrary execution).
var readOnlyCmds = map[string]bool{
	"cat": true, "bat": true, "tac": true, "nl": true, "head": true, "tail": true,
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true, "ack": true,
	"ls": true, "ll": true, "find": true, "fd": true, "tree": true, "pwd": true, "cd": true,
	"wc": true, "stat": true, "file": true, "du": true, "df": true, "cut": true, "comm": true,
	"echo": true, "printf": true, "which": true, "type": true,
	"whoami": true, "id": true, "hostname": true, "uname": true, "date": true,
	"printenv": true, "basename": true, "dirname": true,
	"readlink": true, "realpath": true, "diff": true, "cmp": true, "column": true,
	"od": true, "xxd": true, "hexdump": true, "strings": true, "ps": true,
	"less": true, "more": true, "seq": true, "true": true, "false": true,
	"test": true, "jq": true, "yq": true,
	"git": true,
}

// destructiveCmds is the set of shell binaries whose first-token presence makes
// a segment immediately destructive, with no flag inspection required.
// Shell wrappers (sh/bash/zsh) and xargs gate unconditionally because their
// payloads are opaque to first-token analysis.
var destructiveCmds = map[string]bool{
	"rm": true, "rmdir": true, "shred": true,
	"mv":   true, // may silently overwrite the destination
	"dd":   true, // raw I/O, almost always destructive
	"sudo": true, // privilege escalation — always gate
	"kill": true, "pkill": true, "killall": true, "sigkill": true,
	"mkfs": true, "fdisk": true, "parted": true,
	"truncate": true,
	// Shell wrappers: the payload is opaque; gate all invocations.
	"sh": true, "bash": true, "zsh": true,
	// xargs executes arbitrary commands on its input; gate unconditionally.
	"xargs": true,
	// tee writes its stdin to one or more files while passing it through.
	"tee": true,
}

// isDestructiveShell reports whether cmd contains an operation that must always
// require user confirmation, even when AutoApprove is enabled.
//
// Strategy: split on shell sequence operators (&&, ||, |, ;, &, newline) using
// splitShellSegments, then for each segment tokenize and match the FIRST token
// (after skipping leading VAR=value env assignments, same as IsReadOnlyShell).
// This avoids false positives like `echo "rm is a command"` (first token: echo)
// while still catching `safe-cmd && rm -rf x` (second segment first token: rm).
// Redirection (>, >>, 2>, &>) and command substitution ($(...), backticks) make
// a segment destructive because they can hide writes or arbitrary commands.
func IsDestructiveShell(cmd string) bool {
	c := strings.TrimSpace(cmd)
	if c == "" {
		return false
	}
	if strings.Contains(c, "`") {
		return true
	}
	if strings.Contains(c, "$(") || strings.Contains(c, "<(") {
		return true
	}
	if hasDestructiveRedirect(c) {
		return true
	}
	for _, seg := range splitShellSegments(c) {
		fields := strings.Fields(seg)
		if len(fields) == 0 {
			continue
		}
		i := 0
		for i < len(fields) && !strings.HasPrefix(fields[i], "-") && strings.Contains(fields[i], "=") {
			if name := envPrefixName(fields[i]); dangerousEnvPrefix(name) {
				return true
			}
			i++
		}
		if i >= len(fields) {
			continue
		}
		bin := unquoteShellArg(fields[i])
		if j := strings.LastIndex(bin, "/"); j >= 0 {
			bin = bin[j+1:]
		}
		if destructiveCmds[bin] {
			return true
		}
		rawArgs := fields[i+1:]
		args := make([]string, len(rawArgs))
		for k, a := range rawArgs {
			args[k] = unquoteShellArg(a)
		}
		switch bin {
		case "git":
			if len(args) == 0 {
				continue
			}
			switch args[0] {
			case "reset", "clean":
				return true
			case "checkout":
				for _, a := range args[1:] {
					if a == "--" {
						return true
					}
				}
			case "push":
				for _, a := range args[1:] {
					if a == "--force" || a == "-f" {
						return true
					}
				}
			case "stash":
				if len(args) >= 2 && (args[1] == "drop" || args[1] == "clear") {
					return true
				}
			case "branch":
				for _, a := range args[1:] {
					if a == "-D" {
						return true
					}
				}
			}
		case "find":
			for _, a := range args {
				if a == "-delete" || a == "-exec" || a == "-execdir" {
					return true
				}
			}
		case "rsync":
			for _, a := range args {
				if strings.HasPrefix(a, "--delete") {
					return true
				}
			}
		case "sed":
			for _, a := range args {
				if a == "-i" || strings.HasPrefix(a, "-i") && len(a) > 2 {
					return true
				}
			}
		case "chmod":
			for _, a := range args {
				if a == "-R" || a == "-r" || a == "--recursive" {
					return true
				}
			}
		case "chown":
			for _, a := range args {
				if a == "-R" || a == "-r" || a == "--recursive" {
					return true
				}
			}
		}
	}
	return false
}

// hasDestructiveRedirect reports whether cmd contains a redirection that
// writes to a file (>, >>, or a numbered prefix thereof that targets a
// real path). It returns false for fd duplication (2>&1, >&2, 1>&2,
// 2>&-) and for redirects to /dev/null (>/dev/null, 2>/dev/null,
// &>/dev/null) — those only copy or discard existing output and do not
// create or truncate a file.
//
// fd duplication requires an explicit "&" after the ">" (N>&M, >&M, &>M).
// "2>1" is a file write (to a file named "1"), not fd duplication — only
// "2>&1" copies fd 1 to fd 2. /dev/null must match as an exact token
// (followed by end-of-string, whitespace, or a shell separator) so
// ">/dev/nullfoo" is a file write, not a discard.
func hasDestructiveRedirect(cmd string) bool {
	for i := 0; i < len(cmd); i++ {
		if cmd[i] != '>' {
			continue
		}
		// Skip the second '>' of '>>' (append) — it is handled by
		// the next iteration as a single '>'.
		if i > 0 && cmd[i-1] == '>' {
			continue
		}
		// Skip whitespace after the '>' (or '>>').
		j := i + 1
		if j < len(cmd) && cmd[j] == '>' {
			j++ // append redirect ('>>')
		}
		for j < len(cmd) && (cmd[j] == ' ' || cmd[j] == '\t') {
			j++
		}
		right := ""
		if j < len(cmd) {
			right = cmd[j:]
		}

		// fd duplication: N>&M, N>&-, >&M, &>M — requires an explicit
		// '&' immediately after the '>'. Without '&', "2>1" writes to
		// a file named "1".
		if len(right) > 0 && right[0] == '&' {
			// &> followed by /dev/null (exact token) is a discard.
			if isDevNull(right) {
				continue
			}
			// &>M or >&M with M a digit or '-' is fd duplication.
			if len(right) > 1 && (right[1] == '-' ||
				(right[1] >= '0' && right[1] <= '9')) {
				continue
			}
			// &> followed by a file path is a write — destructive.
			return true
		}

		// Redirect to /dev/null (exact token): discard, not a write.
		if isDevNull(right) {
			continue
		}

		// Any other '>' or '>>' is a redirect to a file — destructive.
		return true
	}
	return false
}

// isDevNull reports whether right begins with "/dev/null" as an exact
// token — followed by end-of-string, whitespace, or a shell separator
// (|, ;, &, newline). "/dev/nullfoo" is NOT /dev/null.
func isDevNull(right string) bool {
	const token = "/dev/null"
	if !strings.HasPrefix(right, token) {
		return false
	}
	rest := right[len(token):]
	if rest == "" {
		return true
	}
	switch rest[0] {
	case ' ', '\t', '|', ';', '&', '\n':
		return true
	}
	return false
}

// unquoteShellArg strips symmetric surrounding single or double quotes from a
// token.
func unquoteShellArg(s string) string {
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// hasExpansionOrEscape reports whether a token contains shell constructs this
// first-token classifier cannot model.
func hasExpansionOrEscape(tok string) bool {
	return strings.ContainsAny(tok, `$\'"{}`+"`")
}

// envPrefixName extracts the variable name from a leading VAR=value token.
func envPrefixName(tok string) string {
	if j := strings.Index(tok, "="); j > 0 {
		return tok[:j]
	}
	return ""
}

// dangerousEnvPrefix reports whether an env assignment can influence execution
// of allowlisted readers.
func dangerousEnvPrefix(name string) bool {
	if name == "PATH" || strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_") {
		return true
	}
	if strings.HasPrefix(name, "GIT_") || strings.HasPrefix(name, "LESS") {
		return true
	}
	if strings.Contains(name, "PAGER") {
		return true
	}
	return false
}

// isReadOnlyShell reports whether a shell command is safe to treat as read-only.
// It checks the same redirect and substitution guards as IsDestructiveShell,
// then walks each segment (split by splitShellSegments) and verifies the
// binary is in readOnlyCmds and its flags pass readFlagsOK. Env-var prefixes
// are checked for dangerous overrides (PATH, LD_*, GIT_*, etc.) so a wrapper
// like `GIT_CONFIG_COUNT=1 cat file` is not treated as read-only.
func IsReadOnlyShell(cmd string) bool {
	c := strings.TrimSpace(cmd)
	if c == "" {
		return false
	}
	if strings.Contains(c, "`") {
		return false
	}
	if strings.Contains(c, "$(") || strings.Contains(c, "<(") {
		return false
	}
	if hasDestructiveRedirect(c) {
		return false
	}
	segs := splitShellSegments(c)
	if len(segs) == 0 {
		return false
	}
	for _, seg := range segs {
		fields := strings.Fields(seg)
		i := 0
		for i < len(fields) && !strings.HasPrefix(fields[i], "-") && strings.Contains(fields[i], "=") {
			if name := envPrefixName(fields[i]); dangerousEnvPrefix(name) {
				return false
			}
			i++
		}
		if i >= len(fields) {
			return false
		}
		bin := unquoteShellArg(fields[i])
		if j := strings.LastIndex(bin, "/"); j >= 0 {
			bin = bin[j+1:]
		}
		if !readOnlyCmds[bin] {
			return false
		}
		rawArgs := fields[i+1:]
		args := make([]string, len(rawArgs))
		for k, a := range rawArgs {
			args[k] = unquoteShellArg(a)
		}
		if hasExpansionOrEscape(bin) {
			return false
		}
		for _, a := range args {
			if hasExpansionOrEscape(a) {
				return false
			}
		}
		if !readFlagsOK(bin, args) {
			return false
		}
	}
	return true
}

// splitShellSegments breaks a command on the operators that sequence or connect
// commands. Redirections (2>&1, &>, etc.) are protected from splitting on '&':
// the splitter skips a '&' that immediately follows a '>' (fd duplication like
// 2>&1) or precedes one (&>). A standalone '&' backgrounds a command and is a
// separator; the '&&' form is handled first (replaced before the single-'&'
// pass sees it).
func splitShellSegments(c string) []string {
	// First pass: replace && and || with the sentinel so the single-&
	// pass below does not split fd-dup redirections like 2>&1.
	r := strings.NewReplacer("&&", "\x00", "||", "\x00", "|", "\x00", ";", "\x00", "\n", "\x00")
	replaced := r.Replace(c)
	// Second pass: split on '\x00' but keep single '&' as a separator
	// only when it is NOT part of a redirection (i.e. not preceded by
	// '>' and not followed by '>'). A standalone '&' backgrounds the
	// preceding command, so it is a separator.
	var out []string
	for _, p := range strings.Split(replaced, "\x00") {
		// Within this segment, split on '&' that is not part of >& or &>.
		for _, sub := range splitOnAmpersand(p) {
			if s := strings.TrimSpace(sub); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// splitOnAmpersand splits s on '&' characters that are NOT part of a
// redirection. A '&' is a separator (background operator) when it is
// standalone — not immediately preceded by '>' and not immediately
// followed by '>'. "2>&1" stays as one segment; "cmd &" splits into
// "cmd " and "".
func splitOnAmpersand(s string) []string {
	var result []string
	var buf strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '&' {
			// Check if this '&' is part of a redirection.
			// >& (fd duplication): '&' preceded by '>' — not a separator.
			// &> (combined redirect): '&' followed by '>' — not a separator.
			prevGT := i > 0 && s[i-1] == '>'
			nextGT := i+1 < len(s) && s[i+1] == '>'
			if prevGT || nextGT {
				buf.WriteByte(s[i])
				continue
			}
			// Standalone '&' — background operator, split here.
			result = append(result, buf.String())
			buf.Reset()
		} else {
			buf.WriteByte(s[i])
		}
	}
	result = append(result, buf.String())
	return result
}

// readFlagsOK rejects flags that turn an otherwise-read command into a writer.
// It is the second gate for readOnlyCmds: even when a binary is allowlisted,
// flags like --output, -i (in-place), -O (pager), or -delete can write files.
func readFlagsOK(bin string, args []string) bool {
	switch bin {
	case "git":
		if len(args) == 0 {
			return false
		}
		switch args[0] {
		case "diff", "status", "log", "show", "blame", "shortlog",
			"ls-files", "ls-tree", "rev-parse", "describe", "name-rev",
			"diff-tree", "cat-file", "ls-remote", "for-each-ref",
			"rev-list", "range-diff", "merge-base", "cherry":
			for _, a := range args[1:] {
				if strings.HasPrefix(a, "--output") || a == "--ext-diff" ||
					a == "--no-textconv" || a == "--textconv" {
					return false
				}
			}
			return true
		case "reflog":
			if len(args) == 1 {
				return true
			}
			switch args[1] {
			case "show", "list":
				for _, a := range args[2:] {
					if strings.HasPrefix(a, "--output") {
						return false
					}
				}
				return true
			}
			return false
		case "branch":
			allowed := map[string]bool{
				"-a": true, "-v": true, "-vv": true, "-r": true,
				"--list": true, "--show-current": true, "--all": true,
				"--remotes": true, "--column": true, "--no-column": true,
			}
			for _, a := range args[1:] {
				if !allowed[a] {
					return false
				}
			}
			return true
		case "grep":
			for _, a := range args[1:] {
				if a == "-O" || strings.HasPrefix(a, "--open-files-in-pager") {
					return false
				}
			}
			return true
		case "stash":
			if len(args) < 2 || args[1] != "list" {
				return false
			}
			for _, a := range args[2:] {
				if strings.HasPrefix(a, "--output") {
					return false
				}
			}
			return true
		case "config":
			for _, a := range args[1:] {
				if a == "--get" || a == "--get-all" || a == "--list" || a == "-l" {
					return true
				}
			}
			return false
		}
		return false
	case "xxd":
		pos := 0
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				pos++
			}
		}
		if pos >= 2 {
			return false
		}
	case "less", "more", "tree":
		for _, a := range args {
			switch {
			case strings.HasPrefix(a, "-o"), strings.HasPrefix(a, "-O"),
				strings.HasPrefix(a, "--log-file"):
				return false
			}
		}
	case "rg":
		for _, a := range args {
			if strings.HasPrefix(a, "--pre") || strings.HasPrefix(a, "--hostname-bin") {
				return false
			}
		}
	case "ag", "ack":
		for _, a := range args {
			if strings.HasPrefix(a, "--pager") || strings.HasPrefix(a, "+") {
				return false
			}
		}
	case "find", "fd":
		for _, a := range args {
			switch a {
			case "-delete", "-exec", "-execdir", "-ok", "-okdir",
				"-fprint", "-fprintf", "-fls", "-fprint0", "--exec", "-x",
				"-X", "--exec-batch":
				return false
			}
		}
	case "yq":
		for _, a := range args {
			if a == "-i" || a == "--inplace" || a == "-s" || strings.HasPrefix(a, "--split-exp") {
				return false
			}
		}
	case "date":
		for _, a := range args {
			if a == "-s" || a == "--set" || strings.HasPrefix(a, "--set=") {
				return false
			}
		}
	case "hostname":
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				return false
			}
		}
	}
	return true
}
