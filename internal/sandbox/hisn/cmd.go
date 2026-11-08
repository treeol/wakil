// Package hisn implements sandbox.Backend on top of the hisn CLI
// (github.com/treeol/hisn, >= 0.2.1). Wakil drives it through the command line
// (one detached session per chat) and the session control socket (one exec
// per tool call, via the ctl client).
package hisn

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// clientEnvAllow lists the variables a hisn CLI process may inherit from
// Wakil. Everything else (API keys, tokens, the rest of the host environment)
// is dropped: hisn scrubs again on its side, but Wakil must not rely on that.
var clientEnvAllow = map[string]bool{
	"PATH": true, "HOME": true, "XDG_RUNTIME_DIR": true,
	"LANG": true, "TZ": true, "TERM": true,
	// hisn's own trusted profile directory (a path, not a secret): needed to
	// resolve a named hisn.profile.
	"HISN_PROFILES": true,
}

func clientEnvAllowed(name string) bool {
	return clientEnvAllow[name] || strings.HasPrefix(name, "LC_")
}

// scrubbedEnv builds the environment for a hisn CLI process from base
// (KEY=VALUE entries). When pass is non-nil the named variables are copied
// too — only `hisn run` needs them, because hisn snapshots its environment at
// session start.
func scrubbedEnv(base []string, pass []string) []string {
	want := map[string]bool{}
	for _, n := range pass {
		want[n] = true
	}
	// Non-nil on purpose: exec.Cmd treats a nil Env as "inherit the parent's
	// environment", which would be the opposite of scrubbing.
	out := []string{}
	for _, kv := range base {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if clientEnvAllowed(name) || want[name] {
			out = append(out, kv)
		}
	}
	return out
}

// command is THE factory for every hisn invocation (version, check, policy
// validate, run, exec, grant, revoke, ls, kill, inspect). Nothing else in the
// package may call exec.Command, so no hisn process can inherit the host
// environment by accident (a test enforces this).
func (b *Backend) command(ctx context.Context, withPass bool, args ...string) *osexec.Cmd {
	cmd := osexec.CommandContext(ctx, b.opts.Binary, args...)
	var pass []string
	if withPass {
		// Copy EnvPass into a fresh slice before appending: append may
		// write into the backing array, and EnvPass is shared state.
		pass = append(append([]string(nil), b.opts.EnvPass...), "SSH_AUTH_SOCK")
	}
	cmd.Env = scrubbedEnv(b.baseEnv(), pass)
	return cmd
}

func (b *Backend) baseEnv() []string {
	if b.opts.Env != nil {
		return b.opts.Env
	}
	return os.Environ()
}

// output runs a hisn command to completion and returns its stdout. On success
// only stdout is returned, so JSON parsing (listSessions, verifyInstance) is
// not corrupted by a warning on stderr (e.g. "GOCOVERDIR not set" from a
// coverage-instrumented test binary whose env was scrubbed). On error,
// stdout and stderr are returned combined so callers that inspect the output
// for diagnostics (noteRefused, isConnRefused) still see stderr.
func (b *Backend) output(ctx context.Context, withPass bool, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := b.command(ctx, withPass, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		combined := stdout.String()
		if s := stderr.String(); s != "" {
			if combined != "" {
				combined += "\n"
			}
			combined += s
		}
		return combined, err
	}
	return stdout.String(), nil
}

// supervisorStderrCap bounds the in-memory copy of the supervisor's output.
const supervisorStderrCap = 64 << 10

// supervisorWaitDelay bounds how long Wait lingers for the output pipe after
// the supervisor exited (a straggler holding stderr must not wedge the reaper).
const supervisorWaitDelay = 2 * time.Second

// teeWriter sends supervisor output to the in-memory ring AND the per-chat log
// file. The ring is written synchronously; the file is written by a separate
// goroutine through a bounded queue, and chunks are DROPPED (counted, and noted
// in the log) when the queue is full. A stalled log destination (a hung network
// filesystem, a FIFO) must never block hisn's stderr: a supervisor stuck in a
// stderr write is a wedged session. A failing file never fails the write either
// (an error would end the stderr copier and hisn would die of SIGPIPE).
type teeWriter struct {
	ring    *ringBuffer
	q       chan []byte
	done    chan struct{} // closed when the file writer goroutine has finished
	dropped atomic.Int64
	once    sync.Once
	mu      sync.Mutex // guards q against send-after-close
	closed  bool
}

