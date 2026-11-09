package hisn

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

// TestInstallScriptHisnVersionMatchesMinVersion keeps install.sh from
// installing a hisn older than the one Wakil requires: HISN_VERSION in the
// installer must equal MinVersion.
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
		t.Fatalf("install.sh HISN_VERSION = %q, want MinVersion %q (internal/sandbox/hisn/backend.go)", got, MinVersion)
	}
}
