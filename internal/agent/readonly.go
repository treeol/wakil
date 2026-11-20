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
	"sed": true, // read-only when no -i and only print/address forms (readFlagsOK gates w/e/-i)
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

// extractDollarParenInners returns the content of every $(...) pair in s.
// Returns nil when there is none. Unbalanced $( is ignored (returns what
// was found so far).
func extractDollarParenInners(s string) []string {
	var result []string
	i := 0
	for {
		j := strings.Index(s[i:], "$(")
		if j < 0 {
			break
		}
		start := i + j + 2
		depth := 1
		k := start
		for k < len(s) && depth > 0 {
			switch s[k] {
			case '(':
				depth++
			case ')':
				depth--
			}
			k++
		}
		if depth == 0 {
			result = append(result, s[start:k-1])
		}
		i = k
	}
	return result
}

// isLoopConstruct reports whether cmd begins with a for/while/until loop or
// a case statement — control structures whose read-only/destructive
// classification depends on every command in the header and body.
func isLoopConstruct(cmd string) (kind string, ok bool) {
	c := strings.TrimSpace(cmd)
	if strings.HasPrefix(c, "for ") || strings.HasPrefix(c, "for\t") {
		return "for", true
	}
	if strings.HasPrefix(c, "while ") || strings.HasPrefix(c, "while\t") {
		return "while", true
	}
	if strings.HasPrefix(c, "until ") || strings.HasPrefix(c, "until\t") {
		return "until", true
	}
	if strings.HasPrefix(c, "case ") || strings.HasPrefix(c, "case\t") {
		return "case", true
	}
	return "", false
}

// loopBodyCommands extracts the body commands from a for/while/until loop
// or case statement. For loops it returns everything after "do". For case
// it returns every command after each ")". Conservative: on parse
// failure returns nil (the caller treats the construct as destructive
// / not read-only).
func loopBodyCommands(cmd string) []string {
	c := strings.TrimSpace(cmd)
	// Find "do" keyword (for/while/until) or extract case branches.
	if strings.HasPrefix(c, "case ") || strings.HasPrefix(c, "case\t") {
		// case: extract commands after each ")". Conservative: return
		// nil on unparseable.
		var cmds []string
		i := strings.Index(c, " in ")
		if i < 0 {
			return nil
		}
		rest := c[i+4:]
		for {
			j := strings.Index(rest, ")")
			if j < 0 {
				break
			}
			rest = rest[j+1:]
			// Everything up to the next ";;" or "esac" is the command.
			k := strings.Index(rest, ";;")
			if k < 0 {
				cmds = append(cmds, strings.TrimSpace(rest))
				break
			}
			cmds = append(cmds, strings.TrimSpace(rest[:k]))
			rest = rest[k+2:]
		}
		return cmds
	}
	// for/while/until: find "do" keyword, return everything after it.
	j := strings.Index(c, " do")
	if j < 0 {
		j = strings.Index(c, "\tdo")
	}
	if j < 0 {
		return nil
	}
	body := strings.TrimSpace(c[j+3:])
	// Strip trailing "done".
	if k := strings.LastIndex(body, "done"); k >= 0 {
		body = strings.TrimSpace(body[:k])
	}
	// Split on ; and && to get individual commands.
	var cmds []string
	for _, p := range strings.Split(body, ";") {
		for _, q := range strings.Split(p, "&&") {
			if s := strings.TrimSpace(q); s != "" {
				cmds = append(cmds, s)
			}
		}
	}
	return cmds
}

