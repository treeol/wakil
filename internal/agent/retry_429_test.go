package agent

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// http429 writes a 429 Too Many Requests (rate limited).
func http429(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "rate limited", http.StatusTooManyRequests)
}

// TestHandleStreamError_429RetriedInUnattendedMode: a 429 must classify as
// ErrBackendStream and enter the retry loop (recovering when the next attempt
// succeeds) — previously it was fatal and aborted immediately.
func TestHandleStreamError_429RetriedInUnattendedMode(t *testing.T) {
	srv := errorServer(t, http429, sseSuccess)
	defer srv.Close()

	app := newResilienceApp(srv.URL)
	app.AutoApprove = true
	app.Cfg.BackendMaxRetries = 3

	_, err := app.Send(context.Background(), "task")
	if !errors.Is(err, proxy.ErrBackendStream) {
		t.Fatalf("429 should be ErrBackendStream (retryable); got: %v", err)
	}
	if errors.Is(err, proxy.ErrBackendFatal) {
		t.Fatal("429 must not be ErrBackendFatal")
	}

	if result := HandleStreamError(context.Background(), app, err); result != nil {
		t.Errorf("retry should recover on second attempt; got: %v", result)
	}
}
