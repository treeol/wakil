package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/core/event"
)

// e2eReady waits for the input placeholder and the first session.
func e2eReady(t *testing.T, e *e2e) {
	t.Helper()
	e.waitScreen("type a task", 30*time.Second)
	e.waitKind(event.KindSessionCreated, 1, 10*time.Second)
}

// e2eWaitTurns waits for turns and fails on session errors or read errors.
func e2eWaitTurns(t *testing.T, e *e2e, n int) {
	t.Helper()
	e.waitTurns(n, 30*time.Second)
	evs, err := e.events()
	if err != nil {
		t.Fatalf("events() read error (a finding, not swallowed): %v", err)
	}
	for _, ev := range evs {
		if ev.Kind == event.KindSessionError {
			t.Fatalf("unexpected session_error event: %+v", ev.Payload)
		}
	}
}

// e2eNotAsked fails if another approval was requested. There is no
// screen check because the screen accumulates every rendered frame and
// the TUI redraws an earlier prompt in later frames, so an old prompt
// reappears after any mark. The durable log is the source of truth,
// and the caller also asserts the tool effect.
func e2eNotAsked(t *testing.T, e *e2e, before int) {
	t.Helper()
	if got := e.countKind(event.KindApprovalRequested); got != before {
		t.Fatalf("approval_requested count = %d, want %d", got, before)
	}
}

// e2eFileIs fails if the file content differs from want.
func e2eFileIs(t *testing.T, e *e2e, path string, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v\n%s", path, err, e.tail(3000))
	}
	if string(data) != want {
		t.Fatalf("%s = %q, want %q\n%s", path, data, want, e.tail(3000))
	}
}

// e2eDirExists fails if path is not an existing directory.
func e2eDirExists(t *testing.T, e *e2e, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat dir %s: %v\n%s", path, err, e.tail(3000))
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory\n%s", path, e.tail(3000))
	}
}

// e2eWaitSessions polls until at least want sessions exist.
func e2eWaitSessions(t *testing.T, e *e2e, want int) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		ids, err := e.sessionIDs()
		if err == nil && len(ids) >= want {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	ids, err := e.sessionIDs()
	t.Fatalf("timed out waiting for %d sessions, got %v, err %v\n%s", want, ids, err, e.tail(2000))
}

// TestE2EGrantThenSameToolNotAsked checks a grant covers the next write.
func TestE2EGrantThenSameToolNotAsked(t *testing.T) {
	turns := map[string][][]string{
		"alpha one": {
			e2eToolCallFrames("call_1", "write_file", `{"path":"a.txt","content":"A"}`),
			{e2eContentChunk("done")},
		},
		"bravo two": {
			e2eToolCallFrames("call_2", "write_file", `{"path":"b.txt","content":"B"}`),
			{e2eContentChunk("done")},
		},
	}
	e := startE2E(t, e2eOpts{Turns: turns})
	e2eReady(t, e)

	e.submit("alpha one")
	e.waitScreen("[g] grant for this session", 30*time.Second)
	e.key("g")
	e2eWaitTurns(t, e, 1)

	e.submit("bravo two")
	e2eWaitTurns(t, e, 2)
	e2eNotAsked(t, e, 1)
	e2eFileIs(t, e, filepath.Join(e.workdir, "a.txt"), "A")
	e2eFileIs(t, e, filepath.Join(e.workdir, "b.txt"), "B")
	if got := e.countKind(event.KindApprovalRequested); got != 1 {
		t.Fatalf("approval_requested count = %d, want exactly 1", got)
	}
}

