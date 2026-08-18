# Proposal: README Refactor — Split + Differentiators Section

> Reviewed with Mashūra (3-model panel: gpt-5.6-sol, claude-fable-5, glm-5.2).
> Verdict: directionally sound, corrections folded in below. See "Mashūra
> review summary" at the end.

## Problem

The current `README.md` is 36.9 KB and reads like a justification document
from the ilm-proxy era rather than a technical description. Most capabilities
it describes at length (memory, staging, subagents, counsel, workflow,
browser, LSP) are now built into wakil itself — yet the README still
positions the ilm proxy as the central value proposition.

Additionally, there is **no section explaining what makes wakil different**
from other terminal coding agents. The answers — Go binary, sandbox
isolation, model-agnostic endpoints, built-in memory, per-command gate —
are not prominently listed anywhere.

## Current state (with evidence)

| Claim in README | Evidence | Status |
|---|---|---|
| Two endpoint kinds: `openai` vs `ilm-proxy` | `internal/config/config.go:34` (Kind field), `internal/proxy/client.go:362` (KindIlmProxy) | ✅ accurate |
| ilm-proxy: X-Ilm-* headers, /v1/ilm/limits, backend routing | `internal/proxy/client.go:843-853` (X-Ilm headers), `internal/agent/ctxlimit.go:192` (/v1/ilm/limits), `internal/agent/learn.go` (/learn) | ✅ accurate but ilm-proxy is optional |
| **Legacy/env-var-only path synthesizes ilm-proxy kind** | `internal/config/config.go:1067-1081` (no endpoints block → `Kind: EndpointKindIlmProxy`) | ⚠️ confirmed — Quickstart ambiguity is real |
| Two-tier durable memory (SQLite, propose→promote, per-workspace) | `internal/memory/store.go:56` (StatusProposed), `:132` (workspaceRoot), `:702` (Promote) | ✅ built into wakil, endpoint-independent |
| Staging kvr store | `internal/staging/client.go` | ✅ built-in (but auto-disabled in direct mode) |
| Counsel/mashura: panel/fallback/fusion/debate | `internal/counsel/oracle.go:347` (RunPanel), `:363` (fusion), `:387` (fallback), `:382` (debate) | ✅ debate mode EXISTS but is undocumented in README |
| /plan workflow state machine | `internal/workflow/workflow.go:17-24` (WorkflowPhase: Gather→Plan→Review→Present→Implement→Done) | ✅ accurate |
| Subagent 3-tier capability | `internal/tools/tools.go:286-302` (CapabilityDiscovery/Edit/Tools) | ✅ accurate |
| LSP code intelligence | `internal/lsp/` (manager.go, tools.go, protocol.go) | ✅ accurate (gopls is the only tested server) |
| Headless browser | `internal/browser/` (manager.go, tools.go) | ✅ accurate |
| No "why wakil" / differentiators section | grep for 'claude code\|differentiator\|why wakil' across README, docs/, SECURITY.md → no matches | ❌ missing |

## What's stale / justification-flavored

1. **Architecture diagram** (README ~lines 10-14): positions ilm proxy as an
   equal branch to "OpenAI-compatible endpoint", implying it's required for
   memory/grounding. Durable memory + staging are built into wakil (T2 SQLite
   is host-side, `internal/memory/store.go:3`). The proxy adds *server-side*
   recall/routing but is not required.

2. **Three Quickstart options** (A/B/C): Option C (ilm proxy) is presented as
   a peer to A and B, but it's a niche optional backend. Should be a link.

3. **"How state works" section** (~lines 480-520): ~80% describes ilm-proxy-
   specific routing (task path, memory/meta path, `ilm-no-memory-write`
   metadata, eventually-consistent recall). This is proxy-behavior docs.

4. **Security section** (~lines 120-180): partially duplicates `SECURITY.md`
   BUT also contains README-only material (gated vs ungated tool
   classifications, `/auto` behavior, subagent consent inheritance, memory
   injection caveats). **Must not be lost** — see migration plan.

