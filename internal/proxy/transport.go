package proxy

// Single transport for all model calls (data policy, step 2).
//
// Every request to a model host — the main loop (Stream), Mashūra panel
// calls, aux calls, and the init-time test call — goes through this file.
// Exec is the seam: a caller that already built its request body (a Mashūra
// panel member, the init test call) sends the body bytes here instead of
// opening its own HTTP request. Exec applies the data policy, owns the
// HTTP round trip (timeouts, streaming, idle detection, partial usage), and
// returns the collected answer plus exact usage, mirroring what counsel
// previously did with its own HTTP client.
//
// Errors follow the package convention: ErrBackendStream (retryable) for
// transport/stream failures, ErrBackendFatal for non-200 responses. The
// caller maps those onto its own vocabulary (VOID / ProviderHTTPError…).

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// ChatRequest is one model call through the transport. Body is the complete
// serialized request body as built by the caller (a Mashūra panel member, the
// init test call); the transport never inspects it beyond the data policy's
// provider-object merge, which preserves every other field.
type ChatRequest struct {
	// BaseURL is the full request URL, path included
	// ("https://openrouter.ai/api/v1/chat/completions"). The policy host is
	// derived from it; a regional pin rewrites only the host.
	BaseURL string

	// Model is the requested model id, used for the policy's no-ZDR memory.
	Model string

	// Body is the serialized request body, sent verbatim unless the policy
	// adds provider fields.
	Body []byte

	// Headers are applied verbatim (Content-Type: application/json and
	// Accept: text/event-stream are always set; caller values win). Never
	// logged or stored.
	Headers map[string]string

	// IdleTimeout bounds mid-stream stalls (0 = 120s); TotalTimeout bounds
	// the whole call (0 = 30min).
	IdleTimeout  time.Duration
	TotalTimeout time.Duration

	// Observe, when set, receives each raw SSE data line (already joined,
	// before parsing) as it arrives — the panel debug capture hook. It runs
	// on the stream path; keep it fast and non-blocking.
	Observe func(line string)
}

// ChatResult is the collected outcome of one Exec call.
type ChatResult struct {
	// Content is the assembled text of the reply (SSE deltas concatenated).
	Content string

	// FinishReason is the provider's stop signal ("stop", "length", …).
	FinishReason string

	// Usage is the exact token/cost accounting from the trailing usage chunk;
	// fields are zero when the provider reported none.
	Usage UsageStat

	// StatusCode is the HTTP status (200 on a parsed stream).
	StatusCode int

	// SentBody is the request body as finally sent, after the data policy
	// ran (provider fields added, or stripped by the auto fallback). Set
	// whenever a policy decision was recorded.
	SentBody []byte

	// ToolCalls are the assembled function calls, in stream order. Empty for
	// plain-text replies. Kept for the init test call, which asserts that a
	// tool call came back.
	ToolCalls []ToolCall
}

// HTTPStatusError reports a non-200 HTTP response with its body. It wraps
// ErrBackendFatal so the retry loop treats it as non-retryable, and carries
// the raw body so callers can parse provider error text (the privacy-policy
// 404 check lives in the caller, as it did in counsel).
type HTTPStatusError struct {
	StatusCode int
	Body       string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("HTTP %d: %s", e.StatusCode, e.Body)
}

func (e *HTTPStatusError) Unwrap() error { return ErrBackendFatal }

// PolicyRefusedError reports that the data policy refused the request before
// sending. Zero bytes were sent.
type PolicyRefusedError struct{ Err error }

func (e *PolicyRefusedError) Error() string { return e.Err.Error() }
func (e *PolicyRefusedError) Unwrap() error { return e.Err }

// Exec sends one chat-completions request through the data policy and the
// shared HTTP client, returning the collected answer and usage.
func (c *Client) Exec(ctx context.Context, req ChatRequest) (ChatResult, error) {
	return c.exec(ctx, req, nil)
}

// ExecObserved is Exec with a per-call raw-line observer (the panel capture).
func (c *Client) ExecObserved(ctx context.Context, req ChatRequest, observe func(line string)) (ChatResult, error) {
	return c.exec(ctx, req, observe)
}

