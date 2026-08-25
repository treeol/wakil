package agent

// Tests for the notify_on_exit parameter on run_background.
// Verifies:
//   - notify_on_exit=true registers as pending async work (asyncActive incremented)
//   - isIdle returns true while a notify_on_exit job is running
//   - process exit publishes exactly one completion to asyncInbox (via publishAsyncOp)
//   - asyncActive is decremented back to 0 after completion (no leak)
//   - kill_process on a notify_on_exit job suppresses the completion notice
//   - default (notify_on_exit=false) does NOT register as pending async work
//   - the completion message carries the correct toolName and originChatID

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/proxy"
)

// helper to start a run_background job and return the bgEntry
func startBgJob(t *testing.T, app *App, args string) string {
	t.Helper()
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "run_background", Arguments: args,
	}})
	if !strings.Contains(res.text, "id:") {
		t.Fatalf("run_background failed: %s", res.text)
	}
	// Extract bgID from result (first line: "id: bg1\n...")
	lines := strings.Split(res.text, "\n")
	bgID := strings.TrimPrefix(lines[0], "id: ")
	return bgID
}

// waitForBgExit waits for a background process to exit by polling the done channel.
func waitForBgExit(t *testing.T, app *App, bgID string, timeout time.Duration) {
	t.Helper()
	app.bgMu.RLock()
	entry, ok := app.bgProcs[bgID]
	app.bgMu.RUnlock()
	if !ok {
		t.Fatalf("entry %s not found in bgProcs", bgID)
	}
	if entry.done == nil {
		t.Fatalf("entry %s has nil done channel", bgID)
	}
	select {
	case <-entry.done:
	case <-time.After(timeout):
		t.Fatalf("process %s did not exit within %s", bgID, timeout)
	}
}

// TestNotifyOnExit_RegistersAsyncWork verifies that notify_on_exit=true increments
// asyncActive, making isIdle return true while the job runs.
func TestNotifyOnExit_RegistersAsyncWork(t *testing.T) {
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

	// Start a long-running job with notify_on_exit=true.
	bgID := startBgJob(t, app, `{"command":"sleep 5","label":"test","notify_on_exit":true}`)

	// asyncActive must be 1 (the job registered as pending async work).
	if active := app.countActiveAsyncOps(); active != 1 {
		t.Fatalf("asyncActive = %d, want 1 (notify_on_exit should register)", active)
	}

	// isIdle should return true (noToolCalls=true, asyncActive > 0).
	if !app.isIdle(true) {
		t.Error("isIdle should return true while notify_on_exit job is running")
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

	// Kill the job to clean up.
	app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "kill_process", Arguments: `{"id":"` + bgID + `"}`,
	}})

	// Wait for the reaper to observe the exit.
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

// TestNotifyOnExit_DefaultNoRegistration verifies that without notify_on_exit
// (default false), asyncActive stays 0 and isIdle returns false.
func TestNotifyOnExit_DefaultNoRegistration(t *testing.T) {
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

	bgID := startBgJob(t, app, `{"command":"sleep 5","label":"daemon"}`)

	// asyncActive must be 0 (default: poll-by-design, no registration).
	if active := app.countActiveAsyncOps(); active != 0 {
		t.Fatalf("asyncActive = %d, want 0 (default should not register)", active)
	}

	// isIdle should return false (no async work pending).
	if app.isIdle(true) {
		t.Error("isIdle should return false for default run_background (no async work)")
	}

	// The bgEntry must have notifyOnExit=false.
	app.bgMu.RLock()
	entry := app.bgProcs[bgID]
	app.bgMu.RUnlock()
	if entry.notifyOnExit {
		t.Error("entry.notifyOnExit should be false for default")
	}
	if entry.asyncOp != nil {
		t.Error("entry.asyncOp should be nil for default")
	}

	// Clean up.
	app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "kill_process", Arguments: `{"id":"` + bgID + `"}`,
	}})
}

