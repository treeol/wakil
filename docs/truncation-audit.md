# Truncation audit: content cut before a model call

> Historical — describes the state as of 0.3.1 (an audit of content truncated before model calls, with line numbers from before its fix); no successor document.

Scope: everything Wakil does that shortens content before a model request
(main agent path and Mashūra/counsel). Line numbers are for the tree before the
fix (baseline `master`). "Visible" = the user sees a notice that names the cut.

## 1. Main-agent request path

| # | Where | What it drops | Hits current turn? | Visible? |
|---|-------|---------------|--------------------|----------|
| A1 | `agent/collapse.go:877-1036` `collapseFinishedTurns` | W (tool calls + results) of finished turns → archive pointer. Runs in `finalizeTurn` (`turn_phases.go:584`), so the turn just finished is collapsed at once | yes (just finished) | one dim line, sizes only: `collapsed request N — Xk → Yk sent` (:1033); no ids, no turn range |
| A2 | `collapse.go:539-556`, `774-797`, cfg `ContextTailAnswerBytes` (default 500, `config.go:1027`) | final assistant answer A of every collapsed turn cut to 500 B + `…` in the projection | the previous turn's report, on the next request | silent |
| A3 | `collapse.go:565-605`, `747-754`, cfg `ContextTailTurns` (20) | turns older than 20 collapsed turns → one pointer line (U to 80 chars, summary/answer to 200) | no (old turns) | silent |
| A4 | `collapse.go:1055-1275` `handleCollapseContext` | model-invoked checkpoint: prefix of the CURRENT turn → archive + model summary | yes, by explicit model call | dim line + result caveats |
| A5 | `collapse.go:134-264` | collapse-note path lists (`maxNotePaths`=12, `noteCapBytes`=1200) | n/a | note says `+N more` |
| A6 | `compact.go:323-602` `Compact`, `keepBoundary` `:52-76` | older turns → summary (`SummaryBytes` 20000 `truncateBytes` `:572`); `renderTranscript` cuts tool output to 600 chars (`:113`); budget-exhausted fallback truncates (`:508`,`:554`). `keepBoundary` counts injected user messages (async envelopes, wrap-up prompts) as turn starts, so the boundary can land INSIDE the current turn and summarise its brief | yes (head of current turn) | `· compacted earlier turns into a summary` (`turn_phases.go:601`), no sizes/archive |
| A7 | `compact.go:748-848` `enforceHardMax`, `dropOldestTurn` `:672-717` | sheds oldest turns until under hardMax; loop stops only at `convLen <= 1` (:775), so the current turn is shed last; last-resort branch drops single raw messages | yes | warning lists spill paths of tool results only; U/A text is gone with no archive |
| A8 | `compact.go:265-282` `fitConvToWindow` (call `app.go:1162`) | runs A7 on backend downshift, before the new user message exists | no | ⚠ line |
| A9 | `app.go:1908-1956` `evictStaleToolResults` | tool results older than `ToolResultTTL` (3) turns → stub with spill path; `turnBoundary` (`compact.go:32`) counts injected user messages as turns | yes (early results of a long turn) | silent |
| A10 | `app.go:1861-1893` `CapOrStub` → `tools/toolcap.go:410,464,499` | per-result cap `ToolResultCap` 8000, per-turn `TurnToolBudget` 40000 → spill pointer | yes (current turn results) | marker in the result |
| A11 | `proxy/client.go:943-973`, `1370-1401` `trimToolResults` | largest tool results → stub when request > `MaxRequestBytes` (8 MB) | yes (cannot tell turns apart) | stub text only |
| A12 | `agent/async_ops.go:50-51`, `1264-1280` | async result in envelope: 8000 B excerpt + `[full content at: PATH]`; envelope total 16000 | n/a | pointer present; full text recoverable |
| A13 | `agent/handoff.go:381-396` `truncateTailUTF8` | keeps NEWEST bytes (drops the head) for handoff seeds | session seed | silent |
| A14 | `agent/subagent.go:356-376` | transcript caps in partial-work flush | n/a | `[+N chars truncated]` |
| A15 | `turn_phases.go:396-402`, `mashura.go:1259`, `compact.go:125` `Truncate` | display-only previews (2000/120 chars) | no (UI) | n/a |
| A16 | `tui/clipboard.go:463-596` paste placeholder | placeholder expanded at send; no cut intended; not verified here | brief | placeholder shown |

