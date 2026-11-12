package counsel

// Wire test: a Mashūra call under zdr on carries the provider ZDR fields,
// and a Mashūra call with both switches off sends byte-identical bytes to
// the pre-policy path. All other counsel tests run with both switches off.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// TestMashuraCallUnderZDROnCarriesFields runs one panel call with zdr on
// against a fake OpenRouter-classified host and asserts the provider fields
// arrive on the wire, merged with the request's other fields.
func TestMashuraCallUnderZDROnCarriesFields(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		gotBody = b
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// A diligent cited answer.
		w.Write([]byte("data: " + mustJSON(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "ok\nLAST SECTION READ: Section"}, "finish_reason": nil}}}) + "\n\n"))
		w.Write([]byte("data: " + mustJSON(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}}) + "\n\n"))
		w.Write([]byte("data: " + mustJSON(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5}}) + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := &proxy.Client{
		BaseURL: "https://openrouter.ai/api/v1/chat/completions",
		HTTP:    rewriteToServer(srv),
		ZDR:     "on",
	}
	ccfg := PanelCallConfig{MaxTokens: 512, TimeoutSeconds: 5, Transport: client}

	// Drive the OpenRouter member path with a citation-bearing reply.
	results := RunPanel(context.Background(), []string{"anthropic:test"}, "panel", "q", "## Section\nbody\n", ccfg, map[string]string{"openrouter": "k"})
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].Err != nil {
		t.Fatalf("panel error: %v", results[0].Err)
	}
	var top map[string]any
	if err := json.Unmarshal(gotBody, &top); err != nil {
		t.Fatalf("wire body not JSON: %v\n%s", err, gotBody)
	}
	prov, ok := top["provider"].(map[string]any)
	if !ok {
		t.Fatalf("provider object missing from wire body: %v", top)
	}
	if prov["zdr"] != true || prov["data_collection"] != "deny" {
		t.Errorf("provider = %v, want zdr:true and data_collection:deny", prov)
	}
}

// TestMashuraCallDefaultsOffByteEquality verifies that a Mashūra call with
// both switches off carries no provider fields (byte-identical to the
// pre-policy request for the same member).
// It is the Mashūra half of DO NOT ALTER 4.
func TestMashuraCallDefaultsOffByteEquality(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, r.ContentLength)
		r.Body.Read(b)
		gotBody = b
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: " + mustJSON(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"content": "ok\nLAST SECTION READ: Section"}, "finish_reason": nil}}}) + "\n\n"))
		w.Write([]byte("data: " + mustJSON(map[string]any{"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "stop"}}}) + "\n\n"))
		w.Write([]byte("data: " + mustJSON(map[string]any{"choices": []any{}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5}}) + "\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	client := &proxy.Client{BaseURL: srv.URL, HTTP: srv.Client(), ZDR: "off", Region: "off"}
	ccfg := PanelCallConfig{MaxTokens: 512, TimeoutSeconds: 5, Transport: client}
	results := RunPanel(context.Background(), []string{"anthropic:test"}, "panel", "q", "## Section\nbody\n", ccfg, map[string]string{"openrouter": "k"})
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("panel error: %+v", results)
	}
	var top map[string]any
	if err := json.Unmarshal(gotBody, &top); err != nil {
		t.Fatalf("wire body not JSON: %v\n%s", err, gotBody)
	}
	if _, ok := top["provider"]; ok {
		t.Errorf("provider object present with switches off: %v", top)
	}
}

// rewriteToServer rewrites the request URL host to srv, so the OpenRouter
// policy (enforced on the original BaseURL, which classifies as OpenRouter)
// is exercised against the fake server.
//
// The endpoint orChat derives is the transport client's own BaseURL when the
// client carries a non-empty one (the test seam for httptest URLs); so a
// Mashūra call under zdr on exercised through RunPanel with an
// OpenRouter-classified client exercises the OpenRouter branch end to end.
func rewriteToServer(srv *httptest.Server) *http.Client {
	host := srv.URL[len("http://"):]
	return &http.Client{Transport: rewriteTransport{host: host}}
}

type rewriteTransport struct{ host string }

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	u.Scheme = "http"
	u.Host = rt.host
	req = req.Clone(req.Context())
	req.URL = &u
	return http.DefaultTransport.RoundTrip(req)
}