5. **`ILM_*` env vars leading in Quickstart**: reinforces proxy-era framing.
   New Quickstart should lead with `WAKIL_*` vars and/or explicit config.

## Proposed structure

### `README.md` (slim — target measured, not estimated)

```
# wakīl
[badges]

<one-paragraph technical description: terminal-native coding agent, Go binary,
OpenAI-compatible endpoints, Docker sandbox, per-command confirmation gate>

## Why wakil?

<NEW section — 5-6 bullet differentiators, each one line. See below.>

## Quickstart

<Single track: build + docker image + explicit endpoints config block with
"kind": "openai". NO env-var-only path (it synthesizes ilm-proxy kind —
confirmed at config.go:1067-1081). Link to docs for direct mode and ilm proxy.>

## Requirements

<keep the 3-row table>

## Architecture

<updated diagram (see below) — ilm-proxy as optional endpoint/gateway, NOT
an equal branch or "sidecar".>

## Documentation

<links to split docs — see below>

## Project layout

<keep the existing tree>

## Contributing

<short paragraph + link to CONTRIBUTING.md>

## Security

<short paragraph: confirmation gate, sandbox is convenience-grade (link
SECURITY.md), direct mode has no isolation. WARNING callout for untrusted
tasks. Link to SECURITY.md for full threat model.>

## License

<keep>
```

### Updated architecture diagram

```text
you → wakil → configured endpoint
                ├─ OpenAI-compatible model server (llama.cpp, OpenRouter, vLLM, Ollama)
                └─ optional ilm-proxy → routed backend/model
                                     (server-side recall, /learn, backend routing)
```

The proxy is an **optional endpoint/gateway** (not a "sidecar" — it's a
mutually exclusive endpoint choice, not an adjacent service).

### New/expanded docs files

| File | Content moved from README | Notes |
|---|---|---|
| `docs/configuration.md` | Configuration: flags table, config-only fields, endpoints schema, agent prompt, execution modes, sessions | The two big reference tables |
| `docs/endpoints.md` | Endpoint kinds, `openai` vs `ilm-proxy`, switching, `/backend` `/model`, context-limit discovery, **"How state works"** (proxy routing, /learn, eventually-consistent recall) | Proxy behavior gets its own home — NOT buried in features |
| `docs/tools.md` | Tools table, subagent tabs, subagent 3-tier capability, MCP servers, **per-tool gating classification** (moved from README security section) | Tool reference + gating table |
| `docs/tui.md` | TUI commands, keybindings, copying text over SSH (OSC 52) | SSH clipboard is niche — belongs here |
| `docs/workflows.md` | `/plan` workflow: phases, review behavior, oracle modes | Split from features to avoid monolith |
| `docs/features.md` | **Short index only** — links to per-feature docs, NOT a monolith. One-line descriptions of LSP, browser, counsel, web search, cost sidebar, memory, staging, context management | Index, not content |
| `SECURITY.md` | **Updated**: merge README-only security material (gated/ungated classification, `/auto` behavior, subagent consent inheritance, memory injection, destructive-command gating) into existing threat model | README links here |
| `docs/remote-provisioning.md` | Already exists — README links to it | No content change |

**Content completeness mapping** (all current README sections → destination):

| Current README section | Destination |
|---|---|
| Requirements | README (keep) |
| Status | README (keep, 1-2 lines) |
| Quickstart (A/B/C) | README (single track, explicit config) |
| Security and the confirmation gate | `SECURITY.md` (updated) + README (summary) |
| Configuration | `docs/configuration.md` |
| The TUI | `docs/tui.md` |
| Tools | `docs/tools.md` |
| Optional features (LSP, browser, /plan, counsel, search, cost) | `docs/workflows.md` (plan) + `docs/features.md` (index → per-feature docs) |
| Memory and staging | `docs/features.md` → existing `docs/memory.md`, `docs/staging.md` |
| How state works | `docs/endpoints.md` (proxy-specific routing) |
| Testing | `CONTRIBUTING.md` (already covers build/test; deduplicate) |
| Project layout | README (keep) |
| Contributing | README (summary) + `CONTRIBUTING.md` (existing) |
| Remote provisioning | README (link) → `docs/remote-provisioning.md` (existing) |
| License | README (keep) |

