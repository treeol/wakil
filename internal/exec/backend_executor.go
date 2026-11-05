package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/treeol/wakil/internal/sandbox"
)

// BackendOptions configures NewBackendExecutor.
type BackendOptions struct {
	// Label names the backend in Describe ("hisn").
	Label string
	// GrantableRW are the prefixes under which rw grants outside the workspace
	// are allowed (policy only; the backend enforces its own).
	GrantableRW []string
	// ShellOutputCap bounds RunShell's retained output; <= 0 = default.
	ShellOutputCap int64
}

// shared is the state common to a BackendExecutor and its worktree children.
type shared struct {
	b      sandbox.Backend
	opts   BackendOptions
	grants *sandbox.GrantManager

	// life serialises lifecycle transitions (bind, reset, close, start) with
	// ADMISSION: an exec holds it shared only while it is handed to the backend
	// (not while it runs), transitions hold it exclusively. A call admitted for
	// one chat therefore can never start inside a replacement session.
	life      sync.RWMutex
	chatID    string
	startedID string
	gen       int
	closed    bool

	toolsOnce    sync.Once
	sandboxTools string
}

// BackendExecutor is an Executor built on any sandbox.Backend. It runs the
// same shell snippets the docker executor runs, one backend exec per call,
// inside a session that belongs to the bound chat.
type BackendExecutor struct {
	*shared
	root  string // agent-visible workspace root (the worktree dir for a child)
	child bool
}

var (
	_ Executor        = (*BackendExecutor)(nil)
	_ FileModer       = (*BackendExecutor)(nil)
	_ WorktreeHost    = (*BackendExecutor)(nil)
	_ ChatBinder      = (*BackendExecutor)(nil)
	_ SandboxResetter = (*BackendExecutor)(nil)
	_ AccessGranter   = (*BackendExecutor)(nil)
	_ SandboxStatuser = (*BackendExecutor)(nil)
	_ SessionEnsurer  = (*BackendExecutor)(nil)
	_ KVRPlanner      = (*BackendExecutor)(nil)
)

// NewBackendExecutor wraps b. The session is started lazily on the first call
// after a chat is bound (BindChat); nothing runs before that.
func NewBackendExecutor(b sandbox.Backend, opts BackendOptions) *BackendExecutor {
	if opts.Label == "" {
		opts.Label = "sandbox"
	}
	return &BackendExecutor{
		shared: &shared{
			b:      b,
			opts:   opts,
			grants: sandbox.NewGrantManager(b, b.AgentRoot(), opts.GrantableRW),
			gen:    1,
		},
		root: b.AgentRoot(),
	}
}

// ---- lifecycle ----

// callScope pins one logical operation to the (chat, generation) that was
// current when it began. A call whose scope no longer matches is rejected
// instead of being retried against whichever session happens to be current.
type callScope struct {
	chatID string
	gen    int
}

type scopeKey struct{}

func (s *shared) captureScope() *callScope {
	s.life.RLock()
	defer s.life.RUnlock()
	return &callScope{chatID: s.chatID, gen: s.gen}
}

// withScope returns ctx carrying a scope captured now, unless it already has one.
// Multi-step operations call it once so every step shares one identity.
func (s *shared) withScope(ctx context.Context) context.Context {
	if _, ok := ctx.Value(scopeKey{}).(*callScope); ok {
		return ctx
	}
	return context.WithValue(ctx, scopeKey{}, s.captureScope())
}

var errRebound = errors.New("sandbox session was replaced while the call was in flight; retry")

// matches reports whether sc is still the live identity. Caller holds life.
func (s *shared) matches(sc *callScope) bool {
	return sc == nil || (sc.chatID == s.chatID && sc.gen == s.gen)
}

// current reports whether a session for the bound chat is running. Caller
// holds life (shared or exclusive).
func (s *shared) current() bool {
	return !s.closed && s.chatID != "" && s.startedID == s.chatID
}

