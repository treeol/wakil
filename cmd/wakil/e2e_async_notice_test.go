package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	// The job ends only when the test creates the flag file, after round 2's
	// reasoning is on the screen. A job that ended at once ("true") could
	// finish before round 2's request on a slow runner, and the notice would
	// then come before round 2's thought line instead of between the two.
	// The loop is bounded (30 s) so a failed test leaves no job behind.
	flagDir := t.TempDir()
	flag := filepath.Join(flagDir, "release-job")
	jobCmd := fmt.Sprintf(`i=0; while [ ! -e %q ] && [ $i -lt 600 ]; do i=$((i+1)); sleep 0.05; done`, flag)
	jobArgs, err := json.Marshal(map[string]any{"command": jobCmd, "label": "quick", "notify_on_exit": true})
	if err != nil {
		t.Fatal(err)
	}
	script := [][]string{
		e2eToolCallFrames("bg1", "run_background", string(jobArgs)),
		append([]string{e2eReasoningChunk(reasoningHead), e2eReasoningChunk(reasoningTail)},
			e2eToolCallFrames("wc1", "wait_for_completion", `{}`)...),
		{e2eReasoningChunk("The job finished, so I can answer."), e2eContentChunk("final answer text")},
	}
	vs := testutil.NewVScreen(160, 200)
	e := startE2E(t, e2eOpts{width: 160, height: 200, tee: vs, script: script})
	e2eReady(t, e)
	e.submit("start a job and wait for it")
	// Round 2's reasoning is folded into a thought line once its tool call
	// arrives, which is after round 2's loop top: only then may the job end.
	// e2eAnswerApprovals returns when the turn is complete, and the turn needs
	// the job, so the flag is written from a goroutine that polls the screen.
	released := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(60 * time.Second)
		for time.Now().Before(deadline) {
			if strings.Contains(e.screen(), "· thought") {
				released <- os.WriteFile(flag, nil, 0o644)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		released <- fmt.Errorf("round 2's thought line never appeared")
	}()
	e2eAnswerApprovals(t, e, 1)
	if err := <-released; err != nil {
		t.Fatal(err)
	}
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
