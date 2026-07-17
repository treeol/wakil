package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- Golden off: flag unset → byte-identical to current shape ---

// TestCacheControlUnsetByteIdentical verifies that when CacheControl is nil,
// the request body is byte-identical to the pre-change shape: content is a
// plain JSON string, never an array of parts.
func TestCacheControlUnsetByteIdentical(t *testing.T) {
	srv, _, body := captureServer(t)
	c := &Client{BaseURL: srv.URL, Kind: KindOpenAI, ConfiguredModel: "m", Model: "m", HTTP: http.DefaultClient}
	msgs := []Message{
		{Role: "system", Content: strPtr("system prompt")},
		{Role: "user", Content: strPtr("hello")},
		{Role: "assistant", Content: nil, ToolCalls: []ToolCall{{ID: "tc1", Type: "function", Function: FunctionCall{Name: "foo", Arguments: "{}"}}}},
		{Role: "tool", ToolCallID: "tc1", Name: "foo", Content: strPtr("result")},
	}
	if _, err := c.Stream(t.Context(), msgs, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var raw struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(*body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(raw.Messages) != len(msgs) {
		t.Fatalf("messages: got %d, want %d", len(raw.Messages), len(msgs))
	}
	// Every message's content must be a plain string or null, never an array.
	for i, m := range raw.Messages {
		contentStr := string(m.Content)
		if len(contentStr) > 0 && contentStr[0] == '[' {
			t.Errorf("message %d (role %q) content is array (parts-shaped) — must be plain string/null when CacheControl is off: %s",
				i, m.Role, contentStr)
		}
	}
	// The null-content assistant turn must round-trip as "content":null.
	if string(raw.Messages[2].Content) != "null" {
		t.Errorf("null content: got %s, want null", string(raw.Messages[2].Content))
	}
}

// TestCacheControlUnsetExactGolden compares the full body bytes against a
// json.Marshal of the equivalent chatRequest-shaped value (the pre-change
// encoding), proving byte-identity.
func TestCacheControlUnsetExactGolden(t *testing.T) {
	srv, _, body := captureServer(t)
	c := &Client{BaseURL: srv.URL, Kind: KindOpenAI, ConfiguredModel: "m", Model: "m", HTTP: http.DefaultClient}
	msgs := []Message{
		{Role: "system", Content: strPtr("preamble")},
		{Role: "user", Content: strPtr("query")},
	}
	if _, err := c.Stream(t.Context(), msgs, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Build the expected body with the same shape the old chatRequest would
	// have produced (now via wireMessage with no marks). max_tokens gets a
	// default of 8192 for KindOpenAI when unset (reasoning-model fix).
	wireMsgs, _ := marshalWireMessages(msgs, nil)
	defaultMax := 8192
	expected, _ := json.Marshal(struct {
		Model         string         `json:"model"`
		Stream        bool           `json:"stream"`
		StreamOptions *streamOptions `json:"stream_options,omitempty"`
		Messages      []wireMessage  `json:"messages"`
		MaxTokens     *int           `json:"max_tokens,omitempty"`
	}{
		Model: "m", Stream: true, StreamOptions: &streamOptions{IncludeUsage: true},
		Messages: wireMsgs, MaxTokens: &defaultMax,
	})

	if string(*body) != string(expected) {
		t.Errorf("body differs from golden:\n got: %s\n want: %s", *body, expected)
	}
}

// --- Marking: flag on ---

// TestCacheControlMarksFirstAndLast verifies that with CacheControl on,
// messages[0] and the last non-null-content message get parts-shaped content
// with cache_control, and all other messages stay plain strings.
func TestCacheControlMarksFirstAndLast(t *testing.T) {
	srv, _, body := captureServer(t)
	c := &Client{BaseURL: srv.URL, Kind: KindOpenAI, ConfiguredModel: "m", Model: "m",
		CacheControl: boolPtr(true), HTTP: http.DefaultClient}
	msgs := []Message{
		{Role: "system", Content: strPtr("preamble")},
		{Role: "user", Content: strPtr("turn 1")},
		{Role: "assistant", Content: strPtr("response 1")},
		{Role: "user", Content: strPtr("turn 2")},
	}
	if _, err := c.Stream(t.Context(), msgs, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var raw struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(*body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// messages[0] (system): parts-shaped with cache_control
	assertPartsShaped(t, raw.Messages[0].Content, "preamble", "messages[0]")

	// messages[1], [2]: plain strings
	assertPlainString(t, raw.Messages[1].Content, "turn 1", "messages[1]")
	assertPlainString(t, raw.Messages[2].Content, "response 1", "messages[2]")

	// messages[3] (last non-null): parts-shaped with cache_control
	assertPartsShaped(t, raw.Messages[3].Content, "turn 2", "messages[3] (last)")
}

// TestCacheControlNullContentLastMarksPredecessor: when the last message has
// null content (tool-call-only turn), the predecessor is marked instead.
func TestCacheControlNullContentLastMarksPredecessor(t *testing.T) {
	srv, _, body := captureServer(t)
	c := &Client{BaseURL: srv.URL, Kind: KindOpenAI, ConfiguredModel: "m", Model: "m",
		CacheControl: boolPtr(true), HTTP: http.DefaultClient}
	msgs := []Message{
		{Role: "system", Content: strPtr("preamble")},
		{Role: "user", Content: strPtr("query")},
		{Role: "assistant", Content: nil, ToolCalls: []ToolCall{{ID: "tc1", Type: "function", Function: FunctionCall{Name: "foo", Arguments: "{}"}}}},
	}
	if _, err := c.Stream(t.Context(), msgs, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var raw struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(*body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// messages[0]: marked
	assertPartsShaped(t, raw.Messages[0].Content, "preamble", "messages[0]")
	// messages[1] (predecessor of null-content last): marked
	assertPartsShaped(t, raw.Messages[1].Content, "query", "messages[1] (predecessor)")
	// messages[2]: null content, not marked
	if string(raw.Messages[2].Content) != "null" {
		t.Errorf("messages[2] content: got %s, want null", string(raw.Messages[2].Content))
	}
}

// TestCacheControlSingleMessageOneMark: a single-message conversation gets
// exactly one mark — never two on the same message.
func TestCacheControlSingleMessageOneMark(t *testing.T) {
	srv, _, body := captureServer(t)
	c := &Client{BaseURL: srv.URL, Kind: KindOpenAI, ConfiguredModel: "m", Model: "m",
		CacheControl: boolPtr(true), HTTP: http.DefaultClient}
	msgs := []Message{
		{Role: "user", Content: strPtr("only message")},
	}
	if _, err := c.Stream(t.Context(), msgs, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var raw struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(*body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(raw.Messages) != 1 {
		t.Fatalf("messages: got %d, want 1", len(raw.Messages))
	}
	// Single message: both static and moving point to index 0 — one mark.
	assertPartsShaped(t, raw.Messages[0].Content, "only message", "single message")
}

// --- No-mutation: a.Conv unchanged after Stream ---

// TestStreamDoesNotMutateInputMessages verifies that after Stream returns, the
// input messages slice is unchanged: same Content pointers, same values.
func TestStreamDoesNotMutateInputMessages(t *testing.T) {
	srv, _, _ := captureServer(t)
	c := &Client{BaseURL: srv.URL, Kind: KindOpenAI, ConfiguredModel: "m", Model: "m",
		CacheControl: boolPtr(true), HTTP: http.DefaultClient}

	origContent := "preamble"
	msgs := []Message{
		{Role: "system", Content: &origContent},
		{Role: "user", Content: strPtr("query")},
	}
	// Snapshot before.
	before := make([]Message, len(msgs))
	copy(before, msgs)
	beforePtr := msgs[0].Content

	if _, err := c.Stream(t.Context(), msgs, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// Pointer unchanged.
	if msgs[0].Content != beforePtr {
		t.Errorf("messages[0].Content pointer changed: got %p, want %p", msgs[0].Content, beforePtr)
	}
	// Value unchanged.
	if *msgs[0].Content != origContent {
		t.Errorf("messages[0].Content value changed: got %q, want %q", *msgs[0].Content, origContent)
	}
	// Slice length unchanged.
	if len(msgs) != len(before) {
		t.Errorf("messages length changed: got %d, want %d", len(msgs), len(before))
	}
}

// --- Trim composition: marked positions computed post-trim ---

// TestCacheControlTrimComposition verifies that when the byte guard trims tool
// results, the final request body is encoded once (after trim) and breakpoints
// are recomputed on the trimmed slice.
func TestCacheControlTrimComposition(t *testing.T) {
	srv, _, body := captureServer(t)
	c := &Client{
		BaseURL:         srv.URL,
		Kind:            KindOpenAI,
		ConfiguredModel: "m",
		Model:           "m",
		CacheControl:    boolPtr(true),
		HTTP:            http.DefaultClient,
		MaxRequestBytes: 1000, // tiny limit forces trimming
	}
	bigContent := strings.Repeat("A", 5000)
	msgs := []Message{
		{Role: "system", Content: strPtr("preamble")},
		{Role: "user", Content: strPtr("query")},
		{Role: "tool", ToolCallID: "tc1", Name: "read_file", Content: strPtr(bigContent)},
	}
	if _, err := c.Stream(t.Context(), msgs, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	// The body must be under the limit.
	if len(*body) > 1000 {
		t.Errorf("body %d bytes, expected ≤ 1000 after trim", len(*body))
	}
	// The large content must be replaced.
	if strings.Contains(string(*body), bigContent[:100]) {
		t.Error("large content must have been trimmed")
	}
	// messages[0] must still be parts-shaped (cache_control survived trim).
	var raw struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(*body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	assertPartsShaped(t, raw.Messages[0].Content, "preamble", "messages[0] after trim")
}

// --- Usage: cache_creation_input_tokens ---

// TestUsageCacheCreationTokensParsed verifies cache_creation_input_tokens on
// the trailing usage chunk lands in UsageStat.CacheWriteTok.
func TestUsageCacheCreationTokensParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":20,\"cache_creation_input_tokens\":300}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Kind: KindOpenAI, ConfiguredModel: "m", Model: "m", HTTP: http.DefaultClient}
	if _, err := c.Stream(context.Background(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	u := c.LastUsage()
	if u.CacheWriteTok != 300 {
		t.Errorf("CacheWriteTok = %d, want 300", u.CacheWriteTok)
	}
}

// TestUsageCacheCreationAbsentZero verifies that when cache_creation_input_tokens
// is absent, CacheWriteTok stays 0.
func TestUsageCacheCreationAbsentZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":20}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	c := &Client{BaseURL: srv.URL, Kind: KindOpenAI, ConfiguredModel: "m", Model: "m", HTTP: http.DefaultClient}
	if _, err := c.Stream(context.Background(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	u := c.LastUsage()
	if u.CacheWriteTok != 0 {
		t.Errorf("CacheWriteTok = %d, want 0 when cache_creation_input_tokens absent", u.CacheWriteTok)
	}
}

// --- Helpers ---

// assertPartsShaped checks that content is a JSON array of one text part with
// cache_control: {"type":"ephemeral"}.
func assertPartsShaped(t *testing.T, raw json.RawMessage, wantText, label string) {
	t.Helper()
	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		t.Errorf("%s: content is not parts-shaped (unmarshal error: %v): %s", label, err, raw)
		return
	}
	if len(parts) != 1 {
		t.Errorf("%s: expected 1 part, got %d", label, len(parts))
		return
	}
	if parts[0].Type != "text" {
		t.Errorf("%s: part type = %q, want text", label, parts[0].Type)
	}
	if parts[0].Text != wantText {
		t.Errorf("%s: part text = %q, want %q", label, parts[0].Text, wantText)
	}
	if parts[0].CacheControl == nil {
		t.Errorf("%s: cache_control is nil, want non-nil", label)
		return
	}
	if parts[0].CacheControl.Type != "ephemeral" {
		t.Errorf("%s: cache_control.type = %q, want ephemeral", label, parts[0].CacheControl.Type)
	}
}

// assertPlainString checks that content is a plain JSON string matching want.
func assertPlainString(t *testing.T, raw json.RawMessage, want, label string) {
	t.Helper()
	var got string
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Errorf("%s: content is not a plain string (unmarshal error: %v): %s", label, err, raw)
		return
	}
	if got != want {
		t.Errorf("%s: content = %q, want %q", label, got, want)
	}
}

// --- Heuristic: cache_control auto-enables for Anthropic models ---

// TestAnthropicCacheHeuristic verifies the three states of the heuristic.
func TestAnthropicCacheHeuristic(t *testing.T) {
	cases := []struct {
		name  string
		flag  *bool
		model string
		want  bool
	}{
		// Explicit on always wins.
		{"explicit true, non-anthropic model", boolPtr(true), "qwen3.6-35b", true},
		{"explicit true, anthropic model", boolPtr(true), "anthropic/claude-sonnet-4-6", true},

		// Explicit off always wins (overrides heuristic).
		{"explicit false, anthropic model", boolPtr(false), "anthropic/claude-sonnet-4-6", false},

		// Heuristic: nil flag → auto-on for Anthropic models.
		{"nil flag, anthropic/claude model", nil, "anthropic/claude-sonnet-4-6", true},
		{"nil flag, claude- prefix model", nil, "claude-opus-4-8", true},
		{"nil flag, ANTHROPIC/CLAUDE uppercase", nil, "ANTHROPIC/CLAUDE-SONNET-4-6", true},

		// Heuristic: nil flag → off for non-Anthropic models.
		{"nil flag, qwen model", nil, "qwen3.6-35b", false},
		{"nil flag, gpt model", nil, "openai/gpt-4o", false},
		{"nil flag, ilm proxy alias", nil, "ilm", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := anthropicCacheHeuristic(tc.flag, tc.model)
			if got != tc.want {
				t.Errorf("anthropicCacheHeuristic(%v, %q) = %v, want %v", tc.flag, tc.model, got, tc.want)
			}
		})
	}
}

// TestCacheControlHeuristicAutoOnAnthropic verifies end-to-end that an
// endpoint with no cache_control flag but an anthropic/claude model string
// automatically gets cache_control breakpoints on the wire.
func TestCacheControlHeuristicAutoOnAnthropic(t *testing.T) {
	srv, _, body := captureServer(t)
	c := &Client{BaseURL: srv.URL, Kind: KindOpenAI, ConfiguredModel: "anthropic/claude-sonnet-4-6", Model: "anthropic/claude-sonnet-4-6", HTTP: http.DefaultClient}
	msgs := []Message{
		{Role: "system", Content: strPtr("preamble")},
		{Role: "user", Content: strPtr("query")},
	}
	if _, err := c.Stream(t.Context(), msgs, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var raw struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(*body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// messages[0] should be parts-shaped (heuristic auto-enabled).
	assertPartsShaped(t, raw.Messages[0].Content, "preamble", "messages[0] (heuristic)")
}

// TestCacheControlHeuristicExplicitFalseOverrides verifies that setting
// cache_control: false explicitly disables caching even for an Anthropic model.
func TestCacheControlHeuristicExplicitFalseOverrides(t *testing.T) {
	srv, _, body := captureServer(t)
	c := &Client{BaseURL: srv.URL, Kind: KindOpenAI, ConfiguredModel: "anthropic/claude-sonnet-4-6", Model: "anthropic/claude-sonnet-4-6",
		CacheControl: boolPtr(false), HTTP: http.DefaultClient}
	msgs := []Message{
		{Role: "system", Content: strPtr("preamble")},
		{Role: "user", Content: strPtr("query")},
	}
	if _, err := c.Stream(t.Context(), msgs, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var raw struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(*body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// No parts-shaping — explicit false overrides the heuristic.
	assertPlainString(t, raw.Messages[0].Content, "preamble", "messages[0] (explicit false)")
}
