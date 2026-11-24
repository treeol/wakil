# Harder bench tasks — plan (work/0.8.12 from v0.8.11)

BASE: `6d1a86a` (HEAD, `chore: release 0.8.11`); tag `v0.8.11` = `46e9031` (annotated tag object; HEAD is its target commit). Branch `work/0.8.12` created from `6d1a86a`. Go `go1.26.11`. Tier 3 (multi-file measurement change; leak/comparability risk).

## 1. Verified findings — how the bench works today

### 1.1 Harness (`scripts/bench.sh`)
- Usage/flags: `--wakil --model --task --runs --timeout --mashura --context-modules on|off --no-preflight --self-test --setup` (`scripts/bench.sh:55-76`, arg parsing `:104-139`). Defaults: wakil from PATH, configured model, all tasks, 1 run, 600s, Mashura off (`:78-102`, `bench/README.md:17-18`).
- CSV header today (18 cols, ends `modules_loaded`): `scripts/bench.sh:184`:
  `date,wakil_version,model,task,run,result,exit_reason,seconds,tokens_in,tokens_out,tokens_cached,cost_usd,tool_calls,collapses,collapses_turn_end,mashura_rounds,context_modules,modules_loaded`.
  Note: `collapses`/`collapses_turn_end` already exist as CSV columns (from `--result-json`); Block 2's `collapses` in the task brief duplicates the existing column — see §2/§5 decision.
- `csv_row` (`:542-555`) appends one row via `csv_line`→`csv_field` (RFC 4180 quoting, CR/LF→space, `:478-504`); readers `print_table` (`:1006-1048`), `sum_cost` (`:1050-1063`), `find_prev_csv` (`:992-1004`), `csv_cell` (`:529-540`) all read **by header name** via `CSV_AWK_LIB`/`csvsplit` (`:506-527`) — so appended columns and old CSVs are readable by construction (subject to the Block 2 test).
- Work folders: fixed `${XDG_CACHE_HOME:-$HOME/.cache}/wakil-bench/work/<task>` (`:150-154`); `prep_workdir` (`:557-654`) empties/refills from `bench/tasks/<task>/repo/` (preserving `.wakil/` by mv-aside), runs optional `setup.sh` **before** `git init`/commit (`:626-633`), one commit as `wakil-bench <bench@localhost>`, then `leak_check` (`:656-697`, pre-agent: refuses `check.sh hidden solution gen setup.sh`, `cmd/gen`, seeded-generator grep, and history names).
- Run: `run_one_locked` (`:758-986`) runs `wakil run --auto --allow-destructive --no-memory [--auto-counsel] --result-json <run_root>/<task>.json --transcript <log> <task.txt>` under `timeout -k` with `WAKIL_HOST_WORKDIR=<workdir>`, `WAKIL_CONTEXT_MODULES=<off|on>`, optional `WAKIL_MODEL` (`:782-791`). Result class: 124/137 → `timeout` (no check, metrics discarded, `:860-868`); sandbox-approval wording → `error/sandbox-not-acknowledged`, stop (`:875-887`); workdir guard → `error/missing-workdir|workdir-not-verifiable|wrong-workdir`, stop (`:899-925`); post-run leak (check.sh/hidden/solution present) → `error/leak-detected`, stop (`:927-939`); else `check.sh` in workdir, result from exit 0/1 via `check_result_for_rc` (`:423-433`), 99/other → `error/check-error`, stop (`:941-965`). Locks: one `flock` per task (`:708-742`), held across `--runs` (`run_all :1069-1082`).
- Go toolchain: resolved once (`resolve_go :302-354`, `GOPROXY=off`, no download); `with_go_env` (`:356-380`) passes `BENCH_GOTOOLCHAIN/GOROOT/GOMODCACHE/GOCACHE/GOPATH`, `BENCH_GO_TEST_TIMEOUT=2/3*check_timeout`; `preflight_go` (`:388-421`) runs `setup.sh` + every Go `check.sh` on repo+solution **before any model request**; `--task steps` needs no Go (`bench/README.md:88`).
- Self-test: `self_test()` (`:1181-2676`) redirects CSV/logs/temp work-root/locks/run-root to cache (`wakil-bench/selftest/run.*`, never `bench/results/`), asserts repo results untouched. Cases include: noop→fail every task, solve (copy solution/)→pass every task, declined, cheat (TestMain/init/underscore), long-read leak + `leak_check` kinds, planted solution, CSV quoting, lock, `--runs 2` lock-series, timeouts (TERM-ignoring → 137), workdir guard per backend, sandbox approval, freshness/`.wakil/` preservation, toolchain-pinned/empty-home, tools-first/tools-list-sufficient, failure-report. **Every task is exercised generically by the noop/solve loops** — a new task with repo+task.txt+check.sh+solution automatically gets noop-fail/solve-pass coverage.
- Docs/CI: bench documented in `bench/README.md` (Run, Setup, Go requirement, How a run works, Self-test, Known limits, Adding a task `:317-331`). CI `bench-harness` job: shellcheck + `--self-test` (`.github/workflows/ci.yml:229-257`); `scripts/host_verify.sh:215-224` same with visible SKIP when shellcheck missing.

### 1.2 Task layout (verified per task)
Each task: `bench/tasks/<name>/{task.txt, check.sh, repo/, solution/, hidden?/, setup.sh?/}` (`bench/README.md:317-331`; `Adding a task` requires untouched `repo/` fails check, reference `solution/` passes).
- `steps`: repo=`README.txt` only; no hidden/setup; check compares 6 artifact files (`bench/tasks/steps/check.sh`); no Go needed.
- `long-read`: repo=`go.mod` only; `setup.sh` runs `gen/` (outside repo/, seed fixed `rand.NewSource(42)`, `bench/tasks/long-read/gen/main.go`) to generate 60×~8KB `docs/note-*.txt` into the work copy **before** the commit, so generator/seed never in tree/history (`bench/tasks/long-read/setup.sh`, `bench/README.md:105-111`); check is python3 comparing `result.json` sums (`bench/tasks/long-read/check.sh`), expected sums hardcoded in check (`{"alpha":157,...}`). Known limit: solvable by script (`bench/README.md:292-296`).
- `fix-test`: repo=`wordcount/wordcount.go` (case-sensitive bug) + `go.mod` (go 1.26); `hidden/wordcount/wordcount_test.go` (TestCountCaseInsensitive); solution = `strings.ToLower` fix. Check: `gocheck_prepare` + `gocheck_run ./wordcount/ TestCountCaseInsensitive` (`bench/tasks/fix-test/check.sh`).
- `rename`: repo = `shared/` (OldName) + 6 caller packages (alpha..zeta) each `Total` calling `shared.OldName`, `go.mod` (module `frename`, go 1.26); `hidden/behaviour/behaviour_test.go`; solution renames to NewName everywhere. Check: `gocheck_prepare`, build, grep old name gone, grep NewName defined+used per package, `gocheck_run ./behaviour/ TestBehaviour`.
- `add-flag`: repo=`cmd/grep/main.go` + `go.mod`; no hidden; solution implements `-n`; check builds binary outside work folder and runs 4 cases (`gocheck_bin`).

### 1.3 `bench/lib/gocheck.sh` — how a check stays hidden and cheat-resistant
- `gocheck_init` temp dir + EXIT trap mapping: 0 pass / 1 fail (only with verdict marker from `gocheck_fail`) / 99 error; status 1 without verdict → 99 (`gocheck.sh:37-79`).
- `gocheck_prepare <task-dir> "<allowed stdlib>"`: refuses symlinks/`go.work`/`vendor/`; copies work folder (no `.git`/`.wakil`) to temp; **deletes every `*_test.go` and `testdata/`**; restores reference `go.mod`, drops `go.sum`; walks import closure (`go list -e -deps ./...`) refusing non-allow-listed stdlib imports (so no `os`/`syscall`/`unsafe`/`embed`/cgo/`os.Exit`) and asm/C/cgo files (`gocheck.sh:140-194`).
- `gocheck_run <pkg> <Test...>`: copies task `hidden/` in, `go test -count=1 -v -timeout=…`, requires rc 0, no `--- FAIL`, and `--- PASS: <name>` per named test (`:196-222`). `gocheck_blame`/`gocheck_probe`: on go failure, re-run on reference solution; reference fails → error, else fail (`:74-105`).
- Consequence for new Go tasks: hidden tests + reference `go.mod` live only under `bench/tasks/<name>/{hidden,repo/go.mod}`; agent tests are dropped; agent code cannot import os etc. Visible tests in `repo/` are **deleted by the check**, so they cannot satisfy the check by themselves — but they *can* inform the agent (that is their purpose) and the hidden tests must probe beyond them.

