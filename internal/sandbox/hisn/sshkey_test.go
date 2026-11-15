package hisn

import (
	"strings"
	"testing"
)

// A real ed25519 public key (test fixture, not a secret). The fingerprint is
// the SHA256 of the wire-format key, computed by x/crypto.
const fixturePub = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl test@wakil"

func TestPublicKeyLineFingerprint(t *testing.T) {
	fp, err := PublicKeyLineFingerprint(fixturePub)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fp, "SHA256:") || len(fp) < 40 {
		t.Fatalf("fingerprint %q is not SHA256:…", fp)
	}
	// Stable across calls.
	if again, _ := PublicKeyLineFingerprint(fixturePub); again != fp {
		t.Fatalf("fingerprint changed: %q vs %q", fp, again)
	}
}

func TestPublicKeyLineFingerprintRejects(t *testing.T) {
	if _, err := PublicKeyLineFingerprint("not a key at all"); err == nil {
		t.Fatal("unparseable key must fail")
	}
	if _, err := PublicKeyLineFingerprint(""); err == nil {
		t.Fatal("empty input must fail")
	}
}
