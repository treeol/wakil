package agent

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"

	wakilexec "github.com/treeol/wakil/internal/exec"
)

// setupGitRepo creates a temp git repo with an initial commit and returns the
// path. The repo has one tracked file (hello.txt) so HEAD is not empty.
// Global git config is neutralized to avoid environment-dependent flakiness
// (e.g., commit.gpgsign, core.hooksPath, init.defaultBranch).
func setupGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// git init — use -b to set initial branch (avoids init.defaultBranch warning).
	cmd := osexec.Command("git", "init", "-b", "master", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	// Set per-repo config (avoids depending on global config).
	for _, kv := range []struct{ k, v string }{
		{"user.name", "Test User"},
		{"user.email", "test@example.com"},
		{"commit.gpgsign", "false"},
	} {
		cmd = osexec.Command("git", "-C", dir, "config", kv.k, kv.v)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config %s: %v\n%s", kv.k, err, out)
		}
	}
	// Create a file and commit it.
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello world\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cmd = osexec.Command("git", "-C", dir, "add", "hello.txt")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v\n%s", err, out)
	}
	cmd = osexec.Command("git", "-C", dir, "commit", "-m", "init")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v\n%s", err, out)
	}
	return dir
}

// newWorktreeTestApp creates an App with a DirectExecutor rooted at the given
// dir, with a strings.Builder for output. The minimum fields for worktree
// operations to work.
func newWorktreeTestApp(t *testing.T, dir string) *App {
	t.Helper()
	ex, err := wakilexec.NewDirectExecutor(dir)
	if err != nil {
		t.Fatalf("NewDirectExecutor: %v", err)
	}
	return &App{
		Exec: ex,
		Out:  &strings.Builder{},
	}
}

// TestIsGitRepo_DetectsGitRepo verifies that isGitRepo returns true for a
// directory inside a git repository.
func TestIsGitRepo_DetectsGitRepo(t *testing.T) {
	dir := setupGitRepo(t)
	app := newWorktreeTestApp(t, dir)
	if !isGitRepo(context.Background(), app) {
		t.Error("isGitRepo should return true for a git repo")
	}
}

// TestIsGitRepo_NonGitDir returns false for a plain directory outside any
// git repo. Note: t.TempDir() under GOTMPDIR may be inside the workspace
// (which IS a git repo), so we use /dev/shm which is never inside a repo.
func TestIsGitRepo_NonGitDir(t *testing.T) {
	dir, err := os.MkdirTemp("/dev/shm", "wakil-test-nongit-")
	if err != nil {
		t.Skipf("/dev/shm not available: %v", err)
	}
	defer os.RemoveAll(dir)
	app := newWorktreeTestApp(t, dir)
	if isGitRepo(context.Background(), app) {
		t.Error("isGitRepo should return false for a non-git directory")
	}
}

// TestCreateWorktree_CreatesWorktree verifies that createWorktree creates a
// valid git worktree directory with the same content as the parent repo.
func TestCreateWorktree_CreatesWorktree(t *testing.T) {
	dir := setupGitRepo(t)
	app := newWorktreeTestApp(t, dir)

	wtDir, err := createWorktree(context.Background(), app)
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	defer removeWorktree(context.Background(), app, wtDir)

	// The worktree dir should exist and contain hello.txt.
	if _, err := os.Stat(filepath.Join(wtDir, "hello.txt")); err != nil {
		t.Errorf("hello.txt should exist in worktree: %v", err)
	}

	// The worktree should be a git worktree (has a .git file, not directory).
	info, err := os.Stat(filepath.Join(wtDir, ".git"))
	if err != nil {
		t.Errorf(".git should exist in worktree: %v", err)
	} else if info.IsDir() {
		t.Error(".git in worktree should be a file (gitdir pointer), not a directory")
	}
}

// TestCreateWorktree_DetachedHEAD verifies the worktree is on detached HEAD.
func TestCreateWorktree_DetachedHEAD(t *testing.T) {
	dir := setupGitRepo(t)
	app := newWorktreeTestApp(t, dir)

	wtDir, err := createWorktree(context.Background(), app)
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	defer removeWorktree(context.Background(), app, wtDir)

	// Check HEAD is detached — `git -C <wt> symbolic-ref --short HEAD` should
	// fail (detached HEAD is not a symbolic ref).
	cmd := osexec.Command("git", "-C", wtDir, "symbolic-ref", "--short", "HEAD")
	if err := cmd.Run(); err == nil {
		t.Error("worktree HEAD should be detached, but symbolic-ref succeeded")
	}
}

