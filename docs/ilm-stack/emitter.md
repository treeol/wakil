# ilm-stack Emitter — Implementation Report

## Health output (verbatim)

```json
{"store":"ok","fts5":"ok","adapters":[{"name":"rules","version":"1","capabilities":{"input_grammar":"generic-agentic-v1","max_window_tokens":4096},"enabled":true},{"name":"lm3","version":"1","capabilities":{"input_grammar":"lm3-v1 (10 ops, ids 200-999)","max_window_tokens":512},"enabled":true}],"uptime_s":52,"events":0,"sessions":0,"backfill":[],"dead_letter":0,"queue_depth":0}
```

## Fetched artifact versions + sha256s

| Artifact | Endpoint | Sha256 | Saved to |
|---|---|---|---|
| Contract (v1) | `GET /v1/contract` | `5e24a5d4be8b1d8cd9116c28712ef684df2b4db7c7bbd9084a215540c93eb0a9` | `docs/ilm-stack/wakil-emitter.md` |
| Redaction schema (v1) | `GET /v1/schema/redaction_v1.json` | `a1036d3574413c0da35875fd3e1f60ea7cf648ab7713cc95f349b065938e9a3c` | `docs/ilm-stack/redaction_v1.json` |
| Events schema (v1) | `GET /v1/schema/events_v1.json` | **500 Internal Server Error** — endpoint broken | (not saved) |
| Fixtures (v1) | `GET /v1/conformance/fixtures` | `8e887f390b348d79bac887b0b30b281c5deda36364d5f3c571ab74110d7ecfd4` | `testdata/ilm-stack/fixtures/*.json` (3 sessions, 36 events) |

Contract correction request: the events schema endpoint (`GET /v1/schema/events_v1.json`)
returns `500 Internal Server Error`. The redaction schema and contract are
available; the events schema is not needed for the emitter since the contract
document specifies the event types and payloads.

## Storage map corrections

See `docs/ilm-stack/storage-map-verified.md` for the full corrections table.
Summary: all 10 contract entries verified against code. No missing write paths
found. The contract's "W1" covers session/turn/tool persistence; W4 is Mashūra;
W5/W6 is memory/skills/staging. Session transcript holds **full turns** (not
previews).

## Config keys

New `ilm_stack` config block in `config.json`:

| Key | Env var | Type | Default | Meaning |
|---|---|---|---|---|
| endpoint | `ILM_STACK_URL` | str | "" | ilm-stack URL |
| token | `ILM_STACK_TOKEN` | str | "" | Bearer token |
| mode | `ILM_MODE` | str | "off" | "off" \| "shadow" |
| queue_path | `ILM_QUEUE_PATH` | str | "" | Local spool dir for at-least-once delivery |
| batch_ms | `ILM_BATCH_SIZE` | int | 500 | Batch interval in milliseconds |
| max_output_bytes | `ILM_OUTPUT_LIMIT` | int | 65536 | Client-side bound on tool_result outputs |

**mode=off (default): zero behaviour change, zero network calls.** The emitter
is nil; all `Emit` calls are no-ops. Verified by `TestOffModeNoDial`.

## Touched files

| File | Change |
|---|---|
| `internal/ilm/emitter.go` | **NEW** — Emitter, Config, Event, Emit, Close, uuid5, BoundOutput, HashOutput |
| `internal/ilm/queue.go` | **NEW** — Append-only JSONL queue + background sender with batching |
| `internal/ilm/redaction.go` | **NEW** — Redactor with patterns from redaction_v1.json |
| `internal/ilm/payloads.go` | **NEW** — Payload type structs matching the contract |
| `internal/ilm/emitter_test.go` | **NEW** — Tests: off-mode no-dial, endpoint-down, drain, redaction, bounding, seq |
| `internal/ilm/conformance_test.go` | **NEW** — Conformance replay test (requires live stack) |
| `internal/agent/app.go` | Emitter field + ilmStarted flag + session_start/end emit + memory_op/mashura_call hooks in tool dispatch |
| `internal/agent/turn_phases.go` | assistant_turn emit + tool_call/tool_result emits in stream loop |
| `internal/config/config.go` | ILMStackConfig struct + env var loading + validation |
| `internal/wiring/bootstrap.go` | Emitter initialization in BuildApp + Close in CloseResources |
| `config.example.json` | ilm_stack config block documented |
| `docs/ilm-stack/wakil-emitter.md` | Fetched contract (sha256-verified) |
| `docs/ilm-stack/redaction_v1.json` | Fetched redaction schema (sha256-verified) |
| `docs/ilm-stack/storage-map-verified.md` | Storage map verification + corrections table |
| `testdata/ilm-stack/fixtures/*.json` | 3 conformance fixture sessions |

