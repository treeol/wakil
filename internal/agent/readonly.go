package agent

// readonly.go: shell-command classification for the auto-mode gate.

import "strings"

// readOnlyCmds is a conservative allowlist of shell binaries that only read.
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
var destructiveCmds = map[string]bool{
	"rm": true, "rmdir": true, "shred": true,
	"mv":   true,
	"dd":   true,
	"sudo": true,
	"kill": true, "pkill": true, "killall": true, "sigkill": true,
	"mkfs": true, "fdisk": true, "parted": true,
	"truncate": true,
	"sh":       true, "bash": true, "zsh": true,
	"xargs": true,
	"tee":   true,
}

// isDestructiveShell reports whether cmd contains an operation that must always
// require user confirmation, even when AutoApprove is enabled.
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
// &>/dev/null) — those only discard or copy existing output and do not
// create or truncate a file.
func hasDestructiveRedirect(cmd string) bool {
	for i := 0; i < len(cmd); i++ {
		if cmd[i] != '>' {
			continue
		}
		left := byte(0)
		if i > 0 {
			left = cmd[i-1]
		}
		right := ""
		j := i + 1
		if j < len(cmd) && cmd[j] == '>' {
			j++
		}
		for j < len(cmd) && (cmd[j] == ' ' || cmd[j] == '\t') {
			j++
		}
		if j < len(cmd) {
			right = cmd[j:]
		}

		// fd duplication: N>&M, N>&-, &>M, >&M (any > followed by &,
		// digit, or - is fd duplication, not a file write). Also N> with
		// N a digit followed by &/digit/-.
		if len(right) > 0 && right[0] == '&' {
			// &> followed by /dev/null is a discard, not a write.
			if strings.HasPrefix(right, "&>/dev/null") {
				continue
			}
			// &> followed by &, digit, or - is fd duplication.
			if len(right) > 1 && (right[1] == '&' || right[1] == '-' ||
				(right[1] >= '0' && right[1] <= '9')) {
				continue
			}
			// &> followed by a file path is a write — destructive.
			return true
		}
		// &> /dev/null (space between &> and /dev/null).
		if left == '&' && strings.HasPrefix(right, ">/dev/null") {
			continue
		}
		// >& /dev/null (space between >& and /dev/null): >& is fd dup.
		if left == '>' && i >= 2 && cmd[i-2] == '&' && strings.HasPrefix(right, "/dev/null") {
			continue
		}
		if left == '&' || (left >= '0' && left <= '9') {
			if len(right) > 0 && (right[0] == '&' || right[0] == '-' ||
				(right[0] >= '0' && right[0] <= '9')) {
				continue
			}
		}

		// Redirect to /dev/null: discard output, not a file write.
		if strings.HasPrefix(right, "/dev/null") {
			continue
		}

		// Any other '>' or '>>' is a redirect to a file — destructive.
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
// commands.
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
