package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"wakil/internal/config"
	"wakil/internal/proxy"
)

// ContextLimit is the authoritative per-slot context window, resolved once at
// startup and cached for the process lifetime. NCtx is the real ceiling the
// backend enforces (in tokens); everything else in Wakil that reasons about
// "how full is the window" keys off this number rather than a hardcoded guess.
//
// Usable() carves the prompt budget out of NCtx by subtracting the headroom a
// completion needs — a turn that thinks (ReasoningBudget) and then answers
// (AnswerMargin) cannot also fill the window to the brim, so the pressure and
// compaction logic must aim below NCtx, not at it.
type ContextLimit struct {
	NCtx            int    // per-slot context size in tokens (the hard ceiling)
	NCtxTrain       int    // model's trained context length in tokens; 0 if the backend didn't report it
	Source          string // "backend" (fetched) or "fallback" (config; backend unreachable)
	ReasoningBudget int    // tokens reserved for extended thinking
	AnswerMargin    int    // tokens reserved for the final answer
}

// FromBackend reports whether NCtx came from the live backend (true) rather than
// the configured fallback (false). A fallback value must never masquerade as
// truth — callers surface it loudly.
func (c ContextLimit) FromBackend() bool { return c.Source == "backend" }

// Usable returns the token budget available for assembling a turn's prompt:
// NCtx minus the reasoning and answer reservations. Never negative; if the
// reservations exceed NCtx (a misconfiguration on a tiny backend) it clamps to a
// small positive floor so callers don't divide by zero or treat every turn as
// over-budget.
func (c ContextLimit) Usable() int {
	u := c.NCtx - c.ReasoningBudget - c.AnswerMargin
	if u < 1 {
		return 1
	}
	return u
}

// limitsTimeout bounds the startup probe so an unreachable or slow backend can't
// stall launch — on timeout the caller falls back to the configured ceiling.
const limitsTimeout = 5 * time.Second

// resolveContextLimit fetches the per-slot n_ctx from the backend (via the proxy)
// and returns the authoritative ContextLimit. On any failure it falls back to
// cfg.ContextTokensFallback and writes a loud, single-line warning to out so a
// stale fallback can't be mistaken for the real ceiling. The headroom fields are
// always taken from cfg regardless of which path supplied NCtx.
func ResolveContextLimit(ctx context.Context, httpc *http.Client, cfg config.Config, out io.Writer) ContextLimit {
	lim := ContextLimit{
		ReasoningBudget: cfg.ReasoningBudgetTokens,
		AnswerMargin:    cfg.AnswerMarginTokens,
	}

	nCtx, nCtxTrain, path, err := fetchContextLimit(ctx, httpc, cfg.BaseURL, cfg.AuthHeader(), "")
	if err != nil || nCtx <= 0 {
		lim.NCtx = cfg.ContextTokensFallback
		lim.Source = "fallback"
		reason := "backend reported no usable n_ctx"
		if err != nil {
			reason = err.Error()
		}
		fmt.Fprintf(out, "⚠ using fallback context limit %d tokens (%s) — set the backend or context_tokens_fallback\n",
			lim.NCtx, reason)
		return lim
	}

	lim.NCtx = nCtx
	lim.NCtxTrain = nCtxTrain
	lim.Source = "backend"
	trainNote := ""
	if nCtxTrain > 0 {
		trainNote = fmt.Sprintf(", n_ctx_train=%d", nCtxTrain)
	}
	fmt.Fprintf(out, "context limit: n_ctx=%d%s (from backend %s); usable=%d (−%d reasoning −%d answer)\n",
		nCtx, trainNote, path, lim.Usable(), lim.ReasoningBudget, lim.AnswerMargin)
	return lim
}

