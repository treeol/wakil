package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/orregistry"
	"github.com/treeol/wakil/internal/proxy"
)

// ContextLimit is the authoritative per-slot context window, resolved once at
// startup and re-resolved on /backend or /model switch. NCtx is the real ceiling
// the backend enforces (in tokens); everything else in Wakil that reasons about
// "how full is the window" keys off this number rather than a hardcoded guess.
//
// UsableCtx is the proxy's pre-computed usable budget (n_ctx minus reasoning and
// answer headroom, already subtracted server-side). When > 0 it is the single
// source of truth for the usable window; when 0 (e.g. the /props fallback path
// which doesn't emit usable_ctx) Usable() falls back to the client-side
// computation: NCtx − ReasoningBudget − AnswerMargin.
//
// ContextSource is the proxy's reported origin of the n_ctx value:
// "props" (llama-server native), "model_meta" (OpenRouter model registry), or "".
// Distinct from Source, which records where the *client* got the number
// ("backend" | "fallback").
type ContextLimit struct {
	NCtx            int    // per-slot context size in tokens (the hard ceiling)
	NCtxTrain       int    // model's trained context length in tokens; 0 if the backend didn't report it
	Source          string // "backend" (fetched) or "fallback" (config; backend unreachable)
	ContextSource   string // proxy-reported origin: "props", "model_meta", or "" (unknown)
	UsableCtx       int    // proxy's pre-computed usable budget; 0 = not reported (fall back to client-side)
	ReasoningBudget int    // tokens reserved for extended thinking
	AnswerMargin    int    // tokens reserved for the final answer

	// ModelUnresolved is true when the proxy reported "resolved": false — the
	// requested model was not in its catalogue and the returned limits belong
	// to a *different* (fallback) model. The numbers are real but may be wrong
	// for the model actually in use, so callers surface it loudly (amber ctx
	// key, startup warning) instead of trusting them silently.
	ModelUnresolved bool
}

// FromBackend reports whether NCtx came from the live backend (true) rather than
// the configured fallback (false). A fallback value must never masquerade as
// truth — callers surface it loudly.
func (c ContextLimit) FromBackend() bool { return c.Source == "backend" }

// Usable returns the token budget available for assembling a turn's prompt.
// When the proxy reported a usable_ctx (pre-computed server-side), it is the
// authoritative single source of truth. Otherwise falls back to the client-side
// computation: NCtx minus the reasoning and answer reservations. Never negative;
// if the result is ≤ 0 it clamps to a small positive floor so callers don't
// divide by zero or treat every turn as over-budget.
func (c ContextLimit) Usable() int {
	if c.UsableCtx > 0 {
		return c.UsableCtx
	}
	u := c.NCtx - c.ReasoningBudget - c.AnswerMargin
	if u < 1 {
		return 1
	}
	return u
}

// limitsTimeout bounds the startup probe so an unreachable or slow backend can't
// stall launch — on timeout the caller falls back to the configured ceiling.
const limitsTimeout = 5 * time.Second

// limitsResult carries the full response from a context-limit probe.
type limitsResult struct {
	nCtx          int
	nCtxTrain     int
	usableCtx     int
	contextSource string
	path          string

	// Model-resolution telemetry from the proxy (P41 fix). resolvedPresent
	// distinguishes "proxy said resolved:false" from "old proxy that doesn't
	// emit the field" — only an explicit false marks the limit unresolved.
	resolved        bool
	resolvedPresent bool
	model           string // the model the returned limits actually belong to
	fallbackModel   string // proxy-reported fallback model when resolved=false
}

// ResolveContextLimit fetches the per-slot n_ctx from the backend (via the proxy)
// and returns the authoritative ContextLimit. On any failure it falls back to
// cfg.ContextTokensFallback and writes a loud, single-line warning to out so a
// stale fallback can't be mistaken for the real ceiling. The headroom fields are
// always taken from cfg regardless of which path supplied NCtx.
func ResolveContextLimit(ctx context.Context, httpc *http.Client, cfg config.Config, out io.Writer) ContextLimit {
	return resolveContextLimit(ctx, httpc, cfg, "", "", out)
}

