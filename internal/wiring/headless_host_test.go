package wiring

// Chunk-7 headless re-route integration tests (plan §Tests). They drive
// runSingleTask — the single-task host path — against a fake SSE backend and
// assert the JSON projection + exit codes + decline-reason-as-data + recovered
// committed text. Helpers (fakeApp/sseServer/contentChunk/toolCallFrames) are
// shared with hostturn_test.go in this package.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/proxy"
)

// noDelay is a zero-backoff RetryDelay override for retry-path tests.
var noDelay = func(_ int) time.Duration { return 0 }

// outputEvents parses the JSON-lines transcript into a slice of maps.
func outputEvents(t *testing.T, out string) []map[string]any {
	t.Helper()
	var evs []map[string]any
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("bad JSON line %q: %v", line, err)
		}
		evs = append(evs, ev)
	}
	return evs
}

// findEvent returns the first event with the given "type" value, or nil.
func findEvent(evs []map[string]any, typ string) map[string]any {
	for _, ev := range evs {
		if ev["type"] == typ {
			return ev
		}
	}
	return nil
}

// cyclingSSEServer returns an SSE server whose i-th call returns the i-th frame
// set (repeating the last), and which returns 500 for calls marked by the
// failOn map (call index → true).
func cyclingSSEServer(t *testing.T, failOn map[int]bool, frames ...[]string) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		idx := int(calls.Add(1)) - 1
		if failOn[idx] {
			http.Error(w, "backend temporarily unavailable", http.StatusInternalServerError)
			return
		}
		f := frames[0]
		if idx < len(frames) {
			f = frames[idx]
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, fr := range f {
			fmt.Fprintf(w, "data: %s\n\n", fr)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// TestHeadlessHostSimpleTask: a content-only turn re-routed through the host
// projects the same JSON shape and exits 0.
func TestHeadlessHostSimpleTask(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("Task complete!")})
	defer srv.Close()

	app := fakeApp(srv.URL)
	app.Costs = proxy.NewCostTracker()

	var out strings.Builder
	code, _ := runSingleTask(context.Background(), app, "print hello", HeadlessOptions{Auto: true}, &out)

	if code != ExitOK {
		t.Fatalf("exit = %d, want %d; output: %s", code, ExitOK, out.String())
	}
	evs := outputEvents(t, out.String())
	done := findEvent(evs, "done")
	if done == nil || done["outcome"] != "pass" {
		t.Errorf("want done{pass}; got %v", done)
	}
	// tokens must be the last event (matches legacy ordering).
	if evs[len(evs)-1]["type"] != "tokens" {
		t.Errorf("last event should be tokens; got %v", evs[len(evs)-1])
	}
}

// TestHeadlessHostDestructiveDeclined: a destructive run_shell is declined by
// the resolver and the decline reason travels in ApprovalResolved → projected
// into done{declined, reason}. Exit 1.
func TestHeadlessHostDestructiveDeclined(t *testing.T) {
	srv := sseServer(t,
		toolCallFrames("c1", "run_shell", `{"command":"rm -rf /tmp/test"}`),
		[]string{contentChunk("cleaned up")},
	)
	defer srv.Close()

	app := fakeApp(srv.URL)

	var out strings.Builder
	code, _ := runSingleTask(context.Background(), app, "clean tmp",
		HeadlessOptions{Auto: true, AllowDestructive: false}, &out)

	if code != ExitDeclined {
		t.Fatalf("exit = %d, want %d; output: %s", code, ExitDeclined, out.String())
	}
	evs := outputEvents(t, out.String())
	done := findEvent(evs, "done")
	if done == nil || done["outcome"] != "declined" {
		t.Fatalf("want done{declined}; got %v", done)
	}
	reason, _ := done["reason"].(string)
	if !strings.Contains(reason, "destructive") {
		t.Errorf("decline reason %q should mention 'destructive'", reason)
	}
}

