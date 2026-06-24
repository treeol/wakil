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
	"sort"
	"strconv"
	"strings"
	"sync"
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

type chatRequest struct {
	Model         string            `json:"model"`
	Stream        bool              `json:"stream"`
	StreamOptions *streamOptions    `json:"stream_options,omitempty"`
	Messages      []Message         `json:"messages"`
	Tools         []Tool            `json:"tools,omitempty"`
	Metadata      map[string]string `json:"metadata,omitempty"`
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
	} `json:"usage"`
}

// UsageStat is the token usage for the most recent Stream call. Exact is true
// when the proxy reported usage; false when counts were estimated from payload
// length (which downgrades the cost confidence from modeled to approx).
type UsageStat struct {
	InputTok     int64
	OutputTok    int64
	ReasoningTok int64
	Exact        bool
}

// GroundingEntry is one provenance record attached to a turn. Proxy-sourced
// entries arrive via X-Ilm-Grounding headers; client-sourced entries (web,
// oracle) are appended by executeToolCall on successful tool execution.
type GroundingEntry struct {
	Type  string // "corpus","zdb","learned","memory","web","oracle"
	Label string
	Score float64 // 0 if absent
}

// Client is a thin HTTP client of the remote ilm proxy.
type Client struct {
	BaseURL       string
	Model         string
	ChatID        string
	AuthHeader    string // full "Authorization" value, e.g. "Bearer sk-…"; empty = none
	NoMemoryWrite bool   // when true: tell the proxy not to write this traffic to memory/learn stores
	HTTP          *http.Client

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
func (c *Client) Grounding() []GroundingEntry {
	c.groundingMu.Lock()
	defer c.groundingMu.Unlock()
	return c.grounding
}

// GroundingState returns whether retrieval was attempted, the max proxy chunk
// score, and the accumulated grounding entries from the current turn.
func (c *Client) GroundingState() (attempted bool, maxScore float64, entries []GroundingEntry) {
	c.groundingMu.Lock()
	defer c.groundingMu.Unlock()
	return c.groundingAttempted, c.groundingMaxScore, c.grounding
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
	reqBody := chatRequest{
		Model:         c.Model,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
		Messages:      messages,
		Tools:         tools,
	}
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
		reqBody.Metadata = metadata
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return Message{}, err
	}

	// Pre-send byte guard: trim the largest tool results to fit within MaxRequestBytes
	// rather than letting an oversized request reach the proxy and get a 400.
	if c.MaxRequestBytes > 0 && len(raw) > c.MaxRequestBytes {
		trimmed := trimToolResults(reqBody.Messages, len(raw), c.MaxRequestBytes)
		if trimmed == nil {
			return Message{}, fmt.Errorf("%w: request %d B exceeds byte limit %d B and no large tool results to trim",
				ErrBackendFatal, len(raw), c.MaxRequestBytes)
		}
		reqBody.Messages = trimmed
		raw, err = json.Marshal(reqBody)
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
	if c.NoMemoryWrite {
		req.Header.Set("X-Ilm-No-Memory-Write", "true")
	}
	if c.Backend != "" {
		req.Header.Set("X-Ilm-Backend", c.Backend)
	}
	if c.AuxModel != "" {
		req.Header.Set("X-Ilm-Aux-Model", c.AuxModel)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		// All pre-response transport errors (timeout, reset, connection refused)
		// are retryable — wrap uniformly so the retry loop can catch them.
		return Message{}, wrapStreamErr(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		msg := strings.TrimSpace(string(body))
		if resp.StatusCode >= 400 && resp.StatusCode < 500 {
			// 4xx: request error — retrying the same request will fail identically.
			return Message{}, fmt.Errorf("%w: %s: %s", ErrBackendFatal, resp.Status, msg)
		}
		// 5xx and other non-success: transient backend failure, retryable.
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

	reader := bufio.NewReader(resp.Body)
	for {
		line, rerr := reader.ReadString('\n')
		if s := strings.TrimRight(line, "\r\n"); strings.HasPrefix(s, "data:") {
			data := strings.TrimSpace(s[len("data:"):])
			if data == "[DONE]" {
				break
			}
			var chunk streamChunk
			if json.Unmarshal([]byte(data), &chunk) == nil {
				// Usage may arrive on a trailing chunk whose Choices is empty, so
				// it is read independently of the delta below.
				if chunk.Usage != nil {
					usageSeen = true
					usage.InputTok = chunk.Usage.PromptTokens
					usage.OutputTok = chunk.Usage.CompletionTokens
					if d := chunk.Usage.CompletionTokensDetails; d != nil {
						usage.ReasoningTok = d.ReasoningTokens
					}
				}
				if len(chunk.Choices) > 0 {
					d := chunk.Choices[0].Delta
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
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			// Any non-EOF read failure mid-stream is a backend stream error
			// (truncated SSE, reset, transport drop).
			return Message{}, wrapStreamErr(rerr)
		}
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
	return msg, nil
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
