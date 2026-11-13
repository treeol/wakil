package agent

import (
	"context"
	"testing"
	"time"
)

// TestMashuraTimeoutZeroIdleTotal pins D1: an explicit 0 selects idle+total.
// The worker context gets the 30-minute total; the watchdog gets the idle
// timeout, so a silent stream is cut at the idle bound while a streaming
// reply is not cut at 300 seconds. Durations are injectable in tests via
// StreamIdleTimeoutSeconds and the total-cap constant override.
func TestMashuraTimeoutZeroIdleTotal(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	a.Cfg.StreamIdleTimeoutSeconds = 2 // 2s idle for the test
	a.Cfg.OracleTimeoutSeconds = 0
	a.Cfg.OracleTimeoutExplicitZero = true

	if got := a.mashuraTimeout(); got != mashuraIdleTotalCap {
		t.Fatalf("mashuraTimeout = %v, want total cap %v", got, mashuraIdleTotalCap)
	}
	if got := a.mashuraWatchdogTimeout(); got != 2*time.Second {
		t.Fatalf("mashuraWatchdogTimeout = %v, want idle 2s", got)
	}
	// Default (unset) case: 300s.
	b := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	b.Cfg.OracleTimeoutSeconds = 300
	if got := b.mashuraTimeout(); got != 300*time.Second {
		t.Fatalf("default mashuraTimeout = %v, want 300s", got)
	}
	if got := b.mashuraWatchdogTimeout(); got != 300*time.Second {
		t.Fatalf("default watchdog = %v, want 300s", got)
	}
}

// TestMashuraZeroWatchdogCutsSilentStream proves end-to-end (watchdog only):
// with explicit 0 and a 1s idle bound, an op whose worker never returns is
// force-terminalized at ~idle+grace rather than at the 300s default or the
// 30-minute total.
func TestMashuraZeroWatchdogCutsSilentStream(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	col := &collectEvents{}
	a.EventSink = col.sink
	a.Cfg.StreamIdleTimeoutSeconds = 1
	a.Cfg.OracleTimeoutSeconds = 0
	a.Cfg.OracleTimeoutExplicitZero = true
	a.watchdogGrace = 50 * time.Millisecond

	op, reason := a.registerAsyncOp("mashura__review", "silent panel")
	if reason != "" {
		t.Fatalf("register: %s", reason)
	}
	op.mu.Lock()
	op.uiJob = true
	op.mu.Unlock()

	start := time.Now()
	a.armMashuraWatchdog(op, a.mashuraWatchdogTimeout())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if ok, err := a.WaitForAsyncCompletion(ctx); err != nil || !ok {
		t.Fatalf("WaitForAsyncCompletion = %v, err=%v — watchdog did not cut the silent stream", ok, err)
	}
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Fatalf("silent stream cut after %v, want ~1s (idle)", elapsed)
	}
}

// TestMashuraZeroStreamingSurvives proves the other half: a worker that
// completes after an idle+over-300s-simulated window is not cut by the
// watchdog when explicit 0 is set. The test runs with an idle bound
// comfortably larger than the simulated slow stream's duration so the
// watchdog's idle deadline cannot fire during it.
func TestMashuraZeroStreamingSurvives(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	a.Cfg.StreamIdleTimeoutSeconds = 60 // idle 60s: slow stream outlives the test
	a.Cfg.OracleTimeoutSeconds = 0
	a.Cfg.OracleTimeoutExplicitZero = true
	a.watchdogGrace = 50 * time.Millisecond

	// Worker that returns after 400ms (simulating a slow but streaming
	// reply that already passed the old 300s default window).
	op, reason := a.enqueueAsyncOpJob("mashura__review", "slow panel", a.mashuraTimeout(), func(_, _ string) (string, []counselUsageRec, []string, error) {
		time.Sleep(400 * time.Millisecond)
		return "slow answer", nil, []string{"m1"}, nil
	})
	if reason != "" {
		t.Fatalf("enqueue refused: %s", reason)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ok, err := a.WaitForAsyncCompletion(ctx)
	if err != nil || !ok {
		t.Fatalf("WaitForAsyncCompletion = %v, err=%v", ok, err)
	}
	op.mu.Lock()
	result, oerr := op.result, op.err
	op.mu.Unlock()
	if oerr != nil {
		t.Fatalf("streaming op was cut by the watchdog: err=%v result=%q", oerr, result)
	}
	if result != "slow answer" {
		t.Fatalf("result = %q, want slow answer", result)
	}
}