func (c *Client) exec(ctx context.Context, req ChatRequest, observe func(line string)) (ChatResult, error) {
	idle := req.IdleTimeout
	if idle <= 0 {
		idle = 120 * time.Second
	}
	total := req.TotalTimeout
	if total <= 0 {
		total = 30 * time.Minute
	}
	// A caller-supplied per-request observer wins over the exec-level one
	// (panel capture sets ChatRequest.Observe).
	if req.Observe != nil {
		observe = req.Observe
	}

	send := func(body []byte, target string) (ChatResult, error) {
		return c.sendOnce(ctx, req, body, target, idle, total, observe)
	}

	zdr, region, trusted := c.DataPolicy()
	pol := ApplyPolicy(PolicyInput{
		Host:              hostOfBaseURL(req.BaseURL),
		Model:             req.Model,
		ZDR:               zdr,
		Region:            region,
		TrustedLocalHosts: trusted,
	}, req.Body)
	if pol.EnforcementErr != nil {
		return ChatResult{}, &PolicyRefusedError{Err: pol.EnforcementErr}
	}
	target := req.BaseURL
	if pol.Host != hostOfBaseURL(req.BaseURL) {
		target = withHost(req.BaseURL, pol.Host)
	}

	// Learned no-ZDR model: send without the fields from the start.
	if zdr == "auto" && pol.WithZDRFields && req.Model != "" && c.hasNoZDR(req.Model) {
		pol.Body = withoutZDRFields(pol.Body)
		pol.WithZDRFields = false
		pol.Decision = "sent without ZDR (not available)"
	}

	if pol.Decision != "" {
		c.recordDecision(pol.Decision, req.Model)
	}

	res, err := send(pol.Body, target)
	if err == nil {
		res.SentBody = pol.Body
		return res, nil
	}
	var hsErr *HTTPStatusError
	if !errors.As(err, &hsErr) || hsErr.StatusCode != http.StatusNotFound || !pol.WithZDRFields {
		return ChatResult{}, err
	}
	if zdr != "auto" {
		return ChatResult{}, err
	}
	// zdr auto: exactly one retry without the two provider fields.
	retryBody := withoutZDRFields(pol.Body)
	res2, err2 := send(retryBody, target)
	if err2 != nil {
		return ChatResult{}, err2
	}
	res2.SentBody = retryBody
	// The fallback succeeded: remember the model, record the decision, and
	// emit the one-per-model-per-session transcript notice. Fired WITHOUT
	// holding policyMu (the hook runs on the agent goroutine).
	if req.Model != "" {
		first := c.markNoZDR(req.Model)
		c.recordDecision("sent without ZDR (not available)", req.Model)
		if first {
			if hook := c.noZDRNoticeHook(); hook != nil {
				hook(req.Model)
			}
		}
	}
	return res2, nil
}

