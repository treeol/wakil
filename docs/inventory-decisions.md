# Wakil decision sheet — 2026-11-04

Companion to [inventory-2026-11.md](inventory-2026-11.md). Read §0 there first:
**no usage evidence was available** (the sandbox cannot see
`~/.local/share/wakil`; no grant was possible; the trace schema also lacks most
of the fields asked for). Every decision below is therefore based on code
structure, reachability, default state, doc drift, and git churn only.
A decision marked *pending usage* must be confirmed with the counting recipe
(inventory Appendix B) before anything is deleted.

Tags: **[V]** I measured it; **[S]** subagent-reported, not re-verified;
**[inferred — confirm]**.

Nothing was changed except these two files. No commit was made.

## 1. Decisions per row

Legend: blast radius = what else breaks; "deps out" = go.mod lines that leave
(measured by deleting the dirs from a copy of the tree and re-listing the
module graph, [V]).

### S01 Entry, wiring, prompts — KEEP
Binary entry and assembly; every other row hangs off it. `wiring` is the biggest
non-agent package (6,197 lines) and churns daily (54 commits), so it will not
shrink until the rows below do. Blast radius: n/a. Deps out: none.

### S02 Agent core loop, tool handlers — KEEP
The product. Fold-worthy leftovers are the 16 unreachable unexported/exported
funcs in `internal/agent` (deadcode without `-test`), see §3. Deps out: none.

### S03 Context management — KEEP
Default `context_mode` is `long-session`, so this runs on every session
(`config.go` defaults, [S]). It has the second-largest test body in the agent after the core loop (6,956
lines, 179 tests; core loop 20,755); the cost is maintenance, not removal. Deps out: none.

### S04 Subagents, async ops, agent-side worktrees — KEEP
Async ops carry subagents, Mashūra and background shell, so removal would take
three features with it. Two private funcs are unreachable
(`finishAsyncOp`, `enqueueAsyncOp`, `async_ops.go:864,880`). The worktree story is
two implementations — see §2 item 5. Deps out: none.

### S05 Skills — KEEP store tools; DEPRECATE auto-creation and templates *(pending usage)*
- Store tools (792 lines) are plain CRUD over the memory DB: KEEP.
- Templates (2,400 lines, 3 tools) and auto-creation (2,212 lines, default mode
  **`on`**, `skill_creation.go:102-107`) are the newest code in the agent
  (last touched 2026-10-31/11-01) with 3,043 test lines, and `deadcode` finds 4
  unreachable funcs in them (`resolveTemplateParams`, `renderTemplate`,
  `joinSkillTemplate`, `formatSkillSequence`). Two reasons for caution: no
  dedicated doc page exists, and the default is on, so users get the behavior
  without asking.
- Blast radius: config key `skill_creation` (+ sub-keys), commands `/skill-create`
  `/skill-creation`, tools `apply_skill_template capture_skill_template
  check_skill_template`, the "app note" nudges, tests, `docs/tools.md` rows. The
  `.wakil/skill-templates.json` provenance file would be orphaned.
- Deps out: none.

### S06 Memory tiers, staging, session history — FOLD the plan hand-off; KEEP the rest
- Evidence against the tier design: `memory_list` shows **90 quarantined
  `plan/current` entries** (104 listed in all, 2026-09-07..2026-11-04), and
  `memory_get("plan/current")` at session start returned "not found". Every
  session that touches MCP, web, or Mashūra is tainted, so mid-tier writes are
  quarantined and a plan can never be handed off. The documented hand-off
  protocol therefore does not work in the sessions that use it most. [V]
- Proposal: store the plan in staging (ephemeral, no promotion needed) or exempt
  `plan/current` from quarantine; delete the 90 stale rows by TTL. KEEP memory
  store, staging, and sessionhistory.
- `correction` capture (675 lines) and `learn` (80) are proposal pipelines whose
  outputs sit as *proposed* until the user promotes them: the listing shows 2
  `correction/*` entries (2026-09-11, 2026-10-18), both still proposed, among 103
  pending proposals. DEPRECATE pending usage if the user never promotes them.
- Blast radius: `plan/current` text in the system prompt; memory.md. Deps out: none
  (sqlite is shared with sessionhistory/sqlstore/tokenstore).

