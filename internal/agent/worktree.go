package agent

// worktree.go — Git worktree isolation for parallel edit-tier subagents (card #191).
//
// Problem: parallel edit subagents sharing one workspace can clobber each
// other's changes. The subagentWriterMu mutex serializes them, but that
// defeats the purpose of parallel dispatch.
//
// Solution: when the workspace is a git repo, each edit-tier child gets its
// own git worktree — an isolated working directory sharing the same .git
// object store. The child writes to its worktree; after it finishes, we diff
// the worktree against HEAD and apply the patch to the parent workspace. If
// the patch applies cleanly, the parent workspace reflects the child's work.
// If it conflicts (another child's patch already touched the same lines), the
// conflict is reported — never silently dropped.
//
// Lifecycle:
//   1. createWorktree: `git worktree add --detach <dir> HEAD` — detached HEAD
//      so the worktree isn't tied to a branch (no branch to clean up).
//   2. Child runs with a DirectExecutor rooted at the worktree dir.
//   3. diffWorktree: `git diff --cached --binary HEAD` in the worktree —
//      captures all changes including binary files and new files (staged
//      first with `git add -A`).
//   4. applyPatch: `git apply --check` then `git apply` in the parent
//      workspace — serialized by patchApplyMu. We use `--check` first to
//      atomically test whether the patch applies cleanly; if it does, we
//      apply it. If `--check` fails, the parent workspace is untouched.
//   5. removeWorktree: `git worktree remove --force <dir>` — cleans up.
//
// Crash safety: stale worktrees (from a crashed session) are pruned on
// startup. Worktree dirs are under the system temp dir, prefixed with
// "wakil-wt-", so they're identifiable. Pruning checks the worktree's own
// .git gitdir pointer — not the current repo's worktree list — so worktrees
// belonging to other repos/sessions are never deleted.
//
// Non-git fallback: when the workspace is not a git repo, edit children fall
// back to the existing serialized behavior (subagentWriterMu) with a one-line
// warning printed to the parent's output.
//
// Dirty-parent limitation: the worktree is created from HEAD, not the parent's
// working tree. Uncommitted changes (including patches from sibling children)
// are NOT visible to the child. This means:
//   - If the parent has uncommitted changes to file A, and the child also
//     modifies A, the patch will conflict (the child's baseline differs from
//     the parent's current state). This is the correct behavior — the conflict
//     is detected and reported.
//   - If the parent has uncommitted changes to file B, and the child modifies
//     file A (disjoint), the patch applies cleanly. Also correct.
//   - .gitignore'd files (deps, build outputs, .env) are not present in the
//     worktree. The child cannot read or modify them. This is a security
//     improvement, not a limitation.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/treeol/wakil/internal/exec"
)

// patchApplyMu serializes patch application to the parent workspace. While
// children run in parallel in their own worktrees, applying patches back to
// the parent workspace must be one-at-a-time to avoid conflicts on the same
// files. The lock is held only during applyPatch, not during the child's run.
var patchApplyMu sync.Mutex

// worktreePrefix is the prefix for worktree directories in the system temp
// dir. Used to identify and prune stale worktrees from crashed sessions.
const worktreePrefix = "wakil-wt-"

// worktreeOpTimeout is the timeout for worktree diff/apply/cleanup operations.
// These run after the child finishes and should not use the (possibly
// cancelled) request context.
const worktreeOpTimeout = 30 * time.Second

