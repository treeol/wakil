package agent

// worktree.go — Git worktree isolation for parallel edit-tier subagents.
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
//   1. captureWorktreeSnapshot: stage the parent's working tree (tracked +
//      non-ignored untracked files) in a temp index, write a tree, create a
//      dangling commit. The parent's index/HEAD are untouched.
//   2. createWorktree: `git worktree add --detach <dir> <snapshot-commit>` —
//      the worktree starts from the snapshot, NOT HEAD. The child sees the
//      same code the parent sees (including uncommitted changes).
//   3. Child runs with an executor rooted at the worktree dir:
//      - DirectExecutor in direct mode (host filesystem).
//      - DockerWorktreeExecutor in docker mode (shares the parent's container,
//        re-rooted at the worktree dir inside container /tmp).
//   4. diffWorktree: `git diff --cached --binary HEAD` in the worktree —
//      since HEAD is the snapshot, this produces ONLY the child's delta
//      (parent dirty changes are in the baseline, not in the patch). Includes
//      binary files and new files (staged first with `git add -A`).
//   5. applyPatch: `git apply --check` then `git apply` in the parent
//      workspace — serialized by patchApplyMu. We use `--check` first to
//      atomically test whether the patch applies cleanly; if it does, we
//      apply it. If `--check` fails, the parent workspace is untouched.
//   6. removeWorktree: `git worktree remove --force <dir>` — cleans up.
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
// .gitignore'd files (deps, build outputs, .env) are not present in the
// worktree. The child cannot read or modify them. This is a security
// improvement, not a limitation.

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

// pruneWorktreesTimeout is the maximum time allowed for stale worktree cleanup
// at session start. Pruning is best-effort; in docker mode this bounds the
// wall-clock time (RunShell honors ctx via exec.CommandContext). In direct
// mode, ctx is checked between entries but cannot interrupt a single blocked
// filesystem syscall (os.Stat, os.RemoveAll). On timeout, a warning is printed
// and remaining stale worktrees are left for the next session's prune.
const pruneWorktreesTimeout = 10 * time.Second

// worktreeOpTimeout is the timeout for worktree diff/apply/cleanup operations.
// These run after the child finishes and should not use the (possibly
// cancelled) request context.
const worktreeOpTimeout = 30 * time.Second

// worktreeVerifyTimeout is the timeout for post-edit verification commands
// (e.g. go build). Separate from worktreeOpTimeout so a slow build doesn't
// consume the patch-application deadline. Verification is advisory — a timeout
// is reported as "verification timed out," not "build failed."
const worktreeVerifyTimeout = 120 * time.Second

// worktreeCleanupTimeout is the maximum time allowed for worktree cleanup
// after a child finishes. Short enough that it doesn't block the parent, long
// enough for git to remove the worktree directory.
const worktreeCleanupTimeout = 10 * time.Second

// isDockerExecutor returns true if the executor keeps its worktree temp dirs in
// a sandbox-private /tmp (docker, hisn): they have no host representation, so
// every operation on them must go through the executor. The name is historical.
func isDockerExecutor(e exec.Executor) bool {
	wh, ok := e.(exec.WorktreeHost)
	return ok && wh.WorktreeKind() == exec.WorktreeSandboxTmp
}

// sandboxTmp returns the executor's sandbox-private temp dir (container or
// session /tmp). Only meaningful when isDockerExecutor(e).
func sandboxTmp(e exec.Executor) string {
	if wh, ok := e.(exec.WorktreeHost); ok {
		if d := wh.SandboxTempDir(); d != "" {
			return d
		}
	}
	return "/tmp"
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
	wh, ok := a.Exec.(exec.WorktreeHost)
	if !ok || wh.IsWorktreeChild() {
		return false
	}
	out, err := a.Exec.RunShell(ctx, gitBaseEnv()+"git "+gitBaseArgs()+" rev-parse --is-inside-work-tree 2>/dev/null")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) == "true"
}