// startSession starts the session for the bound chat if it is not running.
func (s *shared) startSession(ctx context.Context, sc *callScope) error {
	s.life.Lock()
	defer s.life.Unlock()
	if s.closed {
		return errors.New("sandbox executor is closed")
	}
	if !s.matches(sc) {
		return errRebound
	}
	if s.chatID == "" {
		return errors.New("sandbox: no chat bound yet")
	}
	if s.startedID == s.chatID {
		return nil
	}
	if err := s.b.Start(ctx, sandbox.Session{ID: s.chatID, HostCwd: s.b.AgentRoot()}); err != nil {
		return fmt.Errorf("starting %s sandbox session: %w", s.opts.Label, err)
	}
	s.startedID = s.chatID
	// One ledger: a backend that restored grants on a replacement session owns
	// the truth, so the manager follows it instead of forgetting them (which
	// would turn a later Withdraw into a silent no-op). Other backends start
	// with no grants.
	if gl, ok := s.b.(sandbox.GrantLister); ok {
		s.grants.Sync(gl.Grants())
	} else {
		s.grants.Reset()
	}
	return nil
}

// admit runs fn (which must only HAND the work to the backend, not wait for
// it) while the current session is guaranteed to be the one for the bound
// chat, starting it first if needed. It never starts a session for any other
// chat and never after Close.
func (s *shared) admit(ctx context.Context, fn func() error) error {
	sc, _ := ctx.Value(scopeKey{}).(*callScope)
	if sc == nil {
		sc = s.captureScope()
	}
	for {
		s.life.RLock()
		if !s.matches(sc) {
			s.life.RUnlock()
			return errRebound
		}
		if s.current() {
			err := fn()
			s.life.RUnlock()
			if errors.Is(err, sandbox.ErrSessionDead) {
				// The session's supervisor is gone for good. This call fails
				// (it is not retried: it may not be idempotent); the NEXT call
				// starts a fresh session.
				s.markDead(sc)
			}
			return err
		}
		s.life.RUnlock()
		if err := s.startSession(ctx, sc); err != nil {
			return err
		}
	}
}

// markDead forgets the running session after the backend reported it dead, so
// the next admission starts a fresh one. The generation advances so background
// registry entries of the dead session are never trusted. A no-op when the
// identity captured by the failing call is no longer current (someone else
// already replaced or reset the session).
func (s *shared) markDead(sc *callScope) {
	s.life.Lock()
	defer s.life.Unlock()
	if s.closed || !s.matches(sc) || s.startedID != s.chatID {
		return
	}
	s.startedID = ""
	s.gen++
	s.grants.Reset()
}

// BindChat implements ChatBinder.
func (e *BackendExecutor) BindChat(ctx context.Context, chatID string) error {
	if e.child {
		return errors.New("a worktree executor cannot bind the shared sandbox session")
	}
	s := e.shared
	s.life.Lock()
	defer s.life.Unlock()
	if s.closed {
		return errors.New("sandbox executor is closed")
	}
	if chatID == s.chatID {
		return nil
	}
	s.chatID = chatID
	if s.startedID == "" {
		return nil
	}
	// A session of the previous chat is running: end it. Advance the
	// generation even when the stop fails, so stale background entries are
	// never trusted; the failed stop is retried by the next Start/Close.
	s.startedID = ""
	s.gen++
	s.grants.Reset()
	if err := s.b.Stop(ctx); err != nil {
		return fmt.Errorf("stopping previous %s session: %w", s.opts.Label, err)
	}
	return nil
}

// ResetSandbox implements SandboxResetter (used by /rewind).
func (e *BackendExecutor) ResetSandbox(ctx context.Context) error {
	if e.child {
		return errors.New("a worktree executor cannot reset the shared sandbox session")
	}
	s := e.shared
	s.life.Lock()
	defer s.life.Unlock()
	if s.closed || s.startedID == "" {
		return nil
	}
	s.startedID = ""
	s.gen++
	s.grants.Reset()
	return s.b.Stop(ctx)
}

// Close implements Executor. A child executor's Close is a no-op.
func (e *BackendExecutor) Close() error {
	if e.child {
		return nil
	}
	s := e.shared
	s.life.Lock()
	defer s.life.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.startedID = ""
	s.gen++
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return s.b.Close(ctx)
}

