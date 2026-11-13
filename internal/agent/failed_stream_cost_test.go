package agent

// failed_stream_cost_test.go covers B1: cost rows for Stream calls that
// failed after sending, recorded from the usage the client published for
// that request. The exclusion list — user cancels, pre-send policy
// refusals, HTTP error statuses before stream data (including the first
// 404 of the zdr auto retry) — records nothing; a stream that failed while
// open records one row from the provisional estimate (or the exact usage
// when the provider already reported it), unpriced when no price is known.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

func failedStreamTestClient(t *testing.T, srv *httptest.Server) *proxy.Client {
	t.Helper()
	return &proxy.Client{BaseURL: srv.URL, HTTP: srv.Client(), Model: "test-model", Kind: proxy.KindIlmProxy}
}

func failedStreamTestApp(client *proxy.Client) *App {
	return &App{
		Cfg:       config.Config{Costs: config.CostsConfig{Inference: config.InferenceRate{USDPer1MTokens: 5}}},
		Client:    client,
		costState: costState{Costs: proxy.NewCostTracker()},
	}
}

func costRows(t *testing.T, app *App) []proxy.CostRow {
	t.Helper()
	_, rows := app.Costs.Snapshot()
	return rows
}

// costCalls returns the total Calls column across rows plus the total
// InputTok. A double record would merge into one row (CostTracker aggregates
// by source key), so row count alone cannot catch it — these totals can.
func costCalls(t *testing.T, app *App) (calls int, inputTok int64) {
	t.Helper()
	for _, r := range costRows(t, app) {
		calls += r.Calls
		inputTok += r.InputTok
	}
	return calls, inputTok
}

// TestFailedStreamCost_TruncatedStreamRecordsOneRow: stream data arrived,
// then the connection closed without [DONE]. One row from the provisional
// input estimate, marked estimated (approx confidence).
func TestFailedStreamCost_TruncatedStreamRecordsOneRow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	app := failedStreamTestApp(failedStreamTestClient(t, srv))
	before := app.Client.UsageStamp()
	_, err := app.Client.Stream(context.Background(),
		[]proxy.Message{{Role: "user", Content: StrPtr("hi")}}, nil, nil, nil)
	if err == nil {
		t.Fatal("truncated stream must error")
	}
	app.RecordFailedStreamCost(before, err)
	rows := costRows(t, app)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].Calls != 1 {
		t.Errorf("Calls = %d, want 1 (one recorded failure, not two)", rows[0].Calls)
	}
	if rows[0].InputTok <= 0 {
		t.Errorf("InputTok = %d, want the provisional estimate", rows[0].InputTok)
	}
	if rows[0].Confidence != proxy.ConfApprox {
		t.Errorf("Confidence = %q, want %q (estimated)", rows[0].Confidence, proxy.ConfApprox)
	}
}

// TestFailedStreamCost_IdleTimeoutRecordsOneRow: stream data arrived, then
// Wakil's own idle timeout fired. One row.
func TestFailedStreamCost_IdleTimeoutRecordsOneRow(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")
		w.(http.Flusher).Flush()
		// Hold the stream open past the idle timeout without sending more.
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer srv.Close()

	app := failedStreamTestApp(failedStreamTestClient(t, srv))
	app.Client.StreamIdleTimeout = 150 * time.Millisecond
	before := app.Client.UsageStamp()
	_, err := app.Client.Stream(context.Background(),
		[]proxy.Message{{Role: "user", Content: StrPtr("hi")}}, nil, nil, nil)
	if err == nil {
		t.Fatal("idle timeout must error")
	}
	app.RecordFailedStreamCost(before, err)
	rows := costRows(t, app)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if calls, _ := costCalls(t, app); calls != 1 {
		t.Errorf("Calls = %d, want 1", calls)
	}
}

// TestFailedStreamCost_UserCancelRecordsNothing: a stream abandoned by the
// user's cancel records no row even when stream data had arrived.
func TestFailedStreamCost_UserCancelRecordsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	app := failedStreamTestApp(failedStreamTestClient(t, srv))
	ctx, cancel := context.WithCancel(context.Background())
	before := app.Client.UsageStamp()
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	_, err := app.Client.Stream(ctx,
		[]proxy.Message{{Role: "user", Content: StrPtr("hi")}}, nil, nil, nil)
	if err == nil {
		t.Fatal("cancelled stream must error")
	}
	app.RecordFailedStreamCost(before, err)
	if rows := costRows(t, app); len(rows) != 0 {
		t.Fatalf("rows = %d, want 0 on user cancel", len(rows))
	}
}

