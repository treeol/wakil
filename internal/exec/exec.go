package exec

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Executor abstracts where tool_calls actually run. Commands always execute
// from the workspace root; in-command directory changes (cd sub && …) affect
// only that command. Closing tears the backend down.
type Executor interface {
	RunShell(ctx context.Context, command string) (string, error)
	ReadFile(path string) (string, error)
	ListDir(path string) (string, error)
	WriteFile(path, content string) (string, error)
	Cwd() string
	Describe() string
	Close() error

	// A2: one-line sandbox tool inventory, lazy-probed and cached per instance.
	// Returns empty string on probe failure; never panics.
	SandboxTools() string
	// WorkspaceRoot returns the immutable workspace root used for path confinement.
	WorkspaceRoot() string
	// ConfinePath resolves path (relative paths resolved against WorkspaceRoot) and
	// verifies it lies within WorkspaceRoot after symlink resolution. Returns the
	// canonical absolute path or an error describing the violation.
	ConfinePath(ctx context.Context, path string) (string, error)

	// B1: delete a file or empty directory; returns a descriptive error for
	// non-empty directories so the model knows to use run_shell rm -r.
	DeletePath(ctx context.Context, path string) error
	// B2: rename/move src to dst; fails if dst already exists.
	MovePath(ctx context.Context, src, dst string) error

	// StartInteractive spawns a long-running process with stdin/stdout/stderr
	// pipes for bidirectional communication (e.g., an LSP server speaking JSON-RPC).
	// The caller owns the pipes and must close them on completion. In docker mode
	// the process runs inside the container (correct filesystem, toolchain,
	// module cache); the pipes are on the host.
	StartInteractive(ctx context.Context, command string) (
		stdin io.WriteCloser,
		stdout io.ReadCloser,
		stderr io.ReadCloser,
		pid int,
		err error,
	)

	// HostPathToURI translates a host filesystem path to a file:// URI in the
	// namespace the LSP server sees. In docker mode this maps hostMount/<rel>
	// to workspaceRoot/<rel> (container-visible). In direct mode it's identity.
	// This is the ONLY sanctioned way to produce a URI for gopls — never
	// hand-build file:// URIs from host paths.
	HostPathToURI(hostPath string) (uri string, err error)

	// URIToHostPath translates a file:// URI returned by the LSP server back to
	// a host filesystem path. In docker mode this maps workspaceRoot/<rel>
	// (container) back to hostMount/<rel> (host). URIs outside workspaceRoot
	// (e.g. GOROOT /usr/local/go/src/...) return an error — they have no host
	// mapping and must be handled by explicit policy, not silently mapped.
	URIToHostPath(uri string) (hostPath string, err error)

	// B3: start command detached in the background; returns pid and pgid.
	// logPath is where stdout/stderr are redirected.
	StartBackground(ctx context.Context, command, logPath string) (pid, pgid int, err error)
	// KillPgid sends signal sig (15=SIGTERM, 9=SIGKILL) to the entire process group.
	KillPgid(ctx context.Context, pgid, sig int) error
	// IsProcessAlive returns true if pid is still running (kill -0 check).
	IsProcessAlive(ctx context.Context, pid int) bool
	// ReadFileTail returns the last maxBytes of path; enforces the cap internally.
	ReadFileTail(ctx context.Context, path string, maxBytes int64) (string, error)
	// StatFile returns the byte size of the file at path without reading it.
	// Returns an error if the path does not exist or is not accessible.
	StatFile(path string) (int64, error)
	// Generation returns a counter that increments when the executor backend is
	// restarted (e.g. container recreated). Background process entries from an
	// older generation are stale.
	Generation() int
}

