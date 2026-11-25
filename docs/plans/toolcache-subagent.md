
## Findable archives — design (BASE 4c44045)

Scope: what remains after the 4c44045 fix. The "statat" legibility fix
(host ENOENT rendered plainly) already landed in 4c44045. What remains
broken: after several collapses the model has no stable, complete list
of archived results — per/archive spill names carry random suffixes that
leave context, and the model guesses (`statat` failures). Two related
open points also remain: (i) search_files is scoped to the own chat only
while read_file covers the whole toolcache root, so the 4c44045 error
text's "use search_files on the cache directory" hint fails for
child-dir archives; (ii) `renderSubagentResult` appends the files-changed
block after the `[subagent summary at: PATH]` marker, so trailing-marker
recovery (ExtractSpillPath) misses for edit-tier children.

### Verified findings (file:line, read directly in this session)

Spill write (synchronous, before hand-out):

- internal/tools/toolcap.go:371-395 spillToDisk: MkdirAll then
  os.CreateTemp(cacheDir, safe+"-*.txt") then WriteString then Close,
  returns f.Name() only after Close; "" on error (file removed).
- internal/tools/toolcap.go:402-404 SpillToCache calls spillToDisk with
  toolCacheDir(chatID) (toolcache/<chatID>/ toolcap.go:107-113, base
  toolcap.go:91-102). Name: toolName sanitized (non [A-Za-z0-9-_] → '-'),
  then safe+"-*.txt" random suffix.
- toolcap.go:200-258 resolveHostCachePath: os.OpenRoot on the canonicalized
  toolcache root; r.Stat(rel) PathError (op "statat") returned unwrapped
  on ENOENT; escapes → ErrHostCacheEscape; non-regular →
  ErrHostCacheNotRegular.

Every place a path is handed to the model (all use SpillToCache/spillToDisk
via the wrappers below):

- Subagent summaries: renderSubagentResult (internal/agent/
  subagent_parallel.go:479-489) — JSON + "\n[subagent summary at: %s]"
  (line 483); then renderFilesChanged appended (lines 485-487). The
  sequential handler duplicates the same shape at internal/agent/
  tool_handlers.go:1910-1925 (marker line 1918, files-changed lines
  1923-1925). Both use the PARENT app's a.chatID() (app.go:938-946:
  Session.ChatID, else Client.ChatID fallback), i.e. files land in
  toolcache/<PARENT-chatID>/ under the parent chat dir.
- Dispatch partials / rejected: subagent.go:1763
  ("dispatch_subagent_partial"), subagent_guard.go:243
  ("dispatch_subagent_rejected").
- Tool-result caps (CapOrStub app.go:2444-2464, all on a.chatID()):
  CapToolResult (toolcap.go:464-488) → "\n… [+N chars omitted —
  TRUNCATED: … — full content at: PATH]" (capSuffix toolcap.go:437-450);
  SpillFullResult (toolcap.go:499-510) → "\n[full content at: PATH]"
  (only when len > 200); StubToolResult (toolcap.go:410-419) →
  "[budget — N chars at: PATH]" (or "... SPILL FAILED ..." with no path).
  Mashūra/subagent/check_pending/collapse_context results bypass
  CapOrStub entirely (app.go:2444-2446).
- Eviction spill (app.go:2516-2530): content without a spill path gets
  SpillToCache + "\n[full content at: PATH]" injected, then
  MakeEvictionStub (toolcap.go:79-85) →
  "[evicted — N chars — full content at: PATH]".
  NOTE: eviction depends on ExtractSpillPath finding a TRAILING marker —
  this is the consumer most exposed to the renderSubagentResult marker
  order (see below).
- Async envelope: renderAsyncLine (async_ops.go:1275-1324) — over
  asyncEnvelopeOpCap spills full text and appends "\n[full content at:
  PATH]" (lines 1306-1312); Mashūra ops exempt from the per-op cap
  (line 1303); spill failure → bounded truncation + check_pending
  pointer (lines 1315-1320).
- Collapse archives: "turn-%d" (collapse.go:1231), "turn-%d-part"
  (collapse.go:1737, 1996), prune "round-%d" (prune.go:1107, 1182,
  1381), repo_map/repo_symbols (repomap.go:251, 286; symbolmap.go:386;
  init_cmd.go:337, 349), review_diff (review_cmd.go:107), worktree
  conflict/failed patches (subagent.go:2005, 2018), compact
  (compact.go:926).
