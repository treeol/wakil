package counsel

// Tests for card #126 Phase 2: the optional OnMemberEvent progress observer.

import (
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
)

// collectEvents gathers PanelMemberEvent in order (mutex-guarded).
type panelEventCollector struct {
	mu     sync.Mutex
	events []PanelMemberEvent
}

func (c *panelEventCollector) add(ev PanelMemberEvent) {
	c.mu.Lock()
	c.events = append(c.events, ev)
	c.mu.Unlock()
}

func (c *panelEventCollector) snapshot() []PanelMemberEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	cp := make([]PanelMemberEvent, len(c.events))
	copy(cp, c.events)
	return cp
}

func (c *panelEventCollector) sink() func(PanelMemberEvent) {
	return func(ev PanelMemberEvent) { c.add(ev) }
}

// TestRunPanelObserver_PanelMode verifies panel mode fires exactly one Start and
// one Done/Error per member. Inter-member ordering is intentionally
// nondeterministic (parallel goroutines) — only each member's own start→terminal
// order and event totals are asserted.
func TestRunPanelObserver_PanelMode(t *testing.T) {
	srv := oracleTestServer(t, http.StatusOK,
		`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	defer srv.Close()

	col := &panelEventCollector{}
	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, AnthropicEndpoint: srv.URL, OnMemberEvent: col.sink()}
	models := []string{"a", "b", "c"}
	res := RunPanel(context.Background(), models, "panel", "q", "b", ccfg, map[string]string{"anthropic": "key"})
	if len(res) != 3 {
		t.Fatalf("expected 3 results, got %d", len(res))
	}
	evs := col.snapshot()
	// 3 members × (start + done) = 6 events.
	if len(evs) != 6 {
		t.Fatalf("expected 6 observer events, got %d: %+v", len(evs), evs)
	}
	// Reorder by slot: for each member collect its events in order.
	bySlot := map[int][]PanelMemberEventKind{}
	for _, ev := range evs {
		bySlot[ev.Slot] = append(bySlot[ev.Slot], ev.Kind)
	}
	for slot := 0; slot < 3; slot++ {
		kinds := bySlot[slot]
		if len(kinds) != 2 {
			t.Errorf("slot %d: got %d events, want 2", slot, len(kinds))
			continue
		}
		if kinds[0] != PanelMemberStart {
			t.Errorf("slot %d first event = %s, want start", slot, kinds[0])
		}
		if kinds[1] != PanelMemberDone && kinds[1] != PanelMemberError {
			t.Errorf("slot %d second event = %s, want done/error", slot, kinds[1])
		}
	}
}

// TestRunPanelObserver_NilIsNoop verifies a nil observer is safe (no panic) and
// produces identical results.
func TestRunPanelObserver_NilIsNoop(t *testing.T) {
	srv := oracleTestServer(t, http.StatusOK,
		`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	defer srv.Close()

	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, AnthropicEndpoint: srv.URL} // nil observer
	models := []string{"a"}
	res := RunPanel(context.Background(), models, "panel", "q", "b", ccfg, map[string]string{"anthropic": "key"})
	if len(res) != 1 || res[0].Err != nil || res[0].Answer != "ok" {
		t.Fatalf("nil-observer panel wrong result: %+v", res)
	}
}

// TestRunPanelObserver_PanickingObserver is isolated: a panicking observer must
// not alter the returned panel results.
func TestRunPanelObserver_PanickingObserver(t *testing.T) {
	srv := oracleTestServer(t, http.StatusOK,
		`{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	defer srv.Close()

	var panicked int32
	ccfg := PanelCallConfig{
		MaxTokens: 256, TimeoutSeconds: 5, AnthropicEndpoint: srv.URL,
		OnMemberEvent: func(PanelMemberEvent) {
			atomic.AddInt32(&panicked, 1)
			panic("observer boom")
		},
	}
	models := []string{"a", "b"}
	res := RunPanel(context.Background(), models, "panel", "q", "b", ccfg, map[string]string{"anthropic": "key"})
	if len(res) != 2 {
		t.Fatalf("expected 2 results despite panicking observer, got %d", len(res))
	}
	// 2 members × (start + done) = 4 panicking invocations, all recovered.
	if got := atomic.LoadInt32(&panicked); got != 4 {
		t.Errorf("observer called %d times, want 4 (panics recovered)", got)
	}
}

// TestRunPanelObserver_FusionMode verifies fusion fires one Start + one Done
// with the fusion model label.
func TestRunPanelObserver_FusionMode(t *testing.T) {
	// Fusion uses OpenRouter endpoint.
	srv := oracleTestServer(t, http.StatusOK,
		`{"choices":[{"message":{"role":"assistant","content":"fusion answer"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	defer srv.Close()

	col := &panelEventCollector{}
	ccfg := PanelCallConfig{MaxTokens: 256, TimeoutSeconds: 5, OpenRouterEndpoint: srv.URL, OnMemberEvent: col.sink()}
	RunPanel(context.Background(), []string{"a", "b"}, "fusion", "q", "b", ccfg, map[string]string{"openrouter": "key"})

	evs := col.snapshot()
	if len(evs) != 2 {
		t.Fatalf("expected 2 fusion events (start+done), got %d: %+v", len(evs), evs)
	}
	if evs[0].Kind != PanelMemberStart || evs[1].Kind != PanelMemberDone {
		t.Errorf("fusion events should be [start done], got [%s %s]", evs[0].Kind, evs[1].Kind)
	}
	if evs[0].Model != "openrouter:openrouter/fusion" {
		t.Errorf("fusion start model = %q", evs[0].Model)
	}
}
