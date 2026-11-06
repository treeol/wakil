package connect

import (
	"os"
	"testing"

	"github.com/treeol/wakil/internal/testutil"
)

// TestMain isolates every test in this package from the user's real Wakil
// data and config directories via a private temp HOME/XDG tree, and fails the
// package if anything was created or appended under the real directories
// anyway. See internal/testutil. Without it, SaveRepoStateDescribe (via a bare
// agent.App whose workspace resolves to the package cwd) wrote a repo-state
// file under the real ~/.local/share/wakil/repo-state — which the guard of a
// package running at the same moment (internal/agent, internal/exec, under
// go test -p N) then reported as its own violation.
func TestMain(m *testing.M) { os.Exit(testutil.Main(m)) }
