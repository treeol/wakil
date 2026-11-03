# Reasoning / Thinking Budget Audit

Per-call-site audit of how reasoning/thinking fields, max_tokens, and usage
are sent and parsed today. Covers the main LLM client (proxy), the Mashūra
counsel path (oracle + review dispatcher), config, the agent session layer,
cost accounting, and trace.

## 1. Main LLM client — `internal/proxy/client.go`

### Request body (wire)

| Field | Struct field | Wire key | Source | Notes |
|---|---|---|---|---|
| max_tokens | `Client.MaxTokens *int` (`:518`) | `max_tokens` (`:878/:918`) | Endpoint config or default | nil + `KindOpenAI` → default 32768 (`:895–899`) |
| reasoning | `Client.Reasoning *ReasoningConfig` (`:525`) | `reasoning` (`:881/:921`) | `applyReasoningToClientLocked` (agent) | nil = omitted entirely |

`ReasoningConfig` (`:303–308`):

```go
type ReasoningConfig struct {
    Effort    string `json:"effort,omitempty"`     // max|xhigh|high|medium|low|minimal|none
    MaxTokens int    `json:"max_tokens,omitempty"` // reasoning token budget (Anthropic-style)
    Enabled   bool   `json:"enabled,omitempty"`    // enable with defaults
    Exclude   bool   `json:"exclude,omitempty"`    // hide reasoning from response
}
```

Sent verbatim as `body.reasoning`. Only meaningful for `KindOpenAI` endpoints
(OpenRouter, reasoning-capable local servers); `KindIlmProxy` ignores it.

**Provider mapping today**: OpenRouter only. The `reasoning` object is passed
through as-is — no Anthropic-native `thinking` mapping, no OpenAI-native
`reasoning_effort` mapping.

### Streaming

- SSE loop: `:1110–1214`; `[DONE]` at `:1117`.
- Reasoning deltas (`reasoning_content` / `reasoning`) forwarded to
  `reasoningSink` only, **never stored** in history (`:1173–1188`).
- Malformed chunk → fatal abort (`:1133–1145`).
- Mid-stream error chunk → `wrapStreamErr` (`:1150–1152`).

### Usage parsing

`streamChunk.Usage` (`:334–352`):

| Wire field | `UsageStat` field | Line |
|---|---|---|
| `prompt_tokens` | `InputTok` | `:1157` |
| `completion_tokens` | `OutputTok` | `:1158` |
| `completion_tokens_details.reasoning_tokens` | `ReasoningTok` | `:1160` |
| `prompt_tokens_details.cached_tokens` | `CachedTok` | `:1163` |
| `cache_creation_input_tokens` | `CacheWriteTok` | `:1165` |

`UsageStat` (`:365–378`): `InputTok`, `OutputTok`, `ReasoningTok`,
`CachedTok`, `CacheWriteTok`, `Exact bool`.

No-usage fallback: `ApproxTokens` (`:1278–1281`, ~4 chars/token).

`LastUsage()` (`:632`) returns the `UsageStat` from the most recent Stream call.

### Timeouts

| Field | Default | Mechanism | Line |
|---|---|---|---|
| `StreamIdleTimeout` | 120s (config `stream_idle_timeout_seconds`) | `idleReader` (`:1583–1656`) | `:622`, `:1101` |
| ResponseHeaderTimeout | not set (intentional) | — | comment `:39–44` |

No total cap beyond the idle timeout. Non-200 classified by `isFatalStatus`
(`:47`): 429/408/529 retryable.

### finish_reason=length

Three paths, all `ErrBackendFatal` with an "increase max_tokens" hint:
- empty content: `:1267–1270`
- truncated tool-call args: `:1308–1316`
- after tool calls: `:1341–1344`

---

## 2. Mashūra counsel — `internal/counsel/oracle.go`

### Anthropic path (`CallOracle` / `CallOracleURL`)

**Request body** (`oracleReq` `:77–82`):

| Field | Wire key | Source |
|---|---|---|
| Model | `model` | `cfg.OracleModel` |
| MaxTokens | `max_tokens` | `cfg.OracleMaxTokens` |
| System | `system` | `oracleSystemPrompt` (const) |
| Messages | `messages` | question + oracleCtx |

**No reasoning/thinking field is sent.** The Anthropic-native `thinking`
parameter is not wired.

