// Package peercred provides platform-conditional extraction of Unix-socket
// peer credentials (SO_PEERCRED on Linux). It is used by the wakild daemon's
// local auth path (P4b) to resolve the connecting process's UID, which is then
// mapped to a core.Principal by the principal resolver.
//
// On Linux, SO_PEERCRED returns the UID, GID, and PID of the connecting
// process as of connect() time. These values are stable for the connection's
// lifetime.
//
// On unsupported platforms, FromConn returns (0, 0, false, nil) — the caller
// must fail closed, not infer an identity from the absence of credentials.
package peercred

import "net"

// Credentials holds the peer credentials of a Unix-socket connection.
type Credentials struct {
	UID uint32
	GID uint32
	PID int32
}

// FromConn extracts peer credentials from a Unix-socket connection. Returns
// (creds, true, nil) on success, (zero, false, nil) when credentials are
// unavailable (non-Unix connection or unsupported platform), and (zero, false,
// err) on a syscall failure during extraction.
//
// The caller MUST treat (false, nil) as "no identity available" and fail
// closed — never infer an identity from the absence of credentials.
func FromConn(conn net.Conn) (Credentials, bool, error) {
	return fromConn(conn)
}