// TestFailedStreamCost_PolicyRefusalRecordsNothing: a request refused
// before sending (data policy cannot be enforced) records no row.
func TestFailedStreamCost_PolicyRefusalRecordsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("refused request must not reach the server")
		w.WriteHeader(500)
	}))
	defer srv.Close()

	app := failedStreamTestApp(&proxy.Client{BaseURL: "https://api.example.com/v1/chat/completions", HTTP: srv.Client(), Model: "m", ZDR: "on", Region: "off"})
	before := app.Client.UsageStamp()
	_, err := app.Client.Stream(context.Background(),
		[]proxy.Message{{Role: "user", Content: StrPtr("hi")}}, nil, nil, nil)
	if err == nil {
		t.Fatal("policy refusal must error")
	}
	app.RecordFailedStreamCost(before, err)
	if rows := costRows(t, app); len(rows) != 0 {
		t.Fatalf("rows = %d, want 0 on policy refusal", len(rows))
	}
}

// TestFailedStreamCost_HTTPErrorBeforeStreamRecordsNothing: a 500 returned
// before any stream data records no row (refused, not billed).
func TestFailedStreamCost_HTTPErrorBeforeStreamRecordsNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		fmt.Fprint(w, `{"error":{"message":"boom"}}`)
	}))
	defer srv.Close()

	app := failedStreamTestApp(failedStreamTestClient(t, srv))
	before := app.Client.UsageStamp()
	_, err := app.Client.Stream(context.Background(),
		[]proxy.Message{{Role: "user", Content: StrPtr("hi")}}, nil, nil, nil)
	if err == nil {
		t.Fatal("HTTP 500 must error")
	}
	app.RecordFailedStreamCost(before, err)
	if rows := costRows(t, app); len(rows) != 0 {
		t.Fatalf("rows = %d, want 0 on HTTP error before stream data", len(rows))
	}
}

