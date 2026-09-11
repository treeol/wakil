//go:build !windows

package agent

import (
	"os"
	"syscall"
)

// processAliveImpl uses os.FindProcess + Signal(0) on Unix to check if the
// process exists. Signal 0 doesn't send a signal — it just checks existence.
func processAliveImpl(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err == nil {
		return true
	}
	return false
}
