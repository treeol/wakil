package exec

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Executor abstracts where tool_calls actually run. Commands always execute
// from the workspace root; in-command directory changes (cd sub && …) affect
// only that command. Closing tears the backend down.
type Executor interface {
	RunShell(ctx context.Context, command string) (string, error)
	ReadFile(ctx context.Context, path string) (string, error)
	ListDir(ctx context.Context, path string) (string, error)
	WriteFile(ctx context.Context, path, content string) (string, error)
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
	StatFile(ctx context.Context, path string) (int64, error)
	// Generation returns a counter that increments when the executor backend is
	// restarted (e.g. container recreated). Background process entries from an
	// older generation are stale.
	Generation() int

	// KVRSocketPath returns the host-side path to the kvr UDS socket, or ""
	// if kvr is not available (disabled, direct mode, or failed to start).
	// The Go staging client connects to this path directly.
	KVRSocketPath() string
	// KVRAvailable reports whether the kvr staging store started successfully
	// and is ready to serve requests.
	KVRAvailable() bool
}

// runFromRoot starts a shell command from root (the workspace root).
// In-command directory changes work normally but are not tracked between calls.
// A trailing newline after the command body prevents a trailing # comment from
// swallowing the closing brace.
func runFromRoot(root, command string) string {
	return fmt.Sprintf("cd %s && { %s\n}", shQuote(root), command)
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// randSuffix returns n hex characters of randomness for unique-naming purposes
// (container names, etc.). Uses crypto/rand so the result is not predictable.
func randSuffix(n int) string {
	b := make([]byte, n)
	_, _ = crand.Read(b)
	return hex.EncodeToString(b)
}

// ---------------- Docker executor (default) ----------------

// DockerExecutor runs everything inside one persistent container that lives for
// the process lifetime. If hostMount is set, that host path is bind-mounted to
// workdir so files written inside the container appear on the host filesystem.
type DockerExecutor struct {
	container     string
	image         string
	workspaceRoot string    // project root; immutable; all commands and file ops start here
	hostMount     string    // host path mounted at workspaceRoot, empty = no mount
	dockerSock    bool      // host docker socket bind-mounted in
	signing       bool      // SSH signing passthrough active (agent socket mounted)
	sandboxTools  string    // cached probe result (empty = not yet probed or probe failed)
	toolsOnce     sync.Once // guards the probe: executor is shared with concurrent subagents
	generation    int       // increments on container restart; 1 for initial container
	// kvr staging store
	stagingMount string // host path of the staging mount; empty = no kvr
	kvrSocket    string // host-side path to the kvr UDS socket; empty if unavailable
	kvrAvailable bool   // kvr started and PING succeeded
}

// dockerSocketPath is the host docker socket bind-mounted into the sandbox when
// docker_socket is enabled, so the in-container docker CLI drives the host daemon.
const dockerSocketPath = "/var/run/docker.sock"

// DockerOpts bundles the DockerExecutor constructor parameters. Introduced
// when the positional list hit four and SSH signing added more.
type DockerOpts struct {
	Image      string
	Workdir    string // in-container workspace root
	HostMount  string // host path mounted at Workdir; empty = no mount
	DockerSock bool   // bind-mount the host docker socket
	// Signing carries the host-resolved SSH commit-signing setup (agent
	// socket + literal public key). Zero value = signing disabled.
	Signing SigningSetup
	// StagingMount is the host path mounted at /run/kvr-staging inside the
	// container. The kvr UDS socket and snapshot file both live on this
	// mount so the host-side Go client can reach the socket. Empty = no
	// kvr staging store.
	StagingMount string
	// KVREnabled controls whether kvr-server is started. When false, no
	// staging mount is added, no entrypoint is used, and KVRSocketPath()
	// returns "".
	KVREnabled bool
	// KVR config (read from Wakil config, passed as env vars to the container).
	KVRMaxEntries           int
	KVRSweepIntervalSecs    int
	KVRSnapshotIntervalSecs int
	// Docker hardening flags (defense-in-depth). See Config.DockerCaps etc.
	DockerCaps      []string
	DockerMemory    string
	DockerPidsLimit int
}

// waitForKVR polls the kvr UDS socket with PING until it responds or the
// timeout elapses. PING frame: 4B BE length (1) + opcode 0x03 (PING).
// Expected response: 4B BE length (1) + 0x10 (RESP_OK).
func waitForKVR(socketPath string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	pingFrame := []byte{0x00, 0x00, 0x00, 0x01, 0x03} // len=1, PING

	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", socketPath, 500*time.Millisecond)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		conn.SetDeadline(time.Now().Add(500 * time.Millisecond))
		if _, err := conn.Write(pingFrame); err != nil {
			conn.Close()
			time.Sleep(100 * time.Millisecond)
			continue
		}
		// Read response: 4B length + payload.
		var lenBuf [4]byte
		if _, err := io.ReadFull(conn, lenBuf[:]); err != nil {
			conn.Close()
			time.Sleep(100 * time.Millisecond)
			continue
		}
		respLen := binary.BigEndian.Uint32(lenBuf[:])
		if respLen == 0 || respLen > 1<<20 {
			conn.Close()
			time.Sleep(100 * time.Millisecond)
			continue
		}
		resp := make([]byte, respLen)
		if _, err := io.ReadFull(conn, resp); err != nil {
			conn.Close()
			time.Sleep(100 * time.Millisecond)
			continue
		}
		conn.Close()
		if len(resp) >= 1 && resp[0] == 0x10 { // RESP_OK
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("kvr did not become ready within %s", timeout)
}

// dockerHardeningArgs returns the sandbox hardening flags appended to every
// docker run command. Core flags (--cap-drop, --read-only, etc.) are always
// applied; resource limits and cap re-additions are configurable via DockerOpts.
//
// The workspace mount (-v hostMount:workdir, added separately) is RW so the
// agent can write files. /tmp gets a writable tmpfs. The sandbox-home mount
// (added separately) is also RW for Go/Cargo caches. /etc gets a writable
// tmpfs overlay so ensurePasswdEntry can append the mapped uid (needed for
// ssh-keygen, whoami, git commit signing) under the read-only rootfs.
func dockerHardeningArgs(opts DockerOpts) []string {
	args := []string{
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--read-only",
		"--tmpfs=/tmp:rw,nosuid,nodev,size=100m",
		"--tmpfs=/etc:rw,nosuid,nodev,size=1m",
	}
	// Re-add specific capabilities if configured (e.g. CHOWN for go build).
	for _, cap := range opts.DockerCaps {
		args = append(args, "--cap-add="+cap)
	}
	if opts.DockerMemory != "" {
		args = append(args, "--memory="+opts.DockerMemory)
	}
	if opts.DockerPidsLimit > 0 {
		args = append(args, fmt.Sprintf("--pids-limit=%d", opts.DockerPidsLimit))
	}
	return args
}

func NewDockerExecutor(opts DockerOpts) (*DockerExecutor, error) {
	image, workdir, hostMount, dockerSock := opts.Image, opts.Workdir, opts.HostMount, opts.DockerSock
	if workdir == "" {
		workdir = "/work"
	}
	name := "wakil-session-" + fmt.Sprint(os.Getpid()) + "-" + randSuffix(6)
	// Best-effort remove a stale container from a previous crashed run.
	_ = exec.Command("docker", "rm", "-f", name).Run()

	args := []string{"run", "-d", "--name", name, "-w", workdir}
	args = append(args, dockerHardeningArgs(opts)...)
	if hostMount != "" {
		if err := os.MkdirAll(hostMount, 0o755); err != nil {
			return nil, fmt.Errorf("creating host workdir %s: %w", hostMount, err)
		}
		args = append(args, "-v", hostMount+":"+workdir)
	}
	if dockerSock {
		args = append(args, "-v", dockerSocketPath+":"+dockerSocketPath)
		// --user (below) drops supplementary groups, so the in-container
		// user loses the host docker-group membership that made the socket
		// accessible. Re-grant the socket's owning group explicitly.
		if gid, ok := fileGid(dockerSocketPath); ok {
			args = append(args, "--group-add", fmt.Sprint(gid))
		}
	}
	// SSH commit signing: mount the host agent socket at a fixed neutral path
	// and inject git config via GIT_CONFIG_* env ("command" scope). The
	// private key never enters the container — signatures are requested
	// through the agent socket. See internal/exec/signing.go.
	args = append(args, signingEnv(opts.Signing)...)

	// kvr staging mount + env vars. The staging mount is host-side at
	// opts.StagingMount, in-container at /run/kvr-staging. Both the UDS
	// socket and the snapshot file live on this mount so the host-side
	// Go client can reach the socket and the snapshot persists across
	// container restarts.
	kvrEnabled := opts.KVREnabled && opts.StagingMount != ""
	const stagingContainerPath = "/run/kvr-staging"
	if kvrEnabled {
		if err := os.MkdirAll(opts.StagingMount, 0o700); err != nil {
			return nil, fmt.Errorf("creating staging dir %s: %w", opts.StagingMount, err)
		}
		args = append(args, "-v", opts.StagingMount+":"+stagingContainerPath)
		args = append(args,
			"-e", "KVR_SOCKET_PATH="+stagingContainerPath+"/kvr.sock",
			"-e", "KVR_SNAPSHOT_PATH="+stagingContainerPath+"/staging.kvr",
			"-e", "KVR_SNAPSHOT_ON_SHUTDOWN=true",
			"-e", fmt.Sprintf("KVR_MAX_ENTRIES=%d", opts.KVRMaxEntries),
			"-e", fmt.Sprintf("KVR_SWEEP_INTERVAL_SECS=%d", opts.KVRSweepIntervalSecs),
			"-e", fmt.Sprintf("KVR_SNAPSHOT_INTERVAL_SECS=%d", opts.KVRSnapshotIntervalSecs),
		)
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

	// Container command: when kvr is enabled, use the entrypoint script
	// (which starts kvr-server in the background, then runs the main command
	// and traps SIGTERM for graceful kvr shutdown). Otherwise, use the
	// plain "sleep infinity" as before.
	if kvrEnabled {
		args = append(args, image, "/usr/local/bin/wakil-entrypoint.sh", "sleep", "infinity")
	} else {
		args = append(args, image, "sleep", "infinity")
	}

	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker run failed: %s", strings.TrimSpace(string(out)))
	}
	d := &DockerExecutor{
		container: name, image: image, workspaceRoot: workdir, hostMount: hostMount,
		dockerSock: dockerSock, signing: opts.Signing.Enabled, generation: 1,
		stagingMount: opts.StagingMount,
	}
	if hostMount == "" {
		_, _ = d.execCtx(context.Background(), false, "sh", "-c", "mkdir -p "+shQuote(workdir))
	}
	if uid := os.Getuid(); uid > 0 {
		ensurePasswdEntry(name, uid, os.Getgid())
	}
	if dockerSock {
		ensureDockerCLI(name)
	}

	// Host-side kvr readiness check: PING the UDS socket. On success, wire
	// the socket path. On failure, warn and continue (kvr is an enhancement,
	// not a hard dependency — staging tools report "staging unavailable").
	if kvrEnabled {
		kvrSocket := filepath.Join(opts.StagingMount, "kvr.sock")
		d.kvrSocket = kvrSocket
		if err := waitForKVR(kvrSocket, 5*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "kvr: staging store not ready (staging unavailable): %v\n", err)
			d.kvrSocket = ""
			d.kvrAvailable = false
		} else {
			d.kvrAvailable = true
		}
	}

	return d, nil
}

// ensurePasswdEntry appends an /etc/passwd entry for the mapped host uid if
// none exists. Images have no user for arbitrary --user uids; tools that
// resolve the current user (ssh-keygen -Y sign, whoami, git commit signing)
// fail with "No user exists for uid N" otherwise. Runs as root via docker
// exec -u 0; best-effort.
func ensurePasswdEntry(container string, uid, gid int) {
	script := fmt.Sprintf(
		"getent passwd %d >/dev/null 2>&1 || echo 'user:x:%d:%d:sandbox user:/home/user:/bin/sh' >> /etc/passwd",
		uid, uid, gid)
	_ = exec.Command("docker", "exec", "-u", "0", container, "sh", "-c", script).Run()
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

// execCtx runs docker exec with the given args, using CommandContext so the
// call is cancelled when ctx is. All DockerExecutor methods that shell into
// the container go through this helper.
func (d *DockerExecutor) execCtx(ctx context.Context, interactive bool, args ...string) (string, error) {
	prefix := []string{"exec"}
	if interactive {
		prefix = append(prefix, "-i")
	}
	full := append(prefix, append([]string{d.container}, args...)...)
	cmd := exec.CommandContext(ctx, "docker", full...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (d *DockerExecutor) RunShell(ctx context.Context, command string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "exec", d.container, "sh", "-c", runFromRoot(d.workspaceRoot, command))
	out, err := cmd.CombinedOutput()
	return strings.TrimRight(string(out), "\r\n"), err
}

func (d *DockerExecutor) ReadFile(ctx context.Context, path string) (string, error) {
	// cd into workspaceRoot first so relative paths resolve from the project root.
	out, err := d.execCtx(ctx, false, "sh", "-c", "cd "+shQuote(d.workspaceRoot)+` && cat -- "$1"`, "sh", path)
	if err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(out))
	}
	return out, nil
}

func (d *DockerExecutor) StatFile(ctx context.Context, path string) (int64, error) {
	out, err := d.execCtx(ctx, false, "sh", "-c", "cd "+shQuote(d.workspaceRoot)+` && stat -c %s -- "$1"`, "sh", path)
	if err != nil {
		return 0, fmt.Errorf("%s", strings.TrimSpace(out))
	}
	var size int64
	if _, scanErr := fmt.Sscan(strings.TrimSpace(out), &size); scanErr != nil {
		return 0, fmt.Errorf("unexpected stat output: %q", strings.TrimSpace(out))
	}
	return size, nil
}

func (d *DockerExecutor) ListDir(ctx context.Context, path string) (string, error) {
	if path == "" {
		path = "."
	}
	// ls -Ap: all entries except . and .., with a trailing / on directories.
	out, err := d.execCtx(ctx, false, "sh", "-c", "cd "+shQuote(d.workspaceRoot)+` && ls -Ap -- "$1"`, "sh", path)
	if err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(out))
	}
	return out, nil
}

