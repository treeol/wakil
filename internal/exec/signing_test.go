package exec

import (
	"os"
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
}
