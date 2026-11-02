//go:build linux

package hisn

import "syscall"

// supervisorSysProcAttr isolates the hisn supervisor from Wakil's terminal and
// ties its life to Wakil's:
//
//   - Pdeathsig=SIGTERM: the kernel TERMs the supervisor when the OS thread that
//     forked it exits (see startSupervisor, which pins that thread), so a Wakil
//     crash cannot leave a live sandbox behind. hisn's supervisor treats SIGTERM
//     as an orderly shutdown.
//   - Setsid: a new session and process group with NO controlling terminal. The
//     tty-generated SIGINT/SIGHUP/SIGTSTP that reach Wakil's foreground process
//     group (Ctrl-C, closing the terminal tab) must not reach the supervisor,
//     which treats SIGINT/SIGHUP as "shut the session down". Wakil ends it
//     explicitly (Stop) or through Pdeathsig.
func supervisorSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM, Setsid: true}
}