- Collapse notes hand out the archive path: buildCollapseNote
  (collapse.go:284-396) — "full record: %s (read_file with offset/limit —
  host-side path: shell cannot see it)" (lines 377, 390 re-emitted in the
  minimal-note rebuild). Note cap: noteCapBytes = 1200 (collapse.go:431)
  with the documented exception that the archive-path line is never
  truncated (collapse.go:379-394). Path buckets (write targets / reads)
  are truncated by fitNotePaths (buildCollapseNote uses it at line 354):
  the OLDEST entries drop, within a shared budget — a handed-out path
  from BEFORE the collapse can fall out of the note even though the
  archive still exists.
- Trailing-marker recovery: ExtractSpillPath (toolcap.go:27-74) requires
  the known marker to sit in the FINAL bracketed segment (must be the
  last non-space char ']'), with one of the prefixes: "full content at: ",
  "budget — ", "+", "evicted — ", "pre-send trim — ", "subagent summary at: "
  (lines 45-52). The same logic is DUPLICATED in internal/proxy/client.go
  (extractSpillPath, lines 1991-2031) — any marker change must be mirrored
  there (proxy cannot import tools: it would be circular per client.go:1988).
- Confirmed marker-order gap: renderSubagentResult appends
  renderFilesChanged AFTER the summary marker (subagent_parallel.go:485-
  487), and tool_handlers.go does the same (lines 1923-1925). So for
  edit-tier children with files changed, ExtractSpillPath (both copies)
  returns "" — eviction/pre-send-trim recovery and MakeEvictionStub
  cannot recover that path.

Read / search scoping — the asymmetry (verified):

- read_file / read_file_full intercept: IsToolCacheHostPath (toolcap.go:
  154-162) is an exact-prefix lexical classifier over the WHOLE root —
  any <root>/<chatID>/... matches, any chat. Confinement re-verified in
  resolveHostCachePath via os.Root (toolcap.go:200-258) — reads cover
  child chats the same as the own chat.
- search_files: handleSearchFiles (tool_handlers.go:1106-1115) routes to
  host search only when OwnToolCachePath(cid, searchPath) — i.e. ONLY the
  caller's OWN toolcache/<chatID>/ dir (toolcap_search.go:34-45).
  SearchHostCache (toolcap_search.go:56-178) opens the root with os.Root,
  refuses a symlinked chat dir, walks with fs.WalkDir refusing symlinks,
  binary-skip.
