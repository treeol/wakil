package hisn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A real ed25519 public key (test fixture, not a secret). The fingerprint is
// the SHA256 of the wire-format key, computed by x/crypto.
const fixturePub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test@wakil"

func TestPublicKeyFingerprint(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "id.pub")
	if err := os.WriteFile(good, []byte(fixturePub+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fp, err := PublicKeyFingerprint(good)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fp, "SHA256:") || len(fp) < 40 {
		t.Fatalf("fingerprint %q is not SHA256:…", fp)
	}
	// Stable across calls.
	if again, _ := PublicKeyFingerprint(good); again != fp {
		t.Fatalf("fingerprint changed: %q vs %q", fp, again)
	}
}

func TestPublicKeyFingerprintRejects(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.pub")
	if err := os.WriteFile(bad, []byte("not a key at all\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := PublicKeyFingerprint(bad); err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatalf("unparseable key must fail naming the path, got %v", err)
	}
	empty := filepath.Join(dir, "empty.pub")
	_ = os.WriteFile(empty, nil, 0o600)
	if _, err := PublicKeyFingerprint(empty); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Fatalf("empty file must fail, got %v", err)
	}
	if _, err := PublicKeyFingerprint(filepath.Join(dir, "missing.pub")); err == nil {
		t.Fatal("missing file must fail")
	}
}
