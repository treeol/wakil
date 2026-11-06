package wiring

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// gitRunner runs git in dir with a scrubbed global/system config so tests do
// not read or mutate the host user's real git config.
type gitRunner struct {
	t   *testing.T
	dir string
}

func newGitRunner(t *testing.T, dir string) *gitRunner {
	t.Helper()
	return &gitRunner{t: t, dir: dir}
}

func (g *gitRunner) run(args ...string) string {
	g.t.Helper()
	cmd := osexec.Command("git", args...)
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		g.t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// signingKeyPair generates a throwaway ed25519 key in dir and returns the
// private path, the .pub path, the public key line, and its SHA256 fingerprint.
func signingKeyPair(t *testing.T, dir string) (keyPath, pubPath, pubLine, fp string) {
	t.Helper()
	keygenPath, err := osexec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen not found")
	}
	keyPath = filepath.Join(dir, "id_ed25519")
	pubPath = keyPath + ".pub"
	if err := osexec.Command(keygenPath, "-t", "ed25519", "-N", "", "-f", keyPath).Run(); err != nil {
		t.Skipf("ssh-keygen failed: %v", err)
	}
	out, err := osexec.Command(keygenPath, "-lf", pubPath).Output()
	if err != nil {
		t.Skipf("ssh-keygen -lf failed: %v", err)
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) < 2 || !strings.HasPrefix(fields[1], "SHA256:") {
		t.Skipf("could not parse fingerprint from %q", string(out))
	}
	fp = fields[1]
	b, err := os.ReadFile(pubPath)
	if err != nil {
		t.Fatal(err)
	}
	pubLine = strings.TrimSpace(string(b))
	return keyPath, pubPath, pubLine, fp
}

// signRepo initializes a repo in dir with commit.gpgsign=true, gpg.format=ssh,
// and user.signingkey=<key>.
func signRepo(t *testing.T, dir, key string) {
	t.Helper()
	g := newGitRunner(t, dir)
	g.run("init", "-q")
	g.run("config", "user.name", "Test")
	g.run("config", "user.email", "test@example.test")
	g.run("config", "commit.gpgsign", "true")
	g.run("config", "gpg.format", "ssh")
	g.run("config", "user.signingkey", key)
}

// A throwaway ssh-agent over a temp socket, so tests never touch the user's
// real agent or its keys. ssh-add talks to it through SSH_AUTH_SOCK.
func throwawayAgent(t *testing.T) {
	t.Helper()
	agentPath, err := osexec.LookPath("ssh-agent")
	if err != nil {
		t.Skip("ssh-agent not found")
	}
	socket := filepath.Join(t.TempDir(), "agent.sock")
	out, err := osexec.Command(agentPath, "-s", "-a", socket).Output()
	if err != nil {
		t.Skipf("ssh-agent failed: %v", err)
	}
	var sock, pid string
	for _, line := range strings.Split(string(out), "\n") {
		if kv := strings.SplitN(line, ";", 2); len(kv) > 0 {
			if strings.HasPrefix(kv[0], "SSH_AUTH_SOCK=") {
				sock = strings.TrimPrefix(kv[0], "SSH_AUTH_SOCK=")
			}
			if strings.HasPrefix(kv[0], "SSH_AGENT_PID=") {
				pid = strings.TrimPrefix(kv[0], "SSH_AGENT_PID=")
			}
		}
	}
	if sock == "" || pid == "" {
		t.Fatalf("could not parse ssh-agent output: %s", out)
	}
	if p, err := strconv.Atoi(pid); err == nil && p > 0 {
		t.Cleanup(func() { _ = syscall.Kill(p, syscall.SIGTERM) })
	}
	t.Setenv("SSH_AUTH_SOCK", sock)
}

// scrubGlobalGit hides the host environment's git config from the product's
// `git config` subprocesses (inherited env): global/system files AND any
// command-scope GIT_CONFIG_* pairs (a real wakil session injects them into
// everything it runs, including this test binary). GIT_CONFIG_COUNT=0
// neutralizes the pairs.
func scrubGlobalGit(t *testing.T) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_CONFIG_COUNT", "0")
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

// 0.5.1: a repo with commit.gpgsign NOT true gets NO signing injected, and the
// plan succeeds — signing is a per-repo decision now.
func TestHisnGitSetupGpgsignOffNoSigning(t *testing.T) {
	scrubGlobalGit(t)
	dir := t.TempDir()
	g := newGitRunner(t, dir)
	g.run("init", "-q")
	g.run("config", "user.name", "Test")
	g.run("config", "user.email", "test@example.test")

	cfg := config.Config{SSHSigning: "auto", HostWorkDir: dir}
	name, email, key, _, err := hisnGitSetup(cfg, config.HisnConfig{SSHAgent: true, SSHKeys: []string{"SHA256:other"}})
	if err != nil {
		t.Fatalf("gpgsign off must start unsigned, got: %v", err)
	}
	if name != "Test" || email != "test@example.test" || key != "" {
		t.Fatalf("no signing settings expected, got name=%q email=%q key=%q", name, email, key)
	}
}