### S07 Counsel / Mashūra — KEEP
Used here for the final review of this sheet. ~20 config keys and 11 `docs/` pages
mention it; the cost is key sprawl, see §2 item 3 and §4. Deps out: none.

### S08 Model clients (proxy, orregistry) — KEEP
`proxy` has two endpoint kinds only (`openai`, `ilm-proxy`). AGENTS.md says
"Anthropic, OpenRouter" — wrong or counsel-only [inferred — confirm].
`orregistry` (371 lines, 0 commits in 60 days, only 2 importers) is fine to keep;
`SetCacheForTest`/`SetPricingForTest`/`SetModelsURLForTest`/`ResetCache` are
exported test hooks in a non-test file (`orregistry.go:333-363`, deadcode). Deps out: none.

### S09 Sandbox backends (exec, sandbox/*, hisn) — KEEP
hisn is the default (`81319db`) and what this session runs on. `sandbox/fake` has 14
unreachable funcs outside tests (it is a test double; fine). `BurntSushi/toml` is
used only by `sandbox/hisn` yet is listed `// indirect` in `go.mod:23`; fix with
`go mod tidy` (not done). Deps out: none.

### S10 Docker-only executor path — KEEP, but FOLD INTO `BackendExecutor` *(owner decision)*
- Docker must keep working (user requirement in the hisn task) and CI builds
  darwin, where hisn (Landlock) is not an option [inferred — confirm]. So no removal.
- It is the second implementation of everything: `DockerExecutor`
  (1,771 lines) beside `BackendExecutor` (867); `dockerWorktreeExecutor`
  (232) beside `BackendExecutor.NewWorktreeExecutor`; the `dockerBackend` adapter
  (`docker_backend.go`, 75 lines) exists "so docker satisfies the same lower-level
  contract" but has no non-test caller by grep.
- Proposal: either finish the migration (docker as a `sandbox.Backend` driven by
  `BackendExecutor`, delete `docker_worktree.go` and the adapter, about -300 lines
  now, -2,000 later) or delete `dockerBackend` and stop pretending. Choose one.
- Blast radius: the Docker image, `docker/entrypoint.sh`, `Dockerfile` (CI:
  multi-arch buildx), tests that use real docker. Deps out: none.

### S11 TUI and session client — KEEP
Primary UI; 73 commits in 60 days. No imports of `agent` (guarded by test).
Deps out only if the TUI went: bubbles, bubbletea, glamour, plus 13 transitive
(chroma, goldmark, bluemonday, …).

### S12 Daemon stack — split decision (pending usage)

**S12a oidcresolver + OIDC RPCs — REMOVE.**
The only non-test construction is `oidcresolver.New(tokenStore, nil, Config{})`
(`daemon_server.go:334`): a nil validator, so `Resolve` returns "disabled" on every
path; `OIDCConfig{}` is constructed only in `cmd/wakil/p4e_test.go`. No flag or
config key can supply an issuer or validator. [V]
Size: `oidcresolver` 298 code / 361 test, plus `GetOIDCAuthURL`/`ExchangeOIDCCode`
handlers, `NewAuthHandlerWithOIDC*`, `NewServerFromInterfaces*` (3 unreachable
constructors, `server.go:138,180,198`), README mention. Blast radius: proto RPCs
(`buf breaking` will fail; the fields/RPCs must be `reserved` or the check
waived), regenerated `api/gen`, `docs`/README OIDC lines. Deps out: none.

