package agent

// T5 review (fable): a partial collapse must upgrade to a full collapse when the
// full form is smaller than the form it replaces, and it must not stay stuck
// because the upgrade is measured against the whole original turn. The stored
// form being replaced is the partial one, not the original turn.

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// A finished turn whose working set was already partially collapsed. Its
// visible form (U + checkpoint + the W that is still in context + A) is what a
// full upgrade replaces. The full collapse must be applied when it is smaller
// than that visible form.
func TestPartialUpgradeIsNotStuckBehindOriginalSize(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextCollapseMinBytes = 0

	// Turn 1: U, two tool rounds (a, b), the collapse_context call that
	// boundaries the partial record (call id "cc"), a third round (c), and A.
	// The partial record elides everything up to and including the boundary
	// message, so the visible form is: U + checkpoint + round c + A.
	conv := []proxy.Message{
		{Role: "user", Content: StrPtr("investigate"), Turn: 1},
		tcMsg("t1", "read_file", `{"path":"a.go"}`),
		trMsg("t1", "read_file", strings.Repeat("a", 4000)),
		tcMsg("t2", "read_file", `{"path":"b.go"}`),
		trMsg("t2", "read_file", strings.Repeat("b", 4000)),
		tcMsg("cc", "collapse_context", `{"summary":"a and b read"}`),
		tcMsg("t3", "read_file", `{"path":"c.go"}`),
		trMsg("t3", "read_file", strings.Repeat("c", 4000)),
		{Role: "assistant", Content: StrPtr("Done: summary of a, b and c."), Turn: 1},
	}
	// tcMsg/trMsg do not set Turn; the projection passes Turn==0 messages
	// through untouched, so stamp every message of the turn (as appendConv does).
	for i := range conv {
		conv[i].Turn = 1
	}
	app.Conv = conv
	app.turnSeq, app.curTurn = 1, 1

	// A partial record already exists for the first two tool rounds (what
	// collapse_context writes mid-turn); the working set still in context is
	// the third tool round plus the answer.
	app.stateMu.Lock()
	app.collapsed = map[int]CollapseRecord{1: {
		Turn: 1, Partial: true, BoundaryCallID: "cc", Archive: "/nonexistent/x",
		Note: "[request 1 working set collapsed — 2 tool calls]", Summary: "a and b read",
	}}
	app.stateMu.Unlock()
	before := app.contextProjection()
	beforeBytes := 0
	for _, m := range before {
		beforeBytes += messageSize(m)
	}
	if len(before) >= len(conv) {
		t.Fatalf("test premise: the partial record must elide the first two rounds (projection %d msgs, turn %d)", len(before), len(conv))
	}

	app.collapseFinishedTurns()

	app.stateMu.RLock()
	rec, ok := app.collapsed[1]
	app.stateMu.RUnlock()
	if !ok {
		t.Fatal("record vanished")
	}
	if rec.Partial {
		t.Fatalf("a full upgrade that shrinks the visible form must complete; the turn is stuck partial (visible %d bytes)", beforeBytes)
	}
}

// When the full form is NOT smaller than the form it replaces, the partial
// record is kept as it was: the upgrade falls back to the previous form
// instead of leaving the turn in an undefined state.
func TestPartialUpgradeFallsBackToPreviousForm(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextCollapseMinBytes = 0

	conv := []proxy.Message{
		{Role: "user", Content: StrPtr("hi"), Turn: 1},
		tcMsg("t1", "run_shell", `{}`),
		trMsg("t1", "run_shell", "ok"),
		{Role: "assistant", Content: StrPtr("done"), Turn: 1},
	}
	for i := range conv {
		conv[i].Turn = 1
	}
	app.Conv = conv
	app.turnSeq, app.curTurn = 1, 1
	partial := CollapseRecord{Turn: 1, Partial: true, BoundaryCallID: "t1", Note: "[partial]", Summary: "s"}
	app.stateMu.Lock()
	app.collapsed = map[int]CollapseRecord{1: partial}
	app.stateMu.Unlock()

	app.collapseFinishedTurns()

	app.stateMu.RLock()
	rec := app.collapsed[1]
	app.stateMu.RUnlock()
	if !rec.Partial || rec.Note != partial.Note {
		t.Errorf("a non-shrinking upgrade must keep the previous partial form unchanged, got %+v", rec)
	}
}

// The gate must measure the form being replaced. Here the visible partial form
// is smaller than the full collapse (the upgrade would inflate the request),
// while the full collapse is still smaller than the ORIGINAL turn. Comparing
// against the original turn would apply the upgrade and grow the request; the
// correct baseline is the visible partial form, so the upgrade is refused and
// the partial record stays.
func TestPartialUpgradeRefusedWhenItWouldInflateVisibleForm(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextCollapseMinBytes = 0

	// Two big rounds before the boundary, one tiny round after, a tiny answer.
	// The visible partial form is U + checkpoint + the tiny round + A: small.
	conv := []proxy.Message{
		{Role: "user", Content: StrPtr("go"), Turn: 1},
		tcMsg("t1", "read_file", `{"path":"a.go"}`),
		trMsg("t1", "read_file", strings.Repeat("a", 8000)),
		tcMsg("t2", "read_file", `{"path":"b.go"}`),
		trMsg("t2", "read_file", strings.Repeat("b", 8000)),
		tcMsg("cc", "collapse_context", `{"summary":"ab"}`),
		tcMsg("t3", "run_shell", `{}`),
		trMsg("t3", "run_shell", "ok"),
		{Role: "assistant", Content: StrPtr("done"), Turn: 1},
	}
	for i := range conv {
		conv[i].Turn = 1
	}
	app.Conv = conv
	app.turnSeq, app.curTurn = 1, 1
	app.stateMu.Lock()
	app.collapsed = map[int]CollapseRecord{1: {
		Turn: 1, Partial: true, BoundaryCallID: "cc", Archive: "/nonexistent/x",
		Note: "[partial]", Summary: "ab",
	}}
	app.stateMu.Unlock()

	visibleBefore := 0
	for _, m := range app.contextProjection() {
		visibleBefore += messageSize(m)
	}
	origBytes := 0
	for _, m := range conv {
		origBytes += messageSize(m)
	}
	if visibleBefore >= origBytes {
		t.Fatalf("test premise: the partial form must be smaller than the original (%d vs %d)", visibleBefore, origBytes)
	}

	app.collapseFinishedTurns()

	after := 0
	for _, m := range app.contextProjection() {
		after += messageSize(m)
	}
	if after > visibleBefore {
		t.Fatalf("the upgrade inflated the request: visible %d -> %d bytes", visibleBefore, after)
	}
}
