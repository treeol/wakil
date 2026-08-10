package agent

// Tests for card #126 Phase 2: runMashuraCore async path emits live
// AsyncJobChunkMsg progress events (per panel member) into the async-job tab,
// while synchronous call sites (registry-full fallback) emit none.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
)

// newAnthropicFormEndpoint returns a fake Anthropic-format oracle endpoint.
func newAnthropicFormEndpoint(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
}

// TestMashuraAsyncEmitsChunks verifies an async Mashūra panel emits
// AsyncJobChunkMsg events (routed by opID) and that they all precede the
// AsyncJobDoneMsg for the same op.
func TestMashuraAsyncEmitsChunks(t *testing.T) {
	os.Setenv("MASHURA_TEST_KEY", "x")
	defer os.Unsetenv("MASHURA_TEST_KEY")

	srv := newAnthropicFormEndpoint(t)
	defer srv.Close()

	app, _ := mashuraTestApp(t, "f", "1. do it", "[step 1] done")
	app.Cfg.MashuraPanels = map[string]config.MashuraPanelConfig{
		"default": {
			Models: []string{"anthropic:claude-opus-4-8", "anthropic:claude-fable-5"},
			Mode:   "panel",
		},
	}
	app.Cfg.OracleEndpoint = srv.URL + "/v1/messages"
	app.Confirm = func(_, _, _ string, _ bool) bool { return true }
	// Register an origin chat id; chunk OriginChatID must match the op's
	// registered origin (bug fix: not a worker-time a.chatID() re-read).
	app.Client.ChatID = "origin-chat"

	col := &collectEvents{}
	app.EventSink = col.sink

	got := app.handleMashura(context.Background(), "mashura__review",
		tcArgs("mashura__review", `{"focus":"check"}`))
	var opID string
	if !strings.HasPrefix(got, "queued as op-") {
		t.Fatalf("expected async placeholder, got: %q", got)
	}
	opID = strings.TrimSpace(strings.Split(strings.TrimPrefix(got, "queued as "), " (")[0])

	waitAsyncOps(t, app)

	var chunks, dones, starts int
	var doneIdx = -1
	var chunkIdx []int
	wrongOrigin := false
	for i, e := range col.snapshot() {
		switch m := e.(type) {
		case AsyncJobStartMsg:
			if m.OpID == opID {
				starts++
			}
		case AsyncJobChunkMsg:
			if m.OpID == opID {
				chunks++
				chunkIdx = append(chunkIdx, i)
				if m.OriginChatID != "origin-chat" {
					wrongOrigin = true
				}
			}
		case AsyncJobDoneMsg:
			if m.OpID == opID {
				dones++
				doneIdx = i
			}
		}
	}
	if starts != 1 {
		t.Errorf("Start events = %d, want 1", starts)
	}
	if dones != 1 {
		t.Errorf("Done events = %d, want 1", dones)
	}
	if chunks < 2 {
		t.Errorf("Chunk events = %d, want ≥2 (one per member start+done)", chunks)
	}
	if wrongOrigin {
		t.Error("a Chunk carried a OriginChatID that diverged from the op's registered origin")
	}
	for _, ci := range chunkIdx {
		if doneIdx >= 0 && ci > doneIdx {
			t.Errorf("Chunk at index %d arrived after Done at %d", ci, doneIdx)
		}
	}
}

// TestMashuraSyncFallbackEmitsNoChunks verifies the registry-full synchronous
// fallback emits NO AsyncJobChunkMsg (chunks are async-job-tab telemetry only).
func TestMashuraSyncFallbackEmitsNoChunks(t *testing.T) {
	os.Setenv("MASHURA_TEST_KEY", "x")
	defer os.Unsetenv("MASHURA_TEST_KEY")

	srv := newAnthropicFormEndpoint(t)
	defer srv.Close()

	app, _ := mashuraTestApp(t, "f", "1. do it", "[step 1] done")
	app.Cfg.MashuraPanels = map[string]config.MashuraPanelConfig{
		"default": {Models: []string{"anthropic:claude-opus-4-8"}, Mode: "panel"},
	}
	app.Cfg.OracleEndpoint = srv.URL + "/v1/messages"
	app.Confirm = func(_, _, _ string, _ bool) bool { return true }

	// Force the synchronous fallback (registry stopping).
	app.asyncMu.Lock()
	app.asyncStopping = true
	app.asyncMu.Unlock()

	col := &collectEvents{}
	app.EventSink = col.sink

	got := app.handleMashura(context.Background(), "mashura__review",
		tcArgs("mashura__review", `{"focus":"check"}`))
	// With the registry stopping, the call must NOT return the async placeholder
	// (it ran synchronously). The "running synchronously" notice goes to a.Out
	// (io.Discard here), so assert only that it wasn't queued and no chunks fired.
	if strings.HasPrefix(got, "queued as op-") {
		t.Fatalf("expected synchronous fallback, got async placeholder: %q", got)
	}
	for _, e := range col.snapshot() {
		if _, ok := e.(AsyncJobChunkMsg); ok {
			t.Error("synchronous fallback emitted an AsyncJobChunkMsg (should be none)")
		}
	}
}
