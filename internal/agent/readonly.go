// Package agent — readonly.go: shell-command classification for the auto-mode gate.
//
// What the classifier guarantees:
//   - IsReadOnlyShell reports true only when it positively recognises every part
//     of the command: each segment's command word is in readOnlyCmds, its
//     arguments pass readFlagsOK, and every redirect, substitution and loop is
//     one of the forms listed below. Anything it cannot parse, or is unsure
//     about, is not read-only. It fails closed.
//   - IsDestructiveShell reports true for commands that remove, overwrite,
//     kill, escalate, wrap another command, or write through a redirect. It
//     also reports true for a command word it cannot judge (a variable, a
//     substitution, a quoted or escaped name, a shell keyword or a "(" group).
//     A command that IsReadOnlyShell accepts is never reported destructive.
//     A command that is neither is gated for confirmation, not auto-approved.
//   - The intended read-only forms are: fd duplication (">&N", "N>&M", "N>&-"
//     followed by a token boundary), redirects to exactly /dev/null, sed print
//     and address forms without -i, -f, -e tricks or "--", a complete one-line
//     for/while/until loop whose header and body are read-only, and "$(...)"
//     whose inner and outer commands are read-only.
//
// This is a classifier for an auto-approval gate. It is not a security
// boundary: it reads the command text, not what the shell or a program will do
// at run time. The sandbox (hisn, Docker or the direct backend) is the real
// boundary. Use this classifier to avoid asking for routine reads and to stop
// accidental destruction, and rely on the sandbox to contain anything else.
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

