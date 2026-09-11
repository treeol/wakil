//go:build windows

package agent

import (
	"os/exec"
	"time"
)

// setProcessGroupAndCancel is the Windows fallback for hook process management.
// Windows does not support syscall.SysProcAttr{Setpgid} or syscall.Kill(-pid,
// SIGKILL). We rely on cmd.Cancel (which sends SIGKILL to the process on
// context cancellation via the Go runtime) and WaitDelay to force cleanup.
//
// Note: hooks run via "sh -c" which is not available on native Windows. Users
// on Windows should use WSL or a POSIX shell environment. This fallback
// ensures the code compiles on Windows even if hooks are non-functional
// without a POSIX shell.
func setProcessGroupAndCancel(cmd *exec.Cmd) {
	cmd.WaitDelay = 2 * time.Second
}
