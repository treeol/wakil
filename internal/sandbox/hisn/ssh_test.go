package hisn

import (
	"strings"
	"testing"
)

// TestSSHTableEmission: [ssh] appears only when the agent is on AND at least
// one fingerprint is allowed (plan D7). Agent off, or no keys, emits nothing.
func TestSSHTableEmission(t *testing.T) {
	pp := policyParams{Cwd: "/work", Home: "/state/h", Hostname: "wakil-x", ShmSize: 64, TmpSize: 1024}
	fp := "SHA256:abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"
	cases := []struct {
		name string
		s    Surface
		want bool
	}{
		{"agent off, no table", Surface{}, false},
		{"agent on, no keys: no table", Surface{SSHAgent: true}, false},
		{"keys without agent: no table", Surface{SSHKeys: []string{fp}}, false},
		{"agent on with a key emits [ssh]", Surface{SSHAgent: true, SSHKeys: []string{fp}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := buildProfile(pp, c.s, "")
			if err != nil {
				t.Fatal(err)
			}
			got := strings.Contains(string(out), "[ssh]")
			if got != c.want {
				t.Fatalf("[ssh] present=%v, want %v:\n%s", got, c.want, out)
			}
			if c.want && !strings.Contains(string(out), "agent = true") {
				t.Fatalf("[ssh] without agent = true:\n%s", out)
			}
		})
	}
}

// TestSSHNeedsGlobalKeys: agent on with no allowlist is refused by the backend
// surface (through Normalize the empty allowlist check lives in Open, so here
// the table emission is the contract: empty keys never produce a table).
func TestSSHEmptyKeysNeverEmitted(t *testing.T) {
	out, err := buildProfile(policyParams{Cwd: "/w", Home: "/h", Hostname: "wakil-x", ShmSize: 64, TmpSize: 1024},
		Surface{SSHAgent: true, SSHKeys: []string{}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "[ssh]") {
		t.Fatalf("empty allowlist must not emit [ssh]:\n%s", out)
	}
}