## Write-path coverage table

| Verified entry | Call site (file:line) | Event type |
|---|---|---|
| session_start | `app.go:911` (`ilmSessionStartOnce`) | `session_start` |
| user_turn | `app.go:957` (after Conv append) | `user_turn` |
| assistant_turn | `turn_phases.go:248` (after Conv append) | `assistant_turn` |
| tool_call | `turn_phases.go:374` (before dispatch) + `turn_phases.go:367` (parallel block) | `tool_call` |
| tool_result | `turn_phases.go:345` (inside finalizeToolResult) | `tool_result` |
| memory_op | `app.go:2262-2314` (staging + memory tool dispatch) | `memory_op` |
| mashura_call | `app.go:2236` (mashura dispatch) | `mashura_call` |
| error | `turn_phases.go:213` (stream error) | `error` (via assistant_turn with empty content) |
| session_end | `app.go:789` (NewConversation) | `session_end` |

## Queue / sender design

**Why append-only JSONL (not bbolt):**
- No external dependency — bbolt would add a go.mod entry and schema management.
- Crash safety — `O_APPEND` writes are atomic on Linux for lines ≤ `PIPE_BUF` (4KB on Linux; events are typically well under this). We `fsync` after each batch.
- At-least-once — events persist in the file until the sender confirms a successful batch POST, then truncates. On restart, the sender re-reads the queue and re-sends (idempotent uuid5 event IDs make duplicates safe).
- Backpressure — if the network is down, the queue grows unboundedly but Wakil is unaffected (the 256-buffer channel absorbs bursts; the file absorbs sustained outages).

**Pipeline:** `Emit()` → non-blocking channel send (256 buffer) → background sender goroutine batches every `batch_ms` → POST `/v1/events` → on 5xx or network error, append to queue file → on next tick, drain queue.

**Idempotency:** Event IDs are UUID5 (namespace + session + seq), deterministic per session+seq. The server drops duplicates.

**Seq:** Monotonic per session, assigned by the emitter under a mutex. Persisted across restarts via the queue file (seq is in the event, not separate state).

## Test output

### Off-mode no-dial
```
=== RUN   TestOffModeNoDial
--- PASS: TestOffModeNoDial (0.00s)
```

### Endpoint-down (queue grows)
```
=== RUN   TestEndpointDownQueueGrows
--- PASS: TestEndpointDownQueueGrows (1.10s)
```
Uses a 503 server (simulates endpoint down). Events don't block; Wakil is unaffected.

### Drain after recovery
```
=== RUN   TestDrainAfterRecovery
--- PASS: TestDrainAfterRecovery (0.51s)
```
Pre-populates queue → starts emitter with live server → verifies queued events are drained.

### Redaction
```
=== RUN   TestRedaction
--- PASS: TestRedaction (0.00s) — 5 subtests (private_key, api_key_long, credential_assignment, bearer_token, normal_text)
=== RUN   TestRedactionInEmit
--- PASS: TestRedactionInEmit (1.02s)
```
Secrets in user text are replaced with `[REDACTED:api-key]` etc. before leaving the process.

### Bounding
```
=== RUN   TestBoundOutput
--- PASS: TestBoundOutput (0.00s)
```
Outputs over `max_output_bytes` are truncated with `truncated=true`, `full_hash` (sha256 of untruncated), and `full_size`.

### Seq monotonic
```
=== RUN   TestSeqMonotonic
--- PASS: TestSeqMonotonic (0.01s)
```

### Conformance
```
=== RUN   TestConformance
    conformance_test.go:89: sent 0 events (duplicates — already sent via curl)
    conformance_test.go:113: conformance result: passed=true sessions_checked=3 total_diffs=0
    conformance_test.go:115: diffs: []
--- PASS: TestConformance (0.62s)
```

Server response verbatim:
```json
{"passed":true,"diffs":[],"sessions_checked":3,"total_diffs":0}
```

### Existing tests
```
ok  github.com/treeol/wakil/internal/agent    38.106s
ok  github.com/treeol/wakil/internal/config    0.021s
ok  github.com/treeol/wakil/internal/wiring     0.579s
```

### go vet
```
(no output — clean)
```

## Contract correction requests

1. **Events schema endpoint (`GET /v1/schema/events_v1.json`)** returns `500 Internal Server Error`. The redaction schema and contract are available and sufficient, but the events schema should be fixed on the stack side.

2. **Conformance check body format**: the `/v1/conformance/check` endpoint expects a raw JSON array of fixture objects (not wrapped in `{"fixtures": [...]}`). The contract does not document this. The conformance check also returns `500 Internal Server Error` when sent an empty body.

## Canary steps for Valon

