package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// CaptureTransport rewrites any request URL to the httptest capture server so
// an OpenRouter-host BaseURL can be exercised offline.
type captureTransport struct {
	srvURL string
	base   http.RoundTripper
}

func (c *captureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r.URL.Scheme = "http"
	r.URL.Host = strings.TrimPrefix(c.srvURL, "http://")
	return c.base.RoundTrip(r)
}

func TestOpenRouterHostGate(t *testing.T) {
	if !isOpenRouterHost("https://openrouter.ai/api") {
		t.Error("openrouter.ai must be recognised")
	}
	if !isOpenRouterHost("https://api.openrouter.ai/v1") {
		t.Error("api.openrouter.ai subdomain must be recognised")
	}
	if isOpenRouterHost("http://127.0.0.1:8080") {
		t.Error("loopback must not be OpenRouter")
	}
	if isOpenRouterHost("https://openrouter.ai.evil.example/") {
		t.Error("evil suffix must not match")
	}
}

func TestWireBodyOpenRouterFieldsAbsentForNonOpenRouter(t *testing.T) {
	srv, _, body := captureServer(t)
	c := &Client{
		BaseURL: srv.URL, Kind: KindOpenAI, ConfiguredModel: "m", Model: "m",
		ChatID: "sess-1", HTTP: srv.Client(),
	}
	_, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(*body, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["session_id"]; ok {
		t.Error("non-OpenRouter request must not carry session_id")
	}
	if _, ok := raw["plugins"]; ok {
		t.Error("non-OpenRouter request must not carry plugins")
	}
}

func TestWireBodyOpenRouterFieldsPresent(t *testing.T) {
	// Drive a real Stream with BaseURL=https://openrouter.ai/api but rewrite
	// the transport to the capture server (offline). Asserts the exact new
	// body fields: session_id = ChatID and plugins with
	// context-compression disabled.
	srv, _, body := captureServer(t)
	c := &Client{
		BaseURL: "https://openrouter.ai/api", Kind: KindOpenAI,
		ConfiguredModel: "m", Model: "m", ChatID: "sess-or-1",
		HTTP: &http.Client{Transport: &captureTransport{srvURL: srv.URL, base: http.DefaultTransport}},
	}
	_, err := c.Stream(t.Context(), []Message{{Role: "user", Content: strPtr("hi")}}, nil, nil, nil)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(*body, &raw); err != nil {
		t.Fatal(err)
	}
	var sid string
	if err := json.Unmarshal(raw["session_id"], &sid); err != nil || sid != "sess-or-1" {
		t.Errorf("session_id = %s (err %v), want sess-or-1", raw["session_id"], err)
	}
	var plugins []orWirePlugin
	if err := json.Unmarshal(raw["plugins"], &plugins); err != nil {
		t.Fatalf("plugins unmarshal: %v (raw %s)", err, raw["plugins"])
	}
	if len(plugins) != 1 || plugins[0].ID != "context-compression" || plugins[0].Enabled {
		t.Errorf("plugins = %s, want exactly [{context-compression,false}]", raw["plugins"])
	}
}

func TestCategoryTotalsAndExportRestore(t *testing.T) {
	tr := NewCostTracker()
	tr.RecordCategorized("inference·openrouter/m", CostCatMain, 100, 50, 0.30, true, ConfExact)
	tr.RecordCategorized("inference·openrouter/m", CostCatSubagent, 10, 5, 0.03, true, ConfExact)
	tr.RecordCategorized("mashura·m", CostCatMashura, 10, 5, 0.02, true, ConfExact)
	tr.RecordCategorized("inference", CostCatAux, 10, 5, 0.01, true, ConfModeled)
	ct, rows := tr.SnapshotDetail()
	if ct.Main != 0.30 || ct.Subagent != 0.03 || ct.Mashura != 0.02 || ct.Aux != 0.01 {
		t.Errorf("split = %+v", ct)
	}
	if ct.Provider != 0.35 || ct.Estimated != 0.01 {
		t.Errorf("provider/estimated = %v/%v", ct.Provider, ct.Estimated)
	}
	// Export → restore into a fresh tracker.
	tr2 := NewCostTracker()
	tr2.RestoreRows(rows)
	ct2, _ := tr2.SnapshotDetail()
	if ct2.Total != ct.Total || ct2.Main != ct.Main {
		t.Errorf("restore mismatch: %+v vs %+v", ct2, ct)
	}
}

// TestFmtUSDSplitBoundary pins the split format: four decimals below $1 so
// small subagent and Mashūra costs stay visible, two decimals from $1 up.
func TestFmtUSDSplitBoundary(t *testing.T) {
	cases := []struct {
		v    float64
		want string
	}{
		{0, "$0.0000"},
		{0.0034, "$0.0034"},
		{0.99995, "$1.00"}, // rounds to $1.0000 — shown as $1.00
		{0.9999, "$0.9999"},
		{1, "$1.00"},
		{12.345, "$12.35"},
	}
	for _, c := range cases {
		if got := FmtUSDSplit(c.v); got != c.want {
			t.Errorf("FmtUSDSplit(%v) = %q, want %q", c.v, got, c.want)
		}
	}
}