### 1.4 Collapse — how it is triggered and counted
- Only in long-session mode (default; `internal/config/config.go:1577` `ContextMode: DefaultContextMode`), never subagents (`internal/agent/app_state.go:234-237` `IsLongSession`: `contextMode==long-session && !IsSubagent`).
- **Mid-turn** (what `--result-json` `collapses` counts): `collapseMidTurnPrefix` (`internal/agent/collapse.go:1873-1916`) fires between tool calls in `streamTurn` (`internal/agent/turn_phases.go:375-376`) when projected sent size > `hardMax` (95% of effective context, `internal/config/config.go:1584` `HardMaxFrac 0.95`; effective = `Usable() × ContextCapacityFrac 0.80 × charsPerToken 4`, `internal/agent/compact.go:18,218-246`) **or** > absolute ceiling `ContextCollapseCeilingTokens` (default 200000 tokens ×4 = 800k chars, `internal/config/config.go:375-382,1579`, `collapse.go:1859-1870`). Tail kept ≤30% of ceiling/hardMax.
- **Turn-end**: `collapseFinishedTurns` (`collapse.go:1125+`) collapses a finished clean-answer turn when `|W| >= ContextCollapseMinBytes` (default 2000, `config.go:373,1578`) — this is `collapses_turn_end`, not `collapses`.
- Counting: `CollapseCounts()` (`collapse.go:2306-2337`) → `writeResultJSON` `Collapses`/`CollapsesTurnEnd` (`internal/wiring/resultjson.go:44-45,130-132`) → bench CSV columns 14/15 (`scripts/bench.sh:184,548-554`).
- Per-model window: **not a repo table** — `ContextLimit` is probed from the backend at startup (`internal/agent/ctxlimit.go:22-55`, `Usable()` = proxy `usable_ctx` or `NCtx − reasoning − answer margin`). So mid-turn collapse depends on the served model's window: a fixture big enough for a 128k model may never trigger for a 1M model. No bench threshold override exists today; config keys `context_collapse_*` exist (`config.go:373,382`) with env/flag override only for `ContextModules` (`WAKIL_CONTEXT_MODULES`, `config.go:2186`) and model (`WAKIL_MODEL`, `config.go:2162`) — no `WAKIL_CONTEXT_COLLAPSE_*` env is read (verified by grep; only the JSON keys).
- Logged: transcript records + result-json counts; bench log carries the run transcript (`--transcript`).

### 1.5 Result JSON (`internal/wiring/resultjson.go:28-56`)
Fields: `wakil_version, model, exit_reason, workdir?, sandbox_backend?, seconds, tokens_in/out/cached?, cost_usd, tool_calls, collapses, collapses_turn_end, mashura_rounds, declined_calls, context_modules?, modules_loaded?`. `model` = `app.Client.Model` (requested/configured, `:103`), **not** the served provider. No `provider` field. `DeclinedCalls` is in JSON but not in CSV.

### 1.6 Provider — what the repo proves (Block 3 verdict)
- **No source in the repo documents an OpenRouter provider-pinning mechanism** (`provider.order` / `provider.only` / `provider.sort`). Verified: main client `wireBody` sends `model/cwd/session_id/plugins` only (`internal/proxy/client.go:1207-1223,1281-1297`); the **only** provider-object write in the codebase is the ZDR merge `provider:{zdr:true,data_collection:deny}` preserving existing fields (`internal/proxy/policy.go:128-156`, `withZDRFields`; `withoutZDRFields :158-190` keeps other fields — proof the wire accepts a `provider` object, but not which fields pin). `EndpointConfig` has no provider/order field (`internal/config/config.go:40-95`). `docs/configuration.md:232` documents the ZDR `provider` object only. Tests use `{"provider":{"sort":"throughput"}}` merely as a merge-preservation fixture (`internal/proxy/policy_test.go:58,108-125,184-195`).
- Response side: `ServedModel` (chunk `model`) is captured in the Exec transport (`internal/proxy/transport.go:288-289,510`) and in `UsageStat.ServedModel` (`client.go:457-463`), consumed only for pricing (`internal/agent/app.go:1928-1929`); the **Stream loop does not propagate chunk.model into UsageStat** (verified `client.go:1520-1760`: `usage` built from usage chunk only), and `ResultJSON` has no provider/served-model field. OpenRouter's chat-completions response does not carry a provider name in the chunk shape Wakil parses (no `provider` field in `streamChunk`/`usage` parsing, `transport.go:240-320`).
- **Block 3 verdict (per brief: "if there's no source in the repo, say so and leave the flag out"): NO `--provider` flag.** The `provider` CSV column (Block 2) therefore records: pinned value when known, else **empty** for non-OpenRouter backends and `n/a`-style when not reported — concretely (§5): empty string when the binary reports nothing (today: always, unless a future binary reports `provider` in result-json), `mixed` when multiple distinct values observed (future-proofing; today unreachable but specified so the column semantics are fixed now). The bench will read a `provider` field from `--result-json` if present (forward-compatible, same pattern as every other `json_field` column) and otherwise leave it empty — never inventing a value. This keeps Wakil byte-identical to v0.8.11 (no Go changes at all in this session).

## 2. Data model and touched paths
- **No Go changes.** Wakil stays byte-identical to v0.8.11 (§DO NOT ALTER: "this session changes only the bench"). All changes are: `scripts/bench.sh`, `bench/tasks/{collapse-long,refactor-multi,debug-subtle}/**`, `bench/README.md`, `CHANGELOG.md`, this plan. (Docs/plans is tracked — existing `docs/plans/*.md` are committed.)
- **Task data model** (unchanged schema, `bench/README.md:317-331`): `task.txt` (prompt, no check/solution content), `repo/` (agent-visible fixture), `check.sh` (exit 0/1/99 via gocheck), `hidden/` (check-only), `solution/` (self-test/preflight reference only), optional `setup.sh` (pre-commit generation) + `gen/` (outside repo/).
- **CSV data model**: header order is append-only. Current 18 cols (`bench.sh:184`) stay; Block 2 appends `valid,provider` (2 cols; see §5 for why `collapses` is not re-added). `csv_row` appends 2 cells; all readers are header-name based so old CSVs (any width) still parse — proven by a self-test with a literal old-format CSV fixture (§6). `print_table` gains no new printed columns for old CSVs (missing header → `n/a`, same pattern as `tokens_cached`).
- **Validity model**: single column `valid` = `true` / `false:<reason>` (`:`-joined, no comma → CSV-safe). Reasons: `no_collapse` (collapse-long completed with 0 collapses), `timeout` (result `timeout`), `error` (result `error`). All other completed runs (pass/fail on any task, including collapse-long with ≥1 collapse) are `true`. For a completed collapse-long run with 0 collapses the harness writes `result=invalid` (new result value alongside pass/fail/timeout/error; readers are header-based and print it verbatim; old CSVs have no such rows) with `valid=false:no_collapse`, so the check outcome is not overwritten by a pass/fail claim — the row records that the run was not scorable. Downstream pass-rate denominators must exclude `result=invalid` (and timeout/error) rows; see `bench/README.md` validity note. Self-test asserts `result=invalid ∧ valid=false:no_collapse` for the no-collapse case.
- **Provider model**: `provider` cell = value of result-json `provider` if the binary reports it (today: never → empty cell), else empty. Pinned value recorded when `--provider` exists (Block 3: it does not — §1.6); `mixed` specified for future multi-value runs, unreachable today. Empty (not `n/a`) per brief: "empty for non-OpenRouter backends". Distinction: `n/a` = binary did not report a field it normally reports (tokens/cost pattern, `na()`); empty = provider genuinely absent. Today every run is empty — honest, forward-compatible.

## 3. Blocks and their tests
### Block 1 — three new tasks (one signed commit)
Each task is self-contained (own fixture, own check, no imports of other tasks' scripts). All use `go 1.26` (latest stable = repo's `go.mod` `go 1.26`, `bench/go.mod:3` — "latest stable Go" = the toolchain the bench resolves).