// loopHeaderCommands extracts the header commands from a for/while/until
// loop (everything between the keyword and "do"). For `for x in $(...)` the
// inner $(...) is a header command. For `while cmd` / `until cmd` the cmd
// itself is the header command. For `for x in list` the list is NOT a
// command — it's a file list or literal — so it returns nil (the list is
// checked separately by the destructive redirect / $() checks). Returns
// nil on parse failure.
func loopHeaderCommands(cmd string) []string {
	c := strings.TrimSpace(cmd)
	end := strings.Index(c, " do")
	if end < 0 {
		end = strings.Index(c, "\tdo")
	}
	if end < 0 {
		return nil
	}
	header := strings.TrimSpace(c[:end])
	// For "while cmd" / "until cmd": return cmd.
	if strings.HasPrefix(header, "while ") || strings.HasPrefix(header, "until ") {
		return []string{strings.TrimSpace(header[6:])}
	}
	// For "for x in list": the list is not a command unless it contains
	// $(...) — a file glob or literal list is not a command. Return nil
	// so the caller only checks the body. The $(...) check at the top of
	// IsReadOnlyShell/IsDestructiveShell catches command substitution in
	// the header.
	return nil
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
	// for/while/until/case: destructive when any command in the header or
	// body is destructive.
	if _, ok := isLoopConstruct(c); ok {
		for _, hc := range loopHeaderCommands(c) {
			if IsDestructiveShell(hc) {
				return true
			}
		}
		for _, bc := range loopBodyCommands(c) {
			if IsDestructiveShell(bc) {
				return true
			}
		}
		return false
	}
	if strings.Contains(c, "`") {
		return true // backtick substitution — always destructive (opaque inner)
	}
	if strings.Contains(c, "$(") || strings.Contains(c, "<(") {
		// Process substitution <(...) always creates a temp fifo — destructive.
		if strings.Contains(c, "<(") {
			return true
		}
		// $(...) is destructive when any inner command is destructive.
		for _, inner := range extractDollarParenInners(c) {
			if IsDestructiveShell(inner) {
				return true
			}
		}
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
				// Shell treats \X as literal X — strip backslashes for matching.
				aa := strings.ReplaceAll(a, "\\", "")
				if aa == "-delete" || aa == "-exec" || aa == "-execdir" {
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
			hasScript := false
			for _, a := range args {
				// Shell treats \X as literal X — strip backslashes for matching.
				aa := strings.ReplaceAll(a, "\\", "")
				if aa == "-i" || strings.HasPrefix(aa, "-i") {
					return true
				}
				// Check sed script for destructive commands (w, e, r, a, i, c,
				// d, D, s///w). -e adds a script; non-flag arg is the script.
				if aa == "-e" || strings.HasPrefix(aa, "-e") {
					script := strings.TrimPrefix(aa, "-e")
					script = strings.TrimPrefix(script, " ")
					if sedScriptIsDestructive(script) {
						return true
					}
					hasScript = true
				} else if !strings.HasPrefix(aa, "-") {
					// First non-flag arg is the script (or a file if -e was used).
					if !hasScript && sedScriptIsDestructive(a) {
						return true
					}
					hasScript = true
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
// first-token classifier cannot model. A bare $ followed by a letter or
// underscore is a variable reference (safe); $(, ${, backtick, and quote
// characters are dangerous.
func hasExpansionOrEscape(tok string) bool {
	for i := 0; i < len(tok); i++ {
		switch tok[i] {
		case '$':
			// $ followed by ( or { is command/parameter substitution — dangerous.
			if i+1 < len(tok) && (tok[i+1] == '(' || tok[i+1] == '{') {
				return true
			}
			// $ followed by a letter or _ is a variable reference — safe.
			// $ at end of string or followed by non-alnum is ambiguous — conservative.
			if i+1 >= len(tok) {
				return true
			}
			c := tok[i+1]
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_') {
				return true
			}
		case '`', '\'', '"', '{', '}':
			return true
		}
	}
	return false
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
// like `GIT_CONFIG_COUNT=1 cat file` is not treated as read-only. Command
// substitution ($(...), backticks) is read-only when the inner command is
// read-only; a for/while loop is read-only when every command in the header
// and body is read-only.
func IsReadOnlyShell(cmd string) bool {
	c := strings.TrimSpace(cmd)
	if c == "" {
		return false
	}
	// for/while/until/case: read-only when every command in the header
	// and body is read-only.
	if _, ok := isLoopConstruct(c); ok {
		for _, hc := range loopHeaderCommands(c) {
			if !IsReadOnlyShell(hc) {
				return false
			}
		}
		for _, bc := range loopBodyCommands(c) {
			if !IsReadOnlyShell(bc) {
				return false
			}
		}
		return true
	}
	if strings.Contains(c, "`") {
		// Backtick substitution — always non-read-only (opaque inner).
		return false
	}
	if strings.Contains(c, "$(") || strings.Contains(c, "<(") {
		// Command/process substitution: read-only when every inner command
		// is read-only. Process substitution <(...) is always destructive
		// (it creates a temp fifo).
		if strings.Contains(c, "<(") {
			return false
		}
		for _, inner := range extractDollarParenInners(c) {
			if !IsReadOnlyShell(inner) {
				return false
			}
		}
		// All inner commands are read-only → the substitution is read-only.
		return true
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

// sedScriptIsDestructive reports whether a sed script fragment contains a
// command that writes, edits, or deletes — anything beyond print and address
// forms. It scans for the destructive command letters: w (write), e (exec),
// r (read file), a/i/c (append/insert/change), d/D (delete), and s with a
// write flag (s///w). A bare address or print (p, P, =, n, l, q, b, N) is
// read-only. Conservative: if the script is ambiguous or unparseable,
// returns true (gate it).
func sedScriptIsDestructive(script string) bool { // Strip leading address (digits, $, /regex/, \X, ,step). We only need
	// to find the command letter after the address.
	s := strings.TrimSpace(script)
	if s == "" {
		return false
	}
	// Skip a leading address: digits, $, /.../ or \c, optionally
	// followed by a comma and another address.
	i := 0
	skipAddr := func() {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i < len(s) && s[i] == '$' {
			i++
		} else if i < len(s) && s[i] >= '0' && s[i] <= '9' {
			for i < len(s) && s[i] >= '0' && s[i] <= '9' {
				i++
			}
		} else if i < len(s) && s[i] == '/' {
			i++
			for i < len(s) && s[i] != '/' {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				i++
			}
			if i < len(s) {
				i++ // skip closing /
			}
		} else if i < len(s) && s[i] == '\\' {
			i++
			if i < len(s) {
				i++
			}
		}
		// Skip optional comma + second address.
		if i < len(s) && s[i] == ',' {
			i++
			skipAddr2 := func() {
				for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
					i++
				}
				if i < len(s) && s[i] == '$' {
					i++
				} else if i < len(s) && s[i] >= '0' && s[i] <= '9' {
					for i < len(s) && s[i] >= '0' && s[i] <= '9' {
						i++
					}
				} else if i < len(s) && s[i] == '/' {
					i++
					for i < len(s) && s[i] != '/' {
						if s[i] == '\\' && i+1 < len(s) {
							i++
						}
						i++
					}
					if i < len(s) {
						i++
					}
				}
			}
			skipAddr2()
		}
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
	}
	skipAddr()
	if i >= len(s) {
		// Address-only (no command) — read-only.
		return false
	}
	switch s[i] {
	case 'p', 'P', '=', 'n', 'l', 'q', 'b', 'N', '#':
		return false
	case 'd', 'D':
		return true
	case 'a', 'i', 'c':
		return true
	case 'r':
		return true
	case 'w':
		return true
	case 'e':
		return true
	case 's':
		// s is destructive when it has a write flag (w) in the flags
		// after the delimiter. e.g. s/a/b/w or s|a|b|gw
		// Find the delimiter, skip the pattern and replacement, check flags.
		if i+1 >= len(s) {
			return true // s with no delimiter — conservative
		}
		delim := s[i+1]
		j := i + 2
		for j < len(s) && s[j] != delim {
			if s[j] == '\\' && j+1 < len(s) {
				j++
			}
			j++
		}
		if j >= len(s) {
			return true // no closing delimiter — conservative
		}
		j++ // skip closing delimiter (end of pattern)
		for j < len(s) && s[j] != delim {
			if s[j] == '\\' && j+1 < len(s) {
				j++
			}
			j++
		}
		if j >= len(s) {
			return true // no second closing delimiter — conservative
		}
		j++ // skip second closing delimiter (end of replacement)
		// Check flags for w (write).
		for j < len(s) && s[j] != ' ' && s[j] != '\t' && s[j] != ';' && s[j] != '\n' {
			if s[j] == 'w' {
				return true
			}
			j++
		}
		return false
	default:
		// Unknown command letter — conservative: gate it.
		return true
	}
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
			// Shell treats \X as literal X (backslash before non-special char).
			// Strip ALL backslashes for flag matching (the shell would have
			// removed them before the binary saw the arg).
			aa := strings.ReplaceAll(a, "\\", "")
			switch aa {
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
	case "sed":
		// sed is read-only only when its script consists solely of print
		// and address forms (e.g. -n '5p', -n '1,10p', 5p, 1,10p). Any
		// command that writes (w, a, i, c), edits (s///w, e), reads a
		// file (r), or deletes (d, D) is destructive. -i is already
		// caught by IsDestructiveShell.
		hasScript := false
		for _, a := range args {
			// Shell treats \X as literal X — strip backslashes for matching.
			aa := strings.ReplaceAll(a, "\\", "")
			if strings.HasPrefix(aa, "-") {
				if aa == "-i" || strings.HasPrefix(aa, "-i") {
					return false // -i is destructive
				}
				// -n is fine (suppress auto-print). -e adds a script.
				if aa == "-e" || strings.HasPrefix(aa, "-e") {
					script := strings.TrimPrefix(aa, "-e")
					script = strings.TrimPrefix(script, " ")
					if sedScriptIsDestructive(script) {
						return false
					}
					hasScript = true
				}
				continue
			}
			// Non-flag arg: this is the script (or a file when a script
			// was already given via -e). If no -e was seen, the first
			// non-flag arg is the script.
			if !hasScript {
				if sedScriptIsDestructive(a) {
					return false
				}
				hasScript = true
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
