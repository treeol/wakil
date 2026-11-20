package main

import (
	"testing"

	agent "github.com/treeol/wakil/internal/agent"
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
		// fd duplication and /dev/null redirects: not destructive, and
		// echo/cat with these are read-only (no file write).
		"echo x 2>&1",        // fd dup — not a file write
		"echo x >&2",         // fd dup
		"echo x 1>&2",        // fd dup
		"echo x 2>&-",        // fd close
		"echo x 2>/dev/null", // /dev/null redirect
		"echo x &>/dev/null", // combined /dev/null redirect
		"cat a 2>&1",         // cat with fd dup
		// sed print/address forms (read-only): no -i, only p/P/=/n/l/q/b/N/# or s/// (no w)
		"sed -n '5p' file",    // sed print line 5
		"sed -n '1,10p' file", // sed print range
		"sed '5p' file",       // sed print (no -n)
		"sed '1,10p' file",    // sed print range (no -n)
		"sed 's/a/b/' file",   // sed substitute (no write flag)
		// for/while loops (read-only when every command in header and body is read-only)
		"for f in *.go; do echo $f; done",  // for + echo
		"for f in *.go; do cat $f; done",   // for + cat
		"for x in 1 2 3; do echo $x; done", // for + echo
		"while true; do echo hi; done",     // while + echo (infinite but read-only)
		// $(...) (read-only when inner command is read-only)
		"echo $(cat file)",           // $(cat)
		"echo $(ls -la)",             // $(ls)
		"result=$(grep needle file)", // $(grep)
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
		// H1 audit 2026-09-21
		"command rm -rf foo",      // command executes its argument
		"git branch newbranch",    // branch creation
		"git branch -m a b",       // branch rename
		"git reflog expire --all", // reflog mutation
		"find . \"-delete\"",      // quoted flag (shell strips quotes)
		"git diff --output=p.txt", // diff writes a file
		// redirect classification: file writes that look like fd-dup
		"2>1",        // file write to file named "1"
		"2>-",        // file write to file named "-"
		"2>123.txt",  // file write
		"echo x 2>1", // 2>1 (no &) is a file write
		// redirect classification: /dev/null boundary — not /dev/null
		"echo x >/dev/nullfoo",   // /dev/null prefix is not /dev/null
		"echo x 2>/dev/null.bak", // /dev/null.bak is not /dev/null
		// redirect classification: quoted ">" in arguments
		`echo "a > b"`, // > inside quoted string (not a redirect)
		// redirect classification: mixed file redirect + fd dup
		"echo x > /tmp/out 2>&1", // file redirect + fd dup — destructive
		// redirect classification: &>file (combined redirect to a file)
		"echo x &> /tmp/out", // &> with a space then a file — destructive
		"echo x &>/tmp/out",  // &>file (no space) — destructive
		// sed classification: -i is destructive (already caught by IsDestructiveShell)
		"sed -i s/a/b/ f", // sed in-place edit — destructive
		// sed classification: w/e/r/a/i/c/d/D commands are destructive
		"sed 'w out.txt' file",   // sed write to file — destructive
		"sed 'r other.txt' file", // sed read file — destructive (inserts content)
		"sed 'a text' file",      // sed append — destructive
		"sed 'd' file",           // sed delete — destructive
		"sed 's/a/b/w' file",     // sed substitute + write flag — destructive
		// for/while classification: loop is destructive when any command is destructive
		"for f in *; do rm $f; done",                 // for + rm — destructive
		"for f in *.txt; do sed -i s/a/b/ $f; done",  // for + sed -i — destructive
		"while read line; do echo $line > out; done", // while + redirect — destructive
		// $(...) classification: destructive when inner command is destructive
		"echo $(rm -rf /tmp/x)",               // $(rm) — destructive
		"cat $(find . -name '*.tmp' -delete)", // $(find -delete) — destructive
		"result=$(sed -i s/a/b/ file)",        // $(sed -i) — destructive
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
		// P0-2: redirection → destructive (writes to a file).
		{"echo x > /etc/hosts", "redirection to file"},
		{"echo x >> /etc/hosts", "append redirection"},
		// P0-2: command substitution → destructive.
		{"cat $(rm -rf /tmp/x)", "command substitution"},
		{"cat `whoami`", "backtick substitution"},
		{"cat <(grep needle file)", "process substitution"},
		// Mixed: file redirect + fd dup — the file write makes it destructive.
		{"echo x > /tmp/out 2>&1", "file redirect + fd dup"},
		// fd-dup false positives: "2>1" (no &) is a file write.
		{"echo x 2>1", "2>1 without & is a file write"},
		{"2>1", "bare 2>1 is a file write"},
		// /dev/null boundary: not /dev/null.
		{"echo x >/dev/nullfoo", "/dev/null prefix is not /dev/null"},
		{"echo x 2>/dev/null.bak", "/dev/null.bak is not /dev/null"},
		// &>file (combined redirect to a file, not /dev/null).
		{"echo x &> /tmp/out", "&> file (with space) — destructive"},
		{"echo x &>/tmp/out", "&>file (no space) — destructive"},
		// sed -i is destructive (in-place edit).
		{"sed -i s/a/b/ f", "sed in-place edit"},
		{"cat `whoami`", "backtick substitution"},
		// sed w/e/r/a/i/c/d/D commands are destructive.
		{"sed 'w out.txt' file", "sed write to file"},
		{"sed 'r other.txt' file", "sed read file (inserts content)"},
		{"sed 'a text' file", "sed append"},
		{"sed 'd' file", "sed delete"},
		{"sed 's/a/b/w' file", "sed substitute + write flag"},
		{"sed -i s/a/b/ f", "sed in-place edit"},
		// for/while loop is destructive when any command is destructive.
		{"for f in *; do rm $f; done", "for + rm body"},
		{"for f in *.txt; do sed -i s/a/b/ $f; done", "for + sed -i body"},
		{"while read line; do echo $line > out; done", "while + redirect body"},
		// $(...) is destructive when the inner command is.
		{"echo $(rm -rf /tmp/x)", "$(rm) inner"},
		{"cat $(find . -name '*.tmp' -delete)", "$(find -delete) inner"},
		{"result=$(sed -i s/a/b/ file)", "$(sed -i) inner"},
		// Quoted ">" inside arguments — conservative: classified as
		// destructive because hasDestructiveRedirect does not track quotes.
		{`echo "a > b"`, "quoted > inside arguments (conservative)"},
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
		{"git branch -a", "branch listing is a read"},
		{"go build ./...", "go build"},
		{`echo "rm is a command"`, "rm inside quoted string"},
		{"find . -name '*.log'", "find without -delete"},
		{"chmod 644 file.txt", "chmod without -R"},
		// Env-var prefix before a safe command should NOT gate.
		{"X=1 ls -la", "env-var prefix + safe command"},
		{"FOO=bar echo hi", "env-var prefix + echo"},
		// fd duplication and /dev/null redirects: only copy or discard
		// output, never write a file — not destructive.
		{"echo x 2>&1", "fd duplication (2>&1)"},
		{"echo x >&2", "fd duplication (>&2)"},
		{"echo x 1>&2", "fd duplication (1>&2)"},
		{"echo x 2>&-", "fd close (2>&-)"},
		{"echo x 2>/dev/null", "redirect stderr to /dev/null"},
		{"echo x &>/dev/null", "combined redirect to /dev/null"},
		{"cat a 2>&1", "cat with fd dup"},
		{"go test ./... 2>&1", "go test with fd dup"},
		{"go vet ./... 2>/dev/null", "go vet with /dev/null redirect"},
		// sed print/address forms are read-only (not destructive).
		{"sed -n '5p' file", "sed print line (read-only)"},
		{"sed -n '1,10p' file", "sed print range (read-only)"},
		{"sed '5p' file", "sed print (no -n, read-only)"},
		{"sed 's/a/b/' file", "sed substitute (no write flag, read-only)"},
		// for/while loops are read-only when every command is read-only.
		{"for f in *.go; do echo $f; done", "for + echo (read-only)"},
		{"for f in *.go; do cat $f; done", "for + cat (read-only)"},
		{"for x in 1 2 3; do echo $x; done", "for + echo (read-only)"},
		{"while true; do echo hi; done", "while + echo (read-only)"},
		// $(...) is read-only when the inner command is read-only.
		{"echo $(cat file)", "$(cat) read-only"},
		{"echo $(ls -la)", "$(ls) read-only"},
		{"result=$(grep needle file)", "$(grep) read-only"},
	}
	for _, tc := range shouldNotGate {
		if agent.IsDestructiveShell(tc.cmd) {
			t.Errorf("should NOT gate (%s): %q", tc.desc, tc.cmd)
		}
	}
}