// runFromRoot starts a shell command from root (the workspace root).
// In-command directory changes work normally but are not tracked between calls.
// A trailing newline after the command body prevents a trailing # comment from
// swallowing the closing brace.
func runFromRoot(root, command string) string {
	return fmt.Sprintf("cd %s 2>/dev/null && { %s\n}", shQuote(root), command)
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ---------------- Docker executor (default) ----------------

// DockerExecutor runs everything inside one persistent container that lives for
// the process lifetime. If hostMount is set, that host path is bind-mounted to
// workdir so files written inside the container appear on the host filesystem.
type DockerExecutor struct {
	container     string
	image         string
	workspaceRoot string // project root; immutable; all commands and file ops start here
	hostMount     string // host path mounted at workspaceRoot, empty = no mount
	dockerSock    bool   // host docker socket bind-mounted in
	sandboxTools  string // cached probe result (empty = not yet probed or probe failed)
	toolsProbed   bool   // true once SandboxTools() has run the probe
	generation    int    // increments on container restart; 1 for initial container
}

// dockerSocketPath is the host docker socket bind-mounted into the sandbox when
// docker_socket is enabled, so the in-container docker CLI drives the host daemon.
const dockerSocketPath = "/var/run/docker.sock"

func NewDockerExecutor(image, workdir, hostMount string, dockerSock bool) (*DockerExecutor, error) {
	if workdir == "" {
		workdir = "/work"
	}
	name := "wakil-session-" + fmt.Sprint(os.Getpid())
	// Best-effort remove a stale container from a previous crashed run.
	_ = exec.Command("docker", "rm", "-f", name).Run()

	args := []string{"run", "-d", "--name", name, "-w", workdir}
	if hostMount != "" {
		if err := os.MkdirAll(hostMount, 0o755); err != nil {
			return nil, fmt.Errorf("creating host workdir %s: %w", hostMount, err)
		}
		args = append(args, "-v", hostMount+":"+workdir)
	}
	if dockerSock {
		args = append(args, "-v", dockerSocketPath+":"+dockerSocketPath)
	}

	// Run as the current host user so files written to the mounted workspace
	// are owned correctly. A persistent directory on the host serves as HOME
	// so Go/Cargo module caches survive across sessions.
	if uid := os.Getuid(); uid > 0 {
		if hostHome, err := os.UserHomeDir(); err == nil {
			sandboxHome := filepath.Join(hostHome, ".wakil", "sandbox-home")
			if os.MkdirAll(sandboxHome, 0o700) == nil {
				// PATH must be set explicitly: we prepend user bin dirs to the
				// system PATH baked into the image by the Dockerfile.
				const systemPath = "/usr/local/go/bin:/usr/local/go-workspace/bin" +
					":/usr/local/cargo/bin" +
					":/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
				args = append(args,
					"--user", fmt.Sprintf("%d:%d", uid, os.Getgid()),
					"-v", sandboxHome+":/home/user",
					"-e", "HOME=/home/user",
					"-e", "GOPATH=/home/user/go",
					"-e", "CARGO_HOME=/home/user/.cargo",
					"-e", "PATH=/home/user/go/bin:/home/user/.cargo/bin:"+systemPath,
				)
			}
		}
	}

	args = append(args, image, "sleep", "infinity")

	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker run failed: %s", strings.TrimSpace(string(out)))
	}
	d := &DockerExecutor{container: name, image: image, workspaceRoot: workdir, hostMount: hostMount, dockerSock: dockerSock, generation: 1}
	if hostMount == "" {
		_, _ = d.exec(false, "sh", "-c", "mkdir -p "+shQuote(workdir))
	}
	if dockerSock {
		ensureDockerCLI(name)
	}
	return d, nil
}

// ensureDockerCLI copies the host docker binary into the container if the
// container doesn't already have one. Copying from the host is instant, needs
// no network, and is version-matched to the running daemon.
func ensureDockerCLI(container string) {
	hostBin, err := exec.LookPath("docker")
	if err != nil {
		return
	}
	out, _ := exec.Command("docker", "exec", container, "sh", "-c",
		"command -v docker >/dev/null 2>&1 && echo yes").Output()
	if strings.TrimSpace(string(out)) == "yes" {
		return
	}
	_ = exec.Command("docker", "cp", hostBin, container+":/usr/local/bin/docker").Run()
}

func (d *DockerExecutor) exec(interactive bool, args ...string) (string, error) {
	full := append([]string{"exec"}, append(map[bool][]string{true: {"-i"}, false: nil}[interactive], append([]string{d.container}, args...)...)...)
	cmd := exec.Command("docker", full...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (d *DockerExecutor) RunShell(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "exec", d.container, "sh", "-c", runFromRoot(d.workspaceRoot, command))
	out, err := cmd.CombinedOutput()
	return strings.TrimRight(string(out), "\r\n"), err
}

func (d *DockerExecutor) ReadFile(path string) (string, error) {
	// cd into workspaceRoot first so relative paths resolve from the project root.
	out, err := d.exec(false, "sh", "-c", "cd "+shQuote(d.workspaceRoot)+` && cat -- "$1"`, "sh", path)
	if err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(out))
	}
	return out, nil
}