// Generation implements Executor.
func (e *BackendExecutor) Generation() int {
	e.life.RLock()
	defer e.life.RUnlock()
	return e.gen
}

// ---- running things ----

// result is one finished backend exec.
type result struct {
	out  string
	info sandbox.ExitInfo
}

// runOpts tunes one exec.
type runOpts struct {
	stdin  io.Reader
	capOut int64 // > 0: keep only the tail of the output
}

func (e *BackendExecutor) run(ctx context.Context, argv []string, o runOpts) (result, int64, error) {
	var (
		buf  bytes.Buffer
		tail *boundedTailBuffer
		w    io.Writer = &buf
	)
	if o.capOut > 0 {
		tail = newBoundedTailBuffer(o.capOut)
		w = tail
	}
	ctx = e.withScope(ctx) // one identity for admission AND a dead-session verdict
	var h sandbox.ExecHandle
	err := e.admit(ctx, func() (err error) {
		h, err = e.b.Exec(ctx, sandbox.ExecSpec{Argv: argv, Stdin: o.stdin, Stdout: w, Stderr: w})
		return err
	})
	if err != nil {
		return result{}, 0, err
	}
	info, werr := h.Wait()
	// The session may have died while this exec was in flight: forget it so the
	// next call starts a fresh one (the failing call is never retried).
	werr = e.noteDead(ctx, werr)
	var omitted int64
	out := buf.String()
	if tail != nil {
		out, omitted = tail.Snapshot()
	}
	if werr != nil {
		return result{out: out, info: info}, omitted, werr
	}
	return result{out: out, info: info}, omitted, nil
}

// exitErr converts a finished exec into the error docker's `docker exec` would
// have produced: nil on success, an "exit status N" error otherwise.
func (e *BackendExecutor) exitErr(r result) error {
	switch {
	case r.info.Lost:
		return fmt.Errorf("%s exec lost: the program may still be running inside the sandbox (it is stopped when the session ends)", e.opts.Label)
	case r.info.BackendError:
		return fmt.Errorf("%s error: %s", e.opts.Label, strings.TrimSpace(r.out))
	case r.info.Code != 0:
		return &ExitStatusError{Code: r.info.Code}
	}
	return nil
}

// sh runs `sh -c script args...` and returns the combined output.
func (e *BackendExecutor) sh(ctx context.Context, script string, args ...string) (string, error) {
	argv := append([]string{"sh", "-c", script, "sh"}, args...)
	if len(args) == 0 {
		argv = []string{"sh", "-c", script}
	}
	r, _, err := e.run(ctx, argv, runOpts{})
	if err != nil {
		return r.out, err
	}
	return r.out, e.exitErr(r)
}

// RunShell implements Executor.
func (e *BackendExecutor) RunShell(ctx context.Context, command string) (string, error) {
	r, omitted, err := e.run(ctx, []string{"sh", "-c", runFromRoot(e.root, command)}, runOpts{capOut: capOr(e.ShellOutputCap())})
	if err != nil {
		return r.out, err
	}
	xerr := e.exitErr(r)
	return shellResult(r.out, omitted, ctx.Err() != nil && xerr != nil), xerr
}

// ShellOutputCap returns the configured cap (0 = default).
func (e *BackendExecutor) ShellOutputCap() int64 { return e.opts.ShellOutputCap }

// ReadFile implements Executor.
func (e *BackendExecutor) ReadFile(ctx context.Context, path string) (string, error) {
	out, err := e.sh(ctx, "cd "+shQuote(e.root)+` && cat -- "$1"`, path)
	if err != nil {
		return "", e.fileErr(out, path, err)
	}
	return out, nil
}

// fileErr maps shell-level failures onto the docker executor's error shapes.
func (e *BackendExecutor) fileErr(out, path string, err error) error {
	var es *ExitStatusError
	if !errors.As(err, &es) {
		return err // backend / lost / transport error: keep its own text
	}
	msg := strings.TrimSpace(out)
	if isShellNotFound(msg) {
		return fmt.Errorf("%w: %s", ErrFileNotFound, path)
	}
	if msg == "" {
		return err
	}
	return fmt.Errorf("%s", msg)
}

