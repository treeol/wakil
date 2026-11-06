# Wakil inventory — 2026-11-04 (code-only; usage evidence unavailable)

Discovery only. No code, config, or doc other than this file and
`inventory-decisions.md` was changed. Repo state when measured: `master`,
clean worktree, 811 commits, HEAD `81319db` (2026-11-04, "hisn becomes the
default backend when usable (v0.4.0)").

Evidence tags: **[V]** = I ran the command or read the line myself;
**[S]** = reported by a discovery subagent and not re-verified;
**[inferred — confirm]** = my inference.

## 0. Usage evidence: what I could and could not count

**Grant: none was given, and this build has no way for the agent to ask for one.**

- `~/.local/share/wakil` does not exist inside the sandbox. `$HOME` there is
  `/run/hisn/home` and holds only `.config/go` and `go/`. `ls` reports "no such
  file"; `read_file`/`list_dir`/`search_files` on `…/wakil/traces`, `…/sessions`
  and `…/toolcache` all fail with `resolving path … exit status 1`. [V]
- The only host path that resolves is an exact toolcache spill path returned by
  a tool (the documented host-side intercept). I used those to read subagent
  digests and the repo map. They are not usage data and I counted nothing in them.
- There is no grant tool in my toolset. The grant machinery exists
  (`sandbox.GrantManager`, `exec.AccessGranter.EnsureAccess`) but
  `docs/sandbox-backends.md:724-729` says no caller exists, and a grep for
  `EnsureAccess` finds only the interface and its one implementation
  (`internal/exec/capabilities.go:81`, `backend_executor.go:819`). [V]
  `/grants` is a different feature: session approval grants for read-only
  tools (`docs/tui.md:50`).
- The route that does exist is a read-only bind in the user's sandbox config
  (`mounts_ro`, `docs/sandbox-backends.md:559`), applied at session start.
  I suggest binding only `~/.local/share/wakil/traces`, not the whole directory
  (session history and the memory DB hold conversation text).

**Files counted: none. Date range counted: none.** The intended window was
2026-09-05 .. 2026-11-04 (60 days). Every "usage" cell below is therefore
**no evidence (no grant)**.

**Even with the grant, the trace files could not answer most of the questions.**
`internal/trace/trace.go:25-60` [V] defines the whole record:

| asked for | in trace? |
|---|---|
| tool name | yes: `turn.tool_calls[].name` |
| model | yes, once per session: `store_header.model` (the configured model, not per turn) |
| provider | no |
| inference backend | partly: `turn.backend` is the `X-Ilm-Backend-Used` header |
| slash command | no |
| subagent type | no |
| sandbox backend | no (`backend` is the inference backend, not hisn/docker) |
| config keys set | no |

Tracing is also opt-in: `trace_sessions` defaults to false (`config.go:658`),
or `--trace`. Files are `<trace_dir>/<session_id>.jsonl`; the default dir is
`~/.local/share/wakil/traces` (`config.go:659,1557-1561`). Other sources would be
needed: `input_history` (one line per entry, no timestamps:
`internal/tui/history.go`), the sessionhistory DB (`sessions`/`turns` tables,
`internal/sessionhistory/store.go:154-175`), the memory DB, and the user's
config file. The recipe is in Appendix B.

**Substitutes I did use (none is a trace count):**

1. *Memory store via the memory tools.* Startup digest: 75 active durable, 73
   mid-tier, 103 pending proposals; namespaces `ctx` ×70, `arch` ×38, `done` ×17,
   `handoff` ×10, `plan` ×2. `memory_list` returned **90 quarantined
   `plan/current` entries**, 2026-09-07 .. 2026-11-04, while
   `memory_get("plan/current")` at the start of this session returned "not
   found". Mid-tier writes from a tainted session are quarantined and never
   become active, so the plan hand-off mechanism does not work in such sessions.
   [V, counted by hand from the listing]
2. *This session's own configuration* (n=1, not a frequency): four MCP servers
   (context7, invoicely, telegram-bridge, trello), `mashura__*` tools, `lsp_*`
   tools, a Go style guide, and the hisn sandbox are all active.
3. *Git churn* (commits touching a directory since 2026-09-05; 415 commits total).
   This measures maintenance effort, not use.
4. `list_skills` returned 16 active global skills (newest 2026-10-31).

## 1. Summary table

LOC = physical lines (`cat | wc -l`, blanks and comments included) of `*.go`,
split code / `_test.go`. Tests = count of `^func Test`. Row sums overlap by about 1.3k LOC
because `cmd/wakil/daemon*.go` (858) is in both S01 and S12 and some agent
globs overlap; Appendix A is exact per package.

| ID | Subsystem | Code | Test | #Tests | Last substantive commit | Churn 60d | Usage |
|---|---|---:|---:|---:|---|---:|---|
| S01 | Entry, wiring, prompts | 8,057 | 14,273 | 437 | 2026-11-04 | 22/54/15 | no evidence |
| S02 | Agent core loop, tool handlers, tools | 13,887 | 22,892 | 794 | 2026-11-04 | 196/15 | no evidence |
| S03 | Context management (collapse/compact/checkpoint/handoff) | 4,247 | 6,956 | 179 | 2026-10-25 [S: prompt] | in 196 | no evidence |
| S04 | Subagents, async ops, agent-side worktrees | 6,027 | 8,575 | 219 | 2026-11-04 | in 196 | no evidence |
| S05 | Skills (store tools, templates, auto-creation) | 5,472 | 3,926 | 179 | 2026-11-01 | in 196 | no evidence |
| S06 | Memory tiers, staging, session history | 5,840 | 7,699 | 246 | 2026-11-04 | 11/2/2 | see §0 (1) |
| S07 | Counsel / Mashūra | 4,175 | 6,609 | 214 | 2026-11-03 | 15 | no evidence |
| S08 | Model clients (proxy, orregistry) | 2,449 | 3,953 | 129 | 2026-11-02 / 2026-08-16 | 11/0 | no evidence |
| S09 | Sandbox backends (exec, sandbox/*, hisn) | 9,798 | 11,455 | 340 | 2026-11-04 | 45/37/35 | this session runs on hisn |
| S10 | ⊂ S09: Docker-only executor path | 2,387 | (in S09) | (in S09) | 2026-11-04 | in 45 | no evidence |
| S11 | TUI + session client | 9,339 | 11,268 | 472 | 2026-11-04 | 73/8 | no evidence |
| S12 | Daemon stack: Connect server, auth, remote client, stores, web | 9,556 + web 1,632 lines | 8,708 + ≤3,103 in cmd | 263 | 2026-10-26 (remote) … 2026-08-26 (web) | 8/5/4/3 | no evidence |
| S13 | Shared core (core, event, format, id, sessionhost, sqlstore, migrations) | 4,594 | 5,493 | 137 | 2026-10-21 | 19/10 | no evidence |
| S14 | Browser | 940 | 854 | 49 | 2026-11-01 (refactor-driven) | 6 | no evidence |
| S15 | LSP | 2,970 | 2,218 | 59 | 2026-09-20 | 6 | enabled in this session |
| S16 | `/plan` workflow | 1,240 | 733 | 26 | 2026-11-02 | 1 | no evidence |
| S17 | ILM emitter and assist | 1,647 | 2,179 | 56 | 2026-09-18 | 19 | no evidence |
| S18 | Style guides | 830 | 748 | 28 | 2026-10-26 | 3 | active in this session |
| S19 | Verify | 530 | 575 | 38 | 2026-08-15 | 0 | no evidence |
| S20 | Scrub, diag, trace, policy | 703 | 1,318 | 75 | 2026-09-13 … 2026-07-22 | 3/0/2/0 | no evidence |
| S21 | MCP client | 681 | 769 | 27 | in agent | in 196 | 4 servers live here |
| S22 | Config | 2,679 | 2,185 | 109 | 2026-11-04 | 44 | n/a |
| S23 | Small agent features (slash dispatcher, repomap/init/review, hooks, repostate, git tools, math) | 5,241 | ≥2,748 | ≥124 | various | in 196 | no evidence |
| — | Generated (api/gen) / proto | 13,816 / 1,291 | 0 | 0 | 2026-08-26 | — | — |

Whole repo, hand-written Go: 99,636 code lines, 126,134 test lines, 4,200 tests
(`cmd`, `internal`, `prompts`, `web`). Tests outnumber code 1.27:1. [V]

## 2. Row detail

Common facts [V]: go.mod has 14 direct modules. Dependents come from
`go list -f '{{.Imports}}'` (non-test imports). "Real I/O" counts test *files*
containing `httptest.`, `net.Listen/Dial`, `exec.Command`, `TempDir/MkdirTemp`,
`sql.Open/sqlite`, `t.Skip`.

### S01 Entry, wiring, prompts
- Purpose: binary entry, config → runtime assembly (TUI / headless / remote), embedded prompts.
- Packages: `cmd/wakil` 1,790/9,549/295; `internal/wiring` 6,197/4,655/140; `internal/buildinfo` 37/0/0; `prompts` 33/69/2 (+ `agent.txt`, `core.txt`, `extension.txt`).
- Surface: subcommands `version`, `doctor`, `daemon` (`main.go:27,30`; daemon [S]); flags (config.go): `base-url searxng-url google-cx mention-base host port api-key model exec sandbox output-mode sandbox-image image attach-image workdir host-workdir docker-sock ssh-signing config resume resume-id list-sessions all auto trace trace-dir daemon socket context-mode prompt-mode think`; daemon flags `--socket --ephemeral --shutdown-timeout --http-addr --tls-cert --tls-key --allowed-origins --master-key-file --generate-master-key --scrub-level` [S, `daemon.go:34-163`].
- Dependents: `cmd` ← none; `wiring` ← cmd, server/connect; `buildinfo` ← cmd, config, exec; `prompts` ← wiring.
- External deps: bubbletea (cmd).
- Tests real I/O: cmd: 29 files (httptest 7, net 5, tmpdir 14, skip 1); wiring: 22 files (httptest 3, net 1, tmpdir 10, skip 1).
- Docs: README, configuration.md.

### S02 Agent core loop, tool handlers, tools
- Purpose: turn loop, tool dispatch, consent, git/file/shell/web tools.
- Packages: `internal/agent` (whole package 38,088/50,002/1,654; 81 source and 136 test files; this row is the part outside S03–S06, S07-glue, S21, S23) 11,185/20,755/713; `internal/tools` 2,659/2,010/78; `internal/safe` 43/127/3.
- Surface: model-callable tools, from the `ExecuteToolCall` switch (`app.go:2626-2788` [S]): `finalize_goal run_shell open_url read_file read_file_full list_dir math_eval find_files search_files write_file write_binary_file edit_file delete_file move_file searxng_search url_read google_search fetch_url run_background check_pending wait_for_completion collapse_context kill_process read_process_log git_{status,diff,log,show,blame} dispatch_subagent(s)`, plus families listed in S04–S07, S14, S15, S21. 64 distinct tool-name literals under `tools/`, `browser/tools.go`, `lsp/tools.go`. [V]
- Dependents: `agent` ← cmd, server/connect, wiring; `tools` ← agent, tui, wiring; `safe` ← agent, browser, counsel, exec.
- Tests real I/O: agent 136 files (httptest 27, tmpdir 46, exec.Command 3, sqlite 2, skip 12).
- Docs: tools.md (257 lines), features.md, README.

### S03 Context management
- Purpose: long-session mode — collapse, compaction, checkpoints, hand-off, context limits.
- Files: `collapse.go` 1,521, `compact.go` 940, `checkpoint.go` 713, `handoff.go` 566, `ctxlimit.go` 507; tests 6,956/179.
- Surface: tool `collapse_context`; `/compact /handoff /keep /maxctx /rewind`; keys `context_mode` (default `long-session`), `compact_at*`, `keep_bytes*`, `context_tail_turns`, `context_collapse_min_bytes`, flag `--context-mode`.
- Docs: long-session-mode.md (148 lines), truncation-audit.md (136).

### S04 Subagents, async ops, worktrees
- Purpose: bounded child agents (discovery/edit/tools tiers), op registry with wake/wait, git-worktree isolation for parallel edit children.
- Files: `subagent*.go` 2,394 (tests 4,655/123); `async*.go`, `idle_wake.go` 2,372 (2,449/60); `worktree.go`, `process_alive_*.go` 1,261 (1,471/36). Docker-side worktree executor is `exec/docker_worktree.go` (S10).
- Surface: tools `dispatch_subagent(s) check_pending wait_for_completion`; `/subagent /submodel /maxpar`; keys `max_parallel_subagents subagent_timeout[_seconds] subagent_sync_timeout[_seconds] subagent_mcp_servers subagent_edit_verify_command`.
- Hypothesis "docker worktree executor": two implementations of the `WorktreeHost` seam exist — `DockerExecutor` + `dockerWorktreeExecutor` (232 lines) and `BackendExecutor.NewWorktreeExecutor` (`backend_executor.go:786-815`). [V]
- Docs: workflows.md, continuous-mode.md, tools.md.

### S05 Skills
- Purpose: global skill store tools, template skills, auto-creation pipeline.
- Files (all in agent): store tools `skills.go` + `skill_handlers.go` 792; templates `skill_template.go` + `skill_template_handlers.go` 2,400; auto-creation `skill_creation*.go`, `skill_create.go`, `skill_suggestion.go` 2,212; tests 3,926/179. Store itself: `memory/store_skills.go`.
- Surface: 10 tools (list/load/search/history/save/update/forget skill; apply/capture/check skill template); `/skills /skill-create /skill-creation`; config `skill_creation` (mode default **`on`**, `skill_creation.go:102-107`).
- Two forms: `/skill-create` (332-line manual flow) beside `/skill-creation` (`commands.go:1340-1359`).
- Docs: tools.md, tui.md, configuration.md; templates and auto-creation have no dedicated page.

### S06 Memory tiers, staging, session history
- Packages: `memory` 1,715/2,140/45 (`store.go`, `store_skills.go`; sqlite); `staging` 738/2,414/79; `sessionhistory` 548/472/19 (sqlite, FTS5, `sessions`+`turns`); agent glue (`memory_tools correction retrieval sessionhistory_bridge learn`) 2,465/2,342/97; `staging.go`, `staging_tools.go` 374/331/6.
- Surface: tools `memory_{put,promote,reject,get,search,list,forget,promote_from_staging}`, `staging_{put,get,delete,list,get_many}`; `/remember /recall /resume /sessions /session /learn /taint`; key `memory_preflight_recall`.
- Tiers: mid (TTL), durable (proposed → promoted), quarantine for tainted sessions, correction capture.
- Dependents: memory ← agent, wiring; staging ← agent, wiring; sessionhistory ← agent, wiring.
- External deps: `modernc.org/sqlite` (shared with tokenstore and sqlstore).
- Tests real I/O: memory 6 files with tmpdir, 2 sqlite; staging 5 files (net 1, exec.Command 2, skip 2).
- Docs: memory.md (310), staging.md (163).

### S07 Counsel / Mashūra
- Packages: `counsel` 2,609/3,765/124 (`oracle.go review.go stream.go modellimit.go client.go`); agent `mashura*.go` 1,566/2,844/90.
- Surface: `mashura__{review,debug,decide,check}` plus alias `oracle__ask` (`app.go:2705`); `/mashura` (`commands.go:1255`), `/counsel` (`commands.go:1215`); ~20 keys `oracle_* mashura_* auto_counsel counsel_max_per_session wf_oracle_mode`.
- Tests real I/O: counsel 7 files, httptest 4.
- Docs: reasoning-audit.md (437), truncation-audit.md, configuration.md.

### S08 Model clients
- Packages: `proxy` 2,078/3,627/114 (endpoint kinds `openai`, `ilm-proxy`, `client.go:393-394`; constants duplicated in config on purpose, `client.go:390-392`); `orregistry` 371/326/15 (OpenRouter registry + pricing).
- Dependents: proxy ← agent, browser, cmd, core/format, sessionclient, lsp, remote, tools, tui, wiring, workflow; orregistry ← agent, counsel.
- Tests real I/O: proxy httptest 8 of 13 files; orregistry httptest 2 of 2.
- Docs: endpoints.md (79), reasoning-audit.md.
- Note: AGENTS.md says "proxy client (Anthropic, OpenRouter)"; the proxy has only `openai` and `ilm-proxy` kinds. Whether Anthropic traffic is counsel-only was not verified. [inferred — confirm]

### S09 Sandbox backends
- Packages: `exec` 4,437/4,873/181; `sandbox` 617/139/7; `sandbox/fake` 136/0/0; `sandbox/hisn` 3,822/5,739/139; `sandbox/hisn/ctl` 594/571/8; `sandbox/project` 192/133/5.
- Surface: `--sandbox` (`docker|hisn|direct`), `--exec` (deprecated), `--sandbox-image`, `--image`, `--docker-sock`, `--ssh-signing`, `--workdir`, `--host-workdir`; `sandbox.*` and `sandbox.hisn.*` keys; `.wakil/sandbox.json`; `/sandbox /backend /grants`; `wakil doctor`.
- Default: `sandbox.backend` unset → hisn when usable (≥0.3.1 and `hisn check` passes), else docker (`configuration.md:325`).
- Dependents: sandbox ← agent, config, exec, fake, hisn; hisn ← cmd, project, wiring; project ← wiring. The tool layer must not import a backend (`internal/agent/no_backend_import_test.go`).
- External deps: `BurntSushi/toml` is imported only by `sandbox/hisn` yet is marked `// indirect` in go.mod:23 (hygiene bug). [V]
- Tests real I/O: hisn 21 files (net 4, exec.Command 8, tmpdir 9, skip 6 — skip unless `hisn` is installed); exec 22 files (exec.Command 7, tmpdir 18, skip 8).
- Docs: sandbox-backends.md (823), design-note-hisn-031.md (293), hisn-ctl.md (179).

### S10 Docker-only executor path (subset of S09)
- Files: `exec.go` 1,771, `docker_worktree.go` 232, `docker_backend.go` 75, `seccomp_profile.go` 92, `signing.go` 217 = 2,387 code; plus `Dockerfile` (sandbox image, installs chromium, `Dockerfile:64`), `docker/entrypoint.sh`.
- Selected by `switch cfg.ExecMode` at `wiring/bootstrap.go:454` and `wiring/hisn_executor.go:23`.
- The `dockerBackend` adapter (`docker_backend.go:22`) has no non-test caller by grep. [V grep; inferred — confirm]

### S11 TUI and session client
- Packages: `tui` 8,571/11,035/464 (64 files; no import of `agent`, enforced by `no_agent_import_test.go` [S]); `core/sessionclient` 768/233/8.
- Surface: 50 entries in `allTUICommands` (`tui/complete.go:72-123`); TUI-local `/info /queue /ask` [S]; `Ctrl+O`/`F2` info panel.
- External deps (leave only with the TUI): bubbles, bubbletea, glamour, lipgloss, plus 13 transitive (chroma, goldmark, bluemonday, …).
- Tests real I/O: 42 files (httptest 2, exec.Command 2, skip 5).
- Docs: tui.md (170).

### S12 Daemon stack
Everything below is reachable only from `wakil daemon` / `wakil --daemon` [V by import graph], except where noted.
- Auth tree (code/test/#tests): `auth` 200/211/14; `apitoken` 211/459/16; `jointoken` 341/364/10; `oidcresolver` 298/361/13; `peercred` 106/112/3; `tokenresolver` 269/377/10; `tokenstore` 620/782/23 (sqlite).
- `server/connect` 2,943/3,374/91; `remote` 2,267/685/10; `crypto` 327/397/22; `store/agentstore` 231/386/14, `backendstore` 246/497/16, `workspacestore` 114/283/13; `protoconv` 525/420/8; `cmd/wakil/daemon*.go` 858 lines; `web/` app.js+index.html+styles.css 1,632 lines, no tests; `api/gen` 13,816 generated; `api/proto` 1,291.
- RPC surface [S, `api/proto/wakil/v1alpha1/`]: AgentService (6 rpcs), AuthService (join/api tokens, WhoAmI, Logout, OIDC), BackendService (CRUD), EventService (3), SessionService (9), SessionStateService (20+), SystemService, WorkspaceService (CRUD).
- Gates [S]: `--ephemeral` disables tokenstore/auth/backends/workspaces/agents; backend RPCs are `Unimplemented` unless `--master-key-file`; TCP only with `--http-addr`.
- OIDC: `oidcresolver.New(tokenStore, nil, oidcresolver.Config{})` (`daemon_server.go:334`) passes a nil validator, so it returns "disabled" in every production path; `OIDCConfig{}` appears only in `cmd/wakil/p4e_test.go`. [V]
- Remote client parity: `/mcp /plan /verify /learn /image /handoff /remember /recall` are unavailable or no-ops remotely (`remote/facade.go:29-58` [S]).
- External deps that leave if all of S12 goes: `connectrpc.com/connect`, `google.golang.org/protobuf` (verified by removing the dirs from a copy and re-listing the module graph). `uuid`, `sqlite`, `crypto/*` stay (used elsewhere).
- Tests real I/O: server/connect httptest 5, sqlite 5; auth/* tmpdir+sqlite; peercred net 1; remote net 1; cmd daemon-related tests up to 3,103 lines (files mentioning daemon/tokenstore/connsvc/apitoken — upper bound).
- Docs: README (daemon, OIDC mentions); `sandbox-backends.md`; `remote-provisioning.md` (150). No page describes the auth tree, tokens, workspaces, backends, agents, or the web UI. `--http-addr` and `--tls-*` appear in no docs file by grep. [V]
- Old-path residue: `defaultSocketPath` was duplicated in a removed `cmd/wakild` and consolidated (`daemon_server.go:521-526` [S]). `Dockerfile.daemon:95` correctly runs `wakil daemon`.

### S13 Shared core
- Packages: `core` 489/132/5; `core/event` 1,108/714/31; `core/format` 54/96/5; `core/id` 162/112/4; `core/sessionhost` 1,984/2,961/63; `sessionhost/sqlstore` 350/877/18; `sessionhost/storetest` 311/0/0; `store/migrations` 136/601/11.
- Dependents: event ← 15 packages; sessionhost ← cmd, server/connect, wiring (the embedded path uses it too); id ← sessionhost, wiring.
- Dead: 7 of `core/id`'s constructors have no non-test caller (§ dead code).

### S14 Browser
- `internal/browser` 940/854/49; tools `browser_{navigate,screenshot,viewport,click,eval,text,html,reduced_motion}`; gate `browser_enabled` default **false**, and **forced off under the hisn backend** (`config.go:2340-2346`) — hisn is now the default backend.
- External deps that leave: `chromedp/chromedp`, `chromedp/cdproto`, indirect `go-json-experiment/json`. [V by copy simulation]
- Image weight: `Dockerfile:64` installs chromium plus a workaround block (`Dockerfile:97-114`).
- Tests: 4 files, httptest 1, skip 1. Docs: configuration.md:142, features.md, tools.md, sandbox-backends.md, README.

### S15 LSP
- `internal/lsp` 2,970/2,218/59, pure-Go JSON-RPC, no external deps; tools `lsp_{definition,references,hover,symbols}`; gate `lsp_enabled` default false; also feeds `/repomap --symbols` (`agent/symbolmap.go` 398). Tests: 8 files, 1 skip.
- Docs: configuration.md:38,138, features.md, tools.md.

### S16 `/plan` workflow
- `internal/workflow` 610/733/26 (phase machine Gather→Plan→Review→Present→Implement→Done, sentinels, briefing assembly) and agent `workflow_engine.go` 630 (no tests of its own). `/plan`; keys `wf_*`, `mashura_mode`.
- Overlap: the user's global skills `task-workflow` and `trello-card-workflow` describe the same verify→plan→review→implement loop (`list_skills`). [V observation]
- Docs: workflows.md (40 lines).

### S17 ILM emitter and assist
- `internal/ilm` 1,469/1,951/55 + agent `assist.go` 178/228. Config `ilm_stack` (`mode` default `off`, "zero network calls", `config.go:729-742`). Docs: `docs/ilm-stack/*` (191 lines), configuration.md:193.
- Drift: see decisions doc §Docs.

### S18 Style guides
- `internal/styleguide` 389/220/9 + agent `styleguides.go` 441; keys `style_guides_enabled` (default false), `style_guide_paths`. `google-style-guides/` at repo root (24K) is **not tracked** (worktree is clean, so it is ignored). Docs: configuration.md only.

### S19 Verify
- `internal/verify` 530/575/38, pure functions (parse `go test` failures, safe re-run suggestion, `DetectCommands`). Used by `agent/verify.go`, `init_cmd.go`; `/verify`. Zero commits since 2026-08-15. Docs: configuration.md, tui.md.

### S20 Scrub, diag, trace, policy
- `scrub` 138/430/32 (secret redaction; also used by `sqlstore` and `cmd`); `diag` 119/214/5 (logging off stderr); `trace` 177/259/8 (opt-in JSONL, schema in §0); `policy` 269/415/30 (declarative consent profile; loaded only in `wiring/headless.go:296`, read by `agent/consent.go:167`).
- Telemetry overlap: four records of "what happened" — trace JSONL, ILM emitter queue, sessionhistory DB, toolcache spill. [inferred — confirm]
- Docs: configuration.md, features.md (trace); policy in 6 design/ops docs but no page of its own.

### S21 MCP client
- `agent/mcp_manager.go` 681/769/27. Config `mcp_servers`, `subagent_mcp_servers`; `/mcp`. Default route for unknown tool names (`app.go` default case → `handleMCPTool`).
- External deps that leave if removed: `modelcontextprotocol/go-sdk`, `google/jsonschema-go`, `segmentio/asm`, `segmentio/encoding`, `yosida95/uritemplate/v3`, `golang.org/x/oauth2`, `x/sync`, `x/time`. [V by copy simulation]

### S22 Config
- `internal/config` 2,679/2,185/109. 206 JSON-tagged fields, 70 env-var names, 32 flag definitions. Example file + 352-line doc. Deprecated aliases: see decisions doc §Two forms.

### S23 Small agent features
- Slash dispatcher (`commands.go` 1,748 + `tui_cmds.go`, `cmd.go`, `sandbox_cmd.go`) 2,135 code, 46 test lines; repomap/symbolmap/init/review 1,419/1,390/66; hooks 316/536/26; repostate 625/406/18; git tools 327/370/12; `handle_math.go` 419 (`math_eval`).

## Appendix A — exact per-package LOC (code / test / #Test), real-I/O test files

```
api/gen/wakil/v1alpha1 11381/0/0        api/gen/.../wakilv1alpha1connect 2435/0/0
cmd/wakil 1790/9549/295                 internal/agent 38088/50002/1654
internal/auth 200/211/14                internal/auth/apitoken 211/459/16
internal/auth/jointoken 341/364/10      internal/auth/oidcresolver 298/361/13
internal/auth/peercred 106/112/3        internal/auth/tokenresolver 269/377/10
internal/auth/tokenstore 620/782/23     internal/browser 940/854/49
internal/buildinfo 37/0/0               internal/config 2679/2185/109
internal/core 489/132/5                 internal/core/event 1108/714/31
internal/core/format 54/96/5            internal/core/id 162/112/4
internal/core/sessionclient 768/233/8   internal/core/sessionhost 1984/2961/63
internal/core/sessionhost/sqlstore 350/877/18   .../storetest 311/0/0
internal/counsel 2609/3765/124          internal/crypto 327/397/22
internal/diag 119/214/5                 internal/exec 4437/4873/181
internal/ilm 1469/1951/55               internal/lsp 2970/2218/59
internal/memory 1715/2140/45            internal/orregistry 371/326/15
internal/policy 269/415/30              internal/protoconv 525/420/8
internal/proxy 2078/3627/114            internal/remote 2267/685/10
internal/safe 43/127/3                  internal/sandbox 617/139/7
internal/sandbox/fake 136/0/0           internal/sandbox/hisn 3822/5739/139
internal/sandbox/hisn/ctl 594/571/8     internal/sandbox/project 192/133/5
internal/scrub 138/430/32               internal/server/connect 2943/3374/91
internal/sessionhistory 548/472/19      internal/staging 738/2414/79
internal/store/agentstore 231/386/14    internal/store/backendstore 246/497/16
internal/store/migrations 136/601/11    internal/store/workspacestore 114/283/13
internal/styleguide 389/220/9           internal/tools 2659/2010/78
internal/trace 177/259/8                internal/tui 8571/11035/464
internal/verify 530/575/38              internal/wiring 6197/4655/140
internal/workflow 610/733/26            prompts 33/69/2        web (embed.go) 11/0/0
```

Last substantive commit per package (heuristic: newest commit touching the
package's non-test `.go` files whose subject does not start with
refactor/cleanup/lint/coverage/fmt/comment/test/ci/chore/deps/docs):
`policy` 2026-07-20; `verify` 2026-08-15; `diag` `safe` `orregistry` 2026-08-16;
`web` `auth/jointoken|oidcresolver|peercred|tokenresolver` `core/id` `core/format`
`protoconv` `store/migrations` `sqlstore` 2026-08-26; `auth/apitoken|tokenstore`
2026-09-18; `crypto` `trace` `scrub` `store/*store` 2026-09-13; `lsp` 2026-09-20;
`ilm` 2026-09-18; `core` 2026-09-06; `prompts` 2026-10-15; `core/sessionclient`
2026-10-20; `core/sessionhost` 2026-10-21; `styleguide` `remote` 2026-10-26;
`memory` `browser` `buildinfo` 2026-11-01; `proxy` `tools` `workflow` 2026-11-02;
`counsel` `sandbox/fake` `sandbox/project` 2026-11-03; everything else 2026-11-04.

Real-I/O test files per package (httptest/net/exec.Command/tmpdir/sqlite/skip of
total files): cmd 7/5/0/14/0/1 of 29; agent 27/0/3/46/2/12 of 136; exec
0/0/7/18/0/8 of 22; hisn 0/4/8/9/0/6 of 21; tui 2/0/2/2/0/5 of 42; wiring
3/1/0/10/0/1 of 22; proxy 8/0/0/2/0/0 of 13; server/connect 5/0/0/0/5/0 of 9;
memory 0/0/0/6/2/0 of 6; staging 0/1/2/3/0/2 of 5; counsel 4/0/0/0/0/0 of 7;
ilm 3/0/0/2/0/1 of 5; orregistry 2/0/0/0/0/0 of 2. Pure-unit (no markers):
core/*, diag, safe, scrub, verify, workflow, protoconv, sandbox, fake.

## Appendix B — counting recipe for a later pass (not run)

With `~/.local/share/wakil/traces` readable:

```sh
D=~/.local/share/wakil/traces; SINCE=2026-09-05
jq -r --arg s "$SINCE" 'select(.type=="turn" and .ts>=$s) | .tool_calls[]?.name' $D/*.jsonl | sort | uniq -c | sort -rn
jq -r --arg s "$SINCE" 'select(.type=="store_header" and .ts>=$s) | .model' $D/*.jsonl | sort | uniq -c | sort -rn
jq -r --arg s "$SINCE" 'select(.type=="turn" and .ts>=$s) | .backend // empty' $D/*.jsonl | sort | uniq -c   # inference backend only
```

Slash commands: `~/.local/share/wakil/input_history` (no dates; whole-history counts
only) or the sessionhistory DB `turns` table. Subagent type, sandbox backend, and
config keys set are **not in the trace schema**; the `trace.Record` would need
fields, or the user's config must be diffed against `DefaultConfig`
(`config.go:1120`). Output of these commands is counts only; do not print
conversation text.

## Appendix C — verification notes

- One subagent claimed `prompt_mode` does not exist; that is wrong
  (`config.go:361,2084`). The `docs/follow-ups.md:24` sentence is accurate.
- Subagent runs were budget-truncated: remote/daemon, agent, config, tools-bundle,
  and TUI digests were partial. LOC, tests, imports, module ownership, dead code,
  config-field checks, and trace schema are my own measurements; items tagged
  [S] are not re-verified.
- Dead code: `golang.org/x/tools/cmd/deadcode -test ./...` (27 unreachable) and
  without `-test` (102). Module-leave sets: remove dirs from a `git ls-files`
  copy, re-run `go list -deps`, diff (compile errors ignored; this measures
  import-graph ownership).
- Not covered: `docs/` claims were spot-checked, not audited line by line;
  `golangci-lint` and `go vet` were not run (nothing was changed).
- `master.key` exists at the repo root (gitignored, `.gitignore:61`). I did not
  open it. `ilmql/` (5.8 GB, a Rust `target/` dir, excluded via
  `.git/info/exclude`) and `tmp/`, `dist/`, `wakil` (binary) are untracked clutter.
