//go:build !windows

package agent

import (
	"os/exec"
	"syscall"
	"time"
)

// setProcessGroupAndCancel configures the command to run in its own process
// group (Setpgid) and kills the entire group on context cancellation. This
// ensures that when a hook times out, the shell AND any child processes
// (e.g., sleep) are all killed, not just the sh parent.
//
// Unix-only: uses syscall.SysProcAttr{Setpgid: true} and syscall.Kill(-pid,
// SIGKILL), which are not available on Windows. The non-Windows build tag
// covers Linux, macOS, BSDs, and other Unix-likes.
func setProcessGroupAndCancel(cmd *exec.Cmd) {
	// Process group: when the timeout fires, kill the entire process group
	// (sh + any children like sleep), not just the sh process.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Use Cancel + WaitDelay (Go 1.20+) for coordinated timeout:
	// When the context expires, Cmd sends SIGKILL to the process group, and
	// WaitDelay forces the process to terminate within 2s of the signal.
	// Combined with Setpgid, this kills the whole process group.
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
}
