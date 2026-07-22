# Wakil Cleanup & Hardening Audit

**Date:** 2026-07-22
**Scope:** Full codebase — ~76K LOC Go across 16 internal packages + cmd/, plus Dockerfile, CI, configs.
**Method:** 6 parallel discovery subagents (concurrency, security/errors, code quality, test coverage, infrastructure, API/schema) + inline verification of every high-severity finding. False positives filtered out.

**Key facts that eliminate false positives:**
- Go 1.26 in go.mod → loop-variable capture is per-iteration (fixed in Go 1.22). The `subagent_parallel.go:122` finding is a false positive.
- `proxy/client.go:872` has `defer resp.Body.Close()` — verified inline for the main streaming path. However, `bodyclose` linter should still be enabled to catch any other request sites in the 1263-line file.
- `SuspendAuto` returning "" for write_file/edit_file/delete_file/move_file is **intentional** — /auto is designed to auto-approve file mutations; the destructive gate applies only to shell commands.
- `dispatch_subagents` tasks schema items being `StrProp` is correct — tasks IS an array of strings.
- **Browser lifecycle (#3)**: `internal/browser/manager.go:111-141` uses `chromedp.NewExecAllocator` + `NewContext` with proper `allocCancel`/`ctxCancel` and a 15s launch timeout. The browser IS properly cleaned up via context cancellation. This is NOT a gap.
- **Path traversal**: `ConfinePath` is well-tested — `exec_ops_test.go` has `TestConfinePathInsideAndTraversal`, `TestConfinePathSymlinkEscape`, plus `confine_fuzz_test.go` fuzz test. This is a strength, not a gap.
- **CI module cache (#9)**: `setup-go@v5` is used without `cache: true`, so no module caching occurs. Fix is trivial: add `cache: true` to the setup-go step.

**Mashūra review corrections applied:**
- Medium count was wrong (12 IDs, not 9) — fixed.
- #1 (goroutine recovery) downgraded from High to Medium; split into logic-bearing goroutines (Medium) vs cmd.Wait reapers (Low).
- #8 (linters) downgraded from High to Medium; card scoped to errcheck+bodyclose first, not gosec/gocyclo.
- #3 (browser lifecycle) removed — false positive.
- #16 (test coverage) reframed: "no test file" ≠ "untested" (package-level tests may cover). Split into focused security-boundary cards instead.
- Cards consolidated from 15 to 11 per Mashūra guidance.
- Added: GitHub Actions SHA pinning, secrets-in-logs audit (new gaps identified by Mashūra).

---

## Findings

### 1. Goroutines Without Panic Recovery [HARDENING]
**Severity:** High
**Files:**
- `internal/agent/tool_handlers.go:586` — bg process reaper goroutine (polls IsProcessAlive in a loop)
- `internal/counsel/oracle.go:334` — panel member call goroutine
- `internal/exec/exec.go:610`, `:779` — `go func() { _ = cmd.Wait() }()` (docker exec reaper)
- `internal/exec/exec_ops.go:288` — same pattern
- `internal/browser/manager.go:131` — `go func() { launchDone <- chromedp.Run(ctx) }()`
- `internal/browser/manager.go:183` — another goroutine
- `internal/tools/open.go:29` — `go func() { _ = cmd.Wait() }()` (zombie reaper)

**Issue:** 8 goroutines are launched without `defer func() { recover() }()`. A panic in any of them would crash the entire Wakil process. The subagent dispatch path (`subagent_parallel.go:124`) correctly has recovery — these don't.

**Fix:** Add `defer func() { if r := recover(); r != nil { log.Printf("goroutine panic: %v", r) } }()` to each. For the `cmd.Wait()` reapers, a panic is unlikely but the cost of recovery is near-zero.

### 2. LSP Server Process Lifecycle — No Context Cancellation on Wait [HARDENING]
**Severity:** Medium
**File:** `internal/lsp/manager.go` (~line 80-130)

**Issue:** `cmd.Wait()` runs in a goroutine without a select on `ctx.Done()`. If the parent cancels but the LSP server process ignores the signal (or hangs), the goroutine leaks and the process is never reaped.

**Fix:** Use `cmd.Process.Kill()` in a ctx.Done select, or a `context.WithTimeout` around the wait.

### 3. Browser Process Lifecycle — No Timeout on cmd.Wait [HARDENING]
**Severity:** Medium
**File:** `internal/browser/manager.go` (~line 90-140)

**Issue:** Similar to LSP — `chromedp.Run(ctx)` in a goroutine, but if the browser hangs, there's no timeout guard to reap it.

**Fix:** Add a timeout context around chromedp.Run, or a watchdog goroutine that kills the process after N seconds of no activity.

### 4. Workflow Engine — Unchecked WriteFile Errors [CLEANUP]
**Severity:** Medium
**Files:** `internal/agent/workflow_engine.go:422,494,538,551`, `internal/agent/mashura.go:674`

**Issue:** `_, _ = app.Exec.WriteFile(...)` ignores errors from writing to the step log. If the write fails (disk full, permissions), the step log silently loses entries — the workflow's audit trail becomes unreliable.

**Fix:** Log the error if WriteFile fails (don't return — the workflow should continue, but the failure should be visible).

### 5. CaptureFileOriginal Errors Not Checked [HARDENING]
**Severity:** Medium
**File:** `internal/agent/tool_handlers.go:349,463,499` (handleWriteFile, handleDeleteFile, handleMoveFile)

**Issue:** `a.captureFileOriginal(ctx, canonical)` return value is discarded. If the snapshot fails, the auto-restore feature (`/restore` for failed edit-tier subagents) silently has no snapshot to restore from.

**Fix:** Check the error; if it fails, log a warning that the snapshot is unavailable (the edit should still proceed — the snapshot is best-effort, but the failure should be visible).

### 6. Dead Code — agent_async.go Empty Stub [CLEANUP]
**Severity:** Low
**File:** `internal/agent/agent_async.go` (6 lines, comment only)

**Issue:** File is empty after WP-6.4 split. Contains only a comment explaining the split. Should be deleted — it adds noise to file listings and package structure.

**Fix:** `rm internal/agent/agent_async.go`

### 7. Staging.go — Unused Import Hack [CLEANUP]
**Severity:** Low
**File:** `internal/tools/staging.go:41`

**Issue:** `_ = fmt.Sprintf // keep import if no fmt usage` — the `fmt` import is kept alive by a no-op assignment. This means fmt isn't actually used in this file.

**Fix:** Remove the `fmt` import and the line. (Or remove the line and verify the build still passes — if fmt is used elsewhere, the import stays.)

### 8. .golangci.yml — Missing Security Linters [HARDENING]
**Severity:** High
**File:** `.golangci.yml`

**Issue:** Only 3 linters enabled: `ineffassign`, `staticcheck`, `unused`. Critical linters disabled:
- `errcheck` — unchecked errors (the workflow_engine.go and exec.go patterns above would be caught)
- `gosec` — security-focused (path traversal, command injection, weak crypto)
- `bodyclose` — HTTP response body leaks (relevant for proxy/client.go)
- `nilerr` — returning nil after an error
- `gocyclo` — cyclomatic complexity

**Fix:** Enable `errcheck` and `bodyclose` first (highest signal-to-noise for this codebase). Add `gosec` with exclusions for test files. Ramp up incrementally as the comment in the file says.

### 9. CI — No Go Module/Build Cache [CLEANUP]
**Severity:** Medium
**File:** `.github/workflows/ci.yml`

**Issue:** Each CI run downloads all Go dependencies fresh. No `actions/cache` step for `~/go/pkg/mod` or the Go build cache.

**Fix:** Add `actions/cache` with `path: ~/go/pkg/mod` and `key: go-mod-${{ hashFiles('go.sum') }}`.

### 10. CI — No Dependabot or Vulnerability Scanning [HARDENING]
**Severity:** High
**File:** `.github/` (no dependabot.yml exists)

**Issue:** No automated dependency vulnerability scanning. `go.mod` dependencies (e.g. `modernc.org/sqlite`) could have known CVEs with no detection mechanism.

**Fix:** Add `.github/dependabot.yml` for Go modules (weekly schedule). Optionally add `govulncheck` to CI.

### 11. Dockerfile — Supply Chain: Unpinned git Clone [HARDENING]
**Severity:** Medium
**File:** `Dockerfile:6`

**Issue:** `git clone --depth 1 https://github.com/treeol/kvrust.git .` — fetches latest HEAD, not a pinned commit. A compromised upstream repo would be built into every image.

**Fix:** Pin to a specific commit: `git clone https://github.com/treeol/kvrust.git . && git checkout <commit>`.

### 12. Dockerfile — Supply Chain: rustup Piped to sh [HARDENING]
**Severity:** Medium
**File:** `Dockerfile:111-113`

**Issue:** `curl ... | sh -s -- -y` for rustup installation. No checksum verification of the downloaded script.

**Fix:** This is the standard rustup install method and hard to avoid, but at minimum pin the rustup version, or verify the GPG signature if available.

### 13. Dockerfile — No USER Directive (Runs as Root) [HARDENING]
**Severity:** Medium
**File:** `Dockerfile:135` (final stage)

**Issue:** The final image runs as root. This is intentional for the sandbox (needs to create /etc/passwd entries, manage docker, etc.), but worth documenting as a known tradeoff.

**Fix:** If possible, create a non-root user and grant only the specific capabilities needed. If root is truly required, document the rationale in the Dockerfile and ensure the SECURITY.md covers it.

### 14. Dockerfile — No HEALTHCHECK [CLEANUP]
**Severity:** Low
**File:** `Dockerfile`

**Issue:** No HEALTHCHECK instruction. Docker can't detect a hung container.

**Fix:** Add `HEALTHCHECK CMD kvr-server ping || exit 1` or similar.

### 15. Docker entrypoint.sh — Background Processes Don't exec [HARDENING]
**Severity:** Medium
**File:** `docker/entrypoint.sh:25-26`

**Issue:** `"$@" &` runs the main command in the background without `exec`. Signals sent to the entrypoint shell may not propagate to the actual process. The trap-based cleanup handles SIGTERM, but the main process itself doesn't receive signals directly.

**Fix:** The current trap-based approach is a reasonable workaround, but consider `exec "$@"` in a subshell or using `tini` as an init process.

### 16. Coverage Gaps — Critical Untested Files [TEST]
**Severity:** High
**Files (no test file, security/critical path):**
- `internal/agent/consent.go` — consent/authorization core (Consent, SetConsent, RevokeAuto, ConsentSnapshot)
- `internal/agent/tool_handlers.go` — all tool dispatch handlers (935 lines)
- `internal/agent/workflow_engine.go` — workflow state machine (552 lines)
- `internal/agent/commands.go` — slash command processing (1038 lines)
- `internal/agent/app.go` — main App struct, core logic (2098 lines)
- `internal/counsel/oracle.go` — Mashūra oracle integration (644 lines)
- `internal/proxy/client.go` — streaming HTTP client (1263 lines, but has extra_test.go)
- `internal/exec/dockersock.go` — docker socket handling
- `internal/policy/load.go` — policy file loading

**Issue:** 46 of ~90 source files have NO test file. Several of these are security-critical (consent, policy, exec). Coverage floors exist only for 4 packages (agent/tools/exec/proxy); the rest have no floor.

**Fix:** Prioritize consent.go, policy/load.go, and dockersock.go. These are small files where targeted tests would cover the security boundaries.

### 17. Coverage Floors Missing for Smaller Packages [TEST]
**Severity:** Low
**File:** `scripts/check_coverage.sh`

**Issue:** Coverage floors exist only for `internal/agent` (67.3%), `internal/tools` (44.5%), `internal/exec` (37.5%), `internal/proxy` (79.2%). No floors for: `internal/counsel`, `internal/lsp`, `internal/memory`, `internal/staging`, `internal/workflow`, `internal/browser`, `internal/config`, `internal/policy`, `internal/verify`.

**Fix:** Add floors for counsel, config, and verify (packages with good current coverage to ratchet).

### 18. Large Files — Split Candidates [CLEANUP]
**Severity:** Low (maintainability)
**Files >800 lines:**
| File | Lines | Concern |
|---|---|---|
| `internal/agent/app.go` | 2098 | App struct + core logic — the god object |
| `internal/tui/tui.go` | 1519 | TUI model + update + view routing |
| `internal/proxy/client.go` | 1263 | HTTP client, streaming, retry, cost, grounding |
| `internal/memory/store.go` | 1202 | SQLite store + all tiers |
| `internal/config/config.go` | 1167 | Config struct + validation + endpoint logic |
| `internal/agent/subagent.go` | 1142 | Subagent dispatch + consent + limits |
| `internal/agent/commands.go` | 1038 | Slash commands + plan command |
| `internal/agent/tool_handlers.go` | 935 | All tool handlers |
| `internal/agent/mashura.go` | 914 | Mashūra integration |
| `internal/tui/tui_view.go` | 826 | TUI rendering |
| `internal/exec/exec.go` | 814 | Docker exec + sandbox hardening |

**Fix:** Not urgent — these are large but cohesive. `app.go` (2098 lines) is the strongest split candidate (costState/turnState/subagentState extraction was deferred per WP-6.3). The rest are large but have clear single concerns.

### 19. TODO Comments [CLEANUP]
**Severity:** Low
**Files:**
- `internal/config/config.go:248` — `TODO(per-tool-briefing): per-member briefing customization`
- `internal/counsel/oracle.go:289` — `TODO(parallel): fan-out here — replace panel/fallback loop with goroutine`
- `internal/counsel/oracle.go:581` — `TODO(per-model-debate): critique-of-critique mode`

**Fix:** These are feature ideas, not debt. Leave as-is or convert to Trello cards if any are wanted.

### 20. Unchecked Error Assignments — exec.go Docker Commands [HARDENING]
**Severity:** Medium
**File:** `internal/exec/exec.go:278,369,406,452,472,569`

**Issue:** Multiple `_ = exec.Command("docker", ...).Run()` calls ignore errors. These are setup/cleanup commands (mkdir, docker cp, docker rm, docker stop). If they fail silently, the sandbox may be in an inconsistent state.

**Fix:** Log errors at minimum. For critical setup commands (docker cp of the docker binary, mkdir workdir), consider returning the error.

### 21. MCP CallTool — Args Unmarshal Fallback Bypasses Schema [HARDENING]
**Severity:** Medium
**File:** `internal/agent/mcp_manager.go:294-297`

**Issue:** When `argsJSON` can't unmarshal as a structured object, it falls back to passing the raw string. This bypasses any schema validation for that MCP tool call.

**Fix:** Reject the call if the args don't unmarshal as a map — don't fall through to a raw string that the MCP server might interpret unpredictably.

### 22. StubToolResult — Silent Data Loss on Spill Failure [HARDENING]
**Severity:** Medium
**File:** `internal/tools/toolcap.go:238-244`

**Issue:** When `spillToDisk` fails, `StubToolResult` returns `"[budget — N chars]"` with no spill path. The model gets no signal that the full content was lost (vs. spilled and recoverable). The model may try to read the path that doesn't exist.

**Fix:** Include a marker like `"[budget — N chars — SPILL FAILED]"` so the model knows the content is unrecoverable and doesn't attempt a read.

### 23. dispatch_subagent — Capability Schema Has No Enum [CLEANUP]
**Severity:** Low
**File:** `internal/tools/tools.go:27`

**Issue:** The `capability` field uses `StrProp` (plain string) with no `enum` constraint. The LLM could send arbitrary strings; they're validated in Go code (`ValidCapability`), but the schema doesn't guide the model.

**Fix:** Use an enum schema: `{"type": "string", "enum": ["discovery", "edit", "tools"]}`.

### 24. Deferred WP-6.3 and WP-6.9 Items [KNOWN]
**Severity:** Low (deferred, non-blocking)
**Source:** `release/v0.1.0` memory entry

**Issue:** WP-6.3 remainder (costState/turnState/subagentState extraction from app.go) and WP-6.9 remainder (shared helper dedup groups 2/3/5/7/8 + analyze group 6) were explicitly deferred as non-blockers at v0.1.0 tag time.

**Fix:** Already tracked; pick up when app.go splitting becomes a priority (related to finding #18).

---

## Summary by Severity

| Severity | Count | Finding IDs |
|---|---|---|
| **High** | 2 | #10 (dependabot/vuln scanning), #16 (test coverage for security-critical code) |
| **Medium** | 11 | #1, #2, #4, #5, #8, #9, #11, #12, #13, #15, #20, #21, #22 |
| **Low** | 8 | #6, #7, #14, #17, #18, #19, #23, #24 |

(Note: #3 removed as false positive. #1 downgraded High→Medium. #8 downgraded High→Medium.)

## Card-Worthy Items (consolidated to 11 per Mashūra review)

1. **#10** — Add Dependabot + govulncheck to CI, also pin GitHub Actions by SHA
2. **#16a** — Targeted tests for consent.go (consent/authorization boundaries)
3. **#16b** — Targeted tests for policy/load.go (policy loading + path canonicalization)
4. **#16c** — Targeted tests for dockersock.go (docker socket/sandbox boundary)
5. **#1** — Safe goroutine wrapper for logic-bearing goroutines (oracle, browser, bg reaper) with stack-trace logging
6. **#8** — Enable errcheck + bodyclose linters with baseline exclusion strategy
7. **#4 + #5** — Log unchecked errors in workflow engine + captureFileOriginal (silent audit-trail/restore failures)
8. **#11 + #12 + #13 + #15** — Dockerfile/container hardening: pin git clone, pin base image by digest, document root rationale, review entrypoint signal handling
9. **#2** — Audit LSP server process lifecycle for ctx.Done guard on Wait
10. **#20** — Classify and handle ignored Docker command errors in exec.go
11. **#21 + #22** — MCP CallTool args validation + StubToolResult spill-failure marker

**Quick-win PRs (not separate cards — bundle into one cleanup PR):**
- #6 (delete agent_async.go), #7 (remove staging.go fmt hack), #23 (capability enum schema), #9 (add `cache: true` to setup-go), #14 (HEALTHCHECK if justified)

Items NOT card-worthy: #17 (coverage floors for small packages), #18 (file splitting — deferred WP-6.3), #19 (TODOs), #24 (already tracked).
