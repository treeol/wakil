package lsp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
)

// probeExec is a fake executor whose RunShell answers the `command -v` probe
// from a set of present binaries. StartInteractive records that it was
// called, so tests can tell a launch attempt from a probe-only failure.
type probeExec struct {
	fakeLSPExec
	present    map[string]bool
	rootfs     string
	launches   int
	probeCalls []string
}

func (p *probeExec) RunShell(ctx context.Context, cmd string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	p.probeCalls = append(p.probeCalls, cmd)
	name := strings.TrimSuffix(strings.TrimPrefix(cmd, "command -v '"), "'")
	if p.present[name] {
		return "/usr/local/bin/" + name + "\n", nil
	}
	return "", &exitCodeErr{code: 1}
}

func (p *probeExec) SandboxRootfs() string { return p.rootfs }

func (p *probeExec) StartInteractive(_ context.Context, _ string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, int, error) {
	p.launches++
	return nil, nil, nil, 0, fmt.Errorf("launch stub: stop after probe")
}

type exitCodeErr struct{ code int }

func (e *exitCodeErr) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// The sandbox executor does not report a rootfs (direct mode, docker): the
// message falls back to "host toolchain" rather than naming a store entry.
func TestProbe_MissingBinary_NamesRootfs(t *testing.T) {
	ex := &probeExec{present: map[string]bool{}, rootfs: "wakil-dev"}
	mgr := NewManager(ex, config.DefaultConfig(), "file:///test")
	_, err := mgr.EnsureServer(context.Background(), "go")
	if err == nil {
		t.Fatal("want error for missing gopls, got nil")
	}
	want := "gopls not found in the sandbox (rootfs wakil-dev); install it in the image"
	if err.Error() != want {
		t.Fatalf("error text:\n got: %q\nwant: %q", err.Error(), want)
	}
	if ex.launches != 0 {
		t.Fatalf("launched %d time(s) after a failed probe; want 0", ex.launches)
	}
}

func TestProbe_MissingBinary_NoRootfs(t *testing.T) {
	ex := &probeExec{present: map[string]bool{}}
	mgr := NewManager(ex, config.DefaultConfig(), "file:///test")
	_, err := mgr.EnsureServer(context.Background(), "go")
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "not found in the sandbox (host toolchain)") {
		t.Fatalf("error text %q lacks the host-toolchain location", err.Error())
	}
	if strings.Contains(err.Error(), "crash") {
		t.Fatalf("missing binary must not read as a crash: %q", err.Error())
	}
}

// With the binary present the probe passes and the launch proceeds.
func TestProbe_PresentBinary_LaunchProceeds(t *testing.T) {
	ex := &probeExec{present: map[string]bool{"gopls": true}, rootfs: "wakil-dev"}
	mgr := NewManager(ex, config.DefaultConfig(), "file:///test")
	_, err := mgr.EnsureServer(context.Background(), "go")
	if ex.launches != 1 {
		t.Fatalf("launches = %d, want 1 (probe passed, so launch must run)", ex.launches)
	}
	// The launch stub fails, so the error is the spawn failure, not the probe's.
	if err == nil || !strings.Contains(err.Error(), "launch stub") {
		t.Fatalf("want the launch error through to the caller, got %v", err)
	}
	if strings.Contains(err.Error(), "not found") {
		t.Fatalf("present binary reported missing: %v", err)
	}
}

// A missing binary leaves the server Dead, so installing it and calling again
// re-probes and succeeds without restarting Wakil.
func TestProbe_RetryAfterInstall(t *testing.T) {
	ex := &probeExec{present: map[string]bool{}, rootfs: "wakil-dev"}
	mgr := NewManager(ex, config.DefaultConfig(), "file:///test")
	if _, err := mgr.EnsureServer(context.Background(), "go"); err == nil {
		t.Fatal("first call: want not-found error")
	}
	ex.present["gopls"] = true
	_, err := mgr.EnsureServer(context.Background(), "go")
	if ex.launches != 1 {
		t.Fatalf("after install, launches = %d, want 1", ex.launches)
	}
	if err == nil || strings.Contains(err.Error(), "not found") {
		t.Fatalf("second call should reach launch, got %v", err)
	}
}

// The probe honours cancellation rather than reporting "not found".
func TestProbe_CancelledContext_IsNotNotFound(t *testing.T) {
	ex := &probeExec{present: map[string]bool{}}
	mgr := NewManager(ex, config.DefaultConfig(), "file:///test")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := mgr.EnsureServer(ctx, "go")
	if err == nil {
		t.Fatal("want error")
	}
	var nb *missingBinaryError
	if errors.As(err, &nb) {
		t.Fatalf("cancelled probe reported as missing binary: %v", err)
	}
}
