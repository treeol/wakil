package agent

// Tests for card #130: a timeout watchdog for Mashūra async ops (uiJob). A hung
// panel must be force-terminalized so the async-job tab doesn't spin forever,
// the admission slot is released, and AsyncJobDoneMsg is emitted exactly once —
// without double-closing op.done or losing paid (late) usage.

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/counsel"
	"github.com/treeol/wakil/internal/proxy"
)

// TestMashuraWatchdogForceTerminalizes verifies that if the Mashūra worker
// doesn't return within timeout + grace, the watchdog force-terminalizes the
// op: slot released, exactly one AsyncJobDoneMsg with a timeout error, op.done
// NOT closed by the watchdog (only the worker closes it).
func TestMashuraWatchdogForceTerminalizes(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	col := &collectEvents{}
	a.EventSink = col.sink

	// Directly register a Mashūra uiJob op and arm the watchdog WITHOUT starting
	// a worker — simulate a genuinely stuck panel goroutine.
	op, reason := a.registerAsyncOp("mashura__review", "hung panel")
	if reason != "" {
		t.Fatalf("register: %s", reason)
	}
	op.mu.Lock()
	op.uiJob = true
	op.mu.Unlock()

	a.watchdogGrace = 1 * time.Second
	a.armMashuraWatchdog(op, 300*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ok, err := a.WaitForAsyncCompletion(ctx)
	if err != nil {
		t.Fatalf("WaitForAsyncCompletion error: %v", err)
	}
	if !ok {
		t.Fatal("watchdog did not fire — no completion within window")
	}

	// Slot released.
	if n := a.countActiveAsyncOps(); n != 0 {
		t.Errorf("expected 0 active ops after watchdog, got %d", n)
	}

	// Exactly one AsyncJobDoneMsg with a timeout error.
	var doneCount int
	var gotErr bool
	for _, e := range col.snapshot() {
		if m, ok := e.(AsyncJobDoneMsg); ok && m.OpID == op.id {
			doneCount++
			if m.Err != "" {
				gotErr = true
			}
		}
	}
	if doneCount != 1 {
		t.Errorf("AsyncJobDoneMsg emitted %d times, want exactly 1", doneCount)
	}
	if !gotErr {
		t.Error("AsyncJobDoneMsg did not carry a timeout error")
	}

	// op.done must NOT be closed by the watchdog (only the worker closes it).
	select {
	case <-op.done:
		t.Error("op.done was closed by the watchdog — should only be closed by the worker")
	default:
	}

	// Terminal state with timeout error.
	terminal, _, perr := op.terminalSnapshot()
	if !terminal {
		t.Error("op should be terminal after watchdog")
	}
	if perr == nil || !strings.Contains(perr.Error(), "timed out") {
		t.Errorf("op.err = %v, want timeout error", perr)
	}
}

// TestMashuraWatchdogCancelledOnNormalCompletion verifies that a worker that
// completes before the watchdog fires cancels the timer — no spurious
// AsyncJobDoneMsg beyond the single normal one, one publish, slot returns.
// It uses a SHORT effective timeout (config min 1s + tiny grace) so the armed
// window is actually testable, then asserts the timer is cancelled (watchdog
// never fires).
func TestMashuraWatchdogCancelledOnNormalCompletion(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	// Shorten the built-in default so the armed window is genuinely short.
	a.Cfg.OracleTimeoutSeconds = 1 // config is in whole seconds; 0 would use default 300
	a.watchdogGrace = 100 * time.Millisecond
	col := &collectEvents{}
	a.EventSink = col.sink

	op, reason := a.enqueueAsyncOpJob("mashura__review", "quick panel", func(_ string, _ string) (string, []counselUsageRec, []string, error) {
		return "answer", nil, []string{"m1"}, nil
	})
	if reason != "" {
		t.Fatalf("enqueueAsyncOpJob refused: %s", reason)
	}
	waitAsyncOps(t, a)

	// The worker completed and its defer cancelWatchdog cleared the timer.
	op.mu.Lock()
	w := op.watchdog
	op.mu.Unlock()
	if w != nil {
		t.Error("watchdog was not cancelled after normal completion (timer still armed)")
	}

	// Wait beyond timeout+grace to prove the cancelled timer never fires a
	// second (error) Done.
	time.Sleep(1200 * time.Millisecond)

	var doneCount int
	var errs int
	for _, e := range col.snapshot() {
		if m, ok := e.(AsyncJobDoneMsg); ok && m.OpID == op.id {
			doneCount++
			if m.Err != "" {
				errs++
			}
		}
	}
	if doneCount != 1 {
		t.Errorf("AsyncJobDoneMsg emitted %d times, want exactly 1 (watchdog not cancelled)", doneCount)
	}
	if errs != 0 {
		t.Errorf("got %d error Done events, want 0 (watchdog should be cancelled)", errs)
	}
	if n := a.countActiveAsyncOps(); n != 0 {
		t.Errorf("expected 0 active ops, got %d", n)
	}
}

// TestMashuraWatchdogLateCostReconciled verifies the Mashūra review op-2 finding
// #3: when the watchdog wins terminalization but the late worker eventually
// returns billed usage, that usage is committed exactly once — paid tokens for a
// slow-but-completed panel are never lost.
func TestMashuraWatchdogLateCostReconciled(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	a.Costs = proxy.NewCostTracker()
	col := &collectEvents{}
	a.EventSink = col.sink

	// Block the worker until after the watchdog would have fired.
	proceed := make(chan struct{})
	started := make(chan struct{})
	usage := []counselUsageRec{{Model: "late-model", Usage: counsel.OracleUsage{InputTokens: 3, OutputTokens: 2}}}
	var finished atomic.Bool

	op, reason := a.enqueueAsyncOpJob("mashura__review", "slow panel", func(_ string, _ string) (string, []counselUsageRec, []string, error) {
		close(started)
		<-proceed // block past watchdog
		finished.Store(true)
		return "late answer", usage, []string{"late-model"}, nil
	})
	if reason != "" {
		t.Fatalf("enqueueAsyncOpJob refused: %s", reason)
	}

	// Short timeout + grace so the watchdog fires while the worker is blocked.
	a.watchdogGrace = 500 * time.Millisecond
	// The watchdog was armed at enqueue with a.mashuraTimeout() (default real
	// value, too long for a test). Re-arm with a short timeout directly, and
	// cancel the original.
	<-started
	a.cancelWatchdog(op)
	a.armMashuraWatchdog(op, 100*time.Millisecond)

	// Watchdog fires (~100ms + 500ms grace), publishing the timeout.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if ok, err := a.WaitForAsyncCompletion(ctx); err != nil || !ok {
		t.Fatalf("wait for watchdog completion: ok=%v err=%v", ok, err)
	}

	// Now let the worker finish with its billed usage. Wait on op.done (closed
	// only by the worker AFTER it commits cost) so the cost snapshot below is
	// race-free — the watchdog already released asyncActive, so we can't rely
	// on active-op counting to know the worker finished.
	close(proceed)
	select {
	case <-op.done:
	case <-time.After(10 * time.Second):
		t.Fatal("late worker never finished")
	}
	if !finished.Load() {
		t.Fatal("worker never returned with usage")
	}

	// The timeout outcome remains authoritative: only ONE Done, with the error,
	// not overwritten by the late "answer".
	var doneCount int
	var gotErr bool
	for _, e := range col.snapshot() {
		if m, ok := e.(AsyncJobDoneMsg); ok && m.OpID == op.id {
			doneCount++
			if m.Err != "" {
				gotErr = true
			}
		}
	}
	if doneCount != 1 {
		t.Errorf("AsyncJobDoneMsg emitted %d times, want exactly 1", doneCount)
	}
	if !gotErr {
		t.Error("timeout Done should carry the error (late result must not overwrite it)")
	}

	// Late usage committed exactly once (no lost tokens).
	_, rows := a.Costs.Snapshot()
	var calls int
	for _, r := range rows {
		if strings.Contains(r.Source, "late-model") {
			calls = r.Calls
		}
	}
	if calls != 1 {
		t.Fatalf("late model cost calls = %d, want 1 (billed usage lost or double-committed)", calls)
	}
}

