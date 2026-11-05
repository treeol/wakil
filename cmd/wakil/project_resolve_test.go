package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
)

// Golden keys frozen at baseline (docs/CLEANUP-2026-11.md, A1). A change here
// orphans every existing store for /mnt/wakil and is a behaviour release.
const (
	goldenWsKey  = "f3fa3243c6fc07e588a42f82ee6ea59ba4d2c27cb028afbbbba79a0283a11ffb"
	goldenMemKey = "f3fa3243c6fc07e5"
)

func TestResolveProjectGoldenKeys(t *testing.T) {
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	t.Setenv("WAKIL_WORKSPACE_PATH", "")
	got, err := ResolveProject("/mnt/wakil")
	if err != nil {
		t.Fatal(err)
	}
	if k := agent.WorkspaceKey(got); k != goldenWsKey {
		t.Fatalf("workspace key drifted: got %s want %s", k, goldenWsKey)
	}
	if db := agent.MemoryDBPath(got); filepath.Base(filepath.Dir(db)) != goldenMemKey {
		t.Fatalf("memory store key drifted: got %s want %s", db, goldenMemKey)
	}
}

// Invariant (0.4.3 guarantee): with WAKIL_WORKSPACE_PATH unset, a chat started
// in directory C and plan gc run in C resolve to the same store, including when
// C is reached through a symlink, in docker and direct mode.
func TestResolveProjectMatchesChatForCwd(t *testing.T) {
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	t.Setenv("WAKIL_WORKSPACE_PATH", "")
	dir := t.TempDir()
	real := filepath.Join(dir, "proj")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{real, link} {
		for _, mode := range []string{"direct", "docker"} {
			cfg := config.Config{ExecMode: mode, WorkDir: c, HostWorkDir: c}
			chat := agent.ChatProjectPath(cfg)
			gc, err := ResolveProject(c)
			if err != nil {
				t.Fatal(err)
			}
			if agent.WorkspaceKey(chat) != agent.WorkspaceKey(gc) {
				t.Fatalf("mode=%s cwd=%s: chat key %s != gc key %s",
					mode, c, agent.WorkspaceKey(chat), agent.WorkspaceKey(gc))
			}
		}
	}
}
