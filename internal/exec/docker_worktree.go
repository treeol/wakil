package exec

import (
	"bytes"
	"context"
	"fmt"
	osexec "os/exec"
	"strings"
)

// dockerWorktreeExecutor shares the parent DockerExecutor's container but
// re-roots all path-sensitive operations at wtRoot (a container-internal path,
// typically /tmp/wakil-wt-XXX). It is used by worktree isolation for parallel
// edit-tier subagents in Docker mode.
//
// Design:
//   - Embeds *DockerExecutor (pointer, not copy) so container-level methods
//     (Generation, KVRSocketPath, ContainerName, process probes, etc.) delegate
//     to the live parent — a container restart is visible to the child, and
//     sync.Once/generation state is shared, not copied.
//   - Overrides only the ~15 path-sensitive methods that must use wtRoot
//     instead of the parent's workspaceRoot.
//   - Close() is a no-op — the parent owns the container lifecycle.
//   - HostPathToURI/URIToHostPath return errors: a container /tmp worktree has
//     no host-side mapping. Edit-tier children don't use LSP tools, so these
//     methods are never called in practice; the errors are a safety guard.
//
// What delegates safely (no cd, takes absolute paths or pids):
//   - DeletePath, MovePath (operate on pre-ConfinePath'd absolute paths)
//   - StartBackground, StartInteractive (edit-tier children don't get exec tools;
//     if they ever do, these would need override — see comment on each method)
//   - KillPgid, IsProcessAlive, IsProcessGroupAlive (pid-based, path-agnostic)
//   - SandboxTools (probe runs in the container, not workspace-specific)
//   - Generation, KVRSocketPath, KVRAvailable, ContainerName, CDPPort (container-level)
type dockerWorktreeExecutor struct {
	*DockerExecutor          // delegate container-level methods
	wtRoot          string   // container-internal worktree root (e.g. /tmp/wakil-wt-XXX)
}

// NewDockerWorktreeExecutor creates an Executor that shares the parent's
// container but re-roots file operations at wtRoot. The parent must not be
// closed before the child is done (diff, apply, cleanup). Close() on the
// returned executor is a no-op.
func NewDockerWorktreeExecutor(parent *DockerExecutor, wtRoot string) Executor {
	return &dockerWorktreeExecutor{
		DockerExecutor: parent,
		wtRoot:         wtRoot,
	}
}

// RunShell runs a command in the container, cd'd into the worktree root.
func (w *dockerWorktreeExecutor) RunShell(ctx context.Context, command string) (string, error) {
	cmd := osexec.CommandContext(ctx, "docker", "exec", w.container, "sh", "-c",
		runFromRoot(w.wtRoot, command))
	out, err := cmd.CombinedOutput()
	return strings.TrimRight(string(out), "\r\n"), err
}

// Cwd returns the worktree root.
func (w *dockerWorktreeExecutor) Cwd() string { return w.wtRoot }

// WorkspaceRoot returns the worktree root.
func (w *dockerWorktreeExecutor) WorkspaceRoot() string { return w.wtRoot }

// Describe returns a human-readable summary.
func (w *dockerWorktreeExecutor) Describe() string {
	return fmt.Sprintf("docker-wt[%s → %s]%s", w.image, w.wtRoot, w.dockerSuffix())
}

// Close is a no-op — the parent owns the container lifecycle.
func (w *dockerWorktreeExecutor) Close() error { return nil }

// ConfinePath resolves path against the worktree root and verifies it lies
// within the worktree after symlink resolution.
func (w *dockerWorktreeExecutor) ConfinePath(ctx context.Context, path string) (string, error) {
	if !strings.HasPrefix(path, "/") {
		path = w.wtRoot + "/" + path
	}
	out, err := w.execCtx(ctx, false, "sh", "-c", "readlink -f "+shQuote(path)+" 2>&1")
	if err != nil {
		return "", fmt.Errorf("resolving path %q: %s", path, strings.TrimSpace(out))
	}
	canonical := strings.TrimSpace(out)
	if canonical == "" {
		return "", fmt.Errorf("could not resolve path %q", path)
	}
	if !isInsideWorkspace(canonical, w.wtRoot) {
		return "", fmt.Errorf("path %q (→ %s) is outside worktree %q — traversal not allowed", path, canonical, w.wtRoot)
	}
	return canonical, nil
}

