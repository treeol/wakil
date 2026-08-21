//go:build !linux

// peercred_other.go: unsupported-platform stub for peercred (P4b).
//
// On non-Linux platforms, SO_PEERCRED is unavailable. The daemon must fail
// closed — it may not infer EmbeddedPrincipal from the absence of credentials.
// A future Darwin/BSD implementation using LOCAL_PEERCRED or getpeereid could
// replace this stub.

package peercred

import "net"

func fromConn(conn net.Conn) (Credentials, bool, error) {
	// On unsupported platforms, we still report whether this is a Unix
	// connection — the caller can distinguish "not a Unix conn" from
	// "Unix conn but no peercred support" if needed. For now, return
	// false in both cases; the daemon must fail closed.
	if _, ok := conn.(*net.UnixConn); !ok {
		return Credentials{}, false, nil // not a Unix connection
	}
	// Unix connection but peercred is unsupported on this platform.
	return Credentials{}, false, nil
}