- Confirmed open point (i): a search on a child-chat dir (or any foreign
  chat dir) MISSes the host route and falls to ConfinePath (tool_handlers.
  go:1117-1119) → confined out — while read_file on the SAME path works.
  The 4c44045 error text (app.go:3829-3830, verbatim: "ERROR: no file at
  this exact path in the tool cache — %s. Check the path was copied
  verbatim from the tool result marker or collapse note; if it was, use
  search_files on the cache directory to find the file's current name.")
  therefore points at a search that can fail for exactly the paths whose
  names the model has lost. Its "use search_files on the cache directory"
  hint only works TODAY for the own chat dir.
- The bench case: subagents were dispatched under the PARENT's chat dir
  (renderSubagentResult uses a.chatID() of the parent), so in the bench
  run the parent's own-chat search would have worked — but the model
  never got to search; it guessed names. The index (below) is the fix for
  the guessing.

### Design (revised after Mashūra round 1 — corrections folded in)

Revisions vs the first draft, in response to Mashūra findings verified
against code (each correction below was checked against the file read):

- Scope/acceptance narrowed to what the index actually covers: archives
  spilled AFTER this change, in the CURRENT chat (own toolcache/<chatID>/
  dir). "Every archived result" = every spill under that chat's dir
  written since the index exists and whose append succeeded. Pre-existing
  archives (spilled before the upgrade, or in a resumed session before
  this build) have no index line — the index is not backfilled; the
  model can still find them via read_file on a known path (marker in
  context) or search_files on the chat dir (host route covers the own
  chat dir). Child subagents' OWN spill dirs (their tool-result caps
  live under toolcache/<CHILD-chatID>/, NOT the parent's: each child App
  is minted with its own NewChatID per subagent_parallel.go:383;
  chatID() = Session/Client fallback app.go:938-946, fixed for the life
  of that App) are indexed in THEIR OWN chat index, and their summaries
  (in the parent's index) are how the parent recovers the findings. The
  bench failure was parent-dir files; the index guarantees parent-dir
  findability. Cross-chat parent aggregation is out of scope (no lineage
  mechanism exists).
- No signature change, no caller-supplied labels. Index line:
  "<kind>\t<absolute path>\t<label>". kind comes from a fixed mapping
  table over the toolName the caller ALREADY passes (spillToDisk callers:
  SpillToCache, StubToolResult, CapToolResult, SpillFullResult):
  dispatch_subagent* → subagent-summary; turn-*/round-*/compact-*/
  hardmax-* → collapse-spill; repo_map/repo_symbols → repo-map;
  review_diff → review-diff; read_file*/load_skill/search_files/
  write_file* → tool-result; default → misc (unknown names still
  indexed — findable). label = the spilled content's FIRST NON-EMPTY
  line, truncated to 80 runes, with \t\n\r stripped and invalid UTF-8
  cut (same truncateInvalidUTF8Suffix discipline as toolcap.go:166-175).
  The label is a convenience, not guaranteed distinctive (no false
  promise: if Render()'s first line is a bare "{" it distinguishes
  nothing — kind+path still work). "Find by label" acceptance becomes
  "find every subagent archive in the index (kind=subagent-summary)".
  Labels never contain tool arguments or result content beyond the
  first line.
- Index size/truncation: NO cap on the index. read_file on it goes
  through hostCacheReadResult → ReadHostCacheFileBounded (app.go:3804-
  3842, 1<<20 default hardCap) and supports offset/limit paging — the
  collapse note says "(read_file with offset/limit …)" so the model
  pages it. One short line per spill needs ~7,000 spills to exceed the
  bound; paging covers it. Test pins paging on an over-bound index.
- search_files widening — DROPPED. Reading other chats via read_file
  needs the random-suffix filename; content SEARCH across chats would
  let a model grep other sessions' archived output without any name —
  a materially larger disclosure; the own-chat-only scope
  (tool_handlers.go:1106-1115, OwnToolCachePath toolcap_search.go:34-
  45) was deliberate. The index lives in the chat dir whose path the
  collapse note / ENOENT line names, and read_file reaches it from any
  app — no search widening needed. Block 2a becomes: KEEP search at
  own-chat scope; preserve-and-pin the isolation (test: a search
  scoped to chat A returns nothing from chat B; search on the own-chat
  dir CAN grep the index file itself so the model can find a task word
  in the index). Bare-root and cross-chat search stay refused exactly
  as today (ConfinePath path, tool_handlers.go:1117-1119).
- Index append attack surface: fixed-name append under the chat dir.
  A pre-planted symlink at that name would redirect the append.
  Defence: open with O_CREATE|O_WRONLY|O_APPEND (+O_NOFOLLOW where the
  platform supports it); before appending, Lstat the index path and
  refuse if it exists as a symlink or non-regular file (same paranoia
  as SearchHostCache refusing a symlinked chat dir, toolcap_search.go:
  91-97). On refusal or any open/write error: skip the index write
  (spill still succeeds), increment the observability counter. Test:
  symlink at the index path → target untouched, spill returned,
  counter incremented.
- Crash-torn lines: each record is written as "\n" + record (leading
  newline per append, empty first file aside). A torn write merges
  into the NEXT record as a corrupt fragment; the model reads the
  index as text — a mangled line's path ENOENTs harmlessly through
  the host intercept, whose text then re-points at the index. No
  machine "skip" logic is promised or needed.
- Concurrency: O_APPEND + single WriteString per record; short lines
  are atomic on local Linux filesystems in practice (not asserted as a
  POSIX guarantee). A parallel-dispatch race test asserts every index
  line parses (kind, path columns present) after many concurrent
  spawns. Best-effort under exotic filesystems per this note.
- Observability (best-effort is not silent): a package-level atomic
  counter in internal/tools counts index-append failures; the first
  failure per session emits one notice line via the existing notice
  path (mirroring pruneArchiveFailed, prune.go:1109-1114). The
  completeness test injects an append failure and asserts: spill
  path returned, index line absent, counter incremented, notice
  emitted. Pre-existing archives without lines are a stated
  limitation, above.
- Pointer survival (compact/prune/hardmax) — verified by reading:
  * compact (compact.go:552-558): archives the originals, then the
    summary message carries "· archive: <path>" (line 558, later
    attached to a.lastCompactNote). Prior collapse notes inside the
    summarized block dissolve into the lossy summary. Fix (one
    sprintf): the same note ALSO names the index
    ("· archive index: <path>", static per chat, no counts). Then any
    later collapse note also names it; and if the model has neither,
    the next ENOENT on any path re-points at the index.
  * prune (prune.go:1107 pruneCandidatesLoop): archives each round
    and puts its archive path into context as the round's stub —
    prune removes ROUND tool results, never collapse notes; the stub
    IS the recovery pointer. No index line added (each stub already
    hands out a concrete path; a second pointer is noise).
  * hardmax shed (compact.go:814-867): archives each dropped turn
    (archiveTranscript → spill, indexed) and the warning lists
    "spilled content at: · <path>" per dropped turn (compact.go:862-
    867). The warning is the pointer set; no index line added there.
- Marker-order (Block 2b) — CORRECTION: the effect is NOT an
  unrecoverable path. Eviction re-spills content that has no trailing
  marker (app.go:2516-2530), so for the edit-tier files-changed case
  eviction produces a DUPLICATE spill (the whole rendered result
  re-spilled under a new name) and the stub points at the re-spill,
  not the original summary. Moving the marker last makes
  ExtractSpillPath (toolcap.go:27-74, and its duplicate
  proxy/client.go:1991-2031) find the original summary path → the
  stub points at the ORIGINAL file and no duplicate spill happens.
  renderFilesChanged's only consumers are the two render sites +
  subagent_edit_test.go:542 (grep-verified). The marker text itself
  is unchanged, so neither extractor needs touching.
- Modules-off / cache hazard: the index line is a CONSTANT string per
  chat (fixed name + fixed dir + static wording; NO counts, NO
  per-spill content). Collapse notes already vary per turn (tool
  counts/sizes, collapse.go:345-353) — notes were never prefix-cached
  bytes; the index adds zero variability. Test: two collapse notes for
  the same chat with different spill counts → the index line is
  byte-identical. Modules-off: TestContextModulesOff_ByteIdentical
  compares BuildTools JSON and TestContextModulesOff_Preamble
  ByteIdentical compares buildPreambleForNetwork (modules_test.go:
  28-78, read) — neither exercises subagent results or collapse
  notes; marker-order and error-text touch tool outputs, not the
  tool array / system prompt. Both tests must still pass in Gates.
- chatID stability: verified. Session.ChatID is set at init; the
  Client.ChatID fallback applies only when Session is nil
  (subagent/headless), fixed for that App's life (subagents minted
  per-dispatch). Edge: archiveTranscript's "_unscoped" bucket for
  chatless callers (compact.go:921-926) — indexed under _unscoped's
  dir, no note points there. Accepted limitation (headless/tests
  only; bench always has a real chatID), listed under Open risks.
