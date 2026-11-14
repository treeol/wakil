package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/proxy"
)

// errorServer returns a test HTTP server whose handler is selected per-call via
// a slice of handlers. The last handler repeats for all subsequent calls.
func errorServer(t *testing.T, handlers ...http.HandlerFunc) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/chat/completions") {
			http.NotFound(w, r)
			return
		}
		idx := int(calls.Add(1)) - 1
		if idx >= len(handlers) {
			idx = len(handlers) - 1
		}
		handlers[idx](w, r)
	}))
}

// sseSuccess writes a valid SSE response with a single text chunk.
func sseSuccess(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

// http500 writes a plain 500 response.
func http500(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "backend temporarily unavailable", http.StatusInternalServerError)
}

// noDelay is used as app.retryDelay in all retry tests to skip the backoff.
func noDelay(_ int) time.Duration { return 0 }

func newResilienceApp(url string) *App {
	app := newTestApp(url, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.RetryDelay = noDelay
	app.Session = &Session{ChatID: "aaaa1111-0000-0000-0000-000000000001", Model: "ilm"}
	return app
}

// --- classification tests ---

func TestHandleStreamError_NilPassThrough(t *testing.T) {
	app := newResilienceApp("http://unused")
	if got := HandleStreamError(context.Background(), app, nil); got != nil {
		t.Errorf("nil error should pass through; got %v", got)
	}
}

func TestHandleStreamError_NonStreamPassThrough(t *testing.T) {
	app := newResilienceApp("http://unused")
	plainErr := errors.New("something else entirely")
	if got := HandleStreamError(context.Background(), app, plainErr); !errors.Is(got, plainErr) {
		t.Errorf("non-stream error should pass through unchanged")
	}
}

func TestHandleStreamError_FatalImmediateNoRetry(t *testing.T) {
	// 400 → ErrBackendFatal; must be returned immediately without calling Send again.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	app := newResilienceApp(srv.URL)
	app.SetAutoApprove(true)

	_, err := app.Send(context.Background(), "test task")
	if !errors.Is(err, proxy.ErrBackendFatal) {
		t.Fatalf("Send should return ErrBackendFatal for 400; got: %v", err)
	}
	before := calls.Load()
	result := HandleStreamError(context.Background(), app, err)
	after := calls.Load()

	if !errors.Is(result, proxy.ErrBackendFatal) {
		t.Errorf("HandleStreamError should return ErrBackendFatal unchanged; got: %v", result)
	}
	if after != before {
		t.Errorf("HandleStreamError should make no additional calls for fatal errors; made %d", after-before)
	}
}

// --- retry behaviour ---

func TestHandleStreamError_InteractiveNoRetry(t *testing.T) {
	// Non-auto interactive: the error enters the retry loop (Group 4 changed
	// the rule from "surface immediately"). Each attempt prints one line
	// naming the reason and the wait, and the exhausted message names the
	// last status and attempts.
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	var out strings.Builder
	app := newResilienceApp(srv.URL)
	app.SetAutoApprove(false)
	app.IsHeadless = false
	app.Cfg.BackendMaxRetries = 2
	app.Out = &writerAdapter{&out}

	_, err := app.Send(context.Background(), "hi")
	if !errors.Is(err, proxy.ErrBackendStream) {
		t.Fatalf("expected ErrBackendStream from 500; got: %v", err)
	}
	before := calls.Load()
	result := HandleStreamError(context.Background(), app, err)
	after := calls.Load()

	if !errors.Is(result, proxy.ErrBackendStream) {
		t.Errorf("exhausted interactive retries should still return the error; got: %v", result)
	}
	if after != before+2 {
		t.Errorf("interactive retries should make 2 attempts; made %d", after-before)
	}
	note := out.String()
	if !strings.Contains(note, "retrying in") || !strings.Contains(note, "attempt 1 of 2") {
		t.Errorf("per-attempt line must name the wait and attempt; got: %q", note)
	}
	if !strings.Contains(note, "gave up after 2 attempts") || !strings.Contains(note, "last status") || !strings.Contains(note, "Next step") {
		t.Errorf("final message must name the last status, attempts and a next step; got: %q", note)
	}
}

func TestHandleStreamError_AutoRetryRecovers(t *testing.T) {
	// 500 on first call, success on second → HandleStreamError returns nil.
	srv := errorServer(t, http500, sseSuccess)
	defer srv.Close()

	app := newResilienceApp(srv.URL)
	app.SetAutoApprove(true)
	app.Cfg.BackendMaxRetries = 3

	_, err := app.Send(context.Background(), "first task")
	if !errors.Is(err, proxy.ErrBackendStream) {
		t.Fatalf("expected ErrBackendStream from 500; got: %v", err)
	}

	result := HandleStreamError(context.Background(), app, err)
	if result != nil {
		t.Errorf("retry should have recovered; got: %v", result)
	}
	// Conv should have the retry user message + a response.
	if len(app.Conv) == 0 {
		t.Error("Conv should not be empty after recovery")
	}
}

func TestHandleStreamError_HeadlessRetryRecovers(t *testing.T) {
	// IsHeadless=true (not AutoApprove) also triggers retry.
	srv := errorServer(t, http500, sseSuccess)
	defer srv.Close()

	app := newResilienceApp(srv.URL)
	app.SetAutoApprove(false)
	app.IsHeadless = true
	app.Cfg.BackendMaxRetries = 3

	_, err := app.Send(context.Background(), "headless task")
	if !errors.Is(err, proxy.ErrBackendStream) {
		t.Fatalf("expected ErrBackendStream; got %v", err)
	}
	if got := HandleStreamError(context.Background(), app, err); got != nil {
		t.Errorf("headless retry should recover; got: %v", got)
	}
}

func TestHandleStreamError_ExhaustedRetries(t *testing.T) {
	// All calls return 500; after maxRetries HandleStreamError returns ErrBackendStream.
	srv := errorServer(t, http500, http500, http500, http500, http500)
	defer srv.Close()

	app := newResilienceApp(srv.URL)
	app.SetAutoApprove(true)
	app.Cfg.BackendMaxRetries = 3

	_, err := app.Send(context.Background(), "task")
	if !errors.Is(err, proxy.ErrBackendStream) {
		t.Fatalf("expected ErrBackendStream; got %v", err)
	}
	result := HandleStreamError(context.Background(), app, err)
	if !errors.Is(result, proxy.ErrBackendStream) {
		t.Errorf("exhausted retries should return ErrBackendStream; got: %v", result)
	}
}

func TestHandleStreamError_PersistentResetNote(t *testing.T) {
	// All retries fail with 500 (all-stream-errors path). Verify a "persistent"
	// note is written to app.Out.
	srv := errorServer(t, http500, http500, http500, http500)
	defer srv.Close()

	var out strings.Builder
	app := newResilienceApp(srv.URL)
	app.SetAutoApprove(true)
	app.Cfg.BackendMaxRetries = 2
	app.Out = &writerAdapter{&out}

	_, err := app.Send(context.Background(), "task")
	HandleStreamError(context.Background(), app, err)

	note := out.String()
	if !strings.Contains(note, "persistent") {
		t.Errorf("exhausted-all-stream-errors should emit persistent note; got: %q", note)
	}
}

// TestHandleStreamError_SessionSavedOnExhaustion verifies that the session is
// persisted (via SaveSession defer inside Send) even when all retries fail.
func TestHandleStreamError_SessionSavedOnExhaustion(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())

	srv := errorServer(t, http500, http500, http500, http500)
	defer srv.Close()

	const chatID = "aaaa1111-0000-0000-0000-000000000001"
	app := newResilienceApp(srv.URL)
	app.Client.ChatID = chatID
	app.Session = &Session{ChatID: chatID, Model: "ilm"}
	app.SetAutoApprove(true)
	app.Cfg.BackendMaxRetries = 2
	// Give the session a Conv entry so SaveSession actually writes.
	app.Conv = []proxy.Message{{Role: "user", Content: StrPtr("prior turn")}}

	_, err := app.Send(context.Background(), "task")
	HandleStreamError(context.Background(), app, err)

	got, loadErr := LoadSession("aaaa1111")
	if loadErr != nil {
		t.Fatalf("session not saved after exhausted retries: %v", loadErr)
	}
	if len(got.Conv) == 0 {
		t.Error("saved session Conv should not be empty")
	}
}

