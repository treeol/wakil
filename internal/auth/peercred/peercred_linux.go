//go:build linux

// peercred_linux.go: SO_PEERCRED extraction on Linux (P4b).
//
// Uses golang.org/x/sys/unix.GetsockoptUcred via the connection's raw fd.
// SO_PEERCRED returns the UID, GID, and PID of the connecting process as of
// connect() time. The values are stable for the connection's lifetime.

package peercred

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

func fromConn(conn net.Conn) (Credentials, bool, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return Credentials{}, false, nil // not a Unix connection
	}

	rawConn, err := uc.SyscallConn()
	if err != nil {
		return Credentials{}, false, fmt.Errorf("peercred: get syscall conn: %w", err)
	}

	var ucred *unix.Ucred
	var sockErr error
	// The control function runs in the conn's network thread. We capture the
	// result via closure; the fd is valid only within the callback.
	err = rawConn.Control(func(fd uintptr) {
		ucred, sockErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	})
	if err != nil {
		return Credentials{}, false, fmt.Errorf("peercred: control fd: %w", err)
	}
	if sockErr != nil {
		return Credentials{}, false, fmt.Errorf("peercred: getsockopt SO_PEERCRED: %w", sockErr)
	}
	if ucred == nil {
		return Credentials{}, false, fmt.Errorf("peercred: nil ucred")
	}
	return Credentials{
		UID: ucred.Uid,
		GID: ucred.Gid,
		PID: ucred.Pid,
	}, true, nil
}