**S12b rest of the daemon (jointoken, apitoken, tokenstore, tokenresolver, peercred,
server/connect, remote client, crypto, stores, protoconv, web, `--daemon`/`daemon`) —
DEPRECATE if usage count is zero; otherwise KEEP and document.**
Reasons: (1) it is 9,556 hand-written lines + 8,708 test lines + 13,816 generated +
1,632 web lines, about 10% of hand-written code, behind the `daemon` subcommand and
`--daemon`; (2) 8 REPL features are unavailable remotely (`/mcp /plan /verify /learn
/image /handoff /remember /recall`, [S] `remote/facade.go:29-58`), so remote is a
reduced product; (3) near-zero recent churn in the auth tree (apitoken/
tokenstore last substantive 2026-09-18, the rest 2026-08-26) while the rest of the
tree moves daily; (4) **undocumented**: no `docs/` page covers tokens, workspaces,
backends, agents, or the web UI; `--http-addr`, `--tls-cert`, `--tls-key`,
`--allowed-origins` appear in no doc ([V] grep); (5) `Dockerfile.daemon` and
`docker-compose.yml` ship it, so someone may deploy it.
Blast radius if removed: `cmd/wakil/daemon*.go` (858), `Dockerfile.daemon`,
`docker-compose.yml`, `buf.yaml/buf.lock`, `api/`, `web/`, `internal/remote`,
`scripts/check_coverage.sh` floors, CI jobs (`buf breaking`, daemon image), up to
3,103 test lines in `cmd/wakil`, docs `remote-provisioning.md` partly. **Not**
removable: `core`, `core/event`, `sessionhost`, `sqlstore` (the embedded TUI path
imports `sessionhost` — verified by the import graph: `sessionhost` ← cmd, wiring,
server/connect), `scrub`, `migrations` (also `sqlstore`).
Deps out if S12a+S12b both go: `connectrpc.com/connect`, `google.golang.org/protobuf`
(and nothing else; `uuid` and `sqlite` stay) [V].
Intermediate option: keep `--ephemeral` Unix-socket daemon (no tokens, no TCP, no
web, no OIDC), which `main.go`'s `--daemon` client needs, and delete the auth tree,
stores, web, and TCP listener.

### S13 Shared core — KEEP
Used by the embedded path. Dead code: 7 unused `core/id` constructors
(`NewFromReader NewTenantID NewUserID NewWorkspaceID NewTurnID NewToolCallID
NewSubagentID`), `WithNow`, `WithIDGenerator` (test-only seams). Remove the 9
functions, not the package. Deps out: none.

### S14 Browser — DEPRECATE
- Default off, and **forced off under hisn** (`config.go:2340-2346`), which is now
  the default backend. It can run only with docker or direct, and under `direct`
  the browser runs unsandboxed on the host.
- Costs: 940 + 854 test lines, `chromedp` + `cdproto` + `go-json-experiment/json`
  (3 go.mod lines out [V]), `chromium` in the sandbox image (`Dockerfile:64,
  97-114`, a long workaround block), 8 tools in the tool list.
- Blast radius: keys `browser_enabled`, `browser_path`, flag-free; `docs/features.md`,
  `tools.md`, `configuration.md:142,338`, README; `BrowserForcedOff` startup banner;
  `SandboxExecutor` interface in `browser/manager.go`. Smaller image, one fewer tool
  family in each prompt.

### S15 LSP — KEEP
Pure Go, no external deps, default off, powers `/repomap --symbols`. Only 1
unreachable method. The cheapest feature per capability in the tree.

### S16 `/plan` workflow — KEEP *(pending usage)*
1,240 code lines, 1 commit in 60 days, 40-line doc. It hard-gates tools by phase
(`wfPhaseBlock`, `app.go:2578-2614`), which makes it intrusive. Overlaps the user's
global skills `task-workflow`/`trello-card-workflow`; if `/plan` is rarely used,
DEPRECATE in favour of skills. `workflow_engine.go` has no tests of its own.

### S17 ILM emitter and assist — DEPRECATE *(owner decision)*
`mode` defaults off ("zero network calls"); shadow/assist only. Docs and code
disagree, which is a sign nobody exercises it: `docs/ilm-stack/wakil-emitter.md`
names `batch_size` (default 64), `flush_interval_s` (`ILM_FLUSH_S`),
`output_byte_limit`; the code has `batch_ms` (default 500) and `max_output_bytes`;
`ILM_FLUSH_S` is read nowhere, and **`ILM_BATCH_SIZE` writes into `BatchMS`**
(`config.go:1474`) [V]. Last substantive commit 2026-09-18. Caveat: the default
model name is `ilm` and one endpoint kind is `ilm-proxy`, so the project is the
author's own ecosystem. Keep `ilm-proxy` as an endpoint kind in S08; this row is only
the emitter + assist (1,647 lines + 178 in agent). Blast radius: `ilm_stack` config
block, `/assist`, 191 lines of `docs/ilm-stack/`, `agent/assist.go`. Deps out: none.

### S18 Style guides — KEEP
Shipped 2026-10-26, small (830 lines), default off. The built-in guide source
directory `google-style-guides/` is **untracked** (git-excluded); how `styleguide`
obtains built-in guides on a fresh clone was not verified [inferred — confirm].