// ReadFile reads a file from the worktree, resolving relative paths against wtRoot.
func (w *dockerWorktreeExecutor) ReadFile(ctx context.Context, path string) (string, error) {
	out, err := w.execCtx(ctx, false, "sh", "-c",
		"cd "+shQuote(w.wtRoot)+` && cat -- "$1"`, "sh", path)
	if err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(out))
	}
	return out, nil
}

// StatFile returns the byte size of a file in the worktree.
func (w *dockerWorktreeExecutor) StatFile(ctx context.Context, path string) (int64, error) {
	out, err := w.execCtx(ctx, false, "sh", "-c",
		"cd "+shQuote(w.wtRoot)+` && stat -c %s -- "$1"`, "sh", path)
	if err != nil {
		return 0, fmt.Errorf("%s", strings.TrimSpace(out))
	}
	var size int64
	if _, scanErr := fmt.Sscan(strings.TrimSpace(out), &size); scanErr != nil {
		return 0, fmt.Errorf("unexpected stat output: %q", strings.TrimSpace(out))
	}
	return size, nil
}

// ListDir lists entries in the worktree.
func (w *dockerWorktreeExecutor) ListDir(ctx context.Context, path string) (string, error) {
	if path == "" {
		path = "."
	}
	out, err := w.execCtx(ctx, false, "sh", "-c",
		"cd "+shQuote(w.wtRoot)+` && ls -Ap -- "$1"`, "sh", path)
	if err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(out))
	}
	return out, nil
}

// WriteFile writes string content to a file in the worktree.
func (w *dockerWorktreeExecutor) WriteFile(ctx context.Context, path, content string) (string, error) {
	cmd := osexec.CommandContext(ctx, "docker", "exec", "-i", w.container, "sh", "-c",
		"cd "+shQuote(w.wtRoot)+` && mkdir -p "$(dirname -- "$1")" && cat > "$1"`, "sh", path)
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), path), nil
}

// WriteFileBytes writes raw bytes to a file in the worktree via stdin pipe.
func (w *dockerWorktreeExecutor) WriteFileBytes(ctx context.Context, path string, content []byte) (string, error) {
	cmd := osexec.CommandContext(ctx, "docker", "exec", "-i", w.container, "sh", "-c",
		"cd "+shQuote(w.wtRoot)+` && mkdir -p "$(dirname -- "$1")" && cat > "$1"`, "sh", path)
	cmd.Stdin = bytes.NewReader(content)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), path), nil
}

// ReadFileTail returns the last maxBytes of a file in the worktree.
func (w *dockerWorktreeExecutor) ReadFileTail(ctx context.Context, path string, maxBytes int64) (string, error) {
	out, err := w.execCtx(ctx, false, "sh", "-c",
		fmt.Sprintf("cd %s && tail -c %d -- \"$1\" 2>&1", shQuote(w.wtRoot), maxBytes), "sh", path)
	if err != nil {
		return "", fmt.Errorf("reading log: %s", strings.TrimSpace(out))
	}
	return out, nil
}

// HostPathToURI returns an error — a container /tmp worktree has no host-side
// mapping. Edit-tier children don't use LSP tools, so this is never called.
func (w *dockerWorktreeExecutor) HostPathToURI(hostPath string) (string, error) {
	return "", fmt.Errorf("HostPathToURI not supported for worktree executor (no host mapping for %q)", w.wtRoot)
}

// URIToHostPath returns an error — a container /tmp worktree has no host-side
// mapping. Edit-tier children don't use LSP tools, so this is never called.
func (w *dockerWorktreeExecutor) URIToHostPath(uri string) (string, error) {
	return "", fmt.Errorf("URIToHostPath not supported for worktree executor (no host mapping for %q)", w.wtRoot)
}

// dockerSuffix returns the suffix string for Describe, mirroring DockerExecutor.Describe.
func (w *dockerWorktreeExecutor) dockerSuffix() string {
	sock := ""
	if w.dockerSock {
		sock = " +docker"
	}
	if w.signing {
		sock += " +sign"
	}
	if w.kvrAvailable {
		sock += " +kvr"
	} else if w.stagingMount != "" {
		sock += " +kvr(off)"
	}
	if w.seccompProfilePath != "" {
		sock += " +iouring"
	}
	return sock
}
