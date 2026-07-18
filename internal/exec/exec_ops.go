package exec

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
)

// ── A2: sandbox tool probe ────────────────────────────────────────────────────

var versionRe = regexp.MustCompile(`\d+\.\d+[\d.]*`)

// probeScript checks each tool with `command -v`, then captures the first line
// of version output. `go` uses `go version` (not --version); `docker` reports
// the CLI version but is also pinged against the daemon so it only shows when
// the host socket is actually mounted and reachable. Absent tools produce no
// output for that iteration.
const probeScript = `_ver() {
  case "$1" in
    go) "$1" version 2>&1 | head -1 ;;
    docker) "$1" version --format '{{.Client.Version}}' 2>&1 | head -1 ;;
    *)  "$1" --version 2>&1 | head -1 ;;
  esac
}
for t in git curl jq make gcc python3 node npm go rustc docker; do
  if command -v "$t" > /dev/null 2>&1; then
    v=$(_ver "$t")
    echo "$t:$v"
  fi
done
if command -v docker > /dev/null 2>&1; then
  if docker info > /dev/null 2>&1; then echo "docker-daemon:up"; else echo "docker-daemon:down"; fi
fi`

// probeTools runs the probe via the supplied runner and returns the formatted
// "Sandbox tools: …" line, or empty string on failure / all absent.
func probeTools(run func(string) string) string {
	out := strings.TrimSpace(run(probeScript))
	if out == "" {
		return ""
	}

	order := []string{"git", "curl", "jq", "make", "gcc", "python3", "node", "npm", "go", "rustc", "docker"}
	versions := make(map[string]string, len(order))
	daemonDown := false

	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		idx := strings.Index(line, ":")
		if idx < 1 {
			continue
		}
		tool := strings.TrimSpace(line[:idx])
		rest := strings.TrimSpace(line[idx+1:])
		if tool == "docker-daemon" {
			// docker CLI present but daemon unreachable (socket not mounted
			// or daemon down) — report the CLI as unavailable.
			daemonDown = rest != "up"
			continue
		}
		m := versionRe.FindString(rest)
		if m != "" {
			versions[tool] = m
		} else {
			versions[tool] = "?" // present but version unparseable
		}
	}
	if daemonDown {
		delete(versions, "docker")
	}

	var present, absent []string
	for _, t := range order {
		if v, ok := versions[t]; ok {
			present = append(present, t+" "+v)
		} else {
			absent = append(absent, t)
		}
	}

	if len(present) == 0 {
		return ""
	}
	result := "Sandbox tools: " + strings.Join(present, ", ")
	if len(absent) > 0 {
		result += " — unavailable: " + strings.Join(absent, ", ")
	}
	return result
}

// SandboxTools is guarded by sync.Once: executors are shared with concurrent
// subagent workers, so the lazy probe cache must not be written racily.
func (d *DockerExecutor) SandboxTools() string {
	d.toolsOnce.Do(func() {
		d.sandboxTools = probeTools(func(script string) string {
			out, _ := d.execCtx(context.Background(), false, "sh", "-c", script)
			return out
		})
	})
	return d.sandboxTools
}

func (e *DirectExecutor) SandboxTools() string {
	e.toolsOnce.Do(func() {
		e.sandboxTools = probeTools(func(script string) string {
			out, _ := exec.Command("sh", "-c", script).CombinedOutput()
			return string(out)
		})
	})
	return e.sandboxTools
}

// ── B1/B2: path confinement ───────────────────────────────────────────────────

// isInsideWorkspace returns true if p equals root or is directly nested inside it.
func isInsideWorkspace(p, root string) bool {
	root = filepath.Clean(root)
	p = filepath.Clean(p)
	return p == root || strings.HasPrefix(p, root+"/")
}

func (d *DockerExecutor) ConfinePath(ctx context.Context, path string) (string, error) {
	if !strings.HasPrefix(path, "/") {
		path = d.workspaceRoot + "/" + path
	}
	// readlink -f canonicalises the path and resolves symlinks; on GNU coreutils
	// it also works for non-existent paths by resolving existing components.
	out, err := d.execCtx(ctx, false, "sh", "-c", "readlink -f "+shQuote(path)+" 2>&1")
	if err != nil {
		return "", fmt.Errorf("resolving path %q: %s", path, strings.TrimSpace(out))
	}
	canonical := strings.TrimSpace(out)
	if canonical == "" {
		return "", fmt.Errorf("could not resolve path %q", path)
	}
	if !isInsideWorkspace(canonical, d.workspaceRoot) {
		return "", fmt.Errorf("path %q (→ %s) is outside workspace %q — traversal not allowed", path, canonical, d.workspaceRoot)
	}
	return canonical, nil
}

func (e *DirectExecutor) ConfinePath(_ context.Context, path string) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(e.root, path)
	}
	path = filepath.Clean(path)
	// Resolve symlinks for existing paths; skip for non-existent (move dst).
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	if !isInsideWorkspace(path, e.root) {
		return "", fmt.Errorf("path %q is outside workspace %q — traversal not allowed", path, e.root)
	}
	return path, nil
}

// ── B1: delete ────────────────────────────────────────────────────────────────

func (d *DockerExecutor) DeletePath(ctx context.Context, path string) error {
	// For directories use rmdir (fails on non-empty); for files/symlinks use rm.
	script := fmt.Sprintf(
		`if [ -d %[1]s ] && [ ! -L %[1]s ]; then rmdir -- %[1]s 2>&1; else rm -- %[1]s 2>&1; fi`,
		shQuote(path))
	out, err := d.execCtx(ctx, false, "sh", "-c", script)
	if err != nil {
		msg := strings.TrimSpace(out)
		if strings.Contains(msg, "not empty") || strings.Contains(strings.ToLower(msg), "directory not empty") {
			return fmt.Errorf("directory is not empty — use run_shell rm -r to remove recursively")
		}
		if msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return err
	}
	return nil
}