- ENOENT text (Block 2c): names the index of the REQUESTED PATH's
  chat (parse the chat segment out of the failed toolcache path),
  falling back to the caller's own-chat index when unparseable. The
  search hint is replaced by the index (search stays own-chat-scoped
  per above, so its hint could NOT work for child-dir paths).
- Acceptance widened (per panel): tests cover all spill kinds
  (subagent-summary, tool-result, collapse-spill, envelope, prune-round
  via its call site, repo-map, review-diff at the SpillToCache call
  sites or via the kind-mapping table), the minimal-note cap path
  under real noteCapBytes pressure with a long path (archive + index
  lines present), collapse → dispatch → collapse ordering, paging on
  an over-bound index, symlink-refusal on the index path, and
  index-line byte-identity. The index file ITSELF will match
  search_files needles when its content matches — accepted
  (it is model-readable text in the own chat dir); not excluded.

1. Per-chat archive index (Block 1). Inside tools, at SpillToCache /
   spillToDisk time: after the spill file is CLOSED successfully and
   before the path returns to any caller (structural "written before
   hand-out"), append one record to a NEW append-only index file in the
   same chat dir: toolcache/<chatID>/archive-index.txt. One record =
   "\n" + "<kind>\t<absolute path>\t<label>". Best-effort: index write
   failure never loses the spill. The index file itself gets no record
   (spillToDisk is only called for real spills; the index append path
   does not call spillToDisk — self-reference impossible; a tool named
   "archive-index" would get a random suffix, no collision). Labels
   stack a per-chat monotonic "#N" sequence number (see verification
   round 2) onto the capped first line. Reading the index back can
   itself be capped and spilled (CapOrStub applies to every tool
   result); that spill lands in the index as a tool-result row —
   accepted noise, not exempt (stated; non-blocking).
   Collapse note: buildCollapseNote gains the line "archive index:
   <abs path> (read_file with offset/limit — one line per archived
   result: kind, path, label)" in BOTH the full and the minimal
   (rebuild) note, and in the degraded/older-turn projection
   (collapse.go:741-772, next to rec.Archive). archiveTranscript
   (compact.go:921-926) uses SpillToCache so collapse/compact/prune/
   hardmax spills all index automatically.
   Cache rule: the line appears only inside collapse notes (user-role
   conversation content, NOT the system prefix / tool array) and is
   constant per chat → stable prompt prefix unchanged; no new tool, no
   schema change, no system-prompt change → modules-off bytes
   unchanged.
   Concurrency: O_APPEND single WriteString per record; race test
   pins well-formed lines under parallel dispatch.
2. Search scope (Block 2a — NO widening). Keep OwnToolCachePath
   routing (tool_handlers.go:1106-1115). Tests pin isolation: search
   scoped to chat A returns nothing from chat B; a path outside the
   root still refused; symlink escape refused (existing SearchHost-
   Cache refusals, toolcap_search.go:91-105, unchanged); the own-chat
   dir search CAN find a needle inside the index file.
3. Marker order (Block 2b). In renderSubagentResult
   (subagent_parallel.go:479-489) AND the duplicated sequential path
   (tool_handlers.go:1910-1925), emit "[subagent summary at: PATH]"
   AFTER renderFilesChanged, so it is the final bracketed segment and
   ExtractSpillPath recovers it (both copies: toolcap.go:27-74,
   proxy/client.go:1991-2031 — unchanged). The eviction path then
   stubs to the ORIGINAL summary file instead of re-spilling.
4. Error text (Block 2c). In hostCacheReadResult's ENOENT branch
   (app.go:3829-3830): replace the search_files hint with the index
   path of the requested path's chat (fallback: caller's own chat):
   "… read_file the archive index at <path> to find the file's
   current path." Host-cache layer only (executor path untouched).

