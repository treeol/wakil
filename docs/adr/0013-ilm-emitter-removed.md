# 0013 — ilm emitter removed

- **Status:** Accepted
- **Date:** 2026-11-08
- **Origin:** CHANGELOG 0.6.0, B2 (exit latency).

## Context

The rubin timeline (`.wakil/perf/rubin-khatam-b2.json`) put exit at 5132 ms.
Of that, `close ilm emitter` was **5002 ms** — essentially the whole exit. The
ilm-stack ingestion path had two independent problems:

- the background sender's `stop()` waited up to 5 s for a flush, and the flush's
  POST used a 30 s HTTP timeout, so an unreachable ilm endpoint pinned the exit
  path to the 5 s cap;
- the emitter was opt-in (`ilm_stack.mode`), and the ilm-stack ingestion service
  is no longer used by anyone.

A bounded-flush-plus-spool design (one 200 ms flush attempt, remainder written to
a local spool for the next start) was drafted and then discarded: the requirement
is not "make the emitter exit fast" but "stop shipping a subsystem nobody uses".
Keeping a network client, a durable queue, an HTTP sender, a redaction engine and
an assist client — all behind no flag — for a service that is not consumed is
dead weight with an exit-path failure mode.

## Decision

Remove the ilm emitter entirely, keeping nothing behind a flag:

- `internal/ilm` (emitter, durable queue, background sender, redaction, assist
  client, allowlist, payloads) and `internal/agent/assist.go`;
- the `ilm_stack` config block, its `ILM_STACK_*` / `ILM_MODE` / `ILM_QUEUE_PATH`
  / `ILM_BATCH_SIZE` / `ILM_OUTPUT_LIMIT` / `ILM_ASSIST_AUTO` env vars, and the
  `ilm_stack.mode` validation;
- the `/assist` TUI command and its status segment, the `AssistEnabled` /
  `AssistAuto` fields in the info snapshot, and the `assistEnabled` / `assistAuto`
  reads in the wiring facade;
- every emitter call site in `internal/agent` (session start/end, user/assistant
  turn, tool call/result, memory op, mashura call, assist event) and the
  `close ilm emitter` step in `facade.Close` / `CloseResources`;
- `sessionhost.Host.NotifySessionNote` (its only caller, the Telegram approval
  consumer, was already removed) and its tests;
- `docs/ilm-stack/` and the `/assist` line in `docs/tui.md`.

The `ilm-proxy` **endpoint kind** is unrelated and stays: it names Wakil's own
inference proxy (memory/grounding, `X-Ilm-*` headers, `/v1/ilm/*` routes). The
`ILM_MODEL` / `ILM_BASE_URL` / `ILM_API_KEY` / `ILM_*` legacy env aliases for the
endpoint are likewise unrelated and stay.

## Consequences

- Exit no longer has an ilm flush step. The remaining `facade.Close` and
  `CloseResources` teardown paths were audited for fixed network timeouts: MCP
  server close is ~9 ms on the rubin timeline and the host-session close is
  bounded by a 10 s context that returns as soon as the cancel is acknowledged;
  no other exit-path step waits on a network timeout > 200 ms.
- The assist mode (server-proposed tool calls before the model runs) is gone with
  the package. It was reachable only through `ilm_stack.mode=assist`, which the
  ilm-stack service no longer serves.
- `deadcode -test ./...` returns to 0; the removal does not leave orphaned
  helpers (the package's exports were used only by the package and the removed
  call sites).
