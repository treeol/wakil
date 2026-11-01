package tui

import (
	"os"
	"testing"
)

// TestDumpSplashANSI writes the splash logo's real braille render to the file
// named by WAKIL_DUMP_SPLASH (used by scripts/gen_readme_logo.py to build the
// README image). Skipped unless that variable is set.
func TestDumpSplashANSI(t *testing.T) {
	path := os.Getenv("WAKIL_DUMP_SPLASH")
	if path == "" {
		t.Skip("WAKIL_DUMP_SPLASH not set")
	}
	out := pickSplashLogo(splashMaxCols, 100, true)
	if out == "" {
		t.Fatal("no logo rendered")
	}
	if err := os.WriteFile(path, []byte(out), 0o644); err != nil {
		t.Fatal(err)
	}
}
