package hisn

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/sandbox"
)

// TestValidateErrorNetworkRefusal: when hisn's profile validate output mentions
// "network" and "open", the error names the reason and how to switch back.
// Wakil never falls back to a wider mode on its own.
func TestValidateErrorNetworkRefusal(t *testing.T) {
	out := `error: network = "open" requires kernel >= 5.14 and ptrace_scope <= 1 (got 2)
hint: use network = "host" or --network host`
	err := validateError(errors.New("exit status 1"), out)
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "refused the open network mode") {
		t.Errorf("error does not name the refusal: %q", msg)
	}
	if !strings.Contains(msg, "/network host") {
		t.Errorf("error does not say how to switch back: %q", msg)
	}
}

// TestValidateErrorNonNetwork: a normal validate error passes through unchanged.
func TestValidateErrorNonNetwork(t *testing.T) {
	out := `error: E_RISK_ACK_REQUIRED`
	err := validateError(fmt.Errorf("exit status 1"), out)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "E_RISK_ACK") {
		t.Errorf("risk ack error not preserved: %q", err.Error())
	}
}

// TestStartOpenRefusalShowsHisnText: when hisn itself refuses to start an open
// session (the refusal comes out of `hisn run`, after validate passed), the
// user sees hisn's own line, how to switch back, and the full stderr — not a
// generic failure.
func TestStartOpenRefusalShowsHisnText(t *testing.T) {
	fe := newFakeEnv(t)
	b, err := openWith(t, fe, func(o *Options) { o.Network = "open" })
	if err != nil {
		t.Fatal(err)
	}
	fe.setMode("refuseopen")
	err = b.Start(ctxT(t), sandbox.Session{ID: "ro1", HostCwd: fe.cwd})
	if err == nil {
		t.Fatal("a refused open session must fail Start")
	}
	for _, want := range []string{
		`refusing to start network = "open": this kernel lacks SECCOMP_ADDFD (kernel >= 5.14 required)`,
		"hisn refused the open network mode",
		"/network host",
		"stderr:",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
}

// In host mode the same stderr is not reinterpreted as an open refusal.
func TestStartHostFailureNotLabelledOpenRefusal(t *testing.T) {
	fe := newFakeEnv(t)
	b, err := openWith(t, fe, nil)
	if err != nil {
		t.Fatal(err)
	}
	fe.setMode("refuseopen")
	err = b.Start(ctxT(t), sandbox.Session{ID: "ro2", HostCwd: fe.cwd})
	if err == nil {
		t.Fatal("expected failure")
	}
	if strings.Contains(err.Error(), "refused the open network mode") {
		t.Errorf("host-mode failure labelled as an open refusal: %v", err)
	}
}

// TestNetworkPrecedenceProjectOpenVersusUserHost pins what the code does today
// (documented in docs/sandbox-backends.md): a project file's "open" and the
// user's config "open" are merged by Open (open if either says so), but a later
// SetNetworkMode — the user's /network, /network reset or the restored per-repo
// choice — REPLACES the merged value for the next session start. So the user's
// /network host wins over the project's "open"; a project can still never
// widen, because "host" in the project file is refused and SetNetworkMode is
// only reachable from the user's own commands.
func TestNetworkPrecedenceProjectOpenVersusUserHost(t *testing.T) {
	fe := newFakeEnv(t)
	b, err := openWith(t, fe, func(o *Options) { o.Surface.Network = "open" }) // the project file's value
	if err != nil {
		t.Fatal(err)
	}
	if got := b.NetworkMode(); got != "open" {
		t.Fatalf("project open, no user choice: mode = %q, want open", got)
	}
	b.SetNetworkMode("host") // /network host
	if got := b.NetworkMode(); got != "host" {
		t.Errorf("after /network host: mode = %q, want host (the user's choice replaces the project's open)", got)
	}
	b.SetNetworkMode("open") // /network open
	if got := b.NetworkMode(); got != "open" {
		t.Errorf("after /network open: mode = %q, want open", got)
	}
	// /network reset pushes the user's own effective value (config or "host"),
	// which also replaces the project's open: the project file is not consulted
	// again until the next process start.
	b.SetNetworkMode("host")
	if got := b.NetworkMode(); got != "host" {
		t.Errorf("after /network reset to host: mode = %q, want host", got)
	}
}

// A project file can never widen: "host" there is refused at Open.
func TestProjectHostNetworkRefused(t *testing.T) {
	fe := newFakeEnv(t)
	_, err := openWith(t, fe, func(o *Options) { o.Surface.Network = "host" })
	if err == nil || !strings.Contains(err.Error(), "never widen") {
		t.Fatalf("project hisn.network=host must be refused, got %v", err)
	}
}
