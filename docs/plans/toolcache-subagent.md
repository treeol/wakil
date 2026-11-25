# Plan: archived subagent results unreadable after a collapse

BASE: 7b46148 (branch work/0.8.12). Trigger: bench log
bench/results/collapse-long-v2-1-022611.log — 4 consecutive read_file calls on
dispatch_subagent-*.txt spill files fail with "statat …/dispatch_subagen…",
the model concludes "The archived tallies aren't directly readable", loses
30 subtotals, and the run fails.

## Verified findings (file:line)

### 1. The spill write — synchronous, before the path is handed out

- internal/tools/toolcap.go:371-395 spillToDisk: MkdirAll then
  os.CreateTemp(cacheDir, safe+"-*.txt") then WriteString then Close,
  returns f.Name() only after Close; "" on MkdirAll/CreateTemp/write error
  (file removed).
- internal/tools/toolcap.go:402-404 SpillToCache calls spillToDisk with
  toolCacheDir(chatID); MkdirAll(cacheDir, 0o700) is at toolcap.go:375.
- Name scheme: toolName sanitized (non [A-Za-z0-9-_] → '-'), then
  safe+"-*.txt" random suffix — e.g. dispatch_subagent-1234567890.txt.
- SpillBase: toolCacheBase at toolcap.go:91-102: sibling of
  WAKIL_SESSIONS_DIR, else $XDG_DATA_HOME/wakil, else ~/.local/share/wakil.
  SpillDir: toolCacheDir at toolcap.go:107-113: base/toolcache/<chatID>.
- SpillToCache callers (all host-side): dispatch_subagent
  (renderSubagentResult), collapse archives (turn-N / turn-N-part),
  CapToolResult/StubToolResult/SpillFullResult, async envelope spill,
  prune round archives, repo_map, review_diff. Nothing downstream moves,
  renames, prunes or overwrites these files after creation:
  - SweepToolCache (internal/tools/toolcache_sweep.go:33) removes only
    per-chat dirs older than retention and never the active chat's dir
    (keep map honored); it is a startup-time background prune, not
    per-collapse.
  - os.Remove in collapse paths removes only archives this same pass just
    wrote and then discarded (T5 skip / session-changed), never an
    earlier path already handed out.
  - settleOffTask twice-removed debug comment at subagent.go:601 — no file
    touch.

### 2. The path handed to the model

- internal/agent/subagent_parallel.go:482-483 renderSubagentResult: after a
  successful SpillToCache the full JSON gets "\n[subagent summary at: %s]"
  appended — the path is handed out only if the write succeeded.
- Async dispatch_subagents workspace (internal/agent/async_subagents.go:249-303):
  per-child files are written before op.result is published; the merged
  envelope carries one "[<ChatID>]<rendered>" line per child with the
  subagent-summary-at marker inside the rendered body.
- The bench run used dispatch_subagents (async). Its tool result is the
  merged envelope (1.2KB) with the first child's marker inline; the first
  successful read at log line 81 was 16 lines / 30.4KB — the whole envelope
  spill from renderAsyncLine (internal/agent/async_ops.go:1306, which spills
  text over asyncEnvelopeOpCap with a "[full content at: PATH]" pointer).
- Tool-result markers also exist from CapToolResult ("+N chars omitted —
  TRUNCATED …", internal/tools/toolcap.go:437-450), StubToolResult
  ("[budget — N chars at: PATH]", toolcap.go:413) and SpillFullResult
  ("[full content at: PATH]", toolcap.go:509). ExtractSpillPath
  (toolcap.go:27-74) recognizes all of them plus "subagent summary at: ".
- Collapse notes hand out the archive path in "full record: %s (read_file
  with offset/limit — host-side path: shell cannot see it)"
  (internal/agent/collapse.go:377,390,759; handleCollapseContext
  collapse.go:1836 repeats the same warning).

### 3. read_file host intercept — and the "(not readable)" confusion

- internal/agent/tool_handlers.go:879 intercept: wtools.IsToolCacheHostPath
  → hostCacheReadResult → wtools.ReadHostCacheFileBounded / StatHostCacheFile
  (confined via os.Root at internal/tools/toolcap.go:200-258). Non-toolcache
  paths go to a.Exec.StatFile / ReadFileBounded after ConfinePath.
- Host read succeeded in the bench run for: turn-1-part-*.txt collapse
  archives (read_file_full 310B/317B/324B, read_file 159B, read_file
  16 lines / 30.4KB) and for search_files on the cache dir ("(no matches)",
  "32 lines · 8.0KB"). The 4 failing calls were read_file on
  dispatch_subagent-*.txt per-child files through the sandbox executor
  (BackendExecutor / Docker path) — the "(not readable)" outcome is the
  TUI adding " ✗" to the line count when !result.ok in resultSummary
  (internal/agent/app.go:4016-4018), i.e. the status flag, not the 40KB
  file itself. The agent's conclusion "aren't directly readable" came from
  that flag, and it discarded the (readable, host-served) archives.

### 4. Root cause — CORRECTED by Mashūra round 1 (both panels were right)