// A repo whose user.signingkey is a key:: literal signs with it, even though
// the fingerprint is NOT in sandbox.hisn.ssh_keys (ssh_keys no longer gates
// signing): the fingerprint is appended to the session allowlist.
func TestHisnGitSetupKeyLiteralNotInSSHKeys(t *testing.T) {
	scrubGlobalGit(t)
	dir := t.TempDir()
	keyPath, _, pubLine, fp := signingKeyPair(t, dir)
	throwawayAgent(t)
	if err := osexec.Command("ssh-add", keyPath).Run(); err != nil {
		t.Skipf("ssh-add failed: %v", err)
	}
	signRepo(t, dir, "key::"+pubLine)

	cfg := config.Config{SSHSigning: "auto", HostWorkDir: dir}
	_, _, key, keys, err := hisnGitSetup(cfg, config.HisnConfig{SSHAgent: true})
	if err != nil {
		t.Fatalf("expected success with the key in the host agent, got: %v", err)
	}
	if key != pubLine {
		t.Errorf("signing key = %q, want the repo's public key", key)
	}
	if !containsStr(keys, fp) {
		t.Errorf("fingerprint %s should be appended to the session allowlist: %v", fp, keys)
	}
}

// A repo whose user.signingkey is a private-key path whose key is ALREADY in
// the host agent signs (the .pub sibling is read, never the private file).
func TestHisnGitSetupPrivateKeyInAgent(t *testing.T) {
	scrubGlobalGit(t)
	dir := t.TempDir()
	keyPath, _, pubLine, fp := signingKeyPair(t, dir)
	throwawayAgent(t)
	if err := osexec.Command("ssh-add", keyPath).Run(); err != nil {
		t.Skipf("ssh-add failed: %v", err)
	}
	signRepo(t, dir, keyPath) // private path, not .pub

	cfg := config.Config{SSHSigning: "auto", HostWorkDir: dir}
	_, _, key, keys, err := hisnGitSetup(cfg, config.HisnConfig{SSHAgent: true})
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if key != pubLine || !containsStr(keys, fp) {
		t.Errorf("key=%q keys=%v", key, keys)
	}
}

// A repo whose key is NOT in the host agent (and no askpass) fails the plan
// LOUD, naming the repo, the key, and the fingerprint — never a silent
// unsigned start. Simulated with a key file that does not exist.
func TestHisnGitSetupUnloadableKeyFailsLoud(t *testing.T) {
	scrubGlobalGit(t)
	dir := t.TempDir()
	throwawayAgent(t) // empty agent: nothing held
	missing := filepath.Join(dir, "does-not-exist")
	signRepo(t, dir, missing)

	cfg := config.Config{SSHSigning: "auto", HostWorkDir: dir}
	_, _, _, _, err := hisnGitSetup(cfg, config.HisnConfig{SSHAgent: true})
	if err == nil {
		t.Fatal("expected a loud failure for an unloadable signing key")
	}
	// The key path does not exist, so resolution fails before the agent check:
	// the error names the repo and the key.
	for _, want := range []string{dir, "does-not-exist"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %v", want, err)
		}
	}
}

// commit.gpgsign=true with user.signingkey unset fails the plan, naming both.
func TestHisnGitSetupGpgsignTrueKeyUnset(t *testing.T) {
	scrubGlobalGit(t)
	dir := t.TempDir()
	g := newGitRunner(t, dir)
	g.run("init", "-q")
	g.run("config", "user.name", "Test")
	g.run("config", "user.email", "test@example.test")
	g.run("config", "commit.gpgsign", "true")

	cfg := config.Config{SSHSigning: "auto", HostWorkDir: dir}
	_, _, _, _, err := hisnGitSetup(cfg, config.HisnConfig{SSHAgent: true})
	if err == nil || !strings.Contains(err.Error(), "user.signingkey") {
		t.Fatalf("want error naming user.signingkey, got: %v", err)
	}
}

// Two repos with different keys in one run resolve EACH its own key (the
// per-repo point): each repo's effective config decides, and each session
// allowlist carries that repo's fingerprint only.
func TestHisnGitSetupPerRepoKeys(t *testing.T) {
	scrubGlobalGit(t)
	throwawayAgent(t)
	repos := make([][4]string, 2) // keyPath, pubLine, fp, dir
	for i := range repos {
		dir := t.TempDir()
		keyPath, _, pubLine, fp := signingKeyPair(t, dir)
		if err := osexec.Command("ssh-add", keyPath).Run(); err != nil {
			t.Skipf("ssh-add failed: %v", err)
		}
		signRepo(t, dir, "key::"+pubLine)
		repos[i] = [4]string{keyPath, pubLine, fp, dir}
	}
	for i, r := range repos {
		cfg := config.Config{SSHSigning: "auto", HostWorkDir: r[3]}
		_, _, key, keys, err := hisnGitSetup(cfg, config.HisnConfig{SSHAgent: true})
		if err != nil {
			t.Fatalf("repo %d: %v", i, err)
		}
		if key != r[1] {
			t.Errorf("repo %d: key = %q, want its own public key", i, key)
		}
		other := repos[1-i][2]
		if containsStr(keys, other) {
			t.Errorf("repo %d: allowlist must not carry the other repo's fingerprint %s: %v", i, other, keys)
		}
	}
}

func TestContainsStr(t *testing.T) {
	if !containsStr([]string{"a", "b"}, "b") || containsStr(nil, "a") {
		t.Fatal("containsStr broken")
	}
}
