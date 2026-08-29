# Discovery — Anthropic `cache_control` via OpenRouter

**Scope:** READ-ONLY mapping of the implementation surface for adding Anthropic
prompt-caching breakpoints (`cache_control`) on requests sent through OpenRouter.
No design, no code changes.

---

## 1. Content shape: `Message.Content` typing and serialization

### 1.1 The `Message` struct

**`internal/proxy/client.go:51-74`**

```go
type Message struct {
    Role       string     `json:"role"`
    Content    *string    `json:"content"`
    ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
    ToolCallID string     `json:"tool_call_id,omitempty"`
    Name       string     `json:"name,omitempty"`
    Pinned bool `json:"-"`  // local-only, never serialized
}
```

**`Content` is `*string`** — a pointer to a plain string. The JSON tag is
`"content"` with **no `omitempty`**, so `null` round-trips faithfully
(intentional: llama-server emits `"content":null` on tool-call turns; eliding
the field would cause KV-cache divergence).

This means **every message on the wire is `{"role":"...","content":"<string>"}`**.
There is no content-parts shape (`content: [{"type":"text","text":"..."}]`)
anywhere in the type system. Anthropic's `cache_control` lives on content
*parts* (`{"type":"text","text":"...","cache_control":{"type":"ephemeral"}}`),
which requires the array-of-parts form.

### 1.2 Marshal path (request → wire)

**`internal/proxy/client.go:447-457`** — `Stream` builds `reqBody`:
```go
reqBody := chatRequest{
    Model:         model,
    Stream:        true,
    StreamOptions: &streamOptions{IncludeUsage: true},
    Messages:      messages,   // ← []Message, directly from caller
    Tools:         tools,
    Temperature:   c.Temperature,
    ...
    CachePrompt:   c.CachePrompt,
}
```

**`internal/proxy/client.go:473`** — `json.Marshal(reqBody)` serializes to `raw []byte`.

**`internal/proxy/client.go:487`** — re-marshal after `trimToolResults` if the
byte guard triggers.

The `chatRequest` struct (`client.go:99-122`) types `Messages` as `[]Message`.
`json.Marshal` on `[]Message` produces `content` as a JSON string (or null),
never as an array of parts. **To send `cache_control`, the wire shape of
`content` must become an array of parts for marked messages.**

### 1.3 SSE parse (response → `Message`)

**`internal/proxy/client.go:133-169`** — `streamChunk` defines the wire format:
```go
type streamChunk struct {
    Choices []struct {
        Delta struct {
            Content          string `json:"content"`           // ← string, not parts
            ReasoningContent string `json:"reasoning_content"`
            ToolCalls        []struct{ ... } `json:"tool_calls"`
        } `json:"delta"`
        ...
    } `json:"choices"`
    Usage *struct{ ... } `json:"usage"`
}
```

**`Delta.Content` is `string`, not `[]ContentPart`.** Deltas arrive as plain
text strings. Accumulated into `content strings.Builder` at **`client.go:600`**.

**`client.go:649-653`** — final `Message` construction:
```go
msg := Message{Role: "assistant"}
if content.Len() > 0 {
    s := content.String()
    msg.Content = &s
}
```

**Responses never come back parts-shaped.** OpenRouter streams Anthropic
responses in the OpenAI chat-completions delta format (`content` as string
deltas). The SSE parse path would not need changes for cache support — it only
reads response deltas, and `cache_control` is a request-side directive.

### 1.4 Sites that read `*m.Content` as a string

Every consumer of message content uses `DerefStr(m.Content)` → `string`:

| Site | File:line | What it does |
|------|-----------|-------------|
| `TranscriptSize` | `compact.go:18-27` | Sums `len(DerefStr(m.Content))` for context-size estimate |
| `keepBoundary` | `compact.go:51-75` | `len(DerefStr(conv[i].Content))` to find verbatim tail boundary |
| `renderTranscript` | `compact.go:80-100` | `DerefStr(m.Content)` per role for summary prompt text |
| `spillPathsInTurn` | `compact.go:444-458` | `tools.ExtractSpillPath(DerefStr(m.Content))` on tool messages |
| `evictStaleToolResults` | `app.go:1154-1174` | `len(DerefStr(m.Content))` check, then `m.Content = &stub` |
| `trimToolResults` | `client.go:669-704` | `*m.Content` as string, replaces with stub string |
| `ensurePreamble` | `app.go:1045-1066` | Mutates `Conv[0].Content` in place (day rollover) |
| `SessionTurns` | `session.go:134-145` | `DerefStr(m.Content)` for first user message when listing |
| `RecordInferenceCost` | `app.go:899-953` | Reads `u.CachedTok` from usage, not content |

**`DerefStr`** is at **`internal/agent/helpers.go:13`** — converts `*string` →
`string`, returning `""` for nil.

### 1.5 Session persistence

**`internal/agent/session.go:54-72`** — `WriteSession`:
`json.MarshalIndent(s, ...)` serializes `Session.Conv` (`[]proxy.Message`) to
JSON. `Content` round-trips as `*string`.

**`session.go:94`** — `LoadSession`: `json.Unmarshal` back into `[]proxy.Message`.

The `Pinned` field has `json:"-"` so it is **not persisted** — it is
reconstructed at runtime by `ensurePreamble` and subagent append logic.

### 1.6 What minimal parts-shaped content support would touch

If `cache_control` is injected at **serialization time only** (transient wire
copy), then:

- **`Message` struct:** No change needed. The struct stays `Content *string`.
  The decoration happens on a copy marshaled to JSON, not on the Go struct.
- **SSE parse:** No change. Responses are string deltas, never parts-shaped.
- **Compaction/pinning:** No change. All read `DerefStr(m.Content)` — the stored
  messages remain plain strings.
- **`trimToolResults`:** No change. It operates on `[]Message` before marshal;
  if injection happens after trim (at marshal time), the two are composable.
- **`TranscriptSize`:** No change. Reads stored `*string` content.
- **Session persistence:** No change. Stored messages stay plain strings.

**The only touch point is the marshal path** — specifically, a step between
`trimToolResults` and `json.Marshal` that produces a wire-shaped JSON with
content-parts on marked messages.

---

## 2. Marking strategy: where breakpoints would be injected

### 2.1 The `Stream` request build path

**`internal/agent/app.go:592-595`** — the caller in `Send`:
```go
msgs := a.Conv              // ← direct slice reference, NOT a copy
msg, err := a.Client.Stream(ctx, msgs, tools, sink, rsink)
```

**Critical:** `msgs` is `a.Conv` — the live conversation slice, not a copy.
`Stream` receives it directly.

**`internal/proxy/client.go:447-473`** — inside `Stream`:
```go
reqBody := chatRequest{
    ...
    Messages: messages,  // ← still the live slice (no copy yet)
}
raw, err := json.Marshal(reqBody)
```

The only copy happens inside `trimToolResults` (`client.go:682-683`):
```go
out := make([]Message, len(msgs))
copy(out, msgs)
```
…but only when the byte guard triggers (`MaxRequestBytes > 0 && len(raw) > limit`).

### 2.2 Serialization-time injection (preferred)

**The gap between `reqBody` construction and `json.Marshal` is the injection
point.** Specifically:

1. **`client.go:447-457`**: `reqBody` is built with `Messages: messages`.
2. **`client.go:473`**: `json.Marshal(reqBody)` serializes it.

A serialization-time approach would either:
- **(a)** Marshal `reqBody` to JSON, then post-process the JSON to inject
  `cache_control` on the desired messages — fragile but zero struct change.
- **(b)** Build a transient wire-shaped copy (different type or `json.RawMessage`)
  for just the messages that need `cache_control`, marshal that — clean but
  needs a parallel wire type.
- **(c)** Use a custom `MarshalJSON` on `Message` that emits parts-shaped
  content when a transient marker is set — but the marker must not persist.

**What prevents decorating on the wire copy only:**

1. **`Message.Content` is `*string`.** `json.Marshal` will always emit it as a
   JSON string (or null). To produce `content: [{"type":"text","text":"...","cache_control":{"type":"ephemeral"}}]`,
   you cannot use the default marshaler on the current `Message` struct. You
   need either a custom marshaler, a different wire type, or raw JSON
   post-processing.

2. **`trimToolResults` mutates `reqBody.Messages` in place** (`client.go:486`)
   and re-marshals (`client.go:487`). If injection happened before trim, trim
   would need to understand the parts shape. If injection happens *after* trim
   (right before the final marshal), it is composable — but the code currently
   does a single `json.Marshal` at line 473, and the trim path re-marshals at
   487. There is no "final marshal" hook — injection would need to wrap both
   marshal sites, or the logic would need restructuring to marshal once at the
   end.

3. **No copy is made before `Stream`.** `msgs := a.Conv` at `app.go:592` is a
   direct reference. Any mutation of `messages` inside `Stream` would mutate
   `a.Conv`. Serialization-time injection must not write back to the slice —
   it must produce a separate `[]byte` body. This is already what `json.Marshal`
   does (produces `raw`), so the constraint is satisfied as long as injection
   works on the `[]byte` or on a transient copy, not on `messages`.

**Nothing fundamental prevents serialization-time injection.** The obstacles
are mechanical (the `*string` → parts shape conversion, and the two marshal
sites), not architectural. The transcript stays clean strings; no stored
marker is needed.

### 2.3 Static breakpoint after `Conv[0]`

`Conv[0]` is the system/preamble message. `ensurePreamble` (`app.go:1045-1066`)
maintains it. In the request, it becomes `messages[0]` (role "system").

For serialization-time injection: mark `messages[0]`'s content with
`cache_control: {"type":"ephemeral"}` on the wire copy. This is a static
position — always index 0, always the preamble. No stored marker needed.

### 2.4 Moving breakpoint on the final message

The last message in `messages` (`messages[len(messages)-1]`) is the most recent
user/tool turn. Its position changes every iteration as the conversation grows.

For serialization-time injection: mark `messages[len-1]`'s content with
`cache_control` on the wire copy. The position is computed at marshal time from
`len(messages)`, not stored. No stored marker needed.

### 2.5 Stored-on-messages alternative (evaluated and disfavored)

If markers were stored on `Message` (e.g. a `CacheControl *string` field with a
json tag), they would:
- Leak into session persistence (`session.go:62` marshals the whole struct).
- Need handling in every `DerefStr` / content-reading site.
- Survive compaction/eviction on messages that moved or were rewritten — see §5.

The serialization-time approach avoids all of these. **The strong preference
for serialization-time injection is well-founded.**

---

## 3. Config/gating: the `cache_prompt` pattern

### 3.1 The `CachePrompt *bool` pattern end to end

| Layer | File:line | Detail |
|-------|-----------|--------|
| **Config struct** | `config/config.go:43` | `CachePrompt *bool \`json:"cache_prompt,omitempty"\`` on `EndpointConfig` |
| **Config doc** | `config/config.go:38-42` | "llama.cpp's non-standard hint… per-endpoint opt-in, pointer-typed: unset omits the field entirely, never sends a literal false" |
| **Client struct** | `proxy/client.go:237` | `CachePrompt *bool` on `Client` (mirrors config) |
| **Client doc** | `proxy/client.go:234-237` | "nil = omit from request body entirely" |
| **Wire serialization** | `proxy/client.go:121` | `CachePrompt *bool \`json:"cache_prompt,omitempty"\`` on `chatRequest` |
| **Request build** | `proxy/client.go:456` | `CachePrompt: c.CachePrompt` in `reqBody` construction |
| **Main client creation** | `cmd/wakil/main.go:65` | `CachePrompt: ep.CachePrompt` from resolved endpoint |
| **Run client creation** | `cmd/wakil/run.go:439` | `CachePrompt: ep.CachePrompt` |

### 3.2 Subagent endpoint-view carry

| Step | File:line | Detail |
|------|-----------|--------|
| **View struct** | `subagent.go:275` | `cachePrompt *bool` on `subagentEndpointView` |
| **Inherit from parent** | `subagent.go:337` | `cachePrompt: a.Client.CachePrompt` (when inheriting parent endpoint) |
| **From named endpoint** | `subagent.go:362` | `cachePrompt: ep.CachePrompt` (when using explicit subagent endpoint) |
| **Push to child client** | `subagent.go:612` | `CachePrompt: view.cachePrompt` in `subClient` construction |

The carry is **identical to the other pointer-typed sampling fields**
(`temperature`, `topP`, `maxTokens`) — same struct fields, same inherit/named
paths, same push to child client.

### 3.3 Template for `anthropic_cache`

An `anthropic_cache` flag would follow the exact same pattern:

- `EndpointConfig.AnthropicCache *bool \`json:"anthropic_cache,omitempty"\``
  at `config/config.go` (near line 43, next to `CachePrompt`).
- `Client.AnthropicCache *bool` at `proxy/client.go` (near line 237).
- `subagentEndpointView.anthropicCache *bool` at `subagent.go` (near line 275).
- Carry at the same three subagent sites (337, 362, 612).
- Main/run client creation at `main.go:65` / `run.go:439`.

### 3.4 Interaction with the two existing flags

The two existing per-endpoint pointer-bool flags near `CachePrompt` are
`Temperature *float64` (`config.go:34`) and `MaxTokens *int` (`config.go:36`).
These are **sampling parameters**, not caching flags — they interact with the
request body but not with caching semantics.

`CachePrompt` itself is **llama.cpp-specific** (non-standard `cache_prompt`
hint). It is orthogonal to Anthropic `cache_control` — different mechanism,
different provider. No semantic interaction. Both could be set on the same
endpoint without conflict (one goes into `chatRequest.CachePrompt`, the other
into content parts), though in practice they target different providers
(llama.cpp vs. OpenRouter/Anthropic).

**Flag ambiguity:** `CachePrompt` is llama.cpp's KV-cache hint (server-side
prompt caching via `cache_prompt: true`). `anthropic_cache` would be Anthropic's
explicit breakpoint caching via `cache_control` on content parts. The names are
distinguishable but both are "cache" — `anthropic_cache` should perhaps be
named `anthropic_prompt_cache` or `cache_control` to be unambiguous about the
mechanism. **[inferred — confirm] — naming is a design choice, not a
discovery fact.**

---

## 4. Usage/pricing: what the current parse captures vs. what OpenRouter returns

### 4.1 Current usage parse

**`proxy/client.go:154-168`** — `streamChunk.Usage` wire struct:
```go
Usage *struct {
    PromptTokens            int64 `json:"prompt_tokens"`
    CompletionTokens        int64 `json:"completion_tokens"`
    TotalTokens             int64 `json:"total_tokens"`
    CompletionTokensDetails *struct {
        ReasoningTokens int64 `json:"reasoning_tokens"`
    } `json:"completion_tokens_details"`
    PromptTokensDetails *struct {
        CachedTokens int64 `json:"cached_tokens"`
    } `json:"prompt_tokens_details"`
} `json:"usage"`
```

**`proxy/client.go:578-588`** — parse path:
```go
if chunk.Usage != nil {
    usage.InputTok = chunk.Usage.PromptTokens
    usage.OutputTok = chunk.Usage.CompletionTokens
    if d := chunk.Usage.CompletionTokensDetails; d != nil {
        usage.ReasoningTok = d.ReasoningTokens
    }
    if d := chunk.Usage.PromptTokensDetails; d != nil {
        usage.CachedTok = d.CachedTokens
    }
}
```

**`proxy/client.go:174-184`** — `UsageStat` struct:
```go
type UsageStat struct {
    InputTok     int64
    OutputTok    int64
    ReasoningTok int64
    CachedTok    int64   // from prompt_tokens_details.cached_tokens
    Exact        bool
}
```

### 4.2 What OpenRouter returns for Anthropic models with caching

OpenRouter passes through Anthropic's cache token fields in the usage object.
For Anthropic models with `cache_control` active, the response `usage` contains:

```json
{
  "usage": {
    "prompt_tokens": 1234,
    "completion_tokens": 567,
    "prompt_tokens_details": {
      "cached_tokens": 890
    },
    "cache_read_input_tokens": 890,
    "cache_creation_input_tokens": 344
  }
}
```

**Key findings from research:**

- **`cache_read_input_tokens`** and **`cache_creation_input_tokens`** are
  passed through by OpenRouter as top-level usage fields (not nested under
  `prompt_tokens_details`). **[inferred — confirm]** — confirmed by multiple
  sources showing these fields in OpenRouter responses for Anthropic models,
  but not verified against a live OpenRouter API call from this codebase.

- **`prompt_tokens_details.cached_tokens`** maps to `cache_read_input_tokens`
  (cache hits). This is the OpenAI-shaped normalization. The current code
  captures this as `CachedTok`.

- **`cache_creation_input_tokens`** (cache writes, billed at 1.25× base input
  rate) is **NOT captured by the current parse**. The `streamChunk.Usage`
  struct has no field for it. It is silently dropped.

- **`prompt_tokens`** (OpenRouter's `PromptTokens`) includes cache-read tokens
  but **excludes cache-creation tokens** in Anthropic's native API
  (`input_tokens` = non-cached input only). **[inferred — confirm]** —
  Anthropic's `input_tokens` excludes both cache-read and cache-write tokens;
  OpenRouter may normalize differently. The exact inclusion/exclusion
  semantics of `prompt_tokens` when caching is active is **ambiguous** and
  should be verified against a live response.

### 4.3 Where a `cache_write` rate slots into `ModelRate` / `ExternalInferenceCost`

**`config/config.go:290-301`** — `ModelRate` struct:
```go
type ModelRate struct {
    InputUSDPer1M      float64 `json:"input_usd_per_1m"`
    OutputUSDPer1M     float64 `json:"output_usd_per_1m"`
    CachedInputUSDPer1M float64 `json:"cached_input_usd_per_1m,omitempty"`
}
```

**`config/config.go:344-363`** — `ExternalInferenceCost`:
```go
func (c CostsConfig) ExternalInferenceCost(backendModel string, inTok, outTok int64, cachedTok ...int64) (usd float64, priced bool) {
    r, ok := c.InferenceBackends[backendModel]
    ...
    cachedRate := r.CachedInputUSDPer1M
    if cachedRate == 0 {
        cachedRate = r.InputUSDPer1M
    }
    uncached := inTok - cached
    if uncached < 0 { uncached = 0 }
    usd = float64(uncached)/1e6*r.InputUSDPer1M +
          float64(cached)/1e6*cachedRate +
          float64(outTok)/1e6*r.OutputUSDPer1M
    return usd, true
}
```

**`app.go:932-933`** — the call site:
```go
usd, priced = a.Cfg.Costs.ExternalInferenceCost(
    usedBackend+"/"+modelForCost, u.InputTok, u.OutputTok, u.CachedTok)
```

**Where `cache_write` would slot in:**

1. **`ModelRate`**: add `CacheWriteUSDPer1M float64 \`json:"cache_write_usd_per_1m,omitempty"\``
   at `config.go:300` (next to `CachedInputUSDPer1M`). Zero = no separate write
   rate (cache writes billed at `InputUSDPer1M`).

2. **`UsageStat`**: add `CacheWriteTok int64` at `client.go:182` (next to
   `CachedTok`). Populated from `cache_creation_input_tokens` in the parse path
   at `client.go:585-587`.

3. **`streamChunk.Usage`**: add `CacheCreationInputTokens int64 \`json:"cache_creation_input_tokens"\``
   at `client.go:167` (next to `PromptTokensDetails`).

4. **`ExternalInferenceCost`**: add `cacheWriteTok` as another variadic param
   (or change signature), then add a term:
   `float64(cacheWrite)/1e6 * cacheWriteRate` where `cacheWriteRate` falls back
   to `InputUSDPer1M * 1.25` or `InputUSDPer1M` when unset.

5. **`RecordInferenceCost`**: pass `u.CacheWriteTok` at `app.go:933`.

6. **`Costs.Record`**: `proxy/cost.go:79-106` accepts `cachedTok` variadic —
   would need a corresponding `cacheWriteTok` param or a richer call shape.

The `cached_input` plumbing that was just added (`CachedInputUSDPer1M`,
`CachedTok`, the split-rate in `ExternalInferenceCost`) is the **exact
template** for `cache_write`. The pattern is: new field on `ModelRate`, new
token count on `UsageStat`, new wire field on `streamChunk.Usage`, new term in
`ExternalInferenceCost`, pass-through at the call site.

---

## 5. Invalidation interactions

### 5.1 The question

With a **moving last-message breakpoint**, what happens to previously-marked
positions when compaction, `evictStaleToolResults`, and `dropOldestTurn` rewrite
the conversation?

### 5.2 Serialization-time injection: **moot (confirmed)**

If injection is serialization-time (transient, per-request, not stored on
messages), there are **no stored markers to invalidate**. Every request
recomputes breakpoint positions from the current `messages` slice:

- Static breakpoint: always `messages[0]`.
- Moving breakpoint: always `messages[len(messages)-1]`.

After compaction/eviction/drop, the next `Stream` call receives the updated
`a.Conv` slice (`app.go:592`), and the breakpoints are recomputed from the new
slice. There is no stale marker on a moved/rewritten message because no marker
was ever stored.

**This is the key advantage of serialization-time injection and the reason the
strong preference is well-founded.**

### 5.3 Stored-marker scenario (evaluated for completeness, disfavored)

If markers WERE stored on `Message`, these are the invalidation paths:

| Operation | File:line | What it does to markers |
|-----------|-----------|----------------------|
| **`Compact`** | `compact.go:197-323` | Rebuilds `a.Conv` as `pinnedPrefix + summary + a.Conv[boundary:]`. Messages in the summarizable block are dissolved into a summary string — any marker on them is lost. Pinned messages are preserved verbatim (markers survive). The new summary message has no marker. |
| **`evictStaleToolResults`** | `app.go:1154-1174` | Replaces `m.Content` with a stub string (`m.Content = &stub`). A marker on the original content would survive on the struct (it's a separate field), but the content it was marking is now a stub — the breakpoint would point at eviction-stub content, not the original. |
| **`dropOldestTurn`** | `compact.go:394-438` | Removes messages entirely (`append(conv[:first:first], conv[next:]...)`). Markers on dropped messages are gone. Markers on surviving messages are preserved but their indices shift. |
| **`trimToolResults`** | `client.go:669-704` | Copies the slice and replaces content with stubs. Markers on the original slice are untouched; markers on the copy would point at stub content. |
| **`ensurePreamble`** | `app.go:1045-1066` | Mutates `Conv[0].Content` in place on day rollover. A static breakpoint on `Conv[0]` would survive but now marks different content. |

**Specific case where a stored marker would survive on a moved/rewritten message:**
`evictStaleToolResults` at `app.go:1172` sets `m.Content = &stub` but would leave
a `CacheControl` field untouched — the marker survives on a message whose content
was rewritten to an eviction stub. The breakpoint would cache a stub, not the
original tool result.

**This confirms the mootness for serialization-time injection and validates
the preference.**

---

## Summary of key findings

1. **`Message.Content` is `*string`** (`client.go:53`). All consumers read it
   via `DerefStr`. Responses are string deltas. No parts shape anywhere.

2. **Serialization-time injection is viable.** The gap between `reqBody`
   construction (`client.go:447`) and `json.Marshal` (`client.go:473`) is the
   injection point. The obstacle is mechanical (`*string` → parts shape), not
   architectural. The transcript stays clean strings.

3. **The `CachePrompt *bool` pattern** (`config.go:43` → `client.go:237` →
   `client.go:121` → `client.go:456`, with subagent carry at `subagent.go:275/337/362/612`)
   is the exact template for `anthropic_cache`.

4. **The usage parse captures `cached_tokens`** (cache reads) as `CachedTok`
   but **does not capture `cache_creation_input_tokens`** (cache writes). A
   `cache_write` rate would slot into `ModelRate` / `ExternalInferenceCost`
   following the identical pattern just used for `CachedInputUSDPer1M`.

5. **Invalidation is moot for serialization-time injection** — no stored
   markers, no stale positions. Stored markers would break under
   `evictStaleToolResults` (marker survives on stub content) and `Compact`
   (marker dissolved into summary).
