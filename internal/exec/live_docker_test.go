package exec

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// requireDocker skips the test if the Docker CLI or daemon is unavailable.
// Checks daemon reachability (not just CLI presence) — mirrors probeTools'
// docker-daemon:up/down distinction.
func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("docker CLI not available: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "info").CombinedOutput(); err != nil {
		t.Skipf("docker daemon not available: %v: %s", err, strings.TrimSpace(string(out)))
	}
}

// newDockerExec creates a DockerExecutor with the wakil-dev image and a temp
// host mount. Skips if Docker or the image is unavailable.
func newDockerExec(t *testing.T) (*DockerExecutor, string) {
	t.Helper()
	requireDocker(t)

	// Use a dir under the workspace for the host mount — Docker bind mounts
	// resolve on the Docker daemon's host filesystem, and /tmp inside this
	// sandbox is a separate tmpfs not visible to the daemon. /mnt/wakil IS
	// visible to both.
	hostMount, err := os.MkdirTemp("/mnt/wakil/.tmp", "wakil-docker-test-")
	if err != nil {
		requireDocker(t)
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(hostMount) })
	ex, err := NewDockerExecutor(DockerOpts{
		Image:       "wakil-dev:latest",
		Workdir:     "/work",
		HostMount:   hostMount,
		DockerCaps:  []string{"CHOWN"}, // needed to chown /work to the container user
	})
	if err != nil {
		// If the image is missing, skip rather than fail — the test
		// environment may not have the wakil-dev image built.
		if strings.Contains(err.Error(), "image") || strings.Contains(err.Error(), "pull") {
			t.Skipf("wakil-dev image not available: %v", err)
		}
		t.Fatalf("NewDockerExecutor: %v", err)
	}
	t.Cleanup(func() { _ = ex.Close() })

	// The mount point inside the container is created by Docker as root.
	// When running as a non-root user (--user uid:gid), the workspace is
	// not writable. Chown it to the container user via a root exec.
	// This mirrors the production ensurePasswdEntry/ensureCACerts pattern.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	uid := os.Getuid()
	if uid > 0 {
		chownOut, chownErr := exec.CommandContext(ctx, "docker", "exec", "-u", "0", ex.container,
			"sh", "-c", "chown -R "+strconv.Itoa(uid)+":"+strconv.Itoa(os.Getgid())+" /work").CombinedOutput()
		if chownErr != nil {
			t.Fatalf("chown /work failed: %v: %s", chownErr, strings.TrimSpace(string(chownOut)))
		}
	}
	return ex, hostMount
}

// TestDocker_WorkspaceMountVisibility proves files written inside the container
// via WriteFile can be read back via ReadFile, and that RunShell can read them.
// (Note: host-side visibility of the bind mount is not tested here because the
// sandbox runs inside Docker, and Docker-in-Docker bind mounts have propagation
// issues that prevent the host from seeing container-side writes.)
func TestDocker_WorkspaceMountVisibility(t *testing.T) {
	ex, _ := newDockerExec(t)
	ctx := context.Background()

	// Write via WriteFile, read back via ReadFile.
	if _, err := ex.WriteFile(ctx, "from-container.txt", "hello from container"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := ex.ReadFile(ctx, "from-container.txt")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.TrimSpace(got) != "hello from container" {
		t.Errorf("ReadFile = %q, want %q", got, "hello from container")
	}

	// Write via RunShell, read back via ReadFile.
	if _, err := ex.RunShell(ctx, "echo 'from shell' > from-shell.txt"); err != nil {
		t.Fatalf("RunShell write: %v", err)
	}
	got2, err := ex.ReadFile(ctx, "from-shell.txt")
	if err != nil {
		t.Fatalf("ReadFile after RunShell: %v", err)
	}
	if strings.TrimSpace(got2) != "from shell" {
		t.Errorf("ReadFile = %q, want %q", got2, "from shell")
	}
}

// TestDocker_CannotWriteOutsideWorkspace proves the container's root filesystem
// is read-only outside the workspace mount — a write attempt to a non-writable
// path fails.
func TestDocker_CannotWriteOutsideWorkspace(t *testing.T) {
	ex, _ := newDockerExec(t)
	ctx := context.Background()

	// /wakil-test-outside is on the read-only rootfs (not /tmp, not /work).
	// The write should fail; we verify the file does not exist afterward.
	out, _ := ex.RunShell(ctx, "echo x > /wakil-test-outside 2>/dev/null; test -f /wakil-test-outside && echo EXISTS || echo NOFILE")
	if strings.TrimSpace(out) == "EXISTS" {
		t.Error("wrote to read-only rootfs outside workspace — container is not confined")
	}
}

// TestDocker_BackgroundProcessLifecycle proves StartBackground/KillPgid/
// IsProcessAlive/ReadFileTail work inside the container.
func TestDocker_BackgroundProcessLifecycle(t *testing.T) {
	ex, _ := newDockerExec(t)
	ctx := context.Background()

	pid, pgid, err := ex.StartBackground(ctx, "echo hello-bg; sleep 30", "bg.log")
	if err != nil {
		t.Fatalf("StartBackground: %v", err)
	}
	if pid <= 0 {
		t.Fatalf("expected pid>0, got %d", pid)
	}
	if !ex.IsProcessAlive(ctx, pid) {
		t.Error("process should be alive right after start")
	}

	// Wait for the log line to appear.
	deadline := time.Now().Add(5 * time.Second)
	var tail string
	for time.Now().Before(deadline) {
		tail, _ = ex.ReadFileTail(ctx, "bg.log", 1024)
		if strings.Contains(tail, "hello-bg") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !strings.Contains(tail, "hello-bg") {
		t.Errorf("log tail should contain output; got %q", tail)
	}

	// Kill the process group.
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
		t.Error("process should be dead after SIGKILL")
	}
}

// TestDocker_StatFile proves DockerExecutor.StatFile (stat -c %s) returns
// the correct file size.
func TestDocker_StatFile(t *testing.T) {
	ex, _ := newDockerExec(t)
	ctx := context.Background()

	if _, err := ex.WriteFile(ctx, "stat-test.txt", "0123456789"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	size, err := ex.StatFile(ctx, "stat-test.txt")
	if err != nil {
		t.Fatalf("StatFile: %v", err)
	}
	if size != 10 {
		t.Errorf("StatFile size = %d, want 10", size)
	}

	if _, err := ex.StatFile(ctx, "nonexistent.txt"); err == nil {
		t.Error("StatFile on missing file should return error")
	}
}

// TestDocker_ReadFileTailCap proves ReadFileTail respects the maxBytes cap
// inside the container.
func TestDocker_ReadFileTailCap(t *testing.T) {
	ex, _ := newDockerExec(t)
	ctx := context.Background()

	if _, err := ex.WriteFile(ctx, "tail-test.txt", "0123456789abcdef"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := ex.ReadFileTail(ctx, "tail-test.txt", 6)
	if err != nil {
		t.Fatalf("ReadFileTail: %v", err)
	}
	if got != "abcdef" {
		t.Errorf("tail = %q, want last 6 bytes %q", got, "abcdef")
	}
}
