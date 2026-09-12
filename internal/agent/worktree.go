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
//   2. Child runs with an executor rooted at the worktree dir:
//      - DirectExecutor in direct mode (host filesystem).
//      - DockerWorktreeExecutor in docker mode (shares the parent's container,
//        re-rooted at the worktree dir inside container /tmp).
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
// startup. In direct mode, worktree dirs are under the system temp dir,
// prefixed with "wakil-wt-". In docker mode, worktree dirs are under
// container /tmp (tmpfs), which is wiped on container restart — but stale
// .git/worktrees/ metadata in the repo may remain and is cleaned selectively.
// Pruning checks the worktree's own .git gitdir pointer — not the current
// repo's worktree list — so worktrees belonging to other repos/sessions are
// never deleted.
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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/treeol/wakil/internal/exec"
)

// processAlive reports whether the given PID is still running. Uses
// kill(pid, 0) on Unix. On non-Unix platforms, always returns true (can't
// check — fail closed to avoid deleting live worktrees).
//
// Implementation is in process_alive_unix.go (//go:build !windows) and
// process_alive_windows.go (//go:build windows).
func processAlive(pid int) bool {
	return processAliveImpl(pid)
}

// patchApplyMu serializes patch application to the parent workspace. While
// children run in parallel in their own worktrees, applying patches back to
// the parent workspace must be one-at-a-time to avoid conflicts on the same
// files. The lock is held only during applyPatch, not during the child's run.
var patchApplyMu sync.Mutex

// worktreePrefix is the prefix for worktree directories in the temp dir.
// Used to identify and prune stale worktrees from crashed sessions.
const worktreePrefix = "wakil-wt-"

// worktreeOwnerFile is the name of the marker file written into each worktree
// directory. It contains the PID of the process that created the worktree,
// enabling pruneStaleWorktrees to detect stale worktrees from crashed sessions.
const worktreeOwnerFile = ".wakil-owner-pid"

// worktreeOpTimeout is the timeout for worktree diff/apply/cleanup operations.
// These run after the child finishes and should not use the (possibly
// cancelled) request context.
const worktreeOpTimeout = 30 * time.Second

// worktreeCleanupTimeout is the maximum time allowed for worktree cleanup
// after a child finishes. Short enough that it doesn't block the parent, long
// enough for git to remove the worktree directory.
const worktreeCleanupTimeout = 10 * time.Second

// isDockerExecutor returns true if the executor is a DockerExecutor (but not
// a dockerWorktreeExecutor, which embeds it).
func isDockerExecutor(e exec.Executor) bool {
	_, ok := e.(*exec.DockerExecutor)
	return ok
}

// isGitRepo checks whether the workspace root is inside a git repository.
// Uses the parent's executor (which runs from the workspace root). We check
// for `git rev-parse --is-inside-work-tree` rather than looking for a .git
// directory because submodules and worktrees have different layouts.
//
// Enabled for both DirectExecutor (host filesystem) and DockerExecutor
// (container filesystem). In docker mode, worktrees are created inside
// container /tmp (tmpfs), visible to in-container git but isolated from the
// parent workspace.
func isGitRepo(ctx context.Context, a *App) bool {
	// Worktree isolation requires a git repo accessible via the executor.
	// Both DirectExecutor and DockerExecutor are supported. The
	// dockerWorktreeExecutor is not — nested dispatch is not allowed and
	// the wrapper's WorkspaceRoot is a /tmp path, not the repo root.
	switch a.Exec.(type) {
	case *exec.DirectExecutor:
	case *exec.DockerExecutor:
	default:
		return false
	}
	out, err := a.Exec.RunShell(ctx, gitBaseEnv()+"git "+gitBaseArgs()+" rev-parse --is-inside-work-tree 2>/dev/null")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "true"
}