// ReadFileBounded implements Executor.
func (e *BackendExecutor) ReadFileBounded(ctx context.Context, path string, maxBytes int64) (string, bool, error) {
	if maxBytes <= 0 {
		return "", false, fmt.Errorf("ReadFileBounded: maxBytes must be > 0, got %d", maxBytes)
	}
	out, err := e.sh(ctx, fmt.Sprintf("cd %s && head -c %d -- \"$1\"", shQuote(e.root), maxBytes+1), path)
	if err != nil {
		return "", false, e.fileErr(out, path, err)
	}
	truncated := int64(len(out)) > maxBytes
	if truncated {
		out = truncateInvalidUTF8Suffix(out[:maxBytes])
	}
	return out, truncated, nil
}

// StatFile implements Executor.
func (e *BackendExecutor) StatFile(ctx context.Context, path string) (int64, error) {
	out, err := e.sh(ctx, "cd "+shQuote(e.root)+` && stat -c %s -- "$1"`, path)
	if err != nil {
		return 0, e.fileErr(out, path, err)
	}
	var size int64
	if _, scanErr := fmt.Sscan(strings.TrimSpace(out), &size); scanErr != nil {
		return 0, fmt.Errorf("unexpected stat output: %q", strings.TrimSpace(out))
	}
	return size, nil
}

// ListDir implements Executor.
func (e *BackendExecutor) ListDir(ctx context.Context, path string) (string, error) {
	if path == "" {
		path = "."
	}
	out, err := e.sh(ctx, "cd "+shQuote(e.root)+` && ls -Ap -- "$1"`, path)
	if err != nil {
		return "", fmt.Errorf("%s", strings.TrimSpace(out))
	}
	return out, nil
}

func (e *BackendExecutor) write(ctx context.Context, path string, content io.Reader, n int) (string, error) {
	if err := guardWrite(e.root, path); err != nil {
		return "", err
	}
	script := "cd " + shQuote(e.root) + ` && mkdir -p "$(dirname -- "$1")" && cat > "$1"`
	r, _, err := e.run(ctx, []string{"sh", "-c", script, "sh", path}, runOpts{stdin: content})
	if err != nil {
		return "", err
	}
	if xerr := e.exitErr(r); xerr != nil {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(r.out), xerr)
	}
	return fmt.Sprintf("wrote %d bytes to %s", n, path), nil
}

// WriteFile implements Executor.
func (e *BackendExecutor) WriteFile(ctx context.Context, path, content string) (string, error) {
	return e.write(ctx, path, strings.NewReader(content), len(content))
}

// WriteFileBytes implements Executor.
func (e *BackendExecutor) WriteFileBytes(ctx context.Context, path string, content []byte) (string, error) {
	return e.write(ctx, path, bytes.NewReader(content), len(content))
}

// ReadFileTail implements Executor.
func (e *BackendExecutor) ReadFileTail(ctx context.Context, path string, maxBytes int64) (string, error) {
	if err := validateReadFileTailMaxBytes(maxBytes); err != nil {
		return "", err
	}
	out, err := e.sh(ctx, fmt.Sprintf("cd %s && tail -c %d -- \"$1\" 2>&1", shQuote(e.root), maxBytes), path)
	if err != nil {
		return "", fmt.Errorf("reading log: %s", strings.TrimSpace(out))
	}
	return out, nil
}

// ConfinePath implements Executor: readlink -f inside the sandbox, then a
// containment check against this executor's root.
func (e *BackendExecutor) ConfinePath(ctx context.Context, path string) (string, error) {
	if !strings.HasPrefix(path, "/") {
		path = e.root + "/" + path
	}
	out, err := e.sh(ctx, "readlink -f "+shQuote(path)+" 2>&1")
	if err != nil {
		return "", resolvePathError(path, out, err)
	}
	canonical := strings.TrimSpace(out)
	if canonical == "" {
		return "", fmt.Errorf("could not resolve path %q", path)
	}
	if !isInsideWorkspace(canonical, e.root) {
		return "", fmt.Errorf("path %q (→ %s) is outside workspace %q — traversal not allowed", path, canonical, e.root)
	}
	return canonical, nil
}

