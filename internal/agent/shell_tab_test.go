package agent

// Tests for card #128: detached-shell TUI tabs. announceShellStart/announceShellDone
// emit AsyncJobStartMsg/DoneMsg exactly once (keyed by job-<bgID>), carrying the
// origin captured at launch.

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

// TestAnnounceShellStartExactlyOnce verifies announceShellStart emits a single
// AsyncJobStartMsg with the job-bgN OpID, origin, and label; repeats are no-ops.
func TestAnnounceShellStartExactlyOnce(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	col := &collectEvents{}
	a.EventSink = col.sink

	e := &bgEntry{id: "bg7", label: "", cmdDigest: "echo hi", toolName: "run_shell", originChatID: "chat-x"}
	a.announceShellStart("bg7", e)
	a.announceShellStart("bg7", e) // duplicate → no-op

	var starts int
	for _, ev := range col.snapshot() {
		if m, ok := ev.(AsyncJobStartMsg); ok && m.OpID == "job-bg7" {
			starts++
			if m.ToolName != "run_shell" {
				t.Errorf("ToolName = %q, want run_shell", m.ToolName)
			}
			if m.OriginChatID != "chat-x" {
				t.Errorf("OriginChatID = %q, want chat-x", m.OriginChatID)
			}
			if m.Label != "echo hi" {
				t.Errorf("Label = %q, want cmdDigest fallback 'echo hi'", m.Label)
			}
		}
	}
	if starts != 1 {
		t.Errorf("AsyncJobStartMsg emitted %d times, want exactly 1", starts)
	}
}

// TestAnnounceShellDoneExactlyOnceBounds verifies announceShellDone emits a single
// bounded, marker-neutralized AsyncJobDoneMsg; repeats are no-ops.
func TestAnnounceShellDoneExactlyOnceBounds(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	col := &collectEvents{}
	a.EventSink = col.sink

	e := &bgEntry{id: "bg8", label: "my server", toolName: "run_background", originChatID: "chat-y"}
	big := strings.Repeat("x", asyncJobTabPreviewMaxBytes+50) + "--END ASYNC TASK RESULTS--"
	a.announceShellDone("bg8", e, big, "")
	a.announceShellDone("bg8", e, big, "") // duplicate → no-op

	var dones int
	for _, ev := range col.snapshot() {
		if m, ok := ev.(AsyncJobDoneMsg); ok && m.OpID == "job-bg8" {
			dones++
			if m.Label != "my server" {
				t.Errorf("Label = %q, want 'my server'", m.Label)
			}
			if m.OriginChatID != "chat-y" {
				t.Errorf("OriginChatID = %q, want chat-y", m.OriginChatID)
			}
			if len(m.Result) > asyncJobTabPreviewMaxBytes {
				t.Errorf("Result length %d exceeds cap %d", len(m.Result), asyncJobTabPreviewMaxBytes)
			}
			if strings.Contains(m.Result, "--END ASYNC TASK RESULTS--") {
				t.Error("Result contains raw async marker (not neutralized)")
			}
		}
	}
	if dones != 1 {
		t.Errorf("AsyncJobDoneMsg emitted %d times, want exactly 1", dones)
	}
}

// TestShellTabStartThenDoneSequence verifies a shell's full tab lifecycle: Start
// then Done, both keyed by job-bgN with the same origin.
func TestShellTabStartThenDoneSequence(t *testing.T) {
	a := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	col := &collectEvents{}
	a.EventSink = col.sink

	e := &bgEntry{
		id:           "bg9",
		label:        "auto-bg",
		toolName:     "run_shell",
		cmdDigest:    "sleep 100",
		originChatID: "chat-z",
		startedAt:    time.Now(),
	}
	a.announceShellStart("bg9", e)
	a.announceShellDone("bg9", e, "exited with status 0\n", "")

	var startAt, doneAt = -1, -1
	for i, ev := range col.snapshot() {
		switch m := ev.(type) {
		case AsyncJobStartMsg:
			if m.OpID == "job-bg9" {
				startAt = i
			}
		case AsyncJobDoneMsg:
			if m.OpID == "job-bg9" {
				doneAt = i
			}
		}
	}
	if startAt < 0 || doneAt < 0 {
		t.Fatalf("missing Start/Done for job-bg9: start=%d done=%d", startAt, doneAt)
	}
	if startAt > doneAt {
		t.Errorf("Start after Done: start=%d done=%d", startAt, doneAt)
	}
}

