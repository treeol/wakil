package hisn

import (
	"fmt"
	"strings"

	"golang.org/x/crypto/ssh"
)

// fingerprintOf parses the first authorized-key line in raw. name is used only
// in error messages.
func fingerprintOf(raw []byte, name string) (string, error) {
	line := strings.TrimSpace(string(raw))
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	if line == "" {
		return "", fmt.Errorf("ssh public key %s: file is empty", name)
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return "", fmt.Errorf("ssh public key %s: does not parse as an OpenSSH public key: %w", name, err)
	}
	return ssh.FingerprintSHA256(key), nil
}

// PublicKeyLineFingerprint returns the SHA256 fingerprint of one OpenSSH
// public-key line (already resolved on the host; no file is read).
func PublicKeyLineFingerprint(line string) (string, error) {
	return fingerprintOf([]byte(line), "signing key")
}
