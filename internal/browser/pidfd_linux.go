//go:build linux

package browser

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// pidfdOpen returns a pidfd for pid, or -1 when the kernel (< 5.3) or the
// sandbox does not offer one. A pidfd names ONE process for its whole life: a
// signal sent through it can never hit a process that later reused the pid.
func pidfdOpen(pid int) int {
	fd, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		return -1
	}
	return fd
}

// pidfdSignal signals the process the pidfd refers to (ESRCH once it exited).
func pidfdSignal(fd int, sig syscall.Signal) error {
	return unix.PidfdSendSignal(fd, unix.Signal(sig), nil, 0)
}

func pidfdClose(fd int) { _ = unix.Close(fd) }
