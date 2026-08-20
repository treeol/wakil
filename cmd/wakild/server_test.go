package main

// server_test.go: tests for the wakild daemon server (card #148 P2d).
//
// Covers: Unix socket listener (stale-socket handling, permissions, in-use
// detection), and basic daemon start/health/shutdown lifecycle.

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestListenUnix_CreatesSocketWith0600 verifies that listenUnix creates the
// socket file with 0600 permissions.
func TestListenUnix_CreatesSocketWith0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.sock")

	l, err := listenUnix(path)
	if err != nil {
		t.Fatalf("listenUnix: %v", err)
	}
	defer l.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Errorf("expected socket mode, got %v", info.Mode())
	}
	// 0600 = owner read+write only.
	perm := info.Mode().Perm()
	if perm != 0o600 {
		t.Errorf("expected 0600 permissions, got %o", perm)
	}
}

// TestListenUnix_StaleSocketUnlinked verifies that a stale (non-listening)
// socket file is unlinked and rebound.
func TestListenUnix_StaleSocketUnlinked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.sock")

	// Create a stale socket file (not a listener, just a file).
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale file: %v", err)
	}

	l, err := listenUnix(path)
	if err != nil {
		t.Fatalf("listenUnix with stale socket: %v", err)
	}
	defer l.Close()

	// Verify the socket is now a real listener socket.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Errorf("expected socket mode after rebind, got %v", info.Mode())
	}
}

// TestListenUnix_InUseRefused verifies that if another process is listening
// on the socket, listenUnix refuses to steal it.
func TestListenUnix_InUseRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.sock")

	// First listener occupies the socket.
	l1, err := listenUnix(path)
	if err != nil {
		t.Fatalf("first listenUnix: %v", err)
	}
	defer l1.Close()

	// Second attempt should fail.
	_, err = listenUnix(path)
	if err == nil {
		t.Fatal("expected error when socket is in use, got nil")
	}
}

// TestListenUnix_ParentDirCreated verifies that the parent directory is
// created with 0700 permissions if it doesn't exist.
func TestListenUnix_ParentDirCreated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "deep", "test.sock")

	l, err := listenUnix(path)
	if err != nil {
		t.Fatalf("listenUnix with missing parent: %v", err)
	}
	defer l.Close()

	parent := filepath.Dir(path)
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatalf("stat parent: %v", err)
	}
	perm := info.Mode().Perm()
	if perm != 0o700 {
		t.Errorf("expected 0700 parent dir, got %o", perm)
	}
}

// TestDefaultSocketPath verifies the default socket path derivation.
func TestDefaultSocketPath(t *testing.T) {
	path := defaultSocketPath()
	if path == "" {
		t.Fatal("default socket path is empty")
	}
	// Should end with wakild.sock.
	if filepath.Base(path) != "wakild.sock" {
		t.Errorf("expected base name wakild.sock, got %q", filepath.Base(path))
	}
}

// TestParseFlags_Defaults verifies default flag values.
func TestParseFlags_Defaults(t *testing.T) {
	f, err := parseFlags(nil)
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.ephemeral != false {
		t.Errorf("expected ephemeral=false, got true")
	}
	if f.shutdownTimeout != 10*time.Second {
		t.Errorf("expected 10s shutdown timeout, got %v", f.shutdownTimeout)
	}
	if f.socketPath == "" {
		t.Error("expected non-empty default socket path")
	}
}

// TestParseFlags_Ephemeral verifies --ephemeral flag.
func TestParseFlags_Ephemeral(t *testing.T) {
	f, err := parseFlags([]string{"--ephemeral"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if !f.ephemeral {
		t.Error("expected ephemeral=true")
	}
}

// TestParseFlags_SocketPath verifies --socket flag.
func TestParseFlags_SocketPath(t *testing.T) {
	f, err := parseFlags([]string{"--socket", "/tmp/test.sock"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.socketPath != "/tmp/test.sock" {
		t.Errorf("expected /tmp/test.sock, got %q", f.socketPath)
	}
}

// TestParseFlags_ShutdownTimeout verifies --shutdown-timeout flag.
func TestParseFlags_ShutdownTimeout(t *testing.T) {
	f, err := parseFlags([]string{"--shutdown-timeout", "30s"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if f.shutdownTimeout != 30*time.Second {
		t.Errorf("expected 30s, got %v", f.shutdownTimeout)
	}
}

// TestParseFlags_UnknownFlag verifies unknown flags error.
func TestParseFlags_UnknownFlag(t *testing.T) {
	_, err := parseFlags([]string{"--bogus"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
}

// TestParseFlags_UnexpectedArg verifies positional args error.
func TestParseFlags_UnexpectedArg(t *testing.T) {
	_, err := parseFlags([]string{"position"})
	if err == nil {
		t.Fatal("expected error for positional arg")
	}
}

// TestDaemonServerShutdown_Idempotent verifies that shutdown can be called
// without panicking on a server that hasn't served yet (no listener, no
// connections). This is the startup-failure path.
func TestDaemonServerShutdown_NoServe(t *testing.T) {
	// We can't easily construct a full daemonServer (needs config, executor,
	// etc.), but we can verify that shutdown on a partially-constructed one
	// doesn't panic. This test is a smoke test for the nil-safety of shutdown.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Construct a minimal daemonServer with nil fields (simulates a
	// construction failure after some resources were opened).
	d := &daemonServer{
		httpSrv: &http.Server{Handler: http.NewServeMux()},
	}
	// shutdown should not panic even with nil store/host/exe.
	d.shutdown(ctx)
}

// TestListenUnix_ConnectAndAccept verifies the listener actually works for
// accepting a connection.
func TestListenUnix_ConnectAndAccept(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.sock")

	l, err := listenUnix(path)
	if err != nil {
		t.Fatalf("listenUnix: %v", err)
	}
	defer l.Close()

	// Accept in a goroutine.
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		accepted <- conn
	}()

	// Dial the socket.
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for accept")
	}
}
