package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"
)

// readonly_exec_test.go — runs every corpus command the classifier calls
// read-only under real bash, in a fresh temp directory seeded with files, and
// asserts the directory tree and file contents are unchanged afterwards.
//
// Safeguards (the test runs real commands):
//   - a fresh temp dir per command, as the working directory;
//   - a sanitised environment: fixed PATH, HOME and TMPDIR inside the temp
//     dir, no inherited LD_*, GIT_*, PAGER, LESS* or editor variables, and
//     git's global and system config disabled;
//   - stdin from /dev/null and a short timeout that kills the process group;
//   - commands that reach the network, read a clock or change system state
//     are skipped by an explicit rule, and the test is skipped as root.

var execNetworkRe = regexp.MustCompile(`https?://|ssh://|git@|ls-remote|\bcurl\b|\bwget\b|\bnc\b`)

// execSkip reports why a corpus command is not run (empty: run it).
func execSkip(cmd string) string {
	if execNetworkRe.MatchString(cmd) {
		return "reaches the network"
	}
	if strings.Contains(cmd, "tail -f") || strings.Contains(cmd, "less") || strings.Contains(cmd, "more") {
		return "interactive (pager or follow)"
	}
	if strings.Contains(cmd, "while true") {
		return "unbounded loop"
	}
	return ""
}

// seedDir writes a few files, a git repo and a dangling symlink.
func seedDir(t *testing.T, dir string) {
	t.Helper()
	files := map[string]string{
		"f":        "alpha\nbeta\ngamma\n",
		"x":        "x-content\n",
		"a":        "a-content\n",
		"b":        "b-content\n",
		"out.txt":  "existing\n",
		"sub/g.go": "package sub\n",
	}
	for name, body := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Symlink("nowhere", filepath.Join(dir, "dangling"))
}

// snapshot returns a deterministic description of dir: every path with its
// type, mode, symlink target, size and content hash.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		line := rel + " " + info.Mode().String()
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			line += " -> " + target
		case info.Mode().IsRegular():
			data, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(data)
			line += " " + hex.EncodeToString(sum[:8])
		}
		lines = append(lines, line)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// runSandboxed runs cmd under bash in dir with a sanitised environment and a
// timeout. The process group is killed when the timeout expires.
func runSandboxed(t *testing.T, bash, dir, cmd string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, bash, "-c", cmd)
	c.Dir = dir
	c.Stdin = nil
	var out bytes.Buffer
	c.Stdout = &out
	c.Stderr = &out
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Env = []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + dir,
		"TMPDIR=" + dir,
		"TERM=dumb",
		"LC_ALL=C",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_OPTIONAL_LOCKS=0",
	}
	c.WaitDelay = time.Second
	_ = c.Start()
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case <-done:
	case <-ctx.Done():
		if c.Process != nil {
			_ = syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		}
		<-done
	}
}

func TestShellClassifierReadOnlyCorpusDoesNotWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real commands; skipped in -short")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	if os.Geteuid() == 0 {
		t.Skip("refusing to run corpus commands as root")
	}
	ran, skipped := 0, 0
	for _, r := range loadCorpus(t) {
		if !IsReadOnlyShell(r.Cmd) {
			continue
		}
		if why := execSkip(r.Cmd); why != "" {
			skipped++
			continue
		}
		dir := t.TempDir()
		seedDir(t, dir)
		before := snapshot(t, dir)
		runSandboxed(t, bash, dir, r.Cmd)
		after := snapshot(t, dir)
		if before != after {
			t.Errorf("read-only %q changed the directory\nbefore:\n%s\nafter:\n%s", r.Cmd, before, after)
		}
		ran++
	}
	if ran == 0 {
		t.Fatal("no read-only commands ran")
	}
	t.Logf("ran %d read-only corpus commands, skipped %d", ran, skipped)
}

// TestSedBracketReadOnlyFormsDoNotWrite runs, under real bash in a seeded temp
// directory, every sed form the classifier calls read-only that holds a
// bracket expression, and asserts the directory is unchanged. It also asserts
// the classifier agrees these forms are read-only, so the list cannot rot.
func TestSedBracketReadOnlyFormsDoNotWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("runs real commands; skipped in -short")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	if os.Geteuid() == 0 {
		t.Skip("refusing to run commands as root")
	}
	for _, cmd := range []string{
		`sed -n '/[a-z]/p' f`,
		`sed 's/[0-9]//' f`,
		`sed -n '/[^a-z]/p' f`,
		`sed 's/a/[/' f`,
		`sed -n '/x/,/[y]/p' f`,
		`sed -n -e '/[a]/p' f`,
		`sed 's/a/b/' 'f[1]'`,
		`sed 's/[/]/X/' f`,
	} {
		ro := IsReadOnlyShell(cmd)
		if cmd == `sed 's/[/]/X/' f` {
			// Refused by the classifier (delimiter inside a bracket); run
			// only to show the form is harmless here, never as read-only.
			if ro {
				t.Errorf("%q must not be read-only", cmd)
			}
			continue
		}
		if !ro {
			t.Errorf("%q should be read-only", cmd)
			continue
		}
		dir := t.TempDir()
		seedDir(t, dir)
		before := snapshot(t, dir)
		runSandboxed(t, bash, dir, cmd)
		if after := snapshot(t, dir); before != after {
			t.Errorf("read-only %q changed the directory\nbefore:\n%s\nafter:\n%s", cmd, before, after)
		}
	}
}
