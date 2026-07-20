package agent

import (
	"encoding/json"
	"testing"
)

// mcp_fuzz_test.go — Go native fuzz tests for PrettyArgs and ExtractMCPResult.
// CI runs the seed corpus (go test); longer fuzzing is manual.
//
// Invariants:
//   - panic-freedom on arbitrary byte strings
//   - PrettyArgs: if output differs from input, it must be valid JSON
//     (the function either returns raw input or pretty-prints a JSON object)

// FuzzPrettyArgs feeds random bytes to PrettyArgs and asserts that the output
// is either the raw input (for invalid JSON) or valid JSON (for valid input).
func FuzzPrettyArgs(f *testing.F) {
	seeds := []string{
		``,                             // empty
		`{}`,                           // empty object
		`{"key":"value"}`,              // simple object
		`{"nested":{"a":1}}`,           // nested object
		`[1,2,3]`,                      // array (not an object — returns raw)
		`"string"`,                     // string (not an object — returns raw)
		`42`,                           // number (not an object — returns raw)
		`true`,                         // bool (not an object — returns raw)
		`null`,                         // null (edge case)
		`{invalid`,                     // truncated JSON
		`{"key":}`,                     // invalid value
		`{"key":"val" extra}`,          // trailing garbage
		`{"a":"b","a":"c"}`,            // duplicate keys
		`{"unicode":"héllo世界"}`,        // unicode
		`{   "spaced"  :  "val"  }`,    // whitespace
		`{"deep":{"a":{"b":{"c":1}}}}`, // deep nesting
		"\x00\xff",                     // binary garbage
		`{"key":"` + string(make([]byte, 1000)) + `"}`, // long value
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, raw string) {
		// Panic-freedom.
		out := PrettyArgs(raw)

		// If the output differs from input, it must be valid JSON.
		if out != raw {
			if !json.Valid([]byte(out)) {
				t.Errorf("PrettyArgs changed input to invalid JSON: in=%q out=%q", raw, out)
			}
		}
	})
}
