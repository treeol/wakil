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
