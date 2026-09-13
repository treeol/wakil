package exec

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func newDirectExec(t *testing.T) (*DirectExecutor, string) {
	t.Helper()
	dir := t.TempDir()
	ex, err := NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ex.Close() })
	// Resolve through symlinks so comparisons against ConfinePath output (which
	// EvalSymlinks-canonicalises) hold on platforms where TempDir is symlinked
	// (e.g. macOS /var → /private/var).
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	return ex, root
}

func TestConfinePathInsideAndTraversal(t *testing.T) {
	ex, root := newDirectExec(t)
	ctx := context.Background()

	// A relative path resolves under the workspace root.
	got, err := ex.ConfinePath(ctx, "sub/file.txt")
	if err != nil {
		t.Fatalf("in-workspace path rejected: %v", err)
	}
	if want := filepath.Join(root, "sub/file.txt"); got != want {
		t.Errorf("ConfinePath = %q, want %q", got, want)
	}

	// Traversal above the root is rejected.
	if _, err := ex.ConfinePath(ctx, "../../etc/passwd"); err == nil {
		t.Error("expected traversal outside workspace to be rejected")
	}

	// An absolute path outside the workspace is rejected.
	if _, err := ex.ConfinePath(ctx, "/etc/passwd"); err == nil {
		t.Error("expected absolute outside path to be rejected")
	}
}

