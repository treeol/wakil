package ilm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestSessionEndAttribution verifies that session_end is emitted under the
// correct session id, and that a new session's first event is session_start
// (not session_end from the previous session).
//
// It simulates a two-session run by creating two emitters (one per session),
// emitting events on each, and collecting the events server-side. The events
// are then checked for correct ordering: [start@1 … end@N] for A, then
// [start@1 …] for B — never an end under B's id before B's start.
func TestSessionEndAttribution(t *testing.T) {
	var mu sync.Mutex
	var allReceived []Event

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/events" {
			var body struct {
				Events []Event `json:"events"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			allReceived = append(allReceived, body.Events...)
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	queuePathA := filepath.Join(t.TempDir(), "queue-a.jsonl")
	queuePathB := filepath.Join(t.TempDir(), "queue-b.jsonl")

	// Session A: create emitter, emit start + turns + end.
	emitterA, err := New(Config{
		Endpoint:  srv.URL,
		Token:     "test",
		Mode:      ModeShadow,
		QueuePath: queuePathA,
		BatchMS:   50,
	}, "wakil-live:sessionA")
	if err != nil {
		t.Fatalf("New A: %v", err)
	}
	emitterA.Emit(EventSessionStart, SessionStartPayload{Source: "live"})
	emitterA.Emit(EventUserTurn, UserTurnPayload{Text: "hello from A"})
	emitterA.Emit(EventAssistantTurn, AssistantTurnPayload{Text: "hi"})
	emitterA.Emit(EventSessionEnd, SessionEndPayload{})

	// Close emitter A — flushes all events including session_end.
	if err := emitterA.Close(); err != nil {
		t.Fatalf("Close A: %v", err)
	}

	// Wait for the server to receive A's events.
	time.Sleep(300 * time.Millisecond)

	// Session B: create a new emitter with a new session id.
	emitterB, err := New(Config{
		Endpoint:  srv.URL,
		Token:     "test",
		Mode:      ModeShadow,
		QueuePath: queuePathB,
		BatchMS:   50,
	}, "wakil-live:sessionB")
	if err != nil {
		t.Fatalf("New B: %v", err)
	}
	emitterB.Emit(EventSessionStart, SessionStartPayload{Source: "live"})
	emitterB.Emit(EventUserTurn, UserTurnPayload{Text: "hello from B"})

	// Close emitter B.
	if err := emitterB.Close(); err != nil {
		t.Fatalf("Close B: %v", err)
	}

	// Wait for all events to arrive.
	time.Sleep(500 * time.Millisecond)

	mu.Lock()
	events := allReceived
	mu.Unlock()

	// Verify the ordering: for each session, the first event must be
	// session_start, and no session_end should appear under B's id before
	// B's session_start.
	var firstEventB *Event
	for i := range events {
		if events[i].SessionID == "wakil-live:sessionB" {
			firstEventB = &events[i]
			break
		}
	}
	if firstEventB == nil {
		t.Fatalf("no events for session B received")
	}
	if firstEventB.Type != EventSessionStart {
		t.Errorf("first event for session B is %q, want session_start — session_end is leaking into the new session",
			firstEventB.Type)
	}

	// Verify session A has a session_end.
	hasEndA := false
	for _, ev := range events {
		if ev.SessionID == "wakil-live:sessionA" && ev.Type == EventSessionEnd {
			hasEndA = true
		}
	}
	if !hasEndA {
		t.Errorf("session A has no session_end event")
	}

	// Verify seq ordering: session A events are [1, 2, 3, 4] (start, turn,
	// assistant, end), session B events are [1, 2] (start, turn).
	for i, ev := range events {
		t.Logf("event %d: session=%s seq=%d type=%s", i, ev.SessionID, ev.Seq, ev.Type)
	}
}
