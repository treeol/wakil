package wiring

// Headless runs print the collapse and async notices: one plain line each, on
// the same stream as the tool lines, never inside the answer text. The tests
// drive runSingleTask (the real headless path: host, adapter, event consumer)
// against a fake SSE backend.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/proxy"
)

// outputLines returns the "line" of every {"type":"output"} record, in order.
func outputLines(t *testing.T, out string) []string {
	t.Helper()
	var lines []string
	for _, ev := range outputEvents(t, out) {
		if ev["type"] == "output" {
			lines = append(lines, fmt.Sprint(ev["line"]))
		}
	}
	return lines
}

func countLines(lines []string, sub string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

// longSessionApp is a fakeApp in long-session mode with an isolated toolcache
// and a prior turn whose finished work the model-called collapse archives.
func longSessionApp(t *testing.T, url string) *agent.App {
	t.Helper()
	t.Setenv("WAKIL_SESSIONS_DIR", "")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	app := fakeApp(url)
	app.Costs = proxy.NewCostTracker()
	if err := app.SetContextMode("long-session"); err != nil {
		t.Fatal(err)
	}
	// The turn-end sweep needs 100 kB of work to collapse (it does not here);
	// the explicit collapse_context uses min(this, 512) bytes.
	app.Cfg.ContextCollapseMinBytes = 100000
	return app
}

func assertNoEscape(t *testing.T, out string) {
	t.Helper()
	if strings.ContainsRune(out, 0x1b) {
		t.Errorf("headless output carries an escape sequence: %q", out)
	}
}

// TestHeadlessCollapseNoticeOneLine: a turn that ends in one collapse (the
// turn-end sweep; the model calls nothing) gives exactly one notice line in a
// headless run, after the answer and on a line of its own.
func TestHeadlessCollapseNoticeOneLine(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	bulk := strings.Repeat("padding line of file content\n", 400)
	srv := sseServer(t,
		toolCallFrames("w1", "write_file", fmt.Sprintf(`{"path":"a.txt","content":%q}`, bulk)),
		toolCallFrames("w2", "write_file", fmt.Sprintf(`{"path":"b.txt","content":%q}`, bulk)),
		[]string{contentChunk("the final answer")},
	)
	defer srv.Close()
	app := longSessionApp(t, srv.URL)
	app.Cfg.ContextCollapseMinBytes = 1

	var out strings.Builder
	code := runSingleTask(context.Background(), app, "write two files", HeadlessOptions{Auto: true}, &out)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0; output: %s", code, out.String())
	}
	lines := outputLines(t, out.String())
	if n := countLines(lines, "collapsed request"); n != 1 {
		t.Fatalf("collapse notice lines = %d, want 1; lines: %q", n, lines)
	}
	for _, l := range lines {
		if strings.Contains(l, "collapsed request") && !strings.HasPrefix(l, "· collapsed request 1:") {
			t.Errorf("notice line has other text on it: %q", l)
		}
		if strings.Contains(l, "the final answer") && l != "the final answer" {
			t.Errorf("answer line carries other text: %q", l)
		}
	}
	assertNoEscape(t, out.String())
}

// TestHeadlessModelCalledCollapseNotice: the model-called collapse_context in a
// headless run prints its notice once, on a line of its own between the tool
// line and the answer. (The turn-end sweep then upgrades the partial record
// and prints its own notice: a second, separate collapse.)
func TestHeadlessModelCalledCollapseNotice(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	bulk := strings.Repeat("padding line of file content\n", 400)
	srv := sseServer(t,
		toolCallFrames("w1", "write_file", fmt.Sprintf(`{"path":"a.txt","content":%q}`, bulk)),
		toolCallFrames("w2", "write_file", fmt.Sprintf(`{"path":"b.txt","content":%q}`, bulk)),
		toolCallFrames("cc", "collapse_context", `{"summary":"wrote a.txt and b.txt"}`),
		[]string{contentChunk("the final answer")},
	)
	defer srv.Close()
	app := longSessionApp(t, srv.URL)

	var out strings.Builder
	code := runSingleTask(context.Background(), app, "write two files", HeadlessOptions{Auto: true}, &out)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0; output: %s", code, out.String())
	}
	lines := outputLines(t, out.String())
	// The model-called notice drops the two writes and nothing else; the
	// turn-end one also drops the collapse_context call itself.
	modelCalled := 0
	for _, l := range lines {
		if strings.Contains(l, "· collapsed request 1:") && strings.Contains(l, "write_file ×2)") && !strings.Contains(l, "collapse_context") {
			modelCalled++
		}
		if strings.Contains(l, "the final answer") && l != "the final answer" {
			t.Errorf("answer line carries other text: %q", l)
		}
	}
	if modelCalled != 1 {
		t.Fatalf("model-called collapse notice lines = %d, want 1; lines: %q", modelCalled, lines)
	}
	idx := func(sub string) int {
		for i, l := range lines {
			if strings.Contains(l, sub) {
				return i
			}
		}
		return -1
	}
	if a, b, c := idx("· collapse_context"), idx("· collapsed request 1:"), idx("the final answer"); !(a >= 0 && b >= 0 && b < a && a < c) {
		t.Errorf("want notice (made inside the call) < tool line < answer, got %d, %d, %d in %q", b, a, c, lines)
	}
	assertNoEscape(t, out.String())
}

