package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
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

// A sessions directory that does not exist (wrong WAKIL_SESSIONS_DIR) must not
// make every owner dead: each session file would be ENOENT, and --apply would
// reject every quarantined plan. The real planGCSessionExists is used, with no
// skip, so this runs in every environment.
func TestPlanGCMissingSessionsDirKeepsAllOwners(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-sessions")
	t.Setenv("WAKIL_SESSIONS_DIR", missing)
	if agent.SessionsDir() != missing {
		t.Fatalf("SessionsDir = %q, want %q (WAKIL_SESSIONS_DIR must be honoured)", agent.SessionsDir(), missing)
	}
	for _, owner := range []string{"chat-a", "chat-b"} {
		if got := classifyPlanEntry("plan/current", owner, planGCSessionExists); got == planGCDead {
			t.Errorf("owner %q classified dead although the sessions directory is absent; --apply would reject it", owner)
		}
	}
}

// Unknown or incomplete arguments must fail, not run a dry-run that looks like a
// normal result (e.g. a misspelled --apply).
func TestPlanGCRejectsUnknownAndIncompleteArgs(t *testing.T) {
	for _, args := range [][]string{
		{"--aply"},
		{"-apply"},
		{"--project"},
		{"--store", "--apply"},
		{"--bogus"},
	} {
		if got := runPlanGC(args); got != ExitError {
			t.Errorf("runPlanGC(%q) = %d, want ExitError (%d)", args, got, ExitError)
		}
	}
}

// B2 (default store): with no --project, gc must resolve the CURRENT directory,
// not cfg.WorkspacePath(). A stale WAKIL_WORKSPACE_PATH (another project) must
// not change the answer. The default must equal --project <cwd> exactly, also
// when the cwd is reached through a symlink.
func TestPlanGCDefaultWorkspaceIsCwdAndMatchesProject(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "Coding", "wakil")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "coding")
	if err := os.Symlink(filepath.Join(base, "Coding"), alias); err != nil {
		t.Skip("symlinks unavailable")
	}
	other := t.TempDir()
	t.Setenv("WAKIL_WORKSPACE_PATH", other) // the wrong project, as on the host
	cwd := filepath.Join(alias, "wakil")
	t.Chdir(cwd)

	def, err := agentWorkspace(config.Config{}, "")
	if err != nil {
		t.Fatal(err)
	}
	proj, err := agentWorkspace(config.Config{}, cwd)
	if err != nil {
		t.Fatal(err)
	}
	if def != proj {
		t.Errorf("default workspace %q != --project <cwd> %q", def, proj)
	}
	wantReal, _ := filepath.EvalSymlinks(real)
	if def != wantReal {
		t.Errorf("default workspace = %q, want the resolved cwd %q (not WAKIL_WORKSPACE_PATH %q)", def, wantReal, other)
	}
	// Both spellings must produce the same store key as the session.
	if agent.MemoryDBPath(def) != agent.MemoryDBPath(real) {
		t.Errorf("store path differs for the symlinked and resolved spellings: %q vs %q",
			agent.MemoryDBPath(def), agent.MemoryDBPath(real))
	}
}
