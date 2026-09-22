package agent

// Tests for auto-bg run_shell async registry races.
// Verifies:
//   - auto-bg run_shell registers as pending async work (asyncActive incremented)
//   - isIdle returns true while an auto-bg shell is running
//   - process exit publishes exactly one completion and decrements asyncActive (no leak)
//   - the TUI tab is properly closed (announceShellDone) on the registered path
//   - kill_process on an auto-bg shell releases the async slot (no double-close panic)
//   - fast exit (process exits before reaper observes) does not strand an async op

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/proxy"
)

// startAutoBGShell starts a run_shell that will auto-background at its deadline.
// Returns the bgID extracted from the "still running" result.
func startAutoBGShell(t *testing.T, app *App, command string) string {
	t.Helper()
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "run_shell", Arguments: fmt.Sprintf(`{"command":%q}`, command),
	}})
	if !strings.Contains(res.text, "still running") {
		t.Fatalf("expected auto-background, got: %s", res.text)
	}
	// Wait for the auto-bg to register the entry.
	time.Sleep(500 * time.Millisecond)
	app.bgMu.RLock()
	var bgID string
	for id := range app.bgProcs {
		bgID = id
	}
	app.bgMu.RUnlock()
	if bgID == "" {
		t.Fatal("no bg entry found after auto-background")
	}
	return bgID
}

// TestAutoBG_RegistersAsyncWork verifies that an auto-backgrounded run_shell
// registers as pending async work, so wait_for_completion can see it.
func TestAutoBG_RegistersAsyncWork(t *testing.T) {
	exe, err := exec.NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exe.Close()

	cfg := config.DefaultConfig()
	cfg.ShellTimeoutSec = 1 // detach after ~1s
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     cfg,
	}

	bgID := startAutoBGShell(t, app, "sleep 30")

	// asyncActive must be 1 (the auto-bg shell registered as pending async work).
	if active := app.countActiveAsyncOps(); active != 1 {
		t.Fatalf("asyncActive = %d, want 1 (auto-bg should register)", active)
	}

	// isIdle should return true (noToolCalls=true, asyncActive > 0).
	if !app.isIdle(true) {
		t.Error("isIdle should return true while auto-bg shell is running")
	}

	// The bgEntry must have notifyOnExit=true and a non-nil asyncOp.
	app.bgMu.RLock()
	entry := app.bgProcs[bgID]
	app.bgMu.RUnlock()
	if !entry.notifyOnExit {
		t.Error("entry.notifyOnExit should be true")
	}
	if entry.asyncOp == nil {
		t.Error("entry.asyncOp should be non-nil")
	}

	// Kill to clean up.
	app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "kill_process", Arguments: fmt.Sprintf(`{"id":%q}`, bgID),
	}})

	// Wait for asyncActive to return to 0.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if app.countActiveAsyncOps() == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if active := app.countActiveAsyncOps(); active != 0 {
		t.Fatalf("asyncActive = %d after kill, want 0 (slot leak)", active)
	}
}

// TestAutoBG_FastExitNoSlotLeak verifies that if the process exits quickly
// after the deadline is hit (the reaper race window), the async slot is still
// released exactly once — no stranded op.
func TestAutoBG_FastExitNoSlotLeak(t *testing.T) {
	exe, err := exec.NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exe.Close()

	cfg := config.DefaultConfig()
	cfg.ShellTimeoutSec = 1
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     cfg,
	}

	// Run a command that finishes right around the deadline — the reaper
	// might observe the exit before or after we register the async op.
	// The fix ensures the op is registered BEFORE bgMu, so the reaper
	// either sees asyncOp already attached, or doesn't see the entry at all.
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "run_shell", Arguments: `{"command":"sleep 1"}`,
	}})

	// Whether it auto-backgrounded or completed synchronously, asyncActive
	// must return to 0.
	_ = res
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if app.countActiveAsyncOps() == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if active := app.countActiveAsyncOps(); active != 0 {
		t.Fatalf("asyncActive = %d after fast-exit, want 0 (slot leak)", active)
	}
}