### S19 Verify — KEEP
530 pure lines, 0 commits since 2026-08-15, used by `/verify` and `/init`. Folding
into `agent` saves nothing. One unreachable method (`Outcome.WasSkipped`).

### S20 Scrub, diag, trace, policy — KEEP scrub/diag; FOLD trace into session history *(pending usage)*; KEEP policy only if headless is used
- scrub: security control; KEEP.
- diag: KEEP (9 importers).
- trace: opt-in JSONL, default off, and its schema (§0 of the inventory) cannot
  answer tool/provider/backend/slash-command questions that this very inventory
  needed. Either extend the record (add `sandbox_backend`, `provider`, `slash`,
  `subagent_type`) or fold into the sessionhistory DB so there is one record of
  what happened instead of four (trace JSONL, ILM queue, sessionhistory, toolcache).
- policy: the only non-test `LoadFile` caller is `wiring/headless.go:296` [V], so the
  declarative policy engine is reachable from headless runs only. 0 commits since
  2026-07-20. KEEP if headless/CI is used; otherwise DEPRECATE.

### S21 MCP client — KEEP
Four servers are live in this session. Removal would delete the default tool route
(`handleMCPTool`) and eight modules (`go-sdk`, `jsonschema-go`, `segmentio/asm`,
`segmentio/encoding`, `yosida95/uritemplate`, `oauth2`, `x/sync`, `x/time`).

### S22 Config — KEEP; remove aliases (see §2)
206 keys, 70 env names, 32 flags. The surface is the problem, not the package.

### S23 Small agent features — KEEP slash dispatcher, hooks, git tools, math;
FOLD `/init` and `/review` into skills *(pending usage)*
`/init` (376 lines), `/review` (340), `repostate` (625: `RestoreRepoState` and
`RestoreRepoStateResume` unreachable per deadcode), `/repomap` (305) are
one-shot commands. `repostate`'s two restore functions are called by neither the
embedded nor the remote path [V deadcode]; verify before deleting.

## 2. Exists in two forms (old path kept beside new path)

1. **`exec_mode` / `--exec` / `ILM_EXEC_MODE` / `WAKIL_EXEC_MODE`** → replaced by
   `sandbox.backend` / `--sandbox` / `WAKIL_SANDBOX_BACKEND`. Parsed at
   `config.go:220,1445-1446,1488`, mapped by `resolveBackend` (~`:2284-2324`).
   47 non-test Go references to the field, because the resolved value is still stored
   in `cfg.ExecMode` and switched on at `wiring/bootstrap.go:454` and
   `hisn_executor.go:23`. **Still used by shipped files**: `Dockerfile.daemon:69,78`
   and `docker-compose.yml:54,79` set `WAKIL_EXEC_MODE`. Docs: configuration.md
   lines 16, 311, 325, 339; sandbox-backends.md 9, 11, 18; design-note-hisn-031.md 14.
   Proposal: change the compose/Dockerfile to `WAKIL_SANDBOX_BACKEND=direct`, then
   REMOVE the alias (keep the internal field). The warning at startup suggests the
   deprecation has already shipped.
2. **Top-level `image` vs `sandbox.image`; `docker_allow_userns` vs
   `sandbox.docker.allow_userns`** (`config.go:1321-1332`). REMOVE the top-level
   spellings after the exec_mode change; docs say the old ones are accepted.
3. **`oracle_*` keys vs `mashura_*` keys; `wf_oracle_mode` vs `mashura_mode`;
   tool `oracle__ask` vs `mashura__*`.** Code (`config.go:451-454`) says `mashura_*` is
   canonical, but `docs/configuration.md:134-135` documents `oracle_enabled` and
   `oracle_model` as the primary keys. `oracle__ask` is routed in `app.go:2705`.
   Proposal: pick one spelling, migrate the docs; remove `oracle__ask`.
4. **`ILM_*` vs `WAKIL_*` environment prefixes**: 47 `envStr/envBool/envInt` calls
   each read both (`config.go:1429-1476`). Proposal: drop the `ILM_` read side
   except the genuinely ILM-stack names.