// createWorktree creates a new git worktree at a temp dir, detached at a
// snapshot of the parent's current working tree (tracked + non-ignored
// untracked files). Returns the worktree directory path (executor-visible
// path). The caller must call removeWorktree when done (typically via defer).
//
// The worktree baseline is NOT HEAD — it is a synthetic commit capturing the
// parent's working-tree content at dispatch time. This means:
//   - The child sees uncommitted parent changes (the most common case in an
//     interactive coding session).
//   - The child sees non-ignored untracked files (deps, build outputs that
//     aren't .gitignore'd).
//   - diffWorktree's `git diff --cached --binary HEAD` produces ONLY the
//     child's delta — parent dirty changes are in the baseline, not in the
//     patch. A no-op child produces an empty patch even when the parent is
//     dirty.
//
// The snapshot is created by staging the parent's working tree (`git add -A`)
// in a temporary index, writing a tree object (`git write-tree`), and creating
// a dangling commit (`git commit-tree`). This avoids touching the parent's
// index, working tree, or HEAD. The dangling objects are cleaned up by git gc
// later.
//
// Snapshot capture holds patchApplyMu to prevent a sibling's patch from
// changing the parent workspace mid-snapshot (torn baseline). This serializes
// worktree creation, which is cheap relative to the child run.
//
// On any failure (snapshot, worktree add, etc.), the worktree is cleaned up
// and the error is returned so the caller falls back to serialized mode.
//
// In direct mode, the temp dir is on the host filesystem (via os.MkdirTemp).
// In docker mode, the temp dir is inside the container's /tmp (via mktemp -d
// through the executor) so it's visible to in-container git.
func createWorktree(ctx context.Context, a *App) (string, error) {
	// Capture a coherent snapshot of the parent's working tree under
	// patchApplyMu so a sibling can't apply a patch mid-snapshot.
	patchApplyMu.Lock()
	snapshotOID, snapErr := captureWorktreeSnapshot(ctx, a)
	patchApplyMu.Unlock()
	if snapErr != nil {
		return "", fmt.Errorf("worktree snapshot: %w", snapErr)
	}

	var dir string
	if isDockerExecutor(a.Exec) {
		out, err := a.Exec.RunShell(ctx, "mktemp -d -p "+sandboxTmp(a.Exec)+" "+worktreePrefix+"XXXXXX 2>&1")
		if err != nil {
			return "", fmt.Errorf("worktree: could not create temp dir: %s", strings.TrimSpace(out))
		}
		dir = strings.TrimSpace(out)
		_, _ = a.Exec.RunShell(ctx, "rm -rf "+shellQuote(dir))
	} else {
		tmp, err := os.MkdirTemp("", worktreePrefix)
		if err != nil {
			return "", fmt.Errorf("worktree: could not create temp dir: %w", err)
		}
		dir = tmp
		_ = os.RemoveAll(dir)
	}

	repoRoot := a.Exec.WorkspaceRoot()
	cmd := fmt.Sprintf("%sgit -C %s %s worktree add --detach %s %s 2>&1",
		gitBaseEnv(),
		shellQuote(repoRoot),
		gitBaseArgs(),
		shellQuote(dir),
		shellQuote(snapshotOID))
	out, err := a.Exec.RunShell(ctx, cmd)
	if err != nil {
		if isDockerExecutor(a.Exec) {
			_, _ = a.Exec.RunShell(ctx, "rm -rf "+shellQuote(dir))
		} else {
			_ = os.RemoveAll(dir)
		}
		return "", fmt.Errorf("git worktree add: %s", strings.TrimSpace(out))
	}

	writeWorktreeOwnerMarker(a, dir)
	return dir, nil
}

