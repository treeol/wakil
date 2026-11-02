// Package hisn implements sandbox.Backend on top of the hisn CLI
// (github.com/treeol/hisn, >= 0.2.0). Wakil drives it exclusively through the
// command line: one detached session per chat, many `hisn exec` calls.
package hisn

import (
	"context"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// clientEnvAllow lists the variables a hisn CLI process may inherit from
// Wakil. Everything else (API keys, tokens, the rest of the host environment)
// is dropped: hisn scrubs again on its side, but Wakil must not rely on that.
var clientEnvAllow = map[string]bool{
	"PATH": true, "HOME": true, "XDG_RUNTIME_DIR": true,
	"LANG": true, "TZ": true, "TERM": true,
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
		pass = b.opts.EnvPass
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

// output runs a hisn command to completion and returns its combined output.
func (b *Backend) output(ctx context.Context, withPass bool, args ...string) (string, error) {
	out, err := b.command(ctx, withPass, args...).CombinedOutput()
	return string(out), err
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

// supervisorProc is the long-lived `hisn run --detach` process. In hisn the
// supervisor IS the session: it holds the namespace fds and the control socket,
// and `--detach` only means "no main command". It is therefore the session's
// lifetime handle and, because it is the exact process Wakil spawned, an
// instance-bound identity (no pid or id can have been reused while it is
// unreaped).
type supervisorProc struct {
	cmd     *osexec.Cmd
	stderr  *ringBuffer
	logPath string             // per-chat supervisor log (truncated at every start)
	cancel  context.CancelFunc // cancels the SESSION-OWNED context of cmd
	exited  chan struct{}      // closed as soon as cmd.Wait returned (no log I/O before it)
	logged  chan struct{}      // closed once the exit note is logged and the log closed
	waitErr error              // valid once exited is closed
	// killFn delivers SIGKILL. A field only so tests can simulate a supervisor
	// that cannot be killed (uninterruptible sleep); production never sets it.
	killFn func()
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
func (b *Backend) startSupervisor(logPath string, notes []string, args ...string) (*supervisorProc, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o700); err != nil {
		return nil, fmt.Errorf("supervisor log directory: %w", err)
	}
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		// Diagnostics are not optional: a supervisor whose stderr vanishes with
		// the process is exactly what made earlier failures undiagnosable.
		return nil, fmt.Errorf("supervisor log %s: %w", logPath, err)
	}
	ring := newRingBuffer(supervisorStderrCap)
	tee := newTeeWriter(ring, f)
	for _, n := range notes {
		_, _ = fmt.Fprintf(tee, "wakil: %s\n", n)
	}
	_, _ = fmt.Fprintf(tee, "wakil: starting %s %s\n", b.opts.Binary, strings.Join(args, " "))

	supCtx, supCancel := context.WithCancel(context.Background())
	cmd := b.command(supCtx, true, args...)
	cmd.SysProcAttr = supervisorSysProcAttr()
	cmd.Stdin = nil  // /dev/null
	cmd.Stdout = nil // /dev/null; hisn run prints nothing on stdout
	cmd.Stderr = tee
	cmd.WaitDelay = supervisorWaitDelay
	p := &supervisorProc{cmd: cmd, stderr: ring, logPath: logPath, cancel: supCancel, exited: make(chan struct{}), logged: make(chan struct{})}
	started := make(chan error, 1)
	go func() {
		// Pdeathsig is delivered when the OS THREAD that forked the child
		// exits, not when the process does, and the Go runtime may retire an
		// idle thread at any time. Pin this goroutine (and so its thread) for
		// the supervisor's whole life: the same goroutine starts and reaps it,
		// blocked in cmd.Wait, and only then lets the thread go.
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
		// Publish the death FIRST: health checks read `exited`, and no log I/O
		// (a stalled disk, a full volume) may delay them or the cleanup that
		// follows. The exit note goes to the log afterwards; error messages that
		// quote the log wait for `logged` with a short bound.
		supCancel()
		close(p.exited)
		_, _ = fmt.Fprintf(tee, "wakil: supervisor exited: %s\n", exitText(p.waitErr))
		tee.close()
		close(p.logged)
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return p, nil
}

func exitText(err error) string {
	if err == nil {
		return "exit status 0"
	}
	return err.Error()
}

func (p *supervisorProc) hasExited() bool {
	select {
	case <-p.exited:
		return true
	default:
		return false
	}
}

// waitExit reports whether the supervisor exited within d.
func (p *supervisorProc) waitExit(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-p.exited:
		return true
	case <-t.C:
		return false
	}
}

// kill SIGKILLs the supervisor: cancelling its session-owned context makes the
// runtime kill it, and the explicit Kill makes that independent of scheduling.
// Its init dies through its own PDEATHSIG. Killing an already-reaped process is
// a harmless no-op.
func (p *supervisorProc) kill() {
	if p.killFn != nil {
		p.killFn()
		return
	}
	p.cancel()
	_ = p.cmd.Process.Kill()
}

// exitStatus describes how the supervisor ended ("exit status 125",
// "signal: killed", "exit status 0"), or "still running".
func (p *supervisorProc) exitStatus() string {
	if !p.hasExited() {
		return "still running"
	}
	return exitText(p.waitErr)
}

func (p *supervisorProc) pid() int { return p.cmd.Process.Pid }

// stderrText returns the captured stderr. After an exit it waits briefly (bounded;
// the log may be stalled) so the exit note is included.
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
