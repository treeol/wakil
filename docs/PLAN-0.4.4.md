# Wakil 0.4.4 — plan (design only, no code yet)

Scope: B3 (locale) and B4 (browser reaping). Both were moved out of 0.4.3 at the user's
direction. Nothing here is implemented. Code for B4 must not start until the `/proc`
trace in "B4 precondition" has been captured on the host.

---

## B3 — Locale

### Root cause (as established in 0.4.3 analysis)
`TestBackendExecutorLazyStartAndFileOps` fails under a German locale because an
executor path maps "not found" by matching English stderr text. Real `cat` under
`LANG=de_CH.UTF-8` prints `Datei oder Verzeichnis nicht gefunden`, which is not
matched.

**Verification gap.** In the 0.4.3 sandbox the `de_CH.UTF-8` locale is not installed
(`locale -a` has no `de_CH`), so the test passes there under the C fallback. That result
says nothing about B3. A valid check needs the locale generated (`locale-gen de_CH.UTF-8`
on the host, or a sandbox image with it).

### Boundary: RunShell vs internal helper `run`
- `RunShell` (user / model-requested commands) and the internal helper `run` share the
  same underlying exec path. Setting `LC_ALL=C` inside `run` would therefore also change
  user commands, which the contract forbids.
- The boundary must be drawn at the call site: helpers get an explicit env argument
  (`helperEnv`), `RunShell` keeps the inherited environment. This needs a code read of
  every `e.sh(…)` / `e.run(…)` caller before any change.

### Changes (proposed)
- Helper commands parsed by the executor get `LC_ALL=C`, `LANG=C` only.
- Error classification prefers exit status and a `test -e` probe; stderr text stays as a
  fallback (`fileErr`, `isShellNotFound`).
- DockerExecutor English matching stays unchanged (contract 6 of 0.4.3).

### Tests (proposed)
- Run with a generated `de_CH.UTF-8` locale installed: the lazy-start test passes.
- A user command in the same test (`echo $LANG`) still reports the user's locale.

---

## B4 — Browser reaping

### Root cause (as established)
`browser/manager.go` `newLocalManager` launches Chromium through chromedp's
`NewExecAllocator`. Wakil never sees the child PID and never sets a process group, so
there is no group to kill. `Close` only cancels contexts; Chromium's renderer, GPU and
utility children are not reliably reached and outlive the session.

### Identity tuple (recorded per chat, `<state>/browser/<chatID>.json`)
- `pgid`, `leader_pid`, `leader_start` (field 22 of `/proc/<pid>/stat`),
  `boot_id` (`/proc/sys/kernel/random/boot_id`), `user_data_dir` (Wakil-created per chat).

### Kill rule (round-1 requirement)
Kill the group (TERM → 3 s → KILL) **only if all hold**:
1. same boot id;
2. `pgid == leader_pid` (the group was created by this leader; a reused pgid owned by
   another process is never killed);
3. the leader pid still has the recorded start time;
4. the leader's cmdline contains the recorded `--user-data-dir`.

Any mismatch → do not kill, log one line, drop the record.

### Design gaps raised in round 1 (must be closed before code)
1. **Leader exits while children live.** The leader can exit and its children remain in
   the group. The leader identity is then gone, so rules 3 and 4 cannot be checked and the
   group cannot be safely killed by the tuple alone. Open: either keep the record keyed by
   pgid and check that the group still contains a process with the recorded user-data-dir
   in its cmdline, or accept a leak and report it. Not decided.
2. **Owner liveness model.** "Chat is gone" needs a defined source of truth (session file
   present, chat ID in the running daemon, or a lease). Not decided.
3. **Launch-failure rollback.** If the launch fails after the group is created (port not
   read, `DevToolsActivePort` missing), the group, the user-data-dir and the record must
   all be removed. Not specified.

### B4 precondition (blocking)
Before any code, capture on the host a `/proc` process-tree trace of a real Chromium
launch: `ps -eo pid,ppid,pgid,lstart,cmd` and `/proc/<pid>/cmdline` for the leader and
every child, plus what remains after the leader exits. The design above is not
implementable until that trace shows which processes stay in the group and which keep
the user-data-dir in argv.

### Lifecycle (proposed)
- Launch in its own group (`SysProcAttr{Setpgid:true}`); one browser per chat; a second
  launch reuses or replaces (with teardown) the existing one.
- Teardown on: tool close, `/new`, `/quit`, chat end, session teardown, Wakil exit, idle
  timeout (`browser.idle_timeout`, default 600 s). Teardown removes the user-data-dir.
- Startup reap: recorded browsers whose chat is gone get the kill rule; boot-id mismatch
  drops the record without killing.

### Tests (proposed)
- Launch → teardown leaves no process in the group (scan `/proc` for the pgid).
- Idle timeout reaps.
- Recorded tuple with a different start time, or a different boot id: NOT killed.
- Process without the user-data-dir in cmdline: NOT killed.
- Goroutine panic in the tool still reaps on chat end.
- Stub executable (shell script starting a child and grandchild in its group, with the
  user-data-dir in argv) where real Chromium is unavailable. The report must say which ran.

---

## Open points

- B4 design gaps 1–3 above.
- B4 precondition trace not captured.
- B3 locale generation on the host (`de_CH.UTF-8`) needed for a valid test.