// logQueueLen bounds the chunks waiting for the file writer.
const logQueueLen = 256

func newTeeWriter(ring *ringBuffer, f *os.File) *teeWriter {
	t := &teeWriter{ring: ring, q: make(chan []byte, logQueueLen), done: make(chan struct{})}
	go func() {
		defer close(t.done)
		defer f.Close()
		failed := false
		for chunk := range t.q {
			if failed {
				continue
			}
			if _, err := f.Write(chunk); err != nil {
				failed = true
			}
		}
		if n := t.dropped.Load(); n > 0 && !failed {
			_, _ = fmt.Fprintf(f, "wakil: %d log chunk(s) dropped while the log was slow\n", n)
		}
	}()
	return t
}

func (t *teeWriter) Write(p []byte) (int, error) {
	_, _ = t.ring.Write(p)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return len(p), nil
	}
	select {
	case t.q <- append([]byte(nil), p...):
	default:
		t.dropped.Add(1)
	}
	return len(p), nil
}

// close ends the log: queued chunks are flushed when the file keeps up, and the
// wait is bounded so a stalled file cannot hold up lifecycle cleanup.
func (t *teeWriter) close() {
	t.once.Do(func() {
		t.mu.Lock()
		t.closed = true
		close(t.q)
		t.mu.Unlock()
	})
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	select {
	case <-t.done:
	case <-timer.C:
	}
}

// supervisorProc is the `hisn run --detach` process. In hisn <= 0.4.0 the
// supervisor IS the process: it holds the namespace fds and the control
// socket, and `--detach` only means "no main command". The process is the
// session's lifetime handle and, because it is the exact process Wakil
// spawned, an instance-bound identity (no pid or id can have been reused
// while it is unreaped).
//
// In hisn >= 0.4.1 the run process writes "ready\n" (and RISK lines on
// stderr), then EXITS 0: the supervisor lives on as a separate process.
// Its pid is <session-dir>/supervisor.pid (its log
// <session-dir>/supervisor.log). The run process's exit-0 is therefore a
// SUCCESS, not death; the supervisor is found by `hisn ls --json` (field
// dir → the session directory) and tracked by its own pid. The liveness
// methods below detect which mode is active: if the run process exits
// within the ready wait, the session is 0.4.1-style (detachExited=true); if
// it stays alive, it is 0.4.0-style (the process IS the supervisor).
type supervisorProc struct {
	cmd     *osexec.Cmd
	stderr  *ringBuffer
	stdout  string             // run process stdout (0.4.1: "<id> <name|-> ready")
	logPath string             // per-chat supervisor log (truncated at every start)
	cancel  context.CancelFunc // cancels the SESSION-OWNED context of cmd
	exited  chan struct{}      // closed as soon as cmd.Wait returned (no log I/O before it)
	logged  chan struct{}      // closed once the exit note is logged and the log closed
	waitErr error              // valid once exited is closed
	// killFn delivers SIGKILL. A field only so tests can simulate a supervisor
	// that cannot be killed (uninterruptible sleep); production never sets it.
	killFn func()

	// detachExited is true when the run process exited 0 (success) and the
	// supervisor lives on as a separate process (hisn >= 0.4.1). When true,
	// liveness is tracked by supPid/supStart (the REAL supervisor), not by
	// the run process's exited channel.
	detachExited bool
	supPid       int           // the real supervisor's pid (read from <dir>/supervisor.pid)
	supStart     string        // /proc start time of supPid; "" if unknown
	supExited    chan struct{} // closed when the real supervisor is confirmed gone
}

// supervisorLogPath is where chat id's supervisor output goes.
func (b *Backend) supervisorLogPath(id string) string {
	return filepath.Join(b.opts.StateDir, id+".supervisor.log")
}

