package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// captureServer records the raw request body and headers of the last
// /v1/chat/completions call, then answers with a minimal valid SSE stream.
func captureServer(t *testing.T) (*httptest.Server, *http.Header, *[]byte) {
	t.Helper()
	var hdr http.Header
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr = r.Header.Clone()
		b, _ := io.ReadAll(r.Body)
		body = b
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &hdr, &body
}

func floatPtr(f float64) *float64 { return &f }
func intPtr(i int) *int           { return &i }
func boolPtr(b bool) *bool        { return &b }

// TestOpenAIKindRequestShape: a KindOpenAI client must emit no metadata key
// at all, no X-Ilm-* headers, and the configured model — even when session
// state (Model field) holds the proxy alias or a backend-prefixed string.
func TestOpenAIKindRequestShape(t *testing.T) {
	for _, sessionModel := range []string{"ilm", "openrouter/anthropic/claude-opus-4-8"} {
		t.Run("sessionModel="+sessionModel, func(t *testing.T) {
			srv, hdr, body := captureServer(t)

			c := &Client{
				BaseURL:         srv.URL,
				Kind:            KindOpenAI,
				ConfiguredModel: "qwen3.6-35b",
				Model:           sessionModel, // stale session state must NOT win
				ChatID:          "session-1",
				NoMemoryWrite:   true,       // would emit metadata+header on proxy kind
				Backend:         "llama",    // would emit X-Ilm-Backend on proxy kind
				AuxModel:        "aux-tiny", // would emit X-Ilm-Aux-Model on proxy kind
				HTTP:            http.DefaultClient,
			}
			if _, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil); err != nil {
				t.Fatalf("Stream: %v", err)
			}

			// No X-Ilm-* request headers.
			for k := range *hdr {
				if len(k) >= 5 && k[:5] == "X-Ilm" {
					t.Errorf("openai kind must not send %s header", k)
				}
			}

			// Raw-key inspection: metadata must be entirely absent, not empty.
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(*body, &raw); err != nil {
				t.Fatalf("unmarshal request body: %v", err)
			}
			if _, ok := raw["metadata"]; ok {
				t.Error("openai kind must not include a metadata key at all")
			}
			var model string
			if err := json.Unmarshal(raw["model"], &model); err != nil {
				t.Fatal(err)
			}
			if model != "qwen3.6-35b" {
				t.Errorf("model = %q, want configured qwen3.6-35b (session state %q must not leak)", model, sessionModel)
			}
			// No sampling fields configured → none present.
			for _, k := range []string{"temperature", "top_p", "max_tokens"} {
				if _, ok := raw[k]; ok {
					t.Errorf("unset sampling field %q must be absent from body", k)
				}
			}
		})
	}
}

// TestOpenAIKindSamplingFieldsPresentWhenConfigured: configured sampling
// values appear in the body; unset ones stay absent.
func TestOpenAIKindSamplingFieldsPresentWhenConfigured(t *testing.T) {
	srv, _, body := captureServer(t)

	c := &Client{
		BaseURL:         srv.URL,
		Kind:            KindOpenAI,
		ConfiguredModel: "m",
		Model:           "m",
		Temperature:     floatPtr(0.2),
		MaxTokens:       intPtr(1024),
		// TopP deliberately unset.
		HTTP: http.DefaultClient,
	}
	if _, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(*body, &raw); err != nil {
		t.Fatal(err)
	}
	var temp float64
	if err := json.Unmarshal(raw["temperature"], &temp); err != nil || temp != 0.2 {
		t.Errorf("temperature = %s (err %v), want 0.2", raw["temperature"], err)
	}
	var mt int
	if err := json.Unmarshal(raw["max_tokens"], &mt); err != nil || mt != 1024 {
		t.Errorf("max_tokens = %s (err %v), want 1024", raw["max_tokens"], err)
	}
	if _, ok := raw["top_p"]; ok {
		t.Error("top_p unset — must be absent")
	}
}

