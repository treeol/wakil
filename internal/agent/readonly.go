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
var readOnlyCmds = map[string]bool{
	"cat": true, "bat": true, "tac": true, "nl": true, "head": true, "tail": true,
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true, "ack": true,
	"ls": true, "ll": true, "find": true, "fd": true, "tree": true, "pwd": true, "cd": true,
	"wc": true, "stat": true, "file": true, "du": true, "df": true, "cut": true, "comm": true,
	"echo": true, "printf": true, "which": true, "type": true,
	"whoami": true, "id": true, "hostname": true, "uname": true, "date": true,
	"printenv": true, "basename": true, "dirname": true,
	// NB: "command" is deliberately NOT allowlisted — POSIX command executes
	// its argument ("command rm -rf x" would be arbitrary execution).
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
	// Any redirection (>, >>, 2>, &>), backtick, or command/process
	// substitution can hide a write or arbitrary execution — gate it.
	if strings.ContainsAny(c, ">`") {
		return true
	}
	if strings.Contains(c, "$(") || strings.Contains(c, "<(") {
		return true
	}
	for _, seg := range splitShellSegments(c) {
		fields := strings.Fields(seg)
		if len(fields) == 0 {
			continue
		}
		// Skip leading "VAR=value" env assignments before the binary, mirroring
		// IsReadOnlyShell. Without this, `X=1 rm -rf /` is tokenized as bin
		// "X=1" and the rm payload is missed. Dangerous env prefixes (LD_*,
		// GIT_*, etc.) that can influence execution of the payload also make
		// the segment destructive — same check as IsReadOnlyShell for symmetry.
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
			// find is in readOnlyCmds for the read-only gate, but -delete/-exec
			// make it destructive and must gate even in auto mode.
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
				// -i or -i<suffix> (e.g. -i.bak) edits in place.
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

// unquoteShellArg strips symmetric surrounding single or double quotes from a
// token. The shell removes quotes before a binary sees its argv, so a quoted
// `"-delete"` arrives at find as -delete; flag matching must see the same
// tokens the shell will pass, or quoted destructive flags evade classification
// while still executing.
func unquoteShellArg(s string) string {
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// hasExpansionOrEscape reports whether a token contains shell constructs this
// first-token classifier cannot model: variable/parameter expansion ($VAR,
// ${x}, $'...'), backslash escapes, brace expansion ({a,b}), or INTERIOR
// quotes (adjacent fragments concatenate: -de""lete reaches find as -delete;
// the token keeps its quotes so the denylist check misses it). Outer symmetric
// quotes are already stripped by unquoteShellArg, so ordinary quoted arguments
// (find . -name '*.go') still classify. Any surviving construct gates the
// command (fail closed) — convenience trades against not admitting obfuscated
// flags past denylist-model commands.
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
// of allowlisted readers via helpers/pagers/dynamic linking: GIT_* (external
// diff, ssh command, pager), *PAGER*/*PAGER, LESS* (LESSOPEN pipes),
// LD_*/DYLD_* (library injection), PATH (binary resolution).
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

// isReadOnlyShell reports whether a shell command is safe to treat as read-only:
// every chained/piped segment starts with an allowlisted binary, none carry a
// known destructive flag, and the command has no output redirection or command
// substitution (which could hide a write).
func IsReadOnlyShell(cmd string) bool {
	c := strings.TrimSpace(cmd)
	if c == "" {
		return false
	}
	// Any redirection (>, >>, 2>, &>), backtick or process substitution disqualifies.
	if strings.ContainsAny(c, ">`") {
		return false
	}
	if strings.Contains(c, "$(") || strings.Contains(c, "<(") {
		return false
	}
	segs := splitShellSegments(c)
	if len(segs) == 0 {
		return false
	}
	for _, seg := range segs {
		fields := strings.Fields(seg)
		// Skip leading "VAR=value" env assignments before the binary, but only
		// ones that cannot influence execution (helpers, pagers, linking,
		// binary resolution). GIT_SSH_COMMAND=rm git ls-remote must not auto-run.
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
			bin = bin[j+1:] // strip any path prefix
		}
		if !readOnlyCmds[bin] {
			return false
		}
		rawArgs := fields[i+1:]
		args := make([]string, len(rawArgs))
		for k, a := range rawArgs {
			args[k] = unquoteShellArg(a)
		}
		// Tokens still carrying expansion/escape/brace/interior-quote constructs
		// after outer-quote stripping are opaque to this classifier — gate it.
		// (Checked AFTER unquoting: fully-quoted args like '*.go' are safe;
		// fragments like -de""lete keep interior quotes and gate.)
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
// commands (| || && ; & newline) so each segment can be validated independently.
func splitShellSegments(c string) []string {
	r := strings.NewReplacer("&&", "\x00", "||", "\x00", "|", "\x00", "&", "\x00", ";", "\x00", "\n", "\x00")
	var out []string
	for _, p := range strings.Split(r.Replace(c), "\x00") {
		if s := strings.TrimSpace(p); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// readFlagsOK rejects flags that turn an otherwise-read command into a writer.
func readFlagsOK(bin string, args []string) bool {
	switch bin {
	case "git":
		// Only recognized read-only subcommands are auto-approved. Any
		// subcommand not in this list (e.g. "add", "commit", "push" without
		// --force, "stash" without list, "merge", "rebase") falls through
		// to return false → normal confirm prompt. Mutating subcommands that
		// are also destructive (reset, clean, checkout --, push --force, stash
		// drop/clear, branch -D) are caught earlier by IsDestructiveShell,
		// so they never reach readFlagsOK.
		if len(args) == 0 {
			return false // bare "git" is not a read — gate it
		}
		switch args[0] {
		case "diff", "status", "log", "show", "blame", "shortlog",
			"ls-files", "ls-tree", "rev-parse", "describe", "name-rev",
			"diff-tree", "cat-file", "ls-remote", "for-each-ref",
			"rev-list", "range-diff", "merge-base", "cherry":
			// "diff --output=FILE" writes a file; --ext-diff / --textconv run
			// configured helper binaries. Everything else stays read-only.
			for _, a := range args[1:] {
				if strings.HasPrefix(a, "--output") || a == "--ext-diff" ||
					a == "--no-textconv" || a == "--textconv" {
					return false
				}
			}
			return true
		case "reflog":
			// Only bare inspection is read-only. "reflog expire" rewrites/
			// truncates reflog entries; "reflog delete" removes them. Any
			// subcommand other than none/"show"/"list" gates; show/list inherit
			// log machinery, so --output gates too.
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
			// Strict allowlist of branch-READING forms; anything not matching
			// exactly gates (creation by positional, rename -m/-M, copy -c/-C,
			// delete -d/-D/--delete, force -f/--force, upstream -u/--set-*
			// all fall through to a confirm prompt). Unknown/combined flags
			// also gate — exact match only, no prefix or bundle acceptance.
			// NB: "-l" is NOT here — on git branch it means "create reflog".
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
			// git grep -O / --open-files-in-pager launches a pager binary.
			for _, a := range args[1:] {
				if a == "-O" || strings.HasPrefix(a, "--open-files-in-pager") {
					return false
				}
			}
			return true
		case "stash":
			// Only "stash list" is read-only (and it inherits the --output
			// write vector from log machinery — gate it too); push/pop/apply/
			// drop/clear are not reads.
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
			// Only "config --get" is read-only; "config --set" writes.
			for _, a := range args[1:] {
				if a == "--get" || a == "--get-all" || a == "--list" || a == "-l" {
					return true
				}
			}
			return false
		}
		return false // unrecognized subcommand → gate it
	case "xxd":
		// "xxd in out" writes a second positional output file; one positional
		// (stdout dump) is a read. Conservative: any non-flag token counts as
		// a positional, including numeric option operands (-l 16 → "16").
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
		// less/tree output-file flags (short and long forms).
		for _, a := range args {
			switch {
			case strings.HasPrefix(a, "-o"), strings.HasPrefix(a, "-O"),
				strings.HasPrefix(a, "--log-file"):
				return false
			}
		}
	case "rg":
		// rg --pre=CMD runs a preprocessor binary; --hostname-bin runs one too.
		for _, a := range args {
			if strings.HasPrefix(a, "--pre") || strings.HasPrefix(a, "--hostname-bin") {
				return false
			}
		}
	case "ag", "ack":
		// --pager CMD / +CMD launch a pager process.
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
				"-X", "--exec-batch": // fd: batch exec
				return false
			}
		}
	case "yq":
		// yq -i / --inplace edits in place; -s/--split-exp writes multiple files.
		for _, a := range args {
			if a == "-i" || a == "--inplace" || a == "-s" || strings.HasPrefix(a, "--split-exp") {
				return false
			}
		}
	case "date":
		// date -s / --set sets the system clock
		for _, a := range args {
			if a == "-s" || a == "--set" || strings.HasPrefix(a, "--set=") {
				return false
			}
		}
	case "hostname":
		// hostname NAME sets the hostname; flags-only is a read
		for _, a := range args {
			if !strings.HasPrefix(a, "-") {
				return false
			}
		}
	}
	return true
}