The §4/§5 analysis in the first draft of this plan was wrong. The
verification below REPLACES it. All claims here are now verified by code
inspection or by an executed check, cited inline.

The 4 failing read_file calls DID hit the host intercept. They did not
miss it, and the sandbox executor was never involved:

- The error op name "statat" is Go's `(*os.Root).Stat` PathError op:
  `go run` on Go 1.26.11: `Stat err type: *fs.PathError, formatting:
  statat nope.txt: no such file or directory`. resolveHostCachePath
  (internal/tools/toolcap.go:249-255) calls r.Stat(rel) on the opened
  *os.Root and returns that error unwrapped when it is not an escape
  error. The TUI line "· read_file … → ✗ statat
  a38333df-…/dispatch_subagen…" is that error verbatim (truncated to
  60 runes by resultSummary, internal/agent/app.go:4004-4005) — the rel
  path is "<chatID>/dispatch_subagent-XXXX.txt", which is exactly what
  the log shows.
- The §5 coreutils story is impossible on this path, three ways:
  handleReadFile discards the StatFile error (tool_handlers.go:911-916:
  `if fileSize, serr := a.Exec.StatFile(...); serr == nil && …`), the
  executor read is `head -c` (internal/exec/backend_executor.go:492),
  and fileErr maps not-found to ErrFileNotFound (backend_executor.go:
  464-477). A toolcache path that missed the intercept would also meet
  confineReadPath first ("outside workspace" — backend_executor.go:
  571-578), not a stat/head error.

So the real question is: why was the requested dispatch_subagent-*.txt
ENOENT on the host, while every other host-served read in the same run
succeeded? The following mechanism is verified by code; the
implementation phase will confirm the last step with a reproduction.

- Per-child spill files are written under the CHILD's chat dir:
  renderSubagentResult uses `a.chatID()` where `a` is the parent App —
  renders go to `toolcache/<PARENT-chatID>/`. Wait — which `a` does
  renderSubagentResult receive? internal/agent/subagent_parallel.go:
  479-483: `renderSubagentResult(a, …)` with `a` = parent. The async
  path (internal/agent/async_subagents.go:258) the same. Each child's
  file is `dispatch_subagent-<rand>.txt` under the PARENT's chat dir,
  handed out as "[subagent summary at: PATH]" (and via the merged
  envelope "[<ChatID>]<…>" lines).
- After the wait_for_completion delivery (log line 79 "async results
  delivered"), the model read the envelope spill (log line 81: 16
  lines · 30.4KB — the whole envelope), then read the per-child files
  (lines 85-94: 3.7KB, 2.6KB, 3.9KB …) — all succeeded through the
  host intercept — and then, after the mid-turn collapse at log line
  95, the 4 reads at lines 97-100 FAILED with host `statat` ENOENT.
- NOTHING DELETES THOSE FILES. Exhaustive audit of the repo for toolcache
  deletion: the only Remove/RemoveAll touching the toolcache tree is
  * SweepToolCache (internal/tools/toolcache_sweep.go:60) — startup-time,
    retention-gated, keep-list honored, newest-mtime rule protects live
    chats.
  * collapse.go os.Remove calls remove only archives the same pass just
    wrote and discarded (T5 skip, session-changed) — never an earlier,
    handed-out archive.
  No per-collapse, per-dispatch, or child-teardown path deletes anything
  under toolcache/<chatID>/. Verified by grep over internal/agent,
  internal/tools, internal/wiring for RemoveAll/Remove on the cache.
- Therefore the ENOENT is not deletion. The remaining mechanism is the
  one the implementation's Block 0 reproduction must pin down: the
  exact path string the model presented in those 4 calls did not match
  any file that exists — e.g. a tail fragment re-derived by the model
  from a truncated context copy (the collapse note truncates path lists
  to 12 entries; a handed-out archive path from BEFORE the collapse,
  copied from the summary instead of the exact envelope, would produce
  exactly this), or a canonicalized/altered spelling. What IS verified
  is the layering: the 4 calls classified as toolcache (intercept
  reached), the file under the root was absent (host ENOENT), and the
  model-visible error text was its own stdlib op name.

Fix direction (Block 1, pending the Block 0 reproduction):
- The reproduction must record the EXACT string passed to read_file and
  whether a file with that exact name exists under ToolCacheRoot() at
  hand-out and after child teardown. "Collapse twice, then read" is
  necessary but not sufficient — the failing case is a path that was
  handed out, then no longer matches reality in the model's copy.
- Candidate fix classes to evaluate (pick only what the reproduction
  proves):
  (a) a clearer host-missing error instead of raw `statat <rel>: …`,
      rendered by hostCacheReadResult, e.g. "archive file no longer
      exists — <path>" (touches only the host-cache path, keeps
      modules-off byte identity of the executor path);
  (b) if the reproduction shows a path string being mutated between
      hand-out and read, fix the mutation site (capSuffix /
      renderAsyncLine / collapse note) — NOT a retry loop;
  (c) ExtractSpillPath gap flagged by a reviewer: renderSubagentResult
      appends renderFilesChanged AFTER the "[subagent summary at: %s]"
      marker (subagent_parallel.go:482-488), and ExtractSpillPath
      requires the marker to be the trailing "[…]" segment
      (toolcap.go:27-74) — so for edit-tier children with files changed,
      the spill path is NOT recoverable by eviction/trim.
      Verify and fix if confirmed (append the marker last, or widen
      the recognizer). Keep it separate from the ENOENT fix unless
      the reproduction ties them together.