### The "Why wakil?" section (corrected per Mashūra)

5-6 bullets max (not 8-11 — a sharper positioning section). Each claim
qualified to what the code/tests support:

```markdown
## Why wakil?

- **Docker sandbox isolation** — convenience-grade isolation with read-only
  rootfs, dropped capabilities, and resource limits. The host Docker socket
  is opt-in and off by default. (See SECURITY.md for the threat model.)
- **Per-command confirmation gate** — workspace file mutations and command
  execution require confirmation by default; destructive operations remain
  gated in auto mode. Read-only tools are ungated. (See SECURITY.md.)
- **Model-agnostic** — works with OpenAI-compatible Chat Completions
  endpoints tested with llama.cpp, OpenRouter, and vLLM. No vendor lock-in;
  bring your own local or hosted model. The optional ilm proxy adds
  server-side recall and routing but is not required.
- **Cross-session, per-workspace memory** — built-in SQLite store with
  propose→promote review, provenance tracking, and per-workspace isolation.
  No external database required. (See docs/memory.md.)
- **Subagent delegation** — three capability tiers (discovery / edit / tools)
  with parallel dispatch and a writer lock for edit-tier serialization.
- **Multi-model counsel (Mashūra)** — on-demand second opinions from external
  models. Panel, fallback, and fusion modes. Keys read at call time.
```

**Explicitly excluded from differentiators** (per Mashūra — unverified or
overstated):
- ~~"single static binary"~~ → not verified (CGO-dependent); say "single Go
  executable, no Node.js/Python runtime" if confirmed via `ldd`/build config
- ~~"fast startup, low memory"~~ → no benchmarks; remove
- ~~"runs anywhere Go compiles"~~ → too broad; Docker/TUI/browser are
  platform-dependent
- ~~"tool execution is confined"~~ → overstated; direct mode runs on host,
  `open_url` runs on host, SECURITY.md says "convenience-grade"
- ~~"every write prompts"~~ → `/auto` changes this; subagents inherit
  consent; `memory_put` is ungated
- ~~"no telemetry — stays on your machine"~~ → prompts/code/output go to
  configured endpoints; rescope to "no first-party usage telemetry" if
  verified via code audit
- ~~"debate mode"~~ → exists in code but undocumented; document in
  `docs/features.md` first, THEN add to differentiators if desired

**Additional differentiators to consider** (from Mashūra, if space allows):
- Session persistence + `/handoff` (distinctive)
- MCP tool support (stdio + HTTP)
- Context management (compaction, `/maxctx`, backend-truth sizing)
- Mid-session endpoint/model switching

## Migration plan

1. **Extract content** into `docs/configuration.md`, `docs/endpoints.md`,
   `docs/tools.md`, `docs/tui.md`, `docs/workflows.md`. Update `SECURITY.md`
   with README-only security material. Semantic-preserving moves where
   possible; explicit rewrites for proxy framing and corrected claims.
2. **Rewrite `README.md`** to slim structure. Add "Why wakil?" section.
   Update architecture diagram. Reduce Quickstart to a single track using
   an explicit `endpoints` config block with `"kind": "openai"`.