func (d *DockerExecutor) StatFile(path string) (int64, error) {
	out, err := d.exec(false, "sh", "-c", "cd "+shQuote(d.workspaceRoot)+` && stat -c %s -- "$1"`, "sh", path)
	if err != nil {
		return 0, fmt.Errorf("%s", strings.TrimSpace(out))
	}
	var size int64
	if _, scanErr := fmt.Sscan(strings.TrimSpace(out), &size); scanErr != nil {
		return 0, fmt.Errorf("unexpected stat output: %q", strings.TrimSpace(out))
	}
	return size, nil
}

func (d *DockerExecutor) ListDir(path string) (string, error) {
	if path == "" {
		path = "."
	}
	// ls -Ap: all entries except . and .., with a trailing / on directories.
	out, err := d.exec(false, "sh", "-c", "cd "+shQuote(d.workspaceRoot)+` && ls -Ap -- "$1"`, "sh", path)
	if err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(out))
	}
	return out, nil
}

func (d *DockerExecutor) WriteFile(path, content string) (string, error) {
	cmd := exec.Command("docker", "exec", "-i", d.container, "sh", "-c",
		"cd "+shQuote(d.workspaceRoot)+` && mkdir -p "$(dirname -- "$1")" && cat > "$1"`, "sh", path)
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), path), nil
}

func (d *DockerExecutor) Cwd() string           { return d.workspaceRoot }
func (d *DockerExecutor) WorkspaceRoot() string { return d.workspaceRoot }
func (d *DockerExecutor) Generation() int       { return d.generation }
func (d *DockerExecutor) Describe() string {
	sock := ""
	if d.dockerSock {
		sock = " +docker"
	}
	if d.hostMount != "" {
		return fmt.Sprintf("docker[%s → %s]%s", d.image, d.hostMount, sock)
	}
	return fmt.Sprintf("docker[%s]%s", d.image, sock)
}
func (d *DockerExecutor) Close() error {
	return exec.Command("docker", "rm", "-f", d.container).Run()
}

// StartInteractive spawns a long-running process inside the container with
// stdin/stdout/stderr pipes for bidirectional JSON-RPC communication (e.g. gopls).
// The -i flag keeps stdin open. The caller owns the pipes.
//
// Shutdown contract (R1): the caller (Manager) must send the LSP shutdown
// request, await its response, send the exit notification, THEN close stdin.
// Closing stdin alone causes gopls to exit via the error path (noisy, non-zero
// exit). In docker mode, closing the host-side docker exec stdin may not
// reliably reap the in-container gopls child — the proper shutdown sequence
// is the reliable path. The spawn command should use 'exec gopls' so sh does
// not linger as a separate parent.
func (d *DockerExecutor) StartInteractive(_ context.Context, command string) (
	stdin io.WriteCloser, stdout io.ReadCloser, stderr io.ReadCloser, pid int, err error,
) {
	cmd := exec.Command("docker", "exec", "-i", d.container, "sh", "-c", command)
	stdin, err = cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err = cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, nil, nil, 0, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err = cmd.StderrPipe()
	if err != nil {
		stdin.Close()
		stdout.Close()
		return nil, nil, nil, 0, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		stderr.Close()
		return nil, nil, nil, 0, fmt.Errorf("starting interactive process: %w", err)
	}
	// Reap the child when it exits so it doesn't become a zombie.
	go func() { _ = cmd.Wait() }()
	return stdin, stdout, stderr, cmd.Process.Pid, nil
}

// HostPathToURI translates a host filesystem path to a container-visible
// file:// URI for gopls. In docker mode this maps hostMount/<rel> →
// workspaceRoot/<rel>. The input may be relative (resolved against hostMount)
// or absolute (must be under hostMount).
// Returns an error if the path is not under hostMount (the leak guard: a host
// path sent to in-container gopls would silently return empty results).
func (d *DockerExecutor) HostPathToURI(hostPath string) (string, error) {
	// Resolve relative paths against hostMount (the host-side mount point).
	if !filepath.IsAbs(hostPath) {
		hostPath = filepath.Join(d.hostMount, hostPath)
	}
	rel, err := filepath.Rel(d.hostMount, hostPath)
	if err != nil {
		return "", fmt.Errorf("rel path: %w", err)
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path %q is outside host mount %q — cannot translate to container URI", hostPath, d.hostMount)
	}
	containerPath := filepath.Join(d.workspaceRoot, rel)
	return pathToURI(containerPath), nil
}