// TestHeadlessAsyncNoticeOneLine: one async result delivered into a headless
// turn gives exactly one notice line, apart from the answer text.
func TestHeadlessAsyncNoticeOneLine(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	// Round 1 starts a background job that reports its exit (the fake
	// executor's job is already gone when the reaper looks), round 2 waits for
	// it, and the loop top that drains the finished job prints the notice once.
	srv := sseServer(t,
		toolCallFrames("bg1", "run_background", `{"command":"true","label":"quick","notify_on_exit":true}`),
		toolCallFrames("wc1", "wait_for_completion", `{}`),
		[]string{contentChunk("the final answer")},
	)
	defer srv.Close()
	app := fakeApp(srv.URL)
	app.Costs = proxy.NewCostTracker()

	var out strings.Builder
	code := runSingleTask(context.Background(), app, "wait for the job", HeadlessOptions{Auto: true}, &out)
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0; output: %s", code, out.String())
	}
	lines := outputLines(t, out.String())
	if n := countLines(lines, "async results delivered"); n != 1 {
		t.Fatalf("async notice lines = %d, want 1; lines: %q", n, lines)
	}
	for _, l := range lines {
		if strings.Contains(l, "async results delivered") && l != "· async results delivered" {
			t.Errorf("notice line has other text on it: %q", l)
		}
		if strings.Contains(l, "the final answer") && l != "the final answer" {
			t.Errorf("answer line carries other text: %q", l)
		}
	}
	assertNoEscape(t, out.String())
}

// TestHeadlessAutoNoteStaysQuiet: the "⚡ auto:" note of every auto-approved
// tool call is a session note too, but not a notice: it must not appear in a
// headless run (that would change the output of every --auto run).
func TestHeadlessAutoNoteStaysQuiet(t *testing.T) {
	srv := sseServer(t,
		toolCallFrames("w1", "write_file", `{"path":"a.txt","content":"one"}`),
		[]string{contentChunk("done")},
	)
	defer srv.Close()
	app := fakeApp(srv.URL)
	app.SetAutoApprove(true)

	var out strings.Builder
	code := runSingleTask(context.Background(), app, "write a file", HeadlessOptions{Auto: true}, &out)
	if code != ExitOK {
		t.Fatalf("exit = %d; output: %s", code, out.String())
	}
	if strings.Contains(out.String(), "⚡") {
		t.Errorf("an auto/grant session note reached the headless output: %s", out.String())
	}
}

// TestWriteNotice: only a notice is printed; it is one line of its own even
// when an answer line is still open, newlines in the text become spaces, and
// no escape sequence survives.
func TestWriteNotice(t *testing.T) {
	var out strings.Builder
	hw := NewHeadlessWriter(&out)
	_, _ = hw.Write([]byte("answer so far"))
	writeNotice(hw, event.SessionNote{Text: "⚡ auto: write_file\n  detail"}) // not a notice: ignored
	writeNotice(hw, event.SessionNote{Text: "\x1b[2m· first\nsecond\x1b[0m", Notice: true})
	writeNotice(hw, event.SessionNote{Text: " \n ", Notice: true}) // empty: prints nothing
	_, _ = hw.Write([]byte(" and the rest\n"))
	hw.Flush()
	got := outputLines(t, out.String())
	want := []string{"answer so far", "· first second", " and the rest"}
	if len(got) != len(want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
	assertNoEscape(t, out.String())
}

// TestProjectSysNoteCarriesNotice: the projection copies the Notice flag, and
// a note without it stays a plain session note.
func TestProjectSysNoteCarriesNotice(t *testing.T) {
	for _, tc := range []struct{ notice bool }{{true}, {false}} {
		emit := &fakeSessionEmitter{}
		projectAgentEvent(emit, "", agent.SysNoteMsg{Text: "x", Notice: tc.notice})
		got := emit.notifyList()
		if len(got) != 1 || got[0].payload.(event.SessionNote).Notice != tc.notice {
			t.Errorf("Notice=%v: projected %+v", tc.notice, got)
		}
	}
}
