package hisn

import (
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// TestPolicyNetworkHost: the default network mode renders as "host" in the
// [policy.session] table.
func TestPolicyNetworkHost(t *testing.T) {
	pp := policyParams{Cwd: "/w", Home: "/h", Hostname: "wakil-x"}
	pf := policyFor(pp)
	if pf.Session.Network != "host" {
		t.Fatalf("default network: want %q, got %q", "host", pf.Session.Network)
	}
	// Render and re-parse to confirm the TOML carries it.
	var buf strings.Builder
	if err := toml.NewEncoder(&buf).Encode(pf); err != nil {
		t.Fatal(err)
	}
	var dec policyFile
	if _, err := toml.Decode(buf.String(), &dec); err != nil {
		t.Fatal(err)
	}
	if dec.Session.Network != "host" {
		t.Fatalf("decoded network: want %q, got %q", "host", dec.Session.Network)
	}
}

// TestPolicyNetworkOpen: when Network is "open", the profile carries it.
func TestPolicyNetworkOpen(t *testing.T) {
	pp := policyParams{Cwd: "/w", Home: "/h", Hostname: "wakil-x", Network: "open"}
	pf := policyFor(pp)
	if pf.Session.Network != "open" {
		t.Fatalf("network: want %q, got %q", "open", pf.Session.Network)
	}
	var buf strings.Builder
	if err := toml.NewEncoder(&buf).Encode(pf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `network = "open"`) {
		t.Fatalf("rendered profile does not contain network = %q:\n%s", "open", buf.String())
	}
}

// TestPolicyNetworkEmpty: an empty Network defaults to "host" in the profile.
func TestPolicyNetworkEmpty(t *testing.T) {
	pp := policyParams{Cwd: "/w", Home: "/h", Hostname: "wakil-x", Network: ""}
	pf := policyFor(pp)
	if pf.Session.Network != "host" {
		t.Fatalf("empty network: want %q, got %q", "host", pf.Session.Network)
	}
}