// TestNotifyOnExit_FastExitPublishesCompletion verifies that a fast-exiting
// process with notify_on_exit=true publishes exactly one completion to asyncInbox
// and decrements asyncActive back to 0 (no leak).
func TestNotifyOnExit_FastExitPublishesCompletion(t *testing.T) {
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

	bgID := startBgJob(t, app, `{"command":"true","label":"fast","notify_on_exit":true}`)

	// Capture the asyncOp ID before the process exits (the op was registered
	// at launch via registerAsyncOp, so its ID is op-N, not job-bgN).
	app.bgMu.RLock()
	entry := app.bgProcs[bgID]
	opID := ""
	if entry.asyncOp != nil {
		opID = entry.asyncOp.id
	}
	app.bgMu.RUnlock()
	if opID == "" {
		t.Fatal("entry.asyncOp is nil — notify_on_exit did not register")
	}

	// Wait for the process to exit and the reaper to publish.
	waitForBgExit(t, app, bgID, 3*time.Second)

	// Give the reaper time to publish the completion.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if app.countActiveAsyncOps() == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if active := app.countActiveAsyncOps(); active != 0 {
		t.Fatalf("asyncActive = %d after exit, want 0 (slot leak)", active)
	}

	// The asyncInbox should have exactly one completion for this job.
	app.asyncMu.Lock()
	inboxCount := len(app.asyncInbox)
	app.asyncMu.Unlock()
	if inboxCount != 1 {
		t.Fatalf("asyncInbox has %d entries, want 1", inboxCount)
	}

	// The completion op should have toolName "run_background" (not "run_shell")
	// and the correct op ID from registerAsyncOp.
	app.asyncMu.Lock()
	op := app.asyncInbox[0]
	app.asyncMu.Unlock()
	if op.toolName != "run_background" {
		t.Errorf("completion toolName = %q, want \"run_background\"", op.toolName)
	}
	if op.id != opID {
		t.Errorf("completion id = %q, want %q", op.id, opID)
	}
}

// TestNotifyOnExit_KillSuppressesCompletion verifies that kill_process on a
// notify_on_exit job clears notifyOnExit, so no completion notice is published.
func TestNotifyOnExit_KillSuppressesCompletion(t *testing.T) {
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

	bgID := startBgJob(t, app, `{"command":"sleep 30","label":"killed","notify_on_exit":true}`)

	// Capture the done channel before killing (kill_process deletes the entry).
	app.bgMu.RLock()
	entry := app.bgProcs[bgID]
	doneCh := entry.done
	app.bgMu.RUnlock()

	// Kill it — should disarm notifyOnExit.
	app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "kill_process", Arguments: `{"id":"` + bgID + `"}`,
	}})

	// Wait for the reaper to observe the exit (entry is deleted from bgProcs,
	// but the reaper goroutine captured the entry pointer and will close done).
	select {
	case <-doneCh:
	case <-time.After(5 * time.Second):
		t.Fatal("process did not exit within 5s after kill")
	}

	// Give the reaper + cancelBgAsyncOp time to run.
	time.Sleep(300 * time.Millisecond)

	// Assert no completion in asyncInbox (kill suppressed the model notice).
	app.asyncMu.Lock()
	inboxCount := len(app.asyncInbox)
	app.asyncMu.Unlock()
	if inboxCount != 0 {
		t.Fatalf("asyncInbox has %d entries after kill, want 0 (kill should suppress)", inboxCount)
	}

	// P0 assertion: asyncActive must return to 0 — no slot leak.
	if active := app.countActiveAsyncOps(); active != 0 {
		t.Fatalf("asyncActive = %d after kill, want 0 (slot leak — kill_process must release the async slot)", active)
	}
}

// TestNotifyOnExit_ResultText verifies the run_background result text includes
// the notify indicator when notify_on_exit=true.
func TestNotifyOnExit_ResultText(t *testing.T) {
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

	// With notify_on_exit=true
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "run_background", Arguments: `{"command":"sleep 2","label":"test","notify_on_exit":true}`,
	}})
	if !strings.Contains(res.text, "notify: enabled") {
		t.Errorf("result should mention 'notify: enabled', got: %s", res.text)
	}

	// Kill to clean up.
	bgID := strings.Split(res.text, "\n")[0]
	bgID = strings.TrimPrefix(bgID, "id: ")
	app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "kill_process", Arguments: `{"id":"` + bgID + `"}`,
	}})

	// Without notify_on_exit (default)
	res2 := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "run_background", Arguments: `{"command":"sleep 2","label":"test"}`,
	}})
	if strings.Contains(res2.text, "notify: enabled") {
		t.Errorf("default result should NOT mention 'notify: enabled', got: %s", res2.text)
	}

	// Kill to clean up.
	bgID2 := strings.Split(res2.text, "\n")[0]
	bgID2 = strings.TrimPrefix(bgID2, "id: ")
	app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "kill_process", Arguments: `{"id":"` + bgID2 + `"}`,
	}})
}