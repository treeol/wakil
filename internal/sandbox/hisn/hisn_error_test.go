package hisn

import (
	"errors"
	"strings"
	"testing"
)

// Recorded from the real hisn 0.6.5 binary (Block 3 capture): the error shape
// `--json` commands print on stderr, and the plain-text form without --json.
// The 0.6.7 binary emits the same shapes (no wire change, PROTOCOL_REVISION
// stays 4): recorded from the real 0.6.7 binary during the pre-capture
// (`inspect --session nosuch --json` → plain `hisn: no such session …`,
// exit 125; `profile validate --json` shapes in hisnProfileValidation067).
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

// hisnProfileValidation067 is the exact `profile validate --json` output of
// the real hisn 0.6.7 binary (pre-capture) for a profile whose
// [policy.session] network is "bogusmode": one E_PROFILE_PARSE error,
// ok: false, schema hisn-profile-validation/v1, exit 125. The 0.6.7
// "all post-parse errors" change could not be exercised — every dual-defect
// profile tried (bad variant + unknown key, bad type + unknown key, two
// unknown sections) still returns exactly one TOML-parse error, as the TOML
// deserializer fails fast. So the multi-error behaviour is changelog-trust,
// and this fixture pins the single-error shape that is real.
const hisnProfileValidation067 = `{
  "errors": [
    {
      "code": "E_PROFILE_PARSE",
      "column": 11,
      "line": 5,
      "message": "TOML parse error at line 5, column 11\n  |\n5 | network = \"bogusmode\"\n  |           ^^^^^^^^^^^\nunknown variant ` + "`bogusmode`" + `, expected one of ` + "`none`, `host`, `open`" + `\n"
    }
  ],
  "ok": false,
  "schema": "hisn-profile-validation/v1",
  "warnings": []
}
`

// TestProfileValidation067Fixture: the recorded 0.6.7 validate output carries
// the unknown-variant text Wakil's network/open matcher keys on, and the
// schema marker the --json parser expects — the parser reads the same shape
// as before even though errors are now (in principle) a list.
func TestProfileValidation067Fixture(t *testing.T) {
	if !strings.Contains(hisnProfileValidation067, `"schema": "hisn-profile-validation/v1"`) {
		t.Errorf("recorded output lacks the schema marker")
	}
	if !strings.Contains(hisnProfileValidation067, "unknown variant `bogusmode`") {
		t.Errorf("recorded output lacks the variant text")
	}
	if !strings.Contains(hisnProfileValidation067, "E_PROFILE_PARSE") {
		t.Errorf("recorded output lacks the error code")
	}
}

// TestValidateError067Shape: Wakil validates without --json, so the failure
// reaches validateError as the plain text form the 0.6.7 binary prints
// (pre-capture, exit 125) — the text preserves the unknown-variant wording
// byte-identically (DO-NOT-ALTER), and the E_PROFILE_PARSE code stays
// reachable if a future caller opts into --json.
func TestValidateError067Shape(t *testing.T) {
	text := "hisn: parsing profile x.toml: profile parse: TOML parse error at line 5, column 11\n  |\n5 | network = \"bogusmode\"\n  |           ^^^^^^^^^^^\nunknown variant `bogusmode`, expected one of `none`, `host`, `open`\n"
	err := validateError(errors.New("exit status 125"), text)
	if err == nil || !strings.Contains(err.Error(), "unknown variant `bogusmode`") {
		t.Errorf("validateError(0.6.7 text) = %v, want the variant text intact", err)
	}
}