**1a. `collapse-long`** — mechanism decision, after Mashūra round 1 (see §7): **bench-only threshold override via a per-run config file** (`WAKIL_CONFIG`, `internal/config/config.go:1886-1898`; `LoadConfig` reads the JSON config file at `:1958-1980`, then env, then flags). The bench harness writes a temp config for the run that sets `context_collapse_ceiling_tokens` low (and a matching `context_collapse_min_bytes`) **identically for modules off and on**, so every model collapses at the same projected size regardless of its probed window — deterministic and model-independent, with Wakil byte-identical to v0.8.11. The brief explicitly allows this fallback ("only if the fixture can't do it reliably for all models: a bench-only collapse-threshold override, set the same way for modules off and on and recorded in the CSV"). **Recording**: the run's transcript/log records the override values; the CSV `collapses` count plus the log line make each run auditable. (The temp config must not carry secrets — it is built by copying only non-secret keys from the user's config or by documenting that the user points `WAKIL_CONFIG` at their own file and the harness overlays the two collapse keys via a generated file that references the endpoint by name only. Implementation detail: the harness generates a minimal config with the collapse keys and passes it via `WAKIL_CONFIG`; the model/endpoint come from `WAKIL_MODEL`/env as today. If a full config file is required for endpoint resolution, the harness copies the user's resolved config to a temp file, sets the two keys, and deletes it — never logging it.)
- Fixture (now modest, since the threshold is low): `repo/` = `docs/` with ~40 files (~8KB each, prose with one stated fact per file, generated by `setup.sh`+`gen/` with the seed outside the work folder, leak-safe as long-read). Task: read all files, write `report.json` with per-file facts. Because the ceiling is low, collapse fires early and repeatedly; values read *before* the first collapse must be *used after* it to produce the report.
- Sizing acceptance criterion: with the override ceiling set to **8000 tokens** (32k chars), reading ~5 files exceeds it, so ≥1 mid-turn collapse is expected for every model that reads files in-turn. The fixture total (~320KB) is ~2/3 of long-read's (480KB) — no timeout risk at the default 600s.
- Check: `report.json` equals expected per-file facts (expected values baked into `check.sh` like long-read's sums; seed kept in `gen/`, never in the folder; JSON compared after parsing with `python3`, so key order/whitespace do not matter, extra keys fail). Harness-side validity: completed collapse-long run with `collapses == 0` (or unreported) → `result=invalid`, `valid=false:no_collapse` (§2/§4). Known limit (stated in task.txt and README): an agent that scripts extraction or externalizes notes to disk may not trigger a collapse and is marked invalid — the invalid rate per model is a first-class metric alongside pass rate (Mashūra BLOCKING 1 accepted as a documented limit, not a fix: the brief requires the invalid marking). The task measures whether the agent retains early-read facts across a forced collapse when it reads in-turn.
- Determinism: fixture seed fixed; check is parsed-JSON equality. Model strategy varies (that is the measurement); scoring is deterministic.

**1b. `refactor-multi`** — Go module, API change across ≥8 files: rename + signature change.
- `repo/`: module with `shared/` defining `OldAdd(a, b int) int` used by **8 caller packages** (p01..p08, exceeding rename's 6), each with `Total(a, b int) int`. Change: rename to `NewSum` **and** change signature to `NewSum(a, b, c int) int` returning a+b+c. **Exact rule (Mashūra round 1):** `Total`'s own signature is unchanged; each `Total` body changes from `return shared.OldAdd(a, b)` to `return shared.NewSum(a, b, <K>)` where `<K>` is that package's constant (`p01`=1, `p02`=2, …, `p08`=8) — stated verbatim in task.txt so the task is well-defined. "Cross-package consistency" means: every package's `Total` must return `a+b+K` for its own K (hidden tests assert per-package K).
- Visible tests: `repo/` ships a visible test per package asserting the **old** behaviour (e.g. `Total(2,3)==5` for p01's old path). They pass on the untouched fixture; the agent must update them to the new behaviour (task.txt says so). They are not enough: gocheck deletes all agent tests (§1.3), so the check never sees them — only the hidden tests judge.
- `hidden/`: behaviour tests per caller package + shared (new 3-arg signature, a+b+c semantics, per-package K, edge cases incl. negatives/zero). Check: `gocheck_prepare` (allow-list: errors/fmt/math/strconv) + `gocheck_go build ./...` + grep old name gone **in non-test, non-comment code** (grep `-rn --include='*.go' 'OldAdd'` then filter out lines starting with `//` — a comment mentioning the old name does not fail; a dead-code reference does, because it still compiles against a missing symbol or is caught by the hidden tests) + grep `func NewSum` defined + `gocheck_run` all hidden tests. Untouched repo fails (old name present + hidden tests don't compile); solution passes; partial rename (missed caller) fails build.

**1c. `debug-subtle`** — off-by-one on a non-obvious path; minimal fix enforced.
- `repo/": package `window/` with two functions: `Take(s []int, n int) []int` (caller-visible) and `windowTake` (helper, two layers down). The bug is an off-by-one in `windowTake` (`i <= n` vs `i < n`) hit only when the window is exactly full. Symptom in task.txt is at the `Take` level ("when the slice is exactly as long as the window, one element is dropped") — the failing visible test calls `Take`, not the helper, so the bug is not localised to the function. Hidden tests call **both** `Take` and `windowTake` directly (so a caller-side workaround that patches `Take` still fails), covering empty, single, full, over-full, and negative `n` (negative `n` returns empty — specified behaviour, preserved from the fixture).
- `solution/": one-line fix (`i <= n` → `i < n`) inside `windowTake` only.
- Minimal-fix rule (Mashūra round 1 fixes): the check (1) copies `repo/` pristine and the agent tree, (2) **gofmt-normalizes both** (`gofmt -d` / `gofmt -w` on a temp copy — the check already runs Go, so no new dependency), (3) excises the buggy function body from both (awk: `^func windowTake` to the matching `^}`), (4) diffs the remainder with unified diff and counts changed (added+removed) lines **by old-file hunk coordinates** (`@@ -a,b +c,d @@`), so insertions inside the function do not shift the count. New/deleted `.go` files count their full line count as changed. **N = 4 lines** outside the function. Reference fix = 1 line; correct alternatives (condition restructure + guard) stay ≤4. Changes inside the function are unbounded in count but must still pass hidden tests (hardcoding boundary inputs inside the function is limited by the hidden tests' variety). Pristine `repo/` is shipped gofmt-clean (verified in implementation). N stated here and in `check.sh`.
- Determinism: exact hidden-test assertions; gofmt-normalized diff is deterministic.

### Block 2 — CSV columns (one signed commit)
Append `valid,provider` (in that order) after `modules_loaded`. `collapses` already exists (col 14) — **not duplicated**; brief's intent (every run records collapse count) is already satisfied. `csv_row` computes:
- `valid`: `true` normally; `false:no_collapse` iff task is `collapse-long` and collapses is `0`/`n/a`/empty (then `result=invalid`, written by the validity step after check.sh — see §4).
- `provider`: `json_field result_json provider` (empty today → empty cell); `mixed` never produced today (single result-json per run).
- Old-format test: self-test case `csv-old-format` writes a literal old CSV using the **exact header and one data row from `bench/results/20261120T002041-bench.csv`** (14 cols: no `collapses_turn_end`/`context_modules`/`modules_loaded`, verified in-session) and asserts `print_table`/`sum_cost`/`find_prev_csv` read it correctly (rows render, cost sums, model filter works) — the header-name readers already guarantee this; the test locks it in.

### Block 3 — provider pinning (verdict: no flag, no commit)
Per §1.6: no repo source verifies the OpenRouter pinning mechanism (`provider.order`/`only`). The `policy_test.go` `sort` fixture proves only that unknown provider fields survive the ZDR merge. Per the brief's own instruction ("if there's no source in the repo, say so and leave the flag out"), **Block 3 is closed with no code change**: no `--provider` flag, no Go changes, `provider` column stays empty until a future binary reports it. This is recorded here and in `bench/README.md` so the decision is auditable. (If the user later supplies the OpenRouter doc source, Block 3 can be implemented as a bench-only env/flag through the documented `provider` object — the `provider` column and `json_field` reader already support it.)

### Block 4 — self-test + docs (one signed commit)
- Self-test extensions (all in `self_test()`, fake backends only):
  - Generic noop/solve loops cover the 3 new tasks with **explicit stub overrides for collapse-long** (Mashūra round 1): the solve stub emits `collapses:2` for collapse-long (so solve→`pass`), the noop stub emits `collapses:0` for collapse-long (so noop→`invalid`, not `fail` — asserted as such), and the other two new tasks follow the generic fail/pass. The loops assert per-task expectations, not a blanket fail/pass.
  - **Wrong-solution case**: new stub applying a *wrong* solution (e.g. collapse-long: report with one wrong value; refactor-multi: renamed but old signature; debug-subtle: fixed but with an extra 10-line change outside the function) per new task → must `fail`. Implemented as targeted `run_one` cases (not the generic loops).
  - **collapse-long invalid case**: stub with `collapses:0` + correct report → `result=invalid`, `valid=false:no_collapse` (check passed but run invalid — proves validity is independent of check outcome).
  - `csv-old-format` case (Block 2).
  - Reference-solution invisibility: existing `leak_check` + post-run leak guard already cover `solution`/`hidden`/`check.sh`/`setup.sh`/`gen` for every task generically; new tasks obey the same layout, and the self-test asserts (like long-read) that `prep_workdir` for each new task leaves no `solution/hidden/check.sh/setup.sh/gen` in tree or history — extend the existing long-read-only assertion to all tasks with those dirs (or add per-task equivalents). Paths: references live at `bench/tasks/<name>/solution/` and `hidden/`; the agent's sandbox sees only `work_root/<task>/` (repo copy + setup output); `check.sh`/`hidden`/`solution` are never copied in (`prep_workdir`), and the post-run guard + `leak_check` enforce it (`bench.sh:653,927-939`).
- Docs: `bench/README.md` (new tasks in Run/Adding-a-task, new columns, validity semantics, Block 3 verdict, collapse-long sizing note), `CHANGELOG.md` [Unreleased] (one line per user-visible change).

## 4. Validity implementation point (harness)
Single computation point in `csv_row` (no override in `run_one_locked`): `csv_row` takes `result` as an argument; if `task = collapse-long` **and** `result` is `pass`/`fail` (completed, check ran) **and** `collapses` reads as `0`/empty/`n/a`, it rewrites `result=invalid` before writing (so all call sites behave identically). `valid` is then derived from the final `result`: `invalid` → `false:no_collapse`; `timeout` → `false:timeout`; `error` → `false:error`; `pass`/`fail` → `true`. `provider` = `json_field result_json provider` (empty today). This keeps one rule, no contradictory branches: timeout/error rows are never `valid=true` (§2).

## 5. Tests per block (summary)
- B1: `--self-test` noop→fail ×7 + noop→invalid (collapse-long) ×1, solve→pass ×8 (with collapses override), wrong-solution→fail ×3 (new), collapse-long invalid case (new); manual: each new `check.sh` run directly on repo (fail) and repo+solution (pass) in a temp dir.
- B2: `csv-old-format` self-test case; `csv_cell` assertions on new columns; full `--self-test` still green.
- B3: none (no code). Verdict documented.
- B4: full `--self-test` output captured for the report.
- Gates (final): `gofmt -l cmd internal prompts scripts` (empty), `go vet ./...`, `golangci-lint run`, `go test -count=1 -timeout 30m ./...`, `scripts/bench.sh --self-test`, shellcheck on changed scripts (SKIP if absent, reported).

## 6. Open risks
1. **collapse-long config override.** The per-run temp config (§3.1a) must not leak secrets or change endpoint/model resolution. Mitigation: harness copies the user's resolved config to a temp file, sets only the two collapse keys, passes it via `WAKIL_CONFIG`, deletes it after the run, never logs it. If the copy approach proves fragile, fall back to fixture-only sizing with the documented invalid-rate limit. The real bench (run by the user) is the calibration for both.
2. **debug-subtle line-diff fragility.** Mitigated in §3.1c by gofmt-normalizing both sides before diffing and counting by old-file hunk coordinates; pristine fixture ships gofmt-clean. Residual risk: an agent that reformats with a non-gofmt style still normalizes away — only real changes count. Accepted.
3. **refactor-multi third argument.** The per-package constant must be unambiguous from task.txt, else the task measures guessing. Mitigation: task.txt states the exact new signature and the per-package argument rule explicitly.
4. **New result value `invalid`.** Downstream consumers grouping by `result` must tolerate it; old CSVs unaffected. `print_table` prints it verbatim (no code change needed beyond header-based reads).
5. **Self-test runtime.** 3 new tasks × (noop+solve+wrong) + preflight on 3 Go tasks (refactor-multi, debug-subtle, collapse-long's Go generator via setup.sh) adds minutes. Acceptable (CI already runs full self-test); no caching tricks.
6. **Block 3 gap.** Until a binary reports `provider`, the column is always empty — a reader expecting values today sees blanks. Documented as forward-compatible schema, not a bug. OpenRouter runs are therefore not comparable across serving providers until a future binary reports it; stated in `bench/README.md`.

## 7. Mashūra round 1 — findings and disposition
Round 1 ran on this plan (3 reviewers). Folded as BLOCKING: bench-only collapse-threshold override via `WAKIL_CONFIG` (replacing fixture-only sizing); collapse-long acceptance criterion (8000-token ceiling, ~40×8KB fixture); stub overrides so noop/solve loops expect invalid/pass correctly; `valid` semantics fixed (timeout/error → `false:<reason>`, single computation point in `csv_row`); debug-subtle gofmt-normalized hunk-coordinate diff + helper-direct hidden tests + symptom at caller level; refactor-multi exact K rule + comment-filtered grep; old-CSV test uses the real historical header. Listed, not folded: scripting/scratch-file bypass of collapse-long (documented known limit, as with long-read); provider comparability gap (brief-ordered Block 3 closure); existing-task thinness (fix-test/add-flag/long-read) — out of scope for this session; task/fixture hashing in CSV — useful future work, not in brief; `invalid` denominator rule — folded into §2/README.

## 8. Fix 1 — collapse-long run config and ceiling (2026-11-24, from 47b2fb4)

### 8.0 What broke on the host
`--task collapse-long --model anthropic/claude-haiku-5.5` failed: the per-run `WAKIL_CONFIG` (`scripts/bench.sh:812-818`, two collapse keys only) REPLACED the user's config, so the run had no endpoints (`no endpoint configured …: run wakil init --force`). Side effect: missing workdir in the result (`binary did not report workdir`). Root cause: Wakil reads exactly ONE config file.

### 8.1 Verified findings (config resolution)
- **One file.** `LoadConfig` (`internal/config/config.go:1950-1972`): `DefaultConfig()` ← one file (`os.ReadFile(cfgPath)` at `:1964`, single `json.Unmarshal` at `:1972`) ← env block (`:2148-2186`) ← flags (`:2188+`). No second/project/repo/`.wakil` merge in `LoadConfig` (RepoState mentions at `:370,:456,:465` are comments about per-repo settings restored elsewhere, not a file layer here).
- **Path.** `defaultConfigPath` (`:1890-1899`): `--config`/`-config` (`ConfigPathFromArgs :1934-1947`) > `WAKIL_CONFIG` > `~/.config/wakil/config.json`.
- **Env overrides** (`:2149-2186`): `SEARXNG_URL; WAKIL_MENTION_BASE; ILM_/WAKIL_BASE_URL, HOST, PORT, API_KEY, MODEL, EXEC_MODE; WAKIL_SANDBOX_BACKEND, OUTPUT_MODE, CLIPBOARD_MODE, IMAGE, WORKDIR, HOST_WORKDIR, DOCKER_SOCKET, SSH_SIGNING, KVR_*, TRACE_*; WAKIL_CONTEXT_MODE, PROMPT_MODE, CONTEXT_MODULES`. **No `WAKIL_CONTEXT_COLLAPSE_*` env var exists** — the two collapse keys are file-only (or flags, if any). So preference 1 (an existing layering/env mechanism) is **unavailable**: there is no way to set the two keys on top of the user's file without a merged file.
- **Choice: preference 2** — `bench.sh` merges the user's resolved config with only the two collapse keys via `jq` (a verified bench dependency: `scripts/bench.sh:25` `harness_tools` includes `jq`; `json_field :460-465` already shells to it). Wakil stays byte-identical (`git diff 6d1a86a..HEAD -- internal cmd prompts` must stay empty).

### 8.2 Verified findings (what the ceiling counts)
- **Conversion.** `midTurnCollapseCeiling` (`internal/agent/collapse.go:1866-1871`): chars = `ContextCollapseCeilingTokens × charsPerToken` (4, `internal/agent/compact.go:15-18`); `<=0` disables. Token estimate is the same chars/4 as `proxy.ApproxTokens` (`internal/proxy/cost.go:535-542`) — a byte proxy, not a tokenizer.
- **Trigger.** `collapseMidTurnPrefix` (`:1889-1916`): between tool calls in `streamTurn`; fires when `sentSize(contextProjection())` exceeds `hardMax` (95% × effective context, `config.go:1584`, `compact.go:227+`) **or** the absolute ceiling. Tail kept ≤30% of the threshold.
- **What `sentSize` counts.** `TranscriptSize(messages) + styleSelection().Bytes()` (`internal/agent/styleguides.go:430-432`); `TranscriptSize` = Σ `len(Content)` + Σ `len(ToolCall.Arguments)` (`internal/core/format/format.go:43-54`) — no JSON envelope/role overhead.
- **`contextProjection()`** (`:789-797`) = `pruneProjection(collapseProjection(Conv))` — **messages only**. The system prompt is **included** (Conv[0], refreshed by `ensurePreamble`, `internal/agent/app.go:2254-2277`); the task message is **included**; the style-guide note is **included** arithmetically (`Bytes()`); the **tool array/schemas are NOT counted** (built separately, passed as `Stream(ctx, msgs, tools)`, `turn_phases.go:310,396`) — so the real request is larger than the measured one.
- **Modules on/off delta is real.** Preamble = core/agent/extension prompts (52,879 + 18,573 + 34,606 = 106,058 bytes) + **module index appended when on** (`app.go:2058-2061`) + different inventory lines (`:2100-2145`). The tool array (not counted) also differs in size. So modules-off collapses **earlier** at the same ceiling; the counts will not be equal. Per the brief: state this plainly in the README ("on this task, off vs on partly measures prefix size"), do **not** compensate with per-mode values.

### 8.3 The target is smaller than assumed (fixture size)
`read_file` on an 8KB file returns ~8KB **plus** line-number gutter and function markers (`read_file` capped at 256KB covers each ~8KB file whole; `read_file_full` up to 256KB). But `toolResultSize`/`ToolResultCap` is 8000 (`config.go` default `ToolResultCap: 8000`, `CapOrStub app.go:2464`/`toolcap.go:480+`): reads **over 8000 chars are cut to ~8000 chars + a suffix pointing to the spill file**. A `read_file` of an ~8110-byte fixture file is ~8110+ content+gutter — borderline over the cap, so the **in-context tail is ~8K chars**, not the full 8KB + gutter. This *reduces* per-read growth and shifts the ceiling choice down; measurement (§8.5) must use the actual bytes that enter Conv, not the on-disk size. (If reads stay under 8000, full content lands; the table will show the real numbers.)

### 8.4 Blocks and their tests

**Block 1 — merged run config (secret-safe).** Touched: `scripts/bench.sh` (`run_one_locked` collapse-long branch `:803-838`), self-test cases.
- Resolve the user's config **exactly as Wakil will**: `--config` is never passed by the bench; the bench must resolve `WAKIL_CONFIG` (if set in the bench's env) else `${HOME}/.config/wakil/config.json` (XDG_CONFIG_HOME is not consulted for this path — `defaultConfigPath` uses `UserHomeDir()`, `config.go:1890-1899`). If the file is missing or unparseable: the run is written with `valid=false:config` and a clear log line; **never** fall back to an endpoint-less config.
- Merge: `jq '. + {context_collapse_ceiling_tokens: <C>, context_collapse_min_bytes: <M>}' userconfig > merged`. Ceiling/min_bytes are the §8.5-chosen values (constants in bench.sh, identical off/on).
- **Secret rules:** `mktemp -d` private dir (0700, never under `bench/results/` or the repo); file mode 0600 (`umask 077` before creation); trap cleanup on EXIT/INT/TERM (`rm -rf` the private dir, covering success, failure, and timeout kills — the timeout kills the *wakil* process group via `timeout -k`, the bench shell's own traps still fire on the normal wait path and on INT/TERM); contents/path/keys never printed, logged, copied into the run log or CSV; run log says only `collapse-long override: merged config (2 keys)`.
- The `cleanup` trap in bench.sh currently also removes the per-run temp config dir under `run_root` — that dir (`$run_root/$task-config`) is replaced by the mktemp dir; make sure no stale dir is left under `run_root` either.
- **Tests (self-test, fake configs only):** (a) fake user config (with a fake `endpoints` block) + merge → merged keeps `endpoints`, sets exactly the two keys (assert by jq in the test, fake values only — never the real config); (b) temp file gone after success, failure, and timeout (assert no file remains in the private dir's parent); (c) a fake secret value (`AKIA_FAKE_SENTINEL`) in the fake config never appears in run log, CSV, or stdout (grep for the sentinel); (d) missing config → `valid=false:config` row, clear message, no run.

**Block 2 — ceiling that fires 1–3× in both modes.** Measurement without real model calls (details §8.5–8.6). Pick (ceiling, min_bytes) so both modes collapse 1–3× on the replay; counts as close as the prefix-size asymmetry allows; same values both modes. Self-test asserts the chosen values give 1–3 collapses in both modes on the replay.

**Block 3 — docs.** `bench/README.md` (how the override works now, merged-config privacy, ceiling choice + table + the prefix-size caveat), `CHANGELOG.md` [Unreleased] bench entry.

### 8.7 Calibration table (measured, throwaway replay — 2026-11-24)
A throwaway Go test in `internal/agent` (package `agent`, deleted before commit) drove **one long-session turn** through the real `streamTurn` + real `read_file` tool path with a scripted SSE fake backend: 40 sequential `read_file` calls in fixture order, then a final content answer. Real fixture bytes (fixed seed 42), real `ToolResultCap=8000` (each read lands ~8K in Conv — fact at bytes 54–113, **inside** the kept head, so truncation never hides the fact), real preamble via `buildPreamble` (off: 53,084 bytes; on: 53,500 bytes — **on is 416 bytes larger**: the module index is appended, and off actually collapses *later*). One turn = 40 tool calls (not 40 turns — turn-end/min-bytes plays no role; `wakil run` is one user turn, so `min_bytes` is set only to neutralise a user value).

Mid-turn collapses (`[mid-turn]` lines = `midTurnCollapses`):

| ceiling (tokens) | ceiling (chars) | off | on |
|------------------|-----------------|-----|----|
| 8000 | 32,000 | 39 | 39 |
| 16000 | 64,000 | 38 | 38 |
| 24000 | 96,000 | 12 | 13 |
| 30000 | 120,000 | 7 | 7 |
| 34000 | 136,000 | 5 | 6 |
| 36000 | 144,000 | 5 | 5 |
| 40000 | 160,000 | 3 | 4 |
| **45000** | **180,000** | **3** | **3** |
| 55000 | 220,000 | 2 | 2 |
| 60000 | 240,000 | 2 | 2 |
| 70000 | 280,000 | 1 | 1 |
| 90000 | 360,000 | 1 | 1 |
| 120000 | 480,000 | 1 | 1 |
| 200000 (default) | 800,000 | 1 | 1 |

**Chosen: 45,000 tokens (180,000 chars), `min_bytes = 2000` (unchanged default).** Both modes = 3 collapses on the replay — the tightest point where off and on agree (off is 0–1 below on elsewhere). The post-collapse `contextProjection()` sizes measured ~30–80KB, well below 180,000, so the preamble tail is not exceeding the ceiling. Self-test asserts 1–3 collapses in both modes at this ceiling (replay through the stub harness — the stub emits `collapses:3` for collapse-long, so a solved run is pass/valid and a 0-collapse run is invalid).

Caveat (held from §8.2 + Mashūra): the measured prefix is off 53,084 vs on 53,500 bytes — on collapses marginally earlier. The full 106,058-byte prompt total is not all in `Conv[0]` (the extension prompt is a lazy second system message, not counted by `contextProjection` until activated). The table is a property of the replay strategy, not of a real model run.

### 8.8 What the replay did NOT do
The replay used defaults (`ToolResultCap=8000`, `ContextMode=long-session`), not the user's config — real runs inherit the user's `tool_result_cap`/`context_mode`. The replay did not simulate modules-on `load_module` traffic (loaded modules change context growth run-to-run). The 1–3 claim is about the replay; real runs may differ. Stated in `bench/README.md`.

### 8.9 Mashūra round 1 — findings and disposition
Round 1 ran on §8.0–8.6 (3 reviewers). Folded as BLOCKING:
- **Agent-side leak path (config inside sandbox):** the merged config must not be readable by the sandboxed agent. The bench now places it in a private dir (`mktemp -d` → 0700) and resolves the user's config **without copying into the work folder or `run_root`** — the sandbox only mounts the work folder (§1.1 `WAKIL_HOST_WORKDIR`) and the config path is passed via env, so the agent's tools run inside the sandbox and cannot `cat` the host temp dir. README states this and that wakil's own error output is not covered (a stub only proves bench-side silence; a wrong-config error from wakil could name an endpoint URL in the transcript).
- **Trap clobbering:** bench.sh has NO `cleanup` trap on the normal path and `st_on_exit` on EXIT in self-test (`scripts/bench.sh:1216-1225,2889`). A local trap would clobber it. Instead: a global `_bench_cfg_tmp` is set before the file exists, removed **explicitly right after `wait "$run_pid"` on every branch** (success, failure, timeout — before any `return 1`), and a backstop global cleanup in the normal-mode EXIT trap + `st_on_exit` removes it if unset. INT/TERM handlers remove the temp dir and `exit` (no clobbering — only the existing `st_on_exit`/a single composed handler references the global).
- **Self-test must never read the real config:** `self_test()` now sets `WAKIL_CONFIG` to a fake user config (fake `endpoints` block) before any `run_one collapse-long`, and every `run_one` against collapse-long in the generic loops is inside that env. `tools-list-sufficient` (`env -i HOME=<empty>`) gets a fake `WAKIL_CONFIG` too (its collapse-long row was non-pass in CI's no-config env).
- **Missing file → merge onto `{}`:** Wakil tolerates a missing file at `config.go:1964-1971` (`os.IsNotExist` → defaults + env still apply). So a missing user config is **merged onto `{}`** (endpoints can come from env), NOT an abort and NOT `valid=false:config`. **Only an unparseable/invalid JSON file** is `valid=false:config`. (This changes the §8.4 test (d): missing → run proceeds with an empty base; unparseable → `false:config`.)
- **`valid=false:config` contract:** `csv_row` derives `valid` from `result` only (`invalid`→`false:no_collapse`, `timeout`→`false:timeout`, `error`→`false:error`). A non-JSON config is **not a bench control-flow stop**: the run proceeds, wakil itself errors on the unparseable config (`config.go:1972-1973`), its error lands in the log, the run is `result=error`, `exit_reason=config-unreadable` (detected by `grep` on the log, beside `sandbox-not-acknowledged`), and `csv_row` maps **that specific exit_reason** to `valid=false:config` instead of `false:error`. `run_one_locked` returns 0 (the bench continues, like any other `error` row). The brief's `valid=false:config` is exactly this.
- **Ceiling below preamble:** the §8.5 candidates (4K–32K) were below the preamble (53,084 bytes off measured). Corrected by the calibration table (§8.7): candidates now run 32K–800K chars and the chosen 180,000 clears the preamble with headroom. The trigger is `projected > ceiling`, so "prefix near ceiling" is not an under-fire risk.
- **`jq` stderr/umask:** jq runs in a subshell `( umask 077 && jq … ) 2>/dev/null` with exit code checked; never launched with a partial file. `jq '. + {…}'` is a shallow merge — correct here because both collapse keys are top-level (`config.go:373,382`).
- **`chmod` not in `harness_tools`:** avoided — `mktemp -d` (0700) + subshell `umask 077` only.
- **Test (b) parent meaningless / test (c) stub limits:** (b) asserts the private dir itself is gone **and** no merged file remains anywhere under the `TMPDIR` parent created by the run (checked via the known path), plus the private dir is 0700 and the merged file 0600 (`stat`) in a stub that asserts it received `endpoints` + both keys + the modes. (c) sentinel grep covers run log, CSV, and stdout — and the README states wakil's own error output is not covered.
- **Relative paths in a copied config:** the merge copies the file's contents; any relative path inside it (certs, work dirs) resolves relative to the **merged file's location** (the private dir), not the original. Accepted and documented: endpoint resolution does not depend on the config's location (`LoadConfig` reads only the file); the bench does not copy trust state files. If a user's config uses relative cert paths, collapse-long runs will error like a wrong `WAKIL_CONFIG` — a documented limit, not a silent pass.
- **Non-collapse-long runs never set `WAKIL_CONFIG`:** the merge branch is guarded by `task == collapse-long`; a non-collapse-long run keeps the current no-override path. Self-test asserts a non-collapse-long run has no `WAKIL_CONFIG`.
- **Ceiling recorded:** the run log now records the two non-secret numbers (`ceiling=45000 min_bytes=2000`), not just "(2 keys)", so the values are auditable without a new CSV column (appended columns are additive-only; a column is not added for two constants).
- **Replay vs internal diff:** the replay test was throwaway and is deleted; `git diff 6d1a86a..HEAD -- internal cmd prompts` stays empty. The bench `45000` constant and the plan table are the record; the self-test asserts the replay property through the stub harness (`collapses:3` at the chosen ceiling).

Listed, not folded: agent reading the merged config is not possible via the sandbox mount (the sandbox sees only the work folder — a direct-mode run on the host could `cat` a known path, but the path is never logged and lives in a 0700 host temp dir; same exposure as the user's real `~/.config/wakil/config.json`, which the agent under test can equally read — the bench model is "the agent is not adversarial about config paths"). SIGKILL of the bench shell leaves the 0600 temp file (unrecoverable by trap; a stale sweep is the only fix, not implemented). Making the off/on comparison "fair" beyond this mechanism (different ceilings per mode) is forbidden by the brief. `min_bytes` is a no-op (one user turn; turn-end counter does not fire in a one-turn run) and is set only to neutralise a user value.

### 8.5 Measurement method (no real model calls)
Build a deterministic replay of the task through a fake backend that uses **Wakil's real tool array and real prompt for each mode**:
1. For each mode (off, on): construct the App exactly as the bench does (default config with only the two collapse keys set; `ContextModules=off|on`; long-session; the real `collapse-long/task.txt` as the task; workdir = the generated fixture).
2. Fake backend emits a fixed script: N turns, each turn = one `read_file` of `docs/note-KK.txt` in order (K=01..40), then a final turn writing `report.json` (content not needed for size). The fake backend returns the **actual bytes `read_file` would return for the real fixture files** (read the generated docs from disk — the generator is deterministic, seed 42), so `TranscriptSize` measures real Conv growth.
3. At each between-tool-call boundary, compute `sentSize(contextProjection())` and fire `collapseMidTurnPrefix` semantics for candidate ceilings (8000, 4000, 2000, 1500, 1000 tokens → 32000/16000/8000/6000/4000 chars, plus the no-override default 200000); count collapses per mode.
4. Where to run it: a Go test or a small Go program under `internal/agent` test harness that already fakes the backend (there are existing agent tests with a fake Stream backend — reuse that harness), **or** extend the bench self-test's stub machinery. Prefer: a committed Go test (e.g. `internal/agent/collapse_replay_test.go` or a bench-internal package) that can be run with `go test` and is part of `go test ./...` — the self-test then shells it or the test itself asserts 1–3 collapses per mode at the chosen ceiling. The chosen ceiling is then written into `bench.sh` as a constant, and the self-test asserts (via the replay) that 1–3 holds in both modes.
5. `min_bytes` (turn-end collapse threshold, default 2000): keep unless the replay shows turn-end collapses dominating mid-turn ones; the count that matters for validity is mid-turn `collapses` — verify which counter the replay's count maps to and state it.

### 8.6 Open risks
- **Secret spill paths:** the merged file is the one secret-bearing artifact in the bench. Risks: (i) a copy left behind on a crash before the trap; (ii) `set -x`/debug output; (iii) the fake-backend replay running the merged config. Mitigations: trap on EXIT/INT/TERM installed before the file exists; the replay test uses a fake config, never the merge path; self-test asserts (b) and (c) above.
- **The replay is not the real model.** Real agents read files in different orders, sometimes use `run_shell cat`, sometimes batch reads. The replay fixes one canonical strategy; the real `invalid` rate on the host is the calibration. The task remains valid (invalid rows are first-class), but the 1–3 claim is about the replay, stated as such.
- **8000-char tool-result cap** (§8.3) makes per-read growth ~8K chars; the current 8000-token ceiling (32K chars) may only fire ~2-3 times after the prefix is counted — or may not fire at all if the prefix alone is near 32K. The table decides; do not assume 8000 survives.
- **Prefix-size asymmetry** (§8.2) is accepted and documented, per the brief (no per-mode compensation).
- **Timeout rows:** a `timeout -k` kill mid-run could leave the merged config if the bench shell is also killed hard (SIGKILL). The trap covers EXIT/INT/TERM; SIGKILL of the bench shell itself is unrecoverable by design (same as every other bench temp file under run_root) — noted, not solved.
- **bench.sh change surface:** `run_one_locked` is the hottest function in the harness; the merge must not alter behaviour for other tasks (guard: only the `collapse-long` branch changes; other tasks keep the current no-override path) and must not break the existing self-test cases (cheat, lock, timeout 137, workdir guard).

## 9. Fix 2 — relative paths and result label (BASE 09d2e38, Tier 3)

Host smoke (2026-11-25, `collapse-long`, modules off): the merge works
(`merged config (ceiling=45000 min_bytes=2000)`), then wakil errors at bootstrap —
`bootstrap error: style_guide_paths["go"]: opening /tmp/tmp.9KKjSeqpnp/styles/go.md:
open …: no such file or directory`. The user's config uses the relative path
`styles/go.md`; with the merged file in a private temp dir, Wakil resolves it there
and fails. The CSV row says `result=invalid`, `exit_reason=error`, 0 s — a run that
never started must not be `invalid`. Two fixes: make relative paths resolve exactly
as in the user's normal setup, and label pre-model exits as `error` with a specific
reason (and print the single `bootstrap error:` line to the terminal).

### 9.1 Verified findings (read this session; file:line)

**How relative paths resolve.**
- `style_guide_paths` is the only key resolved relative to the config file's
  directory: `baseDir = filepath.Dir(cfgPath)`, `absBase, _ := filepath.Abs(baseDir)`,
  per value `expandTilde`, then `!IsAbs → Join(absBase, path)`, `Clean`
  (`internal/config/config.go:2503-2520`). The comment says it plainly: "Style guide
  paths are host-side user files. Relative paths follow the selected config file, not
  the project workspace or launch directory." The error the user saw is categorical:
  `style_guide_paths["%s"]: opening %s: %w` (`internal/config/config.go:2135-2142`;
  printed as `bootstrap error: %v` by `internal/wiring/bootstrap.go:766-770`).
  `styleguide.NewCatalog` opens the values verbatim (`internal/styleguide/styleguide.go:367-372`)
  — "Paths must already be resolved by configuration loader" (`:54-56`); it does no
  resolution of its own. Resolution only happens when `StyleGuidesEnabled && len>0`
  (`config.go:2505`); disabled/empty leaves raw values.
- `agent_prompt_path`: the DEFAULT is derived config-relative
  (`filepath.Join(Dir(cfgPath), "agent.txt")`, `config.go:2496-2501`), but an
  EXPLICIT value gets no tilde/abs/config-relative treatment in LoadConfig — it is
  passed verbatim to `os.ReadFile` (`internal/wiring/bootstrap.go:659-663`), so an
  explicit relative value is resolved against the **process cwd** at read time.
  `trace_dir` gets `expandTilde` only (any source: file/env/--trace-dir), no abs, no
  config-relative — a relative value stays relative (`config.go:2289-2301`, key at
  `:826`). `expandTilde` itself only expands bare `~`/`~/…` via `os.UserHomeDir`
  (`config.go:2566-2581`; used at `:2301` and `:2515` only).
- Key inventory (path/dir/file keys): `work_dir` (`:278`), `host_work_dir` (`:279`),
  `agent_prompt_path` (`:776`), `style_guide_paths` (`:782`), `trace_dir` (`:826`),
  `browser_path` (`:846`). Only `style_guide_paths` is config-file-relative. Note the
  defaults for the two that read files/dir at the config site: `agent.txt` default
  sits next to the config file; `trace_dir` defaults to a data-dir path, not the
  config dir. Any merged config at a temp path therefore changes resolution for
  exactly `style_guide_paths` (the user's failure) and the implicit `agent.txt`
  default — both resolved from the merged file's directory today.
- The resolved config path: `$WAKIL_CONFIG` if set, else
  `${XDG_CONFIG_HOME:-$HOME/.config}/wakil/config.json` (`config.go:1890-1899`).
  LoadConfig reads exactly one file, `--config` > env > default (`:1950-1972`).

**Writes into the config directory during a normal run: none.**
- `LoadConfig` only `os.ReadFile`s ("nothing is written", "never writes file",
  `config.go:1964,1969,1905,2053`); no `WriteFile`/`Create`/`MkdirAll` anywhere in
  `internal/config`; `styleguide` only opens+reads (`styleguide.go:369`). So a normal
  `wakil run` writes nothing into the config directory, and the mirror can expose
  every entry for reading without the run modifying the user's real files.
- Writes that exist but go elsewhere: `wakil init` writes config (explicit user
  command, `internal/initcmd/write.go` — not a `run`); the memory store
  (`internal/memory/store.go:181,419`, via `WAKIL_MEMORY_DB`) and session data live in
  the data dir (`~/.local/share/wakil`), not the config dir. `trace_dir` is only
  written if the user points it into the config dir (a pathological config; the
  merged file then resolves `trace_dir` identically to a direct run).
- Caveat the discovery raised and I do not bury: whether headless `BuildApp`
  triggers repo-state/session saves on every run was not fully traced; its
  location is the data dir, not the config dir. Mashūra round 1 flagged this as
  the load-bearing assumption. Residual risk accepted with the byte-identity
  self-test below as the watching proof (it cannot prove the real binary — no
  astrix can; the sandbox run on the host will be the first real proof).

**Current bench code (scripts/bench.sh).**
- The merge sits in `run_one_locked` (`:826-857`): `_user_cfg` = `$WAKIL_CONFIG` or
  `~/.config/wakil/config.json` (hardcoded HOME path, not XDG-aware); missing →
  merge onto `{}`; `mktemp -d` (0700) + `(umask 077 && jq '. + {ceiling, min_bytes}')
  2>/dev/null`; unparseable → `csv_row error/config-unreadable` + return 0
  (`:844-851`); missing-file branch printf's the two keys without an umask guard
  (`:853`); `WAKIL_CONFIG=$_cfg_file` is passed to the run (`:861,869`); deletion is
  `rm -rf "$_bench_cfg_tmp"` right after `wait` (`:922-925`) with EXIT/INT/TERM
  backstop (`st_on_exit`, normal-mode trap). The private dir is removed with
  `rm -rf` — safe today because it contains no links; it must change when links
  are introduced (see Block 1).
- Result labels today: `csv_row` (`:555-581`): collapse-long `pass|fail` + 0 collapses
  → `invalid`/`false:no_collapse` (`:558-560`); timeout → `false:timeout`; error →
  config-unreadable maps to `false:config`, else `false:error` (`:561-569`). In
  `run_one_locked`, a run with no workdir (e.g. a bootstrap failure, which never
  writes a result JSON) falls to the missing-workdir guard (`:993-1000`):
  `error/missing-workdir`, bench stops, all of `$log_file` is `cat`'d. A
  `bootstrap error:` line lands in `$log_file.out` (`:931`, appended to the log) but
  is never printed to the terminal.
- Self-test already has the merged-config cases with a fake config (`:1540-1582`,
  from Fix 1): endpoints preserved, exactly two keys, 0700/0600, deletion after
  success/failure/timeout, fake sentinel `AKIA_FAKE_SENTINEL` absent from stdout/log/CSV,
  bad config → `error/config-unreadable/false:config`.

### 9.2 Block 1 — mirrored config dir (preferred approach, adopted)
The key-agnostic mirror from the brief, made precise against the merge code
(Mashūra round 1 folded BLOCKING items are marked with †):

- In the same private temp dir (mode 0700, `mktemp -d`, as today), **for every entry
  of the user's config directory except the resolved config file, create a symlink**
  pointing to the original † (`ln -s` on path entries, not names the bench inferred).
  Write the merged `config.json` (0600, as today) and point `WAKIL_CONFIG` at it.
  Relative paths then resolve identically to a normal run for every key — no key
  list to keep in sync, and the mirror covers the implicit `agent.txt` default too
  (which the key-rewrite fallback could not, since it only rewrites explicit values).
- † The mirror base is canonicalised first: `cfgdir=$(cd "$(dirname "$_user_cfg")" &&
  pwd -P)` — an absolute physical path — so relative `WAKIL_CONFIG` still links
  correctly, and symlinks in `$HOME`'s components are resolved once. The exclusion
  is the resolved source file itself (`$_user_cfg`'s basename AND its full path),
  not the literal name `config.json`: a custom `WAKIL_CONFIG=/x/alt.json` must not
  have `alt.json` mirrored. If the config file is missing today (`_user_cfg=""`,
  merge onto `{}`), there is no directory to mirror: keep the current no-mirror path.
- † Safety guard on shared directories: if `cfgdir` is `$HOME`, `/`, or a directory
  that is not Wakil's own config dir (i.e. the resolved user config is not
  `~/.config/wakil/config.json` and its basename is not `config.json` under a
  `wakil` dir), do NOT mirror the whole directory — instead skip the mirror and
  write only the merged file (relative paths will then resolve against the temp
  dir as today, a documented degradation, not a directory of secrets symlinked
  into a temp dir). Concretely: mirror only when `_user_cfg` ends in
  `/.config/wakil/config.json` (Wakil's own default) or when `WAKIL_CONFIG` points
  into a directory whose basename is `wakil`; otherwise merged-file-only.
- Cleanup removes only the merged file and the links themselves, then `rmdir` —
  never `rm -rf` on a dir that may contain links. Implement as a recorded-manifest
  helper †: at mirror creation, record the set of names created; cleanup deletes
  `config.json` first † (secrets gone even if something below fails), then `rm -f`
  each recorded link (guard `[ -L ]` before unlinking †, never a trailing slash),
  then `rmdir` — if `rmdir` fails because the dir isn't empty, the helper reports
  the leftover loudly (names only) instead of deleting recursively. Replace the
  current `rm -rf` at `:923` (and `:847`) with this helper, used by every cleanup
  site (post-wait, the unparseable early-return, and the EXIT/INT/TERM backstop).
- The `../` limit: relative resolution today is `Join(absBase, path)` (`:2517`), so
  `../x` escapes the config dir identically in both layouts **only if** the temp dir
  sits at the same depth — it doesn't. Detection † (now narrow †): after merging,
  if the merged JSON has any string value under `style_guide_paths` † (the only
  config-file-relative key, §9.1) that, after `Clean`, starts with `../` or equals
  `..` or contains a `../` segment that `Clean` collapses to an escape →
  stop that run, `csv_row error/config-relative-escape` (mapped to
  `valid=false:config` † — a new `csv_row` branch, mirroring `config-unreadable`),
  return 0 (bench continues, like `config-unreadable` †), and write only the key
  name to the run log. Implement with jq extracting `style_guide_paths` values
  (tolerating the object shape and nulls, stderr discarded), then a shell check
  on the cleaned value. No other key gets the detector: `agent_prompt_path` and
  `trace_dir` are cwd-relative (§9.1), so `../` there is a valid user config the
  mirror does not break.
- Because a normal run writes nothing into the config dir (9.1), the mirror needs
  **no write-redirect**: every entry is linked for reading only. If that fact ever
  changed, entries written by a run would be modified through the link — so the
  self-test byte-identity check below is the permanently-watching proof.
- The config path is settled, in Wakil's favour: `defaultConfigPath` reads
  `$WAKIL_CONFIG` else `UserHomeDir/.config/wakil/config.json`
  (`config.go:1890-1899`) — it does NOT consult `XDG_CONFIG_HOME`. The bench's
  hardcoded `$HOME/.config/wakil/config.json` (`:828`) matches. (Mashūra round 1
  caught a contradiction in my §9.1 draft; the code confirms the bench.)

Fallback (not taken): rewriting the known path-valued keys to absolute paths from the
original config's directory would work for explicit `style_guide_paths` but misses the
implicit `agent.txt` default and `agent_prompt_path`'s cwd behaviour, needs a key list
kept in sync with `config.go`, and needs a self-test that fails when `config.go`
gains a path key. The mirror is strictly simpler and more robust.

**Tests (self-test, fake config dir only; `ln` and `sha256sum` are both in
`harness_tools`/`selftest_tools`, `:25-27`).**
- A fake config dir (`$tmpdir/fakeconfig/`) with `config.json` (fake endpoints),
  `styles/go.md` (fake guide), plus other entries (a subdir with a file, a plain
  file, a dotfile): the fake config sets `style_guide_paths` to the relative
  `styles/go.md`. The self-test runs a stub that emulates Wakil's resolution
  (join the relative path against the dirname of `$WAKIL_CONFIG`, assert the file
  exists and is readable) — the only place the self-test can check resolution
  without a real binary — and asserts the bench's merge preserved `endpoints`.
- Byte-identity †: `sha256sum` of every file in the fake config dir before and
  after each of {success, failure, timeout, INT † (mandatory)}; the temp dir
  must be gone; the fake config dir byte-identical AND with identical modes and
  link targets †; every symlink in the mirror removed. A run-created file in the
  temp dir †: `rmdir` fails and the helper reports the leftover by name (names
  only) instead of deleting it.
- `../` detector †: a fake config with a `../`-escaping `style_guide_paths` value
  → that run is `error/config-relative-escape` with `valid=false:config`, the
  run log names `style_guide_paths`, the bench continues (return 0).
- Fake sentinel (`AKIA_FAKE_SENTINEL`) in the fake config never appears in stdout,
  the run log, or the CSV (the existing Fix 1 case, kept).

### 9.3 Block 2 — result labels
- A run that never reached the model gets `result=error` with a specific reason,
  never `invalid`. `invalid` is produced only by the existing `csv_row` rule
  (completed collapse-long pass/fail with 0 collapses, `:558-560`) — that rule
  already cannot fire on a run without a result JSON, because it keys off
  `pass|fail`. The labels to fix are the exit_reasons that today fall into
  `missing-workdir` (`:993-1000`) for a bootstrap failure:
  - On bootstrap error (grep for `^bootstrap error:` **in `$log_file.out`** † —
    the run's own stdout capture, not the combined `$log_file` which also holds
    the transcript — beside the sandbox-approval detection at `:967-979`):
    `result=error`, `exit_reason=bootstrap`, valid `false:error` (the `csv_row`
    error branch). Print the single `bootstrap error:` line to the terminal †
    **only when it matches the known-safe `style_guide_paths` shape** (verified
    against `config.go:2135-2142`: key name + resolved path; the line comes from
    `internal/wiring/headless.go:302`, which prints every bootstrap error
    verbatim, so an allow-list precedes printing) — other bootstrap lines are
    reported by name only, because other error shapes (endpoint URLs with
    userinfo, auth failures) could carry credentials.
  - Config error (the existing `config-unreadable`) stays `valid=false:config` —
    already correct (`:551-553`, `:565`) and covered by a Fix 1 self-test case.
  - Missing workdir stays `error/missing-workdir` (`:995-997`) — already a specific
    reason; today it is the label a bootstrap failure gets by accident, so the
    bootstrap detection must run BEFORE the workdir guard.
  - † Why the host row said `invalid`: a bootstrap failure writes no result JSON,
    but the run DOES proceed to `check.sh` (`:1033-1041`), which fails on the
    untouched fixture; `csv_row` then rewrites `fail` → `invalid`
    (`:558-560`). That is the path the host CSV took (0 s, `exit_reason=error`
    from the empty JSON field). The bootstrap detection therefore sits with the
    other pre-check guards (`:967-979`, `:993-1000`) and returns before
    `check.sh` runs — a run that never reached the model never reaches the check
    either.
- The bench stops on these (return 1, like the other guards) — unchanged.
**Tests.** A stub that emits no result JSON + writes
`bootstrap error: style_guide_paths["go"]: opening /tmp/…/styles/go.md: … no such
file or directory` to its output → row is `error/bootstrap`, the line is printed
to the terminal. The existing no-workdir stub (no result JSON, no bootstrap line)
→ `error/missing-workdir` (unchanged, existing case). A completed 0-collapse
collapse-long run → `invalid` (unchanged, existing case). Other tasks unchanged
(their rows come from the same code paths).

### 9.4 Block 3 — docs
`bench/README.md`: the mirror (what is linked, that it is everything except
the resolved config file itself, plus the shared-dir guard, cleanup guarantees
(only links + merged file removed, `rmdir`, never `rm -rf` on a dir with links),
the `../` limit + the stop rule, result labels
(`invalid` only for a completed 0-collapse collapse-long run; `error/bootstrap`,
`error/config-unreadable`, `error/missing-workdir` for pre-model exits), and the
note that a normal run writes nothing into the config dir (why read-only links are
safe). `CHANGELOG.md` [Unreleased] bench entry, extended.

### 9.5 Open risks
- A symlink in the user's config dir pointing outside it: `-L` entries are linked
  as-is; cleanup `rm -f` on a link never follows it. A config-dir entry that is a
  broken symlink: `ln -s` still succeeds; reading through it fails identically to a
  direct run.
- Concurrency: two bench runs against the same config dir create independent
  `mktemp -d` mirrors; the links are read-only, so no interference. mktemp uniqueness
  means no collision.
- The `../` detector guards one key (`style_guide_paths`); if Wakil ever makes
  another key config-dir-relative, the mirror still resolves it correctly (it is
  key-agnostic) — only the escape check would be missing, and the mis-resolution is
  visible as a wrong-path error, not a silent pass. Accepted and documented.
- `SIGKILL` of the bench shell still leaves the temp dir (as in Fix 1 §8.6); the
  mirror means that leftover contains only symlinks + a 0600 merged config. Same
  unrecoverable-by-trap class, unchanged.
- The XDG question is closed above (Wakil does not consult XDG).
- Wakil's own stderr is still not covered by the sentinel test (as in Fix 1). The
  `bootstrap error:` line is printed to the terminal only for the
  `style_guide_paths` shape (`config.go:2135-2142`); all other bootstrap errors
  are reported by name only because other shapes (`headless.go:302` prints every
  bootstrap error verbatim) could carry endpoint URLs with credentials.

### 9.6 Fix 1 — missing-file branch umask
`bench.sh:853` writes the "no user config" fallback without a `(umask 077 && …)`
guard. It holds no secrets today (two non-secret keys), but the 0600 invariant
should be uniform — fixed alongside the mirror in Block 1.