// TestAutoBG_ExitPublishesCompletion verifies that when an auto-bg shell exits
// naturally, exactly one completion is published to asyncInbox and asyncActive
// returns to 0.
func TestAutoBG_ExitPublishesCompletion(t *testing.T) {
	exe, err := exec.NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exe.Close()

	cfg := config.DefaultConfig()
	cfg.ShellTimeoutSec = 1
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     cfg,
	}

	bgID := startAutoBGShell(t, app, "sleep 2")

	// Wait for the process to exit and the reaper to publish.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if app.countActiveAsyncOps() == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if active := app.countActiveAsyncOps(); active != 0 {
		t.Fatalf("asyncActive = %d after exit, want 0 (slot leak)", active)
	}

	// The asyncInbox should have exactly one completion.
	app.asyncMu.Lock()
	inboxCount := len(app.asyncInbox)
	app.asyncMu.Unlock()
	if inboxCount != 1 {
		t.Fatalf("asyncInbox has %d entries, want 1", inboxCount)
	}

	// The completion should have toolName "run_shell".
	app.asyncMu.Lock()
	op := app.asyncInbox[0]
	app.asyncMu.Unlock()
	if op.toolName != "run_shell" {
		t.Errorf("completion toolName = %q, want \"run_shell\"", op.toolName)
	}

	// Clean up the bg entry if still present.
	app.bgMu.Lock()
	delete(app.bgProcs, bgID)
	app.bgMu.Unlock()
}

// TestAutoBG_KillReleasesSlot verifies that kill_process on an auto-bg shell
// releases the async slot without a double-close panic.
func TestAutoBG_KillReleasesSlot(t *testing.T) {
	exe, err := exec.NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exe.Close()

	cfg := config.DefaultConfig()
	cfg.ShellTimeoutSec = 1
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     cfg,
	}

	bgID := startAutoBGShell(t, app, "sleep 30")

	// Verify the slot is held.
	if active := app.countActiveAsyncOps(); active != 1 {
		t.Fatalf("asyncActive = %d before kill, want 1", active)
	}

	// Kill it — should not panic (double-close guard).
	app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "kill_process", Arguments: fmt.Sprintf(`{"id":%q}`, bgID),
	}})

	// Wait for the slot to be released.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if app.countActiveAsyncOps() == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if active := app.countActiveAsyncOps(); active != 0 {
		t.Fatalf("asyncActive = %d after kill, want 0 (slot leak)", active)
	}

	// No completion should be in the inbox (kill suppresses notification).
	app.asyncMu.Lock()
	inboxCount := len(app.asyncInbox)
	app.asyncMu.Unlock()
	if inboxCount != 0 {
		t.Fatalf("asyncInbox has %d entries after kill, want 0 (kill should suppress)", inboxCount)
	}
}