// fetchContextLimit probes the proxy for the backend's per-slot context size.
// It tries two shapes, in order, returning on the first that yields a positive
// n_ctx:
//
//  1. GET /v1/ilm/limits — the proxy's dedicated route. Flat JSON:
//     {"n_ctx": 196608, "n_ctx_train": 262144}
//  2. GET /props — llama-server's native props endpoint (a clean passthrough).
//     n_ctx lives under default_generation_settings; n_ctx_train may appear
//     top-level or nested.
//
// The returned path names which endpoint answered (for the startup log). On
// total failure it returns the last error so the fallback note can explain why.
// ResolveContextLimitForBackend is like ResolveContextLimit but probes the
// proxy for a specific backend's context window by sending X-Ilm-Backend in the
// request. Called by the /backend command handler to re-synchronise thresholds
// when the user switches backends mid-session.
func ResolveContextLimitForBackend(ctx context.Context, httpc *http.Client, cfg config.Config, backend string, out io.Writer) ContextLimit {
	lim := ContextLimit{
		ReasoningBudget: cfg.ReasoningBudgetTokens,
		AnswerMargin:    cfg.AnswerMarginTokens,
	}
	nCtx, nCtxTrain, path, err := fetchContextLimit(ctx, httpc, cfg.BaseURL, cfg.AuthHeader(), backend)
	if err != nil || nCtx <= 0 {
		lim.NCtx = cfg.ContextTokensFallback
		lim.Source = "fallback"
		reason := "backend reported no usable n_ctx"
		if err != nil {
			reason = err.Error()
		}
		fmt.Fprintf(out, "⚠ using fallback context limit %d tokens for backend %q (%s)\n",
			lim.NCtx, backend, reason)
		return lim
	}
	lim.NCtx = nCtx
	lim.NCtxTrain = nCtxTrain
	lim.Source = "backend"
	trainNote := ""
	if nCtxTrain > 0 {
		trainNote = fmt.Sprintf(", n_ctx_train=%d", nCtxTrain)
	}
	fmt.Fprintf(out, "backend %q: n_ctx=%d%s (from %s); usable=%d\n",
		backend, nCtx, trainNote, path, lim.Usable())
	return lim
}

// fetchContextLimit probes the proxy for a backend's per-slot context size.
// When backend is non-empty the X-Ilm-Backend header is sent so the proxy
// returns the limits for that specific backend rather than the default.
func fetchContextLimit(ctx context.Context, httpc *http.Client, baseURL, auth, backend string) (nCtx, nCtxTrain int, path string, err error) {
	base := strings.TrimRight(baseURL, "/")
	if httpc == nil {
		httpc = http.DefaultClient
	}
	cctx, cancel := context.WithTimeout(ctx, limitsTimeout)
	defer cancel()

	var lastErr error
	for _, p := range []string{"/v1/ilm/limits", "/props"} {
		body, gErr := getJSONWithBackend(cctx, httpc, base+p, auth, backend)
		if gErr != nil {
			lastErr = gErr
			continue
		}
		c, t := ParseContextLimitJSON(body)
		if c > 0 {
			return c, t, p, nil
		}
		lastErr = fmt.Errorf("%s returned no n_ctx", p)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no context-limit endpoint responded")
	}
	return 0, 0, "", lastErr
}

// getJSON issues a GET and returns the response body, bounded so a misbehaving
// endpoint can't stream an unbounded payload into memory.
func getJSON(ctx context.Context, httpc *http.Client, url, auth string) ([]byte, error) {
	return getJSONWithBackend(ctx, httpc, url, auth, "")
}

// getJSONWithBackend is like getJSON but adds X-Ilm-Backend when backend is
// non-empty, routing the probe to a specific backend's context window.
func getJSONWithBackend(ctx context.Context, httpc *http.Client, url, auth, backend string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	if backend != "" {
		req.Header.Set("X-Ilm-Backend", backend)
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

// parseContextLimitJSON pulls n_ctx (per-slot) and n_ctx_train out of either the
// flat /v1/ilm/limits shape or the nested llama-server /props shape. It is
// deliberately permissive: it checks the top level first, then
// default_generation_settings, so it survives minor schema drift across
// llama-server versions and proxy builds.
func ParseContextLimitJSON(body []byte) (nCtx, nCtxTrain int) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return 0, 0
	}
	nCtx = intField(raw, "n_ctx")
	nCtxTrain = intField(raw, "n_ctx_train")

	// llama-server /props nests the per-slot n_ctx under default_generation_settings.
	if dgs, ok := raw["default_generation_settings"]; ok {
		var inner map[string]json.RawMessage
		if json.Unmarshal(dgs, &inner) == nil {
			if nCtx == 0 {
				nCtx = intField(inner, "n_ctx")
			}
			if nCtxTrain == 0 {
				nCtxTrain = intField(inner, "n_ctx_train")
			}
		}
	}
	return nCtx, nCtxTrain
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
