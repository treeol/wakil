# 0010 — Open items carried forward from the process records

- **Status:** Accepted
- **Date:** 2026-11-07
- **Origin:** docs/PLAN-0.5.md, PLAN-0.4.4.md, CLOSEOUT-0.5.0.md open items (removed); docs/follow-ups.md.

## Open items

1. **Project-identity rename migration and `wakil state merge` (B1).** Not built.
   PLAN-0.5 defined the design: rename legacy store directories to the new ID when
   only the legacy one exists; never merge or overwrite when both exist; the merge
   command archives the sources. Requires the host's two IDs to be identified
   first. Must skip migration when `WAKIL_WORKSPACE_KEY` is set.
2. **`/resume --think` (claimed in 0.4.4, not present in source).** PLAN-0.4.4 M2 says
   `/resume` applies `--think`. The `/resume` handler (`internal/agent/commands.go:678`)
   has no think handling, and `git log -S think` over the handler finds no commit that
   added it. `--think` is a startup CLI flag applied in `internal/agent/repostate.go`.
   Left for 0.6: the fix is not implemented in the cleanup pass.
3. **Crashpad flags, `migrate` dry-run `--apply` offer at zero copyable, two
   flaky tests in `internal/agent` and the tests that leak into the user data
   directory.** From CLOSEOUT-0.5.0. The leak is documented in the decision
   sheet; the fix is to point tests at a temporary `XDG_DATA_HOME`/`HOME`.
4. **Browser tunnel stale session and screenshots in host `/tmp`** (CLOSEOUT-0.5.0).
5. **`plan gc` loads unused config; no browser reap after a crash; `mode=ro`
   store open** (PLAN-0.4.4 open points). The `mode=ro` decision for legacy
   databases is in PLAN-0.5.0 (legacy databases are never rewritten).
6. **`isInsideWorkspace` `/` handling** (PLAN-0.4.3, latent).
7. **Real de_CH locale run on a host** (PLAN-0.4.3/0.4.4). The sandbox falls back
   to C, so the German suite is not exercised there.
8. **Review gaps from F7 and F9** (ADR 0006, 0007): `git config` subcommand
   syntax (git ≥ 2.46), `validateExecEnv` breadth, and the hard-coded pair count.
9. **`TestResolveProjectGoldenKeys` hard-codes `/mnt/wakil`** (ADR 0004).
10. **Deferred items in `docs/follow-ups.md`**, moved there from the plan files.
    That file stays in `docs/` until its items are closed or moved to issues.

- **Auto-default hisn version gate is below the explicit gate.**
  `config.MinHisnDefaultVersion` is `0.3.1` (`internal/config/config.go`),
  but `MinHisnVersion` is `0.3.3`. A hisn 0.3.1 or 0.3.2 host can be chosen
  as the default and then refused at startup. The 0.5.2 CHANGELOG says 0.3.2
  is refused. Pre-existing at a18447e, unchanged by the cleanup. Fix in 0.6:
  raise the default gate to `MinHisnVersion` (behaviour change, so it is not
  part of this pass).

## Consequences

Each item stays open until it is built, verified, or moved to an issue. The
process records that listed them are removed in this cleanup. Their text remains
in git history.
