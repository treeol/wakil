// Package hisn implements sandbox.Backend on top of the hisn CLI
// (github.com/treeol/hisn, >= 0.2.0). Wakil drives it exclusively through the
// command line: one detached session per chat, many `hisn exec` calls.
package hisn

import (
	"context"
	"os"
	osexec "os/exec"
	"runtime"
	"strings"
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

// supervisorStderrCap bounds the captured supervisor output.
const supervisorStderrCap = 64 << 10

// supervisorWaitDelay bounds how long Wait lingers for the output pipe after
// the supervisor exited (a straggler holding stderr must not wedge the reaper).
const supervisorWaitDelay = 2 * time.Second

// supervisorProc is the long-lived `hisn run --detach` process. In hisn the
// supervisor IS the session: it holds the namespace fds and the control socket,
// and `--detach` only means "no main command". It is therefore the session's
// lifetime handle and, because it is the exact process Wakil spawned, an
// instance-bound identity (no pid or id can have been reused while it is
// unreaped).
type supervisorProc struct {
	cmd     *osexec.Cmd
	stderr  *ringBuffer
	exited  chan struct{} // closed after cmd.Wait returned
	waitErr error         // valid once exited is closed
	// killFn delivers SIGKILL. A field only so tests can simulate a supervisor
	// that cannot be killed (uninterruptible sleep); production never sets it.
	killFn func()
}

// startSupervisor spawns the supervisor with the same scrubbed environment as
// every hisn invocation (plus env_pass, which hisn snapshots at session start).
// It deliberately has NO context: the process outlives the call that started
// it, and dies with Wakil through Pdeathsig.
func (b *Backend) startSupervisor(args ...string) (*supervisorProc, error) {
	// The factory's ctx-less form: Background never cancels, so the supervisor
	// outlives the call that started it.
	cmd := b.command(context.Background(), true, args...)
	cmd.SysProcAttr = supervisorSysProcAttr()
	ring := newRingBuffer(supervisorStderrCap)
	cmd.Stdout, cmd.Stderr = ring, ring // one interface value: one pipe, one copier
	cmd.WaitDelay = supervisorWaitDelay
	p := &supervisorProc{cmd: cmd, stderr: ring, exited: make(chan struct{})}
	started := make(chan error, 1)
	go func() {
		// Pdeathsig is delivered when the OS THREAD that forked the child
		// exits, and the Go runtime may retire an idle thread at any time. Pin
		// this goroutine (and so its thread) for the supervisor's whole life:
		// the same goroutine starts and reaps it, and only then lets go.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		err := cmd.Start()
		started <- err
		if err != nil {
			close(p.exited)
			return
		}
		p.waitErr = cmd.Wait()
		close(p.exited)
	}()
	if err := <-started; err != nil {
		return nil, err
	}
	return p, nil
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

// kill SIGKILLs the supervisor. Its init dies through its own PDEATHSIG.
// Killing an already-reaped process is a harmless no-op (os.ErrProcessDone).
func (p *supervisorProc) kill() {
	if p.killFn != nil {
		p.killFn()
		return
	}
	_ = p.cmd.Process.Kill()
}

// exitStatus describes how the supervisor ended ("exit status 125",
// "signal: killed", "exit status 0"), or "still running".
func (p *supervisorProc) exitStatus() string {
	if !p.hasExited() {
		return "still running"
	}
	if p.waitErr == nil {
		return "exit status 0"
	}
	return p.waitErr.Error()
}

func (p *supervisorProc) pid() int { return p.cmd.Process.Pid }

func (p *supervisorProc) stderrText() string { return p.stderr.String() }
