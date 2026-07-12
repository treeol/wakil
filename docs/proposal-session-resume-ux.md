# Proposal: workspace-scoped, easy-to-browse session resume

## Problem (as reported)

1. `--list-sessions` / `/sessions` / `/resume` completion show sessions from
   **every repo**, not just the current one — irrelevant noise.
2. Lists are newest-first, which in a scrolling terminal/note means the
   session you want is at the **top**, buried under everything else by the
   time you've scrolled — and there's no real interactive picker, just an
   8-row tab-completion dropdown you can only reach by first typing `/resume `.

## Current behavior (confirmed by reading the code)

- `internal/agent/session.go`: `Session.Workspace` is stored (`omitempty`) but
  **never filtered on** — `ListSessions()` returns everything, sorted
  newest-first. `LoadSession("")` (used by bare `/resume` and `wakil --resume`)
  returns `sessions[0]` — most recent **globally**.
- `cmd/wakil/main.go`: `--list-sessions` short-circuits *before config is
  loaded* (deliberately, so it works with no proxy configured).
  `--resume`/`--resume-id` are resolved before the executor/workspace exists.
- `internal/tui/complete.go`: `/resume <prefix>` tab-completion fetches all
  session short IDs, then **re-sorts them alphabetically** (not newest-first
  as it might appear) and caps display at 8 rows. It only activates once you
  type `/resume ` — there is no standalone picker.