// createWorktree creates a new git worktree at a temp dir, detached at HEAD.
// Returns the worktree directory path (executor-visible path). The caller must
// call removeWorktree when done (typically via defer).
//
// The worktree is created from the parent workspace's current HEAD, so the
// child sees the committed state the parent sees at dispatch time. The
// --detach flag means the worktree is not on any branch — no branch to clean
// up later. Uncommitted changes in the parent are NOT copied (see the dirty-
// parent limitation in the file header).
//
// In direct mode, the temp dir is on the host filesystem (via os.MkdirTemp).
// In docker mode, the temp dir is inside the container's /tmp (via mktemp -d
// through the executor) so it's visible to in-container git.
func createWorktree(ctx context.Context, a *App) (string, error) {
	var dir string
	if isDockerExecutor(a.Exec) {
		// Docker mode: create temp dir inside the container via the executor.
		// mktemp -d creates a directory in TMPDIR (defaults to /tmp).
		out, err := a.Exec.RunShell(ctx, "mktemp -d -p /tmp "+worktreePrefix+"XXXXXX 2>&1")
		if err != nil {
			return "", fmt.Errorf("worktree: could not create temp dir: %s", strings.TrimSpace(out))
		}
		dir = strings.TrimSpace(out)
		// Remove the empty dir so git worktree add can create it fresh.
		_, _ = a.Exec.RunShell(ctx, "rm -rf "+shellQuote(dir))
	} else {
		// Direct mode: create temp dir on the host.
		tmp, err := os.MkdirTemp("", worktreePrefix)
		if err != nil {
			return "", fmt.Errorf("worktree: could not create temp dir: %w", err)
		}
		dir = tmp
		// Remove the empty dir so git worktree add can create it fresh.
		_ = os.RemoveAll(dir)
	}

	repoRoot := a.Exec.WorkspaceRoot()
	cmd := fmt.Sprintf("%sgit -C %s worktree add --detach %s HEAD 2>&1",
		gitBaseEnv(),
		shellQuote(repoRoot),
		shellQuote(dir))
	out, err := a.Exec.RunShell(ctx, cmd)
	if err != nil {
		// Cleanup the temp dir on failure.
		if isDockerExecutor(a.Exec) {
			_, _ = a.Exec.RunShell(ctx, "rm -rf "+shellQuote(dir))
		} else {
			_ = os.RemoveAll(dir)
		}
		return "", fmt.Errorf("git worktree add: %s", strings.TrimSpace(out))
	}

	// Write a session-ownership marker so pruneStaleWorktrees can detect
	// worktrees left behind by crashed sessions. The marker contains the
	// current process's PID — pruning checks if this PID is still alive.
	// The marker is stored in the worktree's gitdir (under the main repo's
	// .git/worktrees/<name>/), NOT in the worktree directory itself — this
	// avoids it appearing in git add -A / diffs.
	writeWorktreeOwnerMarker(a, dir)

	return dir, nil
}

// writeWorktreeOwnerMarker writes a file in the worktree's gitdir (not the
// worktree directory itself) containing the current process PID. This enables
// pruneStaleWorktrees to detect stale worktrees from crashed sessions (whose
// PID is no longer alive). Storing the marker in the gitdir avoids it
// appearing in git add -A / diffs.
func writeWorktreeOwnerMarker(a *App, wtDir string) {
	pid := os.Getpid()
	// The worktree's .git file points to the gitdir under the main repo's
	// .git/worktrees/<name>/. Read it to find the gitdir path.
	var gitDir string
	if isDockerExecutor(a.Exec) {
		ctx := context.Background()
		out, err := a.Exec.RunShell(ctx, "cat "+shellQuote(wtDir+"/.git")+" 2>/dev/null")
		if err != nil {
			return
		}
		gitDir = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "gitdir: "))
	} else {
		data, err := os.ReadFile(filepath.Join(wtDir, ".git"))
		if err != nil {
			return
		}
		gitDir = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir: "))
	}
	if gitDir == "" {
		return
	}
	markerPath := filepath.Join(gitDir, worktreeOwnerFile)
	if isDockerExecutor(a.Exec) {
		ctx := context.Background()
		_, _ = a.Exec.RunShell(ctx,
			fmt.Sprintf("echo %d > %s", pid, shellQuote(markerPath)))
	} else {
		_ = os.WriteFile(markerPath, []byte(fmt.Sprintf("%d\n", pid)), 0o644)
	}
}