5. **Docker worktree executor vs `BackendExecutor` worktree** (S10 above).
6. **`/skill-create` vs `/skill-creation`** (`commands.go:1340-1359`): old manual
   flow beside the new auto-creation manager.
7. **`/reset`=`/new`, `/think`=`/thinking`, `/exit`=`/quit`, `/mode long`=`long-session`**:
   one-line aliases; harmless, keep.
8. **Embedded vs remote** facade: `wiring/facade.go` and `remote/facade.go` implement
   the same `sessionclient.Facade`; remote lacks 8 commands (S12b).
9. **Old `wakild` binary** is gone (merged into `wakil daemon`); no live reference
   (only `tmp/audit-draft.md`, untracked). No action.

## 3. Dead code (unreachable, measured)

`go run golang.org/x/tools/cmd/deadcode@latest`: `-test ./...` reports 27; without
`-test` it reports 102. The 27 test-reachable-excluded ones:
`agent`: `tuiConfirmer`, `RestoreRepoStateResume`, `StagingSocketPath`, `RunTurn`;
`apitoken.IsNotFound`; `config.OutputModeIsValid`, `ClipboardModeIsValid`;
`core/id`: 7 constructors; `sessionhost.WithNow`, `WithIDGenerator`;
`sandbox.Run` (`cmdhandle.go:108`); `fake.Echo`; `hisn.DefaultStateDir`;
`connect.NewAuthHandlerWithOIDCAndSecureCookies`, `NewServerFromInterfaces`,
`Server.SessionState`, `NewServerFromInterfacesWithAuth`,
`NewServerFromInterfacesWithAuthAndOIDC`; `staging.MaxKeyLen`, `MaxMGetKeys`,
`MaxFrameSize`. The 102-line set adds, among others, test hooks in production files
(`orregistry` ×4, `tokenstore.OpenTestDB`, `browser.ScreenshotDir`,
`browser.IsBrowserTool`), `ParseContextLimitJSON`, `RestoreRepoState`, and 4 skill
functions. Treat `deadcode` as a lead: it cannot see reflection or interface-only
dispatch (for example `DockerExecutor.Backend()` is not flagged though I found no
caller).

**Config keys that look unused:** a field-name scan (`.<Field>` outside
`internal/config`) flagged 17 of 206 fields; most are false positives (consumed by
methods inside `config`: `usd_per_*`, `inference_backends`, `trace_sessions`,
`allow_userns`). One is real: **`memory_preflight_recall`** is read nowhere outside
its declaration (`config.go:688`; grep for `MemoryPreflightRecall` finds only
`config.go:681,688`), yet `docs/configuration.md:143` and `config.example.json:133`
say `false` disables the per-request memory injection. `retrieveMemoryContext` runs
unconditionally (`app.go:1164`). So the documented switch does nothing. [V]
Removed 0.4.2 (no-op, confirmed — field, config key, and docs entry deleted).

**Config keys in code but not documented** (CI only checks example→docs):
34 keys are absent from `docs/configuration.md`; I re-checked ten across all docs
and the example, all absent: `fusion_judge server_tools web_search_engine
auto_counsel counsel_max_per_session wf_final_review lsp_idle_timeout_seconds
assist_auto cache_prompt subagent_edit_verify_command`. The rest of the 34:
`cache_control work_dir host_work_dir docker_socket docker_start_timeout_sec
docker_stop_timeout_sec ssh_signing subagent_timeout_seconds
subagent_sync_timeout_seconds read_file_size_limit max_full_read_bytes
max_binary_write_bytes max_request_bytes oracle_max_tokens oracle_endpoint
mashura_timeout_seconds mashura_max_tokens_by_tool wf_briefing_max_bytes
subagent_mcp_servers lsp_index_timeout_seconds cached_input_usd_per_1m
cache_write_usd_per_1m fusion_max_tool_calls server_tool_max_calls`.
Several are documented under a different spelling (for example `work_dir` as
`--workdir`); not each one was checked.

## 4. Docs that describe behavior the code no longer has

