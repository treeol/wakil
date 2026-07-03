package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/proxy"
)

// noDelay is an agent.RetryDelay override that eliminates backoff in tests.
var noDelay = func(_ int) time.Duration { return 0 }

// errorHandlers returns a handler that cycles through hs per call (repeating the last).
func cycleHandlers(hs ...http.HandlerFunc) http.HandlerFunc {
	var calls atomic.Int32
	return func(w http.ResponseWriter, r *http.Request) {
		idx := int(calls.Add(1)) - 1
		if idx >= len(hs) {
			idx = len(hs) - 1
		}
		hs[idx](w, r)
	}
}

func http500h(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "backend temporarily unavailable", http.StatusInternalServerError)
}

func http400h(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "invalid request body", http.StatusBadRequest)
}

func sseSuccessH(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func newHeadlessApp(url string) *agent.App {
	app := newTestApp(url, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.IsHeadless = true
	app.RetryDelay = noDelay
	const chatID = "aaaa1111-0000-0000-0000-000000000099"
	app.Client.ChatID = chatID
	app.Session = &agent.Session{ChatID: chatID, Model: "ilm"}
	return app
}

// TestHeadless_500_ExhaustedRetries_ExitBackendFailure: all calls return 500,
// retries exhaust → ExitBackendFailure with backend_failure outcome event and
// a resume_id so a wrapper can detect and resume.
func TestHeadless_500_ExhaustedRetries_ExitBackendFailure(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())

	srv := httptest.NewServer(cycleHandlers(http500h))
	defer srv.Close()

	app := newHeadlessApp(srv.URL)
	app.Cfg.BackendMaxRetries = 2

	var out strings.Builder
	code := runHeadlessApp(context.Background(), app, "task", false, RunFlags{Auto: true}, &out)

	if code != ExitBackendFailure {
		t.Errorf("want ExitBackendFailure (%d), got %d; output: %s", ExitBackendFailure, code, out.String())
	}

	// The outcome event must be backend_failure with a resume_id.
	found := false
	for _, line := range strings.Split(out.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev["type"] == "done" && ev["outcome"] == "backend_failure" {
			if _, ok := ev["resume_id"]; !ok {
				t.Error("backend_failure event should include resume_id")
			}
			found = true
		}
	}
	if !found {
		t.Errorf("no backend_failure done event found in output:\n%s", out.String())
	}
}

// TestHeadless_500_ThenRecover_ExitOK: first call returns 500, retry succeeds
// → HandleStreamError returns nil → exit 0.
func TestHeadless_500_ThenRecover_ExitOK(t *testing.T) {
	srv := httptest.NewServer(cycleHandlers(http500h, sseSuccessH))
	defer srv.Close()

	app := newHeadlessApp(srv.URL)
	app.Cfg.BackendMaxRetries = 3

	var out strings.Builder
	code := runHeadlessApp(context.Background(), app, "task", false, RunFlags{Auto: true}, &out)

	if code != ExitOK {
		t.Errorf("want ExitOK (%d) after recovery, got %d; output: %s", ExitOK, code, out.String())
	}
}

// TestHeadless_4xx_ExitError_NoRetry: 400 is fatal — must not retry and must
// exit with ExitError (not ExitBackendFailure — not a transient issue).
func TestHeadless_4xx_ExitError_NoRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http400h(w, r)
	}))
	defer srv.Close()

	app := newHeadlessApp(srv.URL)
	app.Cfg.BackendMaxRetries = 3

	var out strings.Builder
	code := runHeadlessApp(context.Background(), app, "task", false, RunFlags{Auto: true}, &out)

	if code == ExitBackendFailure {
		t.Errorf("4xx should not produce ExitBackendFailure (it's deterministic); got code %d", code)
	}
	if code == ExitOK {
		t.Errorf("4xx should not succeed; got ExitOK")
	}
	// Exactly one HTTP call: no retries.
	if n := calls.Load(); n != 1 {
		t.Errorf("4xx should make exactly 1 HTTP call; made %d", n)
	}
}

// TestHeadless_500_SessionSaved verifies session persistence after exhausted retries.
func TestHeadless_500_SessionSaved(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", t.TempDir())

	srv := httptest.NewServer(cycleHandlers(http500h))
	defer srv.Close()

	const chatID = "bbbb2222-0000-0000-0000-000000000001"
	app := newHeadlessApp(srv.URL)
	app.Client.ChatID = chatID
	app.Session = &agent.Session{ChatID: chatID, Model: "ilm"}
	app.Cfg.BackendMaxRetries = 1

	var out strings.Builder
	runHeadlessApp(context.Background(), app, "task", false, RunFlags{Auto: true}, &out)

	_, loadErr := agent.LoadSession("bbbb2222")
	if loadErr != nil {
		t.Errorf("session should be saved after exhausted retries: %v", loadErr)
	}
}

// TestErrBackendFatal_Classification confirms that proxy.ErrBackendFatal is
// what we detect for 4xx, so the exit-code logic (errors.Is) is correct.
func TestErrBackendFatal_Classification(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "context too long", http.StatusUnprocessableEntity)
	}))
	defer srv.Close()

	app := newHeadlessApp(srv.URL)
	_, err := app.Send(context.Background(), "hi")
	if !errors.Is(err, proxy.ErrBackendFatal) {
		t.Errorf("422 should yield ErrBackendFatal; got: %v", err)
	}
	if errors.Is(err, proxy.ErrBackendStream) {
		t.Error("ErrBackendFatal must not match ErrBackendStream")
	}
}
