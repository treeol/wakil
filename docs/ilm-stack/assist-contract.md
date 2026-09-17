# ASSIST-1 contract (served at GET /v1/contract/assist)

This is a **served contract**, not just a file. The Wakil side needs only the
endpoint + bearer token. Every served artifact carries a version and sha256.

ASSIST-1 is **proposal-only**. The server never executes anything. It returns a
candidate next action (grounded in a past session) or an abstention. Wakil
decides what to do with the proposal; gate passage is **not** authorization.

## Config keys

| key | env var | type | meaning |
|---|---|---|---|
| endpoint | `ILM_STACK_URL` | str | e.g. `http://smaragd.local:8400` |
| token | `ILM_STACK_TOKEN` | str | shared bearer token (same as emitter) |
| assist_enabled | `ILM_ASSIST_ENABLED` | `1`\|`0` | kill switch (A5); `0` → 503 |
| decider_endpoint | `ILM_ASSIST_DECIDER_ENDPOINT` | str | D-wlm server (default `http://172.17.0.1:8508`) |
| econ_endpoint | `ILM_ECON_ENDPOINT` | str | E-con embed server (default `http://172.17.0.1:8506`) |
| timeout_s | `ILM_ASSIST_TIMEOUT_S` | float | end-to-end deadline (default `0.8`) |
| shadow_sample | `ILM_ASSIST_SHADOW_SAMPLE` | float | gated-OUT shadow-log fraction (default `0.1`) |

## POST /v1/assist

**Request** — identify the decision point. The stack builds the window and the
k=3 candidates with the SAME code path the shadow runner uses (skew-gated).

```json
{
  "session_id": "wakil-live:9f2c…",   // the live session
  "seq": 42                            // the decision point's seq
}
```

**Response 200** — a proposal or an abstention, with full provenance.

```json
{
  "action": {"b": "tool", "name": "read_file", "args": {"path": "ilm_stack/api.py"}},
  "abstain": false,
  "gate_probability": 0.813,
  "decider_probability": 0.662,
  "candidate_provenance": {
    "selected": {"session_id": "file:00cd7ffd…", "seq": 5},
    "candidates": [
      {"session_id": "file:00cd7ffd…", "seq": 5},
      {"session_id": "file:01ab23…", "seq": 11},
      {"session_id": "file:02bc45…", "seq": 3}
    ]
  },
  "decider_id": "d-wlm",
  "gate_sha256": "d312a6c321b88175",
  "k": 3,
  "threshold": 0.76,
  "latency_ms": 132.5
}
```

Abstention (`action` is `null`, `abstain` is `true`):

```json
{
  "action": null,
  "abstain": true,
  "reason": "gate_probability < threshold",
  "gate_probability": 0.412,
  "decider_probability": null,
  "candidate_provenance": {"candidates": []},
  "decider_id": "d-wlm",
  "gate_sha256": "d312a6c321b88175",
  "k": 3,
  "threshold": 0.76,
  "latency_ms": 68.1,
  "gated_out": true
}
```

**`reason` values** (non-exhaustive): `timeout`, `embed error: …`,
`no eligible candidates`, `gate_probability < threshold`,
`decider route=tool_enum (not candidate)`, `decider route=abstain (not candidate)`,
`tool '<name>' not on read-only allowlist`,
`args do not map to window anchors: [...]`,
`cannot load selected candidate args`, `assist unavailable` (503).

**Non-200** — Wakil treats ANY non-200 as "no proposal" (A5):
- `503` — kill switch on, or the decider/gate was down at startup.
- `400` — malformed request body.
- `401` — bad token.

## The act/abstain policy (AMENDMENT)

`action` is non-null **only if ALL** hold:

1. `gate_probability >= 0.76` (GATE-1, the six retrieval features at k=3 with
   exact f4; **not** D-wlm's confidence — that is ECE 0.21, non-monotone).
2. D-wlm route is `candidate` (not `abstain`, not `tool-enum`).
3. The selected candidate's substituted args map onto anchors in the window.
4. The proposed tool is on the read-only allowlist (never a write op).

The gate predicts **gold-in-block**, not correctness or safety. Test gated
accuracy is 69.1% (below the 70% dev-selection rule) — surfaced, not smoothed.

## Read-only allowlist (A2)

Logged with every acting response. `run_shell` is **not** on it (a neighbour's
literal command is foreign args and may mutate). Write ops (memory_put, file
writes, skill/memory mutations, staging writes) always abstain.

## Logging (A3)

Every call + response + provenance is persisted to `assist_log` and emitted as
a grammar event (`assistant_turn`) in the `wakil-assist:<session_id>`
pseudo-session with `client.name="wakil-assist"`. A random 10% of gated-OUT
decisions are also emitted with `client.name="wakil-assist-shadow"` so false
rejections stay measurable (without executing the rejected proposal).

## Metrics (A4)

`GET /v1/adapters` includes an `assist` block: assist rate, act rate, agreement
with the main model's subsequent action when Wakil ignored the proposal, per
cluster; plus gated-out shadow agreement.

---
version: assist-1
sha256: 260c861e1e33b33384504f72c0ffca56df6d019bb4e4c0301d57860979c0cd12
fetched_at: 2026-09-17