// TestAutoBG_TabDoneEmittedOnNaturalExit verifies that the TUI tab is properly
// closed (announceShellDone) when an auto-bg shell exits naturally on the
// registered-async-op path (finding 2 regression test).
func TestAutoBG_TabDoneEmittedOnNaturalExit(t *testing.T) {
	exe, err := exec.NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exe.Close()

	col := &collectEvents{}
	cfg := config.DefaultConfig()
	cfg.ShellTimeoutSec = 1
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     cfg,
		Client:  newTestClient("http://unused.invalid"),
	}
	app.EventSink = col.sink

	bgID := startAutoBGShell(t, app, "sleep 2")

	// Wait for Start to be emitted.
	time.Sleep(500 * time.Millisecond)
	var sawStart bool
	for _, ev := range col.snapshot() {
		if m, ok := ev.(AsyncJobStartMsg); ok && m.OpID == "job-"+bgID {
			sawStart = true
		}
	}
	if !sawStart {
		t.Fatal("no AsyncJobStartMsg emitted after auto-background")
	}

	// Wait for the process to exit and the reaper to emit Done.
	deadline := time.Now().Add(10 * time.Second)
	var sawDone bool
	for time.Now().Before(deadline) {
		for _, ev := range col.snapshot() {
			if m, ok := ev.(AsyncJobDoneMsg); ok && m.OpID == "job-"+bgID {
				sawDone = true
			}
		}
		if sawDone {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !sawDone {
		t.Fatal("no AsyncJobDoneMsg emitted after auto-bg shell exit (stranded tab)")
	}

	// Verify exactly one Start and one Done.
	var starts, dones int
	for _, ev := range col.snapshot() {
		switch m := ev.(type) {
		case AsyncJobStartMsg:
			if m.OpID == "job-"+bgID {
				starts++
			}
		case AsyncJobDoneMsg:
			if m.OpID == "job-"+bgID {
				dones++
			}
		}
	}
	if starts != 1 {
		t.Errorf("AsyncJobStartMsg emitted %d times, want 1", starts)
	}
	if dones != 1 {
		t.Errorf("AsyncJobDoneMsg emitted %d times, want 1", dones)
	}

	// Clean up.
	app.bgMu.Lock()
	delete(app.bgProcs, bgID)
	app.bgMu.Unlock()
}

// TestAutoBG_ReaperDeletesEntryOnNaturalExit verifies that the reaper removes
// the bgProcs entry after a natural process exit (H7: map stays bounded).
func TestAutoBG_ReaperDeletesEntryOnNaturalExit(t *testing.T) {
	exe, err := exec.NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exe.Close()

	cfg := config.DefaultConfig()
	cfg.ShellTimeoutSec = 1
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     cfg,
	}

	bgID := startAutoBGShell(t, app, "sleep 3")

	// Verify entry exists while process is running.
	app.bgMu.RLock()
	_, exists := app.bgProcs[bgID]
	app.bgMu.RUnlock()
	if !exists {
		t.Fatal("bgProcs entry should exist while process is running")
	}

	// Wait for the process to exit and the reaper to clean up.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		app.bgMu.RLock()
		_, exists := app.bgProcs[bgID]
		app.bgMu.RUnlock()
		if !exists {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	app.bgMu.RLock()
	_, exists = app.bgProcs[bgID]
	app.bgMu.RUnlock()
	if exists {
		t.Error("bgProcs entry should be deleted by reaper after natural exit (H7: map growth)")
	}
}

// TestRunBackground_ReaperDeletesEntryOnNaturalExit verifies the run_background
// reaper also removes the entry after natural exit (H7).
func TestRunBackground_ReaperDeletesEntryOnNaturalExit(t *testing.T) {
	exe, err := exec.NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exe.Close()

	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     config.DefaultConfig(),
	}

	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "run_background", Arguments: `{"command":"sleep 1","label":"test"}`,
	}})
	if !strings.Contains(res.text, "id: bg") {
		t.Fatalf("expected bg id, got: %s", res.text)
	}

	// Extract bgID.
	bgID := "bg1"
	for _, line := range strings.Split(res.text, "\n") {
		if strings.HasPrefix(line, "id: ") {
			bgID = strings.TrimSpace(strings.TrimPrefix(line, "id: "))
			break
		}
	}

	// Wait for the process to exit and the reaper to clean up.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		app.bgMu.RLock()
		_, exists := app.bgProcs[bgID]
		app.bgMu.RUnlock()
		if !exists {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	app.bgMu.RLock()
	_, exists := app.bgProcs[bgID]
	app.bgMu.RUnlock()
	if exists {
		t.Error("bgProcs entry should be deleted by reaper after natural exit (H7: map growth)")
	}
}

// TestPublishBgCompletion_NoDoubleClose verifies that publishBgCompletion
// and cancelBgAsyncOp cannot both close op.done (finding 3).
// This is a unit test for the helper functions directly.
func TestPublishBgCompletion_NoDoubleClose(t *testing.T) {
	app := &App{
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     config.DefaultConfig(),
	}
	app.ensureWake()

	// Register an op.
	op, reason := app.registerAsyncOp("run_shell", "test")
	if reason != "" {
		t.Fatalf("registerAsyncOp refused: %s", reason)
	}

	e := &bgEntry{
		id:        "bg-test",
		cmdDigest: "echo test",
		readOnly:  true,
		startedAt: time.Now(),
	}

	// Simulate the reaper calling publishBgCompletion while a kill path
	// calls cancelBgAsyncOp concurrently. Neither should panic.
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.publishBgCompletion(op, "bg-test", e, "exit 0", "")
	}()

	// Give the publisher a head start, then cancel.
	time.Sleep(1 * time.Millisecond)
	app.cancelBgAsyncOp(op, "bg-test", "killed")

	<-done // publishBgCompletion returned

	// asyncActive should be 0 (exactly one decrement, not two).
	if active := app.countActiveAsyncOps(); active != 0 {
		t.Fatalf("asyncActive = %d, want 0 (double-decrement?)", active)
	}
}

