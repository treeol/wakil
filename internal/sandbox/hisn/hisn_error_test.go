package hisn

import (
	"errors"
	"strings"
	"testing"
)

// Recorded from the real hisn 0.6.5 binary (Block 3 capture): the error shape
// `--json` commands print on stderr, and the plain-text form without --json.
const (
	hisnErrStop    = `{"code":"E_STOP","message":"no such session nonexistent-xyz","schema":"hisn-error/v1"}`
	hisnErrRun     = `{"code":"E_RUN","message":"session.cwd: /tmp overlaps the hisn runtime directory /tmp/hr/hisn","schema":"hisn-error/v1"}`
	plainNoSession = `hisn: no such session nonexistent-xyz`
)

// TestParseHisnErrorRecorded proves the parser reads the exact shapes 0.6.5
// emits (and the legacy plain text stays plain).
func TestParseHisnErrorRecorded(t *testing.T) {
	for _, tc := range []struct{ in, code, msg string }{
		{hisnErrStop, "E_STOP", "no such session nonexistent-xyz"},
		{hisnErrRun, "E_RUN", "session.cwd: /tmp overlaps the hisn runtime directory /tmp/hr/hisn"},
	} {
		e, ok := parseHisnError(tc.in)
		if !ok {
			t.Fatalf("parseHisnError(%q): not recognised", tc.in)
		}
		if e.Code != tc.code || e.Message != tc.msg {
			t.Errorf("parseHisnError(%q) = %+v, want %s / %q", tc.in, e, tc.code, tc.msg)
		}
	}
	if _, ok := parseHisnError(plainNoSession); ok {
		t.Errorf("plain text %q must not parse as an error object", plainNoSession)
	}
	// A hisn-error object inside multi-line supervisor output must NOT be
	// swallowed: only a whole-output object counts.
	if _, ok := parseHisnError("hisn: starting session\n" + hisnErrStop); ok {
		t.Errorf("a hisn-error object inside multi-line output must not parse")
	}
	// E_TOO_LARGE is the 0.6.5 request-size refusal; it becomes one clear
	// tool error naming the limit (here the message carries it).
	too, ok := parseHisnError(`{"code":"E_TOO_LARGE","message":"exec request exceeds 65536 bytes","schema":"hisn-error/v1"}`)
	if !ok || !too.IsTooLarge() {
		t.Fatalf("E_TOO_LARGE not recognised: %+v ok=%v", too, ok)
	}
	if got := too.Error(); !strings.Contains(got, "E_TOO_LARGE") || !strings.Contains(got, "65536") {
		t.Errorf("E_TOO_LARGE error %q must name the code and the limit", got)
	}
}

// TestOwnHisnErrorFormatting: an unmapped hisn-error object renders as
// "hisn: CODE: message" (code first, message verbatim). Plain output passes
// through byte-identical (DO-NOT-ALTER: raw stderr unchanged on no object).
func TestOwnHisnErrorFormatting(t *testing.T) {
	if got, ok := ownHisnError(hisnErrStop); !ok || got != "hisn: E_STOP: no such session nonexistent-xyz" {
		t.Errorf("ownHisnError(stop) = %q ok=%v", got, ok)
	}
	raw := "hisn: injected failure: cannot create sandbox"
	if got, ok := ownHisnError(raw); ok || got != raw {
		t.Errorf("plain %q must pass through unchanged, got %q ok=%v", raw, got, ok)
	}
}

// TestValidateErrorHisnErrorObject: a profile-validate error object surfaces as
// code+message, and the existing code-typed matchers (E_RISK_ACK_, network/open)
// keep first claim on plain text (byte-identical legacy behaviour).
func TestValidateErrorHisnErrorObject(t *testing.T) {
	obj := `{"code":"E_PROFILE_NOTFOUND","message":"profile \"x\" not found","schema":"hisn-error/v1"}`
	err := validateError(errors.New("exit status 125"), obj)
	if err == nil || !strings.Contains(err.Error(), "E_PROFILE_NOTFOUND") || !strings.Contains(err.Error(), `profile "x" not found`) {
		t.Errorf("validateError(object) = %v, want code+message", err)
	}
}