// isWorktreeOwnerAlive checks whether the process that created the worktree
// is still running. Returns true if the owner marker is missing (legacy
// worktree or unknown — err on the side of keeping it).
func isWorktreeOwnerAlive(a *App, wtDir string) bool {
	// Read the worktree's .git file to find the gitdir path.
	var gitDir string
	if isDockerExecutor(a.Exec) {
		ctx := context.Background()
		out, err := a.Exec.RunShell(ctx, "cat "+shellQuote(wtDir+"/.git")+" 2>/dev/null")
		if err != nil {
			return true // can't read — keep it
		}
		gitDir = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(out), "gitdir: "))
	} else {
		data, err := os.ReadFile(filepath.Join(wtDir, ".git"))
		if err != nil {
			return true // can't read — keep it
		}
		gitDir = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(data)), "gitdir: "))
	}
	if gitDir == "" {
		return true // can't find gitdir — keep it
	}
	markerPath := filepath.Join(gitDir, worktreeOwnerFile)
	var pidStr string
	if isDockerExecutor(a.Exec) {
		ctx := context.Background()
		out, err := a.Exec.RunShell(ctx,
			"cat "+shellQuote(markerPath)+" 2>/dev/null")
		if err != nil {
			return true // can't read marker — keep it
		}
		pidStr = strings.TrimSpace(out)
	} else {
		data, err := os.ReadFile(markerPath)
		if err != nil {
			return true // can't read marker — keep it
		}
		pidStr = strings.TrimSpace(string(data))
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return true // invalid marker — keep it
	}
	// Check if the process is still alive. On Unix, kill(pid, 0) returns
	// nil if the process exists (or if we don't have permission to signal
	// it — but we're in the same user's processes, so that's unlikely).
	return processAlive(pid)
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
// The executor's RunShell trims trailing \r\n from output, which would
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
	//
	// In direct mode, use host os.CreateTemp (git runs on host).
	// In docker mode, use container /tmp via the executor (git runs in container).
	var patchPath string
	if isDockerExecutor(a.Exec) {
		// Create a temp file inside the container via mktemp, then write the
		// patch content via WriteFileBytes.
		out, err := a.Exec.RunShell(ctx, "mktemp -p /tmp wakil-patch-XXXXXX 2>&1")
		if err != nil {
			return false, false, fmt.Sprintf("could not create patch temp file: %s", strings.TrimSpace(out))
		}
		patchPath = strings.TrimSpace(out)
		defer func() {
			_, _ = a.Exec.RunShell(ctx, "rm -f "+shellQuote(patchPath))
		}()
		if _, err := a.Exec.WriteFileBytes(ctx, patchPath, []byte(patch)); err != nil {
			return false, false, fmt.Sprintf("could not write patch temp file: %v", err)
		}
	} else {
		patchFile, err := os.CreateTemp("", "wakil-patch-*.patch")
		if err != nil {
			return false, false, fmt.Sprintf("could not create patch temp file: %v", err)
		}
		patchPath = patchFile.Name()
		defer func() { _ = os.Remove(patchPath) }()
		if _, err := patchFile.WriteString(patch); err != nil {
			_ = patchFile.Close()
			return false, false, fmt.Sprintf("could not write patch temp file: %v", err)
		}
		if err := patchFile.Close(); err != nil {
			return false, false, fmt.Sprintf("could not close patch temp file: %v", err)
		}
	}

	// First, check if the patch applies cleanly (--check is a dry run that
	// doesn't modify the working tree or index).
	checkCmd := fmt.Sprintf("%sgit -C %s %s apply --check %s 2>&1",
		gitBaseEnv(),
		shellQuote(repoRoot),
		gitBaseArgs(),
		shellQuote(patchPath))
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

	// --check passed — capture pre-mutation state for each file in the patch
	// BEFORE applying it. This enables /rewind to revert worktree-isolated
	// subagent edits (card #211). The patch header lines (--- a/..., +++ b/...)
	// list the affected files; we extract parent-relative paths from them.
	for _, path := range patchFilePaths(patch) {
		// Resolve to canonical parent-workspace path before capturing.
		canon, err := a.Exec.ConfinePath(ctx, path)
		if err != nil {
			continue // skip paths that can't be confined
		}
		a.captureForCheckpoint(ctx, canon)
	}

	// Apply for real.
	applyCmd := fmt.Sprintf("%sgit -C %s %s apply %s 2>&1",
		gitBaseEnv(),
		shellQuote(repoRoot),
		gitBaseArgs(),
		shellQuote(patchPath))
	applyOut, applyErr := a.Exec.RunShell(ctx, applyCmd)
	if applyErr != nil {
		// --check passed but --apply failed — unexpected. The parent may be
		// partially modified. Report the error.
		return false, false, strings.TrimSpace(applyOut)
	}
	return true, false, ""
}