**Headers**: `x-api-key`, `anthropic-version: 2023-06-01` (`:165–168`).

**Timeout**: `cfg.OracleTimeoutSeconds`, default 300s (`:158–163`). Transport
dial/TLS 10s (`:37–38`). `ResponseHeaderTimeout` deliberately unset (`:39–44`).

**Non-streaming**: `doJSONPost` reads the whole response, capped at 4 MB
(`:51–72`).

### Response parsing

`oracleResp` (`:90–104`):

| Wire field | Go field | Used for |
|---|---|---|
| `content[].{type,text}` | `Content` | text extraction (`:196–200`) |
| `stop_reason` | `StopReason` | truncation check (`:221`) |
| `usage.input_tokens` | `Usage.InputTokens` | `OracleUsage.InputTokens` |
| `usage.output_tokens` | `Usage.OutputTokens` | `OracleUsage.OutputTokens` |

`OracleUsage` (`:108–123`):

```go
type OracleUsage struct {
    InputTokens       int64
    OutputTokens      int64
    WebSearchRequests int64
    FinishReason      string
}
```

**No reasoning tokens field.** Anthropic's `usage` for thinking models reports
separate `input_tokens` / `output_tokens` where output includes thinking
tokens — but `OracleUsage` does not surface them distinctly.

### Truncation

`stop_reason == "max_tokens"` → `IncompleteReplyError{Length: true}` with
"raise oracle_max_tokens" hint (`:221–243`). Thinking-block detection only
alters the remediation hint text (`:229–230`).

### OpenRouter path (`callOpenRouter` / `orChat`)

**Request body** (`orReq` `:664–671`):

| Field | Wire key | Source |
|---|---|---|
| Model | `model` | `model` arg |
| MaxTokens | `max_tokens` | `ccfg.MaxTokens` |
| Messages | `messages` | system + user |
| Plugins | `plugins` | fusion only |
| Tools | `tools` | server tools |
| MaxToolCalls | `max_tool_calls` | server tools |

**No reasoning field.** The OpenRouter `reasoning` parameter is not wired.

`orChat` (`:838`): timeout from `ccfg.TimeoutSeconds` (default 300s, `:849–852`).
Usage from `orResp.Usage` (`:879–885`) — `prompt_tokens`, `completion_tokens`,
`server_tool_use.web_search_requests`. **No `reasoning_tokens` parsing.**

`finish_reason == "length"` → `IncompleteReplyError{Length: true}` (`:900–901`).

---

## 3. Review dispatcher — `internal/counsel/review.go`

### Budget arithmetic (`dispatchReview` `:561`)

| Constant | Value | Role |
|---|---|---|
| `defaultMinAnswerTokens` | 4096 | answer-budget floor for a review |
| `answerReserveTokens` | 512 | held back from context window |
| `contextHeadroom` | 0.90 | share of context window usable |
| `overlapTokens` | 200 | carried into next part |
| `reviewCharsPerToken` | 3 | conservative bytes/token |
| `maxPartsPerMember` | 24 | max sequential calls per member |

```
usable        = ctxTokens * 0.90
inputBudget   = usable - floor - answerReserveTokens
docBudgetChars= inputBudget * 3 - overheadChars
maxTok        = min(answerWant, usable - inTok - answerReserveTokens)
             = max(maxTok, floor)   // floor guaranteed by input budget
```

`answerWant = max(configuredMax, floor)`. `configuredMax` comes from
`ModelLimits.ReviewMaxTokens` (per-model override) → `ccfg.MaxTokens`
(panel-level) → `floor` (4096).

### Per-part send (`sendFunc`)

Each part calls `send(ctx, question, text, maxTok)`. `maxTok` is the **answer
budget only** — no reasoning budget is added. The `send` closure (in
`runMember`) sets `ccfg.MaxTokens = maxTok` and calls `callMember`, which
calls `callAnthropic` or `callOpenRouter`.

### Evidence-of-reading

| Concept | Location | Detail |
|---|---|---|
| `evidencePrefix` | `:40` | `"LAST SECTION READ:"` |
| `evidenceRe` | `:455` | matches the citation line |
| `sourceHeaderRe` | `:143` | `(\d+ lines)` suffix |
| `blockHeaderRe` | `:140` | `── label ──` |
| `headingRe` | `:150` | ATX markdown heading |
| `citationMatches` | `:499–513` | normalises markdown/trailing `(N lines)`; no substring rule |
| `extractEvidence` | `:459–473` | strips trailing citation from last 4 non-blank lines |
| `expectedEvidence` | `:515–524` | last heading + last block label |

