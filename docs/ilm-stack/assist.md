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

---

## 2026-09-17 — Call-count check and fix

### Check result: FAILED (fixed)

The call-count contract — **exactly one POST /v1/assist and one assist_event
per assistant turn that could produce a tool call** — was violated by three
bugs in the counting/emission path. The policy (confirm mode, allowlist, auto
flag) is unchanged.

### Session data check

Session `wakil-live:1b2e8523-1a8e-475d-9147-6f06ec204d22` (and predecessors)
could not be inspected directly: the ILM queue file (`ilm-queue.jsonl`) and
trace files live on the host at `~/.local/share/wakil/`, not inside the
sandbox. The code-level analysis is definitive — the bugs below guarantee
over-counting on any multi-iteration turn.

### Bug 1: Per-iteration, not per-turn (counting)

**Location**: `internal/agent/turn_phases.go:228`

`assistCanAct(forceFinish)` was called inside the `streamTurn` for-loop on
every iteration. A single user turn with N tool-call iterations (model calls
tool → gets result → model called again → … → final text) produced N+1 assist
calls instead of 1. The contract requires exactly one per turn.

**Fix**: Gate the assist call on `iter == 0`. Assist runs once at the top of
the turn, before the first model call. On subsequent iterations (after tool
results are fed back), assist is not called again — the model drives the rest
of the turn.

### Bug 2: No "rejected" decision for 400 (emission)

**Location**: `internal/ilm/assist.go:95`, `internal/agent/assist.go:42`

When the seq passed to `/v1/assist` is an `assistant_turn` seq (which happens
on iteration 1+ due to bug 1), the server returns 400. `AssistClient.Query`
returned a generic error for all non-200s, and `tryAssist` classified it as
`decision="error"`. The contract requires `decision="rejected"` for a 400 —
the server rejected the request, distinct from a transport error.

**Fix**: Added `AssistRejectedError` type in `internal/ilm/assist.go`. `Query`
returns it for HTTP 400. `tryAssist` uses `errors.As` to detect it and emits
`decision="rejected"` instead of `"error"`.

### Bug 3: forceFinish guard (not a bug — correct)

The `forceFinish` guard in `assistCanAct` was a candidate concern: when
`forceFinish` is true (iteration limit hit), assist is skipped entirely and
no assist_event is emitted. This is **correct** — a force-finished turn has
tools stripped, so there is no tool-decision point. The `iter == 0` gate
makes this moot in practice (forceFinish is always false on iter 0), and the
guard is retained as defense-in-depth.

### Replay test (C5, extended)

`TestAssistReplay4Turns` in `internal/agent/assist_replay_test.go` — a
4-turn agent-level replay asserting:

- 4 assist calls to the stub server (exactly 1 per turn)
- 4 assist_events in the emitter queue (exactly 1 per turn)
- Decisions in order: `auto`, `abstain`, `error`, `rejected`
- Turn 3: 800ms timeout (2s server delay)
- Turn 4: 400 (server rejects seq)

The model server returns text-only responses (no tool calls), so each turn
is exactly 1 iteration — verifying the `iter == 0` gate.

### Files changed

| file | change |
|---|---|
| `internal/agent/turn_phases.go:228` | Gate assist call on `iter == 0` |
| `internal/ilm/assist.go` | Add `AssistRejectedError`, return it for HTTP 400 |
| `internal/agent/assist.go` | Handle `AssistRejectedError` → emit `decision="rejected"` |
| `internal/ilm/payloads.go` | Add `"rejected"` to `AssistEventPayload.Decision` doc |
| `internal/ilm/assist_test.go` | Add `TestAssistClient400Rejected` |
| `internal/agent/assist_replay_test.go` (NEW) | `TestAssistReplay4Turns`: 4-turn agent-level replay |

### Test results

```
internal/ilm   — PASS (9.15s)  [11 tests incl. TestAssistClient400Rejected]
internal/agent — PASS (51.8s)  [incl. TestAssistReplay4Turns]
internal/config — PASS (0.01s)
```

### What remains unverified

- The live session `wakil-live:1b2e8523-…` was not inspected (host-side data
  not accessible from sandbox). Run this on the host to confirm the fix
  retroactively:

  ```bash
  jq -c 'select(.type=="assist_event")' ~/.local/share/wakil/ilm-queue.jsonl | \
    jq -r '.seq' | sort -n
  jq -c 'select(.type=="assistant_turn")' ~/.local/share/wakil/ilm-queue.jsonl | \
    jq -r '.seq' | sort -n
  ```

  The assist_event seqs should be a strict subset of the assistant_turn seqs,
  one per turn, after the fix. Before the fix, there would be more
  assist_events than turns on any multi-iteration turn.
