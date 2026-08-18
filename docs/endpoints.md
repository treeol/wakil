# Endpoints and State

wakil supports two endpoint kinds. The ilm proxy is an **optional
endpoint/gateway** — not a sidecar or adjacent service, but a mutually
exclusive endpoint choice. Built-in durable memory and staging work on all
endpoint kinds; the proxy adds server-side recall, routing, and `/learn`.

## Endpoint kinds

- `openai` — any plain OpenAI-compatible Chat Completions server
  (llama.cpp server, OpenRouter, vLLM…). `model` is **required** and is the
  literal string sent in requests. No ilm-specific headers or body fields are
  sent.
- `ilm-proxy` — the ilm proxy with memory/grounding. `model` defaults to the
  proxy alias `ilm`; backend prefix-routing and `X-Ilm-*` headers apply.

See [Configuration → Endpoints](configuration.md#endpoints) for the config
schema and `config.example.json` for a fully commented reference.

**Backward compatibility:** configs without an `endpoints` block keep working
unchanged — the top-level `base_url` (or `host`+`port`) synthesizes a single
`ilm-proxy` endpoint with model `ilm`, byte-identical request shape to before.

## Switching at runtime

`/backend <name>` switches endpoints (on `openai`-kind endpoints), and
`/model <name>` switches models — both re-resolve context limits.

## Backend-truth context sizing

At startup (and on every `/backend` / `/model` switch) wakil resolves the
real per-slot context window (`n_ctx`) and sizes the context meter, pressure
warnings, and compaction against it — with a loud fallback warning when
nothing answers. Resolution depends on endpoint kind:

- `ilm-proxy` — `/v1/ilm/limits` (includes the proxy's pre-computed
  `usable_ctx`), then `/props`.
- `openai` — `/props` for llama.cpp servers; for `openrouter.ai` the
  configured model is resolved against OpenRouter's public model registry.

## How state works

### On `openai` endpoints

State is simple: the standard agent loop runs against a stateless server —
assistant `tool_calls` → execute → `role:"tool"` result → resend → final
answer. wakil keeps a **bounded client-side transcript**, compacting older
turns into a running summary *(last N turns verbatim + summary)*. There is
no server-side memory; `/learn` refuses client-side because an `openai`
endpoint cannot persist the fact.

Client-side **durable memory** (T2) works on all endpoints — the SQLite
store is host-side and endpoint-independent. `memory_put`,
`memory_search`, and the propose→promote flow operate regardless of
whether the backend is an ilm proxy or a plain OpenAI server. See
[Memory and staging](features.md#memory-and-staging).

### On `ilm-proxy` endpoints

The proxy additionally routes by **message content**; statefulness differs by
path.

**Task path** *(normal requests with `tools`)* — standard OpenAI passthrough
to a llama.cpp Qwen backend. Same clean agent loop and bounded transcript as
above.

**Memory / meta path** *(`### learn this`, `remember`, `what have you
learned`, `forget …`)* — short-circuits server-side, returns plain assistant
text *(acks / lists)* regardless of `tools`. Resent history is ignored for
recall; memory lives server-side, keyed by `metadata.chat_id`.

> Memory recall is **eventually consistent** — a fact may not be recallable
> immediately after `### learn this`. Proxy characteristic, not a wakil bug.

### `/learn` — proxy-only

`/learn` asks the proxy to synthesise a fact to remember. It works on
`ilm-proxy` endpoints only — on `openai` endpoints it refuses client-side
instead of faking success.
