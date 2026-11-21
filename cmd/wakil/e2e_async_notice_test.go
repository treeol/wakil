package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/testutil"
)

func e2eReasoningChunk(s string) string {
	return fmt.Sprintf(`{"choices":[{"delta":{"reasoning_content":%q},"finish_reason":null}]}`, s)
}

// TestE2EAsyncNoticeOnOwnLine drives a real async completion through the real
// runTUI path. Round 1 starts a background job with notify_on_exit; round 2
// streams reasoning and calls wait_for_completion, so the turn suspends until
// the job's result is in the inbox; the loop top of round 3 drains it and
// prints "async results delivered" while reasoning and answer text stream
// again. On the virtual screen the reasoning sentences must stay unbroken, and
// the notice must be one line of its own (never inside the reasoning or the
// answer).
func TestE2EAsyncNoticeOnOwnLine(t *testing.T) {
	const reasoningHead = "Now I have a clear picture of the repository"
	const reasoningTail = " and the next step is clear."
	script := [][]string{
		e2eToolCallFrames("bg1", "run_background", `{"command":"true","label":"quick","notify_on_exit":true}`),
		append([]string{e2eReasoningChunk(reasoningHead), e2eReasoningChunk(reasoningTail)},
			e2eToolCallFrames("wc1", "wait_for_completion", `{}`)...),
		{e2eReasoningChunk("The job finished, so I can answer."), e2eContentChunk("final answer text")},
	}
	vs := testutil.NewVScreen(160, 200)
	e := startE2E(t, e2eOpts{width: 160, height: 200, tee: vs, script: script})
	e2eReady(t, e)
	e.submit("start a job and wait for it")
	e2eAnswerApprovals(t, e, 1)
	e.waitScreen("final answer text", 30*time.Second)
	e.waitScreen("async results delivered", 5*time.Second)

	var noticeRows []string
	for _, row := range strings.Split(vs.Screen(), "\n") {
		if strings.Contains(row, "async results delivered") {
			noticeRows = append(noticeRows, row)
		}
	}
	if len(noticeRows) != 1 {
		t.Fatalf("notice rows on screen = %d, want 1:\n%s", len(noticeRows), vs.Screen())
	}
	row := noticeRows[0]
	for _, bad := range []string{"clear", "final answer", "finished"} {
		if strings.Contains(row, bad) {
			t.Errorf("notice shares a screen line with %q text: %q", bad, row)
		}
	}
	// The scripted server answers instantly, so the live reasoning body is
	// folded into "· thought (~N tokens)" before a frame can show it; the
	// folded structure is what the screen holds. Round 2's reasoning (head and
	// tail, one block) is one thought line BEFORE the notice, round 3's is one
	// thought line AFTER it, and the answer comes last, each on its own row. A
	// notice that went through the answer stream would sit in or next to the
	// answer text, or split the reasoning into more blocks.
	screen := vs.Screen()
	var rows []string
	for _, r := range strings.Split(screen, "\n") {
		r = strings.TrimSpace(strings.Trim(strings.TrimSpace(r), "│"))
		if strings.HasPrefix(r, "· thought") || strings.Contains(r, "async results delivered") || strings.Contains(r, "final answer text") {
			rows = append(rows, r)
		}
	}
	if len(rows) != 4 || !strings.HasPrefix(rows[0], "· thought") || !strings.Contains(rows[1], "async results delivered") ||
		!strings.HasPrefix(rows[2], "· thought") || !strings.Contains(rows[3], "final answer text") {
		t.Fatalf("want rows [thought, notice, thought, answer], got %q\n%s", rows, screen)
	}
	if strings.TrimSpace(rows[1]) != "· async results delivered" {
		t.Errorf("notice row has other text on it: %q", rows[1])
	}
	if strings.TrimSpace(rows[3]) != "final answer text" {
		t.Errorf("answer row has other text on it: %q", rows[3])
	}
	// The notice comes after the approved background start (the tool round).
	if i, j := strings.Index(screen, "[approved]"), strings.Index(screen, "async results delivered"); i < 0 || j < i {
		t.Errorf("notice (at %d) is not after the approved tool start (at %d)", j, i)
	}
	for i, f := range vs.AllFrames() {
		if c := strings.Count(f, "async results delivered"); c > 1 {
			t.Fatalf("frame %d shows the notice %d times", i, c)
		}
	}
}
