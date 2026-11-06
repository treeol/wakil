package config

// TestMain isolates every test in this package from the user's real Wakil
// data and config directories via a private temp HOME/XDG tree, and fails the
// package if anything was created or appended under the real directories
// anyway. See internal/testutil. Without it, LoadConfig(nil) created
// ~/.config/wakil/config.json and the docker executor ~/.wakil/sandbox-home in
// the real home — which the guard of a package running at the same moment
// (internal/agent, under go test -p N) then reported as its own violation.

import (
	"os"
	"testing"

	"github.com/treeol/wakil/internal/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Main(m)) }
