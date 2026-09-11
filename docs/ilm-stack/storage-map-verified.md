# ilm-stack Storage Map — Verified against Wakil Code

Fetched contract: `docs/ilm-stack/wakil-emitter.md` (sha256 `5e24a5d4be8b1d8cd9116c28712ef684df2b4db7c7bbd9084a215540c93eb0a9`, version 1)

The contract's event mapping table names write paths W1, W4, W5/W6. There is no
separate "10-entry storage map" served by the stack; the write paths are the rows
in the contract's event mapping table. Below is the verification of each contract
entry against the actual Wakil Go code, plus any paths the contract missed.

## Corrections table

| # | Contract entry | Status | Call site (file:line) | What is written | Format | Timing | Notes |
|---|---|---|---|---|---|---|---|
| 1 | W1: session start | **verified** | `internal/agent/app.go:904-908` (`SendOutcome` → `hookSessionStart`) | Session metadata (chat_id, model, workspace, system prompt) | In-memory App state; hooks fire on first `Send` | First `Send` call per session | The system prompt is loaded once via `loadAgentPrompt` and stored in `app.AgentPrompt`. The preamble is built in `ensurePreamble` and prepended to Conv. |
| 2 | W1: user message | **verified** | `internal/agent/app.go:936-943` (`SendOutcome` → user msg append) | Full user text (with memory context + workflow directive prepended) | `proxy.Message{Role:"user"}` appended to `a.Conv` | Per turn, before model runs | Full content stored. The `stored` variable may have memory context prepended — emitter should emit the original `userText`, not `stored`. |
| 3 | W1: assistant message | **verified** | `internal/agent/turn_phases.go:242-244` (`streamTurn` → assistant msg append) | Full assistant text (content + tool calls) | `proxy.Message` from `Client.Stream` appended to `a.Conv` | Per model response, inside stream loop | Full content. `msg.Content` is the text; `msg.ToolCalls` are the tool requests. |
| 4 | W1: tool call | **verified** | `internal/agent/turn_phases.go:344-371` (`streamTurn` → tool dispatch) | Tool name, args (full JSON object), call_id, turn_id | `proxy.ToolCall` in `msg.ToolCalls` | Per tool call, inside stream loop | The tool call itself is in `msg.ToolCalls[ti]`. The dispatch happens via `a.handleToolCall(ctx, tc)` at line 365. |
| 5 | W1: tool result | **verified** | `internal/agent/turn_phases.go:280-334` (`finalizeToolResult`) | Tool name, call_id, ok (bool/null), output (bounded), full_hash, full_size | `proxy.Message{Role:"tool"}` appended to `a.Conv` | Per tool result, after tool completes | The RAW result text is in `result.text` (pre-cap). `CapOrStub` may truncate it. The emitter should emit the FULL pre-cap output (bounded to `max_output_bytes` with truncated flag), with `full_hash` = sha256 of the untruncated post-redaction output, `full_size` = byte count. |
| 6 | W5/W6: memory_put / memory_get / skill ops / staging ops | **verified** | `internal/agent/app.go:2213-2228` (memory tools dispatch) | op, key, content, provenance | Tool result string | Per tool call | All memory tools are dispatched in `handleToolCall` switch. The handlers are in `internal/agent/memory_tools.go`. Skill handlers in `internal/agent/skill_handlers.go`. Staging handlers in `internal/agent/staging_tools.go`. The emitter hook goes at the dispatch point (app.go:2213), emitting a `memory_op` event for each. |
| 7 | W4: mashura__* calls | **verified** | `internal/agent/app.go:2190-2191` (mashura dispatch) | tool name, args | Tool result string | Per mashura call | Routes through `a.handleMashura(ctx, name, tc)`. The four tools: `mashura__review`, `mashura__debug`, `mashura__decide`, `mashura__check`. |
| 8 | W1: error | **verified** | `internal/agent/turn_phases.go:211-213` (stream error) + `internal/agent/app.go` (various error paths) | Error text | Returned from `Client.Stream` or tool handlers | Per error | Stream errors return from `streamTurn`. Tool errors are in `result.text`. |
| 9 | W1: session end | **verified** | `internal/agent/app.go:778` (`NewConversation` → `hookSessionEnd`) + `internal/agent/app.go:768-771` (`OnStop`) | Outcome, summary | Hooks fire on `/new`, `/handoff`, or process exit | On session rotation or process exit | `OnStop` fires on process exit. `hookSessionEnd` fires on `/new` and `/handoff`. |
| 10 | W1: system prompt | **verified** | `internal/agent/app.go:933` (`ensurePreamble`) | Full system prompt text | Stored in `a.AgentPrompt`, prepended to Conv as preamble | Once per session, on first `Send` | Captured on `session_start` event as `system[]` per contract. |

