package hisn

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestValidateErrorNetworkRefusal: when hisn's profile validate output mentions
// "network" and "open", the error names the reason and how to switch back.
// Wakil never falls back to a wider mode on its own.
func TestValidateErrorNetworkRefusal(t *testing.T) {
	out := `error: network = "open" requires kernel >= 5.9 and ptrace_scope <= 1 (got 2)
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