// resolveContextLimit is the shared implementation: probes with backend and model
// as query params so the proxy returns per-model limits. Called by both startup
// (empty backend/model → proxy defaults) and re-resolve on /backend or /model.
//
// Resolution is endpoint-kind aware:
//   - kind ilm-proxy (or a hand-built Config with no endpoint, which
//     ActiveEndpoint maps to ilm-proxy): /v1/ilm/limits → /props → fallback,
//     unchanged from the pre-endpoints behavior.
//   - kind openai: never touches /v1/ilm/limits. openrouter.ai hosts resolve
//     the configured model against the OpenRouter registry (no /props probe —
//     OpenRouter doesn't serve it); all other hosts probe /props (llama.cpp).
//     Neither → the existing fallback path.
func resolveContextLimit(ctx context.Context, httpc *http.Client, cfg config.Config, backend, model string, out io.Writer) ContextLimit {
	lim := ContextLimit{
		ReasoningBudget: cfg.ReasoningBudgetTokens,
		AnswerMargin:    cfg.AnswerMarginTokens,
	}

	res, err := fetchContextLimitForKind(ctx, httpc, cfg, backend, model)
	if err != nil || res.nCtx <= 0 {
		lim.NCtx = cfg.ContextTokensFallback
		lim.Source = "fallback"
		reason := "backend reported no usable n_ctx"
		if err != nil {
			reason = err.Error()
		}
		id := backend
		if model != "" {
			id = backend + "/" + model
		}
		fmt.Fprintf(out, "⚠ using fallback context limit %d tokens (%s) — set the backend or context_tokens_fallback\n",
			lim.NCtx, reason)
		_ = id
		return lim
	}

	lim.NCtx = res.nCtx
	lim.NCtxTrain = res.nCtxTrain
	lim.UsableCtx = res.usableCtx
	lim.ContextSource = res.contextSource
	lim.Source = "backend"
	lim.ModelUnresolved = res.resolvedPresent && !res.resolved
	trainNote := ""
	if res.nCtxTrain > 0 {
		trainNote = fmt.Sprintf(", n_ctx_train=%d", res.nCtxTrain)
	}
	csNote := ""
	if res.contextSource != "" {
		csNote = fmt.Sprintf(", source=%s", res.contextSource)
	}
	fmt.Fprintf(out, "context limit: n_ctx=%d%s, usable_ctx=%d (from %s%s); usable=%d (−%d reasoning −%d answer)\n",
		res.nCtx, trainNote, res.usableCtx, res.path, csNote, lim.Usable(), lim.ReasoningBudget, lim.AnswerMargin)
	if lim.ModelUnresolved {
		fb := res.fallbackModel
		if fb == "" {
			fb = res.model
		}
		fmt.Fprintf(out, "⚠ proxy did not recognise model %q — the limits above belong to fallback model %q and may be wrong for this session\n",
			model, fb)
	}
	return lim
}

// ResolveContextLimitForBackendModel probes the proxy for a specific backend+model
// pair. Called by the /backend and /model command handlers to re-synchronise
// thresholds when the user switches backends or models mid-session.
func ResolveContextLimitForBackendModel(ctx context.Context, httpc *http.Client, cfg config.Config, backend, model string, out io.Writer) ContextLimit {
	return resolveContextLimit(ctx, httpc, cfg, backend, model, out)
}

// fetchContextLimit probes the proxy for a backend's per-slot context size.
// When backend is non-empty, it is sent as ?backend=<backend>.
// When model is non-empty, it is sent as ?model=<url-encoded model>.
//
// Endpoints tried, in order:
//
//  1. GET /v1/ilm/limits — the proxy's dedicated route. Flat JSON:
//     {"n_ctx": 196608, "n_ctx_train": 262144, "usable_ctx": 188416, "context_source": "props"}
//  2. GET /props — llama-server's native props endpoint (a clean passthrough).
//     n_ctx lives under default_generation_settings; n_ctx_train may appear
//     top-level or nested. Does NOT emit usable_ctx or context_source.
func fetchContextLimit(ctx context.Context, httpc *http.Client, baseURL, auth, backend, model string) (limitsResult, error) {
	base := strings.TrimRight(baseURL, "/")
	if httpc == nil {
		httpc = http.DefaultClient
	}
	cctx, cancel := context.WithTimeout(ctx, limitsTimeout)
	defer cancel()

	var lastErr error
	for _, p := range []string{"/v1/ilm/limits", "/props"} {
		body, gErr := getJSONWithBackend(cctx, httpc, base+p, auth, backend, model)
		if gErr != nil {
			lastErr = gErr
			continue
		}
		res := parseContextLimitJSON(body)
		if res.nCtx > 0 {
			res.path = p
			return res, nil
		}
		lastErr = fmt.Errorf("%s returned no n_ctx", p)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no context-limit endpoint responded")
	}
	return limitsResult{}, lastErr
}

