package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// ErrBackendStream marks a retryable transport failure: connection reset,
// truncated SSE body, timeout, or a 5xx response. Callers match with
// errors.Is to render a tidy ⚠ and to enter the retry loop in unattended runs.
// The underlying cause is preserved as text.
var ErrBackendStream = errors.New("backend stream error")

// ErrBackendFatal marks a non-retryable request error (4xx: bad request, auth
// failure, etc.). Retrying a malformed request fails identically every time.
var ErrBackendFatal = errors.New("backend fatal error")

// wrapStreamErr tags cause as a retryable backend stream error.
func wrapStreamErr(cause error) error {
	return fmt.Errorf("%w: %v", ErrBackendStream, cause)
}

// isFatalStatus classifies a non-200 status as fatal (never retry). 3xx
// redirects are fatal: the Go HTTP client follows redirects by default, so a
// 3xx reaching this function means either a non-GET redirect, a redirect loop,
// or max-redirects exceeded — retrying the same URL is pointless. 4xx are
// fatal except the transient trio: 429 (rate limited), 408 (request timeout),
// and 529 (site overloaded — Anthropic/OpenRouter convention). Everything not
// fatal is retryable.
func isFatalStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, http.StatusRequestTimeout, 529:
		return false
	}
	return code >= 300 && code < 500
}

// --- OpenAI-compatible wire types ---

// Message is one entry in the conversation sent to / received from the proxy.
// Content is *string so null round-trips faithfully: llama-server emits
// "content":null on tool-call turns; omitempty would elide it to no field,
// causing a chat-template rendering difference and KV-cache divergence.
type Message struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`

	// Images holds images attached to this message (vision input). They are
	// serialized at wire-encoding time as OpenAI-compatible image_url content
	// parts alongside the text content. This field is deliberately NOT
	// serialized directly (no json tag) — marshalWireMessages converts it to
	// the wire shape when building the request body. When nil/empty, the
	// message serializes exactly as before (string or null content), preserving
	// the golden no-op guarantee for text-only traffic.
	Images []ImagePart `json:"-"`

	// Pinned marks a message as exempt from compaction summarization and
	// hard-max dropping. Pinned messages are never folded into the lossy prose
	// summary that Compact produces, and never removed by enforceHardMax's
	// drop-oldest-turn loop. They still count toward TranscriptSize (so the
	// drop loop can detect an all-pinned-exceeds-max state and terminate).
	//
	// Used for the subagent's system prompt + task instruction (so the
	// subagent never forgets its own task mid-run) and for durable subagent
	// summary breadcrumbs in the parent's transcript (so the parent can
	// recover findings that were dissolved by its own compaction).
	//
	// This field is deliberately NOT serialized (no json tag) — it is a
	// local-only marker that must never leak into the proxy's request body,
	// because the proxy/memory layer has no concept of pinning and would
	// silently ignore it (or, worse, persist it as a stale marker).
	Pinned bool `json:"-"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Tool is an OpenAI function-tool advertised to the proxy.
type Tool struct {
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// wireMessage is the serialization-time wire shape for one message. It mirrors
// Message's JSON fields exactly, but allows content to be either a plain JSON
// string/null (unmarked, byte-identical to today) or an array of content parts
// (marked, for Anthropic cache_control). The input messages slice is never
// mutated — wireMessage values are built fresh per request from the Message
// slice. Pinned is excluded (no json tag, same as Message).
//
// When cacheControl is off, marshalWireMessages produces JSON byte-identical
// to json.Marshal of []Message — the golden no-op guarantee.
type wireMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []ToolCall      `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

// contentPart is one element of the parts-shaped content array used for
// cache_control decoration and image_url content blocks. Each part is either
// a text block (Type "text") or an image_url block (Type "image_url").
type contentPart struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text,omitempty"`
	CacheControl *cacheControlDirective `json:"cache_control,omitempty"`
	// ImageURL is set when Type == "image_url". The URL is a data URI
	// (data:image/png;base64,...) — the standard OpenAI Chat Completions
	// format for inline image input.
	ImageURL *imageURLPart `json:"image_url,omitempty"`
}

// imageURLPart is the inner object of an image_url content part.
type imageURLPart struct {
	URL string `json:"url"`
}

// cacheControlDirective is the Anthropic ephemeral cache breakpoint.
type cacheControlDirective struct {
	Type string `json:"type"`
}

// marshalWireMessages builds a []wireMessage from messages, decorating marked
// indices with cache_control content parts and appending image_url parts for
// messages with attached images. Unmarked messages without images get content
// as a plain JSON string (or null) — byte-identical to json.Marshal of
// []Message. The input slice is never mutated.
func marshalWireMessages(messages []Message, marked map[int]bool) ([]wireMessage, error) {
	out := make([]wireMessage, len(messages))
	for i, m := range messages {
		wm := wireMessage{
			Role:       m.Role,
			ToolCalls:  m.ToolCalls,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		hasImages := len(m.Images) > 0
		if hasImages {
			// Images present: always emit a parts-shaped content array with
			// text (if any) followed by image_url parts. When marked for
			// cache_control, the text part gets the ephemeral breakpoint.
			var parts []contentPart
			if m.Content != nil && *m.Content != "" {
				p := contentPart{
					Type:         "text",
					Text:         *m.Content,
					CacheControl: nil,
				}
				if marked[i] {
					p.CacheControl = &cacheControlDirective{Type: "ephemeral"}
				}
				parts = append(parts, p)
			}
			for _, img := range m.Images {
				parts = append(parts, contentPart{
					Type:     "image_url",
					ImageURL: &imageURLPart{URL: img.DataURL},
				})
			}
			b, err := json.Marshal(parts)
			if err != nil {
				return nil, err
			}
			wm.Content = b
		} else if marked[i] && m.Content != nil {
			// Marked message with non-null content: emit parts-shaped content
			// with an ephemeral cache_control breakpoint.
			part := contentPart{
				Type:         "text",
				Text:         *m.Content,
				CacheControl: &cacheControlDirective{Type: "ephemeral"},
			}
			b, err := json.Marshal([]contentPart{part})
			if err != nil {
				return nil, err
			}
			wm.Content = b
		} else {
			// Unmarked or null content: plain JSON string or null, matching
			// json.Marshal of *string exactly.
			b, err := json.Marshal(m.Content)
			if err != nil {
				return nil, err
			}
			wm.Content = b
		}
		out[i] = wm
	}
	return out, nil
}

// computeCacheBreakpoints returns the set of message indices to mark with
// cache_control. At most 2 breakpoints: messages[0] (static preamble) and
// the last message with non-null content (moving breakpoint). If both point
// to the same message, only one mark is set. Null-content messages are never
// marked. Returns nil (empty) when the slice is empty.
func computeCacheBreakpoints(messages []Message) map[int]bool {
	marked := map[int]bool{}
	if len(messages) == 0 {
		return marked
	}
	// Static: messages[0], but only if it has non-null content.
	if messages[0].Content != nil {
		marked[0] = true
	}
	// Moving: last message with non-null content, scanning backward.
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Content != nil {
			marked[i] = true // idempotent if already set (single-message case)
			break
		}
	}
	return marked
}