// patchFilePaths extracts the list of file paths affected by a unified diff
// patch. It parses the "+++ b/path" and "--- a/path" header lines to find the
// modified files. For new files, "+++ b/path" is used (--- is /dev/null).
// For deleted files, "--- a/path" is used (+++ is /dev/null).
// Returns parent-workspace-relative paths (without the "a/" or "b/" prefix).
func patchFilePaths(patch string) []string {
	var paths []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(patch, "\n") {
		var path string
		if strings.HasPrefix(line, "+++ ") {
			path = strings.TrimPrefix(line, "+++ ")
		} else if strings.HasPrefix(line, "--- ") {
			path = strings.TrimPrefix(line, "--- ")
		} else {
			continue
		}
		// Skip /dev/null (new or deleted files).
		if path == "/dev/null" {
			continue
		}
		// Strip the "a/" or "b/" prefix used by git diffs.
		if strings.HasPrefix(path, "a/") {
			path = strings.TrimPrefix(path, "a/")
		} else if strings.HasPrefix(path, "b/") {
			path = strings.TrimPrefix(path, "b/")
		}
		// Strip any trailing tab + timestamp.
		if idx := strings.IndexByte(path, '\t'); idx >= 0 {
			path = path[:idx]
		}
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		paths = append(paths, path)
	}
	return paths
}

// removeWorktree removes a git worktree and its directory. Best-effort —
// errors are not returned because cleanup must continue even on failure.
// Safe to call multiple times.
//
// Defense-in-depth: asserts the directory is under the temp dir with the
// wakil-wt- prefix before removing it. A bug elsewhere passing in the
// workspace root would be catastrophic without this check.
func removeWorktree(ctx context.Context, a *App, wtDir string) {
	if wtDir == "" {
		return
	}
	// Assert the path is under the temp dir with our prefix — never
	// remove a path that doesn't match (defense against catastrophic misuse).
	if !isWorktreePath(wtDir, a.Exec) {
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
	if isDockerExecutor(a.Exec) {
		// Docker mode: check and remove via executor (container /tmp).
		_, _ = a.Exec.RunShell(ctx, "rm -rf "+shellQuote(wtDir))
	} else {
		// Direct mode: check and remove via host os.
		if _, err := os.Stat(wtDir); err == nil {
			_ = os.RemoveAll(wtDir)
		}
	}
}

// isWorktreePath returns true if path is under the temp dir and starts with
// the wakil-wt- prefix. Used as a safety guard before deletion.
//
// In direct mode, the temp dir is the host's os.TempDir(). In docker mode,
// it's /tmp (container-internal). The path is matched against the namespace
// the executor sees — a container /tmp path is never confused with a host
// /tmp path.
func isWorktreePath(path string, executor exec.Executor) bool {
	abs, err := filepath.Abs(path)
	if err != nil {
		return false
	}

	var tmpDir string
	if isDockerExecutor(executor) {
		// Docker mode: temp dir is container /tmp.
		tmpDir = "/tmp"
	} else {
		// Direct mode: temp dir is host os.TempDir().
		tmpDir = os.TempDir()
		if resolved, err := filepath.EvalSymlinks(tmpDir); err == nil {
			tmpDir = resolved
		}
	}

	// In docker mode, don't try EvalSymlinks on the host — the path is
	// container-internal and won't resolve on the host.
	if !isDockerExecutor(executor) {
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			abs = resolved
		}
	}
	return strings.HasPrefix(abs, filepath.Join(tmpDir, worktreePrefix))
}

// pruneStaleWorktrees scans for stale wakil-wt-* worktrees and cleans them up.
//
// In direct mode, it scans the host temp dir (os.TempDir()) for stale
// worktree directories whose .git gitdir pointer targets a path that no
// longer exists. Worktrees whose gitdir pointer still resolves to a live
// .git directory are left alone. On ambiguous errors (permission denied,
// I/O error), the worktree is left alone — fail closed.
//
// In docker mode, the container's /tmp is tmpfs and wiped on container
// restart, so stale worktree directories don't survive. However, stale
// .git/worktrees/ metadata in the repo may remain (the repo is bind-mounted
// and persists). We selectively clean entries whose name has the wakil-wt-
// prefix, whose gitdir points under /tmp/wakil-wt-, and whose target no
// longer exists. This avoids deleting user-created host worktrees.
//
// Note: this does NOT invoke `git worktree prune` because it would also
// prune user-created worktrees that happen to be missing from the
// container's view.
func pruneStaleWorktrees(ctx context.Context, a *App) {
	if isDockerExecutor(a.Exec) {
		pruneStaleDockerWorktreeMetadata(ctx, a)
		return
	}

	// Direct mode: scan host temp dir.
	_ = ctx
	_ = a
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
				// No .git file — check if this is a partially-created worktree.
				// If the directory has only the owner marker (or is empty), it's
				// safe to remove. Otherwise, leave it (a concurrent session may
				// be mid-creating it).
				if isWorktreeDirSafeToRemove(fullPath) {
					_ = os.RemoveAll(fullPath)
				}
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
		// If the gitdir target exists, check if the creating session is still
		// alive. If the owner PID is dead, the worktree is stale (crashed
		// session) even though the repo is still present.
		_, statErr := os.Stat(gitDir)
		if statErr == nil {
			// Repo is still alive — check if the session that created this
			// worktree is still running.
			if !isWorktreeOwnerAlive(a, fullPath) {
				// Owner process is dead — stale worktree from a crashed session.
				_ = os.RemoveAll(fullPath)
				// Also clean up the .git/worktrees/<name>/ metadata in the
				// parent repo (card #229). The gitDir points to the
				// .git/worktrees/<name> directory — remove it too.
				_ = os.RemoveAll(gitDir)
			}
			continue
		}
		if !os.IsNotExist(statErr) {
			// Ambiguous error (permission, I/O) — fail closed: leave it.
			continue
		}
		// gitdir target is gone — the parent repo was deleted/moved. This
		// worktree is orphaned. Clean it up.
		_ = os.RemoveAll(fullPath)
		// The .git/worktrees/<name> metadata is also stale — the repo is
		// gone so the metadata dir is orphaned too. Remove it (card #229).
		_ = os.RemoveAll(gitDir)
	}
}