3. **Update `CONTRIBUTING.md`** line 109 ("See README.md for full project
   layout and feature documentation") → point at `docs/features.md` for
   features, keep project layout in README.
4. **Check all cross-references**: `git grep` for README anchor links
   (`#configuration`, `#security-and-the-confirmation-gate`, `#endpoints`,
   `#lsp-code-intelligence`, `#commands`) across all `.md` files,
   `config.example.json`, `prompts/agent.txt`, and error strings in
   `internal/`/`cmd/`. Update or add redirect pointers.
5. **Verify**: measure README size (`wc -l`, `wc -c`); run a markdown link
   checker (or scripted grep of `](docs/` and `](#` targets); smoke-test the
   Quickstart config against a real endpoint.

## Edge cases / backward compat

- **Existing README anchors** will break. Mitigation: keep compatibility
  headings (`## Configuration`, `## Security`) that point to new docs, at
  least for one transition. `git grep` for `#anchor` references first.
- **Env-var-only Quickstart no longer recommended**: the legacy path
  synthesizes `ilm-proxy` kind (`config.go:1067-1081`), sending proxy-shaped
  requests to non-proxy servers. New Quickstart uses explicit config. This
  is a docs change, not a code change — but consider whether a follow-up
  card should change the synthesized default to `openai` kind.
- **`/learn` is proxy-only**: the TUI command refuses client-side on `openai`
  endpoints. Document this clearly in `docs/endpoints.md`.
- **Staging auto-disabled in direct mode**: note in `docs/staging.md` (already
  documented in config table).
- **Debate mode**: exists in code (`oracle.go:382`) but undocumented. Either
  document in `docs/features.md` or leave out of differentiators.
- **CONTRIBUTING.md**: must be explicitly updated (not just mentioned).

## Acceptance criteria

1. Every current README section is mapped to a new destination (see table
   above) or explicitly deleted with rationale.
2. The README Quickstart uses an explicit `endpoints` block with
   `"kind": "openai"` — no env-var-only path that synthesizes `ilm-proxy`.
3. No differentiator claim is unsupported: each is either backed by
   code/tests/build output, or qualified as platform/provider-dependent.
4. README does not claim all writes are gated; security summary links to
   SECURITY.md for the full gating table.
5. README states that configured external endpoints receive task content
   (prompts, code, tool output).
6. All repository-local markdown links and anchors resolve (verified by
   link checker or scripted grep).
7. `git grep` finds no stale links to removed anchors.
8. `CONTRIBUTING.md` and `config.example.json` cross-references updated.
9. README byte/line counts are measured, not estimated.
10. Quickstart config example is checked against `config.example.json` and
    actual request behavior.

---

## Mashūra review summary

**Panel**: gpt-5.6-sol, claude-fable-5, glm-5.2 (3 models, fusion-style review)

**Consensus**: Directionally sound. Split and re-framing are correct. Key
corrections folded into this proposal:

1. **"Optional sidecar" → "optional endpoint/gateway"** — the proxy is a
   mutually exclusive endpoint choice, not an adjacent service.
2. **Quickstart endpoint-kind ambiguity is real** — confirmed at
   `config.go:1067-1081`: legacy path synthesizes `ilm-proxy` kind. New
   Quickstart must use explicit `endpoints` block with `"kind": "openai"`.
3. **Don't leave SECURITY.md unchanged** — README-only security material
   (gated/ungated classification, `/auto`, subagent consent, memory
   injection) must be merged into SECURITY.md before removing from README.
4. **Differentiators are overstated** — "static binary", "fast startup",
   "tool execution is confined", "every write prompts", "any
   OpenAI-compatible", "no telemetry", "keys never stored" all need
   correction or removal. Reduced to 5-6 defensible bullets.
5. **`docs/features.md` is a dumping ground** — split into per-feature docs
   (matching existing `docs/memory.md` / `docs/staging.md` pattern).
   `features.md` becomes a short index, not a monolith. Proxy behavior gets
   its own `docs/endpoints.md`.
6. **Complete content inventory** — Status, Testing, Remote provisioning,
   /learn, backend-truth context sizing all need explicit destinations
   (added to mapping table above).
7. **Better acceptance criteria** — `gofmt`/`go test` don't validate docs.
   Replace with link checking, anchor grep, Quickstart smoke test, and
   measured size targets.

**One disagreement among models**: whether the env-var Quickstart needs a
code change (changing synthesized default to `openai` kind) or just a docs
change (recommending explicit config). This proposal takes the docs-only
path but flags the potential follow-up code change as a separate card.