// captureWorktreeSnapshot creates a dangling commit from the parent's current
// working-tree content (tracked + non-ignored untracked files) and returns its
// OID. The parent's index, working tree, and HEAD are not modified.
//
// Steps:
//  1. Copy the parent's index to a temp file (so we can stage without touching
//     the parent's real index).
//  2. In the temp index: `git add -A` (stages all working-tree content
//     including untracked non-ignored files).
//  3. `git write-tree` → tree OID.
//  4. `git commit-tree <tree> -p HEAD` → dangling commit OID. Uses
//     GIT_AUTHOR_IDENTITY env vars to avoid depending on user config.
//  5. Clean up the temp index.
//
// The caller must hold patchApplyMu to ensure the parent's working tree is
// stable during the snapshot.
func captureWorktreeSnapshot(ctx context.Context, a *App) (string, error) {
	repoRoot := a.Exec.WorkspaceRoot()

	// Create a temp index file by copying the parent's current index.
	// In direct mode, use host temp files. In docker mode, use container /tmp.
	var tempIndexPath string
	if isDockerExecutor(a.Exec) {
		out, err := a.Exec.RunShell(ctx, "mktemp -p "+sandboxTmp(a.Exec)+" wakil-snap-idx-XXXXXX 2>&1")
		if err != nil {
			return "", fmt.Errorf("create temp index: %s", strings.TrimSpace(out))
		}
		tempIndexPath = strings.TrimSpace(out)
	} else {
		f, err := os.CreateTemp("", "wakil-snap-idx-*")
		if err != nil {
			return "", fmt.Errorf("create temp index: %w", err)
		}
		tempIndexPath = f.Name()
		f.Close()
	}
	// Clean up the temp index on all exit paths.
	cleanupTempIndex := func() {
		if isDockerExecutor(a.Exec) {
			_, _ = a.Exec.RunShell(ctx, "rm -f "+shellQuote(tempIndexPath))
		} else {
			_ = os.Remove(tempIndexPath)
		}
	}

	// Copy the parent's index to the temp index. Using the parent's index as
	// a starting point (instead of an empty index) keeps `git add -A`
	// incremental and avoids recording sparse-checkout paths as deletions.
	gitDir := filepath.Join(repoRoot, ".git")
	srcIndex := filepath.Join(gitDir, "index")
	var copyCmd string
	if isDockerExecutor(a.Exec) {
		copyCmd = fmt.Sprintf("cp %s %s 2>&1", shellQuote(srcIndex), shellQuote(tempIndexPath))
	} else {
		copyCmd = fmt.Sprintf("cp %s %s 2>&1", shellQuote(srcIndex), shellQuote(tempIndexPath))
	}
	if out, err := a.Exec.RunShell(ctx, copyCmd); err != nil {
		cleanupTempIndex()
		return "", fmt.Errorf("copy parent index: %s", strings.TrimSpace(out))
	}

	// Stage all working-tree content using the temp index.
	addCmd := fmt.Sprintf("%sGIT_INDEX_FILE=%s git -C %s %s add -A 2>&1",
		gitBaseEnv(),
		shellQuote(tempIndexPath),
		shellQuote(repoRoot),
		gitBaseArgs())
	if out, err := a.Exec.RunShell(ctx, addCmd); err != nil {
		cleanupTempIndex()
		return "", fmt.Errorf("git add -A (snapshot): %s", strings.TrimSpace(out))
	}

	// Write the tree object from the temp index.
	writeTreeCmd := fmt.Sprintf("%sGIT_INDEX_FILE=%s git -C %s %s write-tree 2>&1",
		gitBaseEnv(),
		shellQuote(tempIndexPath),
		shellQuote(repoRoot),
		gitBaseArgs())
	treeOut, err := a.Exec.RunShell(ctx, writeTreeCmd)
	if err != nil {
		cleanupTempIndex()
		return "", fmt.Errorf("git write-tree: %s", strings.TrimSpace(treeOut))
	}
	treeOID := strings.TrimSpace(treeOut)
	if treeOID == "" {
		cleanupTempIndex()
		return "", fmt.Errorf("git write-tree returned empty OID")
	}

	// Create a dangling commit from the tree, with HEAD as parent.
	// Use explicit author/committer identity to avoid depending on user config.
	commitTreeCmd := fmt.Sprintf(
		"%sGIT_AUTHOR_NAME=wakil GIT_AUTHOR_EMAIL=wakil@local GIT_COMMITTER_NAME=wakil GIT_COMMITTER_EMAIL=wakil@local "+
			"git -C %s %s commit-tree %s -p HEAD -m wakil-snapshot 2>&1",
		gitBaseEnv(),
		shellQuote(repoRoot),
		gitBaseArgs(),
		shellQuote(treeOID))
	commitOut, err := a.Exec.RunShell(ctx, commitTreeCmd)
	if err != nil {
		cleanupTempIndex()
		return "", fmt.Errorf("git commit-tree: %s", strings.TrimSpace(commitOut))
	}
	commitOID := strings.TrimSpace(commitOut)
	if commitOID == "" {
		cleanupTempIndex()
		return "", fmt.Errorf("git commit-tree returned empty OID")
	}

	cleanupTempIndex()
	return commitOID, nil
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
	// H4: RunShell output is tail-capped at ShellOutputCap. A patch that hit
	// the cap is INCOMPLETE — applying a truncated patch would corrupt the
	// worktree, and the omission note would corrupt it too. Fail loudly
	// instead of truncating silently (completeness-critical caller).
	if strings.Contains(out, "bytes of output omitted") {
		return "", fmt.Errorf("worktree diff exceeds the shell output cap — too many/binary-large changes for snapshot-based apply; reduce the change set or commit directly in the worktree")
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
		out, err := a.Exec.RunShell(ctx, "mktemp -p "+sandboxTmp(a.Exec)+" wakil-patch-XXXXXX 2>&1")
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
	// subagent edits. The patch header lines (--- a/..., +++ b/...)
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
// patch. It parses "diff --git" headers, "+++"/"---" lines, and "rename
// from"/"rename to"/"copy from"/"copy to" metadata to find all affected files.
//
// For each file, it returns the parent-workspace-relative path (without the
// "a/" or "b/" prefix). Both the old (pre-patch) and new (post-patch) paths are
// captured so /rewind can restore renamed-away files.
//
// Decoding is done at each parse site (not in addPath) because the different
// header types use different formats: "diff --git" and "+++"/"---" carry an
// "a/" or "b/" transport prefix, while "rename from"/"rename to" carry bare
// repository-relative paths with no prefix.
func patchFilePaths(patch string) []string {
	var paths []string
	seen := make(map[string]bool)
	// addPath records a decoded, repository-relative path. It does NOT unquote,
	// strip prefixes, or truncate — each caller is responsible for producing a
	// fully decoded path (separate parsing from collection).
	addPath := func(path string) {
		if path == "" || path == "/dev/null" || seen[path] {
			return
		}
		seen[path] = true
		paths = append(paths, path)
	}
	// stripDiffPrefix removes the "a/" or "b/" transport prefix from a decoded
	// git diff path. Returns the path unchanged if neither prefix is present.
	stripDiffPrefix := func(path string) string {
		if strings.HasPrefix(path, "a/") {
			return strings.TrimPrefix(path, "a/")
		}
		if strings.HasPrefix(path, "b/") {
			return strings.TrimPrefix(path, "b/")
		}
		return path
	}
	// decodeDiffPath unquotes (if C-quoted), strips the a//b/ prefix, and
	// removes any trailing tab+timestamp. Used for "+++"/"---" lines and
	// "diff --git" header paths — all of which carry the transport prefix.
	// For quoted paths, the tab/timestamp is outside the quotes, so we split
	// BEFORE unquoting to avoid corrupting quoted filenames containing \t
	//.
	decodeDiffPath := func(raw string) string {
		// If the path is C-quoted, the tab/timestamp (if present) is outside
		// the closing quote. Find the closing quote and split there.
		if len(raw) > 0 && raw[0] == '"' {
			// Find closing quote (respecting backslash escapes).
			end := 1
			for end < len(raw) {
				if raw[end] == '\\' && end+1 < len(raw) {
					end += 2
					continue
				}
				if raw[end] == '"' {
					break
				}
				end++
			}
			if end < len(raw) {
				// Tab/timestamp is after the closing quote.
				raw = raw[:end+1]
			}
		} else {
			// Unquoted — strip trailing tab+timestamp directly.
			if idx := strings.IndexByte(raw, '\t'); idx >= 0 {
				raw = raw[:idx]
			}
		}
		decoded, err := unquoteGitPath(raw)
		if err != nil {
			return "" // malformed quoting — skip
		}
		return stripDiffPrefix(decoded)
	}
	// decodeRenamePath unquotes (if C-quoted) a bare "rename from"/"rename to"
	// path. These carry NO a//b/ prefix — the value is repository-relative as-is
	// (feeding through stripDiffPrefix would corrupt paths starting
	// with "a/" or "b/"). Only trailing \r\n (from line splitting) is stripped;
	// meaningful whitespace within the path is preserved.
	decodeRenamePath := func(raw string) string {
		decoded, err := unquoteGitPath(raw)
		if err != nil {
			return "" // malformed quoting — skip
		}
		return strings.TrimRight(decoded, "\r\n")
	}
	for _, line := range strings.Split(patch, "\n") {
		if strings.HasPrefix(line, "diff --git ") {
			// "diff --git a/<old> b/<new>" — present for ALL diff types (text,
			// binary, renames, mode-only). parseDiffGitHeader returns the
			// decoded new (b/) path; the old (a/) path is also captured so
			// /rewind can restore a renamed-away file.
			oldPath, newPath := parseDiffGitHeader(line)
			addPath(newPath)
			addPath(oldPath)
			continue
		}
		// "rename from <path>" / "rename to <path>" — bare paths, no a//b/
		// prefix. Capture both so /rewind can restore the old name and remove
		// the new name.
		if rest, ok := strings.CutPrefix(line, "rename from "); ok {
			addPath(decodeRenamePath(rest))
			continue
		}
		if rest, ok := strings.CutPrefix(line, "rename to "); ok {
			addPath(decodeRenamePath(rest))
			continue
		}
		// "copy from <path>" / "copy to <path>" — same format as rename.
		// Capture both endpoints; the source is not mutated by a copy, but
		// capturing it is conservative.
		if rest, ok := strings.CutPrefix(line, "copy from "); ok {
			addPath(decodeRenamePath(rest))
			continue
		}
		if rest, ok := strings.CutPrefix(line, "copy to "); ok {
			addPath(decodeRenamePath(rest))
			continue
		}
		if strings.HasPrefix(line, "+++ ") {
			addPath(decodeDiffPath(strings.TrimPrefix(line, "+++ ")))
		} else if strings.HasPrefix(line, "--- ") {
			addPath(decodeDiffPath(strings.TrimPrefix(line, "--- ")))
		}
	}
	return paths
}

// parseDiffGitHeader extracts the old (a/) and new (b/) paths from a
// "diff --git a/old b/new" line. Both paths are returned decoded (C-unquoted,
// prefix stripped) and repository-relative. The old path is captured so /rewind
// can restore renamed-away files.
//
// Git quotes both names in a diff --git header if either needs quoting
// (quote_two in git's diff.c). Quoted paths are unambiguous: we tokenize by
// finding the closing quote of each.
//
// Unquoted paths with spaces are emitted UNQUOTED by git (git does not quote
// spaces alone), making the header ambiguous. Git's own resolver
// (git_header_name in diff.c) tries all possible splits and picks the one where
// the a/ and b/ halves produce identical paths. We mirror that: for unquoted
// headers, we scan for " b/" separators and accept the split only if both
// halves match after stripping the a//b/ prefix. If no split matches (e.g., a
// rename where old≠new), the header cannot be resolved unambiguously — we
// return empty strings and rely on rename from/to or ---/+++ metadata to
// supply the paths.
func parseDiffGitHeader(line string) (oldPath, newPath string) {
	rest := strings.TrimPrefix(line, "diff --git ")

	// Case 1: first path is C-quoted. Find its closing quote, then the second
	// path starts after the space separator (quoted or not).
	if len(rest) > 0 && rest[0] == '"' {
		// Find the closing quote (respecting backslash escapes).
		end := 1
		for end < len(rest) {
			if rest[end] == '\\' && end+1 < len(rest) {
				end += 2
				continue
			}
			if rest[end] == '"' {
				break
			}
			end++
		}
		if end >= len(rest) {
			return "", "" // unterminated quote
		}
		token1 := rest[:end+1] // includes quotes
		// Skip space(s) after the closing quote.
		pos := end + 1
		for pos < len(rest) && rest[pos] == ' ' {
			pos++
		}
		if pos >= len(rest) {
			return "", ""
		}
		// Second path: quoted or unquoted (if quoted, include to closing quote;
		// if unquoted, take the rest — unquoted second paths can't have spaces
		// in a valid git header since the space is the separator).
		var token2 string
		if rest[pos] == '"' {
			end2 := pos + 1
			for end2 < len(rest) {
				if rest[end2] == '\\' && end2+1 < len(rest) {
					end2 += 2
					continue
				}
				if rest[end2] == '"' {
					break
				}
				end2++
			}
			if end2 >= len(rest) {
				return "", "" // unterminated quote
			}
			token2 = rest[pos : end2+1]
		} else {
			token2 = rest[pos:]
			// Strip trailing whitespace/CR (git headers shouldn't have it, but
			// defensive against malformed input).
			token2 = strings.TrimRight(token2, " \r\n")
		}
		oldPath = decodeDiffToken(token1, "a/")
		newPath = decodeDiffToken(token2, "b/")
		return oldPath, newPath
	}

	// Case 2: unquoted. Scan for " b/" separators and find the split where
	// a/<left> and b/<right> produce matching paths (git's git_header_name
	// approach). This resolves the common case where old==new (spaces in path).
	// For renames where old≠new, no split matches → return "" and rely on
	// rename from/to metadata.
	for i := 0; i+3 <= len(rest); i++ {
		if rest[i] != ' ' || rest[i+1] != 'b' || rest[i+2] != '/' {
			continue
		}
		// Candidate split: a-side = rest[:i], b-side = rest[i+1:]
		// (b-side includes the "b/" prefix).
		aSide := rest[:i]
		bSide := rest[i+1:]
		// Both sides must start with their respective prefix.
		if !strings.HasPrefix(aSide, "a/") {
			continue
		}
		// Strip prefixes and compare.
		aPath := strings.TrimPrefix(aSide, "a/")
		bPath := strings.TrimPrefix(bSide, "b/")
		if aPath == bPath {
			// Match — this is the correct split.
			return aPath, bPath
		}
	}
	// No unambiguous split found. This happens for renames where old≠new
	// (the header is "diff --git a/old b/new" with old≠new and possibly
	// spaces). The paths will be supplied by rename from/to or ---/+++
	// metadata lines in patchFilePaths.
	return "", ""
}

// decodeDiffToken decodes a single path token from a "diff --git" header:
// C-unquotes if needed, then strips the given a//b/ prefix. Returns "" on
// unquote failure (malformed quoting).
func decodeDiffToken(token, prefix string) string {
	decoded, err := unquoteGitPath(token)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(decoded, prefix)
}

// unquoteGitPath unquotes a C-quoted git path. Git C-quotes paths that
// contain special characters (spaces, non-ASCII, etc.) by wrapping them in
// double quotes and escaping. This function reverses that quoting.
func unquoteGitPath(path string) (string, error) {
	if len(path) == 0 || path[0] != '"' {
		return path, nil // not quoted
	}
	// Use strconv.Unquote which handles Go/C-style quoting (same as git's).
	return strconv.Unquote(path)
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
		// Sandbox mode: temp dir is the sandbox-private temp dir.
		tmpDir = sandboxTmp(executor)
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
// pruneWarn prints a one-line notice when stale worktree cleanup is interrupted
// by context cancellation (timeout or user-initiated). The message distinguishes
// deadline-exceeded (timeout) from other cancellation causes so the user knows
// whether this is expected (best-effort cleanup hit its budget) or unexpected
// (the turn was cancelled). On a timeout, remaining stale worktrees are left
// for the next session's prune to clean up.
func pruneWarn(ctx context.Context, a *App) {
	if ctx.Err() == nil {
		return
	}
	if a.Out != nil {
		if ctx.Err() == context.DeadlineExceeded {
			fmt.Fprintf(a.Out, "· stale worktree cleanup timed out after %s — remaining entries deferred to next session\n", pruneWorktreesTimeout)
		} else {
			fmt.Fprintf(a.Out, "· stale worktree cleanup cancelled — remaining entries deferred to next session\n")
		}
	}
}

func pruneStaleWorktrees(ctx context.Context, a *App) {
	if isDockerExecutor(a.Exec) {
		pruneStaleDockerWorktreeMetadata(ctx, a)
		return
	}

	// Direct mode: scan host temp dir. ctx is checked between entries so a
	// timeout (pruneWorktreesTimeout) interrupts the scan between iterations.
	// A single blocked filesystem syscall (os.Stat, os.RemoveAll) cannot be
	// interrupted by ctx — the timeout bounds the number of entries, not each
	// individual syscall.
	tmpDir := os.TempDir()
	entries, err := os.ReadDir(tmpDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if ctx.Err() != nil {
			pruneWarn(ctx, a)
			return
		}
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
				// parent repo. The gitDir points to the
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
		// gone so the metadata dir is orphaned too. Remove it.
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
		if ctx.Err() != nil {
			pruneWarn(ctx, a)
			return
		}
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
		// matching paths like /home/user/tmp/wakil-wt-x/.git.
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

// verifyWorktreeResult holds the outcome of a post-edit verification run.
type verifyWorktreeResult struct {
	status  string // "passed", "failed", "timed_out", "unavailable", "skipped"
	command string // the command that was run (or "none" if skipped)
	output  string // bounded output (first ~2 KB of stdout+stderr)
}

// verifyWorktreeEdits runs a verification command inside the worktree after
// the child finishes editing but before the patch is applied to the parent.
// The verification is advisory (non-blocking): failures are reported to the
// parent model via a high-weight finding + uncertainty, but the patch is still
// applied (the parent model can decide whether to keep or revert).
//
// Command selection (first match wins):
//  1. Cfg.SubagentEditVerifyCommand (if set to "-", verification is disabled).
//  2. Auto-detection: if go.mod exists at the workspace root → "go build ./...".
//  3. No detection → skipped (reported as a short uncertainty).
//
// The command runs through the executor with cwd = worktree dir, using a
// dedicated bounded context (worktreeVerifyTimeout). In docker mode, the
// worktree dir is a container path and the executor handles it correctly.
//
// The worktree HEAD is the snapshot commit (parent's working tree at dispatch
// time), so a build failure may reflect pre-existing parent issues, not the
// child's edits. The result is labeled "worktree verification" to make this
// scope explicit.
func verifyWorktreeEdits(ctx context.Context, a *App, wtDir string) verifyWorktreeResult {
	cmd := a.resolveVerifyCommand()
	if cmd == "-" {
		return verifyWorktreeResult{status: "skipped", command: "disabled"}
	}
	if cmd == "" {
		return verifyWorktreeResult{status: "skipped", command: "none"}
	}

	// Run the command with cwd = worktree dir. The executor's RunShell runs
	// from WorkspaceRoot by default; we prefix with `cd <wtDir> &&` to change
	// into the worktree. shellQuote prevents path injection.
	fullCmd := fmt.Sprintf("cd %s && %s 2>&1", shellQuote(wtDir), cmd)
	out, err := a.Exec.RunShell(ctx, fullCmd)

	// Bound the output to ~2 KB for the summary.
	if len(out) > 2048 {
		out = out[:2048] + "\n… [output truncated]"
	}

	if err == nil {
		return verifyWorktreeResult{status: "passed", command: cmd, output: strings.TrimSpace(out)}
	}

	// Distinguish timeout from other failures. The executor returns a generic
	// error on non-zero exit; a timeout is indicated by ctx.Err() != nil.
	if ctx.Err() != nil {
		return verifyWorktreeResult{status: "timed_out", command: cmd, output: strings.TrimSpace(out)}
	}

	// Check for "command not found" — toolchain absent in the environment.
	lower := strings.ToLower(strings.TrimSpace(out))
	if strings.Contains(lower, "command not found") || strings.Contains(lower, "no such file or directory") {
		return verifyWorktreeResult{status: "unavailable", command: cmd, output: strings.TrimSpace(out)}
	}

	return verifyWorktreeResult{status: "failed", command: cmd, output: strings.TrimSpace(out)}
}

// resolveVerifyCommand determines which verification command to run, based on
// config override and auto-detection. Returns:
//   - "-" if verification is explicitly disabled.
//   - A non-empty command string if configured or auto-detected.
//   - "" if no command is available (verification is skipped).
func (a *App) resolveVerifyCommand() string {
	// Explicit override.
	if v := strings.TrimSpace(a.Cfg.SubagentEditVerifyCommand); v != "" {
		return v
	}

	// Auto-detection: Go projects (go.mod at workspace root).
	repoRoot := a.Exec.WorkspaceRoot()
	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err == nil {
		return "go build ./..."
	}

	// Docker mode: go.mod check needs to go through the executor.
	if isDockerExecutor(a.Exec) {
		ctx := context.Background()
		out, err := a.Exec.RunShell(ctx, "test -f "+shellQuote(filepath.Join(repoRoot, "go.mod"))+" 2>/dev/null && echo yes || echo no")
		if err == nil && strings.TrimSpace(out) == "yes" {
			return "go build ./..."
		}
	}

	return ""
}