// anthropicCacheHeuristic decides whether to inject cache_control breakpoints
// for this request. Three states:
//
//   - flag explicitly *true  → on (user opted in)
//   - flag explicitly *false → off (user opted out — overrides the heuristic)
//   - flag nil (unset)       → heuristic: on when model looks like an Anthropic
//     model routed through OpenRouter ("anthropic/claude-*" or "claude-*").
//
// The heuristic makes caching work out-of-the-box for OpenRouter/Anthropic
// endpoints without requiring a config opt-in, while local llama.cpp/vLLM
// endpoints (model strings like "qwen3.6-35b") stay untouched — their requests
// are byte-identical to the pre-cache_control shape.
//
// Setting cache_control: false in the config is the explicit override that
// disables the heuristic for a model that would otherwise match.
func anthropicCacheHeuristic(flag *bool, model string) bool {
	if flag != nil {
		return *flag
	}
	lower := strings.ToLower(model)
	return strings.Contains(lower, "anthropic/claude") ||
		strings.HasPrefix(lower, "claude-")
}

// streamOptions asks the proxy to emit a trailing usage chunk (OpenAI-standard).
// Proxies that don't honour it simply omit usage, and Stream falls back to a
// length-based estimate marked approximate.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// streamChunk mirrors a single SSE chat.completion.chunk. Tolerant of the
// extra fields the proxy's two backends emit (function_call, refusal, timings…).
type streamChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"` // extended thinking (never stored in history)
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
	// Usage arrives on a trailing chunk (whose Choices is empty) when the proxy
	// honours stream_options.include_usage. Pointer so absence is distinguishable
	// from a zero-token report.
	Usage *struct {
		PromptTokens            int64 `json:"prompt_tokens"`
		CompletionTokens        int64 `json:"completion_tokens"`
		TotalTokens             int64 `json:"total_tokens"`
		CompletionTokensDetails *struct {
			ReasoningTokens int64 `json:"reasoning_tokens"`
		} `json:"completion_tokens_details"`
		// PromptTokensDetails carries cache-hit accounting (OpenAI-shaped
		// convention adopted by llama.cpp/vLLM/OpenRouter alike). Pointer so a
		// backend that never emits it (proxy, older servers) leaves CachedTok
		// at zero rather than a decode error.
		PromptTokensDetails *struct {
			CachedTokens int64 `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
		// CacheCreationInputTokens is Anthropic's cache-write token count,
		// surfaced by OpenRouter as a top-level usage field for Anthropic
		// models. Zero when absent (non-Anthropic models, older servers).
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	} `json:"usage"`
	// Error is OpenRouter's mid-stream error shape: a chunk carrying
	// {"error": {...}} instead of choices. Surfacing it beats letting the
	// stream end without [DONE] and reporting a generic truncation.
	Error *struct {
		Message string `json:"message"`
		Code    any    `json:"code"` // int or string depending on provider
	} `json:"error"`
}

// UsageStat is the token usage for the most recent Stream call. Exact is true
// when the proxy reported usage; false when counts were estimated from payload
// length (which downgrades the cost confidence from modeled to approx).
type UsageStat struct {
	InputTok     int64
	OutputTok    int64
	ReasoningTok int64
	// CachedTok is the subset of InputTok served from the backend's prompt
	// cache (prompt_tokens_details.cached_tokens). Zero when the backend
	// doesn't report it — never estimated, unlike InputTok/OutputTok's
	// length-based fallback.
	CachedTok int64
	// CacheWriteTok is the count of tokens written to the cache this turn
	// (cache_creation_input_tokens). Zero when absent — never estimated.
	CacheWriteTok int64
	Exact         bool
}

// GroundingEntry is one provenance record attached to a turn. Proxy-sourced
// entries arrive via X-Ilm-Grounding headers; client-sourced entries (web,
// oracle) are appended by executeToolCall on successful tool execution.
type GroundingEntry struct {
	Type  string // "corpus","zdb","learned","memory","web","oracle"
	Label string
	Score float64 // 0 if absent
}

// Endpoint kinds understood by the client. Mirrors config.EndpointKind* —
// duplicated here (two string constants) rather than importing config, which
// would invert the package dependency.
const (
	KindOpenAI   = "openai"
	KindIlmProxy = "ilm-proxy"
)

const (
	defaultAppReferer    = "https://github.com/treeol/wakil"
	defaultAppTitle      = "wakil"
	defaultAppCategories = "cli-agent"
)

// appAttributionHeaders resolves the OpenRouter attribution headers for this
// client. nil fields default to known values when the endpoint host is
// openrouter.ai (or a subdomain); non-nil fields are used verbatim, including
// empty string to opt out of the header entirely. For non-openrouter hosts
// with nil fields, no header is sent.
func (c *Client) appAttributionHeaders() (referer, title, categories string) {
	isOR := isOpenRouterHost(c.BaseURL)

	if c.AppReferer != nil {
		referer = *c.AppReferer
	} else if isOR {
		referer = defaultAppReferer
	}

	if c.AppTitle != nil {
		title = *c.AppTitle
	} else if isOR {
		title = defaultAppTitle
	}

	if c.AppCategories != nil {
		categories = *c.AppCategories
	} else if isOR {
		categories = defaultAppCategories
	}

	return referer, title, categories
}

// isOpenRouterHost reports whether the base URL's host is openrouter.ai or a
// subdomain of it.
func isOpenRouterHost(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" {
		return false
	}
	h := u.Hostname()
	return h == "openrouter.ai" || strings.HasSuffix(h, ".openrouter.ai")
}

// Client is a thin HTTP client of an OpenAI-compatible chat endpoint —
// either the remote ilm proxy (Kind "ilm-proxy") or a plain server
// (Kind "openai": llama.cpp server, OpenRouter, vLLM…).
type Client struct {
	BaseURL       string
	Model         string
	ChatID        string
	AuthHeader    string // full "Authorization" value, e.g. "Bearer sk-…"; empty = none
	NoMemoryWrite bool   // when true: tell the proxy not to write this traffic to memory/learn stores
	HTTP          *http.Client

	// Kind gates the proxy-specific request shape. "" is treated as
	// KindIlmProxy — every pre-endpoints construction site (tests, subagents,
	// hand-built clients) gets the exact historical behavior.
	// When KindOpenAI: no X-Ilm-* request headers, no metadata body field
	// (entirely absent, not empty — strict servers 400 on unknown fields),
	// and the model field is always ConfiguredModel.
	Kind string

	// ConfiguredModel is the endpoint's literal model string, sent as the
	// model field on every request when Kind is KindOpenAI — session model
	// overrides and the proxy's name/model prefix-routing trick do not apply
	// to plain endpoints. Ignored for KindIlmProxy.
	ConfiguredModel string

	// Sampling overrides from the endpoint config. nil = omit from the
	// request body (server defaults stay authoritative).
	Temperature *float64
	TopP        *float64
	MaxTokens   *int

	// CachePrompt mirrors EndpointConfig.CachePrompt: llama.cpp's non-standard
	// cache_prompt hint. nil = omit from the request body entirely (server
	// default / no opinion); set only for endpoints that explicitly opt in.
	CachePrompt *bool

	// CacheControl mirrors EndpointConfig.CacheControl: Anthropic-style
	// prompt-caching breakpoints injected on the wire copy at serialization
	// time. nil = no decoration (byte-identical to today); set only for
	// endpoints that explicitly opt in. The stored transcript is never
	// modified — breakpoints are computed per-request from the message slice.
	CacheControl *bool

	// AppReferer, AppTitle, and AppCategories are OpenRouter app attribution
	// headers sent as "HTTP-Referer", "X-Title" (plus its prefixed alias
	// "X-OpenRouter-Title"), and "X-OpenRouter-Categories" on chat completion
	// requests. nil = apply defaults for openrouter.ai hosts; non-nil = use
	// verbatim (empty string opts the header out). Only sent for KindOpenAI.
	AppReferer    *string
	AppTitle      *string
	AppCategories *string

	// Backend is the requested backend name sent as X-Ilm-Backend. Empty = don't
	// send the header (proxy uses its own default). Set by App.Send before each
	// Stream call — read at request build time, not captured at client creation.
	Backend string

	// AuxModel is sent as X-Ilm-Aux-Model to tell the proxy which model to use
	// for auxiliary (memory/compose) calls on this request. Empty = don't send
	// (proxy falls back to ILM_OR_AUX_MODEL env, or follows main when that is
	// also empty). Defaults to the effective main model so aux always tracks
	// main unless an explicit override is configured.
	AuxModel string

	// Retrieval telemetry for the most recent Stream call. groundingAttempted is
	// the X-Ilm-Retrieval sentinel ("attempted"); grounding accumulates both
	// proxy-sourced entries (from X-Ilm-Grounding headers) and client-sourced
	// entries (web/oracle) appended during tool execution.
	// Written by the agent goroutine, read by the TUI render loop → mutex-guarded.
	groundingMu        sync.Mutex
	grounding          []GroundingEntry
	groundingAttempted bool
	groundingMaxScore  float64

	// Token usage of the most recent Stream call. Written by the agent goroutine,
	// read by the cost tracker after each call → mutex-guarded.
	usageMu   sync.Mutex
	lastUsage UsageStat

	// lastUsedBackend stores the X-Ilm-Backend-Used response header from the most
	// recent Stream call. Written by the agent goroutine, read by the TUI render
	// loop → mutex-guarded.
	lastUsedBackendMu sync.Mutex
	lastUsedBackend   string

	// MaxRequestBytes is the pre-send byte-size guard. When > 0 and the
	// serialised request exceeds this limit, the largest tool-role messages are
	// stubbed to fit before sending. 0 = disabled.
	MaxRequestBytes int

	// malformedChunks counts SSE data chunks that failed to parse as JSON.
	// Each one aborts its stream as ErrBackendStream and is logged to stderr
	// with its byte offset (fail-fast — a lost chunk corrupts the accumulated
	// tool-call arguments). Atomic — written from the stream loop, readable
	// for diagnostics.
	malformedChunks int64
}

// LastUsage returns the token usage recorded by the most recent Stream call.
func (c *Client) LastUsage() UsageStat {
	c.usageMu.Lock()
	defer c.usageMu.Unlock()
	return c.lastUsage
}

func (c *Client) SetUsage(u UsageStat) {
	c.usageMu.Lock()
	defer c.usageMu.Unlock()
	c.lastUsage = u
}

// LastUsedBackend returns the X-Ilm-Backend-Used header value from the most
// recent Stream call. Empty when the proxy did not send the header.
func (c *Client) LastUsedBackend() string {
	c.lastUsedBackendMu.Lock()
	defer c.lastUsedBackendMu.Unlock()
	return c.lastUsedBackend
}

func (c *Client) SetLastUsedBackend(s string) {
	c.lastUsedBackendMu.Lock()
	defer c.lastUsedBackendMu.Unlock()
	c.lastUsedBackend = s
}

// Grounding returns the accumulated grounding entries for the current turn.
// A copy is returned so callers cannot mutate the internal slice.
func (c *Client) Grounding() []GroundingEntry {
	c.groundingMu.Lock()
	defer c.groundingMu.Unlock()
	return append([]GroundingEntry(nil), c.grounding...)
}

// GroundingState returns whether retrieval was attempted, the max proxy chunk
// score, and the accumulated grounding entries from the current turn.
// A copy of the entries slice is returned so callers cannot mutate internal state.
func (c *Client) GroundingState() (attempted bool, maxScore float64, entries []GroundingEntry) {
	c.groundingMu.Lock()
	defer c.groundingMu.Unlock()
	return c.groundingAttempted, c.groundingMaxScore, append([]GroundingEntry(nil), c.grounding...)
}

// isProxyGroundingType reports whether t is a proxy-sourced grounding type
// (corpus, zdb, learned, memory). Client-sourced types (web, oracle) return false.
func isProxyGroundingType(t string) bool {
	switch t {
	case "corpus", "zdb", "learned", "memory":
		return true
	}
	return false
}

// SetGrounding is called by Stream on each response. It replaces proxy-sourced
// entries (corpus/zdb/learned/memory) with the freshly parsed header entries,
// while preserving client-sourced entries (web/oracle) accumulated during the
// turn. Identical entries are deduped. maxScore is proxy-only (used by the
// learn-candidate gate in learn.go — do not include client entry scores here).
func (c *Client) SetGrounding(attempted bool, maxScore float64, entries []GroundingEntry) {
	c.groundingMu.Lock()
	defer c.groundingMu.Unlock()
	c.groundingAttempted = attempted
	c.groundingMaxScore = maxScore
	// Preserve client-sourced entries from the current turn.
	var kept []GroundingEntry
	seen := map[string]bool{}
	for _, e := range c.grounding {
		if !isProxyGroundingType(e.Type) {
			kept = append(kept, e)
			seen[e.Type+"\x00"+e.Label] = true
		}
	}
	// Append new proxy entries, deduping against each other and the kept set.
	for _, e := range entries {
		k := e.Type + "\x00" + e.Label
		if !seen[k] {
			kept = append(kept, e)
			seen[k] = true
		}
	}
	c.grounding = kept
}

// AddGrounding appends a client-sourced grounding entry (web, oracle). Identical
// entries (same type+label) are silently dropped. Safe to call concurrently.
//
// IMPORTANT: production code in internal/agent/ MUST go through
// App.addExternalGrounding() instead of calling this directly. The wrapper
// eagerly sets the sticky taint flag (a.touchedExternal) at exposure time.
// Calling AddGrounding directly bypasses the latch and causes taint to
// undercount — a trust-model violation (A1). A lint-style test
// (TestNoDirectAddGroundingInProductionCode) enforces this convention.
func (c *Client) AddGrounding(e GroundingEntry) {
	c.groundingMu.Lock()
	defer c.groundingMu.Unlock()
	key := e.Type + "\x00" + e.Label
	for _, ex := range c.grounding {
		if ex.Type+"\x00"+ex.Label == key {
			return
		}
	}
	c.grounding = append(c.grounding, e)
}

// ResetGrounding clears all grounding entries and resets telemetry flags. Called
// at the start of each user turn so stale entries never bleed across turns.
func (c *Client) ResetGrounding() {
	c.groundingMu.Lock()
	defer c.groundingMu.Unlock()
	c.grounding = nil
	c.groundingAttempted = false
	c.groundingMaxScore = 0
}

// parseGroundingHeader parses the X-Ilm-Grounding header.
// Format: "<type>|<label>[:<score>],…"  (type field is new; entries without "|"
// default to Type "corpus" for back-compat with old proxy versions).
// Returns the typed entries and the max numeric score present (0.0 if none).
func parseGroundingHeader(header string) ([]GroundingEntry, float64) {
	var entries []GroundingEntry
	var maxScore float64
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var e GroundingEntry
		// Split on first "|" to extract the type prefix.
		if i := strings.IndexByte(part, '|'); i >= 0 {
			e.Type = strings.TrimSpace(part[:i])
			part = strings.TrimSpace(part[i+1:])
		} else {
			e.Type = "corpus" // back-compat: untyped entries are corpus chunks
		}
		// Trailing ":score" on the remainder.
		e.Label = part
		if j := strings.LastIndex(part, ":"); j >= 0 {
			if s, err := strconv.ParseFloat(strings.TrimSpace(part[j+1:]), 64); err == nil {
				e.Label = strings.TrimSpace(part[:j])
				e.Score = s
				if s > maxScore {
					maxScore = s
				}
			}
		}
		entries = append(entries, e)
	}
	return entries, maxScore
}

// Sink receives streamed assistant content as it arrives (nil = discard).
type Sink func(string)

// Stream performs one chat-completions call and assembles the full assistant
// message from the SSE deltas. It handles BOTH response shapes the proxy uses:
// plain text (memory/learn/meta acks short-circuited server-side) and
// tool_calls (whose arguments arrive incrementally and are concatenated).
// The assistant turn may legitimately contain content AND tool_calls together.
//
// reasoningSink, when non-nil, receives reasoning_content (extended thinking)
// deltas. Reasoning is NEVER written into the returned Message — the stored
// assistant turn is always final-answer content only.
func (c *Client) Stream(ctx context.Context, messages []Message, tools []Tool, sink Sink, reasoningSink Sink) (Message, error) {
	// proxyShape gates every ilm-proxy-specific request element. Kind ""
	// means an old construction site that predates endpoint kinds — treat as
	// the proxy so existing behavior is untouched.
	proxyShape := c.Kind == "" || c.Kind == KindIlmProxy

	model := c.Model
	if !proxyShape && c.ConfiguredModel != "" {
		// Plain endpoints get the endpoint's literal model string. Session
		// state may hold the proxy alias ("ilm") or a backend-prefixed
		// "name/model" routing string — neither means anything to a plain
		// server, so the configured model always wins.
		model = c.ConfiguredModel
	}

	// Build the request body. Messages are encoded via wireMessage to allow
	// cache_control decoration when the endpoint opts in. When cache_control is
	// off, encoding is byte-identical to json.Marshal of []Message — the golden
	// no-op guarantee. The input messages slice is never mutated.
	//
	// cacheOn has two paths:
	//   1. Explicit: CacheControl is set to *true on the endpoint config.
	//   2. Heuristic: CacheControl is nil (unset) AND the model string looks
	//      like an Anthropic model routed through OpenRouter. This makes
	//      caching work out-of-the-box for "anthropic/claude-*" models without
	//      requiring a config opt-in, while local llama.cpp/vLLM endpoints
	//      (whose model strings are like "qwen3.6-35b") stay untouched.
	//      Setting CacheControl to *false explicitly disables the heuristic.
	cacheOn := anthropicCacheHeuristic(c.CacheControl, model)

	marked := map[int]bool{}
	if cacheOn {
		marked = computeCacheBreakpoints(messages)
	}

	wireMsgs, err := marshalWireMessages(messages, marked)
	if err != nil {
		return Message{}, err
	}

	type wireBody struct {
		Model         string            `json:"model"`
		Stream        bool              `json:"stream"`
		StreamOptions *streamOptions    `json:"stream_options,omitempty"`
		Messages      []wireMessage     `json:"messages"`
		Tools         []Tool            `json:"tools,omitempty"`
		Metadata      map[string]string `json:"metadata,omitempty"`
		Temperature   *float64          `json:"temperature,omitempty"`
		TopP          *float64          `json:"top_p,omitempty"`
		MaxTokens     *int              `json:"max_tokens,omitempty"`
		CachePrompt   *bool             `json:"cache_prompt,omitempty"`
	}

	// MaxTokens: use the configured value if set; otherwise apply a default
	// for OpenAI-compatible endpoints so the server doesn't apply its own
	// (often too-low) default. This is critical for reasoning models (e.g.
	// kimi-k3, or Claude via OpenRouter with extended thinking) where
	// reasoning tokens count against the budget — without enough max_tokens,
	// the model exhausts the budget on thinking and produces no content.
	// 32768 leaves headroom for both a local reasoning model's thinking pass
	// and a hosted model's extended-thinking + response; endpoints that need
	// more (or less, e.g. a small-context local server) should set max_tokens
	// explicitly. The ilm-proxy path manages its own limits and does not need
	// this default.
	maxTokens := c.MaxTokens
	if maxTokens == nil && c.Kind == KindOpenAI {
		defaultMax := 32768
		maxTokens = &defaultMax
	}

	body := wireBody{
		Model:         model,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
		Messages:      wireMsgs,
		Tools:         tools,
		Temperature:   c.Temperature,
		TopP:          c.TopP,
		MaxTokens:     maxTokens,
		CachePrompt:   c.CachePrompt,
	}
	if proxyShape {
		metadata := map[string]string{}
		if c.ChatID != "" && !c.NoMemoryWrite {
			// Subagent clients (NoMemoryWrite=true) never send chat_id — they stay
			// outside the session's pending/confirmation mechanics (defence in depth
			// on top of NoMemoryWrite).
			metadata["chat_id"] = c.ChatID
		}
		if c.NoMemoryWrite {
			metadata["ilm-no-memory-write"] = "true"
		}
		if len(metadata) > 0 {
			body.Metadata = metadata
		}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return Message{}, err
	}

	// Pre-send byte guard: trim the largest tool results to fit within MaxRequestBytes
	// rather than letting an oversized request reach the proxy and get a 400.
	if c.MaxRequestBytes > 0 && len(raw) > c.MaxRequestBytes {
		trimmed := trimToolResults(messages, len(raw), c.MaxRequestBytes)
		if trimmed == nil {
			return Message{}, fmt.Errorf("%w: request %d B exceeds byte limit %d B and no large tool results to trim",
				ErrBackendFatal, len(raw), c.MaxRequestBytes)
		}
		// Recompute breakpoints on the trimmed slice (positions may have
		// changed — trim replaces content, not indices, but correctness
		// demands we recompute from the actual data we'll send).
		marked = map[int]bool{}
		if cacheOn {
			marked = computeCacheBreakpoints(trimmed)
		}
		wireMsgs, err = marshalWireMessages(trimmed, marked)
		if err != nil {
			return Message{}, err
		}
		body.Messages = wireMsgs
		raw, err = json.Marshal(body)
		if err != nil {
			return Message{}, err
		}
		if len(raw) > c.MaxRequestBytes {
			return Message{}, fmt.Errorf("%w: request %d B still exceeds byte limit %d B after trimming tool results",
				ErrBackendFatal, len(raw), c.MaxRequestBytes)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/v1/chat/completions", bytes.NewReader(raw))
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.AuthHeader != "" {
		req.Header.Set("Authorization", c.AuthHeader)
	}
	if proxyShape {
		if c.NoMemoryWrite {
			req.Header.Set("X-Ilm-No-Memory-Write", "true")
		}
		if c.Backend != "" {
			req.Header.Set("X-Ilm-Backend", c.Backend)
		}
		if c.AuxModel != "" {
			req.Header.Set("X-Ilm-Aux-Model", c.AuxModel)
		}
	}

	// OpenRouter app attribution headers (openai-kind endpoints only).
	// nil = apply defaults for openrouter.ai hosts; non-nil = verbatim
	// (empty string opts the header out entirely).
	if !proxyShape {
		referer, title, categories := c.appAttributionHeaders()
		if referer != "" {
			req.Header.Set("HTTP-Referer", referer)
		}
		if title != "" {
			req.Header.Set("X-Title", title)
			// Newer prefixed form of the display-name header; carries the
			// same resolved value as X-Title so both conventions attribute
			// the app identically.
			req.Header.Set("X-OpenRouter-Title", title)
		}
		if categories != "" {
			req.Header.Set("X-OpenRouter-Categories", categories)
		}
	}

	// Provisional usage: the serialised request payload is a faithful proxy for
	// prompt occupancy and is known before the backend answers. Publishing an
	// estimate here lets the TUI's ctx meter move at the *start* of every inner
	// stream — mid-turn, as tool results grow the transcript — instead of only
	// when the trailing usage chunk lands. Exact=false marks it an estimate;
	// the authoritative figure overwrites it at stream end (SetUsage below).
	// Output fields are zeroed: this call's output is genuinely unknown yet.
	//
	// WP-7.10e: the tools schema is a fixed overhead that doesn't grow per-turn.
	// We estimate its size separately and subtract it from the prompt token count
	// so the ctx meter reflects the conversation size, not the tool definitions.
	promptTok := ApproxTokens(len(raw))
	if toolsBytes := estimateToolsBytes(tools); toolsBytes > 0 {
		promptTok -= ApproxTokens(toolsBytes)
	}
	c.SetUsage(UsageStat{InputTok: promptTok})

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// A cancelled context surfaces from Do as a wrapped ctx.Err() — the
		// user aborted before any response arrived. Never wrap it retryable:
		// the retry loop would re-issue a turn the user deliberately killed.
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return Message{}, ctx.Err()
		}
		// All other pre-response transport errors (timeout, reset, connection
		// refused) are retryable — wrap uniformly so the retry loop catches them.
		return Message{}, wrapStreamErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(body))
		if isFatalStatus(resp.StatusCode) {
			// Non-retryable request error — retrying will fail identically.
			return Message{}, fmt.Errorf("%w: %s: %s", ErrBackendFatal, resp.Status, msg)
		}
		// Transient failure (5xx, 429 rate limit, 408 timeout, 529 overloaded,
		// other non-success): retryable.
		return Message{}, fmt.Errorf("%w: %s: %s", ErrBackendStream, resp.Status, msg)
	}

	// Retrieval telemetry. X-Ilm-Retrieval=="attempted" is the sentinel that the
	// retriever ran; X-Ilm-Grounding carries the per-chunk titles and scores.
	attempted := resp.Header.Get("X-Ilm-Retrieval") == "attempted"
	entries, maxScore := parseGroundingHeader(resp.Header.Get("X-Ilm-Grounding"))
	c.SetGrounding(attempted, maxScore, entries)

	// Record which backend actually handled this request. Present on both
	// streaming and non-streaming responses; empty when the proxy doesn't send it.
	c.SetLastUsedBackend(strings.TrimSpace(resp.Header.Get("X-Ilm-Backend-Used")))

	var content strings.Builder
	acc := map[int]*ToolCall{}
	var order []int

	// Usage accumulation. usageSeen flips when the proxy emits a usage chunk;
	// reasoningChars feeds the length-based fallback when it does not.
	var usageSeen bool
	var usage UsageStat
	var reasoningChars int

	// Track finish_reason from the last chunk that carries it. Reasoning models
	// (e.g. kimi-k3) that exhaust their token budget on thinking send
	// finish_reason="length" with empty content — without this check, the model
	// appears to produce an empty response and the error is misdiagnosed.
	var finishReason string

	// sawDone records whether the stream's terminal [DONE] marker arrived.
	// Without it, a connection that closes cleanly mid-stream (EOF without a
	// transport error) is indistinguishable from a completed stream — the
	// truncated partial message would be returned as success.
	var sawDone bool

	// Bounded SSE line reader: a malformed/malicious stream sending a line
	// larger than maxSSELineSize is rejected with an error instead of
	// causing unbounded memory growth.
	const maxSSELineSize = 10 * 1024 * 1024 // 10 MB
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELineSize)
	var bytesRead int // approximate SSE bytes consumed, for error offsets
	for scanner.Scan() {
		s := strings.TrimRight(scanner.Text(), "\r")
		bytesRead += len(s) + 1 // +1 for the consumed newline
		if !strings.HasPrefix(s, "data:") {
			continue
		}
		data := strings.TrimSpace(s[len("data:"):])
		if data == "[DONE]" {
			sawDone = true
			break
		}
		if data == "" {
			// Empty data: frames are keepalives (some proxies send them to
			// hold the connection open during long generations). They carry
			// no payload — skip instead of failing JSON parsing.
			continue
		}
		var chunk streamChunk
		if jerr := json.Unmarshal([]byte(data), &chunk); jerr != nil {
			// A malformed data chunk means the accumulated message is
			// incomplete: tool-call arguments arrive fragmented across chunks
			// and are concatenated — a lost fragment leaves a hole in the JSON
			// (or loses the tool name, which travels only in the first chunk
			// per index). Abort as retryable instead of returning corrupted
			// tool calls as success. Safe to fail unconditionally: only data:
			// lines reach this point (comments/keepalives/event lines are
			// skipped above) and [DONE] is handled before the parse.
			atomic.AddInt64(&c.malformedChunks, 1)
			fmt.Fprintf(os.Stderr, "proxy: malformed SSE chunk at byte %d: %v — aborting stream\n", bytesRead, jerr)
			return Message{}, wrapStreamErr(fmt.Errorf("malformed SSE chunk at byte %d: %v", bytesRead, jerr))
		}
		// OpenRouter-style mid-stream error chunk: {"error": {...}}. Surface
		// its message instead of letting the stream fizzle into a generic
		// "ended without [DONE]" truncation error. Retryable: these are
		// typically provider-rate-limit or upstream failures.
		if chunk.Error != nil {
			return Message{}, wrapStreamErr(fmt.Errorf("backend sent mid-stream error: %s (code %v)", chunk.Error.Message, chunk.Error.Code))
		}
		// Usage may arrive on a trailing chunk whose Choices is empty, so
		// it is read independently of the delta below.
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
		}
		if len(chunk.Choices) > 0 {
			choice := chunk.Choices[0]
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}
			d := choice.Delta
			if d.ReasoningContent != "" {
				reasoningChars += len(d.ReasoningContent)
				if reasoningSink != nil {
					reasoningSink(d.ReasoningContent)
					// Reasoning is intentionally NOT written to content — it
					// must never appear in the Message or the Conv history.
				}
			}
			if d.Content != "" {
				content.WriteString(d.Content)
				if sink != nil {
					sink(d.Content)
				}
			}
			for _, tc := range d.ToolCalls {
				e, ok := acc[tc.Index]
				if !ok {
					e = &ToolCall{Type: "function"}
					acc[tc.Index] = e
					order = append(order, tc.Index)
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
	}
	if err := scanner.Err(); err != nil {
		// Cancellation can surface here as the read error itself (real
		// transports wrap context.Canceled; custom bodies return their own
		// teardown error). Classify via errors.Is against the context's own
		// error — accurate cause, no timing guess. A cancel that arrives
		// AFTER a genuine stream error leaves err unrelated to ctx.Err() and
		// the real error wins below (no masking).
		if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
			return Message{}, ctx.Err()
		}
		// The scanner error is NOT the context's error. Two cases:
		//  (a) a custom/non-standard body returned an opaque teardown error
		//      on cancel ("read on closed response body") — indistinguishable
		//      here from a genuine mid-stream read failure. Since the context
		//      IS done, cancellation is the more useful classification, and
		//      the cost of being wrong is nil: a stream whose connection
		//      died anyway has nothing accurate left to report.
		//  (b) a REAL stream error (oversize line, transport failure) with a
		//      live context — reported accurately.
		if ctx.Err() != nil {
			return Message{}, ctx.Err()
		}
		// bufio.ErrTooLong means a line exceeded maxSSELineSize — abort
		// the stream rather than allowing unbounded memory growth.
		return Message{}, wrapStreamErr(fmt.Errorf("SSE stream: %w", err))
	}

	// The loop exited without a scanner error. If the context was cancelled
	// (or hit its deadline), THAT is the cause — classify it as cancellation,
	// never as a retryable ErrBackendStream, so a deliberate user cancel is
	// never auto-retried. The cancel may surface as a clean EOF (!sawDone)
	// or at a stream boundary, both of which would otherwise misreport as
	// backend truncation.
	if ctx.Err() != nil {
		return Message{}, ctx.Err()
	}

	// A stream that ends without [DONE] was truncated: the connection closed
	// cleanly mid-generation (no transport error), so the loop above exited
	// normally with a partial message. Returning it as success would hand
	// corrupted tool-call arguments (or truncated content) to the caller —
	// fail as retryable instead. All OpenAI-compatible servers terminate
	// streams with data: [DONE].
	if !sawDone {
		return Message{}, wrapStreamErr(fmt.Errorf("SSE stream ended without [DONE] marker — response truncated after ~%d bytes", bytesRead))
	}

	// Check finish_reason: if the model hit the token limit ("length") and
	// produced no content (typical for reasoning models that exhaust the budget
	// on thinking), surface a clear, actionable error instead of returning an
	// empty message that triggers a generic "empty response" path downstream.
	// NOT retryable: resending the same request hits the same budget.
	if finishReason == "length" && content.Len() == 0 && len(order) == 0 {
		return Message{}, fmt.Errorf("%w: model hit token limit (finish_reason=length) — increase max_tokens for this endpoint in config.json (endpoints.<name>.max_tokens); /model only changes the model name, not the token budget",
			ErrBackendFatal)
	}

	// Resolve usage: exact when the proxy reported a usage chunk, otherwise a
	// ~4-chars/token estimate over the request payload and streamed output,
	// marked inexact so the cost tracker downgrades it from modeled to approx.
	if usageSeen {
		usage.Exact = true
	} else {
		usage = UsageStat{
			InputTok:  ApproxTokens(len(raw)),
			OutputTok: ApproxTokens(content.Len() + reasoningChars),
		}
	}
	c.SetUsage(usage)

	msg := Message{Role: "assistant"}
	if content.Len() > 0 {
		s := content.String()
		msg.Content = &s // non-nil only when text was actually streamed
	}
	// Content stays nil (→ "content":null) when the turn is tool-calls only,
	// matching the null llama-server emitted and preserving the KV-cache prefix.
	for _, i := range order {
		msg.ToolCalls = append(msg.ToolCalls, *acc[i])
	}

	// Token-budget exhaustion while tool calls were streaming: if any
	// accumulated arguments are incomplete, the cause is the budget — surface
	// the actionable fatal message (retrying identically hits the same limit),
	// not the generic integrity error. Complete-valid calls fall through to
	// the normal integrity check and post-check handling.
	if finishReason == "length" {
		for i := range msg.ToolCalls {
			tc := &msg.ToolCalls[i]
			if !argsAreValidObject(tc.Function.Arguments) {
				return Message{}, fmt.Errorf("%w: model hit token limit (finish_reason=length) while streaming tool call %q — arguments are truncated; increase max_tokens for this endpoint in config.json (endpoints.<name>.max_tokens)",
					ErrBackendFatal, tc.Function.Name)
			}
		}
	}

	// Final integrity check: defense in depth against any corruption path the
	// guards above missed (e.g. a provider that omits [DONE] after a complete
	// message, or backend-side generation glitches). Every tool call must have
	// a name (it travels only in the first chunk per index — its loss means a
	// lost chunk) and object-shaped JSON arguments (empty string is tolerated:
	// some models emit it for zero-argument tools). Note the explicit opening
	// brace: json.Unmarshal of "null" into a map SUCCEEDS (nil map), so a
	// bare Unmarshal check would wave through a truncated-to-"null" payload.
	// Tool handlers unmarshal arguments downstream, so anything invalid here
	// would surface there as a confusing per-tool error; failing the stream
	// lets the retry machinery resend the whole turn instead.
	for i := range msg.ToolCalls {
		tc := &msg.ToolCalls[i]
		if tc.Function.Name == "" {
			return Message{}, wrapStreamErr(fmt.Errorf("tool call %d arrived without a function name (lost stream chunk?)", i))
		}
		if !argsAreValidObject(tc.Function.Arguments) {
			return Message{}, wrapStreamErr(fmt.Errorf("tool call %q has invalid or non-object JSON arguments", tc.Function.Name))
		}
	}

	// Budget exhaustion AFTER complete-valid tool calls (the model finished
	// the call, then died on trailing content): the turn is still incomplete.
	if finishReason == "length" && len(msg.ToolCalls) > 0 {
		return Message{}, fmt.Errorf("%w: model hit token limit (finish_reason=length) after streaming tool call %q — the turn is incomplete; increase max_tokens for this endpoint in config.json (endpoints.<name>.max_tokens)",
			ErrBackendFatal, msg.ToolCalls[0].Function.Name)
	}
	return msg, nil
}

// argsAreValidObject reports whether s is empty (tolerated: some models emit
// it for zero-argument tools) or a valid JSON object.
func argsAreValidObject(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return true
	}
	if !strings.HasPrefix(s, "{") {
		return false
	}
	var obj map[string]json.RawMessage
	return json.Unmarshal([]byte(s), &obj) == nil
}

// trimToolResults replaces the content of the largest tool-role messages with
// stub strings until the estimated request size fits within maxBytes. Returns
// the modified message slice, or nil if there are no large tool results to trim.
//
// The size estimate is heuristic (subtracts content bytes, adds stub bytes)
// rather than re-marshalling each iteration; the caller re-marshals once after
// to verify the result fits.
func trimToolResults(msgs []Message, currentSize, maxBytes int) []Message {
	type entry struct{ idx, size int }
	var large []entry
	for i, m := range msgs {
		if m.Role == "tool" && m.Content != nil && len(*m.Content) > 200 {
			large = append(large, entry{i, len(*m.Content)})
		}
	}
	if len(large) == 0 {
		return nil
	}
	sort.Slice(large, func(a, b int) bool { return large[a].size > large[b].size })

	out := make([]Message, len(msgs))
	copy(out, msgs)
	cur := currentSize
	for _, e := range large {
		if cur <= maxBytes {
			break
		}
		// Preserve any embedded spill path (from CapToolResult, StubToolResult,
		// or SpillFullResult) so the model can recover the full content after
		// trimming. The marker format is "... at: PATH]" — extract it before
		// replacing the content.
		var stub string
		if path := extractSpillPath(*out[e.idx].Content); path != "" {
			stub = fmt.Sprintf("[pre-send trim — %d bytes — full content at: %s]", e.size, path)
		} else {
			stub = fmt.Sprintf("[pre-send trim — %d bytes — exceeded request byte limit; retrieve with read_file if needed]", e.size)
		}
		s := stub
		out[e.idx].Content = &s
		cur -= e.size - len(stub)
	}
	return out
}

// estimateToolsBytes returns a rough byte size of the tools schema when
// serialised to JSON, so it can be subtracted from the provisional prompt
// token estimate (WP-7.10e: tools schema is fixed overhead, not prompt).
func estimateToolsBytes(tools []Tool) int {
	if len(tools) == 0 {
		return 0
	}
	// Marshal once to get the exact size; this is cheap relative to the
	// network call that follows.
	b, err := json.Marshal(struct {
		Tools []Tool `json:"tools,omitempty"`
	}{Tools: tools})
	if err != nil {
		return 0
	}
	return len(b)
}

// extractSpillPath returns the disk path embedded in a tool result's trailing
// "… at: PATH]" note, or "". This mirrors tools.ExtractSpillPath — duplicated
// here to avoid a circular import (tools imports proxy). Matches only when a
// known marker prefix sits inside the final bracketed segment of the string
// to prevent false positives from file content.
func extractSpillPath(content string) string {
	trimmed := strings.TrimRight(content, " \t\r\n")
	if !strings.HasSuffix(trimmed, "]") {
		return ""
	}
	closeIdx := len(trimmed) - 1
	openIdx := strings.LastIndex(trimmed[:closeIdx], "[")
	if openIdx < 0 {
		return ""
	}
	segment := trimmed[openIdx+1 : closeIdx]

	knownPrefixes := []string{
		"full content at: ",
		"budget — ",
		"+",
		"evicted — ",
		"pre-send trim — ",
		"subagent summary at: ",
	}
	matched := false
	for _, p := range knownPrefixes {
		if strings.HasPrefix(segment, p) {
			matched = true
			break
		}
	}
	if !matched {
		return ""
	}

	atIdx := strings.LastIndex(segment, " at: ")
	if atIdx < 0 {
		return ""
	}
	path := segment[atIdx+len(" at: "):]
	if path == "" {
		return ""
	}
	return path
}