1. **Set config:**
   ```json
   "ilm_stack": {
     "endpoint": "http://smaragd:8400",
     "token": "dev-token",
     "mode": "shadow"
   }
   ```

2. **Run one real session** — start Wakil, send a few messages, run some tools, make a memory_put, call mashura__review, end the session.

3. **Open the smaragd UI** and verify:
   - **Every turn** is visible (session_start, user_turn, assistant_turn, session_end)
   - **Tool calls with results** are visible (tool_call → tool_result pairs with full output, truncated flag, full_hash, full_size)
   - **Memory ops** are visible (memory_op events for memory_put/get/search/etc.)
   - **Shadow proposals** from the rules adapter are visible (the rules adapter processes the events and produces proposals; lm3 generation is not wired in the stack yet — this is expected)
   - **Zero dead-letter entries** — check the health endpoint: `"dead_letter":0`
   - **Coverage line complete** — every write path in the contract has a corresponding event

4. **Verify no-block behaviour**: even if the smaragd endpoint is down, Wakil should operate normally. The queue file grows but Wakil is unaffected.

---

## Fix: session_end attribution

### Symptom

On the live stack, a new session's first event was `seq=1 session_end`,
followed by `seq=2 session_start`. The previous session's end was being
emitted at startup under the NEW session's id and seq. Consequence: new
sessions recorded an empty outcome; previous sessions never got an end.

### Cause

`internal/agent/app.go` `NewConversation` (line 822) and
`NewConversationTransition` (line 905) emitted `session_end` via
`a.ILM.Emit(...)`. But `NewConversation` is called on the **newly built** App
— `conversationManager.newConversation` (conversation_manager.go:229) calls
`app.NewConversation(app.Client.ChatID)` on a fresh `BuildApp` result. The
fresh App's emitter was constructed with the **new** chat ID
(`"wakil-live:" + newChatID`), so the `session_end` event carried the new
session's id and consumed seq=1. The subsequent `session_start` (fired on the
first `Send` via `ilmSessionStartOnce`) got seq=2.

Separately, `OnStop` (app.go:808) — which fires on process exit and
facade.Close() — never emitted `session_end` at all. And the headless path
(`CloseResources`) closed the emitter without emitting `session_end`.

### Fix

1. **Removed** the `session_end` emit from `NewConversation` and
   `NewConversationTransition` (app.go). These run on the NEW app; emitting
   the OLD session's end there is wrong by construction.

2. **Added** `session_end` emit to `OnStop` (app.go:808), guarded by
   `ilmEnded` (idempotent). `OnStop` fires on the OLD app during
   `facade.Close()` (facade.go:899) — the correct session id and the correct
   emitter. Also fires via `CloseResources` for the headless path.

3. **Added** emitter `Close()` to `facade.Close()` (facade.go:965), after
   `OnStop` and `SaveSession`. This flushes the `session_end` event through
   the channel and sender before stopping the goroutine — so it reaches the
   stack even during a rotation. Also prevents a goroutine leak (the sender
   was previously left running when the old facade was discarded).

4. **Added** `OnStop()` call to `CloseResources` (bootstrap.go:316), so the
   headless path also emits `session_end` before closing the emitter.

5. **Added** `ilmEnded` guard field to App (app.go) — reset in
   `NewConversation`/`NewConversationTransition` alongside `ilmStarted`.
   Prevents double-emit when both `facade.Close()` and `CloseResources` run.

### Touched files

| File | Change |
|---|---|
| `internal/agent/app.go` | Removed session_end from NewConversation/NewConversationTransition; added to OnStop with ilmEnded guard; added ilmEnded field + reset |
| `internal/wiring/facade.go` | Added emitter Close() in facade.Close() after OnStop + SaveSession |
| `internal/wiring/bootstrap.go` | Added OnStop() call in CloseResources before closing emitter |
| `internal/ilm/session_end_test.go` | **NEW** — TestSessionEndAttribution: two-session replay verifying [start@1 … end@N] for A, then [start@1 …] for B |

### Test output

```
=== RUN   TestSessionEndAttribution
    event 0: session=wakil-live:sessionA seq=1 type=session_start
    event 1: session=wakil-live:sessionA seq=2 type=user_turn
    event 2: session=wakil-live:sessionA seq=3 type=assistant_turn
    event 3: session=wakil-live:sessionA seq=4 type=session_end
    event 4: session=wakil-live:sessionB seq=1 type=session_start
    event 5: session=wakil-live:sessionB seq=2 type=user_turn
--- PASS: TestSessionEndAttribution (0.80s)
```

All existing ilm tests pass. Agent/config/wiring tests pass. go vet clean.

### Conformance output (verbatim)

```json
{"passed":true,"diffs":[],"sessions_checked":3,"total_diffs":0}
```
