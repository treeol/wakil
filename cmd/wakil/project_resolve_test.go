package main

import (
	"os"
	"path/filepath"
	"strings"
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
// 0.5.0 (C1 fix, review on rubin): a chat's project key is the working directory,
// whatever the config says. The config's host_workdir / workdir / WAKIL_*_WORKDIR
// may point at another repository (the rubin host had a stale /home/valon/wakil-work
// from the environment); they must not key any store. With no project argument,
// the chat key equals ResolveProject(cwd) for every backend, and so does plan gc.
func TestResolveProjectMatchesChatForCwd(t *testing.T) {
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	t.Setenv("WAKIL_WORKSPACE_PATH", "")
	t.Setenv("WAKIL_HOST_WORKDIR", "")
	t.Setenv("ILM_HOST_WORKDIR", "")
	dir := t.TempDir()
	real := filepath.Join(dir, "proj")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "stale-other-repo") // a host_workdir pointing elsewhere
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	for _, c := range []string{real, link} {
		if err := os.Chdir(c); err != nil {
			t.Fatal(err)
		}
		gc, err := ResolveProject("")
		if err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"direct", "docker"} {
			for _, cfg := range []config.Config{
				{ExecMode: mode},                     // no host_workdir set
				{ExecMode: mode, HostWorkDir: stale}, // stale host_workdir: never keys a store
			} {
				chat := agent.ChatProjectPath(cfg)
				if agent.WorkspaceKey(chat) != agent.WorkspaceKey(gc) {
					t.Fatalf("mode=%s cwd=%s cfg.HostWorkDir=%q: chat key %s != gc key %s",
						mode, c, cfg.HostWorkDir, agent.WorkspaceKey(chat), agent.WorkspaceKey(gc))
				}
			}
		}
	}
}

func TestStripFlag(t *testing.T) {
	cases := []struct {
		in    []string
		want  []string
		found bool
	}{
		{[]string{"--print-workspace-key"}, nil, true},
		{[]string{"--exec", "direct", "-print-workspace-key", "/p"}, []string{"--exec", "direct", "/p"}, true},
		{[]string{"--exec", "direct"}, []string{"--exec", "direct"}, false},
		{nil, nil, false},
	}
	for _, c := range cases {
		got, found := stripFlag(c.in, "print-workspace-key")
		if found != c.found || strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("stripFlag(%v) = %v,%v want %v,%v", c.in, got, found, c.want, c.found)
		}
	}
}
