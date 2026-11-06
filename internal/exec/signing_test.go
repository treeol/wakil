package exec

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testPubKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAATESTKEY test@example"

func writeTempPub(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDetectSigningOff(t *testing.T) {
	for _, mode := range []string{"", "off"} {
		s, reason := DetectSigning(mode, t.TempDir())
		if s.Enabled {
			t.Errorf("mode %q: expected disabled", mode)
		}
		if reason != "" {
			t.Errorf("mode %q: expected no skip reason, got %q", mode, reason)
		}
	}
}

func TestResolvePublicKeyLiteralForms(t *testing.T) {
	// key:: prefix
	got, reason := resolvePublicKey("key::" + testPubKey)
	if reason != "" || got != testPubKey {
		t.Errorf("key:: form: got %q, reason %q", got, reason)
	}
	// bare literal
	got, reason = resolvePublicKey(testPubKey)
	if reason != "" || got != testPubKey {
		t.Errorf("bare literal: got %q, reason %q", got, reason)
	}
	// sk- hardware key literal
	skKey := "sk-ssh-ed25519@openssh.com AAAA... comment"
	got, reason = resolvePublicKey(skKey)
	if reason != "" || got != skKey {
		t.Errorf("sk- literal: got %q, reason %q", got, reason)
	}
}

func TestResolvePublicKeyFromPubFile(t *testing.T) {
	dir := t.TempDir()
	pub := writeTempPub(t, dir, "id_ed25519.pub", testPubKey)
	got, reason := resolvePublicKey(pub)
	if reason != "" || got != testPubKey {
		t.Errorf("pub file: got %q, reason %q", got, reason)
	}
}

func TestResolvePublicKeyPrivatePathUsesPubSibling(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "id_ed25519")
	// The private key file exists but must NEVER be read.
	if err := os.WriteFile(priv, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\nsecret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeTempPub(t, dir, "id_ed25519.pub", testPubKey)

	got, reason := resolvePublicKey(priv)
	if reason != "" {
		t.Fatalf("unexpected skip: %q", reason)
	}
	if got != testPubKey {
		t.Errorf("got %q, want pub sibling content", got)
	}
	if strings.Contains(got, "PRIVATE") {
		t.Fatal("private key content leaked into resolved key")
	}
}

func TestResolvePublicKeyPrivatePathNoPubSibling(t *testing.T) {
	dir := t.TempDir()
	priv := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(priv, []byte("-----BEGIN OPENSSH PRIVATE KEY-----\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, reason := resolvePublicKey(priv)
	if reason == "" {
		t.Fatal("expected skip reason when no .pub sibling exists")
	}
}

func TestReadPublicKeyFileRefusesNonPub(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(p, []byte("anything"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, reason := readPublicKeyFile(p)
	if !strings.Contains(reason, "must be a .pub") {
		t.Errorf("expected .pub refusal, got %q", reason)
	}
}

func TestReadPublicKeyFileRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	p := writeTempPub(t, dir, "junk.pub", "not a key at all")
	_, reason := readPublicKeyFile(p)
	if reason == "" {
		t.Fatal("expected rejection of non-key content")
	}
}

func TestSigningEnvDisabled(t *testing.T) {
	if got := signingEnv(SigningSetup{}); got != nil {
		t.Errorf("disabled setup should produce no args, got %v", got)
	}
}

func TestSigningEnvEnabled(t *testing.T) {
	s := SigningSetup{
		Enabled:   true,
		AgentSock: "/run/user/1000/ssh-agent.sock",
		PublicKey: testPubKey,
		AutoSign:  true,
		UserName:  "Valon Mamudi",
		UserEmail: "valon@example.com",
	}
	args := signingEnv(s)
	joined := strings.Join(args, " ")

	for _, want := range []string{
		"/run/user/1000/ssh-agent.sock:/ssh-agent.sock",
		"SSH_AUTH_SOCK=/ssh-agent.sock",
		"GIT_CONFIG_COUNT=5",
		"GIT_CONFIG_KEY_0=gpg.format",
		"GIT_CONFIG_VALUE_0=ssh",
		"GIT_CONFIG_KEY_1=user.signingkey",
		"GIT_CONFIG_VALUE_1=key::" + testPubKey,
		"GIT_CONFIG_KEY_2=commit.gpgsign",
		"GIT_CONFIG_VALUE_2=true",
		"user.name",
		"Valon Mamudi",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in signing args", want)
		}
	}
}

func TestSigningEnvNoAutoSign(t *testing.T) {
	s := SigningSetup{
		Enabled:   true,
		AgentSock: "/tmp/sock",
		PublicKey: testPubKey,
		AutoSign:  false, // host repo does not auto-sign → mirror that
	}
	joined := strings.Join(signingEnv(s), " ")
	if strings.Contains(joined, "commit.gpgsign") {
		t.Error("commit.gpgsign must not be injected when host does not auto-sign")
	}
	if !strings.Contains(joined, "GIT_CONFIG_COUNT=2") {
		t.Errorf("expected 2 pairs (format+key), got: %s", joined)
	}
	// format + key still present so manual `git commit -S` works
	if !strings.Contains(joined, "user.signingkey") {
		t.Error("user.signingkey must be injected even without auto-sign")
	}
}

func TestDetectSigningRejectsNonPathValues(t *testing.T) {
	for _, v := range []string{"true", "on", "1", "yes"} {
		s, reason := DetectSigning(v, t.TempDir())
		if s.Enabled {
			t.Errorf("value %q: must not enable signing", v)
		}
		if !strings.Contains(reason, "unrecognized value") {
			t.Errorf("value %q: want 'unrecognized value' message, got %q", v, reason)
		}
	}
}

func TestDetectSigningExplicitKeyNoAgent(t *testing.T) {
	dir := t.TempDir()
	pub := writeTempPub(t, dir, "sign.pub", testPubKey)
	t.Setenv("SSH_AUTH_SOCK", "") // ensure no agent
	s, reason := DetectSigning(pub, dir)
	if s.Enabled {
		t.Fatal("expected disabled without agent")
	}
	if !strings.Contains(reason, "SSH_AUTH_SOCK") {
		t.Errorf("expected agent skip reason, got %q", reason)
	}
}

func TestDetectSigningExplicitKeyWithAgent(t *testing.T) {
	dir := t.TempDir()
	pub := writeTempPub(t, dir, "sign.pub", testPubKey)
	// A plain file works for the Stat check; docker would mount it fine.
	sock := writeTempPub(t, dir, "fake.sock.pub", "x") // any existing path
	t.Setenv("SSH_AUTH_SOCK", sock)

	s, reason := DetectSigning(pub, dir)
	if !s.Enabled {
		t.Fatalf("expected enabled, skip: %q", reason)
	}
	if s.PublicKey != testPubKey {
		t.Errorf("public key = %q", s.PublicKey)
	}
	if s.AgentSock == "" {
		t.Error("agent sock empty")
	}
	// F5: SigningKeyValue carries the raw user.signingkey value (the explicit
	// path in this case).
	if s.SigningKeyValue != pub {
		t.Errorf("SigningKeyValue = %q, want %q", s.SigningKeyValue, pub)
	}
}

func TestResolveRepoSigningOffWhenGpgsignNotTrue(t *testing.T) {
	// Hide the host environment's git config from the product's git calls:
	// global/system files AND command-scope pairs (a real wakil session
	// injects GIT_CONFIG_* into everything it runs, including this binary).
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	dir := t.TempDir()
	initRepo := func(args ...string) {
		t.Helper()
		cmd := osexec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	initRepo("init", "-q")
	// commit.gpgsign unset → signing off, no error.
	rs, err := ResolveRepoSigning(dir, "")
	if err != nil || rs.Signing {
		t.Fatalf("unset gpgsign: want off/no error, got %+v err %v", rs, err)
	}
	initRepo("config", "commit.gpgsign", "false")
	if rs, err := ResolveRepoSigning(dir, ""); err != nil || rs.Signing {
		t.Fatalf("gpgsign=false: want off/no error, got %+v err %v", rs, err)
	}
}

func TestResolveRepoSigningKeyForms(t *testing.T) {
	// Hide the host environment's git config from the product's git calls:
	// global/system files AND command-scope pairs (a real wakil session
	// injects GIT_CONFIG_* into everything it runs, including this binary).
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := osexec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "commit.gpgsign", "true")
	run("config", "gpg.format", "ssh")
	run("config", "user.signingkey", "key::"+testPubKey)

	// key:: literal: pub used as-is, no private path.
	rs, err := ResolveRepoSigning(dir, "")
	if err != nil || !rs.Signing || rs.PublicKey != testPubKey || rs.PrivateKeyPath != "" {
		t.Fatalf("key:: form: got %+v err %v", rs, err)
	}
	// .pub path: pub read, private path is the .pub-less sibling.
	pub := writeTempPub(t, dir, "sign.pub", testPubKey)
	// (re-point via explicitKey)
	rs, err = ResolveRepoSigning(dir, pub)
	if err != nil || !rs.Signing || rs.PublicKey != testPubKey {
		t.Fatalf("pub path form: got %+v err %v", rs, err)
	}
	if want := strings.TrimSuffix(pub, ".pub"); rs.PrivateKeyPath != want {
		t.Errorf("pub path form: private path = %q, want %q", rs.PrivateKeyPath, want)
	}
}

func TestResolveRepoSigningFailsOnUnsetOrUnloadable(t *testing.T) {
	// Hide the host environment's git config from the product's git calls:
	// global/system files AND command-scope pairs (a real wakil session
	// injects GIT_CONFIG_* into everything it runs, including this binary).
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := osexec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-q")
	run("config", "commit.gpgsign", "true")
	run("config", "gpg.format", "ssh")
	// signingkey unset while gpgsign=true → hard failure.
	if _, err := ResolveRepoSigning(dir, ""); err == nil || !strings.Contains(err.Error(), "user.signingkey") {
		t.Fatalf("unset signingkey: want error naming user.signingkey, got %v", err)
	}
	// key path that does not exist → hard failure.
	if _, err := ResolveRepoSigning(dir, filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing key path: want error")
	}
	// gpg.format not ssh → hard failure.
	run("config", "gpg.format", "openpgp")
	run("config", "user.signingkey", "key::"+testPubKey)
	if _, err := ResolveRepoSigning(dir, ""); err == nil || !strings.Contains(err.Error(), "gpg.format") {
		t.Fatalf("non-ssh format: want error naming gpg.format, got %v", err)
	}
}

// Fail-closed: a git invocation that itself fails (not exit 1 = unset) must
// never read as "signing is off" — a gpgsign=true repo must not silently
// start unsigned. Simulated with a bogus repo dir (git exits 128, not 1).
// (A corrupt .git/config is NOT this case: git itself truncates at the bad
// section and reads the rest, so `config --get` exits 1 = unset — matching
// what a host commit would see.)
func TestResolveRepoSigningFailsClosedOnGitError(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	// A nonexistent repo dir: git fails with exit 128, not 1.
	if _, err := ResolveRepoSigning(filepath.Join(t.TempDir(), "no-such-repo"), ""); err == nil {
		t.Fatal("git failure must be an error, never a silent unsigned start")
	}
}

// A relative user.signingkey resolves against the REPO (git runs with the repo
// as cwd), never wakil's process cwd.
func TestResolveRepoSigningRepoRelativeKeyPath(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("GIT_CONFIG_COUNT", "0")
	// Build a repo dir that differs from the process cwd: t.TempDir is under
	// /tmp, the test process runs in the package dir.
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "keys", "sign")
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath+".pub", []byte(testPubKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := osexec.Command("git", "init", "-q", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	cmd = osexec.Command("git", "-C", dir, "config", "commit.gpgsign", "true")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
	cmd = osexec.Command("git", "-C", dir, "config", "user.signingkey", "keys/sign")
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git config: %v: %s", err, out)
	}
	rs, err := ResolveRepoSigning(dir, "")
	if err != nil {
		t.Fatalf("repo-relative key must resolve: %v", err)
	}
	if rs.PublicKey != testPubKey {
		t.Errorf("public key = %q, want the repo's key", rs.PublicKey)
	}
	if rs.PrivateKeyPath != keyPath {
		t.Errorf("private path = %q, want %q", rs.PrivateKeyPath, keyPath)
	}
}