// URIToHostPath translates a container-visible file:// URI back to a host
// filesystem path. URIs outside workspaceRoot (e.g. GOROOT paths) return
// an error — they have no host mapping.
func (d *DockerExecutor) URIToHostPath(uri string) (string, error) {
	containerPath, err := uriToPath(uri)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(d.workspaceRoot, containerPath)
	if err != nil {
		return "", fmt.Errorf("rel path: %w", err)
	}
	if strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("URI %q (container path %q) is outside workspace root %q — no host mapping (e.g. GOROOT path)", uri, containerPath, d.workspaceRoot)
	}
	return filepath.Join(d.hostMount, rel), nil
}

// ---------------- Direct executor (opt-in) ----------------

// DirectExecutor runs commands directly on the host. Still fully gated.
type DirectExecutor struct {
	root         string // project root; immutable; all commands and file ops start here
	sandboxTools string
	toolsProbed  bool
	generation   int
}

func NewDirectExecutor(workdir string) (*DirectExecutor, error) {
	if workdir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		workdir = wd
	}
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return nil, err
	}
	return &DirectExecutor{root: abs, generation: 1}, nil
}

func (e *DirectExecutor) RunShell(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "sh", "-c", runFromRoot(e.root, command))
	out, err := cmd.CombinedOutput()
	return strings.TrimRight(string(out), "\r\n"), err
}

func (e *DirectExecutor) resolve(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(e.root, path)
}

func (e *DirectExecutor) StatFile(path string) (int64, error) {
	info, err := os.Stat(e.resolve(path))
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (e *DirectExecutor) ReadFile(path string) (string, error) {
	b, err := os.ReadFile(e.resolve(path))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (e *DirectExecutor) ListDir(path string) (string, error) {
	if path == "" {
		path = "."
	}
	entries, err := os.ReadDir(e.resolve(path))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for _, en := range entries {
		name := en.Name()
		if en.IsDir() {
			name += "/"
		}
		b.WriteString(name)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

func (e *DirectExecutor) WriteFile(path, content string) (string, error) {
	full := e.resolve(path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(content), path), nil
}

func (e *DirectExecutor) Cwd() string           { return e.root }
func (e *DirectExecutor) WorkspaceRoot() string { return e.root }
func (e *DirectExecutor) Generation() int       { return e.generation }
func (e *DirectExecutor) Describe() string      { return "direct[" + e.root + "]" }
func (e *DirectExecutor) Close() error          { return nil }

// StartInteractive spawns a long-running process on the host with stdin/stdout/
// stderr pipes for bidirectional JSON-RPC communication (e.g. gopls in direct mode).
// The caller owns the pipes and must close stdin to terminate the server.
func (e *DirectExecutor) StartInteractive(_ context.Context, command string) (
	stdin io.WriteCloser, stdout io.ReadCloser, stderr io.ReadCloser, pid int, err error,
) {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = e.root
	stdin, err = cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err = cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return nil, nil, nil, 0, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err = cmd.StderrPipe()
	if err != nil {
		stdin.Close()
		stdout.Close()
		return nil, nil, nil, 0, fmt.Errorf("stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		stdin.Close()
		stdout.Close()
		stderr.Close()
		return nil, nil, nil, 0, fmt.Errorf("starting interactive process: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return stdin, stdout, stderr, cmd.Process.Pid, nil
}

// HostPathToURI in direct mode: identity (host path == LSP path).
// Resolves relative paths against the workspace root.
func (e *DirectExecutor) HostPathToURI(hostPath string) (string, error) {
	if !filepath.IsAbs(hostPath) {
		hostPath = filepath.Join(e.root, hostPath)
	}
	return pathToURI(hostPath), nil
}

// URIToHostPath in direct mode: identity.
func (e *DirectExecutor) URIToHostPath(uri string) (string, error) {
	return uriToPath(uri)
}

// pathToURI converts a filesystem path to a file:// URI with proper encoding.
func pathToURI(path string) string {
	encoded := strings.ReplaceAll(path, " ", "%20")
	return "file://" + encoded
}

// uriToPath extracts the filesystem path from a file:// URI.
func uriToPath(uri string) (string, error) {
	if !strings.HasPrefix(uri, "file://") {
		return "", fmt.Errorf("not a file:// URI: %q", uri)
	}
	path := uri[len("file://"):]
	path = strings.ReplaceAll(path, "%20", " ")
	return path, nil
}