func (d *DockerExecutor) WriteFile(ctx context.Context, path, content string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", d.container, "sh", "-c",
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
func (d *DockerExecutor) KVRSocketPath() string { return d.kvrSocket }
func (d *DockerExecutor) KVRAvailable() bool    { return d.kvrAvailable }
func (d *DockerExecutor) Describe() string {
	sock := ""
	if d.dockerSock {
		sock = " +docker"
	}
	if d.signing {
		sock += " +sign"
	}
	if d.kvrAvailable {
		sock += " +kvr"
	} else if d.stagingMount != "" {
		sock += " +kvr(off)"
	}
	if d.hostMount != "" {
		return fmt.Sprintf("docker[%s → %s]%s", d.image, d.hostMount, sock)
	}
	return fmt.Sprintf("docker[%s]%s", d.image, sock)
}
func (d *DockerExecutor) Close() error {
	// Graceful teardown: docker stop sends SIGTERM to PID 1 (the entrypoint),
	// which traps it and signals kvr-server for shutdown (snapshot save).
	// -t 10 gives kvr up to 10s to finish the snapshot before SIGKILL.
	// Always run stop (even if kvr readiness failed) because the entrypoint
	// may have started kvr-server regardless — the graceful window is a
	// ceiling, docker stop returns when PID 1 exits.
	_ = exec.Command("docker", "stop", "-t", "10", d.container).Run()
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
	toolsOnce    sync.Once // guards the probe: executor is shared with concurrent subagents
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

func (e *DirectExecutor) StatFile(_ context.Context, path string) (int64, error) {
	info, err := os.Stat(e.resolve(path))
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

func (e *DirectExecutor) ReadFile(_ context.Context, path string) (string, error) {
	b, err := os.ReadFile(e.resolve(path))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func (e *DirectExecutor) ListDir(_ context.Context, path string) (string, error) {
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

func (e *DirectExecutor) WriteFile(_ context.Context, path, content string) (string, error) {
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
func (e *DirectExecutor) KVRSocketPath() string { return "" }
func (e *DirectExecutor) KVRAvailable() bool    { return false }
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

// pathToURI converts a filesystem path to a file:// URI with proper percent-encoding.
func pathToURI(path string) string {
	u := url.URL{Scheme: "file", Path: path}
	return u.String()
}

// uriToPath extracts the filesystem path from a file:// URI, decoding
// percent-encoding.
func uriToPath(uri string) (string, error) {
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("parsing URI %q: %w", uri, err)
	}
	if u.Scheme != "file" {
		return "", fmt.Errorf("not a file:// URI: %q", uri)
	}
	return u.Path, nil
}
