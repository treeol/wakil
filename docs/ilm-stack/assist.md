# ASSIST-1 — ilm_stack assist mode

## Overview

ASSIST-1 adds a new `ilm_stack.mode = "assist"` that queries the ilm-stack's
`/v1/assist` endpoint before each tool-decision model call. When the server
returns a high-confidence proposal (grounded in past sessions) that passes
Wakil's own read-only allowlist, Wakil executes it instead of calling the main
model — saving a model round-trip. The assist path is strictly an optimization:
on any error, abstention, or allowlist failure, the normal model call proceeds
unchanged.

## Configuration

| key | env var | type | default | meaning |
|---|---|---|---|---|
| `ilm_stack.mode` | `ILM_MODE` | `"off"` \| `"shadow"` \| `"assist"` | `"off"` | assist mode (in addition to existing shadow mode) |
| `ilm_stack.assist_auto` | `ILM_ASSIST_AUTO` | bool | `false` | auto-execute proposals without y/n prompt |
| `ilm_stack.endpoint` | `ILM_STACK_URL` | str | — | ilm-stack base URL |
| `ilm_stack.token` | `ILM_STACK_TOKEN` | str | — | shared bearer token |

When `mode = "assist"`, the emitter is created in assist mode (same event
pipeline as shadow) plus an `AssistClient` for synchronous `/v1/assist` calls.

## Per-session toggles (C4)

| command | effect |
|---|---|
| `/assist` | toggle assist on/off (per-session) |
| `/assist auto` | toggle auto-execution on/off (per-session) |

The status line shows `assist` (green) when enabled, or `assist:auto` (green,
bold) when auto-execution is on. Never silent.

## Touched files

| file | change |
|---|---|
| `internal/config/config.go` | Add `"assist"` to valid modes; add `AssistAuto` field + env override |
| `internal/ilm/emitter.go` | Add `ModeAssist`, `EventAssist`, `SessionID()`, `Seq()` accessors; accept assist mode in `New` |
| `internal/ilm/payloads.go` | Add `AssistEventPayload` (C3) |
| `internal/ilm/assist.go` (NEW) | `AssistClient`: POST `/v1/assist`, 800ms timeout, request/response types |
| `internal/ilm/assist_allowlist.go` (NEW) | Explicit read-only allowlist (C2): read_file, search_files, lsp_*, browser_* (read-only) — no run_shell, no writes, no MCP |
| `internal/ilm/assist_test.go` (NEW) | 9 tests: client (action, abstain, timeout, non-200), allowlist (allowed, denied, MCP, malformed), replay (4 sessions, 4 decisions) |
| `internal/agent/assist.go` (NEW) | `tryAssist`: query → parse → allowlist → confirm/auto → execute → emit event |
| `internal/agent/turn_phases.go` | Before model call: if assist enabled, query `/v1/assist`; on take, skip model call |
| `internal/agent/app.go` | Add `Assist`, `AssistEnabled`, `AssistAuto` fields to App |
| `internal/agent/commands.go` | `/assist` and `/assist auto` slash command handler |
| `internal/wiring/bootstrap.go` | In assist mode: create emitter + AssistClient |
| `internal/core/sessionclient/info_snapshot.go` | Add `AssistEnabled`, `AssistAuto` to InfoSnapshot |
| `internal/wiring/facade.go` | Wire assist state into SessionSnapshot |
| `internal/tui/complete.go` | Add `/assist` to slash command picker |
| `internal/tui/tui_view.go` | Add assist indicator to status line + buildStatusInput |
| `docs/ilm-stack/assist-contract.md` (NEW) | Fetched contract from `/v1/contract/assist` |

## Event examples (C3)

Every assist response and Wakil's decision is emitted as `assist_event`:

```json
// Took (auto-executed)
{"decision":"auto","tool":"read_file","args":{"path":"test.go"},
 "gate_probability":0.85,"candidate_provenance":{...},"latency_ms":132.5}

// Ignored (user declined)
{"decision":"ignored","tool":"read_file","args":{"path":"test.go"},
 "gate_probability":0.81,"candidate_provenance":{...},"latency_ms":120.0}

// Not applicable (failed allowlist)
{"decision":"not_applicable","tool":"run_shell","args":{"command":"rm -rf /"},
 "gate_probability":0.90,"candidate_provenance":{...},"latency_ms":95.0}

// Abstain (server abstained)
{"decision":"abstain","abstain":true,"reason":"gate_probability < threshold",
 "gate_probability":0.41,"latency_ms":68.1}

// Error (timeout / non-200 / malformed)
{"decision":"error","reason":"assist: HTTP 503","gate_probability":0,"latency_ms":0}
```

## Test output

```
=== RUN   TestAssistClientAction
--- PASS: TestAssistClientAction (0.00s)
=== RUN   TestAssistClientAbstain
--- PASS: TestAssistClientAbstain (0.00s)
=== RUN   TestAssistClientTimeout
--- PASS: TestAssistClientTimeout (2.00s)
=== RUN   TestAssistClientNon200
--- PASS: TestAssistClientNon200 (0.00s)
=== RUN   TestAssistAllowlistAllowed
--- PASS: TestAssistAllowlistAllowed (0.00s)
=== RUN   TestAssistAllowlistDenied
--- PASS: TestAssistAllowlistDenied (0.00s)
=== RUN   TestAssistAllowlistMCPDenied
--- PASS: TestAssistAllowlistMCPDenied (0.00s)
=== RUN   TestAssistAllowlistMalformedArgs
--- PASS: TestAssistAllowlistMalformedArgs (0.00s)
=== RUN   TestReplayAssistDecisions
    session 1 decision: took (auto) — action passes allowlist, executed
    session 2 decision: abstain — server returned abstention
    session 3 decision: error — timeout, proceed without assist
    session 4 decision: not_applicable — run_shell failed allowlist
    4 decisions asserted: [took abstain error not_applicable]
--- PASS: TestReplayAssistDecisions (2.00s)
PASS
ok  github.com/treeol/wakil/internal/ilm  4.009s
```

All existing tests also pass:
- `internal/ilm` — PASS (5.15s)
- `internal/agent` — PASS (49.8s)
- `internal/config` — PASS
- `internal/tui` — PASS (1.1s)

## Canary steps

1. **Config**: set `ilm_stack.mode = "assist"`, `ilm_stack.endpoint` to the
   smaragd server, `ilm_stack.token` to the shared bearer token.

2. **Start Wakil** with a real session. Type `/assist` to enable. The status
   line should show `assist` (green).

3. **What to see in the TUI**:
   - On a tool-decision point where the assist server has a proposal:
     - `assist_auto=false`: a one-line prompt with tool name, gate
       probability, and provenance session count — press `y` to execute,
       `n` to call the main model instead.
     - `assist_auto=true`: the proposed tool executes directly. The status
       line briefly shows `assist:auto` (bold green).
   - On abstention/timeout/error: nothing visible — the main model runs as
     usual. The assist_event is still emitted (logged).

4. **What to see on the smaragd UI**:
   - `wakil-assist:<session_id>` pseudo-session events: each assist call +
     response + provenance (A3, server-side).
   - `wakil-assist-shadow` events: 10% of gated-OUT decisions (A3,
     server-side).
   - The `assist` block in `/v1/adapters`: assist rate, act rate, agreement
     with the main model's subsequent action when Wakil ignored the proposal
     (A4).