// TestAutoBG_RegistryFullReturnMessage verifies that when the async registry
// is full, the return message does NOT promise "you will be notified" but
// instead tells the model to poll read_process_log.
func TestAutoBG_RegistryFullReturnMessage(t *testing.T) {
	exe, err := exec.NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exe.Close()

	cfg := config.DefaultConfig()
	cfg.ShellTimeoutSec = 1
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     cfg,
	}

	// Fill the async registry with blocking ops.
	block := make(chan struct{})
	for i := 0; i < asyncMaxActive; i++ {
		if _, reason := app.enqueueAsyncOp("mashura__review", "filler", func() (string, []counselUsageRec, []string, error) {
			<-block
			return "", nil, nil, nil
		}); reason != "" {
			t.Fatalf("enqueue %d refused early: %s", i, reason)
		}
	}

	// Now an auto-bg run_shell should get "full" from registerAsyncOp.
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "run_shell", Arguments: `{"command":"sleep 30"}`,
	}})
	if !strings.Contains(res.text, "still running") {
		t.Fatalf("expected auto-background, got: %s", res.text)
	}
	// The message must NOT promise notification.
	if strings.Contains(res.text, "you will be notified") {
		t.Errorf("return message should NOT say 'you will be notified' when registry is full, got: %s", res.text)
	}
	// The message MUST mention the registry is full.
	if !strings.Contains(res.text, "registry is full") {
		t.Errorf("return message should mention 'registry is full', got: %s", res.text)
	}
	// The message MUST still mention read_process_log for polling.
	if !strings.Contains(res.text, "read_process_log") {
		t.Errorf("return message should mention read_process_log, got: %s", res.text)
	}

	// Clean up: kill the auto-bg shell and release the blocking ops.
	time.Sleep(500 * time.Millisecond)
	app.bgMu.RLock()
	var bgID string
	for id := range app.bgProcs {
		bgID = id
	}
	app.bgMu.RUnlock()
	if bgID != "" {
		app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
			Name: "kill_process", Arguments: fmt.Sprintf(`{"id":%q}`, bgID),
		}})
	}
	close(block)
	waitAsyncOps(t, app)
}

// TestAutoBG_StoppingReturnMessage verifies that when the session is stopping,
// the return message says notification will not arrive.
func TestAutoBG_StoppingReturnMessage(t *testing.T) {
	exe, err := exec.NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exe.Close()

	cfg := config.DefaultConfig()
	cfg.ShellTimeoutSec = 1
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     cfg,
	}

	// Mark the async registry as stopping.
	app.asyncMu.Lock()
	app.asyncStopping = true
	app.asyncMu.Unlock()

	// An auto-bg run_shell should get "stopping" from registerAsyncOp.
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "run_shell", Arguments: `{"command":"sleep 30"}`,
	}})
	if !strings.Contains(res.text, "still running") {
		t.Fatalf("expected auto-background, got: %s", res.text)
	}
	// The message must NOT promise notification.
	if strings.Contains(res.text, "you will be notified") {
		t.Errorf("return message should NOT say 'you will be notified' when stopping, got: %s", res.text)
	}
	// The message MUST mention shutting down.
	if !strings.Contains(res.text, "shutting down") {
		t.Errorf("return message should mention 'shutting down', got: %s", res.text)
	}

	// Clean up: kill the shell.
	time.Sleep(500 * time.Millisecond)
	app.bgMu.RLock()
	var bgID string
	for id := range app.bgProcs {
		bgID = id
	}
	app.bgMu.RUnlock()
	if bgID != "" {
		app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
			Name: "kill_process", Arguments: fmt.Sprintf(`{"id":%q}`, bgID),
		}})
	}

	// Reset stopping state for other tests.
	app.asyncMu.Lock()
	app.asyncStopping = false
	app.asyncMu.Unlock()
}