func TestConfinePathSymlinkEscape(t *testing.T) {
	ex, root := newDirectExec(t)
	// A symlink inside the workspace pointing outside must be caught after
	// symlink resolution.
	outside := t.TempDir()
	link := filepath.Join(root, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := ex.ConfinePath(context.Background(), "escape"); err == nil {
		t.Error("expected symlink-escape to be rejected")
	}
}

func TestDeletePathFileAndNonEmptyDir(t *testing.T) {
	ex, root := newDirectExec(t)
	ctx := context.Background()

	// Delete a file.
	f := filepath.Join(root, "f.txt")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ex.DeletePath(ctx, f); err != nil {
		t.Fatalf("deleting file: %v", err)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Error("file should be gone")
	}

	// A non-empty directory yields the actionable rm -r hint.
	d := filepath.Join(root, "d")
	if err := os.MkdirAll(filepath.Join(d, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := ex.DeletePath(ctx, d)
	if err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Errorf("non-empty dir delete should hint rm -r; got: %v", err)
	}
}

func TestMovePathRenameAndExists(t *testing.T) {
	ex, root := newDirectExec(t)
	ctx := context.Background()
	src := filepath.Join(root, "a.txt")
	dst := filepath.Join(root, "b.txt")
	if err := os.WriteFile(src, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := ex.MovePath(ctx, src, dst); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("dst should exist after move: %v", err)
	}

	// Moving onto an existing destination is refused (no silent overwrite).
	other := filepath.Join(root, "c.txt")
	if err := os.WriteFile(other, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := ex.MovePath(ctx, other, dst)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("move onto existing dst should be refused; got: %v", err)
	}
}

func TestBackgroundProcessLifecycle(t *testing.T) {
	ex, root := newDirectExec(t)
	ctx := context.Background()
	logPath := filepath.Join(root, "bg.log")

	pid, pgid, err := ex.StartBackground(ctx, "echo hello; sleep 30", logPath)
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	if pid <= 0 || pgid != pid {
		t.Fatalf("expected pid>0 and pgid==pid (setpgid); got pid=%d pgid=%d", pid, pgid)
	}
	if !ex.IsProcessAlive(ctx, pid) {
		t.Error("process should be alive right after start")
	}

	// The log should capture stdout once the process gets scheduled.
	deadline := time.Now().Add(2 * time.Second)
	var tail string
	for time.Now().Before(deadline) {
		tail, _ = ex.ReadFileTail(ctx, logPath, 1024)
		if strings.Contains(tail, "hello") {
			break
		}
	}
	if !strings.Contains(tail, "hello") {
		t.Errorf("log tail should contain process output; got %q", tail)
	}

	// Kill the whole group; the process should no longer be alive.
	if err := ex.KillPgid(ctx, pgid, 9); err != nil {
		t.Fatalf("KillPgid: %v", err)
	}
	gone := false
	for i := 0; i < 50; i++ {
		if !ex.IsProcessAlive(ctx, pid) {
			gone = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !gone {
		t.Error("process should be dead after SIGKILL to the group")
	}

	// KillPgid on an already-dead group is a no-op (not an error).
	if err := ex.KillPgid(ctx, pgid, 9); err != nil {
		t.Errorf("kill of dead group should be a no-op, got: %v", err)
	}
}

func TestReadFileTailCap(t *testing.T) {
	ex, root := newDirectExec(t)
	p := filepath.Join(root, "big.txt")
	if err := os.WriteFile(p, []byte("0123456789abcdef"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ex.ReadFileTail(context.Background(), p, 6)
	if err != nil {
		t.Fatal(err)
	}
	if got != "abcdef" {
		t.Errorf("tail = %q, want last 6 bytes %q", got, "abcdef")
	}
	// maxBytes larger than the file returns the whole file.
	whole, err := ex.ReadFileTail(context.Background(), p, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if whole != "0123456789abcdef" {
		t.Errorf("tail of small file = %q, want whole file", whole)
	}
}

func TestDirectExecutorStatFile(t *testing.T) {
	ex, root := newDirectExec(t)
	ctx := context.Background()

	// Write a file with known content and stat it.
	p := filepath.Join(root, "data.txt")
	if err := os.WriteFile(p, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	size, err := ex.StatFile(ctx, "data.txt")
	if err != nil {
		t.Fatalf("StatFile: %v", err)
	}
	if size != 11 {
		t.Errorf("StatFile size = %d, want 11", size)
	}

	// Missing file → error.
	if _, err := ex.StatFile(ctx, "nonexistent.txt"); err == nil {
		t.Error("StatFile on missing file should return error")
	}
}

func TestDirectExecutorRunShellContextCancel(t *testing.T) {
	ex, _ := newDirectExec(t)

	// Use a short timeout and a long sleep. 'exec sleep' replaces the shell
	// process so CommandContext kills the sleeper directly (no orphan shell).
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := ex.RunShell(ctx, "echo started; exec sleep 30")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from cancelled RunShell, got nil")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("RunShell did not return promptly after cancel; elapsed %s", elapsed)
	}
}

func TestDirectExecutorListDirAndMeta(t *testing.T) {
	ex, root := newDirectExec(t)
	ctx := context.Background()
	if _, err := ex.RunShell(ctx, "mkdir d && touch d/x f.txt"); err != nil {
		t.Fatal(err)
	}
	out, err := ex.ListDir(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "d/") || !strings.Contains(out, "f.txt") {
		t.Errorf("ListDir should mark dirs with / and list files; got %q", out)
	}
	if ex.WorkspaceRoot() != root {
		t.Errorf("WorkspaceRoot = %q, want %q", ex.WorkspaceRoot(), root)
	}
	if ex.Generation() != 1 {
		t.Errorf("initial generation = %d, want 1", ex.Generation())
	}
	if !strings.HasPrefix(ex.Describe(), "direct[") {
		t.Errorf("Describe = %q, want direct[...]", ex.Describe())
	}
}

// probeTools is exercised with an injected runner so the result is deterministic
// and independent of which tools exist on the test host.
func TestProbeToolsFormatting(t *testing.T) {
	run := func(string) string {
		return "git:git version 2.43.0\n" +
			"jq:jq-1.7\n" +
			"go:go version go1.25.0 linux/amd64\n"
	}
	got := probeTools(run)
	if !strings.HasPrefix(got, "Sandbox tools: ") {
		t.Fatalf("missing prefix: %q", got)
	}
	for _, want := range []string{"git 2.43.0", "jq 1.7", "go 1.25.0"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %q", want, got)
		}
	}
	// Tools not reported are listed as unavailable.
	if !strings.Contains(got, "unavailable:") || !strings.Contains(got, "curl") {
		t.Errorf("absent tools should be listed as unavailable; got %q", got)
	}

	// All-absent → empty string (no noise in the system prompt).
	if probeTools(func(string) string { return "" }) != "" {
		t.Error("empty probe output should yield empty string")
	}
}

func TestIsInsideWorkspace(t *testing.T) {
	cases := []struct {
		p, root string
		want    bool
	}{
		{"/work", "/work", true},
		{"/work/sub/x", "/work", true},
		{"/work2", "/work", false},
		{"/etc/passwd", "/work", false},
		{"/work/../etc", "/work", false},
	}
	for _, c := range cases {
		if got := isInsideWorkspace(c.p, c.root); got != c.want {
			t.Errorf("isInsideWorkspace(%q,%q) = %v, want %v", c.p, c.root, got, c.want)
		}
	}
}

func TestProbeToolsParsing(t *testing.T) {
	fakeOutput := strings.Join([]string{
		"git:git version 2.39.5",
		"curl:curl 7.88.1 (x86_64-pc-linux-gnu) libcurl/7.88.1",
		"jq:jq-1.6",
		"make:GNU Make 4.3",
		"gcc:gcc (Debian 12.2.0) 12.2.0",
		"python3:Python 3.11.2",
		"node:v20.20.2",
		"npm:10.8.2",
		"go:go version go1.26.0 linux/amd64",
		"rustc:rustc 1.85.0 (4d91de4e4 2025-02-17)",
		"docker:28.1.1",
		"docker-daemon:up",
	}, "\n")
	result := probeTools(func(_ string) string { return fakeOutput })
	if !strings.HasPrefix(result, "Sandbox tools:") {
		t.Fatalf("expected 'Sandbox tools:' prefix, got: %q", result)
	}
	for _, want := range []string{"git 2.39.5", "curl 7.88.1", "jq 1.6", "python3 3.11.2", "node 20.20.2", "go 1.26.0", "rustc 1.85.0", "docker 28.1.1"} {
		if !strings.Contains(result, want) {
			t.Errorf("probe result missing %q: %s", want, result)
		}
	}
	if strings.Contains(result, "unavailable") {
		t.Errorf("no tools should be absent here: %s", result)
	}
}

// When the docker CLI is present but the daemon is unreachable (socket not
// mounted or daemon down), docker must be reported as unavailable so the
// agent doesn't assume it can drive the host daemon.
func TestProbeToolsDockerDaemonDown(t *testing.T) {
	fakeOutput := "git:git version 2.39.5\ndocker:28.1.1\ndocker-daemon:down"
	result := probeTools(func(_ string) string { return fakeOutput })
	if !strings.Contains(result, "git 2.39.5") {
		t.Errorf("expected git present: %s", result)
	}
	if strings.Contains(result, "docker 28") {
		t.Errorf("docker should not be listed as present when daemon is down: %s", result)
	}
	if !strings.Contains(result, "unavailable: ") || !strings.Contains(result, "docker") {
		t.Errorf("docker should be listed as unavailable when daemon is down: %s", result)
	}
}

func TestProbeToolsAbsent(t *testing.T) {
	fakeOutput := "git:git version 2.39.5\npython3:Python 3.11.2"
	result := probeTools(func(_ string) string { return fakeOutput })
	if !strings.Contains(result, "Sandbox tools:") {
		t.Fatalf("unexpected: %q", result)
	}
	if !strings.Contains(result, "unavailable:") {
		t.Errorf("should list unavailable tools: %s", result)
	}
	for _, absent := range []string{"curl", "jq", "make", "gcc", "node", "npm"} {
		if !strings.Contains(result, absent) {
			t.Errorf("absent tool %q not listed in unavailable: %s", absent, result)
		}
	}
}

func TestProbeToolsFailure(t *testing.T) {
	result := probeTools(func(_ string) string { return "" })
	if result != "" {
		t.Errorf("probe failure should return empty string, got: %q", result)
	}
}

func TestIsInsideWorkspaceSeparatorAware(t *testing.T) {
	cases := []struct {
		p, root string
		want    bool
		label   string
	}{
		{"/workspace", "/workspace", true, "root itself"},
		{"/workspace/foo", "/workspace", true, "direct child"},
		{"/workspace/foo/bar", "/workspace", true, "nested"},
		{"/workspace-evil", "/workspace", false, "evil sibling — must be rejected"},
		{"/workspace-evil/foo", "/workspace", false, "child of evil sibling"},
		{"/other", "/workspace", false, "unrelated"},
		{"/workspac", "/workspace", false, "prefix truncated"},
	}
	for _, tc := range cases {
		got := isInsideWorkspace(tc.p, tc.root)
		if got != tc.want {
			t.Errorf("isInsideWorkspace(%q, %q) = %v, want %v  [%s]",
				tc.p, tc.root, got, tc.want, tc.label)
		}
	}
}

func TestDirectExecutorReadFileNotFoundReturnsSentinel(t *testing.T) {
	ex, _ := newDirectExec(t)
	ctx := context.Background()

	_, err := ex.ReadFile(ctx, "nonexistent.txt")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
	if !errors.Is(err, ErrFileNotFound) {
		t.Errorf("ReadFile nonexistent: errors.Is(err, ErrFileNotFound) = false; err = %v", err)
	}
}

func TestDirectExecutorStatFileNotFoundReturnsSentinel(t *testing.T) {
	ex, _ := newDirectExec(t)
	ctx := context.Background()

	_, err := ex.StatFile(ctx, "nonexistent.txt")
	if err == nil {
		t.Fatal("expected error for nonexistent file")
	}
	if !errors.Is(err, ErrFileNotFound) {
		t.Errorf("StatFile nonexistent: errors.Is(err, ErrFileNotFound) = false; err = %v", err)
	}
}

func TestDirectExecutorDeletePathNotFoundReturnsSentinel(t *testing.T) {
	ex, root := newDirectExec(t)
	ctx := context.Background()

	err := ex.DeletePath(ctx, filepath.Join(root, "nonexistent.txt"))
	if err == nil {
		t.Fatal("expected error for deleting nonexistent file")
	}
	if !errors.Is(err, ErrFileNotFound) {
		t.Errorf("DeletePath nonexistent: errors.Is(err, ErrFileNotFound) = false; err = %v", err)
	}
}

func TestIsShellNotFound(t *testing.T) {
	cases := []struct {
		msg  string
		want bool
	}{
		{"cat: /work/foo.txt: No such file or directory", true},
		{"rm: cannot remove '/work/foo.txt': No such file or directory", true},
		{"stat: cannot stat '/work/foo.txt': No such file or directory", true},
		{"No such file or directory", true},
		{"no such file or directory", true},
		{"", false},
		{"Error response from daemon: container does not exist", false},
		{"permission denied", false},
		{"directory is not empty", false},
	}
	for _, tc := range cases {
		got := isShellNotFound(tc.msg)
		if got != tc.want {
			t.Errorf("isShellNotFound(%q) = %v, want %v", tc.msg, got, tc.want)
		}
	}
}

// TestProcessAliveFromErr covers the kill(2) error classification used by
// DirectExecutor.IsProcessAlive (card #239): EPERM means the process exists but
// is owned by another user → alive; ESRCH/nil-adjacent errors → dead.
func TestProcessAliveFromErr(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil means alive", nil, true},
		{"EPERM means alive (other user)", syscall.EPERM, true},
		{"wrapped EPERM means alive", fmt.Errorf("kill: %w", syscall.EPERM), true},
		{"ESRCH means dead", syscall.ESRCH, false},
		{"wrapped ESRCH means dead", fmt.Errorf("kill: %w", syscall.ESRCH), false},
		{"EINVAL (bad signal) means dead", syscall.EINVAL, false},
		{"unrelated error means dead", errors.New("boom"), false},
	}
	for _, tc := range cases {
		if got := processAliveFromErr(tc.err); got != tc.want {
			t.Errorf("%s: processAliveFromErr(%v) = %v, want %v", tc.name, tc.err, got, tc.want)
		}
	}
}

// TestDirectExecutorIsProcessAliveRealProcess is the wiring smoke test: a real
// child is reported alive, and a reaped PID is not (so the classifier cannot be
// wired backwards without failing).
func TestDirectExecutorIsProcessAliveRealProcess(t *testing.T) {
	ex, root := newDirectExec(t)
	ctx := context.Background()
	logPath := filepath.Join(root, "alive.log")

	pid, pgid, err := ex.StartBackground(ctx, "sleep 30", logPath)
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	if !ex.IsProcessAlive(ctx, pid) {
		t.Error("live child should be reported alive")
	}
	if err := ex.KillPgid(ctx, pgid, 9); err != nil {
		t.Fatalf("KillPgid: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for ex.IsProcessAlive(ctx, pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if ex.IsProcessAlive(ctx, pid) {
		t.Error("reaped child should not be reported alive")
	}
}

// TestDockerIsProcessAliveScript verifies the exact shell script used by
// DockerExecutor.IsProcessAlive (card #245). Runs the script on the host
// (it is valid POSIX sh with /proc) against real PIDs: a live child and a
// reaped PID. The unreadable-stat path is tested deterministically against a
// temp directory (no root needed).
func TestDockerIsProcessAliveScript(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("/proc not available")
	}

	runScript := func(script string) (string, error) {
		cmd := exec.Command("sh", "-c", script)
		out, err := cmd.Output()
		return string(out), err
	}

	t.Run("live self", func(t *testing.T) {
		state, err := runScript(procPIDAliveScript(os.Getpid()))
		if err != nil {
			t.Fatalf("script failed: %v", err)
		}
		state = strings.TrimSpace(state)
		if state == "" || state == "?" {
			t.Errorf("state = %q, want a non-empty state char", state)
		}
		if strings.HasPrefix(state, "Z") {
			t.Errorf("state = Z (zombie), want non-zombie")
		}
	})

	t.Run("dead PID", func(t *testing.T) {
		ex, root := newDirectExec(t)
		ctx := context.Background()
		logPath := filepath.Join(root, "dead.log")
		pid, pgid, err := ex.StartBackground(ctx, "sleep 30", logPath)
		if err != nil {
			t.Fatalf("StartBackground: %v", err)
		}
		if err := ex.KillPgid(ctx, pgid, 9); err != nil {
			t.Fatalf("KillPgid: %v", err)
		}
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := runScript(procPIDAliveScript(pid)); err != nil {
				return // script exits non-zero → process gone
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Error("dead PID: script still succeeds after 3s — process not reaped?")
	})

	// Deterministic unreadable-stat test (no root needed). Builds a temp
	// directory tree that mirrors /proc/<pid>/ with a directory present but
	// no readable stat file, then runs the script logic against it. This
	// tests the fallback mechanics without relying on procfs permissions.
	t.Run("unreadable stat (temp-dir fallback)", func(t *testing.T) {
		dir := t.TempDir()
		// Create a fake "proc" root: <dir>/<pid>/ exists but has no stat file.
		fakeProcPID := filepath.Join(dir, "12345")
		if err := os.MkdirAll(fakeProcPID, 0o755); err != nil {
			t.Fatal(err)
		}
		// Shell-quote the paths in case t.TempDir() returns paths with spaces.
		quotedPID := shQuote(fakeProcPID)
		// Build a script that uses the temp dir instead of /proc, but
		// applies the same fallback logic as procPIDAliveScript.
		script := fmt.Sprintf(`s=$(cat %s/stat 2>/dev/null) || { [ -d %s ] && printf '?' && exit 0; exit 1; }; rest=${s##*) }; set -f; set -- $rest; set +f; [ "$#" -ge 1 ] || exit 1; printf '%%s' "$1"`, quotedPID, quotedPID)
		out, err := runScript(script)
		if err != nil {
			t.Fatalf("script failed, want output '?': %v", err)
		}
		if strings.TrimSpace(out) != "?" {
			t.Errorf("state = %q, want '?' (dir exists, stat missing)", strings.TrimSpace(out))
		}

		// Also test the "directory gone" case → dead (exit non-zero).
		gonePID := filepath.Join(dir, "99999")
		quotedGone := shQuote(gonePID)
		scriptDead := fmt.Sprintf(`s=$(cat %s/stat 2>/dev/null) || { [ -d %s ] && printf '?' && exit 0; exit 1; }; rest=${s##*) }; set -f; set -- $rest; set +f; [ "$#" -ge 1 ] || exit 1; printf '%%s' "$1"`, quotedGone, quotedGone)
		_, err = runScript(scriptDead)
		if err == nil {
			t.Error("missing dir: script succeeded, want failure (dead)")
		}
	})
}