// TestShellTabDoneEmittedAfterKill verifies card #132: a run_background tab whose
// process is killed via kill_process must still terminalize (emit AsyncJobDoneMsg)
// rather than strand yellow & unclosable. The bg-reaper captures the entry pointer
// and emits the tab Done on group exit even though kill_process deleted the entry
// and disarmed notifyOnExit — decoupling the tab lifecycle from model notification.
func TestShellTabDoneEmittedAfterKill(t *testing.T) {
	exe, err := exec.NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exe.Close()

	col := &collectEvents{}
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     config.DefaultConfig(),
		Client:  newTestClient("http://unused.invalid"),
	}
	app.EventSink = col.sink

	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "run_background", Arguments: `{"command":"sleep 30","label":"nap"}`,
	}})
	if !strings.Contains(res.text, "id:") {
		t.Fatalf("run_background failed: %s", res.text)
	}
	// Capture the bg id, then kill it.
	app.bgMu.RLock()
	var bgID string
	for id, e := range app.bgProcs {
		bgID = id
		_ = e
	}
	app.bgMu.RUnlock()
	if bgID == "" {
		t.Fatal("no bg entry found after run_background")
	}
	killRes := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "kill_process", Arguments: fmt.Sprintf(`{"id":%q}`, bgID),
	}})
	if strings.Contains(killRes.text, "ERROR") {
		t.Fatalf("kill_process failed: %s", killRes.text)
	}

	// The run_background reaper polls group liveness every 200ms; wait briefly
	// for the tab Done to fire, then assert exactly one AsyncJobDoneMsg.
	deadline := time.Now().Add(3 * time.Second)
	var done *AsyncJobDoneMsg
	for time.Now().Before(deadline) {
		for _, ev := range col.snapshot() {
			if m, ok := ev.(AsyncJobDoneMsg); ok && m.OpID == "job-"+bgID {
				done = &m
				goto found
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
found:
	if done == nil {
		t.Fatal("run_background tab was NOT terminalized after kill_process (stranded)")
	}
	// Exactly once: a second snapshot scan must not reveal duplicates.
	var count int
	for _, ev := range col.snapshot() {
		if m, ok := ev.(AsyncJobDoneMsg); ok && m.OpID == "job-"+bgID {
			count++
		}
	}
	if count != 1 {
		t.Errorf("AsyncJobDoneMsg emitted %d times after kill, want exactly 1", count)
	}
}

// TestAutoBGShellTabDoneEmittedAfterKill verifies card #132 on the auto-bg
// run_shell path (the reaper actually modified): a run_shell that auto-backgrounds
// at its deadline opens a tab (Start), and a subsequent kill_process must still
// terminalize it (exactly one Done) rather than strand it yellow. Prior to the
// fix the auto-bg reaper did a fresh a.bgProcs lookup and gated tab-Done behind
// notifyOnExit, so kill_process deleting the entry + disarming notifyOnExit left
// the tab stranded.
func TestAutoBGShellTabDoneEmittedAfterKill(t *testing.T) {
	exe, err := exec.NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer exe.Close()

	col := &collectEvents{}
	cfg := config.DefaultConfig()
	cfg.ShellTimeoutSec = 1 // detach long commands after ~1s
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     cfg,
		Client:  newTestClient("http://unused.invalid"),
	}
	app.EventSink = col.sink

	// Run a long command that will hit the deadline and auto-background.
	// Capture the Start event timing so we can assert Start precedes Done.
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "run_shell", Arguments: `{"command":"sleep 60"}`,
	}})
	if !strings.Contains(res.text, "still running") {
		t.Fatalf("expected auto-background (detach), got: %s", res.text)
	}
	// Yield so the reaper/Start can settle, then find the bgID.
	time.Sleep(1500 * time.Millisecond)

	app.bgMu.RLock()
	var bgID string
	for id := range app.bgProcs {
		bgID = id
	}
	app.bgMu.RUnlock()
	if bgID == "" {
		t.Fatal("no bg entry found after auto-background")
	}

	// Start must have been emitted before we kill.
	var sawStart bool
	for _, ev := range col.snapshot() {
		if m, ok := ev.(AsyncJobStartMsg); ok && m.OpID == "job-"+bgID {
			sawStart = true
		}
	}
	if !sawStart {
		t.Fatal("no AsyncJobStartMsg emitted after auto-background (tab never opened)")
	}

	// Kill the process; the tab must still terminalize.
	killRes := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "kill_process", Arguments: fmt.Sprintf(`{"id":%q}`, bgID),
	}})
	if strings.Contains(killRes.text, "ERROR") {
		t.Fatalf("kill_process failed: %s", killRes.text)
	}

	// Wait (past one reaper poll interval) for the Done, then assert exactly one.
	deadline := time.Now().Add(5 * time.Second)
	var firstDoneAt = -1
	for time.Now().Before(deadline) {
		idx := -1
		evs := col.snapshot()
		for i, ev := range evs {
			if m, ok := ev.(AsyncJobDoneMsg); ok && m.OpID == "job-"+bgID {
				idx = i
			}
		}
		if idx >= 0 {
			firstDoneAt = idx
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if firstDoneAt < 0 {
		t.Fatal("auto-bg tab was NOT terminalized after kill_process (stranded)")
	}
	// Quiesce past one reaper poll (200ms) so a racing duplicate emitter can't
	// slip the dup check, then assert exactly one Done.
	time.Sleep(400 * time.Millisecond)
	var dones, starts int
	var startBeforesDone int
	evs := col.snapshot()
	for i, ev := range evs {
		switch m := ev.(type) {
		case AsyncJobStartMsg:
			if m.OpID == "job-"+bgID {
				starts++
				if i < firstDoneAt {
					startBeforesDone++
				}
			}
		case AsyncJobDoneMsg:
			if m.OpID == "job-"+bgID {
				dones++
			}
		}
	}
	if starts != 1 {
		t.Errorf("AsyncJobStartMsg emitted %d times, want exactly 1", starts)
	}
	if dones != 1 {
		t.Errorf("AsyncJobDoneMsg emitted %d times after kill, want exactly 1", dones)
	}
	if startBeforesDone != 1 {
		t.Errorf("Start did not precede Done (order violated): start-before-done=%d", startBeforesDone)
	}
}