// TestShellClassifierBypasses pins the commands a review of the loop, $(...)
// and sed handling found slipping through. Each row is one case and states the
// verdict of BOTH classifiers: destructive means IsDestructiveShell is true and
// IsReadOnlyShell is false; readOnly means the reverse. A row never leaves the
// two classifiers contradicting each other.
func TestShellClassifierBypasses(t *testing.T) {
	destructive := []struct{ cmd, why string }{
		// $(...) must not clear the outer command.
		{"rm -rf /tmp/x $(cat file)", "destructive outer command with a read-only substitution"},
		{"echo $(cat file) > /tmp/pwn", "redirect next to a substitution"},
		{"echo $(cat file); rm -rf /tmp/x", "second segment after a substitution"},
		{"rm -rf / && echo $(pwd)", "destructive first segment"},
		{"$(echo rm) -rf /", "command word is a substitution"},
		{"rm -rf /; echo $(", "unbalanced $( fails closed"},
		{"echo $(", "unbalanced $( alone fails closed"},
		{"$X -rf /", "command word is a variable"},
		{"sed -n $(echo w) f", "substitution argument to sed"},
		// Loops: header, tail, redirect, and parse failure.
		{"for x in $(rm -rf /tmp/x); do echo $x; done", "destructive substitution in the for list"},
		{"for x in `rm -rf /`; do echo $x; done", "backticks in the for list"},
		{"while rm -rf /tmp/x; do echo x; done", "destructive while header"},
		{"for x in 1; do echo $x; done; rm -rf /tmp/x", "command after done"},
		{"for x in 1; do echo $x; done > /tmp/pwn", "redirect after done"},
		{"for x in a; do echo $x > /tmp/pwn; done", "redirect in the body"},
		{"for f in *;do rm -rf $f;done", "no space before do"},
		{"for f in *\ndo\nrm $f\ndone", "newline-separated loop"},
		{"while\trm -rf /; do echo x; done", "tab after while"},
		{"for ((i=0;i<3;i++)); do rm x; done", "arithmetic for is not parsed"},
		{"case x in a) echo hi;; esac; rm -rf /", "case with a trailing command"},
		{"case $(rm -rf /) in *) echo ok;; esac", "case subject substitution"},
		{"case x in\n*) rm -rf /;; esac", "newline after case ... in"},
		{"if true; then rm -rf /tmp/x; fi", "if is not parsed"},
		{"{ rm -rf /tmp/x; }", "brace group"},
		{"(rm -rf /tmp/x)", "subshell group"},
		// sed: every script, every option.
		{"sed -e 'w/tmp/pwn' file", "-e with a separate script argument"},
		{"sed -e e file", "-e execute command"},
		{"sed --in-place s/a/b/ file", "long in-place option"},
		{"sed -ni s/a/b/ file", "-i inside a cluster"},
		{"sed -f pwn.sed file", "script from a file"},
		{"sed --file=pwn.sed file", "long script-file option"},
		{"sed 's/.*/rm/e' file", "s///e executes"},
		{"sed -n '1p;w /tmp/pwn' file", "w after a print in one script"},
		{"sed -n '1p;1w/tmp/pwn' file", "addressed w after a print"},
		{"sed -n -e 1p -e 'w out' file", "second -e script"},
		{"sed 'y/a/b/;w out' file", "w after y"},
		{"sed -n '/x/{p;w out\n}' file", "w inside a brace group"},
		{"sed -i s/a/b/ file", "in-place"},
		{"sed 'w out' file", "plain w"},
		{"sed '1d' file", "delete"},
		{"sed -n '1p' file > out", "redirect on a print"},
		// Backslash-hidden flag that IsDestructiveShell does gate.
		{"sed -\\i s/a/b/ f", "backslash inside sed -i"},
	}
	for _, tc := range destructive {
		if !agent.IsDestructiveShell(tc.cmd) {
			t.Errorf("IsDestructiveShell(%q) = false, want true (%s)", tc.cmd, tc.why)
		}
		if agent.IsReadOnlyShell(tc.cmd) {
			t.Errorf("IsReadOnlyShell(%q) = true, want false (%s)", tc.cmd, tc.why)
		}
	}

	readOnly := []struct{ cmd, why string }{
		{"echo $(cat file)", "read-only substitution"},
		{"echo $(cat file) $(ls)", "two read-only substitutions"},
		{"result=$(grep needle file)", "assignment from a read-only substitution"},
		{"echo $HOME", "variable argument to echo"},
		{"grep \"$pat\" f", "variable argument to grep"},
		{"cat $(ls)", "substitution argument to cat"},
		{"for f in *.go; do echo $f; done", "for with echo"},
		{"for f in *.go; do cat $f; done", "for with cat"},
		{"for f in $(ls); do cat $f; done", "read-only substitution in the list"},
		{"while true; do ls; done", "while with a read-only header and body"},
		{"for f in a b; do echo $f && cat $f; done", "body with &&"},
		{"sed -n '5p' file", "print one line"},
		{"sed -n '1,10p' file", "print a range"},
		{"sed -n -e '5p' file", "-e with a print"},
		{"sed -ne '5p' file", "-ne cluster"},
		{"sed -n '/foo/p' file", "regex address print"},
		{"sed 's/a/b/g' file", "substitute to stdout"},
		{"sed -n 's/a/b/gp' file", "substitute and print"},
	}
	for _, tc := range readOnly {
		if !agent.IsReadOnlyShell(tc.cmd) {
			t.Errorf("IsReadOnlyShell(%q) = false, want true (%s)", tc.cmd, tc.why)
		}
		if agent.IsDestructiveShell(tc.cmd) {
			t.Errorf("IsDestructiveShell(%q) = true, want false (%s)", tc.cmd, tc.why)
		}
	}

	// Not auto-approvable as reads, but not "always gate even in /auto"
	// either: a variable or a backslash can hide a write flag, which the
	// read-only allowlist must refuse. Only IsReadOnlyShell is asserted;
	// IsDestructiveShell cannot know what a variable holds.
	notReadOnly := []struct{ cmd, why string }{
		{"find . $F", "variable argument to a binary with a write flag"},
		{"find . $(echo -delete)", "substitution argument to find"},
		{"git diff $(echo --output=x)", "substitution argument to git"},
		{"git diff --\\output=evil.txt", "backslash inside a git flag"},
		{"git diff --\\ext-diff", "backslash inside a git flag"},
		{"yq -\\i '.a=1' f.yaml", "backslash inside a yq flag"},
		{"date -\\s '2000-01-01'", "backslash inside date -s"},
		{"less -\\o out.txt in.txt", "backslash inside a less flag"},
		{"for f in -delete; do find . $f; done", "variable carries a find flag"},
		{"for x in --output=pwn; do git diff $x; done", "variable carries a git flag"},
	}
	for _, tc := range notReadOnly {
		if agent.IsReadOnlyShell(tc.cmd) {
			t.Errorf("IsReadOnlyShell(%q) = true, want false (%s)", tc.cmd, tc.why)
		}
	}
}
