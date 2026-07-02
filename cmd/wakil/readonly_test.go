package main

import (
	"testing"

	agent "wakil/internal/agent"
)

func TestIsReadOnlyShell(t *testing.T) {
	readOnly := []string{
		"cat foo.go",
		"ls -la",
		"grep -i needle file.txt",
		"grep needle file | head -n 20",
		"tail -n 50 app.log",
		"cd src && ls",
		"find . -name '*.go'",
		"pwd",
		"wc -l *.go",
		"/usr/bin/cat /etc/hostname",
		"yq '.key' file.yaml", // yq read (no -i)
		"date",                // date read
		"date +%Y-%m-%d",      // date with format
		"hostname",            // hostname read (no arg)
		"hostname -f",         // hostname flag-only
		"fd . --type f",       // fd read
	}
	for _, c := range readOnly {
		if !agent.IsReadOnlyShell(c) {
			t.Errorf("expected read-only: %q", c)
		}
	}

	writes := []string{
		"rm -rf foo",
		"cat a > b",
		"echo hi >> file",
		"find . -delete",
		"find . -exec rm {} ;",
		"sort -o out.txt in.txt", // sort not allowlisted (positional output)
		"cat $(echo file)",       // command substitution
		"ls `whoami`",            // backtick substitution
		"ls; rm x",               // chained write
		"cat a | tee b",          // tee writes
		"python script.py",       // unknown binary
		"sed -i s/a/b/ f",        // sed not allowlisted
		"",                       // empty
		// new security-boundary cases
		"yq -i '.foo = 1' file.yaml", // yq in-place edit
		"date -s 2000-01-01",         // set system clock
		"date --set=2000-01-01",      // set system clock (long form)
		"hostname newname",           // set hostname
		"fd . -X rm",                 // fd exec-batch
		"fd . --exec-batch rm",       // fd exec-batch (long form)
		"cat <(grep needle file)",    // process substitution
		"diff <(cat a) <(cat b)",     // process substitution
	}
	for _, c := range writes {
		if agent.IsReadOnlyShell(c) {
			t.Errorf("expected NOT read-only: %q", c)
		}
	}
}

// TestIsDestructiveShell covers the destructive-gate classification.
// P0-1: env-var prefix must not bypass the gate.
// P0-2: redirection and command substitution must trigger the gate.
func TestIsDestructiveShell(t *testing.T) {
	mustGate := []struct {
		cmd  string
		desc string
	}{
		// P0-1: env-var prefix bypass — the original bug.
		{"X=1 rm -rf /", "env-var prefix hides rm"},
		{"FOO=bar mv a b", "env-var prefix hides mv"},
		{"A=1 B=2 chmod -R 777 .", "multiple env-var prefixes hide chmod -R"},
		// P0-2: redirection → destructive.
		{"echo x > /etc/hosts", "redirection to file"},
		{"echo x >> /etc/hosts", "append redirection"},
		{"echo x 2> /dev/null", "fd redirection"},
		{"echo x &> /dev/null", "combined redirection"},
		// P0-2: command substitution → destructive.
		{"cat $(rm -rf /tmp/x)", "command substitution"},
		{"cat `whoami`", "backtick substitution"},
		{"cat <(grep needle file)", "process substitution"},
		// Existing chaining cases (must still work).
		{"echo ok && rm -rf /tmp/x", "rm after &&"},
		{"ls | rm -rf", "rm in pipeline"},
		{"ls; rm foo", "rm after ;"},
		// Normal destructive commands.
		{"rm -rf /tmp/test", "rm"},
		{"git reset --hard HEAD", "git reset"},
		{"git checkout -- .", "git checkout --"},
		{"git clean -fd", "git clean"},
		{"sudo apt-get install x", "sudo"},
		{"find . -delete", "find -delete"},
		{"tee /etc/config", "tee"},
		{"bash -c 'echo hi'", "bash wrapper"},
	}
	for _, tc := range mustGate {
		if !agent.IsDestructiveShell(tc.cmd) {
			t.Errorf("should gate (%s): %q", tc.desc, tc.cmd)
		}
	}

	shouldNotGate := []struct {
		cmd  string
		desc string
	}{
		{"ls -la", "ls"},
		{"cat file.txt", "cat"},
		{"grep -r 'foo' .", "grep"},
		{"git status", "git status"},
		{"go build ./...", "go build"},
		{`echo "rm is a command"`, "rm inside quoted string"},
		{"find . -name '*.log'", "find without -delete"},
		{"chmod 644 file.txt", "chmod without -R"},
		// Env-var prefix before a safe command should NOT gate.
		{"X=1 ls -la", "env-var prefix + safe command"},
		{"FOO=bar echo hi", "env-var prefix + echo"},
	}
	for _, tc := range shouldNotGate {
		if agent.IsDestructiveShell(tc.cmd) {
			t.Errorf("should NOT gate (%s): %q", tc.desc, tc.cmd)
		}
	}
}