// isWorktreeDirSafeToRemove checks whether a directory that lacks a .git file
// is a partially-created worktree safe to remove, or a directory a concurrent
// session is mid-creating. Returns true only if the directory is empty
// (git worktree add hasn't run yet). Non-empty dirs are left alone.
func isWorktreeDirSafeToRemove(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false // can't read — don't remove
	}
	return len(entries) == 0
}

// pruneStaleDockerWorktreeMetadata selectively removes .git/worktrees/
// entries for wakil-wt-* worktrees whose gitdir target no longer exists
// inside the container. This runs inside the container via the executor.
//
// It does NOT use `git worktree prune` because that would also prune
// user-created worktrees that are invisible from the container's view
// (e.g., a worktree at /home/user/project-wt on the host).
//
// Safety: only entries matching wakil-wt-* prefix with gitdir pointing
// under /tmp/wakil-wt- are considered for removal. All others are left
// alone. The gitdir file contains a bare path (not "gitdir: <path>" —
// that format is in the worktree's own .git file). The existence check
// uses test -e (the target is a file, not a directory).
func pruneStaleDockerWorktreeMetadata(ctx context.Context, a *App) {
	repoRoot := a.Exec.WorkspaceRoot()
	// List .git/worktrees/ entries that have our prefix. Use newline
	// splitting (not strings.Fields) to handle paths with spaces.
	listCmd := fmt.Sprintf(
		`ls -d %s/.git/worktrees/%s* 2>/dev/null`,
		shellQuote(repoRoot), worktreePrefix)
	out, err := a.Exec.RunShell(ctx, listCmd)
	if err != nil {
		return // no entries or error — nothing to prune
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for _, line := range lines {
		wtMetaDir := strings.TrimSpace(line)
		if wtMetaDir == "" {
			continue
		}
		// Read the gitdir file inside .git/worktrees/<name>/gitdir.
		// This file contains a bare path like "/tmp/wakil-wt-XXXXXX/.git"
		// (NOT "gitdir: <path>" — that format is in the worktree's .git file).
		gitdirFile := wtMetaDir + "/gitdir"
		gitdirOut, gitdirErr := a.Exec.RunShell(ctx,
			"cat "+shellQuote(gitdirFile)+" 2>/dev/null")
		if gitdirErr != nil {
			continue // can't read gitdir — leave it
		}
		gitdirPath := strings.TrimSpace(gitdirOut)
		if gitdirPath == "" {
			continue // empty gitdir — leave it
		}
		// Only consider entries whose gitdir points under /tmp/wakil-wt-.
		// Use HasPrefix on the path after trimming, not Contains, to avoid
		// matching paths like /home/user/tmp/wakil-wt-x/.git (card #230).
		if !strings.HasPrefix(gitdirPath, "/tmp/"+worktreePrefix) {
			continue // not one of ours — leave it
		}
		// Check if the gitdir target still exists inside the container.
		// Use test -e (not -d) — the target is a .git file, not a directory.
		existsOut, _ := a.Exec.RunShell(ctx, "test -e "+shellQuote(gitdirPath)+" 2>/dev/null && echo yes || echo no")
		if strings.TrimSpace(existsOut) == "yes" {
			continue // worktree still exists — leave it
		}
		// gitdir target is gone — stale metadata. Remove the .git/worktrees/<name> entry.
		_, _ = a.Exec.RunShell(ctx, "rm -rf "+shellQuote(wtMetaDir))
	}
}