// TestBgShellWatchdogReleasesSlot verifies that the shell watchdog arms on
// registration and force-terminalizes a stuck background shell, releasing the
// async slot so a suspended turn doesn't wait forever. This is the regression
// test for the pre-fix bug where detached-shell ops had no watchdog and the
// 24h reaper leaked the slot.
func TestBgShellWatchdogReleasesSlot(t *testing.T) {
	exe, err := exec.NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exe.Close()

	cfg := config.DefaultConfig()
	cfg.BgShellTimeoutSeconds = 1 // 1s watchdog for test speed
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     cfg,
	}

	bgID := startAutoBGShell(t, app, "sleep 30")

	// asyncActive must be 1 (registered).
	if active := app.countActiveAsyncOps(); active != 1 {
		t.Fatalf("asyncActive = %d after start, want 1", active)
	}

	// The watchdog (1s + 10s grace = ~11s) should fire and release the slot.
	// Wait up to 20s for the watchdog to terminalize.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if app.countActiveAsyncOps() == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if active := app.countActiveAsyncOps(); active != 0 {
		t.Fatalf("asyncActive = %d after watchdog timeout, want 0 (slot leak — watchdog did not fire)", active)
	}

	// The inbox should have the timeout completion.
	app.asyncMu.Lock()
	inboxLen := len(app.asyncInbox)
	app.asyncMu.Unlock()
	if inboxLen < 1 {
		t.Error("asyncInbox should have the timeout completion, got 0 entries")
	}

	// Clean up: kill the process.
	app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "kill_process", Arguments: fmt.Sprintf(`{"id":%q}`, bgID),
	}})
}

// TestReaperAbandonmentReleasesSlot verifies that the 24h reaper abandonment
// path calls cancelBgAsyncOp to release the async slot, rather than just
// deleting the entry — the original bug that caused indefinite "waiting".
func TestReaperAbandonmentReleasesSlot(t *testing.T) {
	exe, err := exec.NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exe.Close()

	cfg := config.DefaultConfig()
	cfg.BgShellTimeoutSeconds = 3600 // 1h watchdog — won't fire during this test
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     cfg,
	}

	bgID := startAutoBGShell(t, app, "sleep 30")

	if active := app.countActiveAsyncOps(); active != 1 {
		t.Fatalf("asyncActive = %d after start, want 1", active)
	}

	// Simulate the reaper's 24h abandonment: grab the asyncOp, delete the
	// entry, and call cancelBgAsyncOp — the fix we added.
	app.bgMu.Lock()
	entry := app.bgProcs[bgID]
	op := entry.asyncOp
	delete(app.bgProcs, bgID)
	app.bgMu.Unlock()

	if op == nil {
		t.Fatal("entry.asyncOp should be non-nil")
	}

	a := app
	a.cancelBgAsyncOp(op, bgID, "reaper abandoned after 24h")

	// asyncActive must be 0 after cancelBgAsyncOp.
	if active := app.countActiveAsyncOps(); active != 0 {
		t.Fatalf("asyncActive = %d after reaper abandonment, want 0 (slot leak)", active)
	}

	// WaitForAsyncCompletion should return (false, nil) — nothing left to wait.
	got, werr := app.WaitForAsyncCompletion(context.Background())
	if werr != nil {
		t.Fatalf("WaitForAsyncCompletion error: %v", werr)
	}
	if got {
		t.Error("WaitForAsyncCompletion should return false (nothing left), got true")
	}

	// Clean up: kill the process.
	app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "kill_process", Arguments: fmt.Sprintf(`{"id":%q}`, bgID),
	}})
}

