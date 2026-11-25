package hisn

import (
	"strings"
	"testing"
)

// The open-mode kernel floor: hisn 0.6.2+ requires Linux 6.7 (Landlock ABI
// v4) for `network = "open"`; an older kernel must be named at Preflight, not
// discovered as a supervisor refusal at Start.
func TestCheckOpenKernelFloor(t *testing.T) {
	for _, tc := range []struct {
		name, release, mode string
		ok                  bool
	}{
		{"new host ok", "7.3.4-300.fc45.x86_64", "open", true},
		{"6.7 boundary ok", "6.7.0", "open", true},
		{"6.6 refused", "6.6.15", "open", false},
		{"6.6.15-arch1 refused", "6.6.15-arch1-1", "open", false},
		{"kernel 5 refused", "5.14.0-300.fc45.x86_64", "open", false},
		{"10.0 treated as 10 not 1.9", "10.0.0", "open", true},
		{"9.9 refused", "9.9.0", "open", false},
		{"host mode never checked", "5.14.0", "host", true},
		{"empty mode never checked", "5.14.0", "", true},
		{"unparseable fails closed for open", "weird", "open", false},
		{"unreadable fails open too", "", "open", false},
	} {
		err := checkOpenKernelFloor(tc.release, tc.mode)
		if (err == nil) != tc.ok {
			t.Errorf("%s: checkOpenKernelFloor(%q,%q) err=%v, want ok=%v", tc.name, tc.release, tc.mode, err, tc.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "6.7") {
			t.Errorf("%s: error %q must name the 6.7 requirement", tc.name, err)
		}
		if err != nil && tc.release != "" && tc.release != "weird" && !strings.Contains(err.Error(), tc.release) {
			t.Errorf("%s: error %q must name the found kernel %q", tc.name, err, tc.release)
		}
	}
}
