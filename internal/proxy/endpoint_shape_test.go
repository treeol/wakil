package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

			// No attribution headers on a non-openrouter endpoint with nil config.
			if v := (*hdr).Get("HTTP-Referer"); v != "" {
				t.Errorf("HTTP-Referer = %q, want absent", v)
			}
			if v := (*hdr).Get("X-Title"); v != "" {
				t.Errorf("X-Title = %q, want absent", v)
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
			// No sampling fields configured → temperature and top_p absent.
			// max_tokens gets a default of 8192 for KindOpenAI (reasoning-model fix).
			for _, k := range []string{"temperature", "top_p"} {
				if _, ok := raw[k]; ok {
					t.Errorf("unset sampling field %q must be absent from body", k)
				}
			}
			if mt, ok := raw["max_tokens"]; ok {
				var v int
				if err := json.Unmarshal(mt, &v); err != nil {
					t.Fatalf("unmarshal max_tokens: %v", err)
				}
				if v != 8192 {
					t.Errorf("default max_tokens = %d, want 8192", v)
				}
			} else {
				t.Error("KindOpenAI must send a default max_tokens=8192 when unset")
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

// strPtr2 returns a pointer to s. (Named strPtr2 to avoid clashing with the
// Message helper strPtr.)
func strPtr2(s string) *string { return &s }

// TestOpenAIAttributionHeadersAbsentOnGenericEndpoint: a non-openrouter
// openai endpoint with no attribution config must NOT send HTTP-Referer or
// X-Title headers.
func TestOpenAIAttributionHeadersAbsentOnGenericEndpoint(t *testing.T) {
	srv, hdr, _ := captureServer(t)
	c := &Client{
		BaseURL:         srv.URL, // httptest server, not openrouter
		Kind:            KindOpenAI,
		ConfiguredModel: "m",
		Model:           "m",
		HTTP:            http.DefaultClient,
	}
	if _, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if v := (*hdr).Get("HTTP-Referer"); v != "" {
		t.Errorf("HTTP-Referer = %q, want absent on non-openrouter endpoint", v)
	}
	if v := (*hdr).Get("X-Title"); v != "" {
		t.Errorf("X-Title = %q, want absent on non-openrouter endpoint", v)
	}
}

// TestOpenAIAttributionHeadersDefaultOnOpenRouter: an openrouter.ai endpoint
// with fields unset (nil) must send both default attribution headers.
func TestOpenAIAttributionHeadersDefaultOnOpenRouter(t *testing.T) {
	srv, hdr, _ := captureServer(t)
	c := &Client{
		BaseURL:         "https://openrouter.ai/api",
		Kind:            KindOpenAI,
		ConfiguredModel: "anthropic/claude-sonnet-4-6",
		Model:           "anthropic/claude-sonnet-4-6",
		HTTP:            &http.Client{Transport: &rewritingTransport{target: srv.URL}},
	}
	if _, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if v := (*hdr).Get("HTTP-Referer"); v != "https://github.com/treeol/wakil" {
		t.Errorf("HTTP-Referer = %q, want default https://github.com/treeol/wakil", v)
	}
	if v := (*hdr).Get("X-Title"); v != "wakil" {
		t.Errorf("X-Title = %q, want default wakil", v)
	}
}

// TestOpenAIAttributionHeadersRefererOptOut: an openrouter.ai endpoint with
// app_referer explicitly set to "" must omit HTTP-Referer but still send
// X-Title (default, since app_title is unset).
func TestOpenAIAttributionHeadersRefererOptOut(t *testing.T) {
	srv, hdr, _ := captureServer(t)
	c := &Client{
		BaseURL:         "https://openrouter.ai/api",
		Kind:            KindOpenAI,
		ConfiguredModel: "m",
		Model:           "m",
		AppReferer:      strPtr2(""),
		HTTP:            &http.Client{Transport: &rewritingTransport{target: srv.URL}},
	}
	if _, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if v := (*hdr).Get("HTTP-Referer"); v != "" {
		t.Errorf("HTTP-Referer = %q, want absent (opt-out via empty string)", v)
	}
	if v := (*hdr).Get("X-Title"); v != "wakil" {
		t.Errorf("X-Title = %q, want default wakil", v)
	}
}

// TestOpenAIAttributionHeadersExplicitPassthrough: any endpoint with both
// fields explicitly set must send them verbatim.
func TestOpenAIAttributionHeadersExplicitPassthrough(t *testing.T) {
	srv, hdr, _ := captureServer(t)
	c := &Client{
		BaseURL:         srv.URL, // non-openrouter host
		Kind:            KindOpenAI,
		ConfiguredModel: "m",
		Model:           "m",
		AppReferer:      strPtr2("https://myapp.example.com"),
		AppTitle:        strPtr2("my-agent"),
		HTTP:            http.DefaultClient,
	}
	if _, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if v := (*hdr).Get("HTTP-Referer"); v != "https://myapp.example.com" {
		t.Errorf("HTTP-Referer = %q, want https://myapp.example.com", v)
	}
	if v := (*hdr).Get("X-Title"); v != "my-agent" {
		t.Errorf("X-Title = %q, want my-agent", v)
	}
}

// rewritingTransport redirects all requests to a target URL while preserving
// the original headers. Used in tests that need the client to think it's
// talking to openrouter.ai but actually hit a httptest.Server.
type rewritingTransport struct{ target string }

func (t *rewritingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	targetURL, _ := url.Parse(t.target)
	req.URL.Scheme = targetURL.Scheme
	req.URL.Host = targetURL.Host
	return http.DefaultTransport.RoundTrip(req)
}
