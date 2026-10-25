# Long-Session Mode — Implementation Plan

> Status: plan only — implementation happens in a new session.
> Design background and review history: `docs/long-session-mode-analysis.md`
> (v3, request-scoped collapse). This document is the build spec; where the two
> disagree, this one wins.
> Line numbers were verified on 2026-10-13 (master, 2 commits ahead of origin).
> Re-check them before editing — they drift.

## 1. What we are building

A second **context mode** next to today's behavior:

| Mode | Behavior |
|------|----------|
| `standard` (default) | Exactly today. Every message stays in what is sent to the model until compaction / hard-max / eviction trims it. |
| `long-session` | After a request (one human turn) finishes, its **working set** — tool calls, tool results, intermediate assistant text, injected envelopes — is archived to disk and no longer sent. Later requests see only that request's **user message + final answer + a one-line note** pointing to the archive. |

User-facing switch: `/mode standard` / `/mode long-session` (alias `/mode long`).
The mode is persisted **per repo** (like `/auto`, `/rawtools`), so it survives
`/new`, `/handoff` and restarts. `standard` stays the default everywhere.

### Core principle: projection, not mutation

`a.Conv` remains the complete log of record. Long-session mode changes only
**what is sent** — a filtered copy built at request time. Consequences:

- `standard` mode is byte-identical to today (the projection is `a.Conv` itself).
- Switching modes mid-session is instant and reversible: nothing was deleted.
- `/rewind` checkpoints (`ConvLen` indices), `/resume`, `/handoff`, session JSON,
  the TUI transcript view and `ConvSnapshot` readers keep working unchanged.
- The one exception: when compaction actually fires in long-session mode, the
  collapsed turns are materialised into `a.Conv` first (§6.6). Compaction
  already rewrites `a.Conv` and clears checkpoints, so this adds no new regression.

### Terms

- **Turn / request**: one `SendOutcome` call (`internal/agent/app.go:967`).
- **U**: the turn's user message (appended at `app.go:1022-1024`).
- **A**: the turn's final answer — the last assistant message, with text and no tool calls.
- **W**: every other message of the turn.
- **Collapsed turn**: in the projection, U, then A with the note appended.

## 2. Verified facts the plan relies on

| Fact | Evidence |
|------|----------|
| The main request path sends every message given to it — no filtering, no server-side history substitution | `internal/proxy/client.go:816-934` (`marshalWireMessages` → `body.Messages`) |
| There is one main model call site | `internal/agent/turn_phases.go:265-267` (`msgs := a.Conv`) → `:301` (`Client.Stream`) |
| `proxy.Message` has local-only fields that are kept out of the wire (`Pinned`, `Images` use `json:"-"`); the wire type is a separate struct | `client.go:61-93`, `wireMessage` `client.go:127-133` |
| `Pinned` is `json:"-"`, so it is **not** persisted in session JSON | `client.go:92` |
| The existing `/mode` prints only the exec backend | `internal/agent/commands.go:356-357`; remote `internal/remote/commands.go:99-103`; help `commands.go:1549`; autocomplete `internal/tui/complete.go:87` |
| Per-repo setting pattern to copy (`/rawtools`) | `RepoState.RawTools` `repostate.go:35`; restored in `restoreEndpointIndependentLocked` `repostate.go:354-376` (both fresh and resume paths, `:236`, `:276`); command `commands.go:427-438`; setter `app_state.go:159-164`; status segment `info_snapshot.go:63-66` → `wiring/facade.go:259-318` → `tui/tui_view.go:562-563,704,882` |
| CLI explicit-flag pattern | `--auto` `config.go:1114`; `AutoExplicit` / `ModelExplicit` from `fs.Visit` `config.go:1135-1142` |
| Context defaults and validation | `DefaultConfig` `config.go:804-852` (context block `:820-835`); `validateContextLimits` `config.go:1522-1608`, called at `:1195-1197`; env helpers `envStr/envBool/envInt` `config.go:1309-1338`, env block `:1044-1089` |
| Spill files are written by the host wakil process; the model can `read_file` them (with offset/limit) because those paths bypass the sandbox | `tools/toolcap.go:371-395` (dir `0o700`, `CreateTemp`), `:121-140` (`ToolCacheRoot`, `IsToolCacheHostPath`) |
| Nothing deletes old tool-cache directories | no `RemoveAll` in `internal/tools` |
| Async guards exist | `hasInboxContent` `internal/agent/idle_wake.go:132`; `countActiveAsyncOps` used at `turn_phases.go:567` |

