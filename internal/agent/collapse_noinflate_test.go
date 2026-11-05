package agent

// T5 (0.4.4): a collapse must never inflate the request. If the collapsed form
// is not smaller than the original, the original is kept and the notice says so.

import (
	"os"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// tinyTurn is a finished turn whose working set is a few bytes: the collapse
// note (tool counts + archive path) is larger than what it would replace.
func tinyTurn(turn int) []proxy.Message {
	conv := []proxy.Message{
		{Role: "user", Content: StrPtr("hi"), Turn: turn},
		tcMsg("tc1", "run_shell", `{}`),
		trMsg("tc1", "run_shell", "ok"),
		{Role: "assistant", Content: StrPtr("done"), Turn: turn},
	}
	for i := range conv {
		conv[i].Turn = turn
	}
	return conv
}

// Regression for the documented incident: a collapse inflated the request.
func TestCollapseSweep_NeverInflates(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	var out strings.Builder
	app.Out = &out
	app.Cfg.ContextCollapseMinBytes = 0 // would collapse anything finished
	app.Conv = tinyTurn(1)
	app.turnSeq = 1

	app.collapseFinishedTurns()

	if n, _ := app.CollapsedCount(); n != 0 {
		t.Fatalf("a non-shrinking turn must not be collapsed, CollapsedCount=%d", n)
	}
	proj := app.contextProjection()
	if len(proj) != len(tinyTurn(1)) {
		t.Fatalf("projection must be the original %d messages, got %d", len(tinyTurn(1)), len(proj))
	}
	if !strings.Contains(out.String(), "collapse skipped for request 1") ||
		!strings.Contains(out.String(), "not smaller") {
		t.Errorf("notice must say the collapse was skipped, got: %q", out.String())
	}
	// Idempotent: a second sweep over the same size does not re-archive or re-announce.
	before := out.String()
	app.collapseFinishedTurns()
	if out.String() != before {
		t.Errorf("second sweep re-announced: %q", out.String()[len(before):])
	}
	// The archive written for the comparison is removed.
	app.stateMu.RLock()
	_, haveRec := app.collapsed[1]
	app.stateMu.RUnlock()
	if haveRec {
		t.Error("no collapse record may exist for a skipped turn")
	}
}

// A turn that does shrink still collapses (the guard is not over-eager).
func TestCollapseSweep_StillCollapsesWhenSmaller(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Conv = oneCollapsedTurn(1)
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1
	app.collapseFinishedTurns()
	n, _ := app.CollapsedCount()
	if n != 1 {
		t.Fatalf("large working set must still collapse, CollapsedCount=%d", n)
	}
	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	if rec.Archive != "" {
		os.Remove(rec.Archive)
	}
}

// Explicit collapse_context: when the checkpoint would not be smaller than the
// working set the original is kept and the tool result says so.
func TestCollapseContext_NeverInflates(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextCollapseMinBytes = 0
	call := proxy.ToolCall{ID: "tcC", Type: "function", Function: proxy.FunctionCall{
		Name: "collapse_context", Arguments: `{"summary":"a fairly long summary of the work done so far, longer than the tiny working set it replaces"}`,
	}}
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("hi"), Turn: 1},
		tcMsg("tcA", "run_shell", `{}`),
		trMsg("tcA", "run_shell", "ok"),
		{Role: "assistant", ToolCalls: []proxy.ToolCall{call}, Turn: 1},
	}
	for i := 1; i < len(app.Conv); i++ {
		app.Conv[i].Turn = 1
	}
	app.turnSeq, app.curTurn = 1, 1

	res := app.handleCollapseContext(call)
	if !strings.Contains(res, "skipped") || !strings.Contains(res, "not smaller") || !strings.Contains(res, "kept unchanged") {
		t.Errorf("result must say the collapse was skipped: %q", res)
	}
	app.stateMu.RLock()
	_, ok := app.collapsed[1]
	app.stateMu.RUnlock()
	if ok {
		t.Error("no collapse record may exist after a skipped collapse")
	}
	if got := app.contextProjection(); len(got) != len(app.Conv) {
		t.Errorf("projection must equal the original: %d vs %d", len(got), len(app.Conv))
	}
}