### Touched paths (planned)

- internal/tools/toolcap.go — index append inside spillToDisk (or
  SpillToCache) post-Close; index path helper; kind-mapping table;
  atomic failure counter; O_NOFOLLOW + Lstat refusal. ExtractSpillPath
  UNCHANGED.
- internal/tools/toolcap_search.go — UNCHANGED (no widening). Tests
  for isolation live with the existing search tests.
- internal/agent/tool_handlers.go — sequential subagent marker order
  only (no search routing change).
- internal/agent/subagent_parallel.go — renderSubagentResult marker
  order.
- internal/agent/app.go — ENOENT error text (index path of requested
  chat).
- internal/agent/collapse.go — buildCollapseNote (full + minimal
  rebuild) + older-turn projection: index line.
- internal/agent/compact.go — archive note (compact.go:558) also
  names the index path.
- proxy/client.go extractSpillPath: UNCHANGED.
- CHANGELOG.md — one entry under [Unreleased] (Block 3).

### Blocks and their tests

Block 1 — archive index that survives collapses (failing test first):

- Test (fake backend): dispatch subagents; collapse mid-turn ≥2 times;
  read the index from the path in the LATEST collapse note; find every
  subagent archive (kind=subagent-summary); read_file each
  successfully via the host intercept. Assert the latest collapse
  note names the index.
- Test (completeness + ordering): every handed-out spill path appears
  in the index; the line exists immediately after SpillToCache
  returns, before any marker assembly.
- Test (all kinds): one test per kind mapping (subagent-summary,
  tool-result via CapToolResult/StubToolResult/SpillFullResult,
  collapse-spill via collapse archive, repo-map via repo outline
  spill, review-diff call site, envelope via renderAsyncLine, misc
  fallback for an unknown tool name).