// writerAdapter wraps a *strings.Builder so it satisfies io.Writer.
type writerAdapter struct{ b *strings.Builder }

func (w *writerAdapter) Write(p []byte) (int, error) { return w.b.Write(p) }

// Compile-time interface check.
var _ io.Writer = (*writerAdapter)(nil)

// TestHandleStreamError_FirstAttemptUsesInitialError pins the
// post-review fix: the first retry's line and wait are computed from the
// INITIAL error, not from a nil slot — an initial 429 with no Retry-After
// still prints "rate limited" on attempt 1 and computes the 15s minimum.
// The wall is kept off the test clock via a short-deadline ctx (the wait
// selects ctx.Done), so the real 15s never elapses.
func TestHandleStreamError_FirstAttemptUsesInitialError(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer srv.Close()

	var out strings.Builder
	app := newResilienceApp(srv.URL)
	app.RetryDelay = nil
	app.SetAutoApprove(false)
	app.Cfg.BackendMaxRetries = 1
	app.Out = &writerAdapter{&out}

	_, err := app.Send(context.Background(), "hi")
	if !errors.Is(err, proxy.ErrBackendStream) {
		t.Fatalf("first Send should return ErrBackendStream; got: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_ = HandleStreamError(ctx, app, err)

	note := out.String()
	if !strings.Contains(note, "rate limited") {
		t.Errorf("first attempt line must name rate limited; got: %q", note)
	}
}

// TestHandleStreamError_FatalMidRetryStopsLoop pins the post-review fix: a
// retry returning a fatal error (500 → 400) stops the loop — no third
// request.
func TestHandleStreamError_FatalMidRetryStopsLoop(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()

	// noDelay override keeps the retry loop fast (waits are 0s); the
	// fatal-stop assertion is about the loop breaking, not the timing.
	app := newResilienceApp(srv.URL)
	app.SetAutoApprove(true)
	app.Cfg.BackendMaxRetries = 3

	_, err := app.Send(context.Background(), "task")
	if !errors.Is(err, proxy.ErrBackendStream) {
		t.Fatalf("first Send should be ErrBackendStream; got: %v", err)
	}
	result := HandleStreamError(context.Background(), app, err)
	if !errors.Is(result, proxy.ErrBackendFatal) {
		t.Errorf("fatal mid-retry should surface ErrBackendFatal; got: %v", result)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("500 → 400 must make exactly 2 calls (no third request); made %d", got)
	}
}