Nothing in A1-A11 compares a message with `a.curTurn`; `Message.Turn` exists
(`proxy` field `wakil_turn`) but only the collapse sweep reads it.

## 2. Mashūra / counsel path

| # | Where | What it drops | Visible? |
|---|-------|---------------|----------|
| M1 | `agent/mashura.go:886-897`, `mashuraSourceFileCap`=32 KB | each source file cut at 32 KB (≈8k tokens). This is why DESIGN.md "stopped at §13" | receipt line `(N bytes, clipped)` after the gate; a marker inside the text |
| M2 | `mashura.go:899-904`, `mashuraSourcesTotal`=150 KB | whole later files dropped | marker + receipt |
| M3 | `mashura.go:651-689` `mashuraCap` (callers `:734,770,1066,1098`), 200 KB | tail of body / sources | marker inside the text |
| M4 | `mashura.go:720,1057` → `workflow/workflow.go:329-338` `CapFindings` | findings cut to 4 KB | marker inside |
| M5 | `mashura.go:952-997`, `mashuraDirFileCap`=20 | files beyond 20 per directory | marker inside |
| M6 | `counsel/modellimit.go:146-253` `FitToContext` (callers `oracle.go:640-652, 765-805, 810-843`) | tail-truncates the briefing at 90% of the model context. Context = table (`:62`) → warm registry → 128k fallback (`:39`); callers ignore `fit.Note` | silent to the user and to the reviewer except an in-text marker |
| M7 | `oracle.go:212-233, 911-913` | reply with `stop_reason=max_tokens` / `finish_reason=length` → plain error. No retry, text discarded; usage still billed | error string |
| M8 | `mashura.go:1104-1120`, `modellimit.go:44` | max_tokens: base 4096, check 1024; `FitToContext` lowers it to a floor of 1024 when the input is large, which produced the empty replies | none |
| M9 | `workflow/workflow.go:340-385` `assembleBriefing`, `WFBriefingCap`=12 KB | oldest step-log entries (labelled window), then hard tail cut of the whole briefing incl. Plan/Question | marker `[briefing truncated]` |
| M10 | `oracle.go:66` | reply body read capped at 4 MB | JSON parse error (loud) |
| M11 | `oracle.go:255-268, 1010-1016` `PanelDetail` | gate text capped at 2000 chars; the HTTP payload itself is whole | `(+N chars)` |
| M12 | `oracle.go:499-624` `runDebate` | none; round 2 appends all round-1 answers, but the result went through M6 | n/a |

Other `RunPanel` callers (same cuts via M6): `agent/workflow_engine.go:235`,
`agent/skill_creation_pipeline.go:737`. There is no code that counts review
rounds; the "two-round cap" is enforced by the agent/skill procedure counting
results, so a failed or void reply must say so in its own text.

## 3. Archive

`wtools.SpillToCache(chatID, "turn-N"|"turn-N-part", renderWorkingSet(...))`
(`collapse.go:969,1185`) writes under the host-side toolcache; the full turn
(U+W+A) is archived (:957-964). `Compact` and `enforceHardMax` archive nothing.

## 4. As built

D1 **Current turn is never cut.** `protectedStart(conv, curTurn)` (`compact.go`) is
Turn-field based (`appendConv` stamps every in-turn message; injected user-role
messages such as async envelopes and wrap-up prompts no longer look like turn
starts); fallback = last user message when no turn counter exists. Applied in:
`Compact` (boundary clamp, every path), `enforceHardMax` (loop stops at it; each
dropped turn is archived first via `archiveTranscript "hardmax-turn-N"` and its
path listed — both `Compact` and `enforceHardMax` refuse to replace or drop a
turn whose archive failed to write), `evictStaleToolResults`, `trimToolResults` (skips the newest `Turn`).
`guardCurrentTurn` runs before every `Stream` in the main agent: over `hard_max`
→ reduce PRIOR turns (summary, then archive-and-drop) → re-measure → still over →
`ErrCurrentTurnTooLarge` naming total/current-turn bytes, ~tokens and the limit;
nothing is sent. `@file` mentions (`tools/mentions.go`) inject the file whole;
> 64 MB fails the chip loudly (was 50 KB/800 lines cut). Paste is expanded whole
at send (`tui/clipboard.go`, `CharLimit=0`, verified by reading, A16).
`context_tail_answer_bytes` default 500 → 0: prior assistant reports are kept whole.

