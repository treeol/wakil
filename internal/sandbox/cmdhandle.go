package sandbox

import (
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"syscall"
	"time"
)

// KillGrace is how long a cancelled or killed client gets after SIGTERM before
// it is killed outright.
const KillGrace = 5 * time.Second

// CmdHandle is an ExecHandle backed by a host-side client process (docker exec,
// hisn exec). Its Pid is the pid of that client, not of anything in the sandbox.
type CmdHandle struct {
	cmd  *osexec.Cmd
	done chan struct{}
	info ExitInfo
	err  error
}

// StartCmd starts cmd, which the caller built with exec.CommandContext(ctx, …)
// and whose Stdin/Stdout/Stderr are already set. Cancelling ctx sends SIGTERM to
// the client and SIGKILL after KillGrace. A background goroutine waits for the
// process so it is always reaped.
func StartCmd(cmd *osexec.Cmd) (*CmdHandle, error) {
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = KillGrace
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", cmd.Path, err)
	}
	h := &CmdHandle{cmd: cmd, done: make(chan struct{})}
	go func() {
		werr := cmd.Wait()
		var ee *osexec.ExitError
		switch {
		case werr == nil:
			h.info = ExitInfo{Code: 0}
		case errors.As(werr, &ee):
			code := ee.ExitCode()
			if code < 0 { // killed by a signal: report 128+sig like a shell
				if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
					code = 128 + int(ws.Signal())
				} else {
					code = 255
				}
			}
			h.info = ExitInfo{Code: code}
		case errors.Is(werr, osexec.ErrWaitDelay):
			// The client exited but a descendant kept the pipes open.
			h.info = ExitInfo{Code: 0}
		default:
			h.err = werr
		}
		close(h.done)
	}()
	return h, nil
}

// Wait implements ExecHandle.
func (h *CmdHandle) Wait() (ExitInfo, error) {
	<-h.done
	return h.info, h.err
}

// Done is closed when the client has exited and been reaped.
func (h *CmdHandle) Done() <-chan struct{} { return h.done }

// Kill implements ExecHandle: SIGTERM now, SIGKILL after KillGrace.
func (h *CmdHandle) Kill() error {
	select {
	case <-h.done:
		return nil
	default:
	}
	p := h.cmd.Process
	if p == nil {
		return nil
	}
	if err := p.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	time.AfterFunc(KillGrace, func() {
		select {
		case <-h.done:
		default:
			_ = p.Kill()
		}
	})
	return nil
}

// Pid implements ExecHandle.
func (h *CmdHandle) Pid() int {
	if h.cmd.Process == nil {
		return 0
	}
	return h.cmd.Process.Pid
}

var _ ExecHandle = (*CmdHandle)(nil)
