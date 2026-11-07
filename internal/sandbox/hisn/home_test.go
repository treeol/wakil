package hisn

import (
	"strings"
	"testing"
)

// TestHomePersistDecisionTable covers the [home] decision (P1): persist emits
// the table on hisn >= 0.3.2 only; tmpfs omits it and needs 0.3.2; the older
// hisn keeps the 0.4.x mount (no table, no error for persist).
func TestHomePersistDecisionTable(t *testing.T) {
	const root = "/state/hisn-home/abc"
	old := [3]int{0, 3, 1}
	newer := [3]int{0, 3, 3}
	cases := []struct {
		name, mode string
		ver        [3]int
		want       string
		wantErr    string
	}{
		{"persist on 0.3.3 emits", "persist", newer, root, ""},
		{"default mode is persist", "", newer, root, ""},
		{"persist on 0.3.1 omits, mount stays", "persist", old, "", ""},
		{"tmpfs on 0.3.3 omits table", "tmpfs", newer, "", ""},
		{"tmpfs on 0.3.1 is a hard error", "tmpfs", old, "", "needs hisn >= 0.3.3"},
		{"unknown mode is an error", "shared", newer, "", "want \"persist\" or \"tmpfs\""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := homePersistFor(c.mode, root, c.ver)
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
// with the resolved path, and a tmpfs/old-hisn profile has no [home] at all.
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