D2 **Reviewer dispatch is whole-or-fail** (`counsel/review.go`, `dispatchReview`,
used for every `RunPanel` mode and caller). Tokens are estimated at 3 B/token over
EVERYTHING sent (system prompt + question + framing + part header + overlap +
briefing; debate round 2 passes the quoted round-1 answers as an explicit
appendix added to every part and excluded from the evidence target); the request may use
90% of the context minus the answer floor and 512. Fits → one request, byte-exact.
Otherwise the briefing is split at `── label ──` blocks and, inside markdown/text,
at ATX headings outside code fences (shallowest level that works, recursing
deeper); source files are atomic (error, never cut mid-file). Parts carry the full
question, a `part i of n` header and ~200 tokens of overlap; findings are merged
under `── part i of n ──` labels. Anything unsplittable, > 24 parts, or a window
smaller than question + floor = error before any call. Per-reviewer log: tokens
sent, parts, model, context, `finish_reason`, read-through section (footer in the result + diag).
Removed: `FitToContext`, `mashuraSourceFileCap/SourcesTotal/ToolBriefingCap`,
`mashuraCap`, `CapFindings`, workflow hard tail cut, directory subset (now an error
above 20 files), async-envelope excerpt for `mashura__*`. Sanity bound: 8 MB.

D3 **Answer budget.** `max_tokens = min(configured, ctx − input − 512)`, never below
4096 (`check` keeps its 1024 floor). finish_reason=length / stop_reason=max_tokens
(`IncompleteReplyError{Length}`) → retry once on the chunked path (a first attempt that
was already chunked is retried with parts of half the size; an atomic source block
that cannot be split is retried once whole with a larger answer budget); still
truncated, or a retry that fails for any reason (`voidAfterTruncation`) → VOID.

D4 **Evidence of reading.** Whenever the document has headings or `── label ──`
blocks the prompt demands a final `LAST SECTION READ: <heading>` line. It is
normalised (case, `#*\`` , numbering) and compared with the part's last heading /
block label (exact normalized match; a citation-only reply with no review text is
also VOID). The line is stripped from the answer.

D5 **VOID** = `PanelMemberResult{Void: true, Err: *VoidError}`, any failed/empty/
truncated/mis-cited part voids the whole member (parts never merged). Output gets
`[mashūra round NOT COMPLETED — …; not a review round, not "no findings"]`;
auto-counsel refunds `counselCalls`; the workflow consult returns "unavailable"
(does not advance); the skill pipeline keeps the error. Billed usage is recorded.
"collapsed request" line now names message range, tool-result ids, bytes ≈ tokens
and the archive; `Compact` archives what it summarises and says where.

D6 **Config** `mashura_models.<model>.{context_tokens,review_max_tokens}`,
defaults from the provider table / OpenRouter registry (128k fallback).

## 5. Status after fix

| Row | Status |
|-----|--------|
| A1 | still collapses the finished turn (it is "prior" next request); line now lists what was dropped and the archive |
| A2 | removed (default 0; opt-in cap kept) |
| A3 | **kept, accepted:** turns older than 20 collapsed turns → archive-backed pointer line |
| A4 | **kept, accepted:** explicit model action on the model's own earlier work only; enforced in code (`handleCollapseContext` refuses a turn that does not start with the user's message, and the collapsed range starts after it) and tested (`TestCollapseContextNeverCollapsesUserMessageOrAttachments`) |
| A5, A13, A14, A15 | unchanged (not user content sent to a model / display only) |
| A6, A7, A8, A9, A11 | current turn protected (D1); prior-turn reductions archived and named |
| A10 | **kept, accepted:** per-result cap (8000) / per-turn budget (40000) spills the full text to disk; the pointer names the file and `read_file` / `read_file_full` serve it back byte-exact (`TestSpilledToolResultRoundTripsThroughReadFile`) |
| A7 (last resort) | **accepted:** `enforceHardMax` summarises first; only then drops a prior turn, archived with its path listed; no hard error |
| A12 | `mashura__*` results exempt from the envelope op cap; others unchanged |
| A16 | verified whole; `@file` fixed (D1) |
| M1-M6, M9 | removed; replaced by D2 |
| M7, M8 | replaced by D3 |
| M10 | unchanged (loud) |
| M11 | gate display only; the HTTP payload is whole |
| M12 | round 2 sized and evidence-checked by D2/D4 |
