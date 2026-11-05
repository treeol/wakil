# Wakil 0.4.3 — workspace root symlinks, plan gc store, locale, browser reaping

Version 0.4.3. No tag, no push, no release. Base: master `4b2d77d` (the 0.4.2 work;
there is no `v0.4.2` tag in this repository — the release range is `4b2d77d..HEAD`).
Out of scope: P1–P4 (hisn parity, see docs/PLAN-0.5.md), hisn profile generation, the
docker backend's behaviour, any project-ID change or state migration.

## Behaviour contracts (DO NOT ALTER)

1. No project-ID change, no migration, no state merge. The host data shows the ID is
   already stable across symlinked spellings (memory/5d8efbe880d17149 serves
   /home/valon/coding/wakil, /home/valon/Coding/wakil and /mnt/wakil).
2. Escape protection is not weakened: resolution happens BEFORE the containment check;
   symlinks pointing outside the workspace stay refused.
3. `plan gc` errors on a missing store and prints the store it opened.
4. User commands keep the user's locale; only internal helpers get LC_ALL=C.
5. The reaper kills only a group whose leader matches boot id + pid + start time +
   Wakil's user-data-dir.
6. No changes to hisn profile generation or the docker backend's behaviour.
7. No tag, no push, no release.

---

## B1 — Workspace root symlinks

### Root cause (verified)
`exec.NewDirectExecutor` (internal/exec/exec.go) stores the **unresolved** root.
`DirectExecutor.ConfinePath` resolves the candidate (`EvalSymlinks`, or the deepest
existing ancestor) and then calls `isInsideWorkspace(resolvedCandidate, root)`. When the
workspace is reached through a symlink, the resolved candidate lies under the real
directory while the root still names the alias, so every in-workspace path is refused:
`path "…/Coding/proj/a.go" is outside workspace "…/coding/proj"`.
Reproduced by `exec_symlink_root_test.go` on the old code; passes with the fix (already
in the working tree).

### Audit (every path-vs-root comparison)
`grep isInsideWorkspace|HasPrefix(…root…)` over internal/:
- `exec/exec_ops.go:189` (DockerExecutor.ConfinePath — the container path; root is a
  container path and is NOT resolved on the host — out of scope, unchanged).
- `exec/exec_ops.go:240` (DirectExecutor.ConfinePath) — fixed by constructor resolution.
- `exec/backend_executor.go:487` and `:777` — the sandbox-side root; resolution happens
  inside the sandbox via `readlink -f`, so the root is canonical there. Verified by
  reading the call sites before any change; no host-side EvalSymlinks is added.
- `exec/docker_worktree.go:88` — worktree root; audited, unchanged in this release.
- `tools/toolcap.go:213-230`, `memory/store.go:446-461` — already resolve both sides.
- `agent/agentsmd.go:80-93` — already resolves cwd and root.

### Changes
- Keep `NewDirectExecutor` resolution (already present).
- Add tests: symlink inside the workspace pointing **outside** is refused; symlink inside
  pointing inside is accepted; escape by `..` is refused; root `/` handled.
- Git tools: a test that reaches the repo through a temp symlink and runs
  git_show/git_blame through the direct executor (replaces reliance on host layout).

### Tests
- `TestDirectExecutorSymlinkedWorkspaceRootAcceptsInside` (existing).
- `TestDirectExecutorSymlinkEscapeRefused` (new).
- `TestGitShowHeadSucceedsDespiteTriggerPhrasesInContent` and
  `TestGitToolsLiveIntegration` must pass with the repo reached through a temp symlink.

---

## B2 — plan gc opens the real store

### Root cause (verified)
`cmd/wakil/plan_gc.go` resolves the store with `agent.MemoryDBPath(cfg.WorkDir)`.
In docker mode `cfg.WorkDir` is the **container** path `DockerAgentRoot(hostDir)` =
`/mnt/<basename>` (internal/config/config.go:1677, internal/sandbox/paths.go:14), not
a host path. The store key is `workspaceKey(ws)` = SHA-256 of the canonical string, so:
- `MemoryDBPath("/home/valon/Coding/wakil")` → `memory/5d8efbe880d17149/memory.db` (the
  real store with ~90 quarantined rows).
- `MemoryDBPath("/mnt/wakil")` → `memory/f3fa3243c6fc07e5/memory.db` (does not exist).
`memory.Open` runs `MkdirAll` and creates an empty DB at the wrong key, so gc reports
zeros with no error. The session's memory tools use `WorkspacePath()` (the host path),
which is why they see the rows. Two defects: a container path used as a host key, and
`Open` silently creating a store on a read-only command.

### Changes
- Resolve the workspace the way the session does: `--project <dir>` if given, else the
  effective host workspace (`WorkspacePath()` semantics: `WAKIL_WORKSPACE_PATH` → host
  dir). Never `cfg.WorkDir`.
- `--store <path>` overrides; must be an existing file.
- Existence check BEFORE opening: missing → `plan gc: no memory store for <ws> at <path>`
  and exit 1, no file created (open read-only path or stat first; do not call `Open` on a
  missing path).
- First output line: `store: <absolute path>`.
- `--apply` semantics unchanged (Reject, not delete).

### Tests
- Integration (real memory handlers): create a store via `handleMemoryPut` on
  `plan/current` in a tainted session (quarantined), close, run the gc resolver for the
  same project dir, expect those rows reported.
- No store → non-zero exit, error text, no file created.
- Store path printed first.
- `WorkDir` ≠ host path (container path given): gc still opens the host-keyed store.

---

## B3 and B4 — moved to docs/PLAN-0.4.4.md

Not in 0.4.3. See docs/PLAN-0.4.4.md for the locale and browser-reaping plans,
including the round-1 design gaps that must be closed before code.

---

## Open questions (round 1)

1. Which other path comparison is still unresolved? (Audit above; the backend-executor
   root is canonical inside the sandbox — confirm.)
2. What can the reaper kill that is not ours? (Only a leader matching the full tuple.)
3. What makes gc report success without a store? (Fixed: existence check before open;
   no `Open` on a missing path.)

## Open points — not done

- B3 (locale) and B4 (browser reaping): see docs/PLAN-0.4.4.md.
- `isInsideWorkspace` is shared by the Docker executors. Its `"/"` fix is latent (no
  current caller uses root `/`); it is covered by a test, not by a Docker change.
- `dockerWorktreeExecutor` and `DockerExecutor` compare container paths resolved by
  `readlink -f` inside the container. They are sound and were not changed.
- Full suite under `LANG=de_CH.UTF-8` is NOT a valid check in this sandbox: the locale
  is not installed (`locale -a` has no de_CH), so the variable falls back to C.
