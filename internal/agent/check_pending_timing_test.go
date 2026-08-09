package agent

// Card #125: check_pending shows registration age instead of runtime for terminal ops.
//
// Invariants under test:
//   - Running ops show "running for Xs" (time.Since(startedAt)), not registration age.
//   - Terminal ops show "completed Xs ago (ran for Ys)" using finishedAt, not createdAt.
//   - Already-delivered terminal ops are excluded from the no-ID listing.
//   - The no-ID listing returns "no async operations pending" when only delivered ops remain.
//   - The ID-specific running path uses startedAt, not createdAt.
//   - Detached shell ops have correct startedAt and finishedAt.

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestCheckPendingRunningShowsElapsedSinceStart verifies that a running op's
// display uses startedAt (worker entry time), not createdAt (registration time).
func TestCheckPendingRunningShowsElapsedSinceStart(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	block := make(chan struct{})
	op, reason := a.enqueueAsyncOp("mashura__review", "slow review", func() (string, []counselUsageRec, []string, error) {
		<-block
		return "result text", nil, nil, nil
	})
	if reason != "" {
		t.Fatal("enqueue refused:", reason)
	}

	// Wait for the worker to enter (startedAt is set).
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, startedAt, _, _ := op.timingSnapshot()
		if !startedAt.IsZero() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker never set startedAt")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The running display should say "running for" not "started X ago".
	got := a.handleCheckPending(tcArgs("check_pending", `{"id":""}`))
	if !strings.Contains(got, "running for") {
		t.Errorf("listing should say 'running for', got: %q", got)
	}
	if strings.Contains(got, "started") && strings.Contains(got, "ago") {
		t.Errorf("listing should not say 'started X ago' for running ops, got: %q", got)
	}

	// ID-specific running path should also use elapsed, not started X ago.
	gotID := a.handleCheckPending(tcArgs("check_pending", `{"id":"`+op.id+`"}`))
	if !strings.Contains(gotID, "still running") || !strings.Contains(gotID, "elapsed") {
		t.Errorf("ID running message: %q", gotID)
	}
	if strings.Contains(gotID, "started") && strings.Contains(gotID, "ago") {
		t.Errorf("ID running should not say 'started X ago', got: %q", gotID)
	}

	close(block)
	waitAsyncOps(t, a)
}

// TestCheckPendingTerminalShowsRunDuration verifies that a completed op's
// display shows "completed Xs ago (ran for Ys)" using finishedAt, not
// "started Xh ago" using createdAt. Uses an undelivered terminal op so it
// appears in the no-ID listing.
func TestCheckPendingTerminalShowsRunDuration(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	block := make(chan struct{})
	_, reason := a.enqueueAsyncOp("mashura__review", "quick review", func() (string, []counselUsageRec, []string, error) {
		<-block
		return "done", nil, nil, nil
	})
	if reason != "" {
		t.Fatal("enqueue refused:", reason)
	}

	// Let the op complete but DON'T drain — so it stays in the listing.
	close(block)
	waitAsyncOps(t, a)

	// The no-ID listing should show "completed" with "ran for" (not "started X ago").
	got := a.handleCheckPending(tcArgs("check_pending", `{"id":""}`))
	if !strings.Contains(got, "completed") {
		t.Errorf("listing should show 'completed', got: %q", got)
	}
	if !strings.Contains(got, "ran for") {
		t.Errorf("listing should show 'ran for', got: %q", got)
	}
	if strings.Contains(got, "started") && strings.Contains(got, "ago") {
		t.Errorf("listing should not say 'started X ago', got: %q", got)
	}

	// Drain to clean up.
	a.drainAsyncInbox()
}

// TestCheckPendingFailedOpShowsFailedState verifies that a failed terminal
// op displays "(failed)" not "(completed)" in the listing.
func TestCheckPendingFailedOpShowsFailedState(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	block := make(chan struct{})
	_, reason := a.enqueueAsyncOp("mashura__review", "doomed", func() (string, []counselUsageRec, []string, error) {
		<-block
		return "", nil, nil, fmt.Errorf("something went wrong")
	})
	if reason != "" {
		t.Fatal("enqueue refused:", reason)
	}

	close(block)
	waitAsyncOps(t, a)

	// Don't drain — the op should show in the listing.
	got := a.handleCheckPending(tcArgs("check_pending", `{"id":""}`))
	if !strings.Contains(got, "failed") {
		t.Errorf("listing should show 'failed' state, got: %q", got)
	}
	if !strings.Contains(got, "ran for") {
		t.Errorf("listing should show 'ran for', got: %q", got)
	}

	a.drainAsyncInbox()
}

// TestCheckPendingTerminalZeroStartedAt verifies that a terminal op with
// zero startedAt (watchdog fired before worker entered) displays "ran for 0s"
// not an absurd ~2562047h duration.
func TestCheckPendingTerminalZeroStartedAt(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	// Register an op directly and terminalize it WITHOUT setting startedAt
	// (simulates watchdog firing before worker goroutine starts).
	op, reason := a.registerAsyncOp("mashura__review", "watchdog race")
	if reason != "" {
		t.Fatal("register refused:", reason)
	}
	op.mu.Lock()
	op.terminal = true
	op.finishedAt = time.Now()
	op.err = fmt.Errorf("timed out")
	op.mu.Unlock()
	close(op.done)

	// Don't drain — the op should show in the listing.
	got := a.handleCheckPending(tcArgs("check_pending", `{"id":""}`))
	if strings.Contains(got, "2562047") {
		t.Errorf("zero startedAt should not produce absurd duration, got: %q", got)
	}
	if !strings.Contains(got, "ran for 0s") {
		t.Errorf("should show 'ran for 0s', got: %q", got)
	}

	// Clean up: remove from registry.
	a.asyncMu.Lock()
	delete(a.asyncOps, op.id)
	a.asyncActive--
	a.asyncMu.Unlock()
}

