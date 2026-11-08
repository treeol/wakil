package hisn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// The minimum-version check refuses hisn 0.3.2 and names the minimum (0.4.1;
// the src:dst known_hosts mount it needs arrived in 0.3.3, and there is no
// same-path fallback).
func TestPreflightRefusesHisn032NamingMinimum(t *testing.T) {
	fe := newFakeEnv(t)
	setVersion(t, fe, "0.3.2")
	_, err := Open(ctxT(t), fe.options())
	if err == nil {
		t.Fatal("hisn 0.3.2 must be refused")
	}
	if !strings.Contains(err.Error(), MinVersion) || !strings.Contains(err.Error(), "too old") {
		t.Fatalf("refusal must say too old and name %s, got: %v", MinVersion, err)
	}
}

// The rendered generated profile carries the known_hosts mount as one
// read-only "src:dst" entry (host file -> in-session path) and never as a bare
// same-path entry.
func TestKnownHostsProfileEntryIsSrcDst(t *testing.T) {
	home := t.TempDir()
	kh := filepath.Join(home, "known_hosts")
	if err := os.WriteFile(kh, []byte("example.test ssh-ed25519 AAAA\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Surface{SSHAgent: true, EnvSet: map[string]string{}}
	b := &Backend{opts: Options{StateDir: t.TempDir(), HomeRoot: home, KnownHosts: kh}}
	s, err := b.withGitConfig(s)
	if err != nil {
		t.Fatal(err)
	}
	prof, err := buildProfile(policyParams{Cwd: t.TempDir(), Home: home, Hostname: "h"}, s, "")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Policy struct {
			Mounts struct {
				RO []string `toml:"ro"`
			} `toml:"mounts"`
		} `toml:"policy"`
	}
	if _, err := toml.Decode(string(prof), &doc); err != nil {
		t.Fatalf("decode profile: %v", err)
	}
	want := kh + ":" + KnownHostsMount
	found := false
	for _, m := range doc.Policy.Mounts.RO {
		if m == kh {
			t.Errorf("profile carries a same-path known_hosts entry %q", m)
		}
		if m == want {
			found = true
		}
	}
	if !found {
		t.Errorf("profile ro mounts %v lack the src:dst entry %q", doc.Policy.Mounts.RO, want)
	}
	if !strings.HasPrefix(KnownHostsMount, "/wakil/") {
		t.Errorf("KnownHostsMount %q must live under /wakil/<purpose>/", KnownHostsMount)
	}
}