| Doc | Claim | Code |
|---|---|---|
| `configuration.md:143`, `config.example.json:133` | `memory_preflight_recall: false` disables turn-entry retrieval | key never read; retrieval always runs (§3) [V] |
| `configuration.md:121` | `oracle_timeout_seconds` `0` → 300 and "built-in default (30min)" in the same row | code message says default 300s (`config.go:2535`); the row contradicts itself [V] |
| `configuration.md:134-135` | `oracle_enabled`, `oracle_model` are the keys | code treats `mashura_*` as canonical (§2.3) [V] |
| `ilm-stack/wakil-emitter.md:8-19`, `assist-contract.md` | `batch_size` (64), `flush_interval_s`/`ILM_FLUSH_S`, `output_byte_limit` | code has `batch_ms` 500, `max_output_bytes`; `ILM_FLUSH_S` read nowhere; `ILM_BATCH_SIZE` writes `BatchMS` [V] |
| `AGENTS.md` (architecture) | proxy = "Anthropic, OpenRouter"; package list | `proxy` kinds are `openai`/`ilm-proxy`; `buildinfo` and `styleguide` are missing from the list [V] |
| `CONTRIBUTING.md:35` | golangci-lint v2.10.0 | AGENTS.md says v2.11.1; CI pins the action, not the lint version [S, unverified which is current] |
| `hisn-ctl.md` | describes the hisn 0.2.1 control client | CHANGELOG 0.4.0 moved to profile mode for hisn ≥0.3.1 [S] |
| `docs/follow-ups.md` | open-items list | 3 items re-checked still open (peer-cred, mask rename, per-project risk_ack) [S]; `/prompt core` sentence is accurate (`config.go:361`) [V] |
| `sandbox-backends.md:724-729` | "no caller of `EnsureAccess` exists yet" | true today; it is a gap, not drift [V] |
| `Dockerfile.daemon:69,78`, `docker-compose.yml:54,79` | `WAKIL_EXEC_MODE` | deprecated alias (§2.1) [V] |
| Missing docs | daemon tokens/workspaces/backends/agents/web UI; `--http-addr`, `--tls-*` | exist in code, no page [V] |
| Historical | `design-note-a1-a6.md`, `design-note-hisn-031.md`, `reasoning-audit.md`, `truncation-audit.md` | shipped-design/audit notes; not checked line by line. Candidates to move under the gitignored `docs/archive/` [inferred — confirm] |

## 5. Hypothesis scorecard

| Hypothesis | Verdict | Evidence |
|---|---|---|
| auth tree is dead weight | Partly. `oidcresolver` is dead in production; the rest is live behind `wakil daemon` | `daemon_server.go:334` |
| Connect/sessionhost/remote/web | sessionhost is shared with the embedded path (KEEP); Connect+remote+web are daemon-only | import graph [V] |
| browser | Forced off under the default backend | `config.go:2340` |
| lsp | Healthy; keep | no deps, 1 dead method |
| workflow | Small, intrusive, overlaps skills; usage needed | §S16 |
| orregistry | Fine; test hooks leak into prod file | deadcode |
| ilm | Docs/code drift; emitter is a shadow-only default-off path | §S17 |
| styleguide | New, working; source dir untracked | §S18 |
| verify | Small, stable, keep | §S19 |
| scrub | Keep (security) | — |
| diag | Keep | 9 importers |
| policy | Reachable from headless only | `headless.go:296` |
| protoconv | Daemon-only; goes with S12b | 3 importers |
| proxy | Keep; AGENTS.md misdescribes | §S08 |
| memory tiers / async-ops | async-ops keep; plan hand-off broken by taint quarantine | 90 quarantined plans |
| exec_mode and aliases | Still wired into compose/Dockerfile; remove after migrating them | §2.1 |
| docker worktree executor | Second implementation beside `BackendExecutor` | §S10 |

## 6. Three largest removal candidates by LOC

Usage counts: **no evidence for any of them** (no grant; see inventory §0).

1. **Daemon stack, S12b (+S12a)** — 9,556 code lines (+ 8,708 test, 13,816
   generated, 1,632 web). Usage: no evidence.
2. **Skill auto-creation and templates, S05** — 4,612 code lines (+ 3,043 test).
   Usage: no evidence; default mode `on`.
3. **ILM emitter and assist, S17** — 1,825 code lines (1,647 + 178 agent) (+ 2,179
   test). Usage: no evidence.

Next in line: LSP 2,970 (recommended KEEP), Docker executor path 2,387 (KEEP/FOLD),
browser 940 (DEPRECATE; smallest in LOC, largest in image/dependency weight).

## Panel

