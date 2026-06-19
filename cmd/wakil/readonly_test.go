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
		"yq '.key' file.yaml",        // yq read (no -i)
		"date",                        // date read
		"date +%Y-%m-%d",             // date with format
		"hostname",                    // hostname read (no arg)
		"hostname -f",                 // hostname flag-only
		"fd . --type f",               // fd read
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
		"sort -o out.txt in.txt",       // sort not allowlisted (positional output)
		"cat $(echo file)",             // command substitution
		"ls `whoami`",                  // backtick substitution
		"ls; rm x",                     // chained write
		"cat a | tee b",                // tee writes
		"python script.py",             // unknown binary
		"sed -i s/a/b/ f",             // sed not allowlisted
		"",                             // empty
		// new security-boundary cases
		"yq -i '.foo = 1' file.yaml",  // yq in-place edit
		"date -s 2000-01-01",          // set system clock
		"date --set=2000-01-01",       // set system clock (long form)
		"hostname newname",             // set hostname
		"fd . -X rm",                  // fd exec-batch
		"fd . --exec-batch rm",        // fd exec-batch (long form)
		"cat <(grep needle file)",     // process substitution
		"diff <(cat a) <(cat b)",      // process substitution
	}
	for _, c := range writes {
		if agent.IsReadOnlyShell(c) {
			t.Errorf("expected NOT read-only: %q", c)
		}
	}
}