`hasStructure` (`:575`): true when the briefing has headings or `── ` blocks.
`evidenceInstruction` (`:528`): tells the reviewer to cite the last heading or
block label. The check matches on the heading/label text, with the `(N lines)`
suffix stripped by `trailingParenRe` (`:492`) inside `citationMatches`.

**Code-only inputs**: a `.go` file block has label `"main.go (120 lines)"`
and no markdown headings. `expectedEvidence` returns `["main.go (120 lines)"]`.
The reviewer must cite `"main.go"` (after `trailingParenRe` strips `(120
lines)`). The `evidenceInstruction` says "the heading (or `── label ──` block
label)". For a code-only block, the last block label IS the expected evidence.

**VOID paths** (`:625–669`, `:545–554`):
- `IncompleteReplyError{Length: true}` → VOID "reply hit max_tokens"
- later-part failure → VOID "interrupted at part i/n"
- empty reply → VOID
- `voidAfterTruncation` wraps retry failures as VOID

---

## 4. Config — `internal/config/config.go`

| Field | Type | JSON key | Default | Line |
|---|---|---|---|---|
| `OracleMaxTokens` | `int` | `oracle_max_tokens` | 4096 (in `defaults()`) | `:453`, `:1061` |
| `OracleTimeoutSeconds` | `int` | `oracle_timeout_seconds` | 0 → 300s | `:456` |
| `MashuraMaxTokens` | `int` | `mashura_max_tokens` | 0 (alias) | `:472` |
| `MashuraModels` | `map[string]MashuraModelLimits` | `mashura_models` | `{}` | `:487` |
| `ReasoningBudgetTokens` | `int` | `reasoning_budget_tokens` | 4096 | `:373`, `:1045` |

`MashuraModelLimits` (`:910–913`):

```go
type MashuraModelLimits struct {
    ContextTokens   int // 0 = provider table / registry
    ReviewMaxTokens int // 0 = mashura_max_tokens
}
```

**Missing**: no `models.<model>.reasoning` config; no
`roles.{main,reviewer,subagent,check}.reasoning` config; no
`reasoning`/`thinking`/`effort` keys in `config.example.json`; no reasoning
documentation in `docs/configuration.md`.

`ReasoningBudgetTokens` (default 4096) is a **context-window reservation** —
headroom subtracted from the context limit for extended thinking. It is
unrelated to the `/thinking` runtime setting.

`config.example.json` has `oracle_enabled`, `oracle_model`,
`oracle_api_key_env`, `mashura_models`, `mashura_panels` — but no
`oracle_max_tokens`, `oracle_timeout_seconds`, or any reasoning keys.

---

## 5. Agent session layer — `internal/agent/`

| Concept | File:line | Detail |
|---|---|---|
| `/thinking` command | `commands.go:1099–1190` | session-scoped; persisted to repo-state; effort levels: max, xhigh, high, medium, low, minimal, none, off |
| `applyReasoningToClientLocked` | `app_state.go:300–313` | derives `Client.Reasoning` from `ReasoningEffort`/`ReasoningMaxTokens`; both empty → nil |
| `App.ReasoningEffort` | `app.go` (session field) | set by `/thinking` |
| `App.ReasoningMaxTokens` | `app.go` (session field) | set by `/thinking` |
| Subagent inheritance | `subagent.go:1295–1301`, `:1359–1360` | copies parent's effort/maxTokens onto child App; child's `prepareTurn` applies them |
| Repo-state persistence | `repostate.go:78–83`, `:468–476` | `ReasoningEffort`, `ReasoningMaxTokens` |
| `mashuraMaxTokensFor` | `mashura.go:1057–1073` | per-tool override → base `OracleMaxTokens`; check capped at 1024 |
| `PanelCallConfig` build | `mashura.go:242–253` | `MaxTokens`, `TimeoutSeconds`, `ModelLimits`, `MinAnswerTokens` — **no reasoning** |

---

## 6. Cost accounting — `internal/proxy/cost.go` + `internal/agent/app.go`

### `CostTracker.Record` (`cost.go:79`)