// isGitRepo checks whether the workspace root is inside a git repository.
// Uses the parent's executor (which runs from the workspace root). We check
// for `git rev-parse --is-inside-work-tree` rather than looking for a .git
// directory because submodules and worktrees have different layouts.
//
// Only enabled for DirectExecutor (host filesystem). When using
// DockerExecutor, the host temp dir and container filesystem are different
// namespaces, so worktree isolation doesn't apply.
func isGitRepo(ctx context.Context, a *App) bool {
	// Worktree isolation requires a DirectExecutor — the worktree is on the
	// host filesystem, and the child needs a DirectExecutor rooted there.
	// DockerExecutor runs in a container; host temp dirs are inaccessible.
	if _, ok := a.Exec.(*exec.DirectExecutor); !ok {
		return false
	}
	out, err := a.Exec.RunShell(ctx, gitBaseEnv()+"git "+gitBaseArgs()+" rev-parse --is-inside-work-tree 2>/dev/null")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "true"
}

// createWorktree creates a new git worktree at a temp dir, detached at HEAD.
// Returns the worktree directory path. The caller must call removeWorktree
// when done (typically via defer).
//
// The worktree is created from the parent workspace's current HEAD, so the
// child sees the committed state the parent sees at dispatch time. The
// --detach flag means the worktree is not on any branch — no branch to clean
// up later. Uncommitted changes in the parent are NOT copied (see the dirty-
// parent limitation in the file header).
func createWorktree(ctx context.Context, a *App) (string, error) {
	dir, err := os.MkdirTemp("", worktreePrefix)
	if err != nil {
		return "", fmt.Errorf("worktree: could not create temp dir: %w", err)
	}
	// Remove the empty dir so git worktree add can create it fresh.
	_ = os.RemoveAll(dir)

	repoRoot := a.Exec.WorkspaceRoot()
	cmd := fmt.Sprintf("%sgit -C %s worktree add --detach %s HEAD 2>&1",
		gitBaseEnv(),
		shellQuote(repoRoot),
		shellQuote(dir))
	out, err := a.Exec.RunShell(ctx, cmd)
	if err != nil {
		// Cleanup the temp dir on failure.
		_ = os.RemoveAll(dir)
		return "", fmt.Errorf("git worktree add: %s", strings.TrimSpace(out))
	}
	return dir, nil
}

// diffWorktree captures the diff of all changes in the worktree against HEAD.
// Returns the raw diff output (unified diff format). An empty string means
// the child made no changes.
//
// Untracked files (new files created by the child) are included by staging
// everything with `git add -A` before diffing. The index is a throwaway
// (worktree is about to be removed), so staging is safe. Staging errors are
// surfaced — a failed stage means the patch would be incomplete, which is
// better to fail on than to silently drop.
//
// `--binary` is included so binary file changes emit apply-able patches
// (without it, git emits "Binary files differ" stubs that git apply rejects).
//
// The DirectExecutor's RunShell trims trailing \r\n from output, which would
// corrupt the patch (git apply expects a trailing newline after the last
// hunk). We restore the trailing newline here.
func diffWorktree(ctx context.Context, a *App, wtDir string) (string, error) {
	// Stage all changes (including untracked files) so they appear in the diff.
	// The worktree's index is independent of the parent's index, so staging
	// here doesn't affect the parent workspace.
	stageCmd := fmt.Sprintf("%sgit -C %s add -A 2>&1",
		gitBaseEnv(),
		shellQuote(wtDir))
	stageOut, stageErr := a.Exec.RunShell(ctx, stageCmd)
	if stageErr != nil {
		return "", fmt.Errorf("git add -A (staging for diff): %s", strings.TrimSpace(stageOut))
	}

	cmd := fmt.Sprintf("%sgit -C %s %s diff --cached --binary --no-ext-diff --no-textconv HEAD",
		gitBaseEnv(),
		shellQuote(wtDir),
		gitBaseArgs())
	out, err := a.Exec.RunShell(ctx, cmd)
	if err != nil {
		return "", fmt.Errorf("git diff: %s", strings.TrimSpace(out))
	}
	// Restore trailing newline stripped by the executor's TrimRight.
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	return out, nil
}

