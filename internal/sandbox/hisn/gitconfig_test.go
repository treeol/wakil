package hisn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testPubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test@wakil"

func TestRenderGitConfigIdentity(t *testing.T) {
	out, err := renderGitConfig(GitIdentity{Name: "Ada", Email: "ada@example.test"}, GitSigning{})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "[user]") || !strings.Contains(s, `name = "Ada"`) || !strings.Contains(s, `email = "ada@example.test"`) {
		t.Fatalf("identity missing:\n%s", s)
	}
}

// Signing off: none of the gpg / commit.gpgsign / tag.gpgsign / signingkey keys.
func TestRenderGitConfigSigningOffHasNoGPG(t *testing.T) {
	out, _ := renderGitConfig(GitIdentity{Name: "A", Email: "a@x.test"}, GitSigning{})
	for _, k := range []string{"gpg", "gpgsign", "signingkey", "format"} {
		if strings.Contains(string(out), k) {
			t.Fatalf("signing off must not write %q:\n%s", k, out)
		}
	}
}

func TestRenderGitConfigSigning(t *testing.T) {
	out, err := renderGitConfig(GitIdentity{Name: "A", Email: "a@x.test"},
		GitSigning{Key: testPubKey, AllowedSigner: "/home/x/.wakil-allowed-signers"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{`signingkey = "key::ssh-ed25519 `, "format = ssh", `allowedSignersFile = "/home/x/.wakil-allowed-signers"`, "gpgsign = true"} {
		if !strings.Contains(s, want) {
			t.Fatalf("want %q:\n%s", want, s)
		}
	}
	// Only the public key text is written, never a private-key marker.
	if strings.Contains(s, "PRIVATE") {
		t.Fatalf("private key material in gitconfig:\n%s", s)
	}
}

// Values that could break out of a gitconfig line are refused.
func TestRenderGitConfigRejectsInjection(t *testing.T) {
	bad := []GitIdentity{
		{Name: "Ada\n[core]\n\tfsmonitor = evil", Email: "a@x.test"},
		{Name: "A\"B", Email: "a@x.test"},
		{Name: "A", Email: "a@x.test\x00"},
	}
	for _, id := range bad {
		if _, err := renderGitConfig(id, GitSigning{}); err == nil {
			t.Errorf("identity %q must be refused", id.Name)
		}
	}
	if _, err := renderGitConfig(GitIdentity{}, GitSigning{Key: "not a key line"}); err == nil {
		t.Error("a non-OpenSSH signing key must be refused")
	}
}

// A refused signing setup writes nothing into the home.
func TestWriteSessionGitConfigNoStrayFiles(t *testing.T) {
	home := t.TempDir()
	// Signing with no email: refused before any file is written.
	if _, err := writeSessionGitConfig(home, GitIdentity{Name: "A"}, GitSigning{Key: testPubKey}); err == nil {
		t.Fatal("signing without an email must be refused")
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Fatalf("refusal left files behind: %v", entries)
	}
}

func TestWriteSessionGitConfigModes(t *testing.T) {
	home := t.TempDir()
	p, err := writeSessionGitConfig(home, GitIdentity{Name: "A", Email: "a@x.test"}, GitSigning{Key: testPubKey})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(p) != home {
		t.Fatalf("gitconfig must live in the home, got %s", p)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("gitconfig mode: %v %v", fi, err)
	}
	signers, err := os.ReadFile(filepath.Join(home, ".wakil-allowed-signers"))
	if err != nil || !strings.HasPrefix(string(signers), "a@x.test ssh-ed25519 ") {
		t.Fatalf("allowed signers: %q %v", signers, err)
	}
}

// TestWithGitConfigInSessionPath (F8): GIT_CONFIG_GLOBAL must point at the
// in-session home (SandboxHome), not the host HomeRoot. The file is written on
// the host under HomeRoot, but hisn remounts it at SandboxHome inside the
// sandbox — the host path does not exist there.
func TestWithGitConfigInSessionPath(t *testing.T) {
	home := t.TempDir()
	b := &Backend{opts: Options{
		StateDir:   t.TempDir(),
		HomeRoot:   home,
		GitName:    "A",
		GitEmail:   "a@x.test",
		SigningKey: testPubKey,
	}}
	s, err := b.withGitConfig(Surface{EnvSet: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	gc := s.EnvSet["GIT_CONFIG_GLOBAL"]
	if gc == "" {
		t.Fatal("GIT_CONFIG_GLOBAL not set")
	}
	if gc == filepath.Join(home, ".wakil-gitconfig") {
		t.Errorf("GIT_CONFIG_GLOBAL points at the host HomeRoot path %q — must be the in-session path", gc)
	}
	want := filepath.Join(SandboxHome, ".wakil-gitconfig")
	if gc != want {
		t.Errorf("GIT_CONFIG_GLOBAL = %q, want %q", gc, want)
	}
	// The file IS written under HomeRoot on the host.
	if _, err := os.Stat(filepath.Join(home, ".wakil-gitconfig")); err != nil {
		t.Errorf(".wakil-gitconfig not written under HomeRoot: %v", err)
	}
}

// TestWithGitConfigSSHCommandKeepsHostPath (F8): GIT_SSH_COMMAND's
// UserKnownHostsFile is a same-path RO mount — the host path IS valid inside
// the sandbox. The env value must keep it.
func TestWithGitConfigSSHCommandKeepsHostPath(t *testing.T) {
	home := t.TempDir()
	kh := filepath.Join(home, "known_hosts")
	if err := os.WriteFile(kh, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	b := &Backend{opts: Options{
		StateDir:   t.TempDir(),
		HomeRoot:   home,
		GitName:    "A",
		GitEmail:   "a@x.test",
		KnownHosts: kh,
	}}
	s, err := b.withGitConfig(Surface{SSHAgent: true, EnvSet: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	sshCmd := s.EnvSet["GIT_SSH_COMMAND"]
	if !strings.Contains(sshCmd, kh) {
		t.Errorf("GIT_SSH_COMMAND must keep the host known_hosts path %q, got %q", kh, sshCmd)
	}
}
