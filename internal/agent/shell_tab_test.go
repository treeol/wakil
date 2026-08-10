package agent

// Tests for card #128: detached-shell TUI tabs. announceShellStart/announceShellDone
// emit AsyncJobStartMsg/DoneMsg exactly once (keyed by job-<bgID>), carrying the
// origin captured at launch.

import (
	"strings"
	"testing"
	"time"
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
