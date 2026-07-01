package counsel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"wakil/internal/config"
)

const oracleEndpoint = "https://api.anthropic.com/v1/messages"

const oracleSystemPrompt = "You are consulted for a second opinion by a local agent. Be direct and concise. Distinguish explicitly between (a) what the provided context shows and (b) what you recall from training. Never state a version-dependent or environment-dependent claim as confirmed — name what file or output would confirm it. Flag uncertainty plainly."

// oracleReq is the Anthropic Messages API request body.
type oracleReq struct {
	Model     string      `json:"model"`
	MaxTokens int         `json:"max_tokens"`
	System    string      `json:"system"`
	Messages  []oracleMsg `json:"messages"`
}

type oracleMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// oracleResp is the relevant subset of the Anthropic Messages API response body.
type oracleResp struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason string `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// OracleUsage is the exact token usage reported by the Messages API, surfaced to
// the caller so the cost tracker can record a billed-grade (exact) figure.
type OracleUsage struct {
	InputTokens  int64
	OutputTokens int64
}

// CallOracle sends question and (optionally) oracleCtx to the Anthropic Messages
// API and returns the assistant text plus the call's token usage. ctx is the
// turn context; an additional timeout is layered so a slow response cannot stall
// indefinitely.
func CallOracle(ctx context.Context, cfg config.Config, apiKey, question, oracleCtx string) (string, OracleUsage, error) {
	endpoint := oracleEndpoint
	if cfg.OracleEndpoint != "" {
		endpoint = cfg.OracleEndpoint
	}
	return CallOracleURL(ctx, cfg, apiKey, question, oracleCtx, endpoint)
}

// CallOracleURL is the implementation of CallOracle with an explicit endpoint,
// used in tests to point at a local httptest.Server instead of the live API.
func CallOracleURL(ctx context.Context, cfg config.Config, apiKey, question, oracleCtx, endpoint string) (string, OracleUsage, error) {
	userContent := question
	if oracleCtx != "" {
		userContent += "\n\nContext:\n" + oracleCtx
	}

	body, err := json.Marshal(oracleReq{
		Model:     cfg.OracleModel,
		MaxTokens: cfg.OracleMaxTokens,
		System:    oracleSystemPrompt,
		Messages:  []oracleMsg{{Role: "user", Content: userContent}},
	})
	if err != nil {
		return "", OracleUsage{}, fmt.Errorf("marshal: %w", err)
	}

	// Derive a child context so Ctrl-C (parent cancel) and the configured
	// timeout both terminate. Default is 300s (Anthropic non-streaming calls
	// with large max_tokens can legitimately take several minutes).
	timeout := time.Duration(cfg.OracleTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(tctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", OracleUsage{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", OracleUsage{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", OracleUsage{}, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var apiErr oracleResp
		if json.Unmarshal(raw, &apiErr) == nil && apiErr.Error != nil {
			msg := apiErr.Error.Message
			// Anthropic rejects non-streaming requests whose response would exceed
			// an internal size threshold. Streaming the oracle call is the proper
			// long-term fix; for now surface a clear hint rather than a raw 400.
			if resp.StatusCode == 400 && strings.Contains(msg, "max_tokens") {
				msg += " (API rejected large non-streaming request; reduce oracle_max_tokens or stream the call)"
			}
			return "", OracleUsage{}, fmt.Errorf("%s: %s", resp.Status, msg)
		}
		return "", OracleUsage{}, fmt.Errorf("%s", resp.Status)
	}

	var result oracleResp
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", OracleUsage{}, fmt.Errorf("parse response: %w", err)
	}

	// Classify content blocks and collect debug info.
	var textParts []string
	var blockTypes []string
	for _, c := range result.Content {
		blockTypes = append(blockTypes, c.Type)
		if c.Type == "text" {
			textParts = append(textParts, c.Text)
		}
	}

	// Debug log: write raw response structure to stderr when WAKIL_DEBUG is set.
	if os.Getenv("WAKIL_DEBUG") != "" {
		fmt.Fprintf(os.Stderr, "[oracle debug] stop_reason=%q blocks=[%s] input=%d output=%d\n",
			result.StopReason,
			strings.Join(blockTypes, ","),
			result.Usage.InputTokens,
			result.Usage.OutputTokens,
		)
	}

	// max_tokens check runs FIRST so the actionable remediation hint wins even
	// when conditions co-occur (e.g. thinking model emits only whitespace before
	// hitting the limit — that is still a truncation, not an empty-response).
	//
	// Both sub-cases (no text / partial text) route to ok=false at the call site;
	// partial text is not salvaged — a truncated oracle answer is as unreliable
	// as no answer at all.
	if result.StopReason == "max_tokens" {
		var hasThinking bool
		for _, t := range blockTypes {
			if t == "thinking" {
				hasThinking = true
			}
		}
		remediation := "; raise oracle_max_tokens to avoid truncation"
		if hasThinking {
			remediation = "; raise oracle_max_tokens or use a non-thinking model"
		}
		if len(textParts) == 0 {
			return "", OracleUsage{}, fmt.Errorf("oracle hit max_tokens before emitting any text%s", remediation)
		}
		joined := strings.Join(textParts, "\n")
		return "", OracleUsage{}, fmt.Errorf("oracle response truncated at max_tokens (%d chars received)%s", len(joined), remediation)
	}

	// Reject empty or whitespace-only text (stop_reason is not max_tokens here).
	if len(textParts) == 0 {
		return "", OracleUsage{}, fmt.Errorf("oracle response contains no text blocks (blocks=%v, stop_reason=%q)", blockTypes, result.StopReason)
	}
	joined := strings.Join(textParts, "\n")
	if strings.TrimSpace(joined) == "" {
		return "", OracleUsage{}, fmt.Errorf("oracle response is empty or whitespace-only")
	}

	usage := OracleUsage{
		InputTokens:  int64(result.Usage.InputTokens),
		OutputTokens: int64(result.Usage.OutputTokens),
	}
	return joined, usage, nil
}

// OracleDetail builds the human-readable detail shown in the confirm-gate
// prompt. The full payload (model + question + context) is shown, but truncated
// at ~2000 display chars with a "(+N chars)" suffix — the actual HTTP call uses
// the untruncated question and context.
func OracleDetail(model, question, oracleCtx string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "model:    %s\nquestion: %s", model, question)
	if oracleCtx != "" {
		fmt.Fprintf(&sb, "\ncontext:  %s", oracleCtx)
	}
	full := sb.String()
	const maxDisplay = 2000
	if len(full) <= maxDisplay {
		return full
	}
	excess := len(full) - maxDisplay
	return full[:maxDisplay] + fmt.Sprintf(" (+%d chars)", excess)
}

// ── Multi-model panel support ─────────────────────────────────────────────────

// PanelCallConfig carries per-call parameters shared by all providers in a panel.
type PanelCallConfig struct {
	MaxTokens          int
	TimeoutSeconds     int    // 0 → 300s default
	AnthropicEndpoint  string // "" = production; override in tests
	OpenRouterEndpoint string // "" = "https://openrouter.ai/api/v1/chat/completions"
	FusionJudge        string // fusion mode: judge model; "" = OpenRouter default
	FusionMaxToolCalls int    // fusion mode: tool-call steps per model (1–16); 0 = default (8)
}

const openRouterEndpoint = "https://openrouter.ai/api/v1/chat/completions"

// ParseModelPrefix splits "provider:model-id" into provider and model.
// A bare name without a colon defaults to the "anthropic" provider.
func ParseModelPrefix(s string) (provider, model string) {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "anthropic", s
}

// PanelMemberResult holds one panel member's outcome.
type PanelMemberResult struct {
	PrefixedModel string      // full "provider:model" as configured
	Model         string      // bare model ID (without provider prefix)
	Answer        string      // empty on error
	Usage         OracleUsage // zero on error
	Err           error       // nil on success
}

// RunPanel executes the panel according to its mode.
//
// panel mode: queries all members sequentially, collects all results.
// fallback mode: queries in order, stops on first success.
// fusion mode: sends ONE OpenRouter Fusion request (models → analysis_models);
//   OpenRouter runs the panel in parallel internally and returns the judge's
//   structured analysis.
//
// Each member in panel/fallback receives an identical briefing — independent
// opinions, never chained.
// TODO(parallel): fan-out here — replace the panel/fallback loop with a goroutine
// pool that writes into the results slice; the slice shape and all call sites
// are unchanged, so parallelism is a localized swap.
func RunPanel(ctx context.Context, models []string, mode, question, briefing string, ccfg PanelCallConfig, apiKeys map[string]string) []PanelMemberResult {
	if mode == "fusion" {
		// Single OpenRouter Fusion call; models become the analysis panel.
		answer, usage, err := callFusion(ctx, models, apiKeys["openrouter"], question, briefing, ccfg)
		return []PanelMemberResult{{
			PrefixedModel: "openrouter:openrouter/fusion",
			Model:         "openrouter/fusion",
			Answer:        answer,
			Usage:         usage,
			Err:           err,
		}}
	}

	results := make([]PanelMemberResult, 0, len(models))
	for _, pm := range models {
		prov, model := ParseModelPrefix(pm)
		answer, usage, err := callMember(ctx, prov, model, question, briefing, ccfg, apiKeys)
		results = append(results, PanelMemberResult{
			PrefixedModel: pm,
			Model:         model,
			Answer:        answer,
			Usage:         usage,
			Err:           err,
		})
		if mode == "fallback" && err == nil {
			// Fallback: stop on first success; remaining members not tried.
			break
		}
	}
	return results
}

// callMember dispatches a single consultation to the right provider.
func callMember(ctx context.Context, prov, model, question, briefing string, ccfg PanelCallConfig, apiKeys map[string]string) (string, OracleUsage, error) {
	key := apiKeys[prov]
	switch prov {
	case "anthropic":
		return callAnthropic(ctx, model, key, question, briefing, ccfg)
	case "openrouter":
		return callOpenRouter(ctx, model, key, question, briefing, ccfg)
	default:
		return "", OracleUsage{}, fmt.Errorf("unknown provider %q", prov)
	}
}

// callAnthropic wraps the existing Anthropic path with a synthetic config.
func callAnthropic(ctx context.Context, model, apiKey, question, briefing string, ccfg PanelCallConfig) (string, OracleUsage, error) {
	ctxLen := ResolveContextLength(ctx, model)
	fit := FitToContext(oracleSystemPrompt, question, briefing, ccfg.MaxTokens, ctxLen)
	if fit.CannotFit {
		return "", OracleUsage{}, fmt.Errorf("mashūra briefing too large for model context (%d tokens)", fit.ContextLength)
	}
	synCfg := config.Config{
		OracleModel:          model,
		OracleMaxTokens:      fit.MaxTokens,
		OracleTimeoutSeconds: ccfg.TimeoutSeconds,
		OracleEndpoint:       ccfg.AnthropicEndpoint,
	}
	return CallOracle(ctx, synCfg, apiKey, question, fit.Briefing)
}

// orFusionPlugin is the "fusion" plugin block sent in the OpenRouter request.
type orFusionPlugin struct {
	ID             string   `json:"id"`
	AnalysisModels []string `json:"analysis_models,omitempty"` // 1–8 models for the panel
	Model          string   `json:"model,omitempty"`           // judge model; "" = OpenRouter default
	MaxToolCalls   int      `json:"max_tool_calls,omitempty"`  // 1–16; 0 = default (8)
}

// orReq is the OpenAI-compatible chat completions request body used by OpenRouter.
type orReq struct {
	Model     string           `json:"model"`
	MaxTokens int              `json:"max_tokens"`
	Messages  []orMsg          `json:"messages"`
	Plugins   []orFusionPlugin `json:"plugins,omitempty"`
}

type orMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// orResp is the relevant subset of the OpenAI chat completions response.
type orResp struct {
	Choices []struct {
		Message      orMsg  `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// callOpenRouter sends a consultation to https://openrouter.ai/api/v1 using the
