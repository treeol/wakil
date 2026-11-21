package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/testutil"
)

// TestE2EMidTurnCollapseNoticeOneLineOnScreen drives one real mid-turn
// collapse through the real runTUI path (agent → EventSink → session note →
// TUI) and reads the virtual screen: the collapse renders exactly one notice
// line carrying the "[mid-turn]" marker, and the same text is not also in the
// assistant stream (a copy there would show up as a second line). The number
// of collapses is taken from the outgoing requests (paired batches drop once),
// not assumed.
func TestE2EMidTurnCollapseNoticeOneLineOnScreen(t *testing.T) {
	// 14 writes cross the 30000-token ceiling once (the same arithmetic as
	// TestE2EMidTurnCollapseKeepsWork, which needs 30 writes for several
	// collapses); the retained tail then stays under the ceiling to the end.
	const writeCount = 14
	script := make([][]string, 0, writeCount+2)
	for i := 1; i <= writeCount; i++ {
		id := fmt.Sprintf("tw%02d", i)
		sentinel := fmt.Sprintf("SENTINEL-%02d-", i)
		content := strings.Repeat(sentinel, 5000/len(sentinel)+1)[:5000]
		args, err := json.Marshal(map[string]string{"path": fmt.Sprintf("dir/tw%02d.txt", i), "content": content})
		if err != nil {
			t.Fatalf("marshal args %d: %v", i, err)
		}
		script = append(script, e2eToolCallFrames(id, "write_file", string(args)))
	}
	script = append(script, []string{e2eContentChunk("all files written")}, []string{e2eContentChunk("ok")})

	// Wide enough that the notice (it carries the archive path) is not wrapped.
	vs := testutil.NewVScreen(400, 300)
	e := startE2E(t, e2eOpts{width: 400, height: 300, tee: vs, script: script, extra: map[string]any{
		"context_prune":                   "off",
		"context_collapse_ceiling_tokens": 30000,
		"context_collapse_min_bytes":      1,
	}})
	e2eReady(t, e)
	e.submit(fmt.Sprintf("write %d numbered files", writeCount))
	e2eAnswerApprovals(t, e, 1)
	// The event log is ahead of the screen: wait for the final answer.
	e.waitScreen("all files written", 30*time.Second)
	// Wait for the mid-turn collapse notice to render on the screen (it
	// carries "[mid-turn]") instead of a fixed sleep. The notice arrives
	// after the turn-end answer text; a fixed sleep can race on a loaded
	// runner, and polling for the content means the assertions below read
	// a settled screen.
	e.waitScreen("[mid-turn]", 5*time.Second)

	// Collapses that happened, from the requests the model received.
	drops, prev := 0, 0
	for _, body := range e.requests() {
		g := len(midTurnGroups(verifyDecodeMessages(t, body)))
		if g < prev {
			drops++
		}
		prev = g
	}
	if drops != 1 {
		t.Fatalf("test premise: want exactly one mid-turn collapse in the requests, got %d", drops)
	}

	var notice, midTurn []string
	for _, row := range strings.Split(vs.Screen(), "\n") {
		if strings.Contains(row, "collapsed request") {
			notice = append(notice, row)
			if strings.Contains(row, "[mid-turn]") {
				midTurn = append(midTurn, row)
			}
		}
	}
	if len(midTurn) != drops {
		t.Fatalf("mid-turn notice lines on screen = %d, want %d (one per collapse):\n%s", len(midTurn), drops, strings.Join(notice, "\n"))
	}
	// The turn-end collapse is a separate event with its own line (no marker);
	// nothing else may print a collapse line.
	if len(notice) > drops+1 {
		t.Errorf("collapse lines on screen = %d, want at most %d (mid-turn + turn-end):\n%s", len(notice), drops+1, strings.Join(notice, "\n"))
	}
	line := midTurn[0]
	for _, want := range []string{"· collapsed request 1:", "tokens →", "sent (msgs", "write_file ×"} {
		if !strings.Contains(line, want) {
			t.Errorf("notice line missing %q:\n%s", want, line)
		}
	}
	// Every frame the user saw holds the line at most once.
	for i, f := range vs.AllFrames() {
		if c := strings.Count(f, "[mid-turn]"); c > drops {
			t.Fatalf("frame %d shows the mid-turn notice %d times, want at most %d", i, c, drops)
		}
	}
}