Mashūra, one round, `mashura__review`, open question: "what would you cut from
this tool, and what is this inventory missing?" Papers attached: this file and
`inventory-2026-11.md`.

**Round status: NOT COMPLETED.** The panel's own header reads: "mashūra round NOT
COMPLETED — VOID reply from openrouter:anthropic/claude-fable-5.1; this is not a
review round and not 'no findings' — the document was not reviewed by every
reviewer; do not count it." Only one of two reviewers produced a valid review. Both
replies are reproduced below without edits; none of it is folded into the sheet
above. I did not re-run the panel (one round was requested; each call costs money).

```
[mashūra round NOT COMPLETED — VOID reply from openrouter:anthropic/claude-fable-5.1; this is not a review round and not "no findings" — the document was not reviewed by every reviewer; do not count it]

── openai/gpt-6-astra ──
## Overall judgment

**Good discovery sheet; not yet a safe deletion plan.** It identifies plausible simplifications, but weights LOC, defaults, and churn more heavily than capability loss, security, and migration cost.

**Evidence boundary:** The supplied documents report measurements and observations; I have not inspected the repository or reproduced them. Their `[V]` labels are the author’s verification, not mine. General technical knowledge recalled from training is explicitly identified below.

## Cuts I would reject or qualify

1. **S06: Do not exempt `plan/current` from quarantine.**  
   The reported 90 quarantined entries and failed retrieval demonstrate a hand-off problem in the observed memory store—not that bypassing taint is safe. Plans can carry malicious instructions from MCP/web results into future sessions. Prefer a provenance-preserving, explicitly approved hand-off. Staging is only an alternative if its lifetime and trust rules support cross-session retrieval; inspect `internal/staging` and test restart/resume behavior. TTL deletion needs a retention policy and preview, not an arbitrary purge of these 90 rows.

2. **S20: Do not remove policy merely because headless usage is unknown.**  
   The shown caller establishes reachability through headless, not dispensability. If headless remains supported, its consent controls need preservation or an equivalent replacement. Inspect `wiring/headless.go` and `agent/consent.go`; require denial-path tests.

3. **S16/S23: Skills are not established replacements for `/plan`, `/init`, or `/review`.**  
   The sheet explicitly says `/plan` hard-gates tools. **General knowledge:** natural-language workflows do not provide equivalent deterministic enforcement. User-specific global skills also are not necessarily shipped to everyone. Define parity—including phase gates, verification, errors, and fresh-install availability—before folding these commands.

4. **S12b: Zero observed daemon usage would not justify removal.**  
   Opt-in local traces cannot establish whether independent daemon deployments exist. Shipped Compose/image files are a concrete compatibility obligation. Separate web UI, TCP administration/auth, and Unix-socket remote operation rather than treating the stack as one cut. The ephemeral option needs socket ownership/permissions and peer-identity checks, not simply “no tokens.”

5. **S14: Browser deprecation is plausible, but its premise is overstated.**  
   The inventory says hisn is selected **when usable, otherwise Docker**. Therefore “disabled under the default backend” is not universal. Establish browser capability demand and replacement options. A lower-risk candidate is removing Chromium from the base image and offering an optional browser image; confirm feasibility from `Dockerfile` and browser startup tests.

6. **S10: Fold Docker only after a backend-parity test suite exists.**  
   Keeping Docker follows the stated requirement. But the proposal to migrate through `sandbox.Backend` while deleting its adapter is unclear: what replaces that adapter? Require parity for cancellation, exit status, mounts, worktrees, signing, process cleanup, and isolation. Cross-compiling Darwin does not prove Docker runtime support; CI definitions and actual integration results must confirm it.

## What I would cut first

- **Production-disabled OIDC internals**, subject to reproducing constructor reachability. Retain disabled RPC stubs initially if compatibility matters. **Protocol knowledge recalled from training:** protobuf `reserved` declarations do not reserve deleted RPC methods; deleting RPCs remains a compatibility concern. The exact `buf breaking` result depends on `buf.yaml`, its baseline, and command output.
- **Truly unreachable implementation leftovers**, after analyzing supported build configurations and tests. Do not delete test seams just because a run without `-test` flags them.
- **Obsolete config aliases**, through warnings, migration documentation, and a defined removal release—not merely after updating in-repo Compose files. External configurations remain affected.
- **ILM emitter/assist**, as an owner-approved candidate separate from `ilm-proxy`. Fix or explicitly retire the reported `ILM_BATCH_SIZE` misbinding while the feature remains supported.
- **Untracked build clutter**, especially the reported 5.8 GB `ilmql/target`, after confirming ownership and rebuildability. This is a concrete disk-cost opportunity, distinct from product simplification.

Also investigate overlapping web tools (`open_url`, `url_read`, `fetch_url`, multiple search routes), counsel commands/tool variants, and automatic skill-generation nudges. These are **consolidation candidates**, not demonstrated duplicates.

## Under-evidenced claims and contradictions

- **“Code-only” is inaccurate:** the inventory also uses live memory records, this session’s configuration, and global skills. Label these narrowly scoped operational observations; do not generalize them to the user population.
- **Dependency savings are provisional:** Appendix C says compile errors were ignored. Confirm savings with a compiling removal branch, `go mod tidy`, and before/after dependency graphs across supported builds.
- **Dead-code reasoning needs correction.** **Tool knowledge recalled from training:** Go’s deadcode analysis accounts for interface dispatch; the blanket claim that it cannot see “interface-only dispatch” is misleading. Pin the tool version instead of `@latest`, and record build tags/platforms.
- **“Largest test area” for context management contradicts the table**, which lists larger S01/S02/S04 areas.
- **“Styleguide: working” exceeds the evidence:** fresh-clone guide provisioning is expressly unverified.
- **“LSP is cheapest” ignores external language-server installation, process management, and compatibility.** No Go dependencies does not mean no operational dependencies.
- Low churn and doc drift do not demonstrate disuse. Likewise, trace, session history, emitter queues, and toolcache have not been shown to be semantically interchangeable.
- Moving historical docs into a **gitignored** archive risks losing maintained provenance rather than merely organizing it.

## Missing dimensions and acceptance criteria

Add a decision column for each of:

- **Security/trust boundaries:** daemon authentication, Docker socket and direct execution, MCP credentials/tool routing, hooks, browser execution, memory/skill poisoning, and telemetry retention. The reported root `master.key` warrants permissions, packaging, and tracking-history checks without exposing its contents.
- **Measured maintenance/CI cost:** test durations, flakes, skipped integration coverage, image size/build time, dependency vulnerabilities, ownership, and incident history. Test LOC and `TempDir` markers are not cost measurements.
- **User-facing risk and runtime cost:** capability lost, deployment/platform affected, default-on behavior, latency, model/token spend, tool-prompt overhead, data migration, and rollback.
- **Distribution correctness:** clean-clone builds, packaged guides/assets, daemon images, and optional runtime dependencies.

Before deletion, require an owner, explicit replacement/non-goal, compatibility window, migration/rollback plan, supported-platform tests, and measured savings. Usage evidence must state its denominator and coverage: **missing or disabled telemetry means unknown, not zero**. Appendix B also needs an upper date bound and validation against actual trace records; counts alone do not make raw traces safe to expose.

[mashūra: openrouter:openai/gpt-6-astra — ~17775 tokens sent, 1 part(s), ctx 1050000; reasoning 56 / answer 1465; finish_reason: stop; read through: Appendix C — verification notes]

── anthropic/claude-fable-5.1 ──
[VOID: part 1 of 1: reply cited "docs/inventory-2026-11.md — ## Appendix C — verification notes" as the last section read but the part ends at "Appendix C — verification notes" — the reviewer did not read it all]

[mashūra: openrouter:anthropic/claude-fable-5.1 — ~17775 tokens sent, 1 part(s), ctx 1000000; reasoning 5414 / answer 3679; finish_reason: stop]
```

### Author's note on the panel (not part of the panel text)

Checked against my own data, not folded into the sheet:
- "Largest test area" for S03 is **my error**: S03 has 6,956 test lines against
  22,892 for S02. Corrected in S03 above; the correction is the only edit made in
  response to the panel.
- "Code-only is inaccurate": fair. The inventory §0 already lists the memory
  listing, session configuration, and skills as non-trace substitutes, but the
  title says code-only; read it as "no trace usage evidence".
- The reviewer states it did not inspect the repository; its tool-knowledge points
  (deadcode and interface dispatch, protobuf `reserved`) are unverified by me.