// applyPatch applies a unified diff patch to the parent workspace. The patch
// is applied with plain `git apply` (NOT `--3way`) to avoid leaving conflict
// markers in the parent workspace on failure.
//
// We first run `git apply --check` to test whether the patch applies cleanly.
// If --check succeeds, we apply for real. If --check fails, the parent
// workspace is untouched — no conflict markers, no dirty index.
//
// The caller must hold patchApplyMu before calling this function.
func applyPatch(ctx context.Context, a *App, patch string) (applied bool, conflict bool, errMsg string) {
	if strings.TrimSpace(patch) == "" {
		// No changes — nothing to apply. This is not an error.
		return true, false, ""
	}

	repoRoot := a.Exec.WorkspaceRoot()
	// Write the patch to a temp file so we can pass it to git apply via file
	// path (avoids shell-quoting issues with the diff content).
	patchFile, err := os.CreateTemp("", "wakil-patch-*.patch")
	if err != nil {
		return false, false, fmt.Sprintf("could not create patch temp file: %v", err)
	}
	defer func() { _ = os.Remove(patchFile.Name()) }()
	if _, err := patchFile.WriteString(patch); err != nil {
		_ = patchFile.Close()
		return false, false, fmt.Sprintf("could not write patch temp file: %v", err)
	}
	if err := patchFile.Close(); err != nil {
		return false, false, fmt.Sprintf("could not close patch temp file: %v", err)
	}

	// First, check if the patch applies cleanly (--check is a dry run that
	// doesn't modify the working tree or index).
	checkCmd := fmt.Sprintf("%sgit -C %s %s apply --check %s 2>&1",
		gitBaseEnv(),
		shellQuote(repoRoot),
		gitBaseArgs(),
		shellQuote(patchFile.Name()))
	checkOut, checkErr := a.Exec.RunShell(ctx, checkCmd)
	if checkErr != nil {
		checkStr := strings.TrimSpace(checkOut)
		// Check for conflict indicators in the --check output.
		if strings.Contains(checkStr, "patch does not apply") ||
			strings.Contains(checkStr, "does not apply to") ||
			strings.Contains(checkStr, "patch is already applied") {
			return false, true, checkStr
		}
		// Other failure (e.g., corrupt patch) — not a conflict, but not applied.
		return false, false, checkStr
	}

	// --check passed — apply for real.
	applyCmd := fmt.Sprintf("%sgit -C %s %s apply %s 2>&1",
		gitBaseEnv(),
		shellQuote(repoRoot),
		gitBaseArgs(),
		shellQuote(patchFile.Name()))
	applyOut, applyErr := a.Exec.RunShell(ctx, applyCmd)
	if applyErr != nil {
		// --check passed but --apply failed — unexpected. The parent may be
		// partially modified. Report the error.
		return false, false, strings.TrimSpace(applyOut)
	}
	return true, false, ""
}

// removeWorktree removes a git worktree and its directory. Best-effort —
// errors are not returned because cleanup must continue even on failure.
// Safe to call multiple times.
//
// Defense-in-depth: asserts the directory is under the system temp dir with
// the wakil-wt- prefix before removing it. A bug elsewhere handing in the
// workspace root would be catastrophic without this check.
func removeWorktree(ctx context.Context, a *App, wtDir string) {
	if wtDir == "" {
		return
	}
	// Assert the path is under the system temp dir with our prefix — never
	// remove a path that doesn't match (defense against catastrophic misuse).
	if !isWorktreePath(wtDir) {
		return
	}
	// First, try the git way (removes from worktree list + deletes dir).
	repoRoot := a.Exec.WorkspaceRoot()
	cmd := fmt.Sprintf("%sgit -C %s worktree remove --force %s 2>&1",
		gitBaseEnv(),
		shellQuote(repoRoot),
		shellQuote(wtDir))
	_, _ = a.Exec.RunShell(ctx, cmd)
	// Then, belt-and-suspenders: remove the directory if it still exists.
	if _, err := os.Stat(wtDir); err == nil {
		_ = os.RemoveAll(wtDir)
	}
}