// TestHeadlessHostRecoveredTextCommitted: the first backend call fails 500 and
// the retry succeeds — the recovered text (not the failed attempt's empty
// outcome) must be the committed message text. This is the D17 correctness
// proof: HandleStreamError retries internally and previously discarded the
// recovered TurnOutcome.
func TestHeadlessHostRecoveredTextCommitted(t *testing.T) {
	const recovered = "recovered after retry"
	srv := cyclingSSEServer(t,
		map[int]bool{0: true}, // first call → 500
		[]string{contentChunk("first attempt")},
		[]string{contentChunk(recovered)},
	)
	defer srv.Close()

	app := fakeApp(srv.URL)
	app.RetryDelay = noDelay
	app.Cfg.BackendMaxRetries = 1
	app.IsHeadless = true // enables retry in HandleStreamError (same as production headless)

	var out strings.Builder
	code, _ := runSingleTask(context.Background(), app, "go", HeadlessOptions{Auto: true}, &out)

	if code != ExitOK {
		t.Fatalf("exit = %d, want %d; output: %s", code, ExitOK, out.String())
	}

	// The committed text is the last assistant message, which after the retry
	// is the recovered chunk (not the failed first attempt).
	if got := agent.DerefStr(app.Conv[len(app.Conv)-1].Content); got != recovered {
		t.Errorf("committed text = %q, want %q", got, recovered)
	}
}

// TestHeadlessTokenLimitSecondCut: cut, cut — the headless run exits
// ExitIncomplete with a done{token_limit, message} record naming the
// endpoint, model and max_tokens, and --result-json maps it to
// exit_reason="token_limit". The first partial text stays in the session.
func TestHeadlessTokenLimitSecondCut(t *testing.T) {
	srv := sseServer(t,
		[]string{`{"choices":[{"delta":{"content":"first half"},"finish_reason":null}]}`, `{"choices":[{"delta":{},"finish_reason":"length"}]}`},
		[]string{`{"choices":[{"delta":{"content":"second half"},"finish_reason":null}]}`, `{"choices":[{"delta":{},"finish_reason":"length"}]}`},
	)
	defer srv.Close()

	app := fakeApp(srv.URL)
	mt := 4096
	app.Client.MaxTokens = &mt
	app.Cfg.EndpointName = "test-ep"

	var out strings.Builder
	code, outcome := runSingleTask(context.Background(), app, "hi", HeadlessOptions{Auto: true}, &out)

	if code != ExitIncomplete {
		t.Fatalf("exit = %d, want %d; output: %s", code, ExitIncomplete, out.String())
	}
	if outcome != "token_limit" {
		t.Fatalf("outcome = %q, want token_limit", outcome)
	}
	evs := outputEvents(t, out.String())
	done := findEvent(evs, "done")
	if done == nil || done["outcome"] != "token_limit" {
		t.Fatalf("want done{token_limit}; got %v", done)
	}
	msg, _ := done["message"].(string)
	for _, want := range []string{"test-ep", "ilm", "4096", "endpoints.test-ep.max_tokens"} {
		if !strings.Contains(msg, want) {
			t.Errorf("token_limit message %q missing %q", msg, want)
		}
	}
	if !strings.Contains(out.String(), "first half") {
		t.Errorf("first partial text not streamed to output: %s", out.String())
	}
	// --result-json maps the code+outcome to exit_reason="token_limit".
	p := t.TempDir() + "/result.json"
	if err := writeResultJSON(p, app, code, outcome, time.Now(), "/tmp/ws", "direct"); err != nil {
		t.Fatalf("writeResultJSON: %v", err)
	}
	res, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if !strings.Contains(string(res), `"exit_reason":"token_limit"`) {
		t.Errorf("result exit_reason not token_limit: %s", res)
	}
	// The first partial text is in the saved conversation.
	found := false
	for _, m := range app.Conv {
		if m.Role == "assistant" && m.Content != nil && *m.Content == "first half" {
			found = true
		}
	}
	if !found {
		t.Errorf("first partial text not in Conv: %v", app.Conv)
	}
}