// substituteDollarParen replaces every balanced $(...) in s with the
// variable-shaped placeholder $__SUBST and returns the inner command strings.
// ok is false when a "$(" has no matching ")": callers must then fail closed
// (treat the command as destructive / not read-only). The placeholder is
// variable-shaped on purpose: a substituted word is as unknowable as a
// variable, so it gets the same restrictions (see varSafeCmds).
func substituteDollarParen(s string) (out string, inners []string, ok bool) {
	var b strings.Builder
	i := 0
	for i < len(s) {
		if s[i] == '$' && i+1 < len(s) && s[i+1] == '(' {
			depth := 1
			k := i + 2
			for k < len(s) && depth > 0 {
				switch s[k] {
				case '(':
					depth++
				case ')':
					depth--
				}
				k++
			}
			if depth != 0 {
				return "", nil, false
			}
			inners = append(inners, s[i+2:k-1])
			b.WriteString("$__SUBST")
			i = k
			continue
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String(), inners, true
}

// shellKeywords are shell reserved words and grouping tokens. A command
// segment that starts with one is a compound construct this first-token
// classifier does not model (case, if, a loop that did not parse as one
// complete "for/while/until ...; do ...; done" line, a { } group), so it is
// always gated: IsDestructiveShell reports true and IsReadOnlyShell false.
var shellKeywords = map[string]bool{
	"for": true, "while": true, "until": true, "case": true, "select": true,
	"if": true, "then": true, "else": true, "elif": true, "fi": true,
	"do": true, "done": true, "esac": true, "function": true,
	"{": true, "}": true,
}

// wrapperCmds run another command given as an argument. They are gated in
// IsDestructiveShell and never allowlisted, because the payload would need
// its own classification (and "time", "env" and "nice" take options).
var wrapperCmds = map[string]bool{
	"env": true, "command": true, "nice": true, "time": true, "nohup": true,
	"timeout": true, "exec": true, "eval": true, "doas": true, "su": true,
	"source": true, ".": true, "builtin": true, "ionice": true, "stdbuf": true,
	"setsid": true, "chroot": true, "strace": true, "ltrace": true, "watch": true,
	"busybox": true, "flock": true, "unbuffer": true, "script": true,
}

// varSafeCmds are the read-only binaries that may receive $VAR (or a $(...)
// result) as an argument. For every other allowlisted binary a variable could
// expand to a write flag (find -delete, git diff --output=, date -s, sed -i),
// so a variable argument keeps the command off the read-only path.
var varSafeCmds = map[string]bool{
	"echo": true, "printf": true, "cat": true, "tac": true, "nl": true,
	"ls": true, "head": true, "tail": true, "wc": true, "pwd": true,
	"grep": true, "egrep": true, "fgrep": true, "basename": true,
	"dirname": true, "stat": true, "readlink": true,
	"realpath": true, "cut": true, "seq": true,
}

// loopKind returns "for", "while" or "until" when c starts with that keyword
// followed by a space, else "". Other spellings (tab, newline) are not
// parsed: they fall through to the generic path where the keyword gate
// (shellKeywords) catches them.
func loopKind(c string) string {
	for _, k := range []string{"for", "while", "until"} {
		if strings.HasPrefix(c, k+" ") {
			return k
		}
	}
	return ""
}

// splitLoop splits a single-line loop of the exact shape
//
//	KIND HEADER; do BODY; done
//
// into header and body. Anything else — a newline, ";do" without the space,
// a redirect or a further command after "done", an empty header or body —
// is not parsed (ok false) and the callers fail closed. Because the line must
// END in "; done", nothing can follow the loop.
func splitLoop(c, kind string) (header, body string, ok bool) {
	if strings.ContainsAny(c, "\n\r") || !strings.HasSuffix(c, "; done") {
		return "", "", false
	}
	idx := strings.Index(c, "; do ")
	if idx < len(kind)+1 {
		return "", "", false
	}
	start := idx + len("; do ")
	end := len(c) - len("; done")
	if start > end {
		return "", "", false
	}
	header = strings.TrimSpace(c[len(kind)+1 : idx])
	body = strings.TrimSpace(c[start:end])
	if header == "" || body == "" {
		return "", "", false
	}
	return header, body, true
}

// loopHeaderCommand returns the command whose classification stands for the
// loop header. "while CMD" / "until CMD" use CMD itself. "for NAME in LIST"
// uses "echo LIST": the list words are arguments, so command substitution,
// backticks, redirects and expansions in it are judged by the ordinary
// rules. Any other "for" shape (no "in", arithmetic form) is not parsed.
func loopHeaderCommand(kind, header string) (string, bool) {
	if kind != "for" {
		return header, true
	}
	f := strings.Fields(header)
	if len(f) < 2 || f[1] != "in" || f[0] == "" {
		return "", false
	}
	// A loop variable that a shell or a read-only tool depends on changes what
	// the body runs ("for PATH in .; do ls; done" runs ./ls). Gate those.
	if dangerousLoopVar(f[0]) {
		return "", false
	}
	for i := 0; i < len(f[0]); i++ {
		ch := f[0][i]
		isAlpha := (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || ch == '_'
		isDigit := ch >= '0' && ch <= '9'
		if !isAlpha && !(isDigit && i > 0) {
			return "", false
		}
	}
	return "echo " + strings.Join(f[2:], " "), true
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
//
// Fail closed: a segment whose first token is a variable or substitution
// ("$X -rf /", "$(echo rm) x"), a shell keyword (case, if, a loop that is not
// one complete "for/while/until ...; do ...; done" line) or a "(" group is
// destructive. A complete one-line loop is destructive when its header or its
// body is.
func IsDestructiveShell(cmd string) bool {
	c := strings.TrimSpace(cmd)
	if c == "" {
		return false
	}
	if kind := loopKind(c); kind != "" {
		header, body, ok := splitLoop(c, kind)
		if !ok {
			return true
		}
		hc, ok := loopHeaderCommand(kind, header)
		if !ok {
			return true
		}
		return IsDestructiveShell(hc) || IsDestructiveShell(body)
	}
	if strings.Contains(c, "`") {
		return true // backtick substitution — always destructive (opaque inner)
	}
	// Process substitution <(...) always creates a temp fifo — destructive.
	if strings.Contains(c, "<(") {
		return true
	}
	// $(...) is destructive when any inner command is destructive; an
	// unbalanced "$(" is destructive (fail closed).
	c2, inners, ok := substituteDollarParen(c)
	if !ok {
		return true
	}
	for _, inner := range inners {
		if IsDestructiveShell(inner) {
			return true
		}
	}
	if hasDestructiveRedirect(c) {
		return true
	}
	for _, seg := range splitShellSegments(c2) {
		fields := strings.Fields(seg)
		if len(fields) == 0 {
			continue
		}
		i, bad := skipLeadingPrefixes(fields)
		if bad {
			return true
		}
		if i >= len(fields) {
			continue
		}
		rawBin := unquoteShellArg(fields[i])
		// The command word itself cannot be judged when it is a variable or
		// a substitution, a keyword, or a "(" group.
		if strings.ContainsAny(rawBin, "$`") || strings.HasPrefix(rawBin, "(") || shellKeywords[rawBin] {
			return true
		}
		// Quotes or a backslash in the command word hide its name ("r\"\"m",
		// "\rm"), so the word cannot be judged: gate it.
		if strings.ContainsAny(fields[i], "'\"\\") {
			return true
		}
		// Wrappers run another command; their payload is judged by its own
		// rules, so gate them rather than unwrapping.
		if wrapperCmds[rawBin] {
			return true
		}

		bin := rawBin
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
		// A substitution or variable argument can expand to any flag, so a
		// binary that has flags it could misuse is gated here too (the
		// read-only path gates the same case through varSafeCmds).
		if !varSafeCmds[bin] {
			for _, a := range rawArgs {
				if strings.Contains(a, "$__SUBST") {
					return true
				}
			}
		}
		switch bin {
		case "git":
			// Global options before the subcommand: "-C <dir>" is skipped;
			// any other option ("-c", "--git-dir", ...) gates the command.
			sub, ok := gitGlobalOptsOK(args)
			if !ok {
				return true
			}
			args = args[sub:]
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
			if sedArgsDestructive(args, rawArgs) {
				return true
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

// skipLeadingPrefixes walks the words that precede the command word and
// returns the index of the command word. Leading VAR=value assignments and
// leading redirects may appear in any order. An assignment that sets a
// dangerous name, or a redirect that is not a harmless one (see
// leadingRedirectOK), sets bad: the caller must then fail closed.
func skipLeadingPrefixes(fields []string) (i int, bad bool) {
	for i < len(fields) {
		tok := fields[i]
		switch {
		case strings.HasPrefix(tok, "-"):
			return i, false
		case startsRedirect(tok):
			consumed, ok := leadingRedirectOK(fields, i)
			if !ok {
				return i, true
			}
			i += 1 + consumed
		case strings.Contains(tok, "="):
			if dangerousEnvPrefix(envPrefixName(tok)) {
				return i, true
			}
			i++
		default:
			return i, false
		}
	}
	return i, false
}

// startsRedirect reports whether tok begins with a redirect operator: an
// optional fd digit prefix followed by "<" or ">", or "&>". Any such token is
// routed to leadingRedirectOK, which accepts only the harmless forms.
func startsRedirect(tok string) bool {
	j := 0
	for j < len(tok) && tok[j] >= '0' && tok[j] <= '9' {
		j++
	}
	if j == 0 && strings.HasPrefix(tok, "&>") {
		return true
	}
	return j < len(tok) && (tok[j] == '>' || tok[j] == '<')
}

// gitArgRunsOrWrites reports whether one argument to a read-only git
// subcommand can execute a program or write a file. It covers grep's -O
// (also attached, "-O./p.sh"), and the long options that name an output file
// or a program: --output, --open-files(-in-pager), --upload-pack,
// --receive-pack, --exec, --ext-diff, --textconv, --exec-path, --config-env,
// --paginate and --pager. Abbreviated long options are matched by prefix
// (min 3 chars), so "--outp=" and "--open-" are caught too.
func gitArgRunsOrWrites(a string) bool {
	if a == "-O" || (strings.HasPrefix(a, "-O") && !strings.HasPrefix(a, "--")) {
		return true
	}
	if !strings.HasPrefix(a, "-") {
		return false
	}
	name := strings.TrimLeft(a, "-")
	// Only the boolean switches that turn a program off are exempt:
	// "--ext-diff=false", "--textconv=false", "--no-ext-diff", "--no-textconv",
	// "--no-pager". Any other "=false" is a file or program name, not a switch.
	switch a {
	case "--ext-diff=false", "--textconv=false", "--no-ext-diff", "--no-textconv", "--no-pager":
		return false
	}
	if j := strings.Index(name, "="); j >= 0 {
		name = name[:j]
	}
	if len(name) < 3 {
		return false
	}
	for _, long := range []string{"output", "open-files", "upload-pack", "receive-pack",
		"exec", "ext-diff", "textconv", "no-textconv", "config-env", "exec-path",
		"paginate", "no-pager", "pager"} {
		// Abbreviation ("--outp") or the full name followed by more text
		// ("--open-files-in-pager"): either way the option names the hook.
		if strings.HasPrefix(long, name) || strings.HasPrefix(name, long) {
			return true
		}
	}
	return false
}

// gitConfigReadOnly reports whether "git config" arguments are a pure read:
// exactly one of --get, --get-all, --list or -l, plus at most the options that
// only select a file for reading. Any "--", any other positional word, or any
// unknown option gates the command.
func gitConfigReadOnly(args []string) bool {
	read := false
	positional := 0
	for _, a := range args {
		switch {
		case a == "--get", a == "--get-all", a == "--list", a == "-l":
			read = true
		case a == "--global", a == "--system", a == "--local", a == "--show-origin",
			a == "--show-scope", a == "--includes", a == "--no-includes", a == "-z", a == "--null":
		case strings.HasPrefix(a, "-"):
			return false // "--" and any other option
		default:
			positional++ // the key to read: a second word would be a value (a write)
		}
	}
	// "--get KEY" and "--list" are reads; "KEY VALUE" is a write, so at most one
	// positional name, and only with a get form.
	if positional > 1 {
		return false
	}
	if positional == 1 && hasListFlag(args) {
		return false
	}
	return read
}

// hasListFlag reports whether a "git config" argument list selects --list or -l,
// which takes no key. Used to reject "--list KEY".
func hasListFlag(args []string) bool {
	for _, a := range args {
		if a == "--list" || a == "-l" {
			return true
		}
	}
	return false
}

// gitGlobalOptsOK reports whether the tokens before a git subcommand contain
// only "-C <dir>" (the subcommand is then judged by its own rules). Any other
// global option ("-c", "--git-dir", "--exec-path", ...) changes what runs.
func gitGlobalOptsOK(args []string) (sub int, ok bool) {
	for k := 0; k < len(args); k++ {
		a := args[k]
		if a == "-C" {
			if k+1 >= len(args) {
				return 0, false
			}
			k++
			continue
		}
		if strings.HasPrefix(a, "-") {
			return 0, false
		}
		return k, true
	}
	return len(args), true
}

// leadingRedirectOK reports whether a token that starts a segment is a
// redirection that is harmless: fd duplication or a redirect to /dev/null.
// consumed is how many following tokens the redirect used (1 for "> /dev/null",
// 0 when the target is glued on). Any other redirect fails closed.
func leadingRedirectOK(fields []string, i int) (consumed int, ok bool) {
	tok := fields[i]
	// Operator: optional digit prefix or "&", then ">" or ">>".
	j := 0
	for j < len(tok) && tok[j] >= '0' && tok[j] <= '9' {
		j++
	}
	if j == 0 && strings.HasPrefix(tok, "&>") {
		j = 1
	}
	if j >= len(tok) || tok[j] != '>' {
		return 0, false // "<" and anything else is not a harmless leading redirect
	}
	j++
	if j < len(tok) && tok[j] == '>' {
		j++
	}
	// A "&" right after the operator with the target glued on is fd dup.
	rest := tok[j:]
	if rest == "" {
		if i+1 >= len(fields) {
			return 0, false
		}
		rest = fields[i+1]
		consumed = 1
	}
	if strings.HasPrefix(rest, "&") {
		if isFDDup(rest) {
			return consumed, true
		}
		return 0, false
	}
	if rest == "/dev/null" {
		return consumed, true
	}
	return 0, false
}

// isFDDup reports whether right (text after ">") is an fd duplication:
// "&N" or "&-" with digits, followed by end, whitespace or a separator.
// "&1out.txt" and "&2x" are file writes and return false.
func isFDDup(right string) bool {
	if len(right) < 2 || right[0] != '&' {
		return false
	}
	j := 1
	if right[j] == '-' {
		j++
	} else {
		for j < len(right) && right[j] >= '0' && right[j] <= '9' {
			j++
		}
		if j == 1 {
			return false
		}
	}
	if j == len(right) {
		return true
	}
	switch right[j] {
	case ' ', '\t', '|', ';', '&', '\n', ')':
		return true
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
		// a file named "1". ">>&1" is not fd duplication (bash rejects it),
		// so an append operator never reaches the fd-dup branch.
		if j > i+1 && len(right) > 0 && right[0] == '&' {
			return true // ">>&N" is not fd duplication
		}
		if len(right) > 0 && right[0] == '&' {
			// &> followed by /dev/null (exact token) is a discard.
			if isDevNull(right) {
				continue
			}
			// >&N, >&-, N>&M: fd duplication only with digits or '-' and a
			// token boundary after them. ">&1out.txt" is a file write.
			if isFDDup(right) {
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
// underscore is a variable reference (IsReadOnlyShell further restricts it to
// varSafeCmds); $(, ${, backtick, quote characters, braces and a backslash are
// dangerous. A backslash is rejected because the shell strips it before the
// binary sees the argument ("--\output=x" reaches git as "--output=x"), which
// would defeat every flag check in readFlagsOK.
func hasExpansionOrEscape(tok string) bool {
	for i := 0; i < len(tok); i++ {
		switch tok[i] {
		case '$':
			// $ followed by ( or { is command/parameter substitution — dangerous.
			if i+1 < len(tok) && (tok[i+1] == '(' || tok[i+1] == '{') {
				return true
			}
			// $ followed by a letter or _ is a variable reference.
			// $ at end of string or followed by non-alnum is ambiguous — conservative.
			if i+1 >= len(tok) {
				return true
			}
			c := tok[i+1]
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_') {
				return true
			}
		case '`', '\'', '"', '{', '}', '\\':
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

// dangerousLoopVar reports whether a loop variable name can change what the
// loop body runs or writes. Names the shell or the allowlisted tools read
// (PATH, IFS, the LD_/DYLD_ loader vars, GIT_*, LESS*, *PAGER*, editors and
// config-path vars) are gated. This is a deny list, not a proof: a name that
// is not listed is still judged by the loop body's own classification.
func dangerousLoopVar(name string) bool {
	if dangerousEnvPrefix(name) {
		return true
	}
	switch name {
	case "PATH", "IFS", "HOME", "CDPATH", "ENV", "BASH_ENV", "SHELLOPTS", "BASHOPTS",
		"PS4", "PROMPT_COMMAND", "HISTFILE", "TMPDIR", "XDG_CONFIG_HOME", "EDITOR",
		"VISUAL", "SHELL", "MANPAGER", "RIPGREP_CONFIG_PATH", "ACKRC", "BAT_CONFIG_PATH",
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_OBJECT_DIRECTORY":
		return true
	}
	return false
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
	// Config-path and home variables change which config an allowlisted
	// reader loads (git core.pager/fsmonitor, rg --pre, ack --pager).
	switch name {
	case "HOME", "XDG_CONFIG_HOME", "RIPGREP_CONFIG_PATH", "ACKRC", "BAT_CONFIG_PATH",
		"PERL5OPT", "PERL5LIB", "BASH_ENV", "ENV", "IFS", "CDPATH", "SHELLOPTS", "BASHOPTS":
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
//
// $(...) is read-only when every inner command is read-only AND the outer
// command (with each substitution replaced by a variable-shaped placeholder)
// is read-only too: "rm x $(cat f)" and "echo $(cat f) > out" stay gated.
// A complete one-line "for/while/until ...; do ...; done" is read-only when
// its header and body are. Anything that does not parse is NOT read-only.
// A variable or substitution argument is accepted only for varSafeCmds.
func IsReadOnlyShell(cmd string) bool {
	c := strings.TrimSpace(cmd)
	if c == "" {
		return false
	}
	if kind := loopKind(c); kind != "" {
		header, body, ok := splitLoop(c, kind)
		if !ok {
			return false
		}
		hc, ok := loopHeaderCommand(kind, header)
		if !ok {
			return false
		}
		return IsReadOnlyShell(hc) && IsReadOnlyShell(body)
	}
	if strings.Contains(c, "`") {
		// Backtick substitution — always non-read-only (opaque inner).
		return false
	}
	// Process substitution <(...) creates a temp fifo — never read-only.
	if strings.Contains(c, "<(") {
		return false
	}
	c2, inners, ok := substituteDollarParen(c)
	if !ok {
		return false
	}
	for _, inner := range inners {
		if !IsReadOnlyShell(inner) {
			return false
		}
	}
	if hasDestructiveRedirect(c) {
		return false
	}
	segs := splitShellSegments(c2)
	if len(segs) == 0 {
		return false
	}
	for _, seg := range segs {
		fields := strings.Fields(seg)
		i, bad := skipLeadingPrefixes(fields)
		if bad {
			return false
		}
		if i >= len(fields) {
			// A pure assignment is read-only only when its value is a
			// substitution whose inner command was already vetted above
			// ("result=$(grep x f)"); a bare "VAR=1" stays gated.
			if len(fields) > 0 && strings.Contains(seg, "$__SUBST") {
				continue
			}
			return false
		}
		// A quoted or escaped command word hides its name ("r\"\"m"), and
		// IsDestructiveShell gates it; the read-only path must agree.
		if strings.ContainsAny(fields[i], "'\"\\") {
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
			// A variable (or substituted) argument can expand to a write flag
			// for binaries that have one; only varSafeCmds accept it.
			if strings.Contains(a, "$") && !varSafeCmds[bin] {
				return false
			}
		}
		if !readFlagsOK(bin, args, rawArgs) {
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

// sedArgsDestructive reports whether a sed invocation (the arguments after the
// binary, each already unquoted) can write, execute or read through a
// script: -i / --in-place, -f / --file, any unknown option, or a script
// (positional, -e SCRIPT, -eSCRIPT, --expression[=]SCRIPT) that
// sedScriptIsDestructive rejects. Every script is checked, including the one
// that follows a separate "-e" argument. Backslashes are stripped first, as
// the shell would, so "-\i" is "-i".
//
// rawArgs are the same words before unquoting. A script that holds a "[" is
// accepted only as one whole word that is single-quoted with no backslash:
// unquoted (or double-quoted, or glued to an option as -eSCRIPT or
// --expression=SCRIPT) the shell can expand it as a glob or substitute into
// it, so what sed sees is not what was written. The same holds for file
// arguments (an unquoted "[-]i" is a glob that can become the -i option).
func sedArgsDestructive(args, rawArgs []string) bool {
	if len(args) != len(rawArgs) {
		return true
	}
	// Any word with a "[" must be one plain single-quoted word. This covers
	// file arguments too: an unquoted "[-]i" is a glob the shell can expand to
	// an existing file named "-i", which sed would then read as an option.
	for _, r := range rawArgs {
		if strings.Contains(r, "[") && !isPlainSingleQuoted(r) {
			return true
		}
	}
	// scriptBad judges one script. raw is the original word when the script
	// is a whole word (positional or the value of a separate -e), else "".
	scriptBad := func(script, raw string) bool {
		if strings.Contains(script, "[") && !isPlainSingleQuoted(raw) {
			return true
		}
		return sedScriptIsDestructive(script)
	}
	strip := func(s string) string { return strings.ReplaceAll(s, "\\", "") }
	hasScript := false
	endOpts := false
	for k := 0; k < len(args); k++ {
		a := strip(args[k])
		// "--" ends the options, so a following "-i" is read as a file or a
		// script, not a flag. The option-free forms are not an intended
		// relaxation: gate the whole "--" form (stricter).
		if a == "--" {
			return true
		}
		switch {
		case endOpts || a == "-" || !strings.HasPrefix(a, "-"):
			// First non-option word is the script unless -e/--expression gave one.
			if !hasScript {
				if scriptBad(a, rawArgs[k]) {
					return true
				}
				hasScript = true
			}
		case a == "--":
			endOpts = true
		case strings.HasPrefix(a, "--"):
			name, val, hasVal := strings.Cut(a, "=")
			switch name {
			case "--quiet", "--silent", "--regexp-extended", "--separate",
				"--null-data", "--zero-terminated", "--unbuffered", "--posix",
				"--debug", "--binary", "--follow-symlinks":
			case "--line-length":
				if !hasVal {
					k++ // its value is the next word
				}
			case "--expression":
				raw := ""
				if !hasVal {
					k++
					if k >= len(args) {
						return true
					}
					val = strip(args[k])
					raw = rawArgs[k]
				}
				if scriptBad(val, raw) {
					return true
				}
				hasScript = true
			default:
				return true // --in-place, --file, anything unknown
			}
		default:
			// Short option cluster, e.g. -n, -nE, -e'5p', -n -e.
			cluster := a[1:]
			for ci := 0; ci < len(cluster); ci++ {
				switch cluster[ci] {
				case 'n', 'E', 'r', 's', 'u', 'z', 'b':
				case 'e':
					script := cluster[ci+1:]
					raw := ""
					if script == "" {
						k++
						if k >= len(args) {
							return true
						}
						script = strip(args[k])
						raw = rawArgs[k]
					}
					if scriptBad(script, raw) {
						return true
					}
					hasScript = true
					ci = len(cluster) // the rest of the cluster was the script
				default:
					return true // i (in-place), f (script file), l, unknown
				}
			}
		}
	}
	return false
}

// sedScriptIsDestructive reports whether a sed script contains ANY command
// other than print/address forms. It walks every command in the script (they
// are separated by ';', newline, or grouped with { }), so "1p;w out" is
// caught even though its first command is a print. Allowed: p P = n N l q Q
// F h H g G x z b t T, grouping braces, comments, and s/RE/REPL/FLAGS with
// flags limited to g p i I m M and digits (a w or e flag writes or executes).
// Everything else — w W r R e a i c d D y and anything unrecognised — is
// destructive (gated). Unparseable input is destructive.
func sedScriptIsDestructive(script string) bool {
	// Every "[" must be accounted for: either a plain bracket expression the
	// parser placed in an address or in the pattern of s///, or a literal in
	// a replacement. A "[" anywhere else (a label, an unparsed command, a
	// shape the bracket rules refuse) leaves the counts unequal: destructive.
	seen := 0
	if sedParseDestructive(script, &seen) {
		return true
	}
	return seen != strings.Count(script, "[")
}

// isPlainSingleQuoted reports whether w is one single-quoted word: it starts
// and ends with ' and holds no other ' and no backslash.
func isPlainSingleQuoted(w string) bool {
	return len(w) >= 2 && w[0] == '\'' && w[len(w)-1] == '\'' &&
		!strings.ContainsAny(w[1:len(w)-1], "'\\")
}

// sedParseDestructive is the parser behind sedScriptIsDestructive; seen counts
// the "[" it consumed.
func sedParseDestructive(script string, seen *int) bool {
	s := script
	n := len(s)
	i := 0
	isSpace := func(ch byte) bool { return ch == ' ' || ch == '\t' }
	isEnd := func(ch byte) bool {
		return isSpace(ch) || ch == ';' || ch == '\n' || ch == '}' || ch == '#'
	}
	skipSpace := func() {
		for i < n && isSpace(s[i]) {
			i++
		}
	}
	skipDigits := func() {
		for i < n && s[i] >= '0' && s[i] <= '9' {
			i++
		}
	}
	// skipRegex moves past the closing delim (i is just after the opening
	// one); false when the delimiter is never closed. It also steps over plain
	// bracket expressions as one unit (counting them in seen). A plain
	// one starts "[" or "[^", has no "]" first, no "[", backslash, newline or
	// delimiter in its body, and ends at the next "]"; sed implementations
	// disagree about everything else (a leading "]", "[:alpha:]", a delimiter
	// inside), so anything else is refused (false: malformed).
	skipRegex := func(delim byte) bool {
		for i < n && s[i] != delim {
			switch s[i] {
			case '\\':
				i += 2
				continue
			case '[':
				if delim == '[' || delim == ']' {
					return false
				}
				j := i + 1
				if j < n && s[j] == '^' {
					j++
				}
				if j < n && s[j] == ']' {
					return false
				}
				k := j
				for k < n && s[k] != ']' {
					if c := s[k]; c == '[' || c == '\\' || c == delim || c == '\n' {
						return false
					}
					k++
				}
				if k >= n {
					return false
				}
				*seen++
				i = k + 1
				continue
			}
			i++
		}
		if i >= n {
			return false
		}
		i++
		return true
	}
	// skipReplacement is skipRegex for the replacement of s///: a "[" is
	// a literal there in every sed; it is counted so the total stays exact.
	skipReplacement := func(delim byte) bool {
		for i < n && s[i] != delim {
			switch s[i] {
			case '\\':
				i++
			case '[':
				*seen++
			}
			i++
		}
		if i >= n {
			return false
		}
		i++
		return true
	}
	skipRegexFlags := func() {
		for i < n && (s[i] == 'I' || s[i] == 'M') {
			i++
		}
	}
	// skipAddr consumes one address if present; false when malformed.
	skipAddr := func() bool {
		skipSpace()
		if i >= n {
			return true
		}
		switch {
		case s[i] == '$':
			i++
		case s[i] >= '0' && s[i] <= '9':
			skipDigits()
			if i < n && s[i] == '~' {
				i++
				skipDigits()
			}
		case s[i] == '/':
			i++
			if !skipRegex('/') {
				return false
			}
			skipRegexFlags()
		case s[i] == '\\':
			i++
			if i >= n {
				return false
			}
			d := s[i]
			i++
			if !skipRegex(d) {
				return false
			}
			skipRegexFlags()
		}
		return true
	}
	for {
		for i < n && (isSpace(s[i]) || s[i] == ';' || s[i] == '\n' || s[i] == '{' || s[i] == '}') {
			i++
		}
		if i >= n {
			return false
		}
		if s[i] == '#' {
			for i < n && s[i] != '\n' {
				i++
			}
			continue
		}
		if !skipAddr() {
			return true
		}
		skipSpace()
		if i < n && s[i] == ',' {
			i++
			skipSpace()
			if i < n && (s[i] == '+' || s[i] == '~') {
				i++
				skipDigits()
			} else if !skipAddr() {
				return true
			}
		}
		skipSpace()
		for i < n && s[i] == '!' {
			i++
			skipSpace()
		}
		if i >= n {
			return false // address only: no command, nothing to run
		}
		c := s[i]
		i++
		switch c {
		case '{':
			continue
		case 'p', 'P', '=', 'n', 'N', 'F', 'h', 'H', 'g', 'G', 'x', 'z':
		case 'l', 'q', 'Q':
			skipSpace()
			skipDigits()
		case 'b', 't', 'T':
			for i < n && s[i] != ';' && s[i] != '\n' && s[i] != '}' {
				i++ // the label
			}
		case 's':
			if i >= n || isSpace(s[i]) || s[i] == '\n' || s[i] == '\\' {
				return true
			}
			delim := s[i]
			i++
			// Pattern, then replacement: each call consumes through its
			// closing delimiter.
			if !skipRegex(delim) {
				return true
			}
			if !skipReplacement(delim) {
				return true
			}
			for i < n && !isEnd(s[i]) {
				switch ch := s[i]; {
				case ch == 'g' || ch == 'p' || ch == 'i' || ch == 'I' || ch == 'm' || ch == 'M':
				case ch >= '0' && ch <= '9':
				default:
					return true // w (write), e (execute), anything unknown
				}
				i++
			}
		default:
			return true
		}
		// A command ends at a separator; anything glued on is unrecognised.
		if i < n && !isEnd(s[i]) {
			return true
		}
	}
}

// readFlagsOK rejects flags that turn an otherwise-read command into a writer.
// It is the second gate for readOnlyCmds: even when a binary is allowlisted,
// flags like --output, -i (in-place), -O (pager), or -delete can write files.
func readFlagsOK(bin string, args, rawArgs []string) bool {
	switch bin {
	case "git":
		sub, ok := gitGlobalOptsOK(args)
		if !ok || sub >= len(args) {
			return false
		}
		args = args[sub:]
		switch args[0] {
		case "diff", "status", "log", "show", "blame", "shortlog",
			"ls-files", "ls-tree", "rev-parse", "describe", "name-rev",
			"diff-tree", "cat-file", "ls-remote", "for-each-ref",
			"rev-list", "range-diff", "merge-base", "cherry":
			for _, a := range args[1:] {
				if gitArgRunsOrWrites(a) {
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
					if gitArgRunsOrWrites(a) {
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
				if gitArgRunsOrWrites(a) {
					return false
				}
			}
			return true
		case "stash":
			if len(args) < 2 || args[1] != "list" {
				return false
			}
			for _, a := range args[2:] {
				if gitArgRunsOrWrites(a) {
					return false
				}
			}
			return true
		case "config":
			// Only the listing and get forms, with no "--" and no other
			// positional argument: "git config k v" and "git config -- k --list"
			// (which writes "k = --list") are writes.
			return gitConfigReadOnly(args[1:])
		}
		return false
	case "xxd":
		// A lone "-" is a stdin operand, not an option; with a second
		// positional word, xxd writes the output file.
		pos := 0
		for _, a := range args {
			if a == "-" || !strings.HasPrefix(a, "-") {
				pos++
			}
		}
		if pos >= 2 {
			return false
		}
	case "less", "more", "tree":
		// Any short option that is not a known read-only switch gates the
		// command: -o/-O/-a/-k/-P... can write a file, run a program or read a
		// key file, and clusters ("-ao out") hide the letter after the first.
		for _, a := range args {
			if strings.HasPrefix(a, "--") {
				lower := strings.ToLower(a)
				if strings.HasPrefix(lower, "--log-file") || strings.HasPrefix(lower, "--output") {
					return false
				}
				continue
			}
			if strings.HasPrefix(a, "-") && a != "-" {
				return false
			}
			if strings.HasPrefix(a, "+") {
				return false // less +CMD runs a command
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
			if strings.HasPrefix(strings.ToLower(a), "--pag") || strings.HasPrefix(a, "+") ||
				strings.HasPrefix(strings.ToLower(a), "--ackrc") || strings.HasPrefix(a, "--ag") {
				return false
			}
		}
	case "bat", "file":
		// bat runs --pager and can write a config file; file -C compiles a
		// magic database to disk. Only the plain read forms are kept.
		for _, a := range args {
			if strings.HasPrefix(a, "-") {
				lower := strings.ToLower(strings.TrimLeft(a, "-"))
				if strings.HasPrefix(lower, "pag") || strings.HasPrefix(lower, "generate") ||
					strings.HasPrefix(lower, "config") || a == "-C" || strings.HasPrefix(a, "-C") {
					return false
				}
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
			// Attached and clustered forms ("-xrm", "--exec=rm", "-Hx", "-HIx",
			// "-fprint=out"): fd and find take these as one argument.
			if strings.HasPrefix(aa, "--exec") || strings.HasPrefix(aa, "--ok") ||
				strings.HasPrefix(aa, "-fprint") || strings.HasPrefix(aa, "-fls") {
				return false
			}
			if strings.HasPrefix(aa, "-") && !strings.HasPrefix(aa, "--") && strings.ContainsAny(aa, "xX") {
				return false // short cluster with an exec letter
			}
		}
	case "yq":
		for _, a := range args {
			if strings.HasPrefix(a, "--in") || strings.HasPrefix(a, "--split-exp") {
				return false // --inplace and --in-place, with or without a value
			}
			// -i in a cluster ("-Pi", "-ie") or -s/-i attached: any short
			// option letter i or s makes yq write files.
			if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.ContainsAny(a, "is") {
				return false
			}
		}
	case "sed":
		// sed is read-only only when no option or script element can write,
		// execute or read a script file (-i, -f, w/e/r commands, ...); see
		// sedArgsDestructive.
		if sedArgsDestructive(args, rawArgs) {
			return false
		}
	case "date":
		// -s/--set changes the system clock, also attached ("-s2020", "-us x")
		// and with "=". Any positional date is refused, because "date 2030..."
		// is a set form on some implementations.
		for _, a := range args {
			if a == "--set" || strings.HasPrefix(a, "--set=") || strings.HasPrefix(a, "--s") {
				return false
			}
			if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "s") {
				return false
			}
			// A "+FORMAT" word only formats output; any other positional
			// word is a date to set on some implementations.
			if !strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "+") {
				return false
			}
		}
	case "hostname":
		// Any positional word sets the name; -F/--file reads it from a file.
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				return false
			}
			if strings.HasPrefix(a, "--file") || strings.HasPrefix(a, "-F") ||
				(strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && strings.Contains(a, "F")) {
				return false
			}
		}
	case "printf":
		// "printf -v NAME ..." assigns a shell variable, which can be PATH.
		// A variable or substitution argument can expand to "-v", so the
		// literal check is not enough: any "$" in printf's arguments gates.
		for _, a := range args {
			if a == "--" {
				break
			}
			if a == "-v" || strings.HasPrefix(a, "-v") || strings.Contains(a, "$") {
				return false
			}
		}
	}
	return true
}