- Test (label hygiene): content with a 500-rune first line and a
  tab/newline and invalid UTF-8 → label ≤ 80 runes, no \t\n\r, no
  tool arguments, no secret-looking content beyond the first line.
  Assert the FIRST LINE of the content is NOT pasted wholesale but
  capped.
- Test (failure observability): unwritable index (read-only chat dir
  or symlink at index path) → spill path returned, counter +1,
  notice emitted once.
- Test (concurrency): many parallel child spills → every index line
  is well-formed (kind + path columns).
- Test (paging): index > read bound → offset/limit reads cover all
  entries.
- Test (cache hazard): collapse note index line byte-identical
  across two notes with different spill counts.
- Test (minimal-note cap): note exceeding noteCapBytes → rebuild
  still carries archive + index lines.
- Existing collapse + subagent + toolcache suites green; modules-off
  identity tests green.

Block 2 — search scope + marker order + error text (failing test first):

- Test (search isolation, NO widening): search scoped to chat A finds
  needles in A (including inside A's index file) and nothing from
  chat B; search outside the root refused; symlink escape refused
  (leaf symlink; chat dir symlink). The child-dir case: a search on
  ANOTHER chat's dir still goes through ConfinePath (refused) —
  asserted as the intended scope, not worked around.
- Test (marker order): edit-tier child with files changed —
  ExtractSpillPath (tools copy) recovers the summary path from
  renderSubagentResult output; eviction over that result produces a
  stub pointing at the ORIGINAL path (no duplicate spill); proxy
  extractSpillPath behavior asserted via its behavior on the same
  string if reachable from a proxy test, else covered by the
  mirroring note (identical logic).
- Test (error text): ENOENT message names the REQUESTED chat's
  index (child-dir path → child's index); unparseable path falls
  back to the caller's own index.

Block 3 — CHANGELOG: one entry under [Unreleased].

### Open risks

- Index file name collision: spill files are "<toolname>-<rand>.txt";
  the fixed name "archive-index.txt" cannot collide. A hand-planted
  file of the same name in the user's own chat dir is ignored via the
  Lstat regular-file check (its content then reads as the index — an
  own-data-dir scenario, not a new attack surface; the symlink case
  is refused).
- noteCapBytes pressure (collapse.go:431, 1200): the index line adds
  ~120 bytes; notes already near the cap go through the
  minimal-rebuild path which must keep archive + index lines (test
  pins this).
- Worker-goroutine appends: O_APPEND single-write lines; on local
  filesystems short writes don't interleave; a torn write merges
  into the next record (leading-"\n" discipline) and degrades to a
  harmless ENOENT + re-pointer, never to a wrong path.
- _unscoped index (compact.go:921-926): chatless spills index under
  _unscoped; no note points there. Headless/test-only; bench has a
  real chatID. Accepted.
- SweepToolCache (startup retention prune) removes per-chat dirs older
  than retention, never the active chat's — index dies with its chat
  dir, consistent with the archives it lists. No new deletion path.
- Pre-upgrade archives have no index lines (no backfill). Statement
  of limitation, above.
- Mashūra rule from round 1 still applies: §§3/4 below are the
  earlier session's corrected root-cause analysis (executor story
  DROPPED); this section supersedes §§3–5 wherever they conflict.

### Verification round 2 (settled against code this session)

- **spillToDisk direct-caller gate**: `grep` for spillToDisk across
  internal shows only SpillToCache, StubToolResult, CapToolResult,
  SpillFullResult (toolcap.go:403, 412, 468, 505); no direct writes to
  the toolcache tree exist (grep for os.WriteFile/os.Create against
  toolcache paths: none) — index append lives in spillToDisk, which
  is toolcache-only by construction.
- **read_file host intercept**: IsToolCacheHostPath (toolcap.go:154-
  162) matches ANY <root>/<chatID>/... path (root-prefix, not
  own-chat-scoped — verified by reading it); hostCacheReadResult
  (app.go:3804-3842) imposes no filename pattern — archive-index.txt
  reads (and child-chat indexes) work over the same path. No index
  exemption (the index is read DIRECTLY; its own reads are
  model-initiated). NoteCapBytes: in the minimal rebuild only the
  descriptive text shrinks; both PATH lines (archive + index) are
  never truncated (collapse.go:379-394 — existing rule) — pinned by
  test.
- **O_NOFOLLOW availability**: repo grep for O_NOFOLLOW: none today.
  Implementation will use unix.O_NOFOLLOW (golang.org/x/sys is already
  a dependency — verify at implementation time; CI covers darwin
  cross-compile). Fallback if unavailable on a build platform:
  Lstat-refusal + fstat-on-fd check (TOCTOU window is own-data-dir,
  documented in resolveHostCachePath's threat model at toolcap.go:
  188-199). Hardlinks: accepted (same inode as a regular file in the
  own chat dir = own data). Hand-planted REGULAR index file:
  correction to the first "Open risks" draft — it would be adopted
  and appended to, not ignored. Only symlinks/non-regular are
  refused. Test pins: symlink leaf → refused; symlinked chat dir →
  refused (same rule as SearchHostCache); regular pre-existing file →
  appended (adopted), stated as accepted own-data behavior.
- **_unscoped reachability**: grep for archiveTranscript across internal
  → compact.go:552 (compact-N, fires only when a summarizable block
  exists, i.e. inside a live session with a real chatID), 814
  (hardmax-turn-N, hard-max path in a live session), 921 (def). The
  _unscoped bucket is hit only when a.chatID() == "" — i.e. Apps with
  Session == nil and Client == nil (headless/test construction
  only). Accept supplied; not blocking — no real session produces
  empty chatID.
- **ChatID assignment site**: rendered summaries use the PARENT's
  a.chatID() at all three render sites (subagent_parallel.go:258,
  482; tool_handlers.go:1917); a child's own tool-result caps use the
  child's Client.ChatID (minted per dispatch, subagent_parallel.go:
  383). The Block 1 test must use a REAL distinct child chatID (the
  existing toolcache_intercept_test.go / readback test harness does —
  "bench-parent"/"bench-child" style), not a fake backend that masks
  it. Pinned in the Block 1 test plan.