## Paths the contract missed (added)

| # | Write path | Call site | Event type | Notes |
|---|---|---|---|---|
| A1 | Session transcript JSON file | `internal/agent/session.go:49` (`WriteSession`) | (covered by W1 events) | The full session is persisted to `$WAKIL_SESSIONS_DIR/<chatID>.json` via `atomicWriteJSON`. Called from `SaveSession` (app.go:743), handoff (handoff.go:126,232). This is the on-disk persistence — the emitter hooks at the in-memory append sites (rows 1-5 above), not at the file write. The file write is a snapshot of the in-memory Conv, so emitting at append time covers it. |
| A2 | Session-history index | `internal/agent/sessionhistory_bridge.go` + `internal/sessionhistory/store.go` | (covered by W1 events) | SQLite index of sessions for recall. Derived from session JSON, not a separate write path for event purposes. |
| A3 | Repo-state persistence | `internal/agent/repostate.go:167` | (not a turn-content write path) | Per-repo settings (AutoApprove, model, etc.). Does NOT carry turn content. Not hooked. |
| A4 | Handoff sidecar JSON | `internal/agent/handoff.go:541-556` | (covered by W1 events) | `storeHandoffRecord` writes a sidecar JSON + durable memory entry. The memory_put is already covered by row 6. The sidecar is an audit artifact, not a live event. |
| A5 | Trace store | `internal/trace/` | (not hooked) | JSONL trace of tool calls. Separate from the event stream. Not a turn-content write path for ilm purposes — it's a debugging artifact. |

## Resolution: does "sessionhost" hold full turns or previews?

The session transcript (`Session.Conv` in `internal/agent/session.go:29`) holds
**full turns** — the entire `[]proxy.Message` slice with complete content, tool
calls, and tool results. `WriteSession` (session.go:49) persists this as indented
JSON to `$WAKIL_SESSIONS_DIR/<chatID>.json`. There is no preview/summary
truncation at the persistence layer; the only truncation is `CapOrStub` applied
to tool results before they enter Conv (turn_phases.go:308-309), which caps the
in-context representation but not the original tool output (which is what the
emitter captures).

The session-history index (`internal/sessionhistory/store.go`) stores a
**derived summary** for recall search, but this is a secondary index — the full
turns live in the session JSON files and in the in-memory Conv slice.

## Emitter hook points summary

The emitter hooks at the in-memory append/dispatch sites, not at the file-write
sites. This is correct because:

1. The Conv slice is the live transcript — every turn, tool call, and result
   passes through it before any persistence happens.
2. File writes (SaveSession) are periodic snapshots of Conv, not individual
   events. Hooking at the file level would miss the event granularity the
   contract requires (per-turn, per-tool-call events).
3. The emitter must fire at the point of append, with the full content, before
   any capping or truncation.

### Hook locations (one-liners):

| Event type | Hook location | Data available |
|---|---|---|
| `session_start` | `app.go:905-908` (first Send) | chat_id, model, workspace, system prompt |
| `user_turn` | `app.go:942` (after Conv append) | full userText |
| `assistant_turn` | `turn_phases.go:243` (after Conv append) | full msg.Content |
| `tool_call` | `turn_phases.go:345` (before dispatch) | tool name, args, call_id |
| `tool_result` | `turn_phases.go:327` (after finalize) | tool, call_id, output, ok |
| `memory_op` | `app.go:2213-2228` (dispatch switch) | op, key, content |
| `mashura_call` | `app.go:2190` (dispatch switch) | tool, args |
| `error` | `turn_phases.go:212` + error paths | error text |
| `session_end` | `app.go:779` (NewConversation) + `app.go:768` (OnStop) | outcome |
