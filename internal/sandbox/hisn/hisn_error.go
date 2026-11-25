package hisn

import (
	"encoding/json"
	"fmt"
	"strings"
)

// hisn-error/v1 is the structured error object hisn's `--json` commands print
// on stderr (0.6.3+, recorded from the real 0.6.5 binary):
//
//	{"code":"E_STOP","message":"no such session nonexistent-xyz","schema":"hisn-error/v1"}
//
// Wakil passes --json only where it already parses stdout on success
// (ls --json, inspect --json, profile explain). Everywhere else the error
// arrives as plain "hisn: …" text, which stays byte-identical: parseHisnError
// only matches a WHOLE-output object, so a hisn-error object inside multi-line
// supervisor output is never swallowed.

// hisnError is one parsed hisn-error/v1 object.
type hisnError struct {
	Code    string
	Message string
}

// parseHisnError parses out as a whole-output hisn-error/v1 object. The second
// return is false for anything else (plain text, multi-line output, JSON of
// another schema), so callers fall back to today's raw-text handling.
func parseHisnError(out string) (hisnError, bool) {
	s := strings.TrimSpace(out)
	if !strings.HasPrefix(s, "{") {
		return hisnError{}, false
	}
	var raw struct {
		Schema  string `json:"schema"`
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(s), &raw); err != nil {
		return hisnError{}, false
	}
	if raw.Schema != "hisn-error/v1" || raw.Code == "" {
		return hisnError{}, false
	}
	return hisnError{Code: raw.Code, Message: raw.Message}, true
}

// IsTooLarge reports the 0.6.5 request-size refusal (E_TOO_LARGE): the exec
// request exceeded the supervisor's line limit and was refused pre-send.
func (e hisnError) IsTooLarge() bool { return e.Code == "E_TOO_LARGE" }

func (e hisnError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("hisn: %s", e.Code)
	}
	return fmt.Sprintf("hisn: %s: %s", e.Code, e.Message)
}

// ownHisnError renders out as "hisn: CODE: message" when out is a whole-output
// hisn-error/v1 object. Plain output passes through byte-identical (the
// DO-NOT-ALTER rule: raw stderr text unchanged when no error object is
// present). The bool reports whether an object was parsed.
func ownHisnError(out string) (string, bool) {
	if e, ok := parseHisnError(out); ok {
		return e.Error(), true
	}
	return out, false
}