// TestCheckPendingListingFiltersDeliveredOps verifies that the no-ID listing
// excludes already-delivered terminal ops, and shows running + undelivered ops.
func TestCheckPendingListingFiltersDeliveredOps(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	// Complete an op and deliver it (so it's delivered + terminal).
	block1 := make(chan struct{})
	_, reason := a.enqueueAsyncOp("mashura__review", "done review", func() (string, []counselUsageRec, []string, error) {
		<-block1
		return "result1", nil, nil, nil
	})
	if reason != "" {
		t.Fatal("enqueue refused:", reason)
	}
	close(block1)
	waitAsyncOps(t, a)
	a.drainAsyncInbox() // marks envelopeDelivered

	// Start a running op (not delivered, not terminal).
	block2 := make(chan struct{})
	_, reason = a.enqueueAsyncOp("mashura__review", "still running", func() (string, []counselUsageRec, []string, error) {
		<-block2
		return "result2", nil, nil, nil
	})
	if reason != "" {
		t.Fatal("enqueue refused:", reason)
	}

	// Listing should show only the running op, not the delivered one.
	got := a.handleCheckPending(tcArgs("check_pending", `{"id":""}`))
	if !strings.Contains(got, "still running") {
		t.Errorf("listing should show the running op, got: %q", got)
	}
	if strings.Contains(got, "done review") {
		t.Errorf("listing should not show delivered terminal op, got: %q", got)
	}

	// Clean up.
	close(block2)
	waitAsyncOps(t, a)
}

// TestCheckPendingListingEmptyWhenAllDelivered verifies that when only delivered
// terminal ops remain, the listing says "no async operations pending".
func TestCheckPendingListingEmptyWhenAllDelivered(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	// Complete and deliver an op.
	block := make(chan struct{})
	_, reason := a.enqueueAsyncOp("mashura__review", "done", func() (string, []counselUsageRec, []string, error) {
		<-block
		return "result", nil, nil, nil
	})
	if reason != "" {
		t.Fatal("enqueue refused:", reason)
	}
	close(block)
	waitAsyncOps(t, a)
	a.drainAsyncInbox()

	// Listing should be empty.
	got := a.handleCheckPending(tcArgs("check_pending", `{"id":""}`))
	if !strings.Contains(got, "no async operations pending") {
		t.Errorf("expected 'no async operations pending', got: %q", got)
	}
}

// TestCheckPendingTerminalOpNotStartedYet verifies that when the worker hasn't
// entered yet (startedAt is zero), the running display falls back to createdAt
// rather than showing a nonsensical duration from the zero time.
func TestCheckPendingTerminalOpNotStartedYet(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	// Register an op directly (not via enqueueAsyncOp) so we control the
	// startedAt — it stays zero because no worker is started.
	op, reason := a.registerAsyncOp("mashura__review", "queued")
	if reason != "" {
		t.Fatal("register refused:", reason)
	}
	_ = op // just hold it registered

	got := a.handleCheckPending(tcArgs("check_pending", `{"id":""}`))
	// Should show "running for" with a small duration (from createdAt), not
	// a huge duration from the zero Time.
	if strings.Contains(got, "running for 2562047") {
		// 2562047h is the max duration from zero time — this is the bug.
		t.Errorf("running op with zero startedAt shows absurd duration: %q", got)
	}
	if !strings.Contains(got, "running for") {
		t.Errorf("should show 'running for', got: %q", got)
	}

	// Clean up: terminalize the op so it doesn't leak.
	op.mu.Lock()
	op.terminal = true
	op.finishedAt = time.Now()
	op.mu.Unlock()
	close(op.done)
	a.asyncMu.Lock()
	a.asyncOps[op.id] = op
	a.asyncActive--
	a.asyncMu.Unlock()
}

// TestCheckPendingListingSortedByCreation verifies the listing is sorted by
// creation time for deterministic output.
func TestCheckPendingListingSortedByCreation(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })

	// Register two running ops.
	block := make(chan struct{})

	op1, reason1 := a.enqueueAsyncOp("mashura__review", "first", func() (string, []counselUsageRec, []string, error) {
		<-block
		return "r1", nil, nil, nil
	})
	if reason1 != "" {
		t.Fatal("enqueue op1 refused:", reason1)
	}
	op2, reason2 := a.enqueueAsyncOp("mashura__review", "second", func() (string, []counselUsageRec, []string, error) {
		<-block
		return "r2", nil, nil, nil
	})
	if reason2 != "" {
		t.Fatal("enqueue op2 refused:", reason2)
	}

	got := a.handleCheckPending(tcArgs("check_pending", `{"id":""}`))
	// op1 should appear before op2 in the listing (sorted by createdAt).
	idx1 := strings.Index(got, op1.id)
	idx2 := strings.Index(got, op2.id)
	if idx1 < 0 || idx2 < 0 {
		t.Fatalf("both ops should be listed, got: %q", got)
	}
	if idx1 > idx2 {
		t.Errorf("op1 should appear before op2 (sorted by creation time), got: %q", got)
	}

	close(block)
	waitAsyncOps(t, a)
}