Records `inTok`, `outTok`, `cachedTok`, `cacheWriteTok`, `costUSD`, `priced`,
`confidence`. **No reasoning tokens.**

### `RecordOracleCostFor` (`app.go:1585`)

```go
usd, priced := a.Cfg.Costs.MashuraCost(model, u.InputTokens, u.OutputTokens)
```

Uses `InputTokens` + `OutputTokens` only. **Reasoning tokens (if any) are not
separated or priced.**

### `RecordInferenceCost` (`app.go:1454`)

```go
usd, priced = a.Cfg.Costs.InferenceCost(u.InputTok + u.OutputTok)
```

For local/modeled: `InputTok + OutputTok` (no reasoning split).
For external/exact: `externalInferenceCost` (`:1531`) uses `TokenDetail` for
cache splits but **no reasoning token pricing**.

### `costEntry` / `CostRow` (`cost.go:48–57`, `:110–120`)

No `ReasoningTok` field. The sidebar and trace show no reasoning breakdown.

---

## 7. Trace — `internal/trace/trace.go`

`Record` (`:29–50`) has `ReasoningTokens int64` (`:47`), populated by
`flushTraceTurn` (`app.go:1316`) from `u.ReasoningTok` — **main proxy client
only**. Mashūra calls do not write trace records with reasoning tokens
(`OracleUsage` has no reasoning field).

---

## 8. CLI / TUI

| Concept | Location | Status |
|---|---|---|
| `--think` CLI flag | `cmd/` | **does not exist** |
| `/think` alias | TUI | **does not exist** (only `/thinking`) |
| `/thinking` completion | `tui/complete.go:118, :325` | registered |
| Reasoning in `config.example.json` | — | **absent** |
| Reasoning in `docs/configuration.md` | — | **absent** |
| Reasoning in `AGENTS.md` | — | **absent** |

---

## Summary: what exists vs. what's missing

### Exists

- Main proxy client sends `reasoning` (OpenRouter-shaped) and parses
  `reasoning_tokens` from usage into `UsageStat.ReasoningTok`.
- `/thinking` session command; subagent inheritance; repo-state persistence.
- Trace records `ReasoningTokens` per main-client turn.
- Review dispatcher's answer-budget arithmetic (4096 floor, 90% headroom,
  chunked retry, VOID on truncation).

### Implemented (this task)

1. **Per-model reasoning config** (`mashura_models.<model>.reasoning =
   {effort, max_tokens}`) and **per-role overrides**
   (`mashura_roles.{reviewer,check}.reasoning`). See `config.go`:
   `ReasoningSpec`, `MashuraModelLimits.Reasoning`, `MashuraRoleConfig`,
   `Config.MashuraRoles`.
2. **Provider mapping**: OpenRouter `reasoning` (sent via `orReq.Reasoning`);
   Anthropic-native `thinking` (`{type:"enabled", budget_tokens:N}`) sent via
   `oracleReq.Thinking` in `callOracleWithReasoning`. Budget derived from
   `ReasoningBudgetFor(spec)` via `EffortToBudget` table.
3. **Additive budgets**: `max_tokens = reasoning budget + answer budget` on
   the wire. The answer budget (`ccfg.MaxTokens` / `cfg.OracleMaxTokens`) is
   never reduced. Implemented in `callOracleWithReasoning` (Anthropic) and
   `callOpenRouter` (OpenRouter).
4. **Usage**: `OracleUsage.ReasoningTokens` added; parsed from OpenRouter
   `completion_tokens_details.reasoning_tokens` in `orChat`. Shown in the
   Mashūra footer (`reasoning N / answer M`) via `ReviewLog.ReasoningTokens` /
   `ReviewLog.AnswerTokens` in `logFooter`. Accumulated in all three usage
   merge sites in `review.go` (run, whole-document retry, chunked retry) and
   in `runDebate` (round-2 merge).
5. **Runtime control**: `/think` alias (dispatches to the same handler as
   `/thinking`); `--think` CLI flag (applied at repo-state restore, overrides
   saved preference); both documented in `config.example.json` and
   `docs/configuration.md`.
6. **Evidence-of-reading for code-only**: instruction text updated to mention
   "for a source file, cite its path". The matcher (`citationMatches`) now
   strips ALL trailing parenthetical groups (not just one) via repeated
   `trailingParenRe` application, so range labels like
   `"foo.go (lines 10–20) (15 lines)"` are correctly matched by a bare
   `"foo.go"` citation. Tests:
   `TestReviewCodeOnlyCitationNotVoid` (correct citation → not VOID),
   `TestReviewCodeOnlyRangeLabelCitation` (range label → not VOID),
   `TestReviewCodeOnlyCitationWrongPathVoid` (wrong citation → VOID).