// TestIlmProxyKindGoldenRequestShape is the no-op proof for proxy users: the
// exact request shape of a pre-endpoints client (Kind ""), compared key by
// key against an explicit Kind "ilm-proxy" client and against the historical
// expectations (metadata.chat_id, X-Ilm-* headers, verbatim model).
func TestIlmProxyKindGoldenRequestShape(t *testing.T) {
	build := func(kind string) (http.Header, []byte) {
		srv, hdr, body := captureServer(t)
		c := &Client{
			BaseURL:  srv.URL,
			Kind:     kind,
			Model:    "ilm",
			ChatID:   "golden-chat",
			Backend:  "openrouter",
			AuxModel: "aux-model",
			HTTP:     http.DefaultClient,
		}
		if _, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil); err != nil {
			t.Fatalf("Stream: %v", err)
		}
		return *hdr, *body
	}

	legacyHdr, legacyBody := build("") // pre-endpoints construction site
	proxyHdr, proxyBody := build(KindIlmProxy)

	// Byte-identical bodies between Kind "" and Kind "ilm-proxy".
	if string(legacyBody) != string(proxyBody) {
		t.Errorf("kind \"\" and kind ilm-proxy bodies differ:\n legacy: %s\n proxy:  %s", legacyBody, proxyBody)
	}

	// Golden expectations (historical shape).
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(proxyBody, &raw); err != nil {
		t.Fatal(err)
	}
	var meta map[string]string
	if err := json.Unmarshal(raw["metadata"], &meta); err != nil {
		t.Fatalf("metadata missing or malformed: %v", err)
	}
	if meta["chat_id"] != "golden-chat" {
		t.Errorf("metadata.chat_id = %q, want golden-chat", meta["chat_id"])
	}
	var model string
	json.Unmarshal(raw["model"], &model)
	if model != "ilm" {
		t.Errorf("model = %q, want alias ilm", model)
	}
	for hdrName, want := range map[string]string{
		"X-Ilm-Backend":   "openrouter",
		"X-Ilm-Aux-Model": "aux-model",
	} {
		if got := proxyHdr.Get(hdrName); got != want {
			t.Errorf("%s = %q, want %q", hdrName, got, want)
		}
		if got := legacyHdr.Get(hdrName); got != want {
			t.Errorf("legacy %s = %q, want %q", hdrName, got, want)
		}
	}
	// Sampling fields never configured → absent for proxy kind too.
	for _, k := range []string{"temperature", "top_p", "max_tokens"} {
		if _, ok := raw[k]; ok {
			t.Errorf("proxy kind: unset sampling field %q must be absent", k)
		}
	}
}

// TestIlmProxyKindNoMemoryWriteShape: subagent-style client (NoMemoryWrite)
// keeps its historical proxy shape: header + metadata flag, no chat_id.
func TestIlmProxyKindNoMemoryWriteShape(t *testing.T) {
	srv, hdr, body := captureServer(t)
	c := &Client{
		BaseURL:       srv.URL,
		Kind:          KindIlmProxy,
		Model:         "ilm",
		ChatID:        "sub-chat",
		NoMemoryWrite: true,
		HTTP:          http.DefaultClient,
	}
	if _, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if got := hdr.Get("X-Ilm-No-Memory-Write"); got != "true" {
		t.Errorf("X-Ilm-No-Memory-Write = %q, want true", got)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(*body, &raw); err != nil {
		t.Fatal(err)
	}
	var meta map[string]string
	if err := json.Unmarshal(raw["metadata"], &meta); err != nil {
		t.Fatalf("metadata missing: %v", err)
	}
	if meta["ilm-no-memory-write"] != "true" {
		t.Errorf("metadata.ilm-no-memory-write = %q, want true", meta["ilm-no-memory-write"])
	}
	if _, ok := meta["chat_id"]; ok {
		t.Error("NoMemoryWrite client must not send chat_id (defence in depth)")
	}
}

// TestCachePromptAbsentWhenUnset: cache_prompt is llama.cpp-specific and must
// never appear unless an endpoint explicitly opts in — an unset *bool must
// omit the key entirely (never send a literal false), same discipline as the
// other pointer-typed sampling fields.
func TestCachePromptAbsentWhenUnset(t *testing.T) {
	srv, _, body := captureServer(t)
	c := &Client{BaseURL: srv.URL, Kind: KindOpenAI, ConfiguredModel: "m", Model: "m", HTTP: http.DefaultClient}
	if _, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(*body, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["cache_prompt"]; ok {
		t.Errorf("cache_prompt unset — must be absent, got %s", raw["cache_prompt"])
	}
}

// TestCachePromptPresentWhenSet: an endpoint that opts in gets the field sent
// verbatim.
func TestCachePromptPresentWhenSet(t *testing.T) {
	srv, _, body := captureServer(t)
	c := &Client{BaseURL: srv.URL, Kind: KindOpenAI, ConfiguredModel: "m", Model: "m", CachePrompt: boolPtr(true), HTTP: http.DefaultClient}
	if _, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(*body, &raw); err != nil {
		t.Fatal(err)
	}
	var got bool
	if err := json.Unmarshal(raw["cache_prompt"], &got); err != nil {
		t.Fatalf("cache_prompt missing or malformed: %v", err)
	}
	if !got {
		t.Errorf("cache_prompt = %v, want true", got)
	}
}

// TestCachePromptSentForIlmProxyKindToo: cache_prompt is deliberately NOT
// gated on Kind — the proxy fronts llama.cpp too, so an ilm-proxy endpoint
// that opts in must also get the field. Only the explicit per-endpoint flag
// controls it, never Kind.
func TestCachePromptSentForIlmProxyKindToo(t *testing.T) {
	srv, _, body := captureServer(t)
	c := &Client{BaseURL: srv.URL, Kind: KindIlmProxy, Model: "ilm", CachePrompt: boolPtr(true), HTTP: http.DefaultClient}
	if _, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(*body, &raw); err != nil {
		t.Fatal(err)
	}
	var got bool
	if err := json.Unmarshal(raw["cache_prompt"], &got); err != nil {
		t.Fatalf("cache_prompt missing or malformed for ilm-proxy kind: %v", err)
	}
	if !got {
		t.Errorf("cache_prompt = %v, want true", got)
	}
}