// DeletePath implements Executor.
func (e *BackendExecutor) DeletePath(ctx context.Context, path string) error {
	if err := guardRemove(e.root, path); err != nil {
		return err
	}
	script := fmt.Sprintf(
		`if [ -d %[1]s ] && [ ! -L %[1]s ]; then rmdir -- %[1]s 2>&1; else rm -- %[1]s 2>&1; fi`, shQuote(path))
	out, err := e.sh(ctx, script)
	if err != nil {
		msg := strings.TrimSpace(out)
		if strings.Contains(strings.ToLower(msg), "not empty") {
			return fmt.Errorf("directory is not empty — use run_shell rm -r to remove recursively")
		}
		if isShellNotFound(msg) {
			return fmt.Errorf("%w: %s", ErrFileNotFound, path)
		}
		if msg != "" {
			return fmt.Errorf("%s", msg)
		}
		return err
	}
	return nil
}

// MovePath implements Executor.
func (e *BackendExecutor) MovePath(ctx context.Context, src, dst string) error {
	if err := guardRemove(e.root, src, dst); err != nil {
		return err
	}
	ctx = e.withScope(ctx) // check and mutation must hit the same session
	chk, cerr := e.sh(ctx, fmt.Sprintf(`[ ! -e %[1]s ] && [ ! -L %[1]s ] && echo ok || echo exists`, shQuote(dst)))
	if cerr == nil && strings.TrimSpace(chk) == "exists" {
		return fmt.Errorf("destination already exists: %s — delete it first if you intended to overwrite", dst)
	}
	out, err := e.sh(ctx, fmt.Sprintf("mv -n -- %s %s 2>&1", shQuote(src), shQuote(dst)))
	if err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(out))
	}
	return nil
}

// ---- processes (all inside the sandbox; pids are sandbox-namespace values) ----

// StartBackground implements Executor.
func (e *BackendExecutor) StartBackground(ctx context.Context, command, logPath string) (pid, pgid int, err error) {
	script := fmt.Sprintf(`setsid nohup sh -c %s > %s 2>&1 & echo $!`, shQuote(command), shQuote(logPath))
	out, serr := e.sh(ctx, script)
	if serr != nil {
		return 0, 0, fmt.Errorf("starting background process: %s", strings.TrimSpace(out))
	}
	if n, _ := fmt.Sscan(strings.TrimSpace(out), &pid); n != 1 || pid <= 0 {
		return 0, 0, fmt.Errorf("unexpected PID output from background start: %q", strings.TrimSpace(out))
	}
	return pid, pid, nil
}

// KillPgid implements Executor.
func (e *BackendExecutor) KillPgid(ctx context.Context, pgid, sig int) error {
	_, err := e.sh(ctx, fmt.Sprintf("kill -%d -%d 2>/dev/null; exit 0", sig, pgid))
	return err
}

// IsProcessAlive implements Executor.
func (e *BackendExecutor) IsProcessAlive(ctx context.Context, pid int) bool {
	out, err := e.sh(ctx, procPIDAliveScript(pid))
	if err != nil {
		return false
	}
	state := strings.TrimSpace(out)
	return state == "?" || (state != "" && !strings.HasPrefix(state, "Z"))
}

// IsProcessGroupAlive implements Executor.
func (e *BackendExecutor) IsProcessGroupAlive(ctx context.Context, pgid int) bool {
	out, err := e.sh(ctx, procGroupAliveScript(pgid))
	return err == nil && strings.TrimSpace(out) == "yes"
}

