package wiring

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// F5: generate a throwaway ed25519 key and set up git config so that
// hisnGitSetup can resolve the signing key. Returns the key path, its .pub
// path, and its SHA256 fingerprint.
func f5SetupKey(t *testing.T, dir string) (keyPath, pubPath, fp string) {
	t.Helper()
	keygenPath, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not found")
	}
	keyPath = filepath.Join(dir, "id_ed25519")
	pubPath = keyPath + ".pub"
	if err := exec.Command(keygenPath, "-t", "ed25519", "-N", "", "-f", keyPath).Run(); err != nil {
		t.Skipf("ssh-keygen failed: %v", err)
	}
	out, err := exec.Command(keygenPath, "-lf", pubPath).Output()
	if err != nil {
		t.Skipf("ssh-keygen -lf failed: %v", err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 2 || !strings.HasPrefix(fields[1], "SHA256:") {
		t.Skipf("could not parse fingerprint from %q", string(out))
	}
	fp = fields[1]
	return keyPath, pubPath, fp
}

// f5GitConfig sets up a git repo in dir with user.name, user.email,
// gpg.format=ssh, and user.signingkey=keyPath. Uses global/system scope only
// when trusted (which is what DetectSigningTrusted reads).
func f5GitConfig(t *testing.T, dir, name, email, keyPath string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "user.name", name)
	run("config", "user.email", email)
	run("config", "gpg.format", "ssh")
	run("config", "user.signingkey", keyPath)
}

// F5: a signing key whose fingerprint IS in sandbox.hisn.ssh_keys succeeds.
func TestHisnGitSetupFingerprintInAllowlist(t *testing.T) {
	dir := t.TempDir()
	keyPath, _, fp := f5SetupKey(t, dir)
	t.Setenv("SSH_AUTH_SOCK", "") // no agent; we're testing the fingerprint check only

	// DetectSigningTrusted reads gpg.format and user.signingkey from
	// global/system scope. Set them globally so they are picked up.
	if err := exec.Command("git", "config", "--global", "gpg.format", "ssh").Run(); err != nil {
		t.Skipf("git config --global gpg.format: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("git", "config", "--global", "--unset", "gpg.format").Run()
	})
	if err := exec.Command("git", "config", "--global", "user.signingkey", keyPath).Run(); err != nil {
		t.Skipf("git config --global user.signingkey: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("git", "config", "--global", "--unset", "user.signingkey").Run()
	})

	// A fake agent socket so finishSetup's SSH_AUTH_SOCK check passes.
	sock := filepath.Join(dir, "fake.sock")
	if err := os.WriteFile(sock, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_AUTH_SOCK", sock)

	cfg := config.Config{SSHSigning: "auto", HostWorkDir: dir}
	_, _, key, keys, err := hisnGitSetup(cfg, config.HisnConfig{
		SSHAgent:         true,
		SSHKeys:          []string{fp},
		GitIdentityName:  "Test",
		GitIdentityEmail: "test@example.test",
	})
	if err != nil {
		t.Fatalf("expected success with fingerprint in allowlist, got: %v", err)
	}
	if key == "" {
		t.Error("signing key should not be empty on success")
	}
	if !containsStr(keys, fp) {
		t.Errorf("fingerprint %s should be in sshKeys: %v", fp, keys)
	}
}

// F5: a signing key whose fingerprint is NOT in sandbox.hisn.ssh_keys fails
// with an error naming the key, the fingerprint, and sandbox.hisn.ssh_keys.
func TestHisnGitSetupFingerprintNotInAllowlist(t *testing.T) {
	dir := t.TempDir()
	keyPath, _, fp := f5SetupKey(t, dir)

	if err := exec.Command("git", "config", "--global", "gpg.format", "ssh").Run(); err != nil {
		t.Skipf("git config --global gpg.format: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("git", "config", "--global", "--unset", "gpg.format").Run()
	})
	if err := exec.Command("git", "config", "--global", "user.signingkey", keyPath).Run(); err != nil {
		t.Skipf("git config --global user.signingkey: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("git", "config", "--global", "--unset", "user.signingkey").Run()
	})

	sock := filepath.Join(dir, "fake.sock")
	if err := os.WriteFile(sock, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_AUTH_SOCK", sock)

	cfg := config.Config{SSHSigning: "auto", HostWorkDir: dir}
	_, _, _, _, err := hisnGitSetup(cfg, config.HisnConfig{
		SSHAgent:         true,
		SSHKeys:          []string{"SHA256:differentFingerprint"},
		GitIdentityName:  "Test",
		GitIdentityEmail: "test@example.test",
	})
	if err == nil {
		t.Fatal("expected error when fingerprint not in allowlist")
	}
	if !strings.Contains(err.Error(), "not in sandbox.hisn.ssh_keys") {
		t.Errorf("error should name sandbox.hisn.ssh_keys, got: %v", err)
	}
	if !strings.Contains(err.Error(), fp) {
		t.Errorf("error should name the fingerprint %s, got: %v", fp, err)
	}
}

// F5: user.signingkey unset while git identity is configured → error naming it.
func TestHisnGitSetupSigningKeyUnsetWithIdentity(t *testing.T) {
	dir := t.TempDir()
	f5GitConfig(t, dir, "Test", "test@example.test", "/tmp/nonexistent")
	// Don't set user.signingkey — remove it if it was set
	_ = exec.Command("git", "config", "--global", "--unset", "user.signingkey").Run()

	// Set gpg.format=ssh globally so auto mode proceeds past the format check
	if err := exec.Command("git", "config", "--global", "gpg.format", "ssh").Run(); err != nil {
		t.Skipf("git config --global gpg.format: %v", err)
	}
	t.Cleanup(func() {
		_ = exec.Command("git", "config", "--global", "--unset", "gpg.format").Run()
	})

	sock := filepath.Join(dir, "fake.sock")
	if err := os.WriteFile(sock, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_AUTH_SOCK", sock)

	cfg := config.Config{SSHSigning: "auto", HostWorkDir: dir}
	_, _, _, _, err := hisnGitSetup(cfg, config.HisnConfig{
		SSHAgent:         true,
		SSHKeys:          []string{"SHA256:something"},
		GitIdentityName:  "Test",
		GitIdentityEmail: "test@example.test",
	})
	if err == nil {
		t.Fatal("expected error when signingkey unset with identity configured")
	}
	if !strings.Contains(err.Error(), "user.signingkey") {
		t.Errorf("error should name user.signingkey, got: %v", err)
	}
}
