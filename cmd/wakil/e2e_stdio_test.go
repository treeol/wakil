package main

import (
	"testing"
	"time"
)

// e2eAssertStdioClean fails if anything wrote to os.Stdout/os.Stderr.
func e2eAssertStdioClean(t *testing.T, e *e2e, stage string) {
	t.Helper()
	stdout, stderr := e.outOfBand()
	if stdout != "" || stderr != "" {
		t.Fatalf("stage %q: out-of-band stdout=%q stderr=%q\n%s", stage, stdout, stderr, e.tail(2000))
	}
}

// TestE2EStdioCleanAcrossNewResume checks no out-of-band stdio across sessions.
func TestE2EStdioCleanAcrossNewResume(t *testing.T) {
	turns := map[string][][]string{
		"stdio one":   {{e2eContentChunk("done")}},
		"stdio two":   {{e2eContentChunk("done")}},
		"stdio three": {{e2eContentChunk("done")}},
	}
	e := startE2E(t, e2eOpts{Turns: turns})
	e2eReady(t, e)
	// The deferred startup probe (GET /props) runs after the first frame;
	// wait for the fake server to have served it before checking stdio.
	e.waitProbes(30 * time.Second)
	e2eAssertStdioClean(t, e, "ready")

	e.submit("stdio one")
	e2eWaitTurns(t, e, 1)
	e2eAssertStdioClean(t, e, "first turn")

	ids, err := e.chatIDs()
	if err != nil {
		t.Fatalf("chatIDs: %v", err)
	}
	if len(ids) == 0 {
		t.Fatalf("chatIDs is empty, sessions dir %q has no session file", e.sessions)
	}
	first := ids[0]

	e.submitRotation("/new")
	e2eWaitSessions(t, e, 2)
	e2eAssertStdioClean(t, e, "after /new")

	e.submit("stdio two")
	e2eWaitTurns(t, e, 2)
	e2eAssertStdioClean(t, e, "second turn")

	e.submitRotation("/resume " + first)
	e2eWaitSessions(t, e, 3)
	e2eAssertStdioClean(t, e, "after /resume")

	e.submit("stdio three")
	e2eWaitTurns(t, e, 3)
	e2eAssertStdioClean(t, e, "third turn")
}
