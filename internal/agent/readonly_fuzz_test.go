package agent

import (
	"strings"
	"testing"
)

// readonly_fuzz_test.go — Go native fuzz tests for IsReadOnlyShell and
// IsDestructiveShell. CI runs the seed corpus (go test); longer fuzzing
// is manual (go test -fuzz=FuzzShellClassify -fuzztime=30s).
//
// Invariants:
//   - panic-freedom on arbitrary byte strings
//   - mutual exclusion: a command cannot be both read-only AND destructive
//     (redirection/substitution rejects both; destructive commands are
//     excluded from read-only allowlist)
//   - whitespace idempotence: trimming doesn't change the result

// FuzzShellClassify feeds random byte strings to both shell classifiers
// and asserts panic-freedom + mutual exclusion.
func FuzzShellClassify(f *testing.F) {
	// Seed corpus — structured to hit each classifier's branches.
	seeds := []string{
		"",                       // empty
		"   ",                    // whitespace-only
		"ls",                     // read-only
		"cat file",               // read-only
		"/bin/ls -la",            // read-only with path
		"rm -rf /",               // destructive
		"echo hi > file",         // destructive (redirection)
		"echo `rm x`",            // destructive (backtick)
		"echo $(rm x)",           // destructive (command substitution)
		"ls && rm -rf x",         // destructive (chained)
		"cat a | grep b",         // read-only (pipe)
		"X=1 rm -rf /",           // destructive (env prefix)
		"VAR=val ls",             // read-only (env prefix)
		"git reset --hard",       // destructive (git subcommand)
		"git push --force",       // destructive (git flag)
		"find . -delete",         // destructive (find flag)
		"find . -exec rm {} \\;", // destructive (find exec)
		"sed -i 's/a/b/' file",   // destructive (sed in-place)
		"chmod -R 755 dir",       // destructive (chmod recursive)
		"echo 'rm is a command'", // read-only (quoted, first token echo)
		"sudo ls",                // not read-only (sudo not in allowlist)
		"\x00\xff\xfe",           // invalid UTF-8
		"strings.Repeat",         // not a command
		"a&&b||c|d;e&f\ng",       // operators + newline
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, cmd string) {
		// Panic-freedom: both must return without panicking.
		ro := IsReadOnlyShell(cmd)
		de := IsDestructiveShell(cmd)

		// Mutual exclusion: a command cannot be both read-only AND destructive.
		// read-only means allowlisted + no destructive flags; destructive means
		// a destructive command/flag was found. If both are true, the
		// classifiers are inconsistent — a bug.
		if ro && de {
			t.Errorf("command is both read-only and destructive: %q", cmd)
		}

		// Whitespace idempotence: trimming leading/trailing whitespace
		// should not change either result (both functions TrimSpace internally).
		if cmd != "" && strings.TrimSpace(cmd) != cmd {
			ro2 := IsReadOnlyShell(strings.TrimSpace(cmd))
			de2 := IsDestructiveShell(strings.TrimSpace(cmd))
			if ro != ro2 {
				t.Errorf("IsReadOnlyShell not whitespace-idempotent: %q -> %q: %v vs %v", cmd, strings.TrimSpace(cmd), ro, ro2)
			}
			if de != de2 {
				t.Errorf("IsDestructiveShell not whitespace-idempotent: %q -> %q: %v vs %v", cmd, strings.TrimSpace(cmd), de, de2)
			}
		}
	})
}
