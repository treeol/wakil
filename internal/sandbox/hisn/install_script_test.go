package hisn

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestInstallScriptHisnVersionMatchesMinVersion keeps install.sh from
// installing a hisn older than the one Wakil requires: HISN_VERSION in the
// installer must equal MinHisnVersion.
func TestInstallScriptHisnVersionMatchesMinVersion(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^HISN_VERSION="([0-9]+\.[0-9]+\.[0-9]+)"$`).FindSubmatch(b)
	if m == nil {
		t.Fatal(`install.sh: line HISN_VERSION="X.Y.Z" not found`)
	}
	if got := string(m[1]); got != MinVersion {
		t.Fatalf("install.sh HISN_VERSION = %q, want MinHisnVersion %q (internal/sandbox/backend.go)", got, MinVersion)
	}
}

// TestDockerfileHisnVersionMatchesMinVersion keeps the sandbox image from
// shipping a hisn older than the one Wakil requires: the HISN_VERSION pin in
// the Dockerfile must equal MinHisnVersion, like the one in install.sh.
func TestDockerfileHisnVersionMatchesMinVersion(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)HISN_VERSION=([0-9]+\.[0-9]+\.[0-9]+)\b`).FindSubmatch(b)
	if m == nil {
		t.Fatal("Dockerfile: HISN_VERSION=X.Y.Z not found")
	}
	if got := string(m[1]); got != MinVersion {
		t.Fatalf("Dockerfile HISN_VERSION = %q, want MinHisnVersion %q (internal/sandbox/backend.go)", got, MinVersion)
	}
}
