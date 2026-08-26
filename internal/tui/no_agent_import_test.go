package tui

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// goList runs go list with the sandbox-friendly environment (the repo's /tmp
// may be noexec — same GOTMPDIR rule as the outer test runner).
func goList(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Env = append(os.Environ(), "GOTMPDIR=/mnt/wakil/.tmp")
	out, err := cmd.Output()
	return string(out), err
}

// TestNoAgentImport (card #148 m4d, Gate #1 TUI half): internal/tui must not
// depend on internal/agent in PRODUCTION code. The go list -deps graph is the
// authoritative check (catches indirect imports too); test files are exempt
// (behavior tests of agent-package functions still live here until they move).
func TestNoAgentImport(t *testing.T) {
	out, err := goList(t, "list", "-deps", ".")
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "github.com/treeol/wakil/internal/agent") {
			t.Fatalf("tui dependency graph contains internal/agent — Gate #1 violated:\n%s", out)
		}
	}
}