- Out of scope but list for Codex: read_file reads ANY path under the
  toolcache root, but search_files is scoped to OwnToolCachePath
  (own chat only) — a parent searching a child-dir path (or vice
  versa) misses the host intercept and hits ConfinePath instead.
  Related dead end, not this bug; list, don't fix here.
- "statat" legibility fix (non-blocking, same host-cache error path):
  render the missing-file host error as a plain sentence instead of the
  raw PathError, via hostCacheReadResult / resolveHostCachePath. The
  oxford in §5 (executor/fileErr/FileMode "stat %s:") is DROPPED — it
  addressed text that never occurred on this path.

### 5. The "statat" text — CORRECTED by Mashūra round 1

The first draft's §5 mechanism was wrong (see §4): "statat" is Go's
`(*os.Root).Stat` PathError op, confirmed by executing it on Go 1.26.11
(`statat nope.txt: no such file or directory`). It is produced when a
toolcache path IS classified and host-served, but the file is absent
under the root. There is no "statat" literal in the repo, and no
code path in the executor produces that word.

Consequences for the fix:
- DROP the planned edits to BackendExecutor.fileErr, FileMode
  ("stat %s: %s") and resultSummary — they addressed text that never
  occurred on this path, and changing model-facing ERROR strings on
  the executor path risks modules-off byte identity for no benefit.
- The legibility fix that remains (Block 1(a), optional, low risk):
  render a host-cache ENOENT as a plain sentence ("archive file no
  longer exists — <path>") at the hostCacheReadResult layer
  (internal/agent/app.go:3817-3822), so the model reads an actionable
  "the file is gone", not the stdlib op name "statat". This touches
  only the host-cache path, never executor strings. If the Block 0
  reproduction instead proves a path-mutation mechanism, that fix
  wins and this one is dropped.

### Touched paths (planned)

- internal/agent/app.go (hostCacheReadResult) — only if the clearer
  host-missing message is the proven fix.
- internal/agent/tool_handlers.go — only if the read intercept needs
  widening after the root cause is pinned (e.g. the search_files
  own-chat scope question, listed in §4 as a related dead end).
- internal/tools/toolcap.go — only if the spill side needs changing
  (e.g. the ExtractSpillPath / renderFilesChanged marker order).
- Tests: the Block 0 reproduction (new), existing collapse and
  subagent tests untouched. Modules-off identity checked in Gates.

### Blocks and their tests

Block 0 — Root-cause confirmation and failing reproduction:
  1. Pin the ENOENT mechanism from §4: assert, for every handed-out
     toolcache path, that the file EXISTS on disk at hand-out and after
     child teardown, and record the EXACT string the model presents
     to read_file (lexical vs canonical vs copied-from-summary). The
     reproduction must FAIL before the fix — if no input reproduces it,
     report that and ship no fix.
  2. A fake-backend test (direct/host executor is enough): dispatch
     subagents (async), collapse mid-turn ≥2 times, then read_file every
     path form recorded in (1) — assert all reads succeed. Include the
     search_files scope question (own-chat-only vs any-chat) as an
     explicit recorded finding, not a silent pass.
Block 1 — Fix:
  - Fix exactly what the Block 0 reproduction proves (candidate classes
    in §4): path-mutation fix, or the clearer host-missing message, or
    the ExtractSpillPath marker-order fix (c) — whichever the
    reproduction demands. No retries papering it over.
  - "statat" legibility (host-cache ENOENT rendered plainly) ONLY if
    the reproduction shows it is the actual pain point.
  - Tests: Block 0 reproduction passes; a walk-every-handed-out-path
    test across several collapses and subagent dispatches; existing
    collapse and subagent suites green. Modules-off identity checked
    per Gates.
Block 2 — CHANGELOG: one entry under [Unreleased].

### Open risks

- If the ENOENT was a model-reconstructed path string (not a Wakil
  routing/deletion bug), there is no code fix to make — the report must
  say so. The clearer host-missing message (Block 1(a)) is then the
  only ship-able change, and only if it measurably helps the model.
- If no mechanism reproduces an ENOENT, say so in the report — do not
  ship a no-op fix.
- Modules-off byte identity (verified in Gates) must be re-checked
  after any touching of path/error formatting code. The dropped §5
  executor edits are the highest-risk touch; the plan now excludes
  them.

## Mashūra round 1 — open question

"Is this the real root cause, and what else references toolcache files
that might not exist or not be readable?" Blocking verdicts only:
wrong root cause, another path with the same bug, or a change that breaks
modules-off byte identity. Everything else is listed, not blocking.
Max 2 rounds.