// OpenAI-compatible chat completions format.
func callOpenRouter(ctx context.Context, model, apiKey, question, briefing string, ccfg PanelCallConfig) (string, OracleUsage, error) {
	endpoint := openRouterEndpoint
	if ccfg.OpenRouterEndpoint != "" {
		endpoint = ccfg.OpenRouterEndpoint
	}

	ctxLen := ResolveContextLength(ctx, model)
	fit := FitToContext(oracleSystemPrompt, question, briefing, ccfg.MaxTokens, ctxLen)
	if fit.CannotFit {
		return "", OracleUsage{}, fmt.Errorf("mashūra briefing too large for model context (%d tokens)", fit.ContextLength)
	}

	userContent := question
	if fit.Briefing != "" {
		userContent += "\n\nContext:\n" + fit.Briefing
	}

	body, err := json.Marshal(orReq{
		Model:     model,
		MaxTokens: fit.MaxTokens,
		Messages: []orMsg{
			{Role: "system", Content: oracleSystemPrompt},
			{Role: "user", Content: userContent},
		},
	})
	if err != nil {
		return "", OracleUsage{}, fmt.Errorf("marshal: %w", err)
	}

	timeout := time.Duration(ccfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(tctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", OracleUsage{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", OracleUsage{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", OracleUsage{}, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		var apiErr orResp
		if json.Unmarshal(raw, &apiErr) == nil && apiErr.Error != nil {
			return "", OracleUsage{}, fmt.Errorf("%s: %s", resp.Status, apiErr.Error.Message)
		}
		return "", OracleUsage{}, fmt.Errorf("%s", resp.Status)
	}

	var result orResp
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", OracleUsage{}, fmt.Errorf("parse response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", OracleUsage{}, fmt.Errorf("openrouter: no choices in response")
	}
	text := strings.TrimSpace(result.Choices[0].Message.Content)
	if text == "" {
		return "", OracleUsage{}, fmt.Errorf("openrouter: empty response content")
	}
	if result.Choices[0].FinishReason == "length" {
		return "", OracleUsage{}, fmt.Errorf("openrouter: response truncated at max_tokens; raise max_tokens to avoid truncation")
	}

	var usage OracleUsage
	if result.Usage != nil {
		usage.InputTokens = int64(result.Usage.PromptTokens)
		usage.OutputTokens = int64(result.Usage.CompletionTokens)
	}
	return text, usage, nil
}

// callFusion sends a single OpenRouter Fusion request. analysisModels are sent
// as "analysis_models" in the plugin block; OpenRouter runs them in parallel
// and the judge synthesizes their responses. Returns the judge's analysis text.
func callFusion(ctx context.Context, analysisModels []string, apiKey, question, briefing string, ccfg PanelCallConfig) (string, OracleUsage, error) {
	endpoint := openRouterEndpoint
	if ccfg.OpenRouterEndpoint != "" {
		endpoint = ccfg.OpenRouterEndpoint
	}

	ctxLen := ResolveContextLength(ctx, "openrouter/fusion")
	fit := FitToContext(oracleSystemPrompt, question, briefing, ccfg.MaxTokens, ctxLen)
	if fit.CannotFit {
		return "", OracleUsage{}, fmt.Errorf("mashūra briefing too large for model context (%d tokens)", fit.ContextLength)
	}

	userContent := question
	if fit.Briefing != "" {
		userContent += "\n\nContext:\n" + fit.Briefing
	}

	plugin := orFusionPlugin{ID: "fusion"}
	if len(analysisModels) > 0 {
		plugin.AnalysisModels = analysisModels
	}
	if ccfg.FusionJudge != "" {
		plugin.Model = ccfg.FusionJudge
	}
	if ccfg.FusionMaxToolCalls > 0 {
		plugin.MaxToolCalls = ccfg.FusionMaxToolCalls
	}

	body, err := json.Marshal(orReq{
		Model:     "openrouter/fusion",
		MaxTokens: fit.MaxTokens,
		Messages: []orMsg{
			{Role: "system", Content: oracleSystemPrompt},
			{Role: "user", Content: userContent},
		},
		Plugins: []orFusionPlugin{plugin},
	})
	if err != nil {
		return "", OracleUsage{}, fmt.Errorf("marshal: %w", err)
	}

	timeout := time.Duration(ccfg.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 300 * time.Second
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(tctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", OracleUsage{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return "", OracleUsage{}, err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", OracleUsage{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		var apiErr orResp
		if json.Unmarshal(raw, &apiErr) == nil && apiErr.Error != nil {
			return "", OracleUsage{}, fmt.Errorf("%s: %s", resp.Status, apiErr.Error.Message)
		}
		return "", OracleUsage{}, fmt.Errorf("%s", resp.Status)
	}

	var result orResp
	if err := json.Unmarshal(raw, &result); err != nil {
		return "", OracleUsage{}, fmt.Errorf("parse response: %w", err)
	}
	if len(result.Choices) == 0 {
		return "", OracleUsage{}, fmt.Errorf("openrouter/fusion: no choices in response")
	}
	text := strings.TrimSpace(result.Choices[0].Message.Content)
	if text == "" {
		return "", OracleUsage{}, fmt.Errorf("openrouter/fusion: empty response")
	}

	var usage OracleUsage
	if result.Usage != nil {
		usage.InputTokens = int64(result.Usage.PromptTokens)
		usage.OutputTokens = int64(result.Usage.CompletionTokens)
	}
	return text, usage, nil
}

// FormatPanelResult renders panel results as the tool-return string.
// For a single-member result: returns the answer verbatim (or the error string).
// For multi-member panels: wraps each member's answer in a labeled section.
// TODO(per-model-debate): a critique-of-critique mode where members see each
// other's answers is a deliberate future feature; in panel mode answers are
// always independent — do not add cross-model context here.
func FormatPanelResult(results []PanelMemberResult) string {
	if len(results) == 0 {
		return "[mashūra error: panel has no members]"
	}
	if len(results) == 1 {
		r := results[0]
		if r.Err != nil {
			return "[mashūra error: " + r.Err.Error() + "]"
		}
		return r.Answer
	}
	var sb strings.Builder
	for i, r := range results {
		if i > 0 {
			sb.WriteString("\n\n")
		}
		fmt.Fprintf(&sb, "── %s ──\n", r.Model)
		if r.Err != nil {
			fmt.Fprintf(&sb, "[error: %s]", r.Err.Error())
		} else {
			sb.WriteString(r.Answer)
		}
	}
	return sb.String()
}

// PanelDetail builds the confirm-gate detail for a panel call. For a
// single-model panel it falls back to OracleDetail-style formatting (backward
// compat). For multi-model panels and fusion it shows the full configuration.
func PanelDetail(panelName string, models []string, mode, question, oracleCtx string) string {
	var sb strings.Builder
	switch {
	case mode == "fusion":
		fmt.Fprintf(&sb, "panel:    %s (fusion, %d analysis models)\n", panelName, len(models))
		fmt.Fprintf(&sb, "analysis: %s\n", strings.Join(models, ", "))
		fmt.Fprintf(&sb, "question: %s", question)
		if oracleCtx != "" {
			fmt.Fprintf(&sb, "\ncontext:  %s", oracleCtx)
		}
	case len(models) == 1:
		_, model := ParseModelPrefix(models[0])
		fmt.Fprintf(&sb, "model:    %s\nquestion: %s", model, question)
		if oracleCtx != "" {
			fmt.Fprintf(&sb, "\ncontext:  %s", oracleCtx)
		}
	default:
		fmt.Fprintf(&sb, "panel:    %s (%d models, mode: %s)\n", panelName, len(models), mode)
		fmt.Fprintf(&sb, "models:   %s\n", strings.Join(models, ", "))
		fmt.Fprintf(&sb, "question: %s", question)
		if oracleCtx != "" {
			fmt.Fprintf(&sb, "\ncontext:  %s", oracleCtx)
		}
	}
	full := sb.String()
	const maxDisplay = 2000
	if len(full) <= maxDisplay {
		return full
	}
	excess := len(full) - maxDisplay
	return full[:maxDisplay] + fmt.Sprintf(" (+%d chars)", excess)
}
