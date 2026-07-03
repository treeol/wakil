package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agent "github.com/treeol/wakil/internal/agent"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// limitsServer serves /v1/ilm/limits and/or /props with caller-supplied bodies.
// An empty body for a path makes it 404, so a test can exercise the /props
// fallback by leaving limits empty.
func limitsServer(t *testing.T, limitsBody, propsBody string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/ilm/limits":
			if limitsBody == "" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, limitsBody)
		case "/props":
			if propsBody == "" {
				http.NotFound(w, r)
				return
			}
			fmt.Fprint(w, propsBody)
		default:
			http.NotFound(w, r)
		}
	}))
}

// TestResolveContextLimitBackend: the backend reports n_ctx=196608 via the
// dedicated /v1/ilm/limits route → Wakil's limit reads ~196k (tokens), not 512k.
func TestResolveContextLimitBackend(t *testing.T) {
	srv := limitsServer(t, `{"n_ctx":196608,"n_ctx_train":262144}`, "")
	defer srv.Close()

	cfg := config.DefaultConfig()
	cfg.BaseURL = srv.URL
	var out bytes.Buffer
	lim := agent.ResolveContextLimit(context.Background(), http.DefaultClient, cfg, &out)

	if !lim.FromBackend() {
		t.Fatalf("expected backend source, got %q", lim.Source)
	}
	if lim.NCtx != 196608 {
		t.Errorf("NCtx = %d, want 196608", lim.NCtx)
	}
	if lim.NCtxTrain != 262144 {
		t.Errorf("NCtxTrain = %d, want 262144", lim.NCtxTrain)
	}
	if lim.NCtx/1000 != 196 {
		t.Errorf("sidebar would show %dk, want 196k", lim.NCtx/1000)
	}
	if strings.Contains(out.String(), "fallback") {
		t.Errorf("backend success must not print a fallback note: %q", out.String())
	}
}

// TestResolveContextLimitProps: when only llama-server's /props passthrough is
// available, the per-slot n_ctx is read from default_generation_settings.
func TestResolveContextLimitProps(t *testing.T) {
	props := `{"default_generation_settings":{"n_ctx":196608},"total_slots":4,"n_ctx_train":262144}`
	srv := limitsServer(t, "", props)
	defer srv.Close()

	cfg := config.DefaultConfig()
	cfg.BaseURL = srv.URL
	var out bytes.Buffer
	lim := agent.ResolveContextLimit(context.Background(), http.DefaultClient, cfg, &out)

	if !lim.FromBackend() || lim.NCtx != 196608 {
		t.Fatalf("props path: source=%q NCtx=%d, want backend/196608", lim.Source, lim.NCtx)
	}
	if lim.NCtxTrain != 262144 {
		t.Errorf("NCtxTrain = %d, want 262144", lim.NCtxTrain)
	}
}

// TestResolveContextLimitFallback: an unreachable / erroring backend falls back
// to the configured ceiling and emits a loud note so it can't pass as truth.
func TestResolveContextLimitFallback(t *testing.T) {
	srv := limitsServer(t, "", "") // both endpoints 404
	defer srv.Close()

	cfg := config.DefaultConfig()
	cfg.BaseURL = srv.URL
	cfg.ContextTokensFallback = 131072
	var out bytes.Buffer
	lim := agent.ResolveContextLimit(context.Background(), http.DefaultClient, cfg, &out)

	if lim.FromBackend() {
		t.Fatalf("expected fallback source, got %q", lim.Source)
	}
	if lim.NCtx != 131072 {
		t.Errorf("fallback NCtx = %d, want 131072", lim.NCtx)
	}
	if !strings.Contains(out.String(), "⚠ using fallback context limit") {
		t.Errorf("missing loud fallback note; got: %q", out.String())
	}
}

// TestContextLimitUsable: the usable prompt budget subtracts the reasoning and
// answer headroom from n_ctx — a turn that thinks and answers can't also fill
// the window to the brim.
func TestContextLimitUsable(t *testing.T) {
	lim := agent.ContextLimit{NCtx: 196608, ReasoningBudget: 4096, AnswerMargin: 4096}
	if got, want := lim.Usable(), 196608-8192; got != want {
		t.Errorf("Usable() = %d, want %d", got, want)
	}
	// Reservations exceeding n_ctx clamp to a positive floor (no div-by-zero).
	tiny := agent.ContextLimit{NCtx: 1000, ReasoningBudget: 4096, AnswerMargin: 4096}
	if got := tiny.Usable(); got < 1 {
		t.Errorf("Usable() = %d, want >= 1 (clamped)", got)
	}
}

// TestParseContextLimitJSON covers both wire shapes and the integer/float
// tolerance of the number parser.
func TestParseContextLimitJSON(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantCtx   int
		wantTrain int
	}{
		{"flat", `{"n_ctx":196608,"n_ctx_train":262144}`, 196608, 262144},
		{"nested", `{"default_generation_settings":{"n_ctx":4096,"n_ctx_train":8192}}`, 4096, 8192},
		{"float", `{"n_ctx":196608.0}`, 196608, 0},
		{"empty", `{}`, 0, 0},
		{"garbage", `not json`, 0, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			nc, nt := agent.ParseContextLimitJSON([]byte(c.body))
			if nc != c.wantCtx || nt != c.wantTrain {
				t.Errorf("parse(%s) = (%d,%d), want (%d,%d)", c.body, nc, nt, c.wantCtx, c.wantTrain)
			}
		})
	}
}