- **Pointer survival coverage**: compact replaces history (summary
  bump), so collapse → compact → collapse is the exact ordering the
  Block 1 test must PROVE (assert index path still in live context
  after compact via the compact.go:558 note; and after hardmax via
  the "spilled content at:" list). Prune never touches collapse
  notes (round tool results only) — assert collapse note survives
  prune in the same test.
- **Cache hazard test widened**: byte-identity of the index line
  across (a) full note, (b) minimal-rebuild note, (c) older-turn
  projection, each with different spill counts.
- **Skip-on-missing rule**: the index line is emitted
  UNCONDITIONALLY (never skipped when the index is missing/failed),
  so re-rendered projections never change mid-chat. Presence pinned
  by test, not just identity.
- **Label gains a sequence number**: "<kind>\t<path>\t<label>" label =
  first non-empty line truncated to 80 runes + a per-chat monotonically
  increasing sequence number ("#N") so identical generic first lines
  ("{", "total 48") remain distinct rows. The sequence counter lives
  in process memory (guarded by the same lock story as the append
  path); a crash resets it — harmless (rows remain distinct by path).
  Label provenance (content-derived, capped at 80 runes) is accepted
  explicitly as a tradeoff; the first line itself is NOT redacted
  (redacting would hide the discriminating info the label exists
  for). No result content beyond the first line ever enters it.
- **Torn-write parse rule**: the model reads the index as text; a
  record whose path column lacks the chat-dir prefix (or contains a
  tab after cleaning) is garbage — the test asserts every line
  written by the append path parses (kind + absolute path under the
  chat dir). A torn write degrades to a corrupt line (path fails →
  ENOENT → re-pointer), never to a wrong-but-valid path, because the
  path column is written as a unit in one WriteString.
- **Concurrency test strengthened**: line count equals spill count
  AND every listed path exists (not only well-formed lines).
- **Go short-write**: single WriteString of ~150-300B under O_APPEND
  is accepted best-effort per panel note; not asserted as POSIX.

## Mashūra round 2 — open question

"After several collapses, can the agent still find every archived result
without knowing its file name, and what could break the prompt cache or
modules-off identity?" BLOCKING only (an archived result that stays
unfindable, a change to modules-off bytes, a cache-breaking change to
the stable prefix, or touching user data); list the rest. Max 2 rounds.