// startSupervisor spawns the supervisor. Its lifetime is owned by the SESSION,
// never by a tool call: the command's context derives from context.Background()
// and is cancelled only by the supervisor's own teardown (kill), so cancelling
// the context of the call that happened to start the session cannot reach it.
// It also dies with Wakil through Pdeathsig.
//
// Stdio: stdin and stdout are explicitly nil (/dev/null) so no TUI descriptor
// can reach hisn; stderr is a pipe teed into a ring buffer and into logPath
// (truncated per start, so it always holds exactly the latest supervisor).
// notes are written first, as "wakil: ..." lines.
//
// In 0.4.1 stdout is NOT nil: the run process prints "<id> <name|-> ready"
// on stdout before exiting. It is captured into p.stdout for diagnostics.
//
// Readiness (hisn >= 0.2.1): `--ready-fd 3` with the WRITE end of a pipe as the
// child's fd 3 (ExtraFiles); hisn writes "ready\n" there once the session is
// live. Wakil closes its own write end immediately after a successful Start —
// EOF on the read end then means every writer (hisn included) is gone. The
// returned reader is drained by waitReady and closed by its caller.
func (b *Backend) startSupervisor(logPath, dir string, notes []string, args ...string) (*supervisorProc, *os.File, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, nil, fmt.Errorf("supervisor log directory: %w", err)
	}
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("supervisor log %s: %w", logPath, err)
	}
	ring := newRingBuffer(supervisorStderrCap)
	tee := newTeeWriter(ring, f)
	for _, n := range notes {
		_, _ = fmt.Fprintf(tee, "wakil: %s\n", n)
	}
	_, _ = fmt.Fprintf(tee, "wakil: starting %s %s\n", b.opts.Binary, strings.Join(args, " "))

	readyR, readyW, err := os.Pipe()
	if err != nil {
		return nil, nil, fmt.Errorf("readiness pipe: %w", err)
	}
	supCtx, supCancel := context.WithCancel(context.Background())
	cmd := b.command(supCtx, true, args...)
	cmd.SysProcAttr = supervisorSysProcAttr()
	// A neutral working directory: hisn also checks a profile against the
	// supervisor's PROCESS cwd (E_PROFILE_UNTRUSTED), and Wakil's own cwd is
	// normally the project. Empty = inherit (legacy --policy path).
	cmd.Dir = dir
	cmd.Stdin = nil // /dev/null
	// stdout is captured: in 0.4.1 the run process prints "<id> <name|-> ready".
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = tee
	cmd.WaitDelay = supervisorWaitDelay
	cmd.ExtraFiles = []*os.File{readyW} // fd 3: --ready-fd 3
	p := &supervisorProc{cmd: cmd, stderr: ring, logPath: logPath, cancel: supCancel,
		exited: make(chan struct{}), logged: make(chan struct{}),
		supExited: make(chan struct{})}
	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := cmd.Start()
		started <- err
		if err != nil {
			supCancel()
			close(p.exited)
			tee.close()
			close(p.logged)
			return
		}
		p.waitErr = cmd.Wait()
		p.stdout = stdout.String()
		supCancel()
		close(p.exited)
		_, _ = fmt.Fprintf(tee, "wakil: supervisor exited: %s\n", exitText(p.waitErr))
		tee.close()
		close(p.logged)
	}()
	if err := <-started; err != nil {
		_ = readyR.Close()
		_ = readyW.Close()
		return nil, nil, err
	}
	_ = readyW.Close()
	return p, readyR, nil
}

func exitText(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}

func (p *supervisorProc) hasExited() bool {
	if p.detachExited {
		// The run process exited (0.4.1). The session lives on as the
		// supervisor process; liveness is tracked by supPid/supExited.
		if p.supPid > 0 && procAlive(p.supPid, p.supStart) {
			return false
		}
		return true
	}
	select {
	case <-p.exited:
		return true
	default:
		return false
	}
}

