package hisn

import (
	"strings"
	"testing"
)

// TestHomePersistDecisionTable covers the [home] decision: persist emits the
// table; tmpfs omits it.
func TestHomePersistDecisionTable(t *testing.T) {
	const root = "/state/hisn-home/abc"
	cases := []struct {
		name, mode string
		want       string
		wantErr    string
	}{
		{"persist emits", "persist", root, ""},
		{"default mode is persist", "", root, ""},
		{"tmpfs omits table", "tmpfs", "", ""},
		{"unknown mode is an error", "shared", "", "want \"persist\" or \"tmpfs\""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := homePersistFor(c.mode, root)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, c.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestProfileEmitsHomeTable checks the generated TOML: [home] persist appears
// with the resolved path, and a tmpfs profile has no [home] at all.
func TestProfileEmitsHomeTable(t *testing.T) {
	pp := policyParams{Cwd: "/work", Home: "/state/hisn-home/abc", Hostname: "wakil-x", ShmSize: 64, TmpSize: 1024}
	withHome, err := buildProfile(pp, Surface{}, "/state/hisn-home/abc")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(withHome), "[home]") || !strings.Contains(string(withHome), `persist = "/state/hisn-home/abc"`) {
		t.Fatalf("profile lacks [home] persist:\n%s", withHome)
	}
	without, err := buildProfile(pp, Surface{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(without), "[home]") {
		t.Fatalf("profile without persist path must omit [home]:\n%s", without)
	}
}
