package agent

// ASSIST-1 call-count replay test: a 4-turn recorded session asserts 4 assist
// calls and 4 assist_events, including one timeout and one 400.
//
// The stub assist server serves one canned response per call:
//   Turn 1: action (read_file, gate=0.85) → decision "auto" (assist_auto=true)
//   Turn 2: abstain → decision "abstain"
//   Turn 3: timeout (2s delay > 800ms client timeout) → decision "error"
//   Turn 4: 400 (server rejects seq) → decision "rejected"
//
// The model server returns text-only responses (no tool calls), so each turn
// is exactly 1 iteration of the streamTurn loop. This verifies the iter==0
// gate: assist is called once per turn, not once per iteration.
//
// After 4 turns, we read the emitter's queue file and count assist_event
// entries. We also verify the stub server received exactly 4 /v1/assist calls.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/ilm"
	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// splitLines splits JSONL data into lines (helper for reading the queue file
// from outside the ilm package).
func splitLines(data []byte) [][]byte {
	return bytes.Split(data, []byte("\n"))
}

// stubAssistServerAgent is a minimal stub for the agent test. It serves one
// canned response per call and counts calls. Unlike the ilm package's
// stubAssistServer, it can also return 400.
type stubAssistServerAgent struct {
	*httptest.Server
	calls int32
}

func newStubAssistServerAgent(t *testing.T, handler http.HandlerFunc) *stubAssistServerAgent {
	s := &stubAssistServerAgent{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/assist" {
			http.NotFound(w, r)
			return
		}
		atomic.AddInt32(&s.calls, 1)
		handler(w, r)
	}))
	return s
}

// makeActionRespJSON builds a raw assist action response JSON.
func makeActionRespJSON(tool string, args map[string]interface{}, gate float64) []byte {
	action, _ := json.Marshal(map[string]interface{}{
		"b":    "tool",
		"name": tool,
		"args": args,
	})
	resp := map[string]interface{}{
		"action":               json.RawMessage(action),
		"abstain":              false,
		"gate_probability":     gate,
		"decider_id":           "d-wlm",
		"threshold":            0.76,
		"latency_ms":           42.5,
		"candidate_provenance": map[string]interface{}{"candidates": []interface{}{}},
	}
	b, _ := json.Marshal(resp)
	return b
}

func makeAbstainRespJSON(reason string, gate float64) []byte {
	resp := map[string]interface{}{
		"action":               nil,
		"abstain":              true,
		"reason":               reason,
		"gate_probability":     gate,
		"decider_id":           "d-wlm",
		"threshold":            0.76,
		"latency_ms":           12.3,
		"candidate_provenance": map[string]interface{}{"candidates": []interface{}{}},
	}
	b, _ := json.Marshal(resp)
	return b
}

// TestAssistReplay4Turns is the C5 replay test at the agent level: a 4-turn
// session must produce exactly 4 assist calls and 4 assist_events, including
// one timeout and one 400.
func TestAssistReplay4Turns(t *testing.T) {
	// Build the 4 canned responses. We use an atomic counter to serve one per
	// call, cycling through action → abstain → timeout → 400.
	var respIdx int32
	assistSrv := newStubAssistServerAgent(t, func(w http.ResponseWriter, r *http.Request) {
		idx := atomic.AddInt32(&respIdx, 1)
		w.Header().Set("Content-Type", "application/json")
		switch idx {
		case 1: // action (read_file, gate=0.85)
			w.Write(makeActionRespJSON("read_file", map[string]interface{}{"path": "test.go"}, 0.85))
		case 2: // abstain
			w.Write(makeAbstainRespJSON("gate_probability < threshold", 0.41))
		case 3: // timeout (2s delay > 800ms client timeout)
			time.Sleep(2 * time.Second)
			w.Write(makeActionRespJSON("read_file", map[string]interface{}{"path": "x"}, 0.9))
		case 4: // 400 (server rejects seq)
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":"seq is not a decision point"}`))
		default:
			// Past the end: abstain to stop the loop.
			w.Write(makeAbstainRespJSON("no more responses", 0.0))
		}
	})
	defer assistSrv.Server.Close()

	// Model server: returns text-only responses (no tool calls). Each turn
	// is exactly 1 iteration — the model produces final text, no tool loop.
	modelSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		flusher.Flush()
		fmt.Fprintf(w, "data: %s\n\n", contentChunk("ok"))
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer modelSrv.Close()

	// Create an emitter in assist mode with a temp queue file.
	queuePath := filepath.Join(t.TempDir(), "queue.jsonl")
	emitter, err := ilm.New(ilm.Config{
		Endpoint:  "http://127.0.0.1:1", // black-hole — events land in queue
		Token:     "test",
		Mode:      ilm.ModeAssist,
		QueuePath: queuePath,
		BatchMS:   500,
	}, "wakil-live:replay-test")
	if err != nil {
		t.Fatalf("ilm.New: %v", err)
	}
	defer emitter.Close()

	// Create the App with assist wired up.
	assistClient := ilm.NewAssistClient(assistSrv.URL, "test-token")
	cfg := config.DefaultConfig()
	cfg.ShellTimeoutSec = 0
	app := &App{
		Cfg:           cfg,
		Client:        &proxy.Client{BaseURL: modelSrv.URL, Model: "test", ChatID: "replay", HTTP: http.DefaultClient},
		Exec:          newFakeExecutor(),
		Tools:         wtools.DefaultTools("/work"),
		Out:           io.Discard,
		ILM:           emitter,
		Assist:        assistClient,
		AssistEnabled: true,
		AssistAuto:    true,
	}

	// Send 4 user messages — each produces one assistant turn with no tool
	// calls (text-only model response), so each turn is 1 iteration.
	for i := 0; i < 4; i++ {
		_, err := app.Send(context.Background(), fmt.Sprintf("turn %d", i+1))
		if err != nil {
			t.Fatalf("turn %d: Send failed: %v", i+1, err)
		}
	}

	// Wait for events to flush to the queue (channel → queue on failed POST).
	time.Sleep(300 * time.Millisecond)
	emitter.Close()

	// Read the queue file directly (queue is unexported, but Event is exported).
	// The emitter's black-hole endpoint ensures all events land in the queue.
	raw, err := os.ReadFile(queuePath)
	if err != nil {
		t.Fatalf("read queue file: %v", err)
	}
	var assistEvents []ilm.Event
	for _, line := range splitLines(raw) {
		if len(line) == 0 {
			continue
		}
		var ev ilm.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue // skip malformed
		}
		if ev.Type == ilm.EventAssist {
			assistEvents = append(assistEvents, ev)
		}
	}

	// Assert exactly 4 assist_events (one per turn).
	if len(assistEvents) != 4 {
		t.Fatalf("expected 4 assist_events, got %d", len(assistEvents))
	}

	// Assert the decisions in order: auto, abstain, error, rejected.
	expectedDecisions := []string{"auto", "abstain", "error", "rejected"}
	for i, ev := range assistEvents {
		var p ilm.AssistEventPayload
		if err := json.Unmarshal(ev.Payload, &p); err != nil {
			t.Fatalf("event %d: unmarshal payload: %v", i+1, err)
		}
		if p.Decision != expectedDecisions[i] {
			t.Errorf("event %d: expected decision %q, got %q", i+1, expectedDecisions[i], p.Decision)
		}
	}

	// Assert the stub server received exactly 4 calls.
	if got := atomic.LoadInt32(&assistSrv.calls); got != 4 {
		t.Errorf("expected 4 assist server calls, got %d", got)
	}

	t.Logf("4 turns → 4 assist calls, 4 assist_events: %v", expectedDecisions)
}
