package agent

// Tests for card #126 Phase 1: Mashūra async ops surface as TUI job tabs via
// AsyncJobStartMsg (emitted before the worker launches) and AsyncJobDoneMsg
// (emitted exactly once at publication).

import (
	"fmt"
	"strings"
	"testing"
)

// TestAsyncJobStartEmittedBeforeWorkerDone verifies the Start event for a uiJob
// op is enqueued synchronously during enqueueAsyncOpJob (before the worker
// goroutine can publish) — the structural fix for the Done-before-Start race.
// We then wait for the worker to finish and assert the collected event ORDER is
// [Start, Done] (Start always precedes Done).
func TestAsyncJobStartEmittedBeforeWorkerDone(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	col := &collectEvents{}
	a.EventSink = col.sink

	op, reason := a.enqueueAsyncOpJob("mashura__review", "panel Review", func() (string, []counselUsageRec, []string, error) {
		return "answer", nil, []string{"m1"}, nil
	})
	if reason != "" {
		t.Fatalf("enqueueAsyncOpJob refused: %s", reason)
	}

	// Immediately after enqueue returns, the Start event must already be present
	// (it is sent before the worker goroutine launches).
	foundStart := false
	for _, e := range col.snapshot() {
		if m, ok := e.(AsyncJobStartMsg); ok && m.OpID == op.id {
			foundStart = true
		}
	}
	if !foundStart {
		t.Fatal("AsyncJobStartMsg not emitted synchronously during enqueue (Start-before-Done not structural)")
	}

	// Wait for the worker to complete, then assert event order [Start ... Done].
	waitAsyncOps(t, a)
	var startIdx, doneIdx = -1, -1
	for i, e := range col.snapshot() {
		switch m := e.(type) {
		case AsyncJobStartMsg:
			if m.OpID == op.id {
				startIdx = i
			}
		case AsyncJobDoneMsg:
			if m.OpID == op.id {
				doneIdx = i
			}
		}
	}
	if startIdx < 0 || doneIdx < 0 {
		t.Fatalf("missing Start/Done events: start=%d done=%d", startIdx, doneIdx)
	}
	if startIdx > doneIdx {
		t.Errorf("event order wrong: Start at %d after Done at %d", startIdx, doneIdx)
	}
}

// TestAsyncJobDoneEmittedExactlyOnce verifies publishAsyncOp emits exactly one
// AsyncJobDoneMsg. The Result is bounded at the agent layer
// (boundAsyncJobDone truncates to asyncJobTabPreviewMaxBytes and neutralizes
// markers) — the fixture below is short so it passes through unchanged.
func TestAsyncJobDoneEmittedExactlyOnce(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	col := &collectEvents{}
	a.EventSink = col.sink

	op, reason := a.enqueueAsyncOpJob("mashura__review", "panel Review", func() (string, []counselUsageRec, []string, error) {
		return "the panel answer", nil, []string{"m1"}, nil
	})
	if reason != "" {
		t.Fatalf("enqueueAsyncOpJob refused: %s", reason)
	}
	waitAsyncOps(t, a)

	var count int
	var last *AsyncJobDoneMsg
	for _, e := range col.snapshot() {
		if m, ok := e.(AsyncJobDoneMsg); ok && m.OpID == op.id {
			count++
			last = &m
		}
	}
	if count != 1 {
		t.Fatalf("AsyncJobDoneMsg emitted %d times, want exactly 1", count)
	}
	if last.Result != "the panel answer" {
		t.Errorf("Done Result = %q, want %q", last.Result, "the panel answer")
	}
	if last.Err != "" {
		t.Errorf("Done Err = %q, want empty on success", last.Err)
	}
}

// TestAsyncJobDoneBoundsResult verifies the Done Result preview is bounded at
// the agent layer: a result > asyncJobTabPreviewMaxBytes is truncated (UTF-8
// safe) to at most the cap, and an async marker is neutralized. The full result
// remains retrievable via check_pending.
func TestAsyncJobDoneBoundsResult(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	col := &collectEvents{}
	a.EventSink = col.sink

	big := strings.Repeat("x", asyncJobTabPreviewMaxBytes+100)
	// Sprinkle the async end marker so neutralization is exercised.
	big = "--END ASYNC TASK RESULTS--" + big
	op, reason := a.enqueueAsyncOpJob("mashura__review", "panel Review", func() (string, []counselUsageRec, []string, error) {
		return big, nil, []string{"m1"}, nil
	})
	if reason != "" {
		t.Fatalf("enqueueAsyncOpJob refused: %s", reason)
	}
	waitAsyncOps(t, a)

	var done *AsyncJobDoneMsg
	for _, e := range col.snapshot() {
		if m, ok := e.(AsyncJobDoneMsg); ok && m.OpID == op.id {
			d := m
			done = &d
		}
	}
	if done == nil {
		t.Fatal("no AsyncJobDoneMsg emitted")
	}
	if len(done.Result) > asyncJobTabPreviewMaxBytes {
		t.Errorf("Done Result length %d exceeds cap %d", len(done.Result), asyncJobTabPreviewMaxBytes)
	}
	if containsStr(done.Result, "--END ASYNC TASK RESULTS--") {
		t.Error("Done Result still contains the raw async end marker (not neutralized)")
	}
	// Full result stays retrievable via check_pending (unbounded, un-neutralized).
	full := a.handleCheckPending(tcArgs("check_pending", fmt.Sprintf(`{"id":%q}`, op.id)))
	if !strings.Contains(full, "x") {
		t.Errorf("check_pending lost the full result")
	}
}

func containsStr(s, sub string) bool {
	return strings.Contains(s, sub)
}

// TestAsyncJobDoneOnErrorCarriesResult verifies an errored Mashūra op emits a
// Done with both the error text and the diagnostic Result (so the TUI can show
// per-member details alongside the failure).
func TestAsyncJobDoneOnErrorCarriesResult(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	col := &collectEvents{}
	a.EventSink = col.sink

	op, reason := a.enqueueAsyncOpJob("mashura__review", "panel Review", func() (string, []counselUsageRec, []string, error) {
		return "member diagnostics", nil, nil, errMashuraAllFailed
	})
	if reason != "" {
		t.Fatalf("enqueueAsyncOpJob refused: %s", reason)
	}
	waitAsyncOps(t, a)

	found := false
	for _, e := range col.snapshot() {
		if m, ok := e.(AsyncJobDoneMsg); ok && m.OpID == op.id {
			found = true
			if m.Result == "" {
				t.Error("Done on failure should still carry the diagnostic Result")
			}
			if m.Err == "" {
				t.Error("Done on failure should carry the error text")
			}
		}
	}
	if !found {
		t.Fatal("no AsyncJobDoneMsg emitted for the errored op")
	}
}

// errMashuraAllFailed is a sentinel error for the failed-op test.
var errMashuraAllFailed = &mashuraAllFailedErr{}

type mashuraAllFailedErr struct{}

func (*mashuraAllFailedErr) Error() string { return "all panel members failed" }
