package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/treeol/wakil/internal/agent"
)

// Only a confirmed ENOENT may make an owner dead. A stat error that is not
// ENOENT (here: the sessions dir is a file, so stat of a child returns
// ENOTDIR) must keep the entry — never reject a row we could not attribute.
func TestPlanGCOwnerStatErrorKeepsEntry(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "sessions")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAKIL_SESSIONS_DIR", blocker)
	if agent.SessionsDir() != blocker {
		t.Skipf("SessionsDir does not honour WAKIL_SESSIONS_DIR here: %q", agent.SessionsDir())
	}
	if got := classifyPlanEntry("plan/current", "gone", planGCSessionExists); got == planGCDead {
		t.Fatalf("stat error classified owner as dead; must keep entry")
	}
	if got := classifyPlanEntry("plan/current", "gone", func(string) bool { return false }); got != planGCDead {
		t.Fatalf("confirmed absence should be dead, got %v", got)
	}
}