// waitExit reports whether the run process (0.4.0: the supervisor) exited
// within d. In 0.4.1 the run process already exited (detachExited), so
// liveness is the supervisor's: check it via supExited.
func (p *supervisorProc) waitExit(d time.Duration) bool {
	if p.detachExited {
		// Poll the pid directly (not only supExited, whose watcher polls
		// slowly): a stopped supervisor is seen within a few milliseconds.
		deadline := time.Now().Add(d)
		for {
			if !procAlive(p.supPid, p.supStart) {
				return true
			}
			select {
			case <-p.supExited:
				return true
			default:
			}
			if !time.Now().Before(deadline) {
				return false
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-p.exited:
		return true
	case <-t.C:
		return false
	}
}

// runExitedOK reports whether the run process has exited with status 0 (the
// hisn >= 0.4.1 "supervisor is up" signal), whether or not it is tracked yet.
func (p *supervisorProc) runExitedOK() bool {
	select {
	case <-p.exited:
		return p.cmd != nil && p.waitErr == nil
	default:
		return false
	}
}

// kill SIGKILLs the supervisor. In 0.4.0 this is the run process (it IS the
// supervisor). In 0.4.1 the run process already exited; the supervisor is a
// separate process found by pid. Killing an already-reaped process is a
// harmless no-op.
func (p *supervisorProc) kill() {
	if p.killFn != nil {
		p.killFn()
		return
	}
	if p.detachExited {
		// Signal only the exact process tracked: a pid whose start time is
		// unknown (or no longer matches) is never signalled.
		if p.supPid > 0 && p.supStart != "" && procStart(p.supPid) == p.supStart {
			_ = syscall.Kill(p.supPid, syscall.SIGKILL)
		}
		return
	}
	if p.cmd != nil && p.cmd.Process != nil {
		p.cancel()
		_ = p.cmd.Process.Kill()
	}
}

// exitStatus describes how the run process or the real supervisor ended
// ("exit status 125", "signal: killed", "exit status 0"), or "still running".
// In 0.4.1 "still running" means the real supervisor is still alive.
func (p *supervisorProc) exitStatus() string {
	if p.detachExited {
		if p.supPid > 0 && procAlive(p.supPid, p.supStart) {
			return "still running"
		}
		if p.supPid > 0 {
			return fmt.Sprintf("supervisor pid %d exited", p.supPid)
		}
		return exitText(p.waitErr)
	}
	if !p.hasExited() {
		return "still running"
	}
	return exitText(p.waitErr)
}

func (p *supervisorProc) pid() int {
	if p.detachExited && p.supPid > 0 {
		return p.supPid
	}
	if p.cmd != nil && p.cmd.Process != nil {
		return p.cmd.Process.Pid
	}
	return 0
}

// stderrText returns the captured stderr. After an exit it waits briefly
// (bounded; the log may be stalled) so the exit note is included.
func (p *supervisorProc) stderrText() string {
	if p.hasExited() {
		t := time.NewTimer(500 * time.Millisecond)
		select {
		case <-p.logged:
		case <-t.C:
		}
		t.Stop()
	}
	return p.stderr.String()
}

// procAlive reports whether pid is running AND its /proc start time matches
// start (when start is non-empty). A pid of 0 is not alive. A zombie (exited,
// not yet reaped) is not alive. When the pid's /proc entry cannot be read for a
// reason that does not prove the process is gone (no procfs, a pid namespace
// mismatch, permissions), it is considered alive: we cannot prove otherwise.
// This mirrors ownerAlive's conservative handling, except for one case that IS
// provable: see procOps.alive.
func procAlive(pid int, start string) bool { return realProcOps.alive(pid, start) }

// procOps are the two kernel reads procAlive depends on, injectable so a test
// can pin the window in which a zombie is reaped between them.
type procOps struct {
	signal0  func(pid int) error               // kill(pid, 0)
	readFile func(path string) ([]byte, error) // os.ReadFile
}

var realProcOps = procOps{
	signal0:  func(pid int) error { return syscall.Kill(pid, 0) },
	readFile: os.ReadFile,
}

// alive implements procAlive. kill(pid, 0) alone cannot tell a zombie from a
// running process, so /proc/<pid>/stat is read next; a zombie's reparented
// init may reap it between the two. That leaves kill(0) saying "exists" and the
// stat entry missing, which must NOT read as "cannot prove it is gone": with a
// working procfs (/proc/self/stat readable), a vanished entry (ENOENT, or ESRCH
// when the reap lands between open and read) plus a second kill(0) answering
// ESRCH proves the process is gone.
func (o procOps) alive(pid int, start string) bool {
	if pid <= 0 {
		return false
	}
	if err := o.signal0(pid); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	state, cur, err := o.stat(pid)
	if err != nil {
		if procEntryGone(err) && o.procfsReadable() && errors.Is(o.signal0(pid), syscall.ESRCH) {
			return false // reaped between kill(0) and the stat read
		}
		return true // cannot prove it is gone
	}
	if state == 'Z' || state == 'X' {
		return false // exited, not yet reaped by its (new) parent
	}
	return start == "" || cur == "" || cur == start
}

// procEntryGone reports whether err from reading /proc/<pid>/stat says the
// process no longer exists: ENOENT when the entry is already gone at open, ESRCH
// when the open succeeded on a task that was reaped before the read.
func procEntryGone(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

// procfsReadable reports whether a procfs is mounted at /proc, so that a
// missing /proc/<pid> entry means the pid is gone rather than that /proc is
// absent.
func (o procOps) procfsReadable() bool {
	_, err := o.readFile("/proc/self/stat")
	return err == nil
}

// stat reads /proc/<pid>/stat: the process state letter (field 3) and the
// start time in clock ticks (field 22, "" if the line is too short). A
// malformed line is an error that is not ErrNotExist.
func (o procOps) stat(pid int) (state byte, start string, err error) {
	data, err := o.readFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, "", err
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, "", errors.New("malformed /proc stat line")
	}
	f := strings.Fields(s[i+1:])
	if len(f) == 0 {
		return 0, "", errors.New("malformed /proc stat line")
	}
	if len(f) >= 20 {
		start = f[19]
	}
	return f[0][0], start, nil
}

// procZombie reports whether pid is a zombie (/proc/<pid>/stat state Z or X).
// A detached supervisor is reparented; until its new parent reaps it, a
// finished one still answers kill(pid, 0).
func procZombie(pid int) bool {
	state, _, err := realProcOps.stat(pid)
	return err == nil && (state == 'Z' || state == 'X')
}

// trackSupervisor finds the real supervisor process for a 0.4.1 detach session
// and arms the supervisorProc to track it. The session directory comes from
// `hisn ls --json` (field dir); the supervisor pid is read from
// <dir>/supervisor.pid. A background goroutine polls the supervisor pid and
// closes supExited when it is gone. Call this after waitReady detects the
// run process has exited (detachExited).
func (b *Backend) trackSupervisor(ctx context.Context, p *supervisorProc, id string) error {
	dir, err := b.sessionDir(ctx, id)
	if err != nil {
		return fmt.Errorf("hisn 0.4.1: cannot find session directory for %s: %w", id, err)
	}
	pidFile := filepath.Join(dir, "supervisor.pid")
	var pidBytes []byte
	deadline := time.Now().Add(min(b.readyTimeout, 2*time.Second))
	for {
		pidBytes, err = os.ReadFile(pidFile)
		if err == nil && len(strings.TrimSpace(string(pidBytes))) > 0 {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			if err != nil {
				return fmt.Errorf("hisn 0.4.1: reading supervisor.pid for %s: %w", id, err)
			}
			return fmt.Errorf("hisn 0.4.1: supervisor.pid for %s is empty: %w", id, ctx.Err())
		}
		time.Sleep(50 * time.Millisecond)
	}
	pid, start, err := parseSupervisorPid(string(pidBytes))
	if err != nil {
		return fmt.Errorf("hisn 0.4.1: %w for %s", err, id)
	}
	if cur := procStart(pid); start == "" {
		start = cur
	} else if cur != "" && cur != start {
		return fmt.Errorf("hisn 0.4.1: supervisor pid %d for %s was reused (start time %s, file says %s)", pid, id, cur, start)
	}
	p.detachExited = true
	p.supPid = pid
	p.supStart = start
	go func() {
		for {
			if !procAlive(p.supPid, p.supStart) {
				close(p.supExited)
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
	}()
	return nil
}

// parseSupervisorPid parses <session-dir>/supervisor.pid. hisn 0.4.1 writes
// "<pid> <starttime>" (starttime is /proc/<pid>/stat field 22, the same value
// procStart returns); a bare "<pid>" is accepted too, with start "".
func parseSupervisorPid(s string) (pid int, start string, err error) {
	f := strings.Fields(s)
	if len(f) == 0 || len(f) > 2 {
		return 0, "", fmt.Errorf("invalid supervisor pid %q", strings.TrimSpace(s))
	}
	pid, err = strconv.Atoi(f[0])
	if err != nil || pid <= 0 {
		return 0, "", fmt.Errorf("invalid supervisor pid %q", strings.TrimSpace(s))
	}
	if len(f) == 2 {
		if _, err := strconv.ParseUint(f[1], 10, 64); err != nil {
			return 0, "", fmt.Errorf("invalid supervisor start time %q", strings.TrimSpace(s))
		}
		start = f[1]
	}
	return pid, start, nil
}

// sessionDir returns the session directory for id from `hisn ls --json`.
func (b *Backend) sessionDir(ctx context.Context, id string) (string, error) {
	m, err := b.listSessions(ctx)
	if err != nil {
		return "", err
	}
	e, ok := m[id]
	if !ok {
		return "", fmt.Errorf("session %s not found", id)
	}
	return e.Dir, nil
}
