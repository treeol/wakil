package agent

// Tests for the parallel-subagent groundwork: consent hoisting (step 1 of the
// parallel-subagents plan) and concurrency safety of dispatchSubagent.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/tools"
)

// TestConcurrentDispatchAfterConsentHoist verifies the step-1 invariant:
// ensureSubagentConsent runs once on the main goroutine, then two
// dispatchSubagent calls may run concurrently without prompting again and
// without racing on consentedBackends (run under -race).
func TestConcurrentDispatchAfterConsentHoist(t *testing.T) {
	summaryJSON := `{"objective":"check","findings":[{"summary":"done","location":"","kind":"fact","weight":"low"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: "+contentChunk(summaryJSON)+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	prompts := 0
	parent := &App{
		Cfg: func() config.Config {
			c := config.DefaultConfig()
			c.ExternalBackends = []string{"openrouter"}
			return c
		}(),
		Client: newTestClient(srv.URL),
		Exec:   newFakeExecutor(),
		Tools:  tools.DefaultTools("/work"),
		Out:    io.Discard,
		Confirm: func(toolName, _, _ string, _ bool) bool {
			if toolName == "external_backend" {
				prompts++ // main-goroutine only: counted without a lock on purpose —
				// a prompt from a worker goroutine would trip the race detector.
			}
			return true
		},
	}

	// Phase A (main goroutine): consent once.
	if !parent.ensureSubagentConsent("openrouter") {
		t.Fatal("consent should be granted")
	}
	if prompts != 1 {
		t.Fatalf("expected exactly 1 consent prompt in prepare phase; got %d", prompts)
	}

	// Phase B: two concurrent dispatches. Neither may prompt or write
	// parent-shared state.
	var wg sync.WaitGroup
	summaries := make([]SubagentSummary, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, _, _, _ := parent.dispatchSubagent(context.Background(),
				fmt.Sprintf("task %d", i), io.Discard, "openrouter")
			summaries[i] = s
		}(i)
	}
	wg.Wait()

	if prompts != 1 {
		t.Errorf("concurrent dispatches must not re-prompt; got %d total prompts", prompts)
	}
	for i, s := range summaries {
		if len(s.Findings) == 0 {
			t.Errorf("summary %d has no findings: %+v", i, s)
		}
	}
}

// TestDispatchGatedDecline verifies the gated wrapper returns the declined
// summary without opening a request when consent is refused.
func TestDispatchGatedDecline(t *testing.T) {
	requestCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	parent := &App{
		Cfg: func() config.Config {
			c := config.DefaultConfig()
			c.ExternalBackends = []string{"openrouter"}
			return c
		}(),
		Client:  newTestClient(srv.URL),
		Exec:    newFakeExecutor(),
		Tools:   tools.DefaultTools("/work"),
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return false },
	}

	summary, _, _, _ := parent.dispatchSubagentGated(context.Background(), "check", io.Discard, "openrouter")
	if requestCount != 0 {
		t.Errorf("declined dispatch must not make a request; got %d", requestCount)
	}
	if len(summary.Uncertainty) == 0 {
		t.Error("declined dispatch should carry an uncertainty note")
	}
}