// sendOnce performs one HTTP round trip with streaming and collects the
// answer. A non-200 returns an *HTTPStatusError (with the response body);
// transport and stream failures return retryable ErrBackendStream errors;
// partial usage is preserved in the returned ChatResult even on error.
func (c *Client) sendOnce(ctx context.Context, req ChatRequest, body []byte, target string, idle, total time.Duration, observe func(line string)) (ChatResult, error) {
	tctx, cancel := context.WithTimeout(ctx, total)
	defer cancel()

	hc := c.HTTP
	if hc == nil {
		hc = defaultChatHTTPClient()
	}

	httpReq, err := http.NewRequestWithContext(tctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return ChatResult{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	for k, v := range req.Headers {
		httpReq.Header.Set(k, v)
	}

	resp, err := hc.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return ChatResult{}, ctx.Err()
		}
		// Transport failures (dial, TLS, reset) are retryable, matching
		// Stream's classification.
		return ChatResult{}, wrapStreamErr(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		return ChatResult{StatusCode: resp.StatusCode}, &HTTPStatusError{StatusCode: resp.StatusCode, Body: strings.TrimSpace(string(raw))}
	}

	// A non-SSE JSON response (some proxies answer application/json to a
	// stream:true request) is parsed as one chunk, exactly like the
	// provider parsers did in counsel: the tolerant chunk decode extracts
	// text from either shape.
	ct := resp.Header.Get("Content-Type")
	if strings.HasPrefix(ct, "application/json") {
		raw, rerr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if rerr != nil {
			return ChatResult{StatusCode: http.StatusOK}, fmt.Errorf("read response: %w", rerr)
		}
		if observe != nil {
			observe(string(raw))
		}
		return parseChatChunk([]byte(raw), http.StatusOK)
	}

	bodyReader := newIdleReader(resp.Body, idle)
	defer bodyReader.Close()
	scanner := bufio.NewScanner(bodyReader)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var content strings.Builder
	var usage UsageStat
	var usageSeen bool
	var finishReason string
	gotTerminal := false
	var dataLines []string
	toolAcc := map[int]*ToolCall{}
	var toolOrder []int
	var bytesRead int

	// applyChunk folds one decoded chunk into the accumulators. A mid-stream
	// error chunk becomes a retryable stream error; the finish reason is
	// kept from the last chunk that carried one.
	applyChunk := func(data string) error {
		var chunk streamChunk
		if jerr := json.Unmarshal([]byte(data), &chunk); jerr != nil {
			// A malformed JSON chunk aborts the stream as retryable:
			// keeping the partial message would hand the caller a
			// corrupted tool-call argument list.
			return wrapStreamErr(fmt.Errorf("malformed SSE chunk at byte %d: %w", bytesRead, jerr))
		}
		if chunk.Error != nil {
			return wrapStreamErr(fmt.Errorf("backend sent mid-stream error: %s (code %v)", chunk.Error.Message, chunk.Error.Code))
		}
		if chunk.Model != "" {
			usage.ServedModel = chunk.Model
		}
		if chunk.Usage != nil {
			usageSeen = true
			usage.InputTok = chunk.Usage.PromptTokens
			usage.OutputTok = chunk.Usage.CompletionTokens
			if d := chunk.Usage.CompletionTokensDetails; d != nil {
				usage.ReasoningTok = d.ReasoningTokens
			}
			if d := chunk.Usage.PromptTokensDetails; d != nil {
				usage.CachedTok = d.CachedTokens
			}
			usage.CacheWriteTok = chunk.Usage.CacheCreationInputTokens
			if chunk.Usage.ServerToolUse != nil {
				usage.WebSearchRequests = int64(chunk.Usage.ServerToolUse.WebSearchRequests)
			}
			if chunk.Usage.Cost != nil {
				usage.ProviderCostUSD = *chunk.Usage.Cost
				usage.HasProviderCost = true
			}
		}
		if len(chunk.Choices) > 0 {
			choice := chunk.Choices[0]
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}
			content.WriteString(choice.Delta.Content)
			for _, tc := range choice.Delta.ToolCalls {
				e, ok := toolAcc[tc.Index]
				if !ok {
					e = &ToolCall{Type: "function"}
					toolAcc[tc.Index] = e
					toolOrder = append(toolOrder, tc.Index)
				}
				if tc.ID != "" {
					e.ID = tc.ID
				}
				if tc.Type != "" {
					e.Type = tc.Type
				}
				if tc.Function.Name != "" {
					e.Function.Name = tc.Function.Name
				}
				e.Function.Arguments += tc.Function.Arguments
			}
		}
		return nil
	}

	mergeEvent := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = nil
		if data == "[DONE]" {
			gotTerminal = true
			return nil
		}
		if data == "" {
			return nil
		}
		if observe != nil {
			observe(data)
		}
		return applyChunk(data)
	}

	for scanner.Scan() {
		s := strings.TrimRight(scanner.Text(), "\r")
		bytesRead += len(s) + 1
		if s == "" {
			if err := mergeEvent(); err != nil {
				return partialResult(http.StatusOK, content.String(), finishReason, usage, usageSeen, toolAcc, toolOrder), err
			}
			if gotTerminal {
				break
			}
			continue
		}
		if strings.HasPrefix(s, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(s[len("data:"):]))
		}
		// Other SSE fields (event:, id:, retry:) are ignored per spec.
	}
	if !gotTerminal {
		if err := mergeEvent(); err != nil {
			return partialResult(http.StatusOK, content.String(), finishReason, usage, usageSeen, toolAcc, toolOrder), err
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return partialResult(http.StatusOK, content.String(), finishReason, usage, usageSeen, toolAcc, toolOrder), ctx.Err()
		}
		return partialResult(http.StatusOK, content.String(), finishReason, usage, usageSeen, toolAcc, toolOrder), wrapStreamErr(fmt.Errorf("SSE stream: %w", err))
	}
	if !gotTerminal {
		return partialResult(http.StatusOK, content.String(), finishReason, usage, usageSeen, toolAcc, toolOrder),
			wrapStreamErr(fmt.Errorf("SSE stream ended without [DONE] marker — response truncated after ~%d bytes", bytesRead))
	}
	res := partialResult(http.StatusOK, content.String(), finishReason, usage, usageSeen, toolAcc, toolOrder)
	if usageSeen {
		res.Usage.Exact = true
	}
	return res, nil
}

