package hisn

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/lsp"
)

// End-to-end: a real hisn session (the fake) behind a BackendExecutor, with
// the LSP manager probing for gopls through that session. The probe runs
// inside the session, so the result reflects the session's PATH.

// lspSessionEnv opens a fake hisn session and returns an LSP manager wired to
// it. gopls is placed on the session's PATH only when withGopls is true.
func lspSessionEnv(t *testing.T, withGopls bool) (*lsp.Manager, string) {
	t.Helper()
	fe := newFakeEnv(t)
	opts := fe.options()
	opts.Surface.Rootfs = "wakil-dev"
	// The session's PATH is the fake's Env PATH. It holds only a directory
	// with sh (what RunShell needs) plus, when asked, a fake gopls. That makes
	// the probe result a property of the session, not of the host.
	dir := t.TempDir()
	if err := os.Symlink(mustLookPath(t, "sh"), filepath.Join(dir, "sh")); err != nil {
		t.Fatal(err)
	}
	if withGopls {
		if err := os.WriteFile(filepath.Join(dir, "gopls"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	setSessionPATH(&opts, dir)
	b, err := Open(ctxT(t), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })

	ex := exec.NewBackendExecutor(b, exec.BackendOptions{Label: "hisn"})
	t.Cleanup(func() { _ = ex.Close() })
	ctx := ctxT(t)
	if err := ex.BindChat(ctx, "lsp-probe"); err != nil {
		t.Fatal(err)
	}

	return lsp.NewManager(ex, config.DefaultConfig(), "file:///ws"), "wakil-dev"
}

// Fake hisn without gopls: the LSP tool gets the actionable message naming the
// rootfs, not a crash report.
func TestLSPProbe_GoplsAbsentFromSession(t *testing.T) {
	mgr, rootfs := lspSessionEnv(t, false)
	_, err := mgr.EnsureServer(ctxT(t), "go")
	if err == nil {
		t.Fatal("want not-found error, got nil")
	}
	want := "gopls not found in the sandbox (rootfs " + rootfs + "); install it in the image"
	if err.Error() != want {
		t.Fatalf("error text:\n got: %q\nwant: %q", err.Error(), want)
	}
}

// With gopls present in the session, the probe passes and the launch proceeds
// (the fake session runs the command, so spawn reaches the initialize step
// and fails there, not at the probe).
func TestLSPProbe_GoplsPresentInSession(t *testing.T) {
	mgr, _ := lspSessionEnv(t, true)
	_, err := mgr.EnsureServer(ctxT(t), "go")
	// The fake gopls exits at once, so the launch proceeds and fails later in
	// the handshake. That failure, not a probe failure, proves the launch ran.
	if err == nil {
		t.Fatal("want a handshake failure from the exiting fake gopls, got nil")
	}
	if strings.Contains(err.Error(), "not found in the sandbox") {
		t.Fatalf("probe reported gopls missing although it is on PATH: %v", err)
	}
	if !strings.Contains(err.Error(), "initialize") && !strings.Contains(err.Error(), "spawn") &&
		!strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "closed") {
		t.Fatalf("expected a launch/handshake failure, got: %v", err)
	}
}

// setSessionPATH sets the PATH the fake session inherits, replacing the host's.
func setSessionPATH(opts *Options, dir string) {
	kept := opts.Env[:0:0]
	for _, kv := range opts.Env {
		if !strings.HasPrefix(kv, "PATH=") {
			kept = append(kept, kv)
		}
	}
	opts.Env = append(kept, "PATH="+dir)
}

func mustLookPath(t *testing.T, name string) string {
	t.Helper()
	p, err := osexec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
