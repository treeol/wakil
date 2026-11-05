package wiring

// TestMain isolates every test in this package from the user's real Wakil
// data and config directories (sessions, memory, input_history, toolcache,
// repo-state, config) via a private temp HOME/XDG tree, and fails the package
// if anything was created or appended under the real directories anyway. See
// internal/testutil. Tests that need their own location keep using t.Setenv,
// which overrides the temp tree for the test's duration.

import (
	"os"
	"testing"

	"github.com/treeol/wakil/internal/testutil"
)

func TestMain(m *testing.M) { os.Exit(testutil.Main(m)) }