// TestSidebarReadsBackendNCtx: the hist/ctx panel denominator reflects the
// fetched n_ctx (~196k), not the old hardcoded 512k.
func TestSidebarReadsBackendNCtx(t *testing.T) {
	app := &agent.App{
		Cfg:      config.DefaultConfig(),
		Client:   newTestClient(""),
		CtxLimit: agent.ContextLimit{NCtx: 196608, Source: "backend", ReasoningBudget: 4096, AnswerMargin: 4096},
		Conv:     []proxy.Message{{Role: "user", Content: strPtr("hi")}},
	}
	app.Client.SetUsage(proxy.UsageStat{InputTok: 48000, Exact: true})

	m := tuiModel{app: app}
	block := plain(m.renderContextBlock(m.contextBlockWidth()))
	if !strings.Contains(block, "196k") {
		t.Errorf("ctx panel must show 196k denominator; got: %q", block)
	}
	if strings.Contains(block, "512k") {
		t.Errorf("ctx panel must not show the old 512k ceiling; got: %q", block)
	}
	if !strings.Contains(block, "48k") {
		t.Errorf("ctx panel should show 48k used (from prompt_tokens); got: %q", block)
	}
}

// TestWarnContextPressure: a measured occupancy above the usable budget emits a
// single warning keyed off the usable number (not raw n_ctx), re-arming only
// after occupancy drops back under the budget.
func TestWarnContextPressure(t *testing.T) {
	var out bytes.Buffer
	app := &agent.App{
		Cfg:      config.DefaultConfig(),
		Client:   newTestClient(""),
		Out:      &out,
		CtxLimit: agent.ContextLimit{NCtx: 196608, Source: "backend", ReasoningBudget: 4096, AnswerMargin: 4096},
	}
	usable := app.CtxLimit.Usable() // 188416

	// Below usable → no warning.
	app.Client.SetUsage(proxy.UsageStat{InputTok: int64(usable - 1000), Exact: true})
	app.WarnContextPressure()
	if out.Len() != 0 {
		t.Fatalf("no warning expected under usable budget; got: %q", out.String())
	}

	// Crossing usable → exactly one warning.
	app.Client.SetUsage(proxy.UsageStat{InputTok: int64(usable + 5000), Exact: true})
	app.WarnContextPressure()
	app.WarnContextPressure() // still over → must not repeat
	if got := strings.Count(out.String(), "context pressure"); got != 1 {
		t.Fatalf("want exactly one pressure warning, got %d: %q", got, out.String())
	}
	if !strings.Contains(out.String(), "196k") {
		t.Errorf("warning should reference n_ctx 196k; got: %q", out.String())
	}

	// Estimate-only occupancy (no usage reported) must not warn.
	out.Reset()
	app.CtxPressureWarned = false
	app.Client.SetUsage(proxy.UsageStat{}) // InputTok 0 → estimate path
	app.Conv = []proxy.Message{{Role: "user", Content: strPtr(strings.Repeat("x", 10))}}
	app.WarnContextPressure()
	if out.Len() != 0 {
		t.Errorf("must not warn on estimate-only occupancy; got: %q", out.String())
	}
}

// streamResetServer drops the connection mid-SSE on the first call (simulating a
// backend stream reset / truncated body) and returns okFrames on later calls.
func streamResetServer(t *testing.T, okFrames []string) *httptest.Server {
	t.Helper()
	call := 0
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := call
		call++
		if n == 0 {
			hj, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("ResponseWriter is not a Hijacker")
			}
			conn, bufrw, err := hj.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			// Announce a 256-byte chunk but write only a few bytes, then close —
			// the client transport sees an unexpected EOF mid-chunk.
			bufrw.WriteString("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")
			bufrw.WriteString("100\r\n")
			bufrw.WriteString("data: partial")
			bufrw.Flush()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range okFrames {
			fmt.Fprintf(w, "data: %s\n\n", f)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// TestStreamResetClassified: a mid-stream connection drop surfaces as
// errBackendStream, not a raw read error.
func TestStreamResetClassified(t *testing.T) {
	srv := streamResetServer(t, []string{contentChunk("ok")})
	defer srv.Close()

	_, err := newTestClient(srv.URL).Stream(context.Background(), nil, nil, nil, nil)
	if err == nil {
		t.Fatal("expected a stream error, got nil")
	}
	if !errors.Is(err, proxy.ErrBackendStream) {
		t.Errorf("error not classified as backend stream error: %v", err)
	}
}

// TestHandleStreamErrorRetriesInAutoMode: in unattended (auto-approve) mode a
// stream error is retried regardless of workflow phase; a successful retry
// clears the error.
func TestHandleStreamErrorRetriesInAutoMode(t *testing.T) {
	srv := streamResetServer(t, []string{contentChunk("recovered")})
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.AutoApprove = true
	app.RetryDelay = func(_ int) time.Duration { return 0 } // no backoff in tests

	_, err := app.Send(context.Background(), "go")
	if !errors.Is(err, proxy.ErrBackendStream) {
		t.Fatalf("first send should report a stream error, got: %v", err)
	}
	err = agent.HandleStreamError(context.Background(), app, err)
	if err != nil {
		t.Errorf("retry should have recovered, got: %v", err)
	}
}

// TestHandleStreamErrorNoRetryInteractive: in interactive non-auto mode the
// stream error is passed through unchanged — a human can re-send.
func TestHandleStreamErrorNoRetryInteractive(t *testing.T) {
	srv := streamResetServer(t, []string{contentChunk("recovered")})
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	// Default: AutoApprove=false, IsHeadless=false → interactive, no retry.
	_, err := app.Send(context.Background(), "go")
	if !errors.Is(err, proxy.ErrBackendStream) {
		t.Fatalf("send should report a stream error, got: %v", err)
	}
	got := agent.HandleStreamError(context.Background(), app, err)
	if !errors.Is(got, proxy.ErrBackendStream) {
		t.Errorf("interactive mode should pass the stream error through, got: %v", got)
	}
}
