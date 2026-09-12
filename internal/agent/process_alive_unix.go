//go:build !windows

package agent

import (
	"errors"
	"os"
	"syscall"
)

// processAliveImpl uses os.FindProcess + Signal(0) on Unix to check if the
// process exists. Signal 0 doesn't send a signal — it just checks existence.
// EPERM means the process exists but is owned by another user — treat it as
// alive to avoid pruning another user's worktree on shared/multi-user systems.
func processAliveImpl(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err == nil {
		return true
	}
	// EPERM means the process exists but we don't have permission to signal
	// it (typically owned by another user). Treat it as alive — pruning a
	// live process's worktree would be destructive.
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	return false
}