// TestMashuraWatchdogPublishesWhenWorkerStuckAfterTerminal verifies the
// watchdog's "terminal && !published" liveness-gap branch: if the worker set
// terminal but blocked before calling publishAsyncOp (deadlock between the two),
// the watchdog publishes for it — emitting exactly one AsyncJobDoneMsg and
// releasing the slot.
func TestMashuraWatchdogPublishesWhenWorkerStuckAfterTerminal(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	col := &collectEvents{}
	a.EventSink = col.sink

	op, reason := a.registerAsyncOp("mashura__review", "stuck-after-terminal")
	if reason != "" {
		t.Fatalf("register: %s", reason)
	}
	op.mu.Lock()
	op.uiJob = true
	op.mu.Unlock()
	// Simulate the worker having set terminal (but not published).
	op.mu.Lock()
	op.terminal = true
	op.finishedAt = time.Now()
	op.result = "worker result"
	op.mu.Unlock()

	a.watchdogGrace = 1 * time.Second
	a.armMashuraWatchdog(op, 200*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if ok, err := a.WaitForAsyncCompletion(ctx); err != nil || !ok {
		t.Fatalf("wait for watchdog publish: ok=%v err=%v", ok, err)
	}

	if n := a.countActiveAsyncOps(); n != 0 {
		t.Errorf("expected 0 active ops, got %d", n)
	}
	// Exactly one Done, using the worker's already-set result (the watchdog only
	// publishes; it does not overwrite).
	var doneCount int
	for _, e := range col.snapshot() {
		if m, ok := e.(AsyncJobDoneMsg); ok && m.OpID == op.id {
			doneCount++
		}
	}
	if doneCount != 1 {
		t.Errorf("AsyncJobDoneMsg emitted %d times, want exactly 1", doneCount)
	}
	// op.done still not closed by the watchdog.
	select {
	case <-op.done:
		t.Error("op.done closed by watchdog")
	default:
	}
}

// TestMashuraWatchdogArmedOnlyForUiJob verifies the watchdog is armed ONLY for
// uiJob (Mashūra) ops, not plain enqueueAsyncOp callers.
func TestMashuraWatchdogArmedOnlyForUiJob(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	op, reason := a.enqueueAsyncOp("mashura__review", "plain op", func() (string, []counselUsageRec, []string, error) {
		return "ok", nil, nil, nil
	})
	if reason != "" {
		t.Fatalf("enqueueAsyncOp refused: %s", reason)
	}
	waitAsyncOps(t, a)
	op.mu.Lock()
	w := op.watchdog
	op.mu.Unlock()
	if w != nil {
		t.Error("plain (non-uiJob) async op should not have a watchdog armed")
	}
}

// TestMashuraTimeoutNeverZero verifies mashuraTimeout() always returns a
// non-zero bounded default even when the config says 0 (or unset).
func TestMashuraTimeoutNeverZero(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	a.Cfg.OracleTimeoutSeconds = 0
	if d := a.mashuraTimeout(); d <= 0 {
		t.Errorf("mashuraTimeout() with 0 config = %v, want a positive bounded default", d)
	}
	a.Cfg.OracleTimeoutSeconds = 45
	if d := a.mashuraTimeout(); d != 45*time.Second {
		t.Errorf("mashuraTimeout() with 45 config = %v, want 45s", d)
	}
}