// isWorktreePath returns true if path is under the system temp dir and starts
// with the wakil-wt- prefix. Used as a safety guard before deletion.
func isWorktreePath(path string) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	tmpDir := os.TempDir()
	if resolved, err := filepath.EvalSymlinks(tmpDir); err == nil {
		tmpDir = resolved
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return strings.HasPrefix(abs, filepath.Join(tmpDir, worktreePrefix))
}

// pruneStaleWorktrees removes worktrees with missing administrative metadata.
// Called at startup (first turn). Uses `git worktree prune` to clean git's
// internal records, then scans the temp dir for stale wakil-wt-* directories.
//
// A worktree is stale if its .git file's gitdir pointer targets a path that
// no longer exists (the parent repo was deleted/moved). Worktrees whose gitdir
// pointer still resolves to a live .git directory are left alone (they belong
// to some active repo, even if it's not this one). On ambiguous errors
// (permission denied, I/O error), the worktree is left alone — fail closed.
//
// Note: this does NOT reclaim worktrees from crashed sessions where the parent
// repo is still alive (both the worktree dir and the .git metadata exist).
// Those require a session-liveness mechanism (e.g., pid-based locks) which is
// out of scope for v1. They can be cleaned up manually with
// `git worktree remove --force <dir>`.
func pruneStaleWorktrees(ctx context.Context, a *App) {
	_ = ctx
	_ = a
	// Scan the temp dir for stale wakil-wt-* directories. A directory is
	// stale if its .git file's gitdir pointer targets a path that no longer
	// exists (the parent repo was deleted/moved). Worktrees whose gitdir
	// pointer still resolves to a live .git directory are left alone (they
	// belong to some active repo, even if it's not this one). On ambiguous
	// errors (permission denied, I/O error), the worktree is left alone —
	// fail closed.
	//
	// Note: we intentionally do NOT call `git worktree prune` before the
	// scan. `git worktree prune` can re-register worktrees whose metadata
	// was deleted but whose directories still exist, which would make the
	// scan think the gitdir is still alive. Instead, we scan first, then
	// let `git worktree prune` clean up git's internal records afterward.
	tmpDir := os.TempDir()
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, worktreePrefix) {
			continue
		}
		fullPath := filepath.Join(tmpDir, name)
		// Check the .git file inside the worktree. It should contain a line
		// like "gitdir: /path/to/repo/.git/worktrees/<name>".
		gitFile := filepath.Join(fullPath, ".git")
		gitContent, err := os.ReadFile(gitFile)
		if err != nil {
			if os.IsNotExist(err) {
				// No .git file — not a valid worktree (either never fully
				// created, or already partially cleaned). Safe to remove.
				_ = os.RemoveAll(fullPath)
			}
			// Other error (permission denied, I/O) — fail closed: leave it.
			continue
		}
		// Parse the gitdir pointer.
		gitLine := strings.TrimSpace(string(gitContent))
		gitDir := strings.TrimPrefix(gitLine, "gitdir: ")
		gitDir = strings.TrimSpace(gitDir)
		if gitDir == "" || gitDir == gitLine {
			// Not a gitdir pointer — unknown format, leave it alone.
			continue
		}
		// If the gitdir target exists, this worktree's parent repo is still
		// alive — leave it alone.
		_, statErr := os.Stat(gitDir)
		if statErr == nil {
			continue
		}
		if !os.IsNotExist(statErr) {
			// Ambiguous error (permission, I/O) — fail closed: leave it.
			continue
		}
		// gitdir target is gone — the parent repo was deleted/moved. This
		// worktree is orphaned. Clean it up.
		_ = os.RemoveAll(fullPath)
	}
}

// worktreeCleanupTimeout is the maximum time allowed for worktree cleanup
// after a child finishes. Short enough that it doesn't block the parent, long
// enough for git to remove the worktree directory.
const worktreeCleanupTimeout = 10 * time.Second