// fetchContextLimitForKind dispatches the limits probe by endpoint kind.
// ilm-proxy keeps the exact historical sequence; openai never calls
// /v1/ilm/limits.
func fetchContextLimitForKind(ctx context.Context, httpc *http.Client, cfg config.Config, backend, model string) (limitsResult, error) {
	ep := cfg.ActiveEndpoint()
	if ep.Kind != config.EndpointKindOpenAI {
		// ilm-proxy (and legacy/hand-built configs): unchanged.
		return fetchContextLimit(ctx, httpc, cfg.BaseURL, cfg.AuthHeader(), backend, model)
	}
	return fetchContextLimitOpenAI(ctx, httpc, cfg, ep, model)
}

// fetchContextLimitOpenAI resolves limits for a plain OpenAI-compatible
// endpoint. openrouter.ai → registry lookup of the configured model (no
// /props probe); anything else → GET /props (llama.cpp), reusing the existing
// nested-shape parse. usable_ctx stays 0 — no server-side computation exists
// in this mode and Usable() falls back to the client-side reservation math.
func fetchContextLimitOpenAI(ctx context.Context, httpc *http.Client, cfg config.Config, ep config.EndpointConfig, model string) (limitsResult, error) {
	if isOpenRouterHost(ep.BaseURL) {
		return fetchContextLimitFromORRegistry(ctx, ep, model)
	}

	base := strings.TrimRight(ep.BaseURL, "/")
	if httpc == nil {
		httpc = http.DefaultClient
	}
	cctx, cancel := context.WithTimeout(ctx, limitsTimeout)
	defer cancel()

	body, err := getJSONWithBackend(cctx, httpc, base+"/props", cfg.AuthHeader(), "", "")
	if err != nil {
		return limitsResult{}, err
	}
	res := parseContextLimitJSON(body)
	if res.nCtx <= 0 {
		return limitsResult{}, fmt.Errorf("/props returned no n_ctx")
	}
	res.path = "/props"
	return res, nil
}

// fetchContextLimitFromORRegistry resolves the model's context length against
// the OpenRouter models registry (shared fetch+cache in internal/orregistry).
// model overrides the endpoint's configured model when non-empty (the /model
// command re-resolve path). NCtxTrain is left 0 — the registry doesn't
// distinguish trained vs served context.
func fetchContextLimitFromORRegistry(ctx context.Context, ep config.EndpointConfig, model string) (limitsResult, error) {
	m := model
	if m == "" {
		m = ep.Model
	}
	entries, err := orregistry.Fetch(ctx)
	if err != nil {
		return limitsResult{}, fmt.Errorf("openrouter registry: %w", err)
	}
	cl, ok := entries[m]
	if !ok || cl <= 0 {
		return limitsResult{}, fmt.Errorf("openrouter registry: model %q not found", m)
	}
	return limitsResult{
		nCtx:          cl,
		contextSource: "model_meta",
		path:          "openrouter-registry",
		model:         m,
	}, nil
}

// isOpenRouterHost reports whether rawURL's host is openrouter.ai (or a
// subdomain). Parsed-host check, not string-contains — "http://evil.example/
// openrouter.ai" must not match.
func isOpenRouterHost(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "openrouter.ai" || strings.HasSuffix(h, ".openrouter.ai")
}

