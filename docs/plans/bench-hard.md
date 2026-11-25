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

## 10. collapse-long-v2 and debug-subtle fix (BASE 326def0, Tier 3)

### 10.0 What happened on the host (verified this session)

**collapse-long v1 never collapsed in real runs.** Three real Haiku runs
completed and passed the check, all correctly marked `invalid` (0 collapses):

| run log | result | in / out tokens | tool calls |
|---|---|---|---|
| `bench/results/collapse-long-1-003037.log` | pass | 394,955 / 3,774 | 6 |
| `bench/results/collapse-long-1-003123.log` | pass | 449,507 / 5,140 | 7 |
| `bench/results/collapse-long-1-003545.log` (CSV `20261125T003545-bench.csv`) | pass | 326,007 / 3,601 | 5 |

Per-tool-call sequence from the transcripts (result sizes are the transcript
labels — bytes **after** `ToolResultCap=8000` truncation, `internal/config/config.go:493`):

| request | tool call | result bytes | cumulative labelled bytes |
|---|---|---|---|
| 003037 #1 | `run_shell ls -la docs/` | 2.6 KB | 2.6 KB |
| | `read_file docs/note-01.txt` | 4.7 KB | 7.3 KB |
| | `search_files docs` | 6.0 KB | 13.3 KB |
| | `run_shell cat … \| sort \| uniq -c` | 2.9 KB | 16.2 KB |
| | `run_shell for f in note-*.txt …` | 4.3 KB | 20.5 KB |
| | `run_shell python3 … parse + write report.json` | 1.6 KB | 22.1 KB |
| 003123 #1 | `list_dir docs` | 1.1 KB | 1.1 KB |
| | `read_file_full docs/note-01.txt` | 8.3 KB | 9.4 KB |
| | `run_shell python3 … unique lines` | 3.8 KB | 13.2 KB |
| | `run_shell python3 … parse` | 3.2 KB | 16.4 KB |
| | `run_shell python3 … verify filler` | 103 B | 16.5 KB |
| | `run_shell python3 … verify` | ~0.2 KB | 16.7 KB |
| | `write_file report.json` | 79 B | 16.8 KB |
| 003545 #1 | `list_dir docs` | 1.1 KB | 1.1 KB |
| | `read_file docs/note-01.txt` | 8.3 KB | 9.4 KB |
| | `search_files docs` | 4.3 KB | 13.7 KB |
| | `search_files docs` (no matches) | 0 B | 13.7 KB |
| | `run_shell grep -v filler` | 2.5 KB | 16.2 KB |
| | `write_file report.json` | 79 B | 16.3 KB |

**Strategy (all three): scripted extraction, never ranged reads.** Each run:
one orientation read of `note-01.txt`, one exploratory grep/search, then a
Python/shell script that parses all 40 files in the shell and writes
`report.json`. The model's transcript says it plainly ("I'll parse them with a
script", "I used a shell filter to strip the repeated filler"). Total
conversation growth was ~16–22 KB labelled bytes ≈ 4–5.5K tokens — an order of
magnitude below the 45,000-token ceiling (180,000 chars, `scripts/bench.sh:867`).

**Why v1 didn't collapse (two compounding causes).**
1. **The fixture is grep/script friendly.** Numbers are in words but the
   sentence shapes are only 5 fixed phrasings (`bench/tasks/collapse-long/gen/main.go:60-66`);
   a script listing unique lines trivially recovers them. Our earlier replay
   (§8.7) assumed 40 sequential `read_file` calls — the real model never did that.
2. **Tool results are capped at `ToolResultCap=8000` chars** (`app.go:2464`
   `CapOrStub`). Even the one full `read_file_full` returned 8.3 KB ≈ the cap.
   The conversation can only grow by ~8 KB per read, so the ceiling needs
   ~180,000 chars ≈ 23 capped reads — and the model's scripted strategy used ~6 calls.

**debug-subtle rejects correct fixes.** `bench/tasks/debug-subtle/check.sh:68-70`
counts ALL changed lines vs the pristine tree and fails at >6. The reference fix
is 5 lines (4-line loop → 1). The host runs (`debug-subtle-1-005309.log` and 5
siblings) all show `check: ok (TestTakeCases TestHelperCases)` + `fix is not
minimal: 8 changed lines`. The original brief asked to count only lines OUTSIDE
`windowTake`; the implementation counts everything. A correct fix that rewrites
the loop plus a comment (12 lines) or clamps negative `k` inside the helper
fails on size, not on correctness.

**No secrets in the logs examined.** The three collapse-long logs and one
debug-subtle log above contain no keys, Authorization headers, or endpoint
URLs — only model prose, tool labels, and harness lines. (The run config
carries endpoints but is never printed; this session did not read it.)

### 10.1 Block 0 — numbers that drive the v2 design

- Ceiling counts **messages only**: `sentSize(contextProjection())`
  (`internal/agent/styleguides.go:430-432`; `TranscriptSize` = Σ `len(Content)` +
  Σ `len(ToolCall.Arguments)`, `internal/core/format/format.go:43-54`). The
  ~53 KB preamble is inside `Conv[0]` and therefore inside the count; the tool
  array is not. Same value for modules off/on (the ~416-byte on/off preamble
  delta from §8.2 is negligible at v2 scale — do not compensate).
- `ToolResultCap` default is **8000 chars** (`internal/config/config.go:493`;
  cap applied `app.go:2464` via `wtools.CapToolResult`). Long-session default
  is 200000 tokens; the bench override is 45000 tokens = 180,000 chars
  (`scripts/bench.sh:867,908,911`).
- Real per-call growth: one `read_file_full` of a whole 8KB fixture file =
  8.3 KB in Conv. One `search_files` = 4.3–6.0 KB. One shell script pass =
  1.6–4.3 KB.
- To collapse ≥2 times under a scripted strategy, a correct solution must
  pull **≥ 2× the ceiling in ceiling units** through the conversation, and the
  must-read text must exceed what any single tool result can return
  (8 KB cap / 40 files ⇒ the fact region must sit past the 8 KB cut in each file).

### 10.2 Block 1 — `collapse-long-v2` (additive; v1 untouched)

New task dir `bench/tasks/collapse-long-v2/` with the same layout as v1
(`task.txt`, `check.sh`, `repo/`, `solution/`, `setup.sh`, `gen/`).
`collapse-long` (v1) files are **not modified** — old CSV rows stay comparable.
v2 is a new task name, discovered by the existing glob (`scripts/bench.sh:222-228`);
self-test noop/solve loops cover it generically.

**Design goal (revised after Mashūra round 1):** make the semantic-judgment
text itself several times the ceiling, so even a best-effort script filter's
output exceeds the ceiling. Two sub-goals from the review, stated plainly:
(a) commit to **many facts per file** with dense decoys — filler must contain
number words, figures for other entities, superseded/conditional statements —
so a script that prints every candidate line across 40 files exceeds 180K
chars; (b) so every reasonable strategy (scripted or in-turn) crosses the
ceiling at least once. The `invalid` rule (completed 0-collapse run) extends
to v2 by task name: `scripts/bench.sh:581` reads `task = "collapse-long"`;
add `|| task = "collapse-long-v2"` (the §4 single computation point in
`csv_row`, no other validity change). **Validity stays ≥1 collapse** — the
"≥2" wording in this plan and in earlier drafts is corrected: ≥1 is what
`csv_row` implements and what the self-test asserts; the design targets
repeated crossings but the rule is ≥1 (Mashūra non-blocking items folded).

