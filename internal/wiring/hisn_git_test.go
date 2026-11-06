package wiring

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/treeol/wakil/internal/config"
)

// The known_hosts mount exists only when the agent (an ssh transport) is on.
func TestKnownHostsPathGatedOnAgent(t *testing.T) {
	if p := knownHostsPath(false); p != "" {
		t.Fatalf("agent off must not mount known_hosts, got %q", p)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home dir")
	}
	if p := knownHostsPath(true); p != filepath.Join(home, ".ssh", "known_hosts") {
		t.Fatalf("agent on: got %q", p)
	}
}

// ssh_signing under hisn with the agent off is an error, never a silent skip.
func TestHisnGitSetupSigningNeedsAgent(t *testing.T) {
	cfg := config.Config{SSHSigning: "/tmp/nope.pub"}
	_, _, _, _, err := hisnGitSetup(cfg, config.HisnConfig{SSHAgent: false})
	if err == nil {
		t.Fatal("ssh_signing without the agent must fail under hisn")
	}
}

// Off signing + no agent: no error, identity from the explicit override.
func TestHisnGitSetupIdentityOverride(t *testing.T) {
	cfg := config.Config{SSHSigning: "off", HostWorkDir: t.TempDir()}
	name, email, key, keys, err := hisnGitSetup(cfg, config.HisnConfig{
		GitIdentityName: "Ada", GitIdentityEmail: "ada@example.test"})
	if err != nil {
		t.Fatal(err)
	}
	if name != "Ada" || email != "ada@example.test" || key != "" || len(keys) != 0 {
		t.Fatalf("got name=%q email=%q key=%q keys=%v", name, email, key, keys)
	}
}

func TestContainsStr(t *testing.T) {
	if !containsStr([]string{"a", "b"}, "b") || containsStr(nil, "a") {
		t.Fatal("containsStr broken")
	}
}
