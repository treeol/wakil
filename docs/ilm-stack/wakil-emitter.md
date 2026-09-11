# Wakil emitter contract v1 (language-neutral)

This is a **served contract** (GET /v1/contract), not just a file. A client on
any host/language needs only the endpoint + bearer token. Every served artifact
carries a version and sha256.

## Config keys

| key | env var | type | meaning |
|---|---|---|---|
| endpoint | `ILM_STACK_URL` | str | e.g. `http://smaragd.local:8400` |
| token | `ILM_STACK_TOKEN` | str | shared bearer token |
| mode | `ILM_MODE` | str | `off` \| `shadow` (`assist` returns 501) |
| queue_path | `ILM_QUEUE_PATH` | str | local spool dir for at-least-once delivery |
| batch_size | `ILM_BATCH_SIZE` | int | default 64 |
| flush_interval_s | `ILM_FLUSH_S` | float | default 2.0 |
| output_byte_limit | `ILM_OUTPUT_LIMIT` | int | client-side bound on tool_result outputs (mirror of server default 65536) |
| client_name | — | str | default `"wakil"` |
| client_version | — | str | emitter version |

## Session-id namespace

Live sessions use `wakil-live:{chat_id}`. Backfilled file sessions use
`file:{chat_id}`. Backfilled sessionhost sessions use `host-{sha8}`. These
namespaces can never collide.

## Event mapping from Wakil write paths (F1 → event types)

| Wakil write path (F1) | ilm-stack event type | payload (FULL content, never preview) |
|---|---|---|
| W1: session start | `session_start` | {source:"live", workspace, model, system?} |
| W1: user message | `user_turn` | {text (full)} |
| W1: assistant message | `assistant_turn` | {text (full, may be empty), reasoning?, model_id?} |
| W1: tool call | `tool_call` | {tool, args (full object), call_id, turn_id?} |
| W1: tool result | `tool_result` | {tool, call_id, ok (bool|null), output (bounded+truncated flag), full_hash, full_size, source_incomplete?} |
| W5/W6: memory_put / memory_get / skill ops / staging ops | `memory_op` | {op, key, content?, provenance?} |
| W4: mashura__* calls | `mashura_call` | {tool, args} |
| W1: error | `error` | {text} |
| W1: session end | `session_end` | {outcome?, summary?} |
| W1: system prompt | captured on `session_start` payload `system[]` | {source:"live", workspace, model, system:[text,...]} |
| mashura__* call | `mashura_call` | {tool, args} |
| any error surfaced to the turn | `error` | {text} |
| session end | `session_end` | {outcome?, summary?} |

`seq` is assigned by the emitter monotonically per session (Wakil's own
sequence + a per-type suffix is acceptable as long as it is strictly
increasing per session).

## Redaction rules (client's job — first line of defence)

- Strip secret-bearing env values, tokens, and credential file contents BEFORE
  constructing the event. The server re-scans and flags, but the emitter MUST
  not rely on that.
- tool_result outputs bounded to `output_byte_limit` bytes; the full hash +
  size are computed on the untruncated output.

## Batching / retry / delivery

- Local disk queue (`queue_path`): append JSONL, at-least-once.
- Batches POSTed to `/v1/events` with `Authorization: Bearer <token>`.
- On network error or 5xx: keep batch in the queue, retry with backoff.
- Idempotency: every event carries a client-generated uuid `event_id`; the
  server drops duplicates. Retries are therefore safe.
- `mode=off`: emitter discards events immediately (no queue writes).

## Shadow mode guarantee

ilm-shadow never writes back to Wakil and the emitter never reads any
response body other than the ingest acknowledgment. No proposal from any
adapter reaches Wakil in v0.