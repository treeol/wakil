package exec

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