**Fixture (seed-generated, generator + seed outside the workdir, as v1).**
`gen/main.go` (new, seed fixed in code, own seed value distinct from v1's 42):
- 40 files `docs/note-XX.txt`, each **~60 KB** (7.5× v1's 8 KB) of natural-language
  prose, each stating **~12 facts** (one per entity) in words — dense facts,
  not 1 fact per filler desert:
  - numbers written in words with varied names/aliases; entities referred to by
    2–3 aliases each, assigned per file;
  - each file states its ~12 facts at a seeded random offset (e.g. a 400-line
    "ledger block" at 20–45 KB into the file, past the 8,000-char
    ToolResultCap cut), so `cat`/head/`read_file` whole-file reads are
    truncated before the facts and the agent must read in offset/limit ranges;
  - **dense decoys in filler**: filler sentences contain number words, figures
    about other (non-totalled) entities, "approximately" hedges, and
    conditional statements — so a number-word filter that prints every
    candidate line outputs **> 180K chars** across the corpus;
  - some files carry a **correction**: an early figure, then a later sentence
    that supersedes it ("the entry above is withdrawn; the corrected figure
    is …"), so a script that grabs the first (or any unverified) figure gets
    it wrong — the check needs the final figure per entity.
- Canvas: 40 files × ~60 KB ≈ 2.4 MB on disk. The correct-solution must-read
  text is the 40 ledger blocks ≈ 40 × ~20 KB ≈ 800 KB ≈ **4× the 180,000-char
  ceiling** (≥ 2× the ceiling in ceiling units, as the brief requires —
  measured in ceiling units, not disk bytes).
- **best-effort script-only solver must fail** (Mashūra B4): before the commit,
  the author runs a best-effort python script (dedupe + number-word filter +
  last-wins over corrections) against the generated fixture. The fixture is
  rejected unless that script produces a **wrong** `report.json`. This test is
  a pre-commit gate, recorded in this plan's final form; the self-test's
  `stub_greponly-v2` is a regression lock on the same property (it must stay
  `fail`), not the proof of the wall.
- **Timeout/cost envelope** (Mashūra B2): a ranged-read strategy reads
  ~800 KB at ≤8 KB per `read_file` ≈ **100 tool calls**. At that scale the
  default 600 s timeout is tight for a 1M-token model with per-call latency.
  The bench is run via `scripts/bench.sh --task collapse-long-v2 …` and the
  user may pass `--timeout 1800` — the README states the recommended timeout
  and the ~100-call estimate. (A smaller fixture was considered and rejected:
  ~800 KB is the honest 4×-ceiling size; a sub-ceiling fixture would be a
  v1-class false pass.)
- **Determinism** (Mashūra B5): the generator must not range over a Go map in
  a way that varies output (slices only, or sorted keys); the author runs
  `setup.sh` twice into two dirs and `diff -r`s them (byte-identical) before
  the commit. The emitted answer key is written to `gen/` (outside the
  workdir) and the self-test asserts it equals the `check.sh` constants
  (generator↔check tie, so a generator bug cannot leave the task unsolvable
  with a green self-test).
- Report requirement: `report.json` with per-category sums, where the check
  needs facts from **early AND late files** — so a run that collapses early
  must retain early facts. **Falsifier note** (Mashūra B3): a run that collapses
  by exploring then scripts, or that externalises a running tally to a scratch
  file, can `pass` validity without retaining anything. The task measures
  "crossed the ceiling with a correct report", not "recalled from the archive"
  — stated in `task.txt` and README as a known limit, not hidden. The dense-
  decoy wall makes the "explore then script" path produce a wrong report, so
  the realistic bypasses are narrow, but the check cannot rule them out
  structurally. `check.sh` checks only `report.json` (as v1); it does not
  inspect the transcript for scratch-file writes.

**task.txt** describes only the goal (read every file, sum by category,
corrections win, don't modify docs/) — no mention of the offset, the cap, or
the phrasing variety.

**check.sh**: parsed-JSON comparison like v1 (expected sums baked in, python3,
exit codes 0/1 via `gocheck_fail`/99 via `gocheck_error`), run through
`gocheck_init`. Expected sums live only in `check.sh` + `solution/report.json`,
never in `repo/`.

**setup.sh**: same shape as v1 (`go run -C "$here/gen" . "$PWD/docs"`),
generator + seed outside `repo/`, runs before the workdir's first commit;
`leak_check` + post-run leak guard cover `gen/` generically.

**10.2.1 Ceiling (chosen from Block 0 measurements, not a replay).**
The real Haiku strategy pulled ~22 KB (003037) in 6 calls and ~17 KB (003123)
in 7, ~16 KB (003545) in 5. The cap is 8 KB per tool result. A ceiling of
**45,000 tokens (180,000 chars)** — the SAME value as v1, settings identical
for modules off/on — needs ≥ 23 capped reads to fire once, and the v2
must-read text (~800 KB, 4× the ceiling) crosses it repeatedly. A scripted
grep strategy must fail the **check** (dense decoys + corrections + aliases),
not merely avoid the collapse by avoiding reads — that is the acceptance
test in the fixture paragraph above. `ToolResultCap` and `ContextMode`
(`long-session`) are **pinned in the same merged config** (Mashūra B6):
`tool_result_cap: 8000`, `context_mode: "long-session"` — user values that
change the task are overridden, and the run log records all four numbers
(ceiling, min_bytes, tool_result_cap, context_mode). `min_bytes` stays 2000
(one-turn run; neutralises a user value, as v1). The bench override line gains
`collapse-long-v2` next to `collapse-long` (`scripts/bench.sh:851`), same
values, same merged-config machinery.

Rejected alternatives: a lower ceiling was considered and rejected — it would
risk firing during the preamble/task for small files and would make the two
tasks' CSV rows non-comparable in scale. Same ceiling keeps v1 and v2
commensurable as "crossed the same ceiling N times".

**Self-test (fake backend).**
- `stub_solve` for `collapse-long-v2` (today: `collapses:3` when workdir ends
  `/collapse-long` — extend the match to `*-v2` or use a `-v2`-aware case) →
  `pass/valid`; `stub_noop` → `invalid/false:no_collapse` (the §4 computation
  point now includes v2).
- `stub_wrongreport` (one wrong sum) → `fail`.
- `stub_greponly-v2`: runs the author-written best-effort filter against the
  fixture → must `fail`. **Regression lock only** — the wall proof is the
  pre-commit best-effort script test in the fixture paragraph (Mashūra B4).
- **Generator↔check tie** (Mashūra B5): the generator writes its answer key
  beside `gen/`; the self-test asserts the key equals the `check.sh`
  constants. Two-generation `diff -r` is a pre-commit gate (see above).
- A **ranged-read simulation**: a stub emitting synthetic tool results that
  cross the ceiling (e.g. a result-json with `collapses:2` + a correct report
  produced after ranged reads) → `pass/valid`, proving the run is scorable
  when the strategy earns ≥1 collapse. (The stub is a harness simulation, not
  a model; the real-run calibration is the host's job — stated in README.)

**Tests per block:** B1 passes when `scripts/bench.sh --self-test` is green
including the three new cases, and a direct `check.sh` run on `repo/` fails
while `repo/`+`solution/` passes in a temp dir.

### 10.3 Block 2 — debug-subtle: a fair minimality rule

`bench/tasks/debug-subtle/check.sh` today counts every changed line across the
whole tree and fails at >6 (`:68-70`). New rule (hidden tests untouched):

- Count changed lines (gofmt-normalized, hidden tests removed first, exactly as
  today) **only outside the extended `windowTake` window** — the window runs
  from the `// windowTake returns…` doc-comment line through the matching
  closing brace (the `^func windowTake` line plus its contiguous comment
  block above), so a doc-comment touch on the function is inside, not
  outside. Lines inside the window are unlimited in count (rewritten loop,
  comments, clamped `k`).
- Outside the window: at most **2 changed lines**. Why 2 (revised after
  Mashūra): the reference fix touches 0 outside lines. The plausible correct
  touches outside are (a) ONE one-line edit (e.g. fixing the `Take` doc
  comment or a one-line `Take` guard), costing `-1 +1 = 2` by the existing
  added+removed counting, or (b) an **import addition** (a new stdlib import
  costs more than 1 line after gofmt normalization — see below). They are
  alternatives, not cumulative: 2 exactly covers one one-line edit, and the
  import case is pre-empted by the wider window + the deleted+added counting
  below. An import-touching fix needs the file's current import block: if
  `window.go` has no import, adding one is `import "x"` (1 added line, no
  blank line is added by gofmt for a single-line import); if it later gains
  a block, the check normalizes both sides identically. The author verifies
  the actual diff for the import case at implementation time.
- **Overall safety cap: at most 30 changed lines in total** (inside +
  outside), to stop a whole-file rewrite, which would otherwise be unlimited
  inside the window and could smuggle in the hidden tests' answers (hardcode
  boundary inputs in ~1000 lines). 30 is sized from the real function: the
  function is 13 lines; a clean rewrite with comments lands at ~20–25
  changed lines (removed+added). 30 rejects anything package-wide.
- **Counting detail** (Mashūra B9): removed lines are classified by their
  OLD-file line number (inside iff inside the pristine window range); added
  lines are classified by their NEW-file line number (inside iff inside the
  agent-tree window range). A hunk spanning the boundary is split per-line.
  Pure insertions are classified by the new-file position. New/deleted files
  (other than the agent's own `*_test.go`, which are excluded from the diff
  entirely — see next bullet) count all their lines as outside. Boundary
  self-tests cover: insertion directly after the closing brace (outside),
  insertion of a comment above `func windowTake` (inside window).
- **Agent-written tests are excluded from the diff.** The diff runs on the
  test-stripped copy (`gocheck_prepare` deletes every `*_test.go`; the check
  deletes the hidden tests it copies in). The agent's visible-test edits
  never count toward the minimality rule — a correct fix plus a regression
  test is not punished (Mashūra B8).

**Implementation in check.sh:** after the existing gofmt-normalization, extract
the `windowTake` line range from `window/window.go` on both trees (awk from
`^func windowTake` to the matching `^}`), count:
- `outside` = changed lines whose old-file coordinate falls outside that range
  (for new/deleted files, all lines count as outside — a new helper file is
  outside, correctly);
- `total` = all changed lines (existing count).
Fail with distinct messages: `fix touches N lines outside windowTake (at most 2
allowed)` / `fix is not minimal: N changed lines (at most 30 allowed)`.
Deterministic across gofmt versions because both trees are gofmt-normalized
before diffing (the §1.3/§3.1c argument, unchanged).

**Self-test cases** (all through the existing `run_one` + stub machinery):
1. 5-line reference fix (existing `stub_solve` for debug-subtle) → `pass`.
2. Correct longer fix inside `windowTake` (12 changed lines: rewritten loop +
   a comment + clamped `k`) → `pass`. New stub `stub_longfix`.
3. Correct fix + 5 unrelated lines changed outside `windowTake` → `fail`. New
   stub `stub_outside`.
4. Whole-file rewrite (>30 lines: rewrite `window.go` with the fix plus
   re-structured functions/comments) → `fail`. New stub `stub_rewrite`.
5. Existing wrong-solution case (bloated `ExtraHelper` outside the function, 12
   lines) → still `fail` (the existing `stub_bloated`).
6. **Replay of the 6 host diffs** (Mashūra B7): the author replays each of the
   6 host `debug-subtle` logs (3 off, 3 on — all `8 changed lines` under the
   old rule) by applying the logged agent fix to the pristine tree and running
   the NEW check against it. If any replayed correct fix still fails (extra
   lines outside the window), the window/budget is widened before the commit.
   The replay result is recorded in the final report.
7. Boundary cases (Mashūra B9): insertion immediately after the closing brace
   → `fail` (outside); contiguous comment added above `func windowTake` →
   `pass` (inside). New stubs or direct check.sh invocations in the self-test.

**Comparability note** in `bench/README.md`: debug-subtle rows before this
commit used the old rule (all changed lines ≤ 6) and are **not comparable**
with rows after it. Name the commit (`git log` at commit time).

### 10.4 Block 3 — docs

- `bench/README.md`: collapsed-long-v2 section (what it is, why v1 didn't
  collapse — the measured table from §10.0, scripted strategy, 8 KB cap,
  chosen ceiling 45,000 tokens with the numbers, ≥2 collapses requirement),
  debug-subtle minimality rule + non-comparability note (§10.3), blockers.
- `CHANGELOG.md` [Unreleased]: one line per user-visible change.

### 10.5 Open risks

1. **Scripted-around collapse again.** If a model writes the fixture to disk
   externally, reads it in pieces under the cap, and never lets 180K chars
   accumulate in Conv, v2 also goes `invalid`. That is the designed outcome
   (invalid, first-class), not a pass. Mitigation: the 1.6 MB must-read text
   makes accumulating < 180K nearly impossible for any reasonable read loop —
   but a per-file process-and-forget loop defeats it structurally. Accepted
   as a known limit, stated in README (same class as v1's scripting limit).
2. **Phrasing variety is finite.** With 6+ phrasings the grep-only stub fails,
   but a clever model that reads 1–2 files fully and infers the pattern can
   still script. The offsets + corrections are the teeth; the phrasings are
   friction, not the wall. The grep-only simulation in the self-test proves the
   wall, not the friction.
3. **Ceiling vs preamble symmetry.** 180K chars vs ~53 KB preamble leaves
   ~127 KB of growth before firing. A ranged-read strategy reading 8 KB at a
   time needs ~16 reads to fire once — the v2 fixture (40 files × ~40 KB of
   ranged reads each) guarantees that when the agent actually reads. OK.
4. **debug-subtle window extraction.** Parsing `func windowTake` by `^func`…
   `^}` is brittle if the agent moves the function or renames it — a rename
   is a correct behaviour change but fails extraction, so every line counts as
   outside and the fix fails. This matches the task text ("inside the buggy
   function") — the task says where the fix belongs. Accepted; the failure
   message names the function.
5. **No real-model validation this session** (DO NOT ALTER: no real model
   requests except Mashūra). The ceiling and the grep-only wall are proven by
   measurement + self-test simulations; the host's real Haiku runs are the
   calibration. Stated as unverified in the final report.

### 10.6 Touched paths

- `bench/tasks/collapse-long-v2/` (new: task.txt, check.sh, setup.sh,
  gen/main.go, repo/{go.mod}, solution/report.json)
- `bench/tasks/debug-subtle/check.sh`
- `scripts/bench.sh` (override branch + validity line + stub match + self-test stubs)
- `bench/README.md`, `CHANGELOG.md`, this plan (`docs/plans/bench-hard.md`)
- Wakil itself is **not** touched: `git diff 6d1a86a..HEAD -- internal cmd prompts`
  stays empty.

## 11. collapse-long-v2 ceiling from a real run (2026-11-25, from 7369737)

### 11.0 What happened (host run, modules off)

`scripts/bench.sh --task collapse-long-v2 --runs 1 --model
anthropic/claude-haiku-5.5 --context-modules off --timeout 1800 --mashura
off`: 99 s, 1,177,802 in / 18,679 out, 89% cached, $0.0356, 18 tool
calls, `collapses=0`, `collapses_turn_end=1`, the check PASSED, so the
run was marked invalid — `valid=false:no_collapse`. Log:
`bench/results/collapse-long-v2-1-013001.log`, CSV
`bench/results/20261125T013001-bench.csv`.

The model solved the task by moving the work into the sandbox (Python
scripts in `/tmp`, iterated hypotheses about correction targeting) instead
of the conversation: only three in-conversation results exceeded 3.4 KB
(one `read_file` 8.3 KB, one `sed` dump 5.9 KB, one `grep` aggregation
8.3 KB), everything else was small probe output. The conversation never
reached the 45,000-token (180,000-char) ceiling. Making the content harder
is an arms race (bigger fixture ⇒ more `grep`/`sed` offload, shorter
in-conversation tail); the decision is to calibrate the ceiling from this
real run instead.

No secret-bearing content is copied into this plan or any report below:
the run log carries no keys here — only tool sizes and agent text lines
that identify paths and sums the report itself printed. Where a transcript
line may contain secret-shaped fragments it is not reproduced.

### 11.1 Verified findings: what the real run's trace holds

- The run log is JSONL; its `"type":"output"` lines are transcript
  lines. Tool-result sizes appear as `· <tool> <args>… → <n> lines ·
  <bytes>` — byte sizes plus a line count, **not** full request contents.
  There is no trace envelope holding the full request bodies in the repo
  for this run (the log is a transcript, not a request trace). The spill
  file the collapsed-turn line points at is host-side
  (`~/.local/share/wakil/toolcache/...`); this session must not read the
  user's real toolcache or config, so the replay simulates from recorded
  sizes, and says so. The run log header itself records the effective
  run profile (52,879-byte prompt from the mirrored `agent.txt`, 1M-token
  `n_ctx` / 991,808 usable), which is the precondition evidence the
  calibration rests on (§11.3): the two 8,499-B results fit an 8,000
  cap plus spill suffix, and the turn-end collapse implies
  `long-session` mode — so defaults, **without** reading the user's
  config. The log does not record the two collapse-key values or
  `tool_result_cap`, so a run-time check that the host's effective
  values match defaults is not possible; README states the calibration
  assumes defaults, with no run-time assertion.
- The log's last line `· collapsed request 1: 12108 tokens → 1268 sent
  (msgs 2–37, run_shell ×14, git_status ×1, list_dir ×1, read_file ×1, …
  (+1 more tools)) → <archive>` is the **turn-end** collapse note
  (`collapses_turn_end=1`), not the mid-turn trigger: `CollapseCounts`
  (`internal/agent/collapse.go:2312-2325`) counts `Partial` records as
  mid-turn and non-`Partial` as turn-end, and `writeResultJSON`
  (`internal/wiring/resultjson.go:130-132`) writes the two separately.
  The number 12,108 is `wSize / charsPerToken` (`formatCollapseNotice`,
  `collapse.go:1449-1453`), where `wSize = Σ messageSize(Conv[i])` over
  the turn's middle messages `i = s.first+1 .. s.last-1`
  (`collapse.go:1191-1194`) — `messageSize` is `len(Content) +
  len(ToolCall.Arguments)` (`collapse.go:2070-2079`). So the true
  working set `W` (all 18 tool rounds: 36 messages, msgs 2–37) is
  48,432–48,443 chars. That exceeds the result+text sum (§11.2) by
  ~12.4 KB — that gap **is** the argument bytes (§11.2). At the old
  ceiling the turn-end writer fired because the turn's working set
  (12,108 tokens) exceeded `min_bytes=2000`, after the last request;
  `collapses` (mid-turn) is what validity gates on (`bench.sh:581`:
  pass/fail with 0 collapses ⇒ invalid). The bench therefore
  calibrates the mid-turn ceiling (`midTurnCollapseCeiling`,
  `internal/agent/collapse.go:1866-1871`: ceiling chars =
  tokens × `charsPerToken` 4, `internal/agent/compact.go:15-18`).
- The ceiling counts messages only: `sentSize(contextProjection())`
  (`internal/agent/styleguides.go:430`) = Σ `len(Content)` + Σ
  `len(ToolCall.Arguments)` (`internal/core/format/format.go:43-54`),
  system message (Conv[0] preamble) included, task message included, tool
  schemas excluded (`internal/agent/collapse.go` per §8.2). The trigger
  fires between tool calls inside `streamTurn`
  (`internal/agent/turn_phases.go:375-376`) when the projection exceeds
  the ceiling (or `hardMax` = 0.95 × effective context — 1M-token model,
  991,808 usable per the log header, so `hardMax` ≈ 750k chars and never
  fires first on this run); the kept tail is ≤30% of the ceiling
  (`collapseTailStart`, `internal/agent/collapse.go:1500-1569`), with at
  least the most recent round always kept.
- The prompt is loaded from `/tmp/.../agent.txt` at 52,879 bytes (log:
  `agent prompt: loaded 52879 bytes`), matching `prompts/agent.txt`
  (52,879 bytes on disk) — the full prompt, not the 18,573-byte core
  mode (`prompts/core.txt`).

### 11.2 Block 0 — measurement (per-request conversation size)

Method: reconstruct Conv growth from the transcript's recorded result
sizes **plus** the known total working set `W` (§11.1). Two independent
sums must agree, and they do only when argument bytes are included:

- Tool-result bytes (transcript `· … → N lines · B`): 32,523.
- Assistant text bytes on tool rounds (the 16 streamed text lines
  before tools 1–16; the final-answer lines belong to the turn's
  answer message `A`, which is outside `W`): 3,322.
- **Argument bytes: 12,587–12,590** (unknown distribution, anchored to
  the collapse notice's exact total: `W` = 48,432–48,435 ⇒ arguments =
  `W` − 32,523 − 3,322). There is no "~24 B/call envelope" anywhere in
  the path — `messageSize` counts content + arguments only.
- The checkpoint is not derived from result content: `buildCollapseNote`
  (`collapse.go:284-361`) uses tool names, counts, and paths extracted
  from **arguments** (`extractPaths`, `collapse.go:463-486` — path/file/
  src/dst/paths fields only), and `buildFindings`
  (`collapse_findings.go:63-122`) additionally reads assistant text and
  result ok-flags. Filler result bodies therefore cannot underweight the
  checkpoint through result content — the remaining variance is findings
  text length, bounded by the 1,500-B `maxFindingsBytes` cap. The replay
  prints the actual checkpoint size per collapse; a ceiling within ~1.5
  KB of a count boundary is treated as unstable and rejected.

Per-request sizes are `sentSize(contextProjection())` in chars
(= preamble + task 751 B + cumulative `W`). The per-call argument
column below is the **replay input table** (reviewer R2 fix); it sums
to 12,588, in the anchored range. Its basis: call 10 (`cat >
/tmp/parse_ledger.py` heredoc, shown in the transcript) is the
parse script, and call 11 (`cat > /tmp/explore.py`) the second script —
the transcript's truncated arguments are ≥50 chars each but cannot
give exact bytes. The distribution is therefore estimated, **not**
trusted: the replay runs it under three named distributions and the
chosen ceiling must be stable across all of them (§11.3).

**Distribution A — heredoc-concentrated** (base input):

| # | tool | result B | text B | arg B | projection before request #+1 (chars) |
|---|------|---------:|-------:|------:|--------------------------------------:|
| 1 | list_dir | 1,126 | 104 | 42 | 55,107 |
| 2 | search_files | 247 | 132 | 44 | 55,530 |
| 3 | read_file | 8,499 | 159 | 45 | 64,233 |
| 4 | run_shell | 150 | 156 | 40 | 64,579 |
| 5 | run_shell | 8,499 | 190 | 42 | 73,310 |
| 6 | run_shell | 1,638 | 297 | 46 | 75,291 |
| 7 | run_shell | 6,042 | 201 | 44 | 81,578 |
| 8 | run_shell | 163 | 250 | 46 | 82,037 |
| 9 | run_shell | 333 | 214 | 50 | 82,634 |
| 10 | run_shell (heredoc `parse_ledger.py`) | ~20 ("parsed 640 unparsed 0") | 85 | 11,784 | 94,523 |
| 11 | run_shell (heredoc `explore.py`) | 41 | 250 | 50 | 94,864 |
| 12–17 | run_shell ×6 (workdir) | 3,379+911+226+149+681+344 | 201+303+277+367+136+0 | 55 each | 98,499 → 102,168 |
| 18 | git_status | 75 | 0 (answer text) | 25 | 102,268 (final peak) |

All three distributions hit the same peak (102,268 chars ≈ 25,570
tokens) because the peak is fixed by `W`; they differ only in
when the crossing happens (first request ≥80,000 chars: req 8 in all
three; first request ≥92,000 chars: req 11 / 13 / 15 across the three
distributions on the trial splits below). The replay verifies the
chosen ceiling's count under each, and the selection fails if the
count moves between distributions (§11.3).

Whether the recorded sizes are pre- or post-`tool_result_cap` is
resolved by `app.go:3967-3998` + `turn_phases.go:587-628`:
`toolLine(tc, result)` — and therefore the transcript's byte labels —
is built from `result.text` **after** `CapOrStub` (the cap is applied
at `turn_phases.go:628`, the line at `:587/:599`). The 8,499 B labels
are already post-cap text (kept head + spill suffix sized to the
8,000-byte cap, `internal/tools/toolcap.go:464-483`), so the
transcript's bytes are the final in-Conv sizes. The replay therefore
sets its test `ToolResultCap` ≥ 8,499 (still the default shape — every
injected body ≤ 8,499 B, so `CapOrStub` is a no-op) instead of
chaining a second cap over the recorded bodies.

(The earlier per-call table is superseded by the input table above —
kept here only for the narrative; delete on commit. It contained no
argument column and ended at 89,950 chars = results + text only,
eleven thousand short of the anchored `W`.)

After tool 18, one final content response (request 19): the report text
(~1.2 KB) plus `report.json` content. The deepest projection is at
request 19 — **102,267–102,270 chars ≈ 25,570 tokens** (preamble
53,084 + task 751 + `W` 48,432–48,435), anchored to the exact total
from the collapse notice. Three results are large (8,499 / 8,499 /
6,042 B); the rest are ≤3.4 KB. The 45,000-token ceiling (180,000
chars) is ~1.76× this peak. The turn-end collapse that *did* fire
(12,108 tokens at the end) shows even the small working set was
collapsed — the mid-turn ceiling was never crossed.

**Strategy named:** sandbox-offloaded parse — the model wrote whole
Python parsers to `/tmp` inside the sandbox, probed hypotheses about
correction targeting with small scripts, and got only bounded output
back. The ledger bytes (40 × ~43 KB) never entered Conv at all; only
query outputs did. This is the correct general workaround for the
conversation growth the task intends to force, and it is why
verification cannot depend on content volume: the offload *reduces*
in-conversation bytes but the conversation still accumulates one round
per probe, so a ceiling in the low-20s of thousands of tokens still
fires on this strategy.

### 11.3 Block 1 — calibration (design)

Replay the recorded sequence through a throwaway Go test (package
`agent`, deleted before commit — same discipline as Fix 1's §8.7), driving
the real `streamTurn` with the scripted fake backend (`sseServer` +
`toolCallFrames`, `internal/agent/testhelpers_test.go:221-269`) and the
real `collapseMidTurnPrefix` path — with **final in-Conv sizes** (§11.2):
the fake executor returns result bodies of exactly the recorded byte
sizes (filler text of that length), and arguments carry the §11.2
input table's byte sizes (passed as the `toolCallFrames` argument
payloads of the matching total length). The sequence, batching (one
tool call per request, matching the real trace), and final content
answer are reproduced as recorded. The real tool **names** are
preserved on the fake calls (`read_file` specially affects
`buildFindings` — §11.2 checkpoint note).

Modes: `ContextModules=off` and `ContextModules=on`, same sequence,
each mode's **own measured preamble** (`buildPreamble` per mode —
written as 53,084 / 53,500 in §8.7 but re-measured in the test, since
Δ416 B flips a count near a boundary). `tool_result_cap` and
`context_mode` are **not** pinned today (a false claim in §10.2.1 /
`bench/README.md:402-404` — the merge sets only the two collapse keys,
`bench.sh:867`, grep-verified), so the throwaway test runs at defaults
and the resulting ceiling is valid **only on that basis**; the host's
effective values are recorded as an explicit precondition, and pinning
is assessed in §11.5. `style_guide_paths` was not active on the real
run (no `style` line in the run log; style selection only adds bytes
when a guide is enabled and a matching tool path was observed —
`styleguides.go:338-431`) — the precondition records that too.

Candidates (tokens → chars): 20,000 (80,000), 21,000 (84,000), 22,000
(88,000), 23,000 (92,000), 24,000 (96,000), 25,000 (100,000), 26,000
(104,000), 28,000 (112,000), 32,000 (128,000). Rationale: peak ≈ 25.6k
tokens, so values just below it; **below ~20k the calibration thrashes**
— post-collapse size is `F + tailBytes` with
`F ≈ 53.8K (preamble+task) + checkpoint (note capped at 1,200,
`noteCapBytes` `collapse.go:431`, + findings capped at 1,500,
`maxFindingsBytes` `collapse_findings.go:18`) ≈ 55–57K`, and the tail can
fill `tailCap = 30% of ceiling`, so re-fire is near-guaranteed while
`0.7·C ≲ F + checkpoint`, i.e. below ~19.7–20.3k tokens (§8.7's 38–39
collapses at 32K/64K chars are exactly that regime). The usable window
is ~20.3k–25.6k; candidates 17–19k are excluded for that reason.
The test also prints `hardMax` (`activeThresholds`) and asserts
`hardMax > ceiling` for every candidate, with `ContextLimit` set to the
real 1M-token profile from the log header (991,808 usable), so
pruning/hard-max cannot silently take over.

Selection rule (from the task, hardened by reviewer R2): the ceiling
where this real run would collapse **at least 1 and at most 3 times in
BOTH modes**, same value in both modes, preferring 2 — **and** whose
count stays in 1–3 in both modes under all three argument
distributions (§11.2: heredoc-concentrated / even-spread /
late-heavy), with **no count boundary within ~1.5 KB** of the chosen
ceiling's trajectory (checkpoint-size variance). The chosen candidate
is the most stable passing one (preferring 2; if two pass, the one
further from a boundary under all three distributions). If no
candidate passes, that is reported and no ceiling is committed.
Applies to collapse-long-v2 only; collapse-long v1 stays unchanged.
The reassurance that the `hardMax` assert (`§11.3`) and the per-mode
preamble printing are part of the table the replay prints, so the
chosen number is traceable. The self-test gains a **v2 recording
stub asserting the new literal and a v1 recording run asserting 45000**
(the existing stub only runs `collapse-long`, so after the split nothing
would assert the v2 branch — reviewer B7). The assertions compare
against **hardcoded literals**, not the `_collapse_ceiling` variable
(a variable-vs-variable comparison is tautological). Otherwise the
stub machinery (pass/valid, fail, invalid via noop, grep-only) is
size-independent and unchanged.

**On-mode caveat (BLOCKING for "both modes" claims).** This trace never
called `load_module` — but every tool in this trace (`list_dir`,
`search_files`, `read_file`, `run_shell`, `git_status`) is in
`coreToolNames` (`internal/agent/modules.go:41-86`), so no tool here
requires `load_module` at all; a real on-mode run of this strategy
loads **zero** modules and `preservedSchemaMessages` has nothing to
pin. A strategy that *does* load a module (e.g. an LSP-heavy one)
raises `F` permanently after the first load, so the on-mode count for
**it** can exceed the replay's. The "1–3 in both modes" claim is
therefore a claim about **this recorded strategy**, on-mode included
(preamble swap only, zero module loads — true for this trace's tool
set). Do-not-alter forbids new real-model runs, so a host on-mode run
is recorded as unverified, not assumed (§11.4).

### 11.4 Risks and what could make the calibration wrong

- **Over-cap results (8,499 B ×2): resolved.** Transcript byte labels
  are built after `CapOrStub` (`turn_phases.go:587-628`,
  `app.go:3967`), so 8,499 B is the final post-cap text. The replay
  injects final in-Conv sizes — the recorded 8,499 B **is** the in-Conv
  size — and bypasses the cap for those two calls. Chaining a second
  `CapOrStub` over them would shrink them by ~500 B each.
- **Argument bytes (12,587–12,590 B total, distribution estimated).**
  The "lower bound is safe" argument proves only ≥1 collapse, never ≤3:
  larger real arguments cross earlier and can produce **more**
  collapses, and shift which rounds the 30% tail keeps. This is why
  §11.2 anchors to the exact total (`W` = 48,432–48,435 from the
  collapse notice) instead of treating the peak as a lower bound. The
  per-call distribution is estimated — the residual risk is a ±1 count
  at a boundary, mitigated by the three-distribution stability rule
  (§11.3): a candidate passes only if its count stays in 1–3 in both
  modes under all three distributions, and the chosen one is the most
  stable. If no candidate passes, that is reported as the result and
  no ceiling is committed.
- **Post-collapse tail dynamics.** After a mid-turn collapse the
  projection keeps preamble + task + one checkpoint (note capped at
  1,200 + findings capped at 1,500) + a ≤30%-of-ceiling tail. Whether a
  second collapse fires depends on that post-collapse size plus
  remaining growth, so the replay exercises the real
  `collapseMidTurnPrefix` and **reports the post-collapse
  `sentSize(contextProjection())` at each collapse** — a 1–2 KB
  checkpoint mis-estimate at a 68–88 KB ceiling would otherwise silently
  flip the count. The `boundaryIdx <= taskIdx+1` early-return (a
  collapse needs at least two completed rounds before it can fire) is
  likewise live in the replay.
- **Modules on/off difference.** The Δ416 B preamble is re-measured
  per mode in the replay. A module-loaded round would pin
  `load_module` schemas through every later collapse
  (`preservedSchemaMessages`, `collapse.go:793-840`), raising `F`
  permanently — but no tool in this trace is module-deferred (§11.3),
  so zero loads is the honest on-mode replay of this strategy. The
  host on-mode run is unverified (DO NOT ALTER forbids new real-model
  runs).
- **Result JSONL ≠ Conv.** The recorded "B" figures are transcript
  display sizes. They are equal to in-Conv sizes only because
  `toolLine` prints `result.text` post-cap and `TranscriptSize` counts
  the same string; per-message structural overhead is counted in
  neither. The one known constant difference: `sentSize` adds
  `styleSelection().Bytes()` (zero here — no style guide active on
  this run). The reconstruction treats displayed bytes as in-Conv
  bytes; the exact `W` cross-check (§11.2) constrains the total, so
  any residual per-message constant can only shift which call crosses.
- **Preamble is host-dependent.** Conv[0] is >50% of any candidate
  ceiling. The log shows the prompt loaded from the mirrored
  `/tmp/.../agent.txt` — the user's own file, byte-equal to
  `prompts/agent.txt` today. A prompt/`agent.txt` change on the host
  invalidates the chosen number; README states this.
- **One run, one model (Haiku).** Only n=1, modules off; modules-on is
  a preamble-swap replay, not a real run. The chosen ceiling is that
  run's property; the `invalid` safety net stays for other strategies
  that stay below it (stated in `bench/README.md`). Run-to-run variance
  exceeds the ~4.5k-token window, so expect `invalid` rows to continue
  on different strategies/models — the calibration makes *this*
  strategy collapse, not every strategy.
- **Merged-config pinning.** `tool_result_cap` / `context_mode` are
  not pinned by the merge today (§11.3); §11.5 only parameterizes the
  ceiling per task and does **not** add pins, so a non-default user
  `tool_result_cap`, `context_mode` or `context_prune` still invalidates
  the calibration. Recorded as a precondition on the host run, and the
  §10.2.1 / README pin claim is corrected in Block 2 rather than
  silently re-verified.
- **Turn-end vs mid-turn.** Only mid-turn collapses (`collapses`) gate
  validity. A ceiling that only triggers the turn-end collapse leaves
  the run invalid, so the replay counts `midTurnCollapses` only.

### 11.5 Touched paths (planned)

- `scripts/bench.sh` — parameterized per task (one `_collapse_ceiling`
  variable, set from the task name before the branch, so the
  `mktemp -d` / `umask 077` / `jq 2>/dev/null` / `../` escape /
  `bench_cfg_cleanup` guards cannot drift): `collapse-long-v2` gets the
  new ceiling, `collapse-long` keeps 45000. The four literal sites
  (`jq` merge, no-user-config printf, log line, self-test `jq`
  assertion) all read the shared variable. The stale `3 collapses in
  both modes (plan §8.7)` comments are updated to point at this
  section's table.
- `bench/README.md` — measurement table, calibration table, chosen
  ceiling + why, the limit, the non-comparability note (§11.6), and a
  correction of the false `tool_result_cap`/`context_mode` pin claim
  (§10.2.1/§11.3): the merged config sets only the two collapse keys.
- `docs/plans/bench-hard.md` — this section.
- `CHANGELOG.md` [Unreleased] — bench entry (Block 2).
- Wakil itself unchanged: `git diff 6d1a86a..HEAD -- internal cmd
  prompts` stays empty. Throwaway replay test deleted before commit.

### 11.6 Block 2 — docs (planned)

`bench/README.md` (the measurement table, the calibration table, the
chosen ceiling and why, the limit: a different strategy can still stay
below; invalid remains the safety net), `CHANGELOG.md` [Unreleased]
entry. Non-comparability note: the single v2 row before this commit
(`20261125T013001`) used the 45,000 ceiling and isn't comparable; name
the commit.

### 11.7 Tests

- **Block 0/1 (throwaway replay):** the throwaway `internal/agent`
  calibration test prints the candidate × mode table (deleted before
  commit) plus per-collapse post-collapse `sentSize`, per-mode
  preambles, and the `hardMax` assert. The permanent record is the
  plan tables (§11.2–11.3) and the chosen literal in `scripts/bench.sh`.
- **Block 1 (self-test):** `scripts/bench.sh --self-test` — the
  collapse-long-v2 cases (grep-only fail, ranged-read pass, noop invalid)
  **plus a new v2 recording run asserting the new ceiling and the
  existing v1 recording run asserting 45000** (so a split bug cannot
  pass green); no other collapse-long-v1 case changes. Full gates per
  the task: `gofmt -l cmd internal prompts scripts`, `go vet ./...`,
  `golangci-lint run`, `go test -count=1 -timeout 30m ./...` (every
  package line reported), `scripts/bench.sh --self-test`, shellcheck on
  changed scripts if installed.
- **Block 2 (docs):** `scripts/check_config_docs.sh` unaffected (no
  config change); `bench/README.md` + CHANGELOG.

### 11.9 Mashūra round 2 — findings and disposition (2026-11-25)

Round 2 asked whether the Round-1 folds resolve the blocking
calibration errors. All three reviewers: folds resolved in **direction**
but two gaps remained. **Folded (R2):** the §11.2 input table now
carries an explicit per-call argument column summing to 12,588 with
rows ending at the anchored peak (102,268 chars) — the trajectory, not
just the total, is specified; selection now requires 1–3 in both modes
under **three named distributions** (§11.2) with no count boundary
within ~1.5 KB, and a defined fallback (no candidate passes ⇒ no
ceiling committed); `ToolResultCap` handling corrected whole (test cap
≥ 8,499, §11.2 — no per-call bypass fiction); checkpoint provenance
confirmed argument-derived (`extractPaths`/`buildFindings` read no
result content) with the ~1.5 KB boundary-margin rule; precondition
evidence cited from the log header with the missing run-time check
stated plainly (§11.1). **Corrected figure:** arguments 12,587–12,590
(was 12,441; the 146-B residue was final-answer text outside `W`).
**Listed:** candidate 20,000 sits on the thrash floor (expect
rejection by the stability rule, not by the sweep); `tokens_in` gap
noted but not reconciled (tool schemas are excluded from the ceiling
by design); "both modes" claims stay scoped to this recorded
strategy in README wording.

### 11.8 Mashūra round 1 — findings and disposition (2026-11-25)

Round 1 asked: "Is this a sound way to make collapse-long-v2 collapse
reliably, and what could make the calibration wrong?" (BLOCKING scoped
to a calibration error that changes the chosen number, a modules off/on
difference, or a leak of secrets or checks). 3 reviewers (claude-fable,
muse-wonder, gemini-neon) answered. Verdict: approach sound in shape,
but the calibration as drafted would pick the wrong number. All claims
below verified against the tree (read in this session), not taken on
faith.

**Folded as BLOCKING:**

- **B1 — Reconstruction undercounted by ~12.4 KB; the log holds the
  exact figure.** `12108` in the turn-end notice is `wSize/4`
  (`formatCollapseNotice`, `collapse.go:1449-1453`), `wSize` = Σ
  `messageSize` over the mid-turn messages (`collapse.go:1191-1194`,
  `messageSize` at `:2070-2079`). True `W` = 48,432–48,443 chars vs
  35,991 reconstructed. **Folded:** §11.1/§11.2 anchor to 48,432 and
  name the 12,441-B gap as argument bytes.
- **B2 — "Lower bound is safe" proves only ≥1, not ≤3 (see §11.4).**
  Candidates 17–20k sit in the post-collapse thrashing floor
  (`C > F/0.7 ≈ 19.7–20.3k` tokens). **Folded:** candidate range starts
  at 20,000; distribution sensitivity checked in the replay.
- **B3 — On-mode `load_module` pinning.** Verified
  (`preservedSchemaMessages`, `collapse.go:793-840`): a loaded module's
  schemas survive every collapse, raising `F` permanently. **Partly
  disproved for this trace:** every tool in the recorded trace is in
  `coreToolNames` (`modules.go:41-86`), so no tool needed
  `load_module` — zero loads is the honest on-mode replay of this
  strategy. **Folded:** on-mode = preamble swap only; host on-mode run
  recorded as unverified (DO NOT ALTER forbids new real-model runs).
- **B4 — `tool_result_cap`/`context_mode` not pinned (false §10.2.1
  claim).** Grep-verified: merge sets only the two collapse keys
  (`bench.sh:867`). **Folded:** corrected in §11.3/§11.4/§11.5; README
  correction assigned to Block 2; precondition recorded. No pins added
  (out of scope for the per-task ceiling split).
- **B5 — Over-cap double-count risk.** Resolved by code order:
  `toolLine` is built from post-cap text (`turn_phases.go:587`,
  `app.go:3967`). **Folded:** inject final in-Conv sizes, bypass the cap
  for the two 8,499-B calls (§11.2/§11.4).
- **B6 (muse-wonder 6) — Same as B4 for v2.** Folded with B4.
- **B7 (muse-wonder 7) — Self-test asserts nothing about the v2
  ceiling.** **Folded:** new v2 recording stub + v1 assertion (§11.5,
  §11.7).

**Listed, not folded:** "reliably" is n=1 — variance exceeds the window
(stated in §11.4); calibration not reproducible from the tree (the
replay is throwaway by DO NOT ALTER; the input tables in §11.2–11.3
are the permanent record); `tokens_in` (62k/request average) vs chars/4
is unexplained here (tool schemas are real request bytes but are
excluded from the ceiling by design — noted, not reconciled); stale
§8.7 comments updated per §11.5; README true-sum ranges / "parsed 640" /
wrong-sum values are a pre-existing leaky surface (unchanged here).

## 12. bench isolation and collapse-long-v3 (BASE c922afe, 2026-11-25)

### 12.0 What happened on the host (verified this session)

1. **MCP servers were available to bench runs.** The run config came from
   the user's own config (`$WAKIL_CONFIG`, else
   `~/.config/wakil/config.json`) for every task except `collapse-long`
   and `collapse-long-v2`, which got the merged config (Fix 1/Fix 2,
   §8–§9) — but that merge only *adds* the two collapse keys, so it kept
   every MCP key too. `wakil run` had no flag to disable MCP. Agents
   under test therefore saw `mcp:caj` (a sandboxed workspace server) and
   called `caj__run_command`, `caj__read_file`, `caj__edit_file`,
   `caj__write_file` — tools that run in a **different sandbox than the
   bench workdir**, so work landed in a foreign workspace
   (`bench/results/collapse-long-v2-1-022611.log` even shows two `/tmp`
   dirs deleted inside caj's sandbox, not the bench's).
   Scan of `bench/results/*.log` (this session, `grep -o '·
   [a-z0-9_-]+__[a-z0-9_-]+'`): **46 logs contain MCP tool calls**,
   `caj__run_command` 64×, `caj__read_file` 19×, `caj__edit_file` 8×,
   `caj__write_file` 7×. No `trello__`/`invoicely__`/`context7__` calls.
   The full row list is in §12.7.
2. **collapse-long-v2's task text does not match its data.** The text
   says each entry has "a year, an entity (an item code with an alias)"
   — the generated files have neither years nor item codes. Corrections
   say "Withdrawal for the entry N of X" with no visible numbering on
   the entry lines (the real rule is the 0-based index of the amended
   entry among the ledger block's lines, which no careful agent can
   confirm from the text). Ledger lines are duplicated (the generator
   writes both the amended entry's original line and the withdrawal
   line), and words are doubled ("The the", "the the miller") because
   the sentence templates prefix "the " to aliases that already start
   with "the " (`gen/main.go:95-106`). Careful agents refused: the
   022241 Haiku run computed the exact expected sums, then judged them
   unreliable and wrote no `report.json`; the 022522 run asked the user
   for the rule. Both count as fail — correct answers rejected by an
   ambiguous task.
3. **v2's answer key leaks into the work folder.**
   `gen/main.go:184` writes `answer-key.json` to `filepath.Dir(out)`;
   `setup.sh:13` invokes it as `go run -C "$here/gen" . "$PWD/docs"`
   with the **workdir** as cwd, so `Dir(docs)` is the workdir **root** —
   the sums sit next to `report.json` where the agent can read them.
   Two run logs prove agents saw it (022344: "I did not open
   `answer-key.json`, which is in the repo root"; 023650: same). The
   README claim that "the self-test asserts it equals the check.sh
   constants" is also false — no such assertion exists in
   `scripts/bench.sh` (grep: zero hits for `answer-key`). v2 is DO NOT
   ALTER, so this is fixed for v3 only and recorded as an open point
   for v2 rows.

### 12.1 Verified findings (read this session; file:line)

**Config keys that bring MCP servers or other external tools into a run**
(all in `internal/config/config.go` unless noted):

- `mcp_servers` (`MCPServers []MCPServerConfig`, `:563`; validation
  `:3688-3760`). Consumed by `internal/wiring/bootstrap.go:154-156` —
  `agent.NewMCPManager` is built when `len(cfg.MCPServers) > 0`; the
  manager's tools enter the tool array (`BuildTools`,
  `bootstrap.go:256`) and the module registry
  (`ModuleRegistryForApp`, `bootstrap.go:257`), and modules are
  registered (`RegisterMCPModules`, `bootstrap.go:271-273`). MCP tool
  names are `<server>__<tool>` (run-log evidence: `caj__run_command`).
- `subagent_mcp_servers` (`SubagentMCPServers []string`, `:771`) — the
  allowlist of MCP server names for tools-tier subagents. Harmless with
  no servers, but stripped for completeness.
- `mcp_module_keywords` (`MCPModuleKeywords map[string][]string`,
  `:474`) — turn-1 module preloading keyed on server names. Same.
- `searxng_url` (`SearXngURL`, `:561`) — the native `searxng_search` /
  `searxng_url_read` web-search tools (`internal/tools/searxng.go:15-49`,
  dispatch `internal/agent/app.go:3531-3533`). External tools; stripped.
- Mashūra / oracle keys (external counsel): `oracle_enabled`
  (`:584`), `oracle_model` (`:585`), `oracle_max_tokens` (`:586`),
  `oracle_api_key_env` (`:587`), `oracle_timeout_seconds` (`:592`),
  `mashura_fallback_model` (`:604`), `mashura_max_tokens` (`:609`),
  `mashura_timeout_seconds` (`:610`), `mashura_review_budget_usd`
  (`:614`), `mashura_mode` (`:615`), `mashura_max_tokens_by_tool`
  (`:620`), `mashura_models` (`:629`), `mashura_roles` (`:639`),
  `mashura_panels` (`:664`), `mashura_tool_panels` (`:670`), `mashura`
  (block, `:675`), `wf_oracle_mode` (`:681`). Tool offer gate:
  `mashuraOffered` (`internal/agent/mcp_manager.go:1064-1075`) — with
  the block, any non-off effective level offers the tools when the
  OpenRouter key is present; without it, `oracle_enabled: true` offers
  them. `--mashura off` (the bench default) only withholds
  `--auto-counsel` — it does **not** un-offer the tools, and the API
  key is read from the environment (never stored in config), so a
  merged config that keeps `oracle_enabled: true` or a `mashura` block
  leaves `mashura__review` etc. callable by the agent: external AI
  calls. All are stripped in the merged config.
- `wf_final_review` (`:682`), `wf_briefing_max_bytes` (`:683`),
  `auto_counsel` (`:617`), `counsel_max_per_session` (`:620-621`) —
  workflow/oracle-adjacent keys. The bench runs `wakil run` without
  `--plan`, so the workflow never starts; still stripped with the
  oracle keys for one coherent "no external counsel" surface.

**What a run writes to the user's real data, and the redirect keys:**

- Memory: `--no-memory` already strips it (`internal/wiring/bootstrap.go:398-430`;
  memory tools removed from the tool array `:275-296`). Not a leak.
- **Skill store: the real `<data>/skills/skills.db` is opened by every
  bench run** (`bootstrap.go:438-449` → `agent.SkillDBPath()`,
  `internal/agent/staging.go:109-115`). It is global, not
  workspace-keyed. Agents can read the user's skills (reference docs —
  data, not secrets) and `memory.Open` gives the store write access.
- Session history: workspace-keyed by the workdir path
  (`staging.go:124-134`, `bootstrap.go:454-465`) — writes a *sandboxed*
  index under `<data>/sessionhistory/<key-of-workdir>/`, not the user's
  real workspace index. Toolcache spill files likewise land under
  `<data>/toolcache/<chatID>/` (per-run chat IDs, sandboxed content).
- Env-redirect keys: `XDG_DATA_HOME` redirects *everything*
  (memory, skills, sessionhistory, sessions, repo-state, toolcache:
  `staging.go:63-71`, `internal/agent/persist.go:14-27`,
  `internal/agent/session.go:62-65`, `internal/tools/toolcap.go:91-99`,
  `internal/wiring/hisn_executor.go:149-158`). But it **also redirects
  the sandbox approval store** (`hisnStateDir` →
  `<XDG_DATA_HOME>/wakil/hisn/approved/…`,
  `internal/wiring/sandbox_project.go:42,115`): setting it for the run
  would invalidate the user's one-time `wakil sandbox trust` approval
  and every run would stop as `sandbox-not-acknowledged`. No key
  redirects only the skill store. **Open point, not changed** (see
  §12.6): a per-store redirect env var would be a Wakil change, which
  DO NOT ALTER forbids.

**Tool-call log format (for the guard):** the transcript log records
each tool call as `· <name> <arg> → …` (`toolLine`,
`internal/agent/app.go:3967-3973`, printed by
`internal/agent/turn_phases.go:587`). A `<server>__<tool>` name is the
one stable signature of an MCP call; no built-in tool name contains
`__` (verified: `grep -rn '__' internal/tools/` hits only comments;
run logs show `caj__run_command` exactly in this shape).

**Existing merge machinery to reuse** (`scripts/bench.sh`): the
collapse-long branch `:851-924` (private 0700 `mktemp -d` dir, 0600
file via `umask 077`, `../` escape check on `style_guide_paths`,
config-dir mirror, `bench_cfg_cleanup` `:548-563`, deleted on every
exit path, secret values never printed; log line records only
non-secret numbers). `csv_row` `:578-604` computes `valid` in one
place. Self-test: fake user config with sentinel
`AKIA_FAKE_SENTINEL` (`:1396-1405`), recording stubs, sentinel
leak-checks, missing/unparseable/escape config cases
(`:1655-1668`).

### 12.2 Block 1 — every task runs MCP-free

- Extend the merged-config mechanism to **every** task: all tasks get
  the private temp dir + merged config in which the MCP/external keys
  are removed or emptied — `mcp_servers` (deleted),
  `subagent_mcp_servers` (deleted), `mcp_module_keywords` (deleted),
  `searxng_url` (deleted), all oracle/mashura keys (deleted), plus the
  workflow oracle-adjacent keys (`wf_oracle_mode`,
  `wf_final_review`, `wf_briefing_max_bytes`, `auto_counsel`,
  `counsel_max_per_session`). **Deletion is not enough for two keys
  whose defaults are ON** (`MashuraLevelDefault` is `medium`,
  `config.go:1156`; `WFFinalReview` defaults true,
  `config.go:1625`): the merge also SETS explicit off values —
  `"oracle_enabled": false`, `"mashura": {"level": "off"}`,
  `"wf_final_review": false` — so the file strip cannot fall back to an
  enabled default (§12.9 B2/G2). The collapse tasks keep the two
  collapse keys (per-task ceiling: v1 45000, v2 21000, v3 21000) in the
  **same** merged file — one mechanism, no second merge. The endpoint
  block and everything else the model needs (endpoints with keys,
  model, `agent_prompt_path`, `style_guide_paths`, …) are kept as-is.
  **The launch env is scrubbed too:** `SEARXNG_URL` is unset for the
  `wakil run` invocation (env overrides apply after the file,
  `config.go:2149`); the remaining vars in that block are TUI-only or
  endpoint config, verified in §12.9.
- A run whose user config is missing merges onto `{}` plus the
  overrides; an unparseable one is `error`/`config-unreadable`/
  `valid=false:config` exactly as today (extended from the
  collapse-only branch to all tasks).
- **Normal-mode traps:** the merged file carries real API keys for
  every task now, so `main()` installs EXIT/INT/TERM traps calling
  `bench_cfg_cleanup` (today only `--self-test` has an EXIT trap,
  `bench.sh:3463`; a SIGINT on a normal run would leave the 0600 file
  in /tmp). INT/TERM added to the self-test trap as well (POSIX EXIT
  traps don't fire on signals).
- **Guard: after each run, scan the run log for MCP calls** — one
  `grep -E '^· [a-zA-Z0-9_-]+__[a-zA-Z0-9_-]+'` over the log file,
  excluding `mashura__` names (builtin counsel tools, §12.9). A hit
  rewrites the row to `result=invalid`, `exit_reason=mcp`,
  `valid=false:mcp` (a new `mcp)` branch in `csv_row`, which today
  maps every `invalid` to `false:no_collapse`), names the tool names in
  the log line, and skips `check.sh` (an MCP call is isolation
  failure, not a task result). The guard runs only on a completed run
  (before the leak/workdir guards' `check.sh` step); config-error and
  timeout rows are unaffected. The match is anchored to a line-leading
  `· ` (the `toolLine` shape, §12.1) so prose or JSONL payload text
  that merely contains `__` is not flagged, and the run log's
  non-transcript sections are excluded.
- Self-test (fake configs only): (a) the merged config for a **normal**
  task (fix-test) keeps the fake endpoints and has none of the
  MCP/external keys plus the explicit-off values (jq assertion on the
  recording stub's `$WAKIL_CONFIG`), and the stub saw `SEARXNG_URL`
  unset; (b) a stub log containing a `· caj__run_command …` line → row
  `invalid` / `mcp` / `false:mcp` and the log line names the tool; (b')
  a stub log containing `· mashura__review …` is NOT flagged; (c) a
  clean stub log is unaffected (row unchanged); (c') the existing
  no-override-line assertion for non-collapse tasks
  (`bench.sh:1702-1704`) is updated for the now-global override; (d)
  the existing secret-sentinel and byte-identity assertions still pass.
- Report: list the 46 MCP-contaminated CSV rows (§12.7), paths only,
  not committed.

### 12.3 Block 2 — collapse-long-v3 (v1/v2 unchanged)

New task `bench/tasks/collapse-long-v3/` (same layout; v1/v2 files are
not touched, apart from the all-task MCP-free config of Block 1 — no
byte of their fixture, task text, ceiling, or check changes).

**Task text ↔ data fidelity** (the v2 lesson): the text describes
exactly what the data holds — categories, entities referred to by
aliases (the only names in the sentence text), quantities in words,
corrections identified by a visible identifier. **Every ledger entry
line carries its own visible identifier** ("entry 7 of orchard"),
**per-file and 1-based** — "entry 7 of orchard" in `note-03.txt` names
that file's ledger entry 7; the same number in another file is a
different entry. The identifier uses the entity's base name ("orchard"),
the sentences use its aliases, and the task text ties them together in
one sentence. The withdrawal line repeats the entry's identifier,
states its replacement figure, and **binds only within its own file**;
each entry has at most one withdrawal (a generator invariant, checked
by the self-check). The rule is stated in the task text: "a withdrawal
replaces the figure of the entry it names, in the same file; that
entry's original figure no longer counts; only identifier-bearing
lines in the Ledger block count." The Ledger block is delimited
start/end markers, stated in the text. No years, no item codes, no
implicit numbering. **No duplicated ledger lines, no doubled words** —
the generator's sentence templates never prefix "the " to an alias,
and each entry line is emitted exactly once. A **generator
self-check** (in `gen/main.go`, runs on every generation) fails the
generation on any of: (a) a ledger line's text appearing twice in the
same file; (b) a doubled word (adjacent-token comparison,
case-insensitive — Go's RE2 regex has no backreferences, §12.9; ledger
lines only); (c) two withdrawals naming one entry; (d) an identifier
that does not resolve within its own file.

**Fixture (seed-generated, generator + seed outside the workdir):**
40 files `docs/note-XX.txt` ≈ 60 KB each. Ledger block per file past
the 8 KB ToolResultCap cut (20–45 KB in), 12 entries with categories
(6 categories), aliases per entity, quantities 1–30 in words;
withdrawals replace a fraction of entries (seeded, ~1/3 as v2). Dense
number-laden decoy filler around it (same filler style as v2, kept
number-word-bearing). Early and late files both required (the answer
needs sums from all 40; the check requires all categories present).
Must-read ledger text ≈ 800 KB ≈ 4× the 84,000-char ceiling (same
envelope as v2, §10.2).

**Answer key placement (v2 leak fixed):** the generator takes an
explicit key-out path; `setup.sh` passes a path under the **task dir**
(`gen/answer-key.json`, outside the workdir), the file is
deterministic and **committed** (byte-stability proven by the
two-generation `diff -r`) — never written into the work folder. The
workdir guard (`leak_check`) additionally greps for `answer-key.json`
in the work folder as a regression lock (§12.9 B5), and the self-test
asserts its absence there while tying the committed key to the
`check.sh` constants (parsing both, comparing values) — a generator
bug cannot leave a green self-test over an unsolvable task.

**Task text adds** (verbatim requirement): "Always write report.json
with your best answer. If you have doubts, put them in a `notes` field
in report.json; the totals are what count." The text shows the exact
report.json shape: the six category keys at top level (integers), an
optional `notes` field of any JSON type, and no other keys.

**check.sh:** parses `report.json`; **reads only the total keys** —
the `notes` field is ignored regardless of its JSON type; a key other
than the six categories plus `notes` fails; the six category sums
(integers) must equal the expected values. Missing `report.json` = fail
(gocheck_fail, as v2). Unparseable JSON = fail. The expected values
are the generator's own sums (tied via the self-test, above).

**Ceiling: 21,000 tokens (84,000 chars), same as v2.** Verified with
the §11 replay method (a throwaway Go test in `internal/agent`,
deleted before commit, driving the real `streamTurn` +
`collapseMidTurnPrefix` through the fake backend/executor with the v3
task text and each mode's real `buildPreambleForNetwork`): the v3 task
text is the same length class as v2's (751 B), and the §11.2 recorded
conversation-shape inputs (result/argument/text byte sizes — a
strategy property, not a fixture-content property) are reused as the
strategy model with the v3 task-text byte length substituted. The
replay asserts 1–3 mid-turn collapses in **both** modes at 21,000
under the three argument distributions; if not, the ceiling is
recalibrated with the §11.3 candidate sweep and the numbers are
recorded here and in the README. (§11.4's strategy-model caveat
applies: the replay models the recorded Haiku strategy against the v3
prompt length; no real-model runs.)

**Self-test cases (v3):** reference passes (solve stub);
untouched fixture fails; wrong report fails; grep-only (naive
first-number) fails; ranged-read stub passes; generator self-check
passes (run the generator in the self-test work folder; a deliberately
broken fixture — one duplicated ledger line — must make the self-check
fail, proven by a purpose-built broken-fixture generation in a temp
dir, never by editing the shipped fixture); a report with a `notes`
field (non-string type included) and correct totals passes; the
committed `gen/answer-key.json` equals the `check.sh` constants. The
solve/noop stubs' collapse counting extends to v3 by task name (the
`csv_row` `invalid` no-collapse rule adds `collapse-long-v3`, as v2
did).

### 12.4 Block 3 — docs

- `bench/README.md`: MCP isolation (every task runs with the merged
  MCP-free config; what is stripped; why the endpoints stay) and the
  post-run MCP guard (`invalid`/`false:mcp`); a note that rows before
  this commit may include MCP calls, with the count and the pointer to
  the plan §12.7 list; the v3 section (what it is, why v2 was
  ambiguous — text/data mismatch, invisible entry numbering, doubled
  words/duplicated lines; visible entry identifiers; the `notes`
  field; the ceiling and its replay verification; the answer-key
  placement fix and the v2 open point).
- `CHANGELOG.md` [Unreleased]: bench entry covering all three blocks.

### 12.5 Touched paths

- `scripts/bench.sh` — all-task merged config + MCP key strip +
  `false:mcp` guard + self-test cases (Block 1); v3 ceiling branch +
  v3 self-test cases (Block 2).
- `bench/tasks/collapse-long-v3/` — new task (task.txt, gen/main.go +
  answer key in gen/, setup.sh, check.sh, solution/report.json,
  repo/go.mod).
- `bench/README.md`, `CHANGELOG.md`, `docs/plans/bench-hard.md` (this
  section).
- Wakil itself unchanged: `git diff 6d1a86a..HEAD -- internal cmd
  prompts` stays empty. Throwaway replay test deleted before commit.

### 12.6 Open risks and open points

- **Skill store (open point, not changed):** a bench run opens the
  user's real `skills.db` (read/write). No config key or env var
  redirects only that store; `XDG_DATA_HOME` would also move the
  sandbox-approval store and invalidate the one-time `wakil sandbox
  trust` (§12.1). A per-store redirect would be a Wakil change —
  listed here, not implemented (DO NOT ALTER: Wakil stays unchanged).
  Exposure is bounded: skills are project-agnostic reference docs; a
  run can read them (prompt material) and the store can persist
  writes. The MCP strip removes the *external-tool* path; this one is
  a *data* path, recorded.
- **v2 answer-key leak (open point):** v2 rows before this commit may
  have been answerable by reading `answer-key.json` from the repo
  root. v2 is DO NOT ALTER, so no fix there; v3 fixes it and v2 rows
  stay non-comparable with v3 anyway (different task). Two v2 run
  logs show agents *noticed* the file and declined to read it; no
  evidence any run used it, but the possibility is recorded.
- **MCP guard is log-based:** the transcript log's `· <name>` line is
  the only in-log MCP signature. The line is printed per call
  (`turn_phases.go:587`), so every executed MCP call is caught; a
  hypothetical future binary that stops printing tool lines would
  need a new signature. Fail-open is not possible for the *strip*
  (config-level: no servers exist to call) — the strip is the primary
  control, the guard the tripwire.
- **Merged-config key drift:** the strip list is a fixed key list in
  `bench.sh`; a future Wakil config key that adds external tools would
  need the list extended (same maintenance property as the existing
  escape check; stated in README).
- **Ceiling replay is strategy-modelled:** as §11.3/§11.4 — the
  recorded Haiku strategy, not a v3-specific run (no real-model runs
  allowed). The `invalid` safety net stays.
- **Timeout/cost envelope:** v3 same as v2 (~100 ranged reads;
  `--timeout 1800` recommended, README).

### 12.7 MCP-contaminated rows (logs under bench/results; not committed)

46 logs, all under `bench/results/`: add-flag-1-220233,
add-flag-1-223202, add-flag-1-224813, add-flag-2-110606,
add-flag-2-220248, add-flag-2-220944, add-flag-3-110635,
add-flag-3-111311, add-flag-3-144357, add-flag-3-220322,
add-flag-3-221025, add-flag-3-224849, collapse-long-v2-1-022611,
collapse-long-v2-2-022850, collapse-long-v2-3-023152,
debug-subtle-2-021436, fix-test-1-105835, fix-test-1-110701,
fix-test-1-171306, fix-test-1-225552, fix-test-2-110736,
fix-test-2-144853, fix-test-2-220449, fix-test-2-222138,
fix-test-2-224930, fix-test-3-110824, fix-test-3-225010,
long-read-1-110123, long-read-1-110923, long-read-1-225703,
long-read-2-110951, long-read-2-220639, long-read-3-111014,
long-read-3-225238, refactor-multi-1-004803, refactor-multi-1-013850,
refactor-multi-1-020848, refactor-multi-2-020959,
refactor-multi-3-021041, rename-1-111039, rename-2-111122,
rename-2-225324, rename-3-111144, rename-3-225351, steps-2-225434,
steps-3-111235.

### 12.8 Tests

- Block 1: `scripts/bench.sh --self-test` (new merged-config-for-all
  cases, MCP guard cases, existing sentinel/byte-identity cases
  unchanged); full gates per the task: `gofmt -l cmd internal prompts
  scripts` (empty), `go vet ./...`, `golangci-lint run`, `go test
  -count=1 -timeout 30m ./...` (every package line reported),
  shellcheck on changed scripts if installed (SKIP if not — it is not
  installed on this host).
- Block 2: self-test v3 cases (§12.3); the throwaway replay (deleted
  before commit) verifying the 21,000 ceiling gives 1–3 collapses in
  both modes; determinism check (two generations byte-identical,
  `diff -r`); best-effort script-only solver must produce a wrong
  report (pre-commit gate, as v2 §10.2).
- Block 3: docs-only; `scripts/check_config_docs.sh` unaffected.

### 12.9 Mashūra round 1 — findings and disposition

Round 1 (panel of 3, BLOCKING = sandbox escape / gameable check / v3
ambiguity / secret leak). Round 2 is not required: every blocking item
below is either verified and folded into the design, or verified false
and dismissed with file:line evidence.

**Folded into the design (verified against the code):**

- **Env overrides survive the file strip (B1, fable; G1, gemini).**
  `envStr(&cfg.SearXngURL, "SEARXNG_URL")` (`config.go:2149`) — env
  applied after the file. Fix: the bench launch unsets `SEARXNG_URL`
  (and audits the env block `:2149-2180` — no other var there enables
  a tool; `WAKIL_MENTION_BASE` is TUI-only, see below). Self-test (a)
  extends: the stub asserts the variable is unset.
- **Deleted mashura keys fall back to DEFAULT MEDIUM (B2/G2).**
  `MashuraLevelDefault = MashuraLevelMedium` (`config.go:1156`); with
  no block and no legacy keys, `ResolveEffective` returns the default
  (`config.go:1225-1231`). Fix: the merged config SETS explicit off —
  `"oracle_enabled": false`, `"mashura": {"level": "off"}` — and
  deletes the remaining oracle/mashura keys. (With the block present,
  legacy keys are ignored, `config.go:2057-2062`;
  `deriveMashuraFromLegacy` maps `oracle_enabled: false` → off,
  `config.go:1742-1746` — belt and braces.)
- **No normal-mode EXIT trap (G3).** `trap st_on_exit EXIT` exists only
  in `--self-test` (`bench.sh:3463`); normal mode has none, so
  Ctrl-C/SIGTERM during a run leaves the 0600 merged config in /tmp.
  With Block 1 the file now exists for every task. Fix: normal-mode
  EXIT/INT/TERM traps calling `bench_cfg_cleanup` (INT/TERM also added
  to the self-test trap; POSIX `EXIT` traps don't fire on signals).
- **Doubled-word regex can't compile in Go (fable/gemini, listed).**
  RE2 has no backreferences. Fix: the generator self-check tokenizes
  each line and compares adjacent words case-insensitively (ledger
  lines only; filler may legitimately contain "had had").
- **`answer-key.json` not in leak_check (B5, muse-wonder).** The
  workdir guard greps for `check.sh hidden solution gen setup.sh`
  only. Fix: `answer-key.json` added to the guard + self-test asserts
  its absence from the generated work folder (regression lock for the
  v2-class path mistake).
- **`csv_row` has no `mcp` exit-reason branch (fable, listed; also
  muse-wonder B5).** `invalid)` maps unconditionally to
  `false:no_collapse`. Fix: the guard sets `exit_reason="mcp"` and
  `csv_row` gains an explicit `mcp)` branch producing
  `result=invalid, valid=false:mcp`.
- **Guard false positives / mashura collision (fable, listed).** The
  log is JSONL; prose mentioning `foo__bar` would false-positive, and
  the built-in `mashura__review` family also contains `__` (so
  §12.1's "no built-in tool contains `__`" was wrong — corrected).
  Fix: the guard matches only the transcript `output` lines that
  start with `· ` (the `toolLine` shape) AND excludes `mashura__`
  names (builtin counsel tools, already off via the explicit-off
  strip; a mashura call after the strip is impossible, and the
  exclusion keeps a stray builtin name from mislabeling a row).
- **v3 under-specifications (B6 fable; #4/#5 gemini; #6 muse-wonder).**
  Folded into the v3 task text + generator invariants: identifiers are
  per-file and 1-based ("entry 7 of orchard" in note-03 names that
  file's ledger entry 7); a withdrawal binds only within its own file;
  at most one withdrawal per entry (generator invariant + self-check);
  the withdrawal line repeats the entry's identifier and states its
  replacement figure; the text says only identifier-bearing lines in
  the Ledger block count, and how the block is delimited; the text
  states the entity's base name in the identifier, aliases in the
  sentences, with one sentence tying them; report.json shape is shown
  exactly in the text (the six category keys at top level, optional
  `notes` of any JSON type, no other keys — the check ignores `notes`
  regardless of type); totals are integers, the text says so.
- **Self-test case (d) stale (fable/gemini, listed).** The existing
  assertion that non-collapse tasks log no override line
  (`bench.sh:1702-1704`) must be inverted/updated when the merge
  extends to all tasks. Folded.
- **`gen/answer-key.json` written into the tree (fable/gemini,
  listed).** The v3 setup runs the generator with an explicit key-out
  path under the **task dir** (`gen/answer-key.json`, outside the
  workdir, deterministic → committed; verified byte-stable by the
  two-generation `diff -r`). The workdir never receives a key; the
  leak_check lock above catches any regression.
- **`ln` missing from harness_tools (gemini, listed).** Cosmetic; the
  mirror already works in self-test (exercised). Not folded — recorded.

**Dismissed with file:line evidence (panel claims that do not hold):**

- **RepoState restores MCP/mashura state into a run (B3, fable).**
  `RestoreRepoState` is TUI-only by contract (`repostate.go:330-347`:
  "cmd/wakil/run.go must never call this function"); `RunHeadless`
  (`headless.go:253-392`) never calls it or the conversation manager's
  `restoreRepoState`. The per-repo mashura level cannot enter a
  headless bench run.
- **Cross-run transcript leak via /sessions //recall //remember (B4,
  fable).** All three are TUI slash commands (`commands.go:557`,
  `:630`, `:571`), dispatched only from the TUI input path
  (`tui.go:1805-1816` → `DispatchCommand`). The headless path is
  `SubmitInput` → `host.handleInput` (`host.go:1094`) →
  `DriveTurnWithResilience` (`resilience.go:185`) → `SendOutcome`
  (`app.go:1263`) → `streamTurn` (`turn_phases.go:216`) — no
  slash-command dispatch anywhere; a task text starting with "/" is
  plain text to the model. Per-run ChatIDs key the transcripts; no
  agent tool reads them.
- **`plugins` server-side web search (B3, fable).** `wireBody` appends
  exactly one plugin, `context-compression` disabled
  (`proxy/client.go:1285-1295`); there is no web-search plugin path.
- **`WAKIL_MENTION_BASE` / mention injection (fable, audit of
  `:2149-2186`).** `@`-mention resolution is TUI-only
  (`tools.ResolveMentions` is called only from `tui.go:1845/:2035`);
  headless never resolves mentions.
- **B7 (fable): "script-only solver gate contradicts the v3 design."**
  The gate is the §10.2-style *pre-commit host check* (a careful
  one-shot script over the generated data must not produce the exact
  totals — the withdrawals/aliasing see to that), not a self-test row
  that would require ambiguity. Kept.

**Listed, not folded:** `--mashura low|medium|high` still passes
`--auto-counsel` (the bench pins `--mashura off`; a non-off value
combined with the explicit-off config strip is a config-vs-flag
contradiction → the README documents that non-off is rejected by the
bench while the strip exists); ceiling likely fires less than v2 (v3
removes the ambiguity that drove the 18-call probing trace — §11.2
strategy model reused with that caveat stated; the `invalid`
no-collapse safety net stands, and the replay recalibrates if needed);
config-dir mirror surface (Wakil reads only `agent.txt` from the config
dir on the run path — `style_guide_paths`/`agent_prompt_path` come from
the config JSON itself; recorded, not re-proved); merged-config-for-all
changes the failure surface of unparseable user configs for every task
(a comparability note in the README, intended); the SIGKILL-leftover
temp-config exposure is the same class as before the change (documented
in §8/§9, unchanged); `searxng_search`/`searxng_url_read` names contain
no `__` — the guard's `__` pattern cannot catch them, which is fine:
the strip + `SEARXNG_URL` unset remove them at the source, and the
guard is the tripwire, not the control.