### Mashūra round 2 corrections applied

- **D3 fix**: Per-model reasoning is now resolved per-member in
  `counsel.runMember` via `ModelLimits[prefixed].Reasoning`, not as a
  single panel-wide value from `mashuraReasoningFor`. The agent's
  `mashuraReasoningFor` now returns only the role-level default.
  `mashuraModelLimits()` threads `Reasoning` through to `counsel.ModelLimits`.
- **D4 fix**: `"off"`/`"none"` effort no longer reaches the wire. Both
  `callOracleWithReasoning` and `callOpenRouter` normalize `off`/`none` to
  nil (omit reasoning entirely) before building the request body.
- **D2 fix**: `--think` is now applied even when no saved repo-state exists
  (fresh workspace). `RestoreRepoState` and `RestoreRepoStateResume` call
  `applyThinkFlagLocked` when `st == nil`. `ThinkEffort` is validated at
  load time in `LoadConfig`.
- **D5 fix**: `citationMatches` strips all trailing parenthetical groups,
  not just one, so range-labelled sources match correctly.

### Mashūra round 3: streaming + defaults (this iteration)

- **Streaming implemented**: `callOracleWithReasoning` and `orChat` now send
  `stream: true` and parse SSE responses (Anthropic: message_start,
  content_block_delta, message_delta, message_stop; OpenRouter:
  chat.completion.chunk with trailing usage). The 300s total timeout is
  replaced by an idle deadline (120s) + total cap (30min). A length stop still
  goes through the existing truncation/VOID path. Test:
  `TestDebugSSE` (Anthropic SSE parsing), `TestCallOracleURL_*` (all updated
  to SSE), `TestCallOpenRouter_*` (all updated to SSE).
- **Defaults implemented**: Built-in default effort levels are applied when
  no explicit setting is configured: main = `medium`, reviewer = `medium`
  (max 8192), check = `none`, subagent = `low`. The main model default is
  applied in `applyReasoningToClientLocked`; the Mashūra defaults in
  `mashuraReasoningFor`; the subagent default in `subagent.go`. `/thinking off`
  sets effort to `"none"` to explicitly disable. Documented in
  `docs/configuration.md` ("Reasoning defaults").

### Still deferred

- **OpenAI-native `reasoning_effort`** mapping: only OpenRouter and
  Anthropic-native are mapped today. OpenAI-native would need a separate
  wire field.
- **D1 (context-window overflow from additive budget)**: the 10%
  `contextHeadroom` masks this for typical models/budgets, but a formal fix
  (passing the reasoning budget into `dispatchReview`'s partition sizing) is
  still deferred.
- **Cost accounting for reasoning tokens**: `RecordOracleCostFor` passes
  `OutputTokens` (which includes reasoning for Anthropic, or separately for
  OpenRouter) to `MashuraCost`. No separate reasoning pricing is applied — the
  output rate covers it. This avoids double-counting (Anthropic) but does not
  split reasoning vs answer for per-token pricing (OpenRouter).

### Mashūra round 4: reviewer ceiling + SSE multi-line assembly

- **Reviewer reasoning ceiling enforced**: `runMember` now clamps the
  effective reasoning spec through `clampReviewerReasoning`, which caps
  `max_tokens` at `maxReviewerReasoningTokens` (16384) regardless of config.
  This covers both per-model overrides (`mashura_models.<model>.reasoning`)
  and the role-level default. The original spec is never mutated. Test:
  `TestClampReviewerReasoning` (unit), `TestRunMemberReviewerReasoningCeiling`
  (integration — verifies the wire request's `thinking.budget_tokens` is
  clamped).
- **SSE multi-line `data:` assembly**: `streamRequest` now assembles
  multi-line `data:` events per the SSE spec — multiple `data:` lines within
  one event are joined with `\n` before dispatching to the parser, and a blank
  line terminates the event. Previously each `data:` line was parsed
  independently, which silently dropped JSON payloads spanning multiple lines.
  Test: `TestSSEMultiLineDataEvent` (two-line data: event → joined → parsed).