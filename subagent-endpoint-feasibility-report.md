# Discovery — Per-Subagent Model/Endpoint Feasibility (READ-ONLY)

Scope: map what "subagent on a different model/endpoint" would touch. No code changes proposed.

---

## 1. Client construction: exact copy sites

**Single construction path.** There is exactly one place that builds a subagent's `proxy.Client` and child `App`: `dispatchSubagent` in `internal/agent/subagent.go:217-277`. The "parallel" path (`subagent_parallel.go`) does not construct a second client — it calls this same function from worker goroutines (`internal/agent/subagent_parallel.go:123-124`). The sequential single-dispatch path (`internal/agent/app.go:1973-2023`, case `"dispatch_subagent"`) and the batch/parallel path (`internal/agent/subagent_parallel.go:144-206`, `runParallelSubagentBlock`) both funnel into `dispatchSubagent`; there is no separate serial-vs-parallel client-construction branch.

Exact struct literal — `internal/agent/subagent.go:222-236`:
```
subClient := &proxy.Client{
    BaseURL:         a.Client.BaseURL,          // :223 — copied verbatim from parent
    Model:           a.Client.Model,             // :224 — copied verbatim
    Kind:            a.Client.Kind,               // :225 — copied verbatim ("endpoint kind gates the proxy-specific request shape")
    ConfiguredModel: a.Client.ConfiguredModel,    // :226 — copied verbatim
    Temperature:     a.Client.Temperature,        // :227 — copied verbatim
    TopP:            a.Client.TopP,               // :228 — copied verbatim
    MaxTokens:       a.Client.MaxTokens,          // :229 — copied verbatim
    ChatID:          subChatID,                   // :230 — NOT copied; freshly minted (NewChatID() or passed chatID param, :218-221)
    AuthHeader:      a.Client.AuthHeader,          // :231 — copied verbatim (auth token)
    NoMemoryWrite:   true,                        // :232 — NOT copied; hardcoded true for every subagent
    HTTP:            a.Client.HTTP,               // :233 — copied verbatim (same *http.Client instance, see §6)
    Backend:         resolvedBackend,              // :234 — NOT copied from parent field; a per-dispatch resolved param (see §3)
    MaxRequestBytes: a.Client.MaxRequestBytes,     // :235 — copied verbatim
}
```

Fields **not copied at all**: `AuxModel` (parent sets it per-Send at `app.go:409`, but the fresh `subClient` starts with zero value — subagent Send calls never populate it since `sub.Cfg.AuxModel` is default-config zero and `sub.Send` sets `a.Client.AuxModel = a.Cfg.AuxModel` at `app.go:409`, so it ends up empty regardless), and the mutex-guarded internal state (`lastUsedBackend`, grounding) which starts fresh per-Client by construction.

The child `App` struct literal — `internal/agent/subagent.go:263-277` — assigns `Cfg` (a **fresh default config with subagent-specific byte constants overlaid**, not copied from parent — see §2), `Client: subClient`, `Exec: a.Exec` (shared), `Tools: tools.DiscoveryTools(...)`, `Confirm`, `Out`, `Session: nil`, `ToolCache`, `IsSubagent: true`, `pinUserMessage: true`, `SelectedBackend: resolvedBackend`, `BackendList: a.BackendList` (shared slice), `consentedBackends: consentSnapshot` (a copy). **`CtxLimit` is never set** — see §2.

**Conclusion for Q1:** one construction path, in `dispatchSubagent`. Every "endpoint identity" field (`BaseURL`, `Kind`, `ConfiguredModel`, `AuthHeader`) is a straight copy from the parent's live `*proxy.Client` at dispatch time, with no per-subagent override point anywhere in this path today.

---

## 2. Limits and budgeting in the child

### What the child App actually uses
- `activeThresholds()` (`internal/agent/compact.go:121-152`) computes `compactAt, keepBytes, hardMax`. It only takes the "scale to real n_ctx" branch when `a.CtxLimit.NCtx > 0 && a.Cfg.CompactAtFrac > 0` (`compact.go:125`); otherwise it falls through to the absolute config values (`compact.go:145-151`).
- `WarnContextPressure()` (`internal/agent/app.go:729-751`) calls `a.ContextLimit()` (`app.go:730`) to get `usable`, compares to `a.ContextTokensUsed()`.
- Tool-result cap: `a.Cfg.ToolResultCap` used at `app.go:1033` (`wtools.CapToolResult(..., a.Cfg.ToolResultCap)`), unrelated to `CtxLimit`.
- `enforceHardMax(ctx, hm)` called at `app.go:664` and `compact.go:168`, where `hm` comes from `activeThresholds()`.

### Where the child's CtxLimit comes from
**Nowhere.** The child `App` literal at `subagent.go:263-277` has no `CtxLimit` field assignment. Go zero-values it to `ContextLimit{}` (`NCtx: 0`). This is corroborated by:
- `ContextLimit()` fallback comment at `internal/agent/ctxlimit.go:377-395`: "When CtxLimit was never populated (tests, **subagents**, a build path that skipped the startup probe) it synthesizes a fallback from Cfg."
- `activeThresholds()`'s own doc comment at `compact.go:118-120`: "When the limit is unknown (startup before the probe, **subagents**, tests that leave CtxLimit zero) the absolute config values are the fallback."
- Test `TestActiveThresholdsFallsBackToAbsolute` (`compact_test.go:272-287`) explicitly documents/verifies this fallback behavior for `CtxLimit.NCtx == 0`.

So: **the child's budgeting comes from neither a parent copy nor a probed default — it comes from hardcoded subagent-specific absolute byte constants** defined at `subagent.go:15-26`:
```
subagentHardMaxBytes   = 70_000   // ~23k tokens @ 3 chars/tok; safe floor for 32k backend
subagentCompactAt      = 55_000
subagentKeepBytes      = 45_000
subagentSummaryBytes   = 8_000
subagentToolResultCap  = 12_000
subagentTurnToolBudget = 50_000
subagentToolResultTTL  = -1
subagentMaxToolIter    = 16
```
overlaid onto a **fresh `config.DefaultConfig()`** at `subagent.go:238-249` (not `a.Cfg`, the parent's config — a fresh default, then these 8 fields overridden). The comment at `subagent.go:15-16` states these constants are "Conservative for a **32k-token backend**" — a hardcoded assumption baked into the byte budget, independent of whatever model/backend the parent (or the resolved subagent backend) is actually using.

### "Child budgeting is only correct because parent and child share a model" — verdict

**Refuted, more precisely: it isn't "correct because they share a model" — it's approximately safe today only because the hardcoded 70KB/55KB/45KB constants happen to be conservative for whatever backend is actually in use (documented as tuned for "a 32k-token backend").** The child's budgeting figures never reference the parent's model, the parent's `CtxLimit`, or the actual resolved subagent backend's context window at all — they are static regardless of what model runs. So today, "parent and child share a model" is *irrelevant* to whether child budgeting is correct; correctness instead rests on the *unstated* assumption that whichever backend actually serves the subagent (which can already differ from the parent via `resolvedBackend`/`SubagentBackend` config, see §3) has at least ~32k tokens of window. That assumption already exists in the codebase, prior to any endpoint-kind/base_url divergence work.

### If a child ran a model with a genuinely different context window, list every computation that would be wrong

1. **`activeThresholds()` fraction path never activates** (`compact.go:125`) — because `CtxLimit.NCtx == 0` always for the child, the fraction-based scaling logic that exists specifically to handle "backend/model changed" (see `fitConvToWindow`, `compact.go:154-169`, built for the *parent's* `/backend` switch case) never runs for subagents. A child on a genuinely small-window model (e.g. 4k or 8k tokens) gets the same 70,000-byte (~23k token) `HardMaxBytes` as a child on a 128k-token model — oversized for the small model, undersized (needlessly conservative) for the large one.
2. **`WarnContextPressure()`** (`app.go:729-751`) reads `a.ContextLimit().Usable()` (`ctxlimit.go:381-395`), which synthesizes from `a.Cfg.ContextTokensFallback` (default 131072, `config.go:374`) — since the child's `Cfg` is `config.DefaultConfig()`, `Usable()` returns a number based on the 131072-token fallback, not the child's actual model's window. If the child's real model has a much smaller window, `WarnContextPressure` will never fire even when genuinely over budget (it compares against a fictitious ~131k ceiling); if the real model is much larger, it could in theory fire early but this direction is less likely given the byte-based caps are far below 131k tokens anyway.
3. **`ContextTokensUsed()`** (referenced at `app.go:732`, defined near `ctxlimit.go:397+`) — feeds off the proxy's reported `prompt_tokens`, so it is itself accurate regardless of model; but it is compared against the wrong `usable` ceiling per (2).
4. **`fitConvToWindow`** (`compact.go:161-169`) — designed for *parent* backend-downshift scenarios; it is never invoked for subagents in a way that would correct for the child's actual (different) window, because it too reads `a.activeThresholds()`/`a.ContextLimit()` off the same zero `CtxLimit`.
5. **`subagentHardMaxBytes` / `subagentCompactAt` / `subagentKeepBytes` / `subagentSummaryBytes`** (`subagent.go:18-21`) — all four are absolute byte constants baked in assuming "32k-token backend" (`subagent.go:15-16`); none scale with whatever model the resolved subagent backend actually serves.
6. **`MaxRequestBytes`** (copied at `subagent.go:235` from `a.Client.MaxRequestBytes`) — this is the parent's pre-send byte guard (default 8MB, `config.go:107`), unrelated to the model's context window but also not derived from whatever the child's actual endpoint would tolerate.

**Net: every context-limit-aware computation in the child (`activeThresholds`, `WarnContextPressure`, `fitConvToWindow`, the hardcoded subagent byte constants) is blind to the actual model/backend the child talks to.** This is a pre-existing gap even without new endpoint-kind divergence — it would simply become more consequential (wrong-direction errors more likely, and in both directions rather than just "generous slack") if children start running genuinely different-context-window models.

---

## 3. Kind-dependent child behavior

`proxyShape := c.Kind == "" || c.Kind == KindIlmProxy` at `internal/proxy/client.go:410` is the single gate for every ilm-proxy-specific request element. Everything below is conditioned on this one boolean, which for the child is **whatever `a.Client.Kind` was at dispatch time — a straight copy from the parent** (`subagent.go:225`), never derived from `resolvedBackend`.

| Feature | Location | Gated by `proxyShape`? | If parent/child Kind differ |
|---|---|---|---|
| `metadata.chat_id` | `client.go:433-438` | Yes (`if proxyShape` at :431) | If child's copied `Kind` says plain/openai but the child is actually meant to hit an ilm-proxy-shaped backend, `chat_id` metadata is never sent (though NoMemoryWrite always suppresses `chat_id` anyway per `client.go:433-436`, since subagents always set `NoMemoryWrite=true`) |
| `metadata.ilm-no-memory-write` | `client.go:439-441` | Yes | Same as above — if `Kind` (copied, possibly stale relative to the actual target) is plain, this metadata field is dropped even though the subagent client always sets `NoMemoryWrite: true` (`subagent.go:232`) |
| `X-Ilm-No-Memory-Write` header | `client.go:479-482` | Yes | If Kind says plain but the actual target backend is proxy-shaped, the no-memory-write guarantee is not communicated via header — a silent policy gap (the intended "subagents never write memory" contract would rely purely on the server ignoring an unlabeled request rather than an explicit opt-out) |
| `X-Ilm-Backend` header (`resolvedBackend`) | `client.go:479,483-485` | Yes | If Kind is copied from a parent on a plain endpoint (Kind=openai) while `resolvedBackend` names a real proxy backend the caller wanted to route to, the header **is never sent** — the requested backend routing silently never happens; the request goes wherever `BaseURL` points with no `X-Ilm-Backend`, i.e. `resolvedBackend` becomes inert |
| `X-Ilm-Aux-Model` header | `client.go:479,486-488` | Yes | Same silent-drop pattern |
| `X-Ilm-Backend-Used` response header → `SetLastUsedBackend` | `client.go:527` | **No** — read unconditionally regardless of Kind | On a plain/direct backend that never emits this header, `LastUsedBackend()` simply stays `""` (no crash, but `SubagentDoneMsg.UsedBackend` and `RecordInferenceCost`'s backend-keyed cost attribution both silently degrade to the "no backend known" path — see §4) |
| `ConfiguredModel` model-string override | `client.go:412-419` | Inverse-gated (`!proxyShape && ConfiguredModel != ""`) | If Kind is copied and says proxy-shaped but the real target is a plain endpoint requiring `ConfiguredModel`, the literal model string is never substituted — the wrong/stale model field (`c.Model`, possibly the proxy alias `"ilm"` or a backend-prefixed routing string) gets sent verbatim to a plain server that has no concept of it |

**What happens if parent and child differ in kind, generally:** since `Kind` is a straight field copy (`subagent.go:225`) rather than re-derived from `resolvedBackend`, if a future change lets a subagent target a genuinely different endpoint (different `Kind`) than the parent, **the request-shape gate (`proxyShape`) would still reflect the *parent's* Kind, not the child's actual target** — every header/metadata/model-substitution decision listed above would be made against the wrong assumption. This is flagged as [inferred — confirm]: the current code has no path where Kind and `resolvedBackend`/`BaseURL` diverge (today they're either both parent-copied or `resolvedBackend` only changes the `X-Ilm-Backend` routing header value within the *same* proxy), so this failure mode is presently latent, not yet triggered.

---

## 4. Cost/trace attribution

### Where subagent usage/cost are recorded
- **`RecordInferenceCost()`** (`internal/agent/app.go:837-888`) is the sole cost-recording function; it is called from `Send` at `app.go:537` ("main inference for this iteration") and from `Compact` at `compact.go:188` ("aux inference: summarization/compaction"). It reads `a.Client.LastUsage()` and `a.Client.LastUsedBackend()`, and looks up pricing via `a.Cfg.Costs.ExternalInferenceCost(usedBackend+"/"+modelForCost, ...)` (external, `app.go:871`) or `a.Cfg.Costs.InferenceCost(...)` (local, `app.go:873`) — **the model string used for pricing lookup is `a.Client.Model`** (`app.go:853`, with backend-prefix stripping at `:854-856`), i.e. it looks up the *actual* client's configured model, not a hardcoded parent model. In principle this is correctly model-aware **if** it runs.
- **It does not run for subagents.** The explicit comment in `subagent_parallel.go:86-89`: *"Costs: the child App's Costs tracker is nil and its Client is fresh, so `RecordInferenceCost` inside a child Send is a no-op on parent state. Subagent inference cost is NOT folded into the parent ledger (documented limitation, unchanged from sequential dispatch)."* `RecordInferenceCost` itself no-ops immediately when `a.Costs == nil` (`app.go:838`), and the child `App` literal (`subagent.go:263-277`) never sets `Costs` — it is nil by Go zero-value.
- **Trace records:** `flushTraceTurn` (`app.go:689-719`) writes `trace.Record{... Backend: a.Client.LastUsedBackend(), InputTokens: u.InputTok, ...}` only `if a.Trace != nil` (`app.go:474`). The child `App` literal never sets `Trace` either — it stays nil, so **no JSONL trace record is written for a subagent's turns at all.**
- **`captureToolTrace`** (`app.go:1129-1136`) is workflow-scoped (`a.Workflow == nil` check) — child never has a `Workflow` set, so this is also inert for children; it's called from the *parent's* handling of the `dispatch_subagent` tool call result (`app.go:1118`, `subagent_parallel.go:643`), i.e. it records the parent's view of the tool call (task string, summary length), not the child's internal token usage.
- **`SubagentDoneMsg.UsedBackend`** (`msgs.go:62-69`, populated at `app.go:1996-2002` and `subagent_parallel.go:188-194` from `dispatchSubagent`'s 4th return value, `subagent.go:283` `sub.Client.LastUsedBackend()`) — this is purely a **UI/TUI display value** (`internal/tui/tui.go:462,469`), not fed into cost or trace bookkeeping.

### Would a child on a different model be attributed at the wrong rate?
**No misattribution occurs — because no attribution occurs at all.** Subagent token usage/cost is currently a complete blind spot: it is neither priced against the parent's rate nor the child's own rate; it is simply dropped (`Costs == nil`, `Trace == nil`). So there is no existing "wrong rate" bug to trigger by changing the child's model — but there is also **no cost visibility to build on**: introducing a differently-priced child model would still show $0 additional session cost, silently, exactly as it does today for same-model subagents. This is a pre-existing gap, documented as intentional/deferred in `subagent_parallel.go:86-89`, not something the endpoint-kind change would newly break.

---

## 5. Config and command surface

### Existing "endpoints" config shape
`internal/config/config.go`:
- `EndpointConfig` struct (`config.go:29-37`): `Kind`, `BaseURL`, `Model`, `AuthHeader`, `Temperature *float64`, `TopP *float64`, `MaxTokens *int`.
- `Config.Endpoints map[string]EndpointConfig` (`config.go:53`, json `"endpoints"`) — a **named map**, not a list; `Config.DefaultEndpoint string` (`config.go:54`) selects the active entry by key.
- `Config.Endpoint EndpointConfig` (`config.go:59`, `json:"-"`) and `Config.EndpointName string` (`config.go:60`) are the **runtime-resolved** active endpoint, populated by `LoadConfig`'s `resolveEndpoint()` and read via `cfg.ActiveEndpoint()`.
- Legacy singular fields (`BaseURL`, `Host`, `Port`, `Model`, `APIKey` at `config.go:43-45,62-63`) remain as the pre-endpoints fallback path when `Endpoints` is empty.
- Separately, `Config.SubagentBackend string` (`config.go:185-194`, json `"subagent_backend"`) already exists as a **top-level, single-value control for which *backend name* (within the currently active endpoint's proxy) a subagent uses** — `"inherit"`, `"default"`, or a pinned backend name — resolved by `ResolveSubagentBackend` (`backends.go:238-256`). This is a *backend* selector (an `X-Ilm-Backend` routing value inside one ilm-proxy endpoint), **not an endpoint selector** — it cannot today point a subagent at a different `Endpoints[...]` entry (different `BaseURL`/`Kind`/`Model`).

**Observation (no recommendation):** the existing `Endpoints` map keyed by name plus `DefaultEndpoint` string, and the existing precedent of `SubagentBackend` as a distinct top-level "which backend does dispatch_subagent use" knob, are the two shapes already in the codebase that a hypothetical "which endpoint does dispatch_subagent use" setting would sit alongside. Whether that would be a new top-level `SubagentEndpoint string` field (keying into `Endpoints`, mirroring `SubagentBackend`'s pattern) or a per-endpoint field is a design question — flagged as out of scope per task instructions, not decided here.

### `/model` and `/backend` command parsing
Both are parsed inside one function, `HandleTUICommand` (`internal/agent/agent_async.go:978-989`):
```
line = strings.TrimSpace(line)
fields := strings.Fields(line)   // agent_async.go:983 — splits on ALL whitespace into N tokens
switch fields[0] { ... }
```
`strings.Fields` is **generic whitespace tokenization**, not "command + rest-of-line as one string." Each `case` then decides for itself how many fields to consume — some commands (`/session name "<label>"` at `agent_async.go:1087-1098`, `/mcp reconnect <name>` at `:1100-1115`) explicitly re-join `fields[2:]` into a multi-word argument; `/model` and `/backend` do **not**.

- **`/backend`** (`agent_async.go:1119-1171`): for `kind=openai` active endpoints, `/backend <endpoint-name>` is repurposed as the endpoint switcher — `if len(fields) >= 2 { return handleEndpointSwitch(app, fields[1], note) }` (`:1127-1129`); only `fields[1]` (one token) is read, `fields[2:]` is **never inspected or passed on** (silently ignored, no error). For `kind=ilm-proxy`, `/backend [<name>[/<model-path>]]` (`:1136-1154`) again reads only `fields[1]` as `arg`, splitting internally on `/` to derive `SelectedBackend`/`SelectedModel`.
- **`/model`** (`agent_async.go:1173-1204`): `if len(fields) >= 2 { ... fields[1] ... }` — only `fields[1]` is ever read, for both the `kind=openai` branch (sets `Client.ConfiguredModel`, `Client.Model`, `Cfg.Endpoint.Model`, `defaultModel`, `:1187-1191`) and the proxy branch (`app.SelectedModel = fields[1]`, `:1195`).

**Literal parsing-trap answer:** for an input like `/model subagent gpt-4`, `fields = ["/model", "subagent", "gpt-4"]`. Both `/model`'s branches read only `fields[1] == "subagent"` as the model name and **silently drop `fields[2] == "gpt-4"`** — no error, no warning, apparent success setting the model to the literal string `"subagent"`. The same trap applies identically to `/backend subagent llama` (would set backend to `"subagent"`, drop `"llama"`).

This means: **a scope-modifier token inserted between the command and its value (`/model subagent <name>`) is not safely parseable by extending the existing single-argument case bodies** — doing so would require either (a) new logic to detect and consume a second token before falling back to today's single-token behavior (a nontrivial compatibility change, since e.g. `/model subagent-model-name` with no space is indistinguishable from `/model subagent` + dropped-tail today only because nothing currently reads past `fields[1]`), or (b) a syntactically distinct new command (e.g. `/subagent-model <name>`) that avoids the ambiguity entirely by not overloading `/model`'s existing single-token grammar. Both are design choices; this report only maps that the current single-token-only parsing of `/model`/`/backend` is what creates the ambiguity, without recommending which resolution to take.

---

## 6. Concurrency interaction

- **Semaphore:** `runSubagentJobs` (`internal/agent/subagent_parallel.go:92-135`) builds `sem := make(chan struct{}, maxPar)` (`:98`) where `maxPar := a.Cfg.MaxParallelSubagents` (`:94`, clamped to ≥1 at `:95-97`). Config default is `2` (`internal/config/config.go:381`, field doc at `:196-201`): *"Values ≤ 1 mean sequential execution... the default is 2. Raising this only helps when the backend serves concurrent requests."* No numeric rationale beyond that comment was found for why 2 specifically (not e.g. 4) — [inferred — confirm: the "2" appears to be a conservative default assuming a single local backend with limited concurrent-slot capacity, consistent with the comment's framing, but no explicit tuning rationale tied to a llama.cpp slot count was found in the source].
- **HTTP client/transport sharing:** `newHTTPClient()` (`cmd/wakil/main.go:221-225`) clones `http.DefaultTransport` and sets only `ResponseHeaderTimeout = 30s`; it does **not** set `MaxIdleConnsPerHost` or `MaxConnsPerHost` explicitly, so Go's `http.Transport` defaults apply (`MaxIdleConnsPerHost` defaults to 2; there is no default cap on total concurrent connections per host beyond idle-pool reuse behavior — Go's default `Transport` has no `MaxConnsPerHost` limit, i.e. 0/unlimited). `newHTTPClient()` is called once per top-level app construction — `cmd/wakil/main.go:67` and `cmd/wakil/run.go:441` — and that single `*http.Client` is stored on the **parent's** `proxy.Client.HTTP` field.
- **Crucially, the subagent client copies this exact same `*http.Client` pointer:** `subagent.go:233` — `HTTP: a.Client.HTTP`. This means **the parent's single request and every concurrent subagent's requests currently all share one `*http.Client` / one underlying `*http.Transport` instance** — same connection pool, same (default, effectively unbounded per-host `MaxConnsPerHost`) limits, all targeting the same `BaseURL` today since `BaseURL` is also copied verbatim.
- **Rationale check:** because `BaseURL` (hence host) is identical between parent and subagents today, "shared transport" has no negative effect — Go's default transport doesn't artificially throttle concurrent requests to the same host beyond idle-connection reuse, so up to `MaxParallelSubagents` (2) + 1 (parent) = 3 concurrent HTTP requests to the same `llama-server` can be in flight, and the semaphore's real purpose (per its doc comment) is to bound *load on the backend's own request-processing slots*, not transport-level connection limits.
- **If parent stayed local and subagents went remote (different `base_url`/host):** the shared `*http.Client`/transport would now be multiplexing connections across two distinct hosts. Since Go's default transport has no host-specific connection ceiling configured here (no `MaxConnsPerHost` override), **this would not create host-level contention between local and remote traffic** — different hosts get independent connection pools within the same `Transport` object automatically (Go's `http.Transport` keys its idle-connection cache and dial logic per host). So sharing one `*http.Client` across differing `BaseURL`s is not itself a technical blocker for connection pooling.
- **What *would* change under parent-local + subagents-remote:** the semaphore's stated rationale ("bounds concurrent dispatch_subagent workers... raising only helps if backend serves concurrent requests," `config.go:196-200`) is calibrated for the case where subagent requests compete with the parent for the **same local llama-server's finite processing slots**. If subagents move to a remote backend, that local-slot-pressure argument for keeping `MaxParallelSubagents` low no longer applies to the subagent side — the local server would only ever see the parent's traffic, freeing local slot pressure — while the remote backend's own concurrency limits (unknown to this config) become the new constraint. This is a **rationale shift, not a code bug**: the semaphore mechanism (a bounded channel) is generic and would still function correctly; only the *reason* for choosing 2 as a default would need re-examination if local/remote split. [inferred — confirm: no code path today conditions `MaxParallelSubagents` on whether the subagent's resolved backend is local or remote — it is a single flat config value regardless of target.]

---

## Summary of flagged ambiguities / unverified points

1. Whether any additional `proxy.Client` fields exist beyond what's listed in §1 that are silently *not* copied — the struct's full field list beyond what's used in the construction site was not exhaustively re-verified against `internal/proxy/client.go`'s complete type definition in this pass beyond the `Kind`/`AuxModel` fields explicitly confirmed.
2. The precise rationale for `MaxParallelSubagents` default = 2 (vs. any other small number) — only the general comment at `config.go:196-201` was found; no backend-slot-count-derived number.
3. Whether `cmd/wakil/main.go:67` and `cmd/wakil/run.go:441` (`newHTTPClient()` calls) represent two different app-lifecycle entry points (TUI vs headless run) or could ever coexist within one process — not fully traced; each appears to be a distinct top-level `main`/`run` construction path rather than concurrent duplicate clients.
4. Whether `Kind` divergence between parent and a hypothetical differently-routed child has ever been exercised by any test — none found; this is a latent-code-path analysis, not an observed failure.