// StartInteractive implements Executor. The returned pid is ALWAYS 0: the only
// pid available is the host-side client's, and every pid-based operation
// (IsProcessAlive/KillPgid) runs in the sandbox's pid namespace, where that
// number could name an unrelated process. Callers treat pid 0 as "no process
// guard"; the server is reaped by its shutdown sequence or when the session
// ends.
func (e *BackendExecutor) StartInteractive(ctx context.Context, command string) (
	stdin io.WriteCloser, stdout io.ReadCloser, stderr io.ReadCloser, pid int, err error,
) {
	// Real OS pipes, not io.Pipe: os/exec hands *os.File streams straight to
	// the child, so no copier goroutine exists whose completion Wait would
	// depend on (a caller that keeps stdin open or never drains stdout can no
	// longer delay or wedge process reaping).
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("stdin pipe: %w", err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		closeAll(inR, inW)
		return nil, nil, nil, 0, fmt.Errorf("stdout pipe: %w", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		closeAll(inR, inW, outR, outW)
		return nil, nil, nil, 0, fmt.Errorf("stderr pipe: %w", err)
	}
	var h sandbox.ExecHandle
	err = e.admit(ctx, func() (err error) {
		h, err = e.b.Exec(ctx, sandbox.ExecSpec{
			Argv: []string{"sh", "-c", command}, Cwd: e.root,
			Stdin: inR, Stdout: outW, Stderr: errW,
		})
		return err
	})
	if err != nil {
		closeAll(inR, inW, outR, outW, errR, errW)
		return nil, nil, nil, 0, fmt.Errorf("starting interactive process: %w", err)
	}
	// The backend may keep writing to outW/errW (and reading inR) until the
	// exec finishes, so the parent's copies of the CHILD-side ends are closed
	// only after Wait. stdin is an *os.File, so os/exec starts no stdin copier
	// and Wait cannot be held up by a caller that keeps stdin open; stdout and
	// stderr copiers are bounded by the client's WaitDelay if nobody reads.
	go func() {
		_, _ = h.Wait()
		closeAll(inR, outW, errW)
	}()
	return inW, outR, errR, 0, nil
}

func closeAll(fs ...*os.File) {
	for _, f := range fs {
		_ = f.Close()
	}
}

// ---- file modes ----

// FileMode implements FileModer.
func (e *BackendExecutor) FileMode(ctx context.Context, path string) (fs.FileMode, error) {
	r, _, err := e.run(ctx, []string{"stat", "-L", "-c", "%f", "--", path}, runOpts{})
	if err == nil {
		err = e.exitErr(r)
	}
	if err != nil {
		msg := strings.TrimSpace(r.out)
		if isShellNotFound(msg) {
			return 0, fmt.Errorf("%w: %s", ErrFileNotFound, path)
		}
		return 0, fmt.Errorf("stat %s: %s", path, msg)
	}
	return parseRawMode(strings.TrimSpace(r.out))
}

// Chmod implements FileModer.
func (e *BackendExecutor) Chmod(ctx context.Context, path string, mode fs.FileMode) error {
	r, _, err := e.run(ctx, []string{"chmod", fmt.Sprintf("%04o", uint32(mode.Perm())), "--", path}, runOpts{})
	if err == nil {
		err = e.exitErr(r)
	}
	if err != nil {
		return fmt.Errorf("chmod %s: %s", path, strings.TrimSpace(r.out))
	}
	return nil
}

// EntryExists implements FileModer.
func (e *BackendExecutor) EntryExists(ctx context.Context, path string) (bool, error) {
	r, _, err := e.run(ctx, []string{"sh", "-c", `[ -e "$1" ] || [ -L "$1" ]`, "sh", path}, runOpts{})
	if err != nil {
		return false, fmt.Errorf("probe %s: %w", path, err)
	}
	if r.info.Code == 0 && !r.info.Lost && !r.info.BackendError {
		return true, nil
	}
	if r.info.Code == 1 && !r.info.Lost && !r.info.BackendError {
		return false, nil
	}
	return false, fmt.Errorf("probe %s: %s: %w", path, strings.TrimSpace(r.out), e.exitErr(r))
}

// ---- identity & paths ----

// Cwd implements Executor.
func (e *BackendExecutor) Cwd() string { return e.root }

// WorkspaceRoot implements Executor.
func (e *BackendExecutor) WorkspaceRoot() string { return e.root }

// Describe implements Executor.
func (e *BackendExecutor) Describe() string {
	if e.child {
		return fmt.Sprintf("%s-wt[%s]", e.opts.Label, e.root)
	}
	return fmt.Sprintf("%s[%s]", e.opts.Label, e.root)
}

// KVRSocketPath implements Executor: the host path of an in-sandbox staging
// service's socket when the backend runs one and it answered (hisn), else ""
// — never a host-side kvr. A worktree child shares the parent's service.
func (e *BackendExecutor) KVRSocketPath() string {
	if k, ok := e.b.(sandbox.KVRProvider); ok {
		return k.KVRSocket()
	}
	return ""
}

// KVRAvailable implements Executor.
func (e *BackendExecutor) KVRAvailable() bool { return e.KVRSocketPath() != "" }

// ContainerName implements Executor: there is no container.
func (e *BackendExecutor) ContainerName() string { return "" }

// CDPPort implements Executor.
func (e *BackendExecutor) CDPPort() int { return 0 }

// SandboxTmpReadable reports that /tmp is the backend sandbox's own, not the
// host's (see SandboxTmpReader).
func (e *BackendExecutor) SandboxTmpReadable() bool { return true }

// HostBrowserAllowed is false: with ContainerName() == "" a browser manager
// would otherwise launch Chromium on the HOST, outside the sandbox.
func (e *BackendExecutor) HostBrowserAllowed() bool { return false }

// SandboxTools implements Executor: probed once, inside the sandbox.
func (e *BackendExecutor) SandboxTools() string {
	e.toolsOnce.Do(func() {
		backend := e.opts.Label
		if backend == "" {
			backend = "docker"
		}
		e.sandboxTools = probeTools(backend, func(script string) string {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			out, _ := e.sh(ctx, script)
			return out
		})
	})
	return e.sandboxTools
}

// HostPathToURI implements Executor via the backend path mapping.
func (e *BackendExecutor) HostPathToURI(hostPath string) (string, error) {
	if e.child {
		return "", fmt.Errorf("HostPathToURI not supported for worktree executor (no host mapping for %q)", e.root)
	}
	if !filepath.IsAbs(hostPath) {
		hostPath = filepath.Join(e.root, hostPath)
	}
	if err := e.hostIdentity(hostPath); err != nil {
		return "", err
	}
	// Map the host-resolved spelling: hostIdentity verified that spelling, and
	// it is the one that lies under the (resolved) host workspace. The alias
	// spelling would not, so UnmapPath would wrongly report it not visible.
	resolved := hostPath
	if r, rerr := resolveExistingAncestor(filepath.Clean(hostPath)); rerr == nil {
		resolved = r
	}
	agent, err := e.b.UnmapPath(resolved)
	if err != nil {
		return "", fmt.Errorf("path %q has no sandbox mapping: %w", hostPath, err)
	}
	return pathToURI(agent), nil
}

// URIToHostPath implements Executor via the backend path mapping.
func (e *BackendExecutor) URIToHostPath(uri string) (string, error) {
	if e.child {
		return "", fmt.Errorf("URIToHostPath not supported for worktree executor (no host mapping for %q)", e.root)
	}
	p, err := uriToPath(uri)
	if err != nil {
		return "", err
	}
	host, err := e.b.MapPath(p)
	if err != nil {
		return "", fmt.Errorf("URI %q has no host mapping: %w", uri, err)
	}
	if err := e.hostIdentity(host); err != nil {
		return "", err
	}
	return host, nil
}

// hostRootResolved returns the workspace's HOST directory with symlinks
// resolved, for host-side containment checks. e.root is a sandbox path and
// must not be compared with host paths. ok is false when the backend has no
// host mount (the check then falls through to the grant/UnmapPath test).
func (e *BackendExecutor) hostRootResolved() (string, bool) {
	// e.root is an AGENT path; MapPath is agent→host, UnmapPath is host→agent.
	ws, err := e.b.MapPath(e.root)
	if err != nil {
		return "", false
	}
	if r, rerr := filepath.EvalSymlinks(ws); rerr == nil {
		return r, true
	}
	return filepath.Clean(ws), true
}

// hostIdentity refuses a host path whose real location (after resolving
// symlinks on the HOST) lies outside the workspace: a symlink such as
// ws/link -> /tmp/x names a HOST object when opened by Wakil but the
// sandbox-private /tmp/x when resolved inside the sandbox, so the two would be
// different files with one name. Nonexistent tails are allowed (the deepest
// existing ancestor is checked).
func (e *BackendExecutor) hostIdentity(hostPath string) error {
	real, err := resolveExistingAncestor(filepath.Clean(hostPath))
	if err != nil {
		return fmt.Errorf("path %q cannot be resolved on the host: %w", hostPath, err)
	}
	if hr, ok := e.hostRootResolved(); ok && isInsideWorkspace(real, hr) {
		return nil
	}
	if _, err := e.b.UnmapPath(real); err == nil {
		return nil // an active grant is the same object on both sides
	}
	return fmt.Errorf("path %q resolves to %q outside the workspace on the host; the sandbox may see a different object there — refusing to map it", hostPath, real)
}

// ---- WorktreeHost ----

// WorktreeKind implements WorktreeHost: worktrees live in the sandbox-private
// temp dir and have no host representation.
func (e *BackendExecutor) WorktreeKind() WorktreeKind { return WorktreeSandboxTmp }

// IsWorktreeChild implements WorktreeHost.
func (e *BackendExecutor) IsWorktreeChild() bool { return e.child }

// SandboxTempDir implements WorktreeHost.
func (e *BackendExecutor) SandboxTempDir() string { return e.b.SandboxTempDir() }

// NewWorktreeExecutor implements WorktreeHost: a child re-rooted at dir that
// shares this executor's sandbox session and lifecycle.
func (e *BackendExecutor) NewWorktreeExecutor(dir string) (Executor, error) {
	if e.child {
		return nil, errNestedWorktree
	}
	return &BackendExecutor{shared: e.shared, root: filepath.Clean(dir), child: true}, nil
}

// ---- AccessGranter ----

// Grants returns the access manager's ledger (outside-workspace grants it
// believes are active), sorted by path.
func (e *BackendExecutor) Grants() []sandbox.GrantEntry { return e.grants.Grants() }

// EnsureAccess implements AccessGranter.
//
// The session is started/admitted first, but the (possibly slow) grant itself
// runs OUTSIDE the lifecycle lock: the backend binds the operation to its
// session instance and cancels it when that session is replaced, so a hung
// grant can neither block a rebind nor reach the replacement session.
func (e *BackendExecutor) EnsureAccess(ctx context.Context, path string, tier sandbox.Tier) error {
	ctx = e.withScope(ctx)
	if err := e.admit(ctx, func() error { return nil }); err != nil {
		return err
	}
	return e.noteDead(ctx, e.grants.Ensure(ctx, path, tier))
}

// WithdrawAccess implements AccessGranter.
func (e *BackendExecutor) WithdrawAccess(ctx context.Context, path string) error {
	ctx = e.withScope(ctx)
	if err := e.admit(ctx, func() error { return nil }); err != nil {
		return err
	}
	return e.noteDead(ctx, e.grants.Withdraw(ctx, path))
}

// noteDead forgets the session when a control operation found it dead, so the
// next call starts a fresh one (see markDead).
func (e *BackendExecutor) noteDead(ctx context.Context, err error) error {
	if errors.Is(err, sandbox.ErrSessionDead) {
		sc, _ := ctx.Value(scopeKey{}).(*callScope)
		e.markDead(sc)
	}
	return err
}

// SandboxStatus implements SandboxStatuser: the backend's own description when
// it has one, else just its name.
func (e *BackendExecutor) SandboxStatus(ctx context.Context) sandbox.Status {
	if r, ok := e.b.(sandbox.StatusReporter); ok {
		return r.SandboxStatus(ctx)
	}
	return sandbox.Status{Backend: e.opts.Label, Risk: "none"}
}

// EnsureSession implements SessionEnsurer: start the session for the bound
// chat if it is not running yet.
func (e *BackendExecutor) EnsureSession(ctx context.Context) error {
	return e.admit(ctx, func() error { return nil })
}

// PlannedKVRSocket implements KVRPlanner.
func (e *BackendExecutor) PlannedKVRSocket() string {
	if k, ok := e.b.(sandbox.KVRPlanner); ok {
		return k.PlannedKVRSocket()
	}
	return ""
}