// TestBgShellWatchdogClosesDone verifies that the shell watchdog closes op.done
// after publishing — the fix for the channel leak where armShellWatchdog
// published but nobody closed op.done (Bug 1 from Mashūra review).
func TestBgShellWatchdogClosesDone(t *testing.T) {
	exe, err := exec.NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exe.Close()

	cfg := config.DefaultConfig()
	cfg.BgShellTimeoutSeconds = 1 // 1s watchdog for test speed
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     cfg,
	}

	bgID := startAutoBGShell(t, app, "sleep 30")

	// Grab the asyncOp before the watchdog fires.
	app.bgMu.RLock()
	entry := app.bgProcs[bgID]
	app.bgMu.RUnlock()
	if entry == nil || entry.asyncOp == nil {
		t.Fatal("entry.asyncOp should be non-nil before watchdog fires")
	}
	op := entry.asyncOp

	// Wait for the watchdog to close op.done (with a timeout). Publication
	// precedes closure, so waiting on done is the correct synchronization.
	select {
	case <-op.done:
		// Good — channel was closed by the watchdog.
	case <-time.After(20 * time.Second):
		t.Fatal("op.done was not closed within 20s (channel leak — watchdog did not close it)")
	}

	// After done is closed, the slot must be released.
	if active := app.countActiveAsyncOps(); active != 0 {
		t.Fatalf("asyncActive = %d after watchdog closed done, want 0", active)
	}

	// Clean up: kill the process.
	app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "kill_process", Arguments: fmt.Sprintf(`{"id":%q}`, bgID),
	}})
}

// TestPublishBgCompletionPreservesWatchdogOutcome verifies that when the
// watchdog terminalizes first (timeout), a subsequent publishBgCompletion call
// does NOT overwrite the timeout outcome — the first terminalizer owns the
// outcome (Bug 2 from Mashūra review).
func TestPublishBgCompletionPreservesWatchdogOutcome(t *testing.T) {
	exe, err := exec.NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exe.Close()

	cfg := config.DefaultConfig()
	cfg.BgShellTimeoutSeconds = 3600 // won't fire — we simulate the watchdog
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     cfg,
	}

	bgID := startAutoBGShell(t, app, "sleep 30")
	defer func() {
		app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
			Name: "kill_process", Arguments: fmt.Sprintf(`{"id":%q}`, bgID),
		}})
	}()

	// Grab the asyncOp.
	app.bgMu.RLock()
	entry := app.bgProcs[bgID]
	app.bgMu.RUnlock()
	if entry == nil || entry.asyncOp == nil {
		t.Fatal("entry.asyncOp should be non-nil")
	}
	op := entry.asyncOp

	// Simulate the watchdog terminalizing first (timeout outcome).
	op.mu.Lock()
	op.terminal = true
	op.finishedAt = time.Now()
	op.startedAt = op.createdAt
	op.err = fmt.Errorf("background shell timed out after %s", 1*time.Second)
	op.result = "Background shell timed out — timeout outcome from watchdog."
	op.mu.Unlock()

	// Now simulate the reaper calling publishBgCompletion with a normal-exit
	// result. The watchdog's timeout outcome must NOT be overwritten.
	app.publishBgCompletion(op, bgID, entry, "exit 0", "normal output")

	// Verify the timeout outcome was preserved, not overwritten.
	op.mu.Lock()
	terminalResult := op.result
	terminalErr := op.err
	doneClosed := op.doneClosed
	published := op.published
	op.mu.Unlock()

	if terminalResult != "Background shell timed out — timeout outcome from watchdog." {
		t.Errorf("op.result was overwritten by publishBgCompletion: got %q, want watchdog timeout message", terminalResult)
	}
	if terminalErr == nil || !strings.Contains(terminalErr.Error(), "timed out") {
		t.Errorf("op.err was overwritten or cleared: got %v, want timeout error", terminalErr)
	}
	if !published {
		t.Error("op.published should be true (publishBgCompletion should still publish)")
	}
	if !doneClosed {
		t.Error("op.doneClosed should be true (publishBgCompletion should close done)")
	}

	// op.done must be closed.
	select {
	case <-op.done:
		// Good.
	default:
		t.Error("op.done should be closed after publishBgCompletion")
	}
}