// TestDiffWorktree_NoChanges returns empty for an unmodified worktree.
func TestDiffWorktree_NoChanges(t *testing.T) {
	dir := setupGitRepo(t)
	app := newWorktreeTestApp(t, dir)

	wtDir, err := createWorktree(context.Background(), app)
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	defer removeWorktree(context.Background(), app, wtDir)

	diff, err := diffWorktree(context.Background(), app, wtDir)
	if err != nil {
		t.Fatalf("diffWorktree: %v", err)
	}
	if diff != "" {
		t.Errorf("diff should be empty for unmodified worktree, got: %s", diff)
	}
}

// TestDiffWorktree_DetectsChanges returns the diff for a modified worktree.
func TestDiffWorktree_DetectsChanges(t *testing.T) {
	dir := setupGitRepo(t)
	app := newWorktreeTestApp(t, dir)

	wtDir, err := createWorktree(context.Background(), app)
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	defer removeWorktree(context.Background(), app, wtDir)

	// Modify a file in the worktree.
	if err := os.WriteFile(filepath.Join(wtDir, "hello.txt"), []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// Create a new file.
	if err := os.WriteFile(filepath.Join(wtDir, "new.txt"), []byte("new\n"), 0644); err != nil {
		t.Fatal(err)
	}

	diff, err := diffWorktree(context.Background(), app, wtDir)
	if err != nil {
		t.Fatalf("diffWorktree: %v", err)
	}
	if !strings.Contains(diff, "hello world") {
		t.Errorf("diff should contain original 'hello world' content: %s", diff)
	}
	if !strings.Contains(diff, "changed") {
		t.Errorf("diff should contain new 'changed' content: %s", diff)
	}
	if !strings.Contains(diff, "new.txt") {
		t.Errorf("diff should contain new file 'new.txt': %s", diff)
	}
}

// TestApplyPatch_AppliesCleanly verifies that a patch from a worktree applies
// to the parent workspace.
func TestApplyPatch_AppliesCleanly(t *testing.T) {
	dir := setupGitRepo(t)
	app := newWorktreeTestApp(t, dir)

	wtDir, err := createWorktree(context.Background(), app)
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	defer removeWorktree(context.Background(), app, wtDir)

	// Modify a file in the worktree.
	if err := os.WriteFile(filepath.Join(wtDir, "hello.txt"), []byte("changed\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Capture the diff.
	diff, err := diffWorktree(context.Background(), app, wtDir)
	if err != nil {
		t.Fatalf("diffWorktree: %v", err)
	}
	if diff == "" {
		t.Fatal("diff should not be empty after modifying worktree file")
	}

	// Apply the patch to the parent workspace.
	patchApplyMu.Lock()
	applied, conflict, applyErr := applyPatch(context.Background(), app, diff)
	patchApplyMu.Unlock()
	if !applied {
		t.Fatalf("applyPatch failed (conflict=%v): %s", conflict, applyErr)
	}

	// Verify the parent workspace now has the modified content.
	content, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "changed\n" {
		t.Errorf("parent workspace should have 'changed\\n', got: %q", string(content))
	}
}

// TestApplyPatch_ConflictDetected verifies that applying a conflicting patch
// returns conflict=true AND the parent workspace is left unchanged (no
// conflict markers, no dirty index).
func TestApplyPatch_ConflictDetected(t *testing.T) {
	dir := setupGitRepo(t)
	app := newWorktreeTestApp(t, dir)

	// Create two worktrees that both modify the same file.
	wtDir1, err := createWorktree(context.Background(), app)
	if err != nil {
		t.Fatalf("createWorktree 1: %v", err)
	}
	defer removeWorktree(context.Background(), app, wtDir1)

	wtDir2, err := createWorktree(context.Background(), app)
	if err != nil {
		t.Fatalf("createWorktree 2: %v", err)
	}
	defer removeWorktree(context.Background(), app, wtDir2)

	// Modify the same file differently in both worktrees.
	if err := os.WriteFile(filepath.Join(wtDir1, "hello.txt"), []byte("change from wt1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir2, "hello.txt"), []byte("change from wt2\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Apply patch 1 (should succeed).
	diff1, _ := diffWorktree(context.Background(), app, wtDir1)
	patchApplyMu.Lock()
	applied1, _, _ := applyPatch(context.Background(), app, diff1)
	patchApplyMu.Unlock()
	if !applied1 {
		t.Fatal("first patch should apply cleanly")
	}

	// Verify patch 1 was applied.
	content, _ := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if string(content) != "change from wt1\n" {
		t.Fatalf("parent should have 'change from wt1\\n' after first patch, got: %q", string(content))
	}

	// Apply patch 2 (should conflict — both changed the same line).
	diff2, _ := diffWorktree(context.Background(), app, wtDir2)
	patchApplyMu.Lock()
	applied2, conflict, applyErr := applyPatch(context.Background(), app, diff2)
	patchApplyMu.Unlock()
	if applied2 {
		t.Error("second patch should NOT apply (should conflict)")
	}
	if !conflict {
		t.Errorf("expected conflict=true, got applyErr: %s", applyErr)
	}

	// Critical: parent workspace should be unchanged — no conflict markers.
	content, _ = os.ReadFile(filepath.Join(dir, "hello.txt"))
	if string(content) != "change from wt1\n" {
		t.Errorf("parent should still have 'change from wt1\\n' after conflict, got: %q", string(content))
	}
	if strings.Contains(string(content), "<<<<<<<") {
		t.Error("parent should NOT contain conflict markers")
	}
}

// TestApplyPatch_EmptyPatch is a no-op and returns applied=true.
func TestApplyPatch_EmptyPatch(t *testing.T) {
	dir := setupGitRepo(t)
	app := newWorktreeTestApp(t, dir)

	patchApplyMu.Lock()
	applied, conflict, _ := applyPatch(context.Background(), app, "")
	patchApplyMu.Unlock()
	if !applied {
		t.Error("empty patch should be a no-op success")
	}
	if conflict {
		t.Error("empty patch should not report conflict")
	}
}

// TestRemoveWorktree_CleansUp verifies that removeWorktree removes the worktree
// from git and from the filesystem.
func TestRemoveWorktree_CleansUp(t *testing.T) {
	dir := setupGitRepo(t)
	app := newWorktreeTestApp(t, dir)

	wtDir, err := createWorktree(context.Background(), app)
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}

	removeWorktree(context.Background(), app, wtDir)

	// Directory should be gone.
	if _, err := os.Stat(wtDir); err == nil {
		t.Error("worktree directory should be removed")
	}

	// Git should no longer list it.
	cmd := osexec.Command("git", "-C", dir, "worktree", "list", "--porcelain")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git worktree list: %v\n%s", err, out)
	}
	if strings.Contains(string(out), wtDir) {
		t.Errorf("git should not list the removed worktree:\n%s", out)
	}
}

// TestRemoveWorktree_NonExistentIsSafe verifies that removing a non-existent
// worktree doesn't panic or error.
func TestRemoveWorktree_NonExistentIsSafe(t *testing.T) {
	dir := setupGitRepo(t)
	app := newWorktreeTestApp(t, dir)

	// Should not panic.
	removeWorktree(context.Background(), app, "/nonexistent/path/that/does/not/exist")
	removeWorktree(context.Background(), app, "")
}

// TestPruneStaleWorktrees_RemovesOrphaned verifies that pruneStaleWorktrees
// removes worktrees whose parent repo's .git directory no longer exists.
func TestPruneStaleWorktrees_RemovesOrphaned(t *testing.T) {
	dir := setupGitRepo(t)
	app := newWorktreeTestApp(t, dir)

	// Create a worktree.
	wtDir, err := createWorktree(context.Background(), app)
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}

	// Simulate a crash: remove the parent repo's .git directory (orphaning
	// the worktree). We can't remove the whole dir (we need it for the test),
	// so we remove the .git/worktrees entry to make the gitdir pointer stale.
	repoRoot := app.Exec.WorkspaceRoot()
	wtName := filepath.Base(wtDir)
	wtGitDir := filepath.Join(repoRoot, ".git", "worktrees", wtName)
	_ = os.RemoveAll(wtGitDir)

	// Now prune. The worktree's .git file points to the now-deleted gitdir.
	pruneStaleWorktrees(context.Background(), app)

	// Log the app output for debugging.
	if sb, ok := app.Out.(*strings.Builder); ok {
		t.Logf("prune output: %s", sb.String())
	}

	// The worktree directory should be gone (its gitdir target is stale).
	if _, err := os.Stat(wtDir); err == nil {
		t.Error("orphaned worktree directory should be removed by prune")
	}
}

// TestPruneStaleWorktrees_PreservesLiveWorktrees verifies that pruneStaleWorktrees
// does NOT remove worktrees whose parent repo is still alive (gitdir pointer
// still resolves).
func TestPruneStaleWorktrees_PreservesLiveWorktrees(t *testing.T) {
	dir := setupGitRepo(t)
	app := newWorktreeTestApp(t, dir)

	// Create a worktree — its gitdir pointer should still be valid.
	wtDir, err := createWorktree(context.Background(), app)
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	defer removeWorktree(context.Background(), app, wtDir)

	// Prune should NOT remove it.
	pruneStaleWorktrees(context.Background(), app)
	if _, err := os.Stat(wtDir); err != nil {
		t.Error("live worktree should NOT be removed by prune")
	}
}

// TestEndToEnd_WorktreeEditAndApply verifies the full flow: create worktree,
// child edits a file, diff, apply to parent, cleanup.
func TestEndToEnd_WorktreeEditAndApply(t *testing.T) {
	dir := setupGitRepo(t)
	app := newWorktreeTestApp(t, dir)

	// Create worktree.
	wtDir, err := createWorktree(context.Background(), app)
	if err != nil {
		t.Fatalf("createWorktree: %v", err)
	}
	defer removeWorktree(context.Background(), app, wtDir)

	// Simulate child edits: modify existing file + create new file.
	if err := os.WriteFile(filepath.Join(wtDir, "hello.txt"), []byte("updated content\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "new_file.go"), []byte("package main\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Diff.
	patch, err := diffWorktree(context.Background(), app, wtDir)
	if err != nil {
		t.Fatalf("diffWorktree: %v", err)
	}
	if patch == "" {
		t.Fatal("patch should not be empty")
	}

	// Apply to parent.
	patchApplyMu.Lock()
	applied, conflict, applyErr := applyPatch(context.Background(), app, patch)
	patchApplyMu.Unlock()
	if !applied {
		t.Fatalf("patch should apply cleanly (conflict=%v): %s", conflict, applyErr)
	}

	// Verify parent workspace has the changes.
	content, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "updated content\n" {
		t.Errorf("parent should have 'updated content\\n', got: %q", string(content))
	}
	if _, err := os.Stat(filepath.Join(dir, "new_file.go")); err != nil {
		t.Errorf("parent should have new_file.go: %v", err)
	}
}

// TestEndToEnd_DisjointEditsBothApplied verifies that two worktrees editing
// different files both have their patches applied to the parent (the key
// parallelism benefit of worktree isolation).
func TestEndToEnd_DisjointEditsBothApplied(t *testing.T) {
	dir := setupGitRepo(t)
	app := newWorktreeTestApp(t, dir)

	// Create two worktrees.
	wtDir1, _ := createWorktree(context.Background(), app)
	defer removeWorktree(context.Background(), app, wtDir1)
	wtDir2, _ := createWorktree(context.Background(), app)
	defer removeWorktree(context.Background(), app, wtDir2)

	// Each worktree modifies a different file.
	if err := os.WriteFile(filepath.Join(wtDir1, "hello.txt"), []byte("wt1 change\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wtDir2, "other.txt"), []byte("wt2 new file\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Diff both.
	patch1, _ := diffWorktree(context.Background(), app, wtDir1)
	patch2, _ := diffWorktree(context.Background(), app, wtDir2)

	// Apply both (serialized by patchApplyMu).
	patchApplyMu.Lock()
	applied1, _, _ := applyPatch(context.Background(), app, patch1)
	applied2, _, _ := applyPatch(context.Background(), app, patch2)
	patchApplyMu.Unlock()

	if !applied1 {
		t.Error("patch1 should apply cleanly")
	}
	if !applied2 {
		t.Error("patch2 should apply cleanly")
	}

	// Both changes should be in the parent.
	content1, _ := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if string(content1) != "wt1 change\n" {
		t.Errorf("parent should have 'wt1 change\\n', got: %q", string(content1))
	}
	content2, _ := os.ReadFile(filepath.Join(dir, "other.txt"))
	if string(content2) != "wt2 new file\n" {
		t.Errorf("parent should have 'wt2 new file\\n', got: %q", string(content2))
	}
}