// TestFailedStreamCost_ZDRAutoRetryOneRow: the first 404 of the zdr auto
// retry is not billed; the successful retry records exactly one row.
func TestFailedStreamCost_ZDRAutoRetryOneRow(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(404)
			fmt.Fprint(w, `{"error":{"message":"no zdr endpoint"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	app := failedStreamTestApp(failedStreamTestClient(t, srv))
	app.Client.ZDR = "auto"
	app.Client.Region = "off"
	app.Client.BaseURL = "https://openrouter.ai/api/v1/chat/completions"
	app.Client.HTTP = rewriteTransportClient(srv)
	before := app.Client.UsageStamp()
	if _, err := app.Client.Stream(context.Background(),
		[]proxy.Message{{Role: "user", Content: StrPtr("hi")}}, nil, nil, nil); err != nil {
		t.Fatalf("stream after zdr retry: %v", err)
	}
	app.RecordInferenceCost()
	app.RecordFailedStreamCost(before, nil) // must not double-count the success
	calls, inputTok := costCalls(t, app)
	if calls != 1 {
		t.Fatalf("Calls = %d, want 1 (the successful retry only)", calls)
	}
	if inputTok != 10 {
		t.Errorf("InputTok = %d, want 10 (the successful retry's exact usage)", inputTok)
	}
}

// TestFailedStreamCost_NoStaleUsageAfterRefusal: a successful request bills
// once; a later policy-refused request adds no row and does not re-bill the
// earlier request's usage.
func TestFailedStreamCost_NoStaleUsageAfterRefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	app := failedStreamTestApp(failedStreamTestClient(t, srv))
	before := app.Client.UsageStamp()
	if _, err := app.Client.Stream(context.Background(),
		[]proxy.Message{{Role: "user", Content: StrPtr("hi")}}, nil, nil, nil); err != nil {
		t.Fatalf("first stream: %v", err)
	}
	app.RecordInferenceCost()

	// The client still holds the first request's usage. A policy-refused
	// request must not attribute it again.
	app.Client.ZDR = "on"
	app.Client.BaseURL = "https://api.example.com/v1/chat/completions"
	beforeRefused := app.Client.UsageStamp()
	_, err := app.Client.Stream(context.Background(),
		[]proxy.Message{{Role: "user", Content: StrPtr("hi")}}, nil, nil, nil)
	if err == nil {
		t.Fatal("policy refusal must error")
	}
	app.RecordFailedStreamCost(beforeRefused, err)
	calls, inputTok := costCalls(t, app)
	if calls != 1 {
		t.Fatalf("Calls = %d, want 1 (the first request only; no stale re-bill)", calls)
	}
	if inputTok != 10 {
		t.Errorf("InputTok = %d, want 10 (only the first request billed)", inputTok)
	}
	_ = before // the success path used its own stamp; kept for symmetry
}

// TestFailedStreamCost_NilErrNoDoubleCount: passing a nil error (a caller
// mistake — the stream succeeded) records nothing; a successful call
// recorded once stays recorded once.
func TestFailedStreamCost_NilErrNoDoubleCount(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	app := failedStreamTestApp(failedStreamTestClient(t, srv))
	before := app.Client.UsageStamp()
	if _, err := app.Client.Stream(context.Background(),
		[]proxy.Message{{Role: "user", Content: StrPtr("hi")}}, nil, nil, nil); err != nil {
		t.Fatalf("stream: %v", err)
	}
	app.RecordInferenceCost()
	app.RecordFailedStreamCost(before, nil)
	if calls, _ := costCalls(t, app); calls != 1 {
		t.Fatalf("Calls = %d, want 1 (success recorded once, failure recorder no-op)", calls)
	}
}

// TestFailedStreamCost_ExactUsagePricedAndUnpriced: exact usage for a failed
// stream prices through the normal path; unknown-price usage counts as
// unpriced.
func TestFailedStreamCost_ExactUsagePricedAndUnpriced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"},\"finish_reason\":null}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1000,\"completion_tokens\":500}}\n\n")
		// No [DONE]: truncated after the usage chunk.
		w.(http.Flusher).Flush()
	}))
	defer srv.Close()

	app := failedStreamTestApp(failedStreamTestClient(t, srv))
	before := app.Client.UsageStamp()
	_, err := app.Client.Stream(context.Background(),
		[]proxy.Message{{Role: "user", Content: StrPtr("hi")}}, nil, nil, nil)
	if err == nil {
		t.Fatal("truncated stream must error")
	}
	// The stream published no final usage (it failed before completing), so
	// seed the exact usage the client would hold for a provider-reported
	// failure and record it.
	app.Client.SetUsage(proxy.UsageStat{InputTok: 1000, OutputTok: 500, Exact: true})
	app.RecordFailedStreamCost(before, err)
	rows := costRows(t, app)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].InputTok != 1000 || rows[0].OutputTok != 500 {
		t.Errorf("row tokens = %d/%d, want 1000/500 (exact)", rows[0].InputTok, rows[0].OutputTok)
	}

	// Unpriced: no inference rate configured.
	app2 := &App{
		Cfg:       config.Config{},
		Client:    failedStreamTestClient(t, srv),
		costState: costState{Costs: proxy.NewCostTracker()},
	}
	before2 := app2.Client.UsageStamp()
	_, err2 := app2.Client.Stream(context.Background(),
		[]proxy.Message{{Role: "user", Content: StrPtr("hi")}}, nil, nil, nil)
	if err2 == nil {
		t.Fatal("truncated stream must error")
	}
	app2.RecordFailedStreamCost(before2, err2)
	rows2 := costRows(t, app2)
	if len(rows2) != 1 {
		t.Fatalf("unpriced rows = %d, want 1", len(rows2))
	}
	if rows2[0].Priced {
		t.Error("unknown price must count as unpriced")
	}
}

// TestFailedStreamCost_NilGuards: nil tracker/client record nothing and
// never panic, on either the failure or the success recorder.
func TestFailedStreamCost_NilGuards(t *testing.T) {
	bare := &App{Cfg: config.DefaultConfig()}
	bare.RecordFailedStreamCost(0, errors.New("boom"))
	bare.RecordInferenceCost()

	noTracker := &App{Cfg: config.DefaultConfig(), Client: &proxy.Client{Model: "m"}}
	noTracker.RecordFailedStreamCost(0, errors.New("boom"))
}

// rewriteTransportClient returns an HTTP client that rewrites every request
// URL to srv, so the transport's data policy (enforced on the original
// OpenRouter BaseURL) is exercised against a fake server.
func rewriteTransportClient(srv *httptest.Server) *http.Client {
	return &http.Client{Transport: rewriteTestTransport{base: srv.URL}}
}

type rewriteTestTransport struct{ base string }

func (t rewriteTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	u := *req.URL
	u.Scheme = "http"
	u.Host = strings.TrimPrefix(t.base, "http://")
	req = req.Clone(req.Context())
	req.URL = &u
	return http.DefaultTransport.RoundTrip(req)
}