// getJSONWithBackend issues a GET and returns the response body, bounded so a
// misbehaving endpoint can't stream an unbounded payload into memory. Backend
// and model are both sent as query parameters (?backend=<b>&model=<url-encoded>)
// when non-empty — the proxy reads them from the query string on the limits
// route (unlike chat requests, which send the backend via the X-Ilm-Backend
// header).
func getJSONWithBackend(ctx context.Context, httpc *http.Client, url, auth, backend, model string) ([]byte, error) {
	// Build query string: ?backend=<b>&model=<url-encoded-model>
	queryParts := make([]string, 0, 2)
	if backend != "" {
		queryParts = append(queryParts, "backend="+urlQueryEscape(backend))
	}
	if model != "" {
		queryParts = append(queryParts, "model="+urlQueryEscape(model))
	}
	if len(queryParts) > 0 {
		url = url + "?" + strings.Join(queryParts, "&")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// parseContextLimitJSON parses the full limits response body into a limitsResult.
// Reads n_ctx, n_ctx_train, usable_ctx, and context_source from the flat
// /v1/ilm/limits shape. Falls back to the nested llama-server /props shape for
// n_ctx/n_ctx_train (usable_ctx and context_source are not present in /props).
func parseContextLimitJSON(body []byte) limitsResult {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return limitsResult{}
	}
	res := limitsResult{
		nCtx:          intField(raw, "n_ctx"),
		nCtxTrain:     intField(raw, "n_ctx_train"),
		usableCtx:     intField(raw, "usable_ctx"),
		contextSource: stringField(raw, "context_source"),
		model:         stringField(raw, "model"),
		fallbackModel: stringField(raw, "fallback_model"),
	}
	// "resolved" is only emitted by proxies with the P41 fix. Absence must not
	// mark the limit unresolved — old proxies resolve fine and just don't say so.
	if v, ok := raw["resolved"]; ok {
		var b bool
		if json.Unmarshal(v, &b) == nil {
			res.resolved = b
			res.resolvedPresent = true
		}
	}

	// llama-server /props nests the per-slot n_ctx under default_generation_settings.
	if dgs, ok := raw["default_generation_settings"]; ok {
		var inner map[string]json.RawMessage
		if json.Unmarshal(dgs, &inner) == nil {
			if res.nCtx == 0 {
				res.nCtx = intField(inner, "n_ctx")
			}
			if res.nCtxTrain == 0 {
				res.nCtxTrain = intField(inner, "n_ctx_train")
			}
		}
	}
	return res
}

// ParseContextLimitJSON is the exported legacy entrypoint for callers that only
// need n_ctx and n_ctx_train (tests, subagent compatibility).
func ParseContextLimitJSON(body []byte) (nCtx, nCtxTrain int) {
	res := parseContextLimitJSON(body)
	return res.nCtx, res.nCtxTrain
}

// contextLimit returns the resolved per-slot context window. When CtxLimit was
// never populated (tests, subagents, a build path that skipped the startup
// probe) it synthesizes a fallback from Cfg so callers always get a positive
// ceiling and consistent headroom without a network round-trip.
func (a *App) ContextLimit() ContextLimit {
	if a.CtxLimit.NCtx > 0 {
		return a.CtxLimit
	}
	fb := a.Cfg.ContextTokensFallback
	if fb <= 0 {
		fb = config.DefaultConfig().ContextTokensFallback
	}
	return ContextLimit{
		NCtx:            fb,
		Source:          "fallback",
		ReasoningBudget: a.Cfg.ReasoningBudgetTokens,
		AnswerMargin:    a.Cfg.AnswerMarginTokens,
	}
}

// contextTokensUsed estimates how many prompt tokens the current context
// occupies. The proxy's last reported prompt_tokens is authoritative — it counts
// the system prompt, tool schemas, and any injected retrieval that a byte-count
// of the stored transcript would miss — so it is preferred. Before the first
// completion (or against a client that reports no usage) it falls back to a
// ~4-chars/token estimate over the transcript.
func (a *App) ContextTokensUsed() int {
	if a.Client != nil {
		if u := a.Client.LastUsage(); u.InputTok > 0 {
			return int(u.InputTok)
		}
	}
	return int(proxy.ApproxTokens(TranscriptSize(a.Conv)))
}

// nearContextLimit reports whether the current context occupancy is within ~10%
// of the usable budget — the regime where a turn that also thinks and answers
// can spill past n_ctx and trip a mid-stream reset. Used to annotate the
// backend-stream-error warning when the reset correlates with context pressure.
func (a *App) NearContextLimit() bool {
	usable := a.ContextLimit().Usable()
	if usable <= 0 {
		return false
	}
	return a.ContextTokensUsed() >= usable*9/10
}

// intField reads a JSON number field as an int, tolerating both integer and
// float encodings. Missing or non-numeric fields yield 0.
func intField(m map[string]json.RawMessage, key string) int {
	v, ok := m[key]
	if !ok {
		return 0
	}
	var f float64
	if json.Unmarshal(v, &f) != nil {
		return 0
	}
	return int(f)
}

// stringField reads a JSON string field. Missing, non-string, or null fields
// yield "".
func stringField(m map[string]json.RawMessage, key string) string {
	v, ok := m[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return ""
	}
	return s
}

// urlQueryEscape encodes s for use in a URL query string, escaping characters
// like '/' that are common in model IDs (e.g. "google/gemini-2.5-pro").
func urlQueryEscape(s string) string {
	result := make([]byte, 0, len(s)+len(s)/3)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if shouldEscape(c) {
			result = append(result, '%', "0123456789ABCDEF"[c>>4], "0123456789ABCDEF"[c&0xF])
		} else {
			result = append(result, c)
		}
	}
	return string(result)
}

func shouldEscape(c byte) bool {
	// Unreserved characters (RFC 3986): ALPHA / DIGIT / - / . / _ / ~
	if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' {
		return false
	}
	switch c {
	case '-', '.', '_', '~':
		return false
	default:
		return true
	}
}