- `internal/agent/repostate.go` already solves "identify this repo" for a
  different feature (per-folder terminal settings): `repoStateKey()` resolves
  a workspace path via `Abs` + `EvalSymlinks` (falling back to `Abs` alone if
  the path doesn't exist) and hashes it. This is the pattern to reuse, not
  duplicate.
- `App.SessionWorkspace()` returns `Cfg.WorkDir` in direct exec mode or
  `Cfg.HostWorkDir` in Docker mode — i.e. it's already the **host** path in
  both cases, so per-repo filtering is meaningful even under Docker.

## Mashūra review findings (3 independent reviewers, converged)

- Don't change `ListSessions()`'s global signature/ordering in place —
  `LoadSession("")`'s correctness depends on it staying newest-first
  internally; add an options-based filter instead of a breaking change.
- Sessions with empty `Workspace` (pre-existing/legacy) need an explicit
  policy — silently hiding them looks like data loss.
- "Per repo" should be defined as **canonical `SessionWorkspace()` path**
  (Abs + EvalSymlinks), same identity `repoStateKey` already uses — not git
  root, so distinct worktrees are distinct scopes (consistent with how
  repo-state already behaves).
- An ID/prefix the user typed explicitly should still resolve globally
  (so a hint like "resume with `<id>`" always works from anywhere); only the
  **no-argument** case should default to workspace-scoped.
- Ordering pain is display-layer, not storage-layer: reverse only at the
  point of printing a scrolling dump; keep the core list newest-first.
- Don't stretch the existing 8-row completion dropdown into a picker — build
  a dedicated modal state in the TUI; it's cleaner and avoids fighting the
  completion component's Enter/Tab semantics.
- Skip a second, pre-TUI interactive picker — not worth the cost; keep the
  CLI flag path fully non-interactive.

## Proposed design

### 1. Shared workspace-identity helper (new, small)

Extract the canonicalization already used by `repoStateKey` into a reusable
function so session filtering and repo-state use the same identity:

```go
// internal/agent/workspace.go
func canonicalWorkspace(ws string) string   // Abs + EvalSymlinks, Abs-only fallback
func workspaceKey(ws string) string         // sha256 of canonicalWorkspace(ws)
```

`repoStateKey` becomes a one-line wrapper around `workspaceKey`. No behavior
change there.

### 2. Non-breaking filtering API in `session.go`

Keep `ListSessions()` exactly as-is (global, newest-first — other code and
tests keep working). Add:

```go
type SessionScope struct {
    Workspace string // canonical match target; "" = current process has none
    All       bool   // ignore Workspace, return everything
}

func ListSessionsScoped(scope SessionScope) ([]Session, error)
func LoadSessionScoped(idOrPrefix string, scope SessionScope) (*Session, error)
```

Matching rule: `workspaceKey(s.Workspace) == workspaceKey(scope.Workspace)`.
**Sessions with empty `Workspace` are excluded from a scoped (non-`All`)
result** but the count of hidden sessions is reported so callers can print
a hint (`"3 sessions in other folders — /sessions all"`).

Rule for `LoadSessionScoped`: an **explicit** id/prefix always matches
against the full global list first (so `/resume <id>` and the
"resume with `<id>`" hints always work); only the **empty-id** ("give me the
latest") case is workspace-scoped.

### 3. Surfaces — each explicitly assigned an order/scope policy

| Surface | Scope default | Order | Notes |
|---|---|---|---|
| `wakil --list-sessions` | current workspace | **oldest-first** (newest ends up at the bottom, next to the prompt) | add `--list-sessions --all` for global; resolved via cwd since it runs pre-config |
| `wakil --resume` (no id) | current workspace | n/a (picks single most-recent) | non-interactive; clear error + hint if none: `no saved sessions for <dir>; try --resume --all` |
| `wakil --resume-id <id>` | global (explicit id) | n/a | unchanged from today |
| `/sessions` (TUI text note) | current workspace | newest-first | `/sessions all` for global; header states scope + hidden count |
| `/resume` **(bare, no argument)** | current workspace | newest-first, interactive | **opens the new modal picker** (see below) — this is the behavior change that directly answers "make selection as easy as possible" |
| `/resume <prefix>` | global (explicit) | n/a | unchanged direct-resume, no picker |
| `/resume all` | global | interactive | opens picker unscoped |

### 4. New TUI modal picker (the actual UX fix)

A dedicated modal state (not an extension of the completion dropdown):

- Rows show: short ID, label, updated time, turn count, first-message
  preview — same info `SessionListText` already computes, reused.
- Newest-first, all entries scrollable (not capped at 8) since it's a real
  list widget, not an inline dropdown.
- `↑`/`↓` or `j`/`k` navigate, `Enter` resumes, `Esc` cancels without
  mutating any state.
- A toggle key (e.g. `a`) switches current-workspace ↔ all-repos scope and
  re-sorts in place; the header shows which scope is active.
- Empty state: `"no sessions in this workspace — press a for all repos"`.
- Selection carries the full `ChatID` internally (never the ambiguous short
  ID) to avoid short-ID collisions once filtering changes match sets.
- Guard: if a turn/tool-confirmation is currently in flight, `/resume`
  (bare or with id) is rejected with a note rather than swapping `app.Conv`
  out from under a running turn (needs verifying against current command
  dispatch — flagged as a check, not assumed).

### 5. Decision points I want your sign-off on before implementing

1. **Bare `/resume` opens the picker instead of silently loading the most
   recent session.** This is a deliberate behavior change (today it's
   silent). I think it's the right trade for "make selection as easy as
   possible," but it does mean muscle-memory of typing `/resume` + Enter no
   longer instantly resumes — it now requires one more Enter in the picker.
   Alternative: keep bare `/resume` as silent-but-scoped (resume latest in
   *this* workspace, no picker), and only open the picker via a new explicit
   trigger (e.g. `/resume pick` or a dedicated key). Which do you want?
2. **`wakil --resume` (CLI, pre-TUI) becomes workspace-scoped by default**
   instead of global-latest. Same class of change, non-interactive. OK?
3. **Empty-`Workspace` legacy sessions are hidden by default** (visible via
   `all`/`--all`, with a "hidden: N" hint). OK, or would you rather they
   always show?

## Implementation outline (once decisions are confirmed)

1. `internal/agent/workspace.go` — extract `canonicalWorkspace`/`workspaceKey`;
   refactor `repoStateKey` to use it (no behavior change, covered by existing
   repo-state tests).
2. `internal/agent/session.go` — add `SessionScope`, `ListSessionsScoped`,
   `LoadSessionScoped`; keep existing functions as thin wrappers so all
   current callers/tests are unaffected until deliberately migrated.
3. Migrate call sites: `PrintSessions` (CLI, oldest-first dump + scope
   header), `SessionListText` (TUI note, scoped + hidden-count header),
   `main.go` resume handling, `agent_async.go` `/resume`/`/sessions` command
   handlers.
4. New `internal/tui/resume_picker.go` — modal state, render, key handling,
   wired into `tui.go`'s update/view alongside the existing completion/
   confirm overlays.
5. Tests: extend `session_test.go`/`session_extra_test.go` with scoped
   listing, empty-workspace exclusion, symlink-equivalence cases; add picker
   key-handling tests mirroring the existing `tui_select_test.go`/
   `complete_test.go` style; verify ordering invariants per-surface
   explicitly (CLI dump oldest-first, picker newest-first).
6. Update `PrintSessions`' footer text and `/help`/docs mentioning
   `/resume`/`--resume` semantics.

## What I have NOT verified yet (will confirm during implementation)

- Whether `/resume` is currently reachable while a turn is running (the
  "block during active generation" guard needs checking against the actual
  command-dispatch busy-state, not assumed).
- Whether `cmd/wakil/run.go` (headless `wakil run`) does anything with
  saved sessions — if so it needs the same scope decision applied
  consistently.
