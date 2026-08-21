package peercred

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestFromConn_UnixSocket(t *testing.T) {
	// On Linux, SO_PEERCRED should return the test process's own UID.
	// On other platforms, FromConn returns (false, nil) — skip the assertion.
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer ln.Close()

	connAccepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			close(connAccepted)
			return
		}
		connAccepted <- conn
	}()

	clientConn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatalf("dial unix: %v", err)
	}
	defer clientConn.Close()

	serverConn := <-connAccepted
	if serverConn == nil {
		t.Fatal("accept returned nil conn")
	}
	defer serverConn.Close()

	creds, ok, err := FromConn(serverConn)
	if err != nil {
		t.Fatalf("FromConn: %v", err)
	}

	if !ok {
		t.Skip("peercred not supported on this platform")
	}

	expectedUID := uint32(os.Getuid())
	if creds.UID != expectedUID {
		t.Errorf("UID = %d, want %d", creds.UID, expectedUID)
	}
}

func TestFromConn_TCPConn(t *testing.T) {
	// A TCP connection is not a Unix connection; FromConn should return
	// (false, nil) — not an error.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen tcp: %v", err)
	}
	defer ln.Close()

	connAccepted := make(chan net.Conn, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			close(connAccepted)
			return
		}
		connAccepted <- conn
	}()

	clientConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial tcp: %v", err)
	}
	defer clientConn.Close()
	_ = clientConn // keep conn open until server accepts

	serverConn := <-connAccepted
	if serverConn == nil {
		t.Fatal("accept returned nil conn")
	}
	defer serverConn.Close()

	creds, ok, err := FromConn(serverConn)
	if err != nil {
		t.Fatalf("FromConn on TCP: unexpected error: %v", err)
	}
	if ok {
		t.Errorf("FromConn on TCP: expected ok=false, got creds=%+v", creds)
	}
}

func TestFromConn_NilConn(t *testing.T) {
	// A nil conn should not panic — it should return (false, nil) or an error.
	_, ok, err := FromConn(nil)
	if ok {
		t.Error("expected ok=false for nil conn")
	}
	_ = err // nil or error is both acceptable; the caller checks ok
}

// Ensure context is not needed for the package but is referenced for
// potential future use.
var _ = context.Background