func (e *DirectExecutor) DeletePath(_ context.Context, path string) error {
	if err := os.Remove(path); err != nil {
		var pe *os.PathError
		if errors.As(err, &pe) && errors.Is(pe.Err, syscall.ENOTEMPTY) {
			return fmt.Errorf("directory is not empty — use run_shell rm -r to remove recursively")
		}
		return err
	}
	return nil
}

// ── B2: move/rename ───────────────────────────────────────────────────────────

func (d *DockerExecutor) MovePath(ctx context.Context, src, dst string) error {
	// Explicit existence check before mv so the error message is actionable.
	checkOut, checkErr := d.execCtx(ctx, false, "sh", "-c",
		fmt.Sprintf(`[ ! -e %[1]s ] && [ ! -L %[1]s ] && echo ok || echo exists`, shQuote(dst)))
	if checkErr == nil && strings.TrimSpace(checkOut) == "exists" {
		return fmt.Errorf("destination already exists: %s — delete it first if you intended to overwrite", dst)
	}
	out, err := d.execCtx(ctx, false, "sh", "-c",
		fmt.Sprintf("mv -n -- %s %s 2>&1", shQuote(src), shQuote(dst)))
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(out))
	}
	return nil
}

func (e *DirectExecutor) MovePath(_ context.Context, src, dst string) error {
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("destination already exists: %s — delete it first if you intended to overwrite", dst)
	}
	if err := os.Rename(src, dst); err != nil {
		var linkErr *os.LinkError
		if errors.As(err, &linkErr) && errors.Is(linkErr.Err, syscall.EXDEV) {
			return crossDeviceCopy(src, dst)
		}
		return err
	}
	return nil
}

// crossDeviceCopy copies src to dst then removes src for cross-filesystem moves.
func crossDeviceCopy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_EXCL, info.Mode())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}
	return os.Remove(src)
}

// ── B3: background processes ──────────────────────────────────────────────────

func (d *DockerExecutor) StartBackground(ctx context.Context, command, logPath string) (pid, pgid int, err error) {
	// setsid creates a new session so PGID == PID of the setsid process.
	// The & sends it to the background; echo $! captures its PID.
	script := fmt.Sprintf(
		`setsid nohup sh -c %s > %s 2>&1 & echo $!`,
		shQuote(command), shQuote(logPath))
	out, execErr := d.execCtx(ctx, false, "sh", "-c", script)
	if execErr != nil {
		return 0, 0, fmt.Errorf("starting background process: %s", strings.TrimSpace(out))
	}
	if n, _ := fmt.Sscan(strings.TrimSpace(out), &pid); n != 1 || pid <= 0 {
		return 0, 0, fmt.Errorf("unexpected PID output from background start: %q", strings.TrimSpace(out))
	}
	pgid = pid // setsid guarantees PGID == PID
	return pid, pgid, nil
}

func (e *DirectExecutor) StartBackground(_ context.Context, command, logPath string) (pid, pgid int, err error) {
	cmd := exec.Command("sh", "-c", command)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return 0, 0, fmt.Errorf("creating log file: %w", err)
	}
	defer logFile.Close()
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return 0, 0, fmt.Errorf("starting process: %w", err)
	}
	pid = cmd.Process.Pid
	// Reap the child when it exits so it doesn't become a zombie.
	go func() { _ = cmd.Wait() }()
	return pid, pid, nil // Setpgid: PGID == PID
}

func (d *DockerExecutor) KillPgid(ctx context.Context, pgid, sig int) error {
	// Use "kill -SIG -PGID" (no --); dash's kill builtin rejects "-- -N".
	script := fmt.Sprintf("kill -%d -%d 2>/dev/null; exit 0", sig, pgid)
	_, err := d.execCtx(ctx, false, "sh", "-c", script)
	return err
}

func (e *DirectExecutor) KillPgid(_ context.Context, pgid, sig int) error {
	err := syscall.Kill(-pgid, syscall.Signal(sig))
	if err != nil && errors.Is(err, syscall.ESRCH) {
		return nil // already gone
	}
	return err
}

func (d *DockerExecutor) IsProcessAlive(ctx context.Context, pid int) bool {
	// kill -0 returns 0 for zombies too, so check /proc state directly.
	// A zombie (state Z) has exited — treat as not alive.
	out, err := d.execCtx(ctx, false, "sh", "-c",
		fmt.Sprintf("ps -o stat= -p %d 2>/dev/null", pid))
	if err != nil {
		return false // docker exec failed (container may be gone)
	}
	state := strings.TrimSpace(out)
	return state != "" && !strings.HasPrefix(state, "Z")
}

func (e *DirectExecutor) IsProcessAlive(_ context.Context, pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func (d *DockerExecutor) ReadFileTail(ctx context.Context, path string, maxBytes int64) (string, error) {
	out, err := d.execCtx(ctx, false, "sh", "-c",
		fmt.Sprintf("tail -c %d %s 2>&1", maxBytes, shQuote(path)))
	if err != nil {
		return "", fmt.Errorf("reading log: %s", strings.TrimSpace(out))
	}
	return out, nil
}

func (e *DirectExecutor) ReadFileTail(_ context.Context, path string, maxBytes int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := stat.Size()
	start := size - maxBytes
	if start < 0 {
		start = 0
	}
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return string(buf), nil
}
