// Package agent — readonly.go: shell-command classification for the auto-mode gate.
//
// isDestructiveShell is friction against accidental destruction in auto mode,
// NOT a security boundary. It is bypassable by construction: a model that wants
// to run `rm -rf /` can wrap it in a variable, a here-doc, or any other shell
// construct that defeats first-token analysis. The goal is to catch common
// accidental cases, not adversarial ones.
//
// Known gaps (out of scope for this pass):
//   - Output redirection (`>`, `>>`, `2>`) is not inspected; `echo x > /etc/passwd`
//     passes the destructive check. Redirection detection is deferred.
//   - Env-var wrappers (`VAR=$(rm foo)`) are not inspected.
package agent

import "strings"

// readOnlyCmds is a conservative allowlist of shell binaries that only read.
// Commands with easy write vectors via positional output files or common flags
// (sort -o, uniq out, tee, dd, sed -i, awk 'print>') are deliberately excluded —
// misclassifying a write as a read is the dangerous direction, so when in doubt
// the command falls through to a normal confirm prompt rather than auto-approve.
var readOnlyCmds = map[string]bool{
	"cat": true, "bat": true, "tac": true, "nl": true, "head": true, "tail": true,
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true, "ack": true,
	"ls": true, "ll": true, "find": true, "fd": true, "tree": true, "pwd": true, "cd": true,
	"wc": true, "stat": true, "file": true, "du": true, "df": true, "cut": true, "comm": true,
	"echo": true, "printf": true, "which": true, "type": true, "command": true,
	"whoami": true, "id": true, "hostname": true, "uname": true, "date": true,
	"printenv": true, "basename": true, "dirname": true,
	"readlink": true, "realpath": true, "diff": true, "cmp": true, "column": true,
	"od": true, "xxd": true, "hexdump": true, "strings": true, "ps": true,
	"less": true, "more": true, "seq": true, "true": true, "false": true,
	"test": true, "jq": true, "yq": true,
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
// splitShellSegments, then for each segment tokenize and match the FIRST token.
// This avoids false positives like `echo "rm is a command"` (first token: echo)
// while still catching `safe-cmd && rm -rf x` (second segment first token: rm).
func IsDestructiveShell(cmd string) bool {
	for _, seg := range splitShellSegments(strings.TrimSpace(cmd)) {
		fields := strings.Fields(seg)
		if len(fields) == 0 {
			continue
		}
		bin := fields[0]
		if j := strings.LastIndex(bin, "/"); j >= 0 {
			bin = bin[j+1:]
		}
		if destructiveCmds[bin] {
			return true
		}
		args := fields[1:]
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
		// Skip leading "VAR=value" env assignments before the binary.
		i := 0
		for i < len(fields) && !strings.HasPrefix(fields[i], "-") && strings.Contains(fields[i], "=") {
			i++
		}
		if i >= len(fields) {
			return false
		}
		bin := fields[i]
		if j := strings.LastIndex(bin, "/"); j >= 0 {
			bin = bin[j+1:] // strip any path prefix
		}
		if !readOnlyCmds[bin] {
			return false
		}
		if !readFlagsOK(bin, fields[i+1:]) {
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
		// yq -i / --inplace edits the file in place
		for _, a := range args {
			if a == "-i" || a == "--inplace" {
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
