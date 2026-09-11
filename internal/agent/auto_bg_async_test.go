package agent

// Tests for card #220: auto-bg run_shell async registry races.
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
// registered-async-op path (card #220 finding 2 regression test).
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

// TestPublishBgCompletion_NoDoubleClose verifies that publishBgCompletion
// and cancelBgAsyncOp cannot both close op.done (card #220 finding 3).
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
// instead tells the model to poll read_process_log (card #221).
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
// the return message says notification will not arrive (card #221).
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