### Every site that writes the main `a.Conv`

| Site | What | Turn stamping (§6.2) |
|------|------|----------------------|
| `app.go:1023` | U (user message) | stamp |
| `turn_phases.go:185` | async inbox envelope (user role) | stamp |
| `turn_phases.go:219` | subagent budget footer | stamp (subagents never collapse anyway) |
| `turn_phases.go:245`, `:253`, `:258` | confinement / budget / iteration-limit prompts | stamp |
| `turn_phases.go:334` | assistant message (intermediate or A) | stamp |
| `turn_phases.go:465-471` | tool result (`finalizeToolResult`) | stamp |
| `turn_phases.go:547-556` | skipped-tool-call results after `finalize_goal` | stamp |
| `mashura.go:1254-1257` | auto-counsel assistant + tool pair (**not under `convMu`**) | stamp |
| `app.go:1595`, `:1602` | preamble (`Conv[0]`) refresh / insert | leave 0 |
| `control.go:87` | `AppendSystemMessage` (handoff seed) | leave 0 |
| `compact.go:504-520` | compaction rewrite (summary message is new) | leave 0 on summary; preserve on kept messages |
| `compact.go:696` | `dropOldestTurn` | n/a (removal) |
| `app.go:1710-1757` | eviction (in-place content stub) | n/a (content only) |
| `app.go:887`, `control.go:97`, `wiring/conversation_manager.go:329` | load / replace whole transcript | restore turn state (§6.7) |
| `app.go:916` | new conversation (`a.Conv = nil`) | reset turn state |

## 3. User-facing behavior

### `/mode`

| Input | Result |
|-------|--------|
| `/mode` | `exec: docker · context: standard` (existing output, extended) |
| `/mode standard` | sets standard, persists, notice `context mode: standard (full history sent)` |
| `/mode long-session` or `/mode long` | sets long-session, persists, notice `context mode: long-session — finished requests are collapsed to question + answer` |
| `/mode status` | mode, number of collapsed / kept requests, `sent ~Xk · full ~Yk`, archive directory |
| anything else | usage line |

Why these names: `standard` says "this is the default"; `long-session` is the
feature's purpose, and `long` is a short alias. `/mode` is already the
"how is this session running" command, so it now reports both axes (exec and context).

### `/keep`

| Input | Result |
|-------|--------|
| `/keep` | exempt the most recent finished request from collapse; if already collapsed, restore it into what is sent |
| `/keep <n>` | same, for request number n |
| `/keep off <n>` | remove the exemption (collapsed again at the next request) |

### Visibility

- Dim line when a request is collapsed: `· collapsed request 12 — 96k → 0.4k sent (archived)`.
- Status-line segment `long` while the mode is on (next to `raw`).
- The `hist` status segment and `/history` show what is sent, plus the full size:
  `/history` → `42 messages · sent ~31k chars · full ~412k chars`.

### Startup / config

- Config `"context_mode": "standard" | "long-session"` (default `standard`).
- Flag `--context-mode <standard|long-session>`; env `WAKIL_CONTEXT_MODE`.
- Precedence: flag > repo state > config file > default. The flag sets
  `ContextModeExplicit`, the same rule as `--auto` / `AutoExplicit`.
- Subagents always run in `standard`, whatever the parent's mode.

## 4. Collapse rules

At the **end** of every turn (start of `finalizeTurn`, before compaction) and again at the
**start** of every `SendOutcome` (before the new U is appended), in long-session mode, each turn
T that is not yet collapsed becomes collapsed when **all** of these hold:

1. `T > 0` — legacy messages, preamble, summary and handoff seed carry `Turn == 0` and are never collapsed.
2. T is finished: its last message is an assistant message with non-empty text
   and no tool calls (a clean A). Aborted or errored turns are kept.
3. `|W| ≥ context_collapse_min_bytes` (default 2000). Small turns aren't worth a note.
4. T is not in the keep set.
5. `!a.IsSubagent`.
6. For the **most recent** turn only:
   - `countActiveAsyncOps() == 0`, `!hasInboxContent()`, and no un-notified background job;
   - the turn did not end suspended (the `suspended` flag passed into `finalizeTurn`, or
     `lastOutcomeSuspended` when called from `SendOutcome`).
   (Async results land in that turn's context; collapsing it would cut them off.)
7. Archiving succeeded (§6.3). On any archive failure, T stays uncollapsed and
   one warning is shown per session.

Any turn skipped by rule 6 or 7 is reconsidered at the next request.

## 5. The note (deterministic, no inference call)

Appended to A in the projection only:

```
[request 12 working set collapsed — 14 tool calls: read_file×6, search_files×3, run_shell×2, edit_file×3 · 96k kb]
write targets: internal/agent/compact.go, internal/agent/turn_phases.go, docs/x.md (+1)
read: internal/agent/app.go, docs/y.md (+2)
full record: /home/u/.local/share/wakil/toolcache/<chat>/turn-12-XXXX.txt (read_file with offset/limit — host-side path: shell cannot see it)
```

- Tool counts come from the `ToolCalls` of W's assistant messages.
- `write targets` lists up to 12 distinct paths (shared budget with `read`,
  write-priority) from write-class tool calls (`edit_file`, `write_file`,
  `write_binary_file`, `delete_file`, `move_file`) — evidence of *intent to
  modify*, not of a completed change. `read` lists everything else
  (`path` / `paths` / `file` / `src` / `dst` values parsed from tool-call
  JSON arguments; errors ignored). A fully-suppressed bucket still reports
  its overflow count.
- The archive path is HOST-SIDE: `read_file` resolves it via a host-side
  intercept; a sandbox shell "not found" does not mean the archive is gone
  — that hint is printed in the note itself.
- Cap the note at 1200 bytes (soft: the archive-path line is never trimmed).
- The note is wakil-authored, not model-authored. The archive path is created by
  wakil inside the tool cache, so it is trusted for `read_file`.

## 6. Implementation steps

Each step is a separately green commit. Run the pre-push checklist (§9) after each.

### 6.1 Commit 1 — mode setting, `/mode`, persistence, status (no behavior change yet)

**`internal/config/config.go`**
- Fields, next to the context block:
  ```go
  ContextMode             string `json:"context_mode,omitempty"`              // "standard" (default) | "long-session"
  ContextCollapseMinBytes int    `json:"context_collapse_min_bytes,omitempty"` // default 2000
  ContextModeExplicit     bool   `json:"-"`                                    // set when --context-mode was passed
  ```
- Defaults in `DefaultConfig` (`:820-835`): `ContextMode: "standard"`, `ContextCollapseMinBytes: 2000`.
- Flag in the flag set (`:1092-1127`): `fs.StringVar(&cfg.ContextMode, "context-mode", cfg.ContextMode, ...)`;
  then `cfg.ContextModeExplicit = flagsSet["context-mode"]` (`:1135-1142`).
- Env in the env block (`:1044-1089`): `envStr(&cfg.ContextMode, "WAKIL_CONTEXT_MODE")`.
- Validation in `validateContextLimits`: mode ∈ {`standard`, `long-session`}
  (normalise `long` → `long-session`); min_bytes ≥ 0.
- Exported helper `NormalizeContextMode(s string) (string, bool)` in `config`,
  shared by config, the command and repo state.

**`internal/agent`**
- New constants in a new `context_mode.go`: `ContextModeStandard = "standard"`, `ContextModeLong = "long-session"`.
- `App` field `contextMode string`, guarded by `stateMu`. Methods `ContextMode() string`,
  `SetContextMode(string)`, `contextModeLocked()`, mirroring `SetRawToolsValue`
  (`app_state.go:159-164`). Initialise from `Cfg.ContextMode` where `RawTools` is initialised.
  `IsLongSession()` returns `contextMode == long && !IsSubagent`.
- `RepoState` (`repostate.go:35` area): `ContextMode string `json:"context_mode,omitempty"``.
  Restore in `restoreEndpointIndependentLocked` (`:354-376`): if `st.ContextMode != ""`
  and `!a.Cfg.ContextModeExplicit`, apply it. `DescribeRepoState` (`:531-533`):
  add a `context mode: long-session` line.
- `/mode` (`commands.go:356`): parse `fields`; no args → `exec: … · context: …`;
  `standard|long-session|long` → `SetContextMode` + `saveRepoState(func(s){ s.ContextMode = m })`
  (copy the `/rawtools` persistence call at `:427-438`); `status` → for now mode only (filled in in commit 3).
- `/help` (`commands.go:1549`): `/mode [standard|long-session|status]  show exec backend / set context mode`.
- Subagent construction (`subagent.go:~1202-1220`, where child `cfg` is built): force `cfg.ContextMode = "standard"`.

**Wiring / TUI**
- `InfoSnapshot.ContextMode string` (`core/sessionclient/info_snapshot.go:63-66` area).
- `wiring/facade.go` `Info()` (`:259-318`): fill it under the same lock as `RawTools`.
- `tui/tui_view.go:562-563,704,882`: render a `long` segment when `ContextMode == "long-session"` (same style family as `raw`).
- `tui/complete.go:87`: if the completer supports subcommands, add `standard`, `long-session`, `status`. Otherwise leave it.

**Remote / daemon** (`remote/commands.go:99-103`): v1 keeps the exec-only output for bare `/mode`;
with arguments, return the notice `context mode switching is not supported over the daemon yet`.
Full parity is §8, item 1.

**Tests**
- `config_test.go`: default; flag sets `Explicit`; env; invalid value rejected; `long` alias normalised.
- `repostate_test.go`: round-trip; explicit flag beats repo state; `""` leaves config value.
- `commands` test: `/mode` output format, set + persist, invalid argument → usage.
- Subagent cfg forced to standard.

### 6.2 Commit 2 — turn identity

**`internal/proxy/client.go`**
- Add to `Message`:
  ```go
  // Turn is the wakil request number this message belongs to (0 = none:
  // preamble, summary, handoff seed, legacy). Local bookkeeping for
  // long-session mode. Persisted in session JSON; MUST NOT reach the wire.
  Turn int `json:"wakil_turn,omitempty"`
  ```
- **Verify** that `marshalWireMessages` builds from `wireMessage` field by field and never
  `json.Marshal`s `Message` directly. Grep for any `json.Marshal` of `[]proxy.Message` /
  `proxy.Message` that reaches a backend (candidates to check: `app.go:1787`, `mashura.go:93`,
  `persist.go:60`).
- Golden test in `internal/proxy`: request body bytes identical with and without `Turn` set
  (cache_control on and off).
- `internal/protoconv`: check whether `Message` ↔ proto conversion carries unknown fields.
  If not, `Turn` is dropped over the daemon path — acceptable for v1, but document it.

**`internal/agent`**
- `App` fields (turn goroutine only, so no lock; read under `convMu` when needed):
  `turnSeq int`, `curTurn int`, `lastOutcomeSuspended bool`.
- New helper in `collapse.go` (or `conv_append.go`):
  ```go
  // appendConv appends msgs to a.Conv under convMu, stamping Turn=a.curTurn
  // on any message that has Turn==0. All in-turn appends go through here.
  func (a *App) appendConv(msgs ...proxy.Message)
  ```
- `SendOutcome` (`app.go:967`): after `admitTurn`, `a.turnSeq++; a.curTurn = a.turnSeq`.
  Stamp U at `:1012`. At the end, set `lastOutcomeSuspended` from the returned outcome
  (including the error path).
- Replace the direct appends listed in §2 (`turn_phases.go:185,219,245,253,258,334,465,547-556`,
  `mashura.go:1254`) with `appendConv`. For `mashura.go:1254`, confirm the caller does not hold
  `convMu` (it doesn't today); the helper adds the lock.
- `NewConversation` path (`app.go:916`): reset `turnSeq`, `curTurn`, collapse state.
- Compaction (`compact.go:504-510`): kept messages keep their `Turn`; the summary message has `Turn 0`.

**Tests**
- Every message appended during a `SendOutcome` has `Turn == curTurn`, including inbox envelopes and the mashura pair.
- Preamble and handoff seed stay at 0.
- `send_characterization_test.go`: requests captured by the fake client are deep-equal to today's,
  ignoring `Turn` (the wire golden already proves `Turn` isn't sent).

### 6.3 Commit 3 — collapse records, archive, projection

**New file `internal/agent/collapse.go`**

```go
type CollapseRecord struct {
    Turn    int       `json:"turn"`
    Archive string    `json:"archive"`  // spill path of the full W render
    WBytes  int       `json:"w_bytes"`
    Note    string    `json:"note"`
    At      time.Time `json:"at"`
}

// App fields (turn goroutine writes; projection reads under convMu.RLock):
//   collapsed map[int]CollapseRecord
//   keep      map[int]bool

func (a *App) collapseFinishedTurns()           // §4 rules; called at SendOutcome start
func (a *App) turnSpans() map[int]span          // first/last index per Turn, one pass
func renderWorkingSet(w []proxy.Message) string // FULL render (no 600-char cut, unlike renderTranscript); tool args + results verbatim
func buildCollapseNote(t int, w []proxy.Message, archive string) string
func (a *App) contextProjection() []proxy.Message
var spillFn = wtools.SpillToCache // test seam for archive failure
```

- **Archive**: `spillFn(a.chatID(), fmt.Sprintf("turn-%d", T), renderWorkingSet(W))`. An empty
  path means failure → skip (rule 7). Archive before recording; the record exists only if the file does.
- **Ordering vs eviction**: collapse of turn N runs at the start of N+1. Eviction stubs
  turn N's results at the end of N+1 at the earliest (`ttl ≥ 0`), so archives normally hold
  full content. A turn skipped by rule 6 and archived later may contain eviction stubs;
  those still carry spill paths, so this is acceptable — document it in a comment.
- **Projection**:
  - If not long-session, or no records → return `a.Conv` (the same slice; zero cost, byte-identical).
  - Otherwise build a new slice under `convMu.RLock`. For a message with `Turn == T` and `T` collapsed:
    - emit it if it is T's first message (U);
    - if it is T's last message (A), emit a **copy** whose `Content` is a new string
      `A + "\n\n" + note` (never write through the shared `*string`);
    - skip anything else.
  - All other messages pass through unchanged.
- **Call sites** — use the projection:
  - `turn_phases.go:265-267` (main stream);
  - `ctxlimit.go:424` (`ContextUsage` fallback estimate);
  - `app.go:645` `ConvStats` → return sent length/size, and add `ConvStatsFull()` for `/history`;
  - `side_question.go:34`: switch to a sanitized projection
    (`sanitizeConvForSideQuestion(a.contextProjection())`).
  - Do **not** change `ConvSnapshot` (`app.go:635`): `wiring/facade.go:227` uses it for the
    TUI transcript, which must keep showing everything.
  - Check `WarnContextPressure` (`app.go:1169`) and switch it to the sent size if it reads `a.Conv`.
- **Hooks** (two call sites, same function, idempotent):
  1. **End of turn** — first statement of `finalizeTurn` (`turn_phases.go:581`), *before*
     `evictStaleToolResults` and `Compact`. This is required: `finalizeTurn` compacts at the
     end of every turn, so if collapse only ran at the next request, the just-finished W
     would count toward `compactAt` and trigger compaction before it could be collapsed.
     [Mashūra op-7] Pass the turn's `suspended` result from `streamTurn` (`SendOutcome`,
     `app.go:1056-1060`) into `finalizeTurn` so rule 6 can block collapse of a suspended turn.
  2. **Start of request** — in `SendOutcome` right before `a.fitConvToWindow(ctx)`
     (`app.go:1010`), before U is appended. This picks up turns that rule 6 skipped earlier
     (async work has since finished).
  Print the dim note once per newly collapsed turn.
- **`/mode status`**: fill in counts and sizes. **`/history`**: show sent and full sizes.

**Tests (`collapse_test.go`)**
- Standard mode: the projection is `a.Conv` (`&p[0] == &a.Conv[0]`, or `reflect.DeepEqual` plus the same length).
- Eligibility: each of rules 1–7 individually blocks collapse.
- Projection invariant helper `assertToolPairs(t, msgs)`: every assistant `tool_calls` id has a
  following tool result in the same turn, and every tool result has a parent. Run it on every test's projection.
- No consecutive messages with the same role created by collapse (U → A alternation).
- Archive failure (`spillFn` returns `""`) → W kept, one warning.
- Archive file contains the full tool arguments and results.
- Note ≤ 600 bytes; paths extracted; counts correct.
- Async pending / inbox content / suspended → the last turn is not collapsed, older turns are.
- Mode switch long → standard → long: records retained, no re-archiving.
- The shared `Content` pointer is not mutated (compare A before and after building the projection).

### 6.4 Commit 4 — compaction, hard-max, rewind, persistence

**Sizes measured on what is sent.** Add `func (a *App) sentSize() int { return TranscriptSize(a.contextProjection()) }`.
Use it instead of `TranscriptSize(a.Conv)` in:
- `fitConvToWindow` (`compact.go:255`);
- `Compact` (`:305`, `:319`);
- `enforceHardMax` (`:669`, `:672`, `:681`, `:704`, `:753`).

**Compact on the projection, commit only on success.** `Compact` today explicitly keeps
checkpoints intact when the summarizer fails (`compact.go:326-329`); materialising first would
break that. [Mashūra op-7] So:
- In `Compact`, after `evictStaleToolResults()` (which keeps its existing in-place behavior), take
  `conv := a.compactionBase()`: the projection as a fresh slice in long-session mode with records,
  otherwise `a.Conv`.
- Replace every `a.Conv` read in the rest of `Compact` with `conv`:
  `keepBoundary(a.Conv, …)` `:322`, `older := a.Conv[:boundary]` `:340`, the `make` capacity `:504`,
  `a.Conv[boundary:]` `:510`.
- Every error return before `:517` leaves `a.Conv`, the records and the checkpoints untouched.
- On success, keep today's order: `clearCheckpoints()` (outside `convMu`), then `convMu.Lock`,
  `a.Conv = newConv`, **then delete all collapse records** (their notes are now physical, inside
  the kept A messages or the summary input). The record deletion is the only addition to the
  commit block.
- `enforceHardMax` (`:668-756`): after its forced `Compact` (`:674`), if records still exist
  (summarizer failed), call `a.materializeCollapsed()` before the drop loop:
  `clearCheckpoints()` → `convMu.Lock` → `a.Conv = projection` → delete records. This is the
  last-resort path, which already sheds turns and clears checkpoints (`:713`), so it adds no new
  regression. It is required so that `dropOldestTurn` and the size check operate on the same slice.
- `fitConvToWindow` (`:253-261`) only calls `enforceHardMax`, so it is covered.

**Rewind** (`checkpoint.go` truncation around `:520-560`): after `a.Conv` is truncated, prune
`collapsed`/`keep` entries whose turn no longer exists, and set `turnSeq` to the max remaining `Turn`.

**Persistence**
- `Session` (`session.go:21-31`): add
  `Collapsed []CollapseRecord `json:"collapsed,omitempty"`` and
  `Keep []int `json:"keep,omitempty"``.
- `SaveSession` (`app.go:738-779`, snapshot at `:757`): copy the records under the same locks.
- One helper `a.restoreTurnState(s *Session)` — rebuild the maps, set `turnSeq` to the max `Turn`
  in `Conv`, clear `lastOutcomeSuspended` — called from:
  - `LoadSession` (`app.go:887`);
  - `wiring/conversation_manager.go:327-331` (`ResumeConversation` assigns `f.app.Conv = s.Conv`
    directly; add the helper call there);
  - `control.go:97` (`SetConv`, daemon resume).
- Handoff (`handoff.go:120-126`, `:226-232`) writes `app.Session` directly via `WriteSession`
  rather than through `SaveSession`. Set `app.Session.Collapsed` / `Keep` there too, or route both
  through a small `a.fillSessionTurnState(s)` helper. Handoff summarises the **full** `a.Conv`
  (better than the projection — it has everything). No change to the summary logic.

**Tests**
- `TranscriptSize(a.Conv)` is large, `sentSize()` is small → `Compact` does not fire.
- `sentSize()` over `compactAt` → `Compact` summarises the projection; on success records are
  cleared, checkpoints are cleared, no orphaned tool messages.
- Summarizer error in long-session mode → `a.Conv`, records **and checkpoints** unchanged
  (mirror the existing checkpoint-preservation test for `Compact`).
- Summarizer error inside `enforceHardMax` → materialise fallback runs; the drop loop terminates
  under max; no orphans.
- End-of-turn ordering: a turn with W = 150k (over `compactAt` alone) and small U/A is collapsed in
  `finalizeTurn` and does **not** trigger `Compact`.
- `/rewind` across a collapsed turn → records pruned, `turnSeq` correct, the projection valid.
- `SaveSession` → `LoadSession` / `ResumeConversation`: the projection is identical before and after.
- Handoff in long-session mode produces the same payload shape; the new session starts with no
  records and the mode taken from repo state.
- `fitConvToWindow` after long → standard on a smaller backend trims as today.

### 6.5 Commit 5 — `/keep`, polish, docs

- `/keep [n]`, `/keep off <n>` (`commands.go`, next to `/mode`). `/keep` on a collapsed turn deletes
  its record (the archive file stays on disk). Persisted via `SaveSession`.
- Help text for `/keep`; mention in `/mode status`.
- Docs (required by the repo's documentation-upkeep rule):
  - `docs/configuration.md`: `context_mode`, `context_collapse_min_bytes`, `--context-mode`,
    `WAKIL_CONTEXT_MODE`, in the context-settings section, together with the
    `tool_result_ttl` row (`:154`, `:170`);
  - `config.example.json`: entries plus `_comment` in the context block (`:115-140`);
  - `README.md`: the slash-command section (`/mode`, `/keep`) and a short "Long-session mode" paragraph;
  - `AGENTS.md`: only if package structure or test commands changed (they shouldn't).
- Update `docs/long-session-mode-analysis.md` status to "implemented v1".

## 7. Edge cases (must be covered)

| Case | Expected |
|------|----------|
| Mode switched on mid-session | Turns stamped since this binary started are collapsed at the next request; older/legacy `Turn 0` messages are untouched |
| Mode switched off | The projection is the identity at the next request; `fitConvToWindow` trims if needed; records kept for switching back |
| Very large single A (e.g. a 10k plan) | Never collapsed in v1; base grows; compaction eventually handles it |
| A turn whose last message is an assistant with tool calls (aborted / force-finish) | Not collapsed (rule 2) |
| `finalize_goal` ended the turn (`turn_phases.go:545-559`) | If the last message is a tool error stub → no clean A → kept |
| Workflow (`/plan`) turns | Collapsed like any other turn; the workflow directive lives in U and is kept |
| Headless / continuous mode | Same `SendOutcome` path → same behavior when the mode is long-session |
| Subagent | Always standard |
| Daemon (`wakild`) sessions | v1: mode is per repo and applies, but `/mode <arg>` over the remote facade returns "not supported yet"; `Turn` may be dropped by protoconv |
| Archive disk full / no chatID | W kept, one warning |
| Tool-cache growth | Not handled in v1 (no retention exists today) — see §8 |
| Prompt cache | One cache rebuild from U_T onward when T collapses; append-only afterwards. Verify with cached-token telemetry (`Costs`) |

## 8. Out of scope for v1 (follow-ups)

1. **Daemon parity**: `context_mode` in proto `SessionState` (next to `raw_tools = 22`,
   `session_state.proto:139`), a `SetContextMode` RPC mirroring `SetRawTools`
   (`server/connect/session_state_handler.go:424-448`, `remote/commands.go:423-439`), and
   `RemoteFacade.Info()` filling `ContextMode` (`remote/facade.go:454-483`). Note: the same
   `Info()` currently omits `RawTools` too — an existing gap worth fixing alongside.
2. **Tool-cache retention**: age/size-based cleanup of `ToolCacheRoot()/<chat>` for sessions not
   touched in N days.
3. **Model-proposed keep** (`context_keep` tool, partial keep with tool-call/sibling closure —
   see the analysis doc §5.3).
4. **Rolling summarisation of old answers**, so base stays bounded on very long sessions.
5. Optional keep-note → kvr / mid-tier memory index (kvr is not the archive: Docker-only,
   snapshot every 300 s).

## 9. Verification per commit

From the repo's agreed pre-push checklist:

```sh
gofmt -l . | grep -v '^\.tmp/'      # must print nothing
go build ./...
go vet ./...
go test -count=1 ./internal/agent/... ./internal/config/... ./internal/proxy/... ./internal/wiring/...
```

Before the final push: full `go test -count=1 ./...` (Docker integration needs the
`wakil-dev:latest` image). For race runs in this sandbox, `/tmp` is noexec:
`GOTMPDIR=/mnt/wakil/.gotmp GOCACHE=/mnt/wakil/.gocache go test -race ./internal/agent/...`.

After commit 4: run a Mashūra review on the diff (`collapse.go`, `compact.go`, `app.go`,
`turn_phases.go`, `session.go`).

### Manual acceptance script

1. `wakil` (standard) → `/mode` shows `context: standard`; behavior unchanged.
2. `/mode long` → status line shows `long`; `/mode status` shows 0 collapsed.
3. Ask for something that reads several large files and then answers.
4. Ask a follow-up ("why did you pick X?"). Expect the dim `· collapsed request 1 …` line, a small
   `sent` size in `/history`, and an answer that still makes sense from A.
5. Ask for something needing the old details ("show the exact lines you read in compact.go").
   Expect the model to `read_file` the archive path or the source file again.
6. `/keep 1` → request 1 is sent in full again; `/mode status` reflects it.
7. `/rewind` one turn → no errors, `/mode status` is consistent.
8. Quit, `wakil --resume` → the mode is still `long-session`, `/history` sizes match.
9. `/mode standard` → the full history is sent again (sent ≈ full).
10. In a different repo, `wakil` starts in `standard`.

## 10. Acceptance criteria

1. With `standard`: request bytes identical to before the change (golden + characterization tests).
2. In long-session mode, the projection never contains orphaned tool calls/results and never breaks
   U/A alternation for collapsed turns.
3. A synthetic run of 30 requests, each with W = 50k and A = 2k: `sentSize()` stays below
   preamble + 30 × (|U| + |A| + 600) + current turn; `Compact` does not fire (baseline: it fires ≥ 5×).
4. The most recent turn is never collapsed while async work or undelivered results exist, or right after a suspension.
5. `/resume`, `/rewind`, `/handoff` work in both modes; mode switches are reversible within a session.
6. Every note's archive path exists and is readable through `read_file`.
7. Docs updated as listed in §6.5.