// TestE2EGrantSurvivesNew checks a grant covers a write in a new session.
func TestE2EGrantSurvivesNew(t *testing.T) {
	turns := map[string][][]string{
		"alpha one": {
			e2eToolCallFrames("call_1", "write_file", `{"path":"a.txt","content":"A"}`),
			{e2eContentChunk("done")},
		},
		"bravo two": {
			e2eToolCallFrames("call_2", "write_file", `{"path":"b.txt","content":"B"}`),
			{e2eContentChunk("done")},
		},
	}
	e := startE2E(t, e2eOpts{Turns: turns})
	e2eReady(t, e)

	e.submit("alpha one")
	e.waitScreen("[g] grant for this session", 30*time.Second)
	e.key("g")
	e2eWaitTurns(t, e, 1)

	e.submitRotation("/new")
	e2eWaitSessions(t, e, 2)

	e.submit("bravo two")
	e2eWaitTurns(t, e, 2)
	e2eNotAsked(t, e, 1)
	e2eFileIs(t, e, filepath.Join(e.workdir, "b.txt"), "B")

	ids, by, err := e.eventsBySession()
	if err != nil {
		t.Fatalf("eventsBySession: %v", err)
	}
	if len(ids) < 2 {
		t.Fatalf("session count = %d, want at least 2", len(ids))
	}
	n := 0
	for _, ev := range by[ids[1]] {
		if ev.Kind == event.KindApprovalRequested {
			n++
		}
	}
	if n != 0 {
		t.Fatalf("second session approval_requested count = %d, want 0", n)
	}
}

// TestE2EGrantSurvivesResume checks a grant covers a write after resume.
func TestE2EGrantSurvivesResume(t *testing.T) {
	turns := map[string][][]string{
		"alpha one": {
			e2eToolCallFrames("call_1", "write_file", `{"path":"a.txt","content":"A"}`),
			{e2eContentChunk("done")},
		},
		"bravo two": {
			{e2eContentChunk("ok")},
		},
		"charlie three": {
			e2eToolCallFrames("call_3", "write_file", `{"path":"c.txt","content":"C"}`),
			{e2eContentChunk("done")},
		},
	}
	e := startE2E(t, e2eOpts{Turns: turns})
	e2eReady(t, e)

	e.submit("alpha one")
	e.waitScreen("[g] grant for this session", 30*time.Second)
	e.key("g")
	e2eWaitTurns(t, e, 1)

	before, err := e.chatIDs()
	if err != nil {
		t.Fatalf("chatIDs: %v", err)
	}
	if len(before) == 0 {
		t.Fatalf("chatIDs is empty, sessions dir %q has no session file", e.sessions)
	}
	first := before[0]

	e.submitRotation("/new")
	e2eWaitSessions(t, e, 2)
	e.submit("bravo two")
	e2eWaitTurns(t, e, 2)

	e.submitRotation("/resume " + first)

	e.submit("charlie three")
	// Resume replays completed-turn events into a new host session. Do
	// not mistake those historical events for this write's completion.
	e.waitFileContent(filepath.Join(e.workdir, "c.txt"), "C", 30*time.Second)
	e2eWaitTurns(t, e, 3)
	e2eNotAsked(t, e, 1)
	e2eFileIs(t, e, filepath.Join(e.workdir, "c.txt"), "C")
}

