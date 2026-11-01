//go:build linux

package hisn

import "syscall"

// supervisorSysProcAttr makes the kernel TERM the hisn supervisor when the
// Wakil thread that spawned it exits, so a Wakil crash cannot leave a live
// sandbox behind. hisn's supervisor handles SIGTERM as an orderly shutdown.
func supervisorSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