// partialResult assembles the collected fields into a ChatResult. Usage is
// kept verbatim (Exact is set by the caller once the stream terminated).
func partialResult(status int, content, finishReason string, usage UsageStat, _ bool, toolAcc map[int]*ToolCall, toolOrder []int) ChatResult {
	res := ChatResult{
		Content:      content,
		FinishReason: finishReason,
		Usage:        usage,
		StatusCode:   status,
	}
	for _, i := range toolOrder {
		res.ToolCalls = append(res.ToolCalls, *toolAcc[i])
	}
	return res
}

// parseChatChunk decodes a single non-streaming JSON body. Non-streaming
// bodies carry choices[0].message (content + tool_calls), NOT delta — the
// SSE parser reads delta. Both shapes are handled so a JSON-answering
// server is never misread as empty.
func parseChatChunk(data []byte, status int) (ChatResult, error) {
	var body struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			Delta struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
				Reasoning        string `json:"reasoning"`
				ToolCalls        []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens            int64 `json:"prompt_tokens"`
			CompletionTokens        int64 `json:"completion_tokens"`
			CompletionTokensDetails *struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details,omitempty"`
			PromptTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details,omitempty"`
			CacheCreationInputTokens int64    `json:"cache_creation_input_tokens"`
			Cost                     *float64 `json:"cost,omitempty"`
			ServerToolUse            *struct {
				WebSearchRequests int64 `json:"web_search_requests"`
			} `json:"server_tool_use,omitempty"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
			Code    any    `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return ChatResult{StatusCode: status}, fmt.Errorf("parse response: %w", err)
	}
	if body.Error != nil {
		return ChatResult{StatusCode: status}, wrapStreamErr(fmt.Errorf("backend sent mid-stream error: %s (code %v)", body.Error.Message, body.Error.Code))
	}
	res := ChatResult{StatusCode: status}
	if len(body.Choices) > 0 {
		c := body.Choices[0]
		res.Content = c.Message.Content
		if res.Content == "" {
			res.Content = c.Delta.Content
		}
		res.FinishReason = c.FinishReason
		for _, tc := range c.Message.ToolCalls {
			res.ToolCalls = append(res.ToolCalls, ToolCall{ID: tc.ID, Type: tc.Type, Function: FunctionCall{Name: tc.Function.Name, Arguments: tc.Function.Arguments}})
		}
		for _, tc := range c.Delta.ToolCalls {
			res.ToolCalls = append(res.ToolCalls, ToolCall{ID: tc.ID, Type: tc.Type, Function: FunctionCall{Name: tc.Function.Name, Arguments: tc.Function.Arguments}})
		}
	}
	if body.Usage != nil {
		res.Usage = UsageStat{
			InputTok:        body.Usage.PromptTokens,
			OutputTok:       body.Usage.CompletionTokens,
			Exact:           true,
			ProviderCostUSD: 0,
		}
		if body.Usage.CompletionTokensDetails != nil {
			res.Usage.ReasoningTok = body.Usage.CompletionTokensDetails.ReasoningTokens
		}
		if body.Usage.PromptTokensDetails != nil {
			res.Usage.CachedTok = body.Usage.PromptTokensDetails.CachedTokens
		}
		res.Usage.CacheWriteTok = body.Usage.CacheCreationInputTokens
		if body.Usage.ServerToolUse != nil {
			res.Usage.WebSearchRequests = body.Usage.ServerToolUse.WebSearchRequests
		}
		if body.Usage.Cost != nil {
			res.Usage.ProviderCostUSD = *body.Usage.Cost
			res.Usage.HasProviderCost = true
		}
	}
	if body.Model != "" {
		res.Usage.ServedModel = body.Model
	}
	return res, nil
}

// sharedChatHTTPClient is the shared HTTP client for Exec calls made with a
// nil Client.HTTP (tests, hand-built clients). It mirrors counsel's former
// client: dial and TLS-handshake deadlines, no response-header timeout (a
// long reasoning call must not be cut before the first byte).
var sharedChatHTTPClient = func() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	tr.TLSHandshakeTimeout = 10 * time.Second
	return &http.Client{Transport: tr}
}()

// defaultChatHTTPClient returns the shared default client.
func defaultChatHTTPClient() *http.Client { return sharedChatHTTPClient }