// TestE2EDestructiveShellStillAsks checks destructive shell still prompts.
func TestE2EDestructiveShellStillAsks(t *testing.T) {
	turns := map[string][][]string{
		"alpha one": {
			e2eToolCallFrames("call_1", "run_shell", `{"command":"touch out.txt"}`),
			{e2eContentChunk("done")},
		},
		"bravo two": {
			e2eToolCallFrames("call_2", "run_shell", `{"command":"mkdir scratch"}`),
			{e2eContentChunk("done")},
		},
		"charlie three": {
			e2eToolCallFrames("call_3", "run_shell", `{"command":"rmdir scratch"}`),
			{e2eContentChunk("done")},
		},
	}
	e := startE2E(t, e2eOpts{Turns: turns})
	e2eReady(t, e)

	e.submit("alpha one")
	e.waitScreen("[g] grant for this session", 30*time.Second)
	e.key("g")
	e2eWaitTurns(t, e, 1)
	e2eFileIs(t, e, filepath.Join(e.workdir, "out.txt"), "")

	e.submit("bravo two")
	e2eWaitTurns(t, e, 2)
	e2eNotAsked(t, e, 1)
	e2eDirExists(t, e, filepath.Join(e.workdir, "scratch"))

	e.submit("charlie three")
	e.waitKind(event.KindApprovalRequested, 2, 10*time.Second)
	e.waitScreenLine(30*time.Second, "destructive prompt without grant choice", func(line string) bool {
		return strings.Contains(line, "[y] proceed") && strings.Contains(line, "[n] decline") && !strings.Contains(line, "[g] grant")
	})
	e.key("g")
	time.Sleep(500 * time.Millisecond)
	if got := e.countKind(event.KindApprovalResolved); got != 1 {
		t.Fatalf("approval_resolved count after g = %d, want still 1", got)
	}
	e.key("n")
	e2eWaitTurns(t, e, 3)
	e2eDirExists(t, e, filepath.Join(e.workdir, "scratch"))

	evs, err := e.events()
	if err != nil {
		t.Fatalf("events() read error (a finding, not swallowed): %v", err)
	}
	var reqs []event.ApprovalRequested
	var outcomes []string
	for _, ev := range evs {
		switch ev.Kind {
		case event.KindApprovalRequested:
			reqs = append(reqs, ev.Payload.(event.ApprovalRequested))
		case event.KindApprovalResolved:
			outcomes = append(outcomes, ev.Payload.(event.ApprovalResolved).Outcome)
		}
	}
	if len(reqs) != 2 {
		t.Fatalf("approval_requested count = %d, want exactly 2", len(reqs))
	}
	if !reqs[0].GrantEligible {
		t.Fatalf("first approval_requested GrantEligible = false, want true")
	}
	if reqs[1].GrantEligible {
		t.Fatalf("second approval_requested GrantEligible = true, want false")
	}
	if len(outcomes) != 2 || outcomes[0] != "granted" || outcomes[1] != "declined" {
		t.Fatalf("approval_resolved outcomes = %v, want [granted declined]", outcomes)
	}
}

// TestE2EGrantEventReplaysAndResumes checks a grant event replays cleanly.
func TestE2EGrantEventReplaysAndResumes(t *testing.T) {
	turns := map[string][][]string{
		"alpha one": {
			e2eToolCallFrames("call_1", "write_file", `{"path":"a.txt","content":"A"}`),
			{e2eContentChunk("done")},
		},
		"bravo two": {
			e2eToolCallFrames("call_2", "write_file", `{"path":"b.txt","content":"B"}`),
			{e2eContentChunk("done")},
		},
	}
	e := startE2E(t, e2eOpts{Turns: turns})
	e2eReady(t, e)

	e.submit("alpha one")
	e.waitScreen("[g] grant for this session", 30*time.Second)
	e.key("g")
	e2eWaitTurns(t, e, 1)

	ids, err := e.chatIDs()
	if err != nil {
		t.Fatalf("chatIDs: %v", err)
	}
	if len(ids) == 0 {
		t.Fatalf("chatIDs is empty, sessions dir %q has no session file", e.sessions)
	}
	e.quit()

	evs, err := e.events()
	if err != nil {
		t.Fatalf("events() on run 1 read error: %v", err)
	}
	found := false
	for _, ev := range evs {
		if ev.Kind == event.KindApprovalResolved && ev.Payload.(event.ApprovalResolved).Outcome == "granted" {
			found = true
		}
	}
	if !found {
		t.Fatalf("run 1 has no approval_resolved granted event")
	}

	e2 := startE2E(t, e2eOpts{Reuse: e, ResumeID: ids[0], Turns: turns})
	e2.waitScreen("type a task", 30*time.Second)
	if scr := e2.screen(); strings.Contains(scr, "internal error") || strings.Contains(scr, "durable emit failed") {
		t.Fatalf("run 2 screen shows a durable/turn error:\n%s", e2.tail(2000))
	}

	m := e2.mark()
	e2.submit("bravo two")
	asked := false
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(e2.screenSince(m), "[y] proceed") {
			asked = true
			break
		}
		if e2.countKind(event.KindTurnCompleted) >= 2 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if asked {
		e2.key("y")
	}
	e2eWaitTurns(t, e2, 2)
	t.Logf("Run 2 asked again: %v.", asked)
	e2eFileIs(t, e2, filepath.Join(e2.workdir, "b.txt"), "B")
	if _, err := e2.events(); err != nil {
		t.Fatalf("events() on run 2 read error: %v", err)
	}
}
