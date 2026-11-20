package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
	"github.com/treeol/wakil/internal/trace"
)

// TestMidTurnCollapseFiresInsideLongTurn drives the real turn loop
// (streamTurn with the scripted fake model) through one turn whose tool
// results push the projected request past the absolute ceiling. The
// completed prefix collapses before the next model request: the request
// that follows is below the ceiling, still contains the user's task
// message word for word, and keeps tool-call/tool-result pairs intact.
//
// The file content is short-per-line (one short line per row) so the
// smart-read preview (isLikelyNoisy) never engages: results arrive
// verbatim at ~69k chars each.
func TestMidTurnCollapseFiresInsideLongTurn(t *testing.T) {
	const taskText = "do the refactor across every file in the tree"
	big := ""
	for i := 0; i < 2450; i++ {
		big += fmt.Sprintf("line %d padding abcdefghij\n", i)
	}
	// Ceiling: 20_000 tokens → 80_000 chars. Three rounds of ~69k chars
	// each accumulate ~207k inside one turn, crossing the ceiling before
	// the fourth request, while staying under the 364k hard max.
	frames := [][]string{
		toolCallFrames("c1", "read_file", `{"path":"a.go"}`),
		toolCallFrames("c2", "read_file", `{"path":"b.go"}`),
		toolCallFrames("c3", "read_file", `{"path":"c.go"}`),
		toolCallFrames("c4", "read_file", `{"path":"d.go"}`),
		[]string{contentChunk("done")},
	}
	srv := sseServer(t, frames...)
	t.Cleanup(srv.Close)
	exe := newFakeExecutor()
	exe.files["a.go"] = big
	exe.files["b.go"] = big
	exe.files["c.go"] = big
	exe.files["d.go"] = big
	app := newTestApp(srv.URL, exe, func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextCollapseCeilingTokens = 20000
	app.Cfg.ContextCollapseMinBytes = 2000
	app.Cfg.ToolResultCap = 0 // keep results verbatim so size accumulates
	app.Cfg.TurnToolBudget = 0
	app.Cfg.ReadFileSizeLimit = 1 << 20
	app.Cfg.ContextMode = "long-session"
	app.CtxLimit = ContextLimit{NCtx: 128000, Source: "backend", ReasoningBudget: 4096, AnswerMargin: 4096}
	app.Cfg.HardMaxBytes = 400000 // keep the guard from tripping mid-test
	app.curTurn = 1
	app.turnSeq = 1
	app.appendConv(proxy.Message{Role: "system", Content: StrPtr("preamble"), Turn: 0})
	// appendConv stamps Turn itself via curTurn.
	app.appendConv(proxy.Message{Role: "user", Content: StrPtr(taskText)})
	var traceToolCalls []trace.ToolTrace
	out := new(strings.Builder)
	app.Out = out
	_, _, err := app.streamTurn(context.Background(), taskText, nil, &traceToolCalls)
	if err != nil {
		t.Fatalf("streamTurn: %v", err)
	}
	// The projection the next request would be built from must still carry
	// the task verbatim. The request that follows the collapse is below
	// the ceiling by construction (the elision removes the prefix that
	// crossed it); the end-of-turn request is bounded by hard max, which
	// the guard enforces in-loop — a failure there surfaces as
	// ErrCurrentTurnTooLarge, not silent growth.
	proj := app.contextProjection()
	found := false
	for _, m := range proj {
		if m.Role == "user" && m.Content != nil && strings.Contains(*m.Content, taskText) {
			found = true
		}
	}
	if !found {
		t.Fatalf("user task message not present verbatim in the post-collapse request:\n%s", out.String())
	}
	// Mid-turn collapse printed its line in the turn's Out sink. The line
	// carries the same shape as the collapse_context tool's but is marked
	// mid-turn.
	if !strings.Contains(out.String(), "collapsed request 1:") || !strings.Contains(out.String(), "[mid-turn]") {
		t.Fatalf("mid-turn collapse line missing; out=%q", out.String())
	}
	assertPairing(t, proj)
	assertNoOrphanedToolCalls(t, proj)
}

// buildRepeatCollapseApp returns an app in a long turn with a large task
// message and rounds small completed read rounds. The ceiling keeps the
// projection over it, and the kept tail spans more than one round, which is
// the case where the repeat guard used to compare the wrong index.
func buildRepeatCollapseApp(t *testing.T, rounds int) (*App, *strings.Builder) {
	t.Helper()
	isolateToolCache(t)
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.curTurn, app.turnSeq = 1, 1
	app.Cfg.ContextCollapseCeilingTokens = 3000
	app.Cfg.ContextCollapseMinBytes = 1
	app.Cfg.HardMaxBytes = 400000
	out := new(strings.Builder)
	app.Out = out
	app.appendConv(proxy.Message{Role: "user", Content: StrPtr(strings.Repeat("big task ", 3000))})
	for i := 1; i <= rounds; i++ {
		id := fmt.Sprintf("c%d", i)
		app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{{ID: id, Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}}}})
		app.appendConv(proxy.Message{Role: "tool", ToolCallID: id, Name: "read_file", Content: StrPtr(strings.Repeat("small line of tool output\n", 60))})
	}
	return app, out
}

// countSpills counts the turn-1 partial spill files in the session's
// toolcache directory. The directory is shared across runs, so callers take
// a baseline with countSpills before the calls under test.
func countSpills(t *testing.T, app *App) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(wtools.ToolCacheRoot(), app.chatID()))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "turn-1-part") {
			n++
		}
	}
	return n
}

// TestMidTurnCollapseRepeatOnUnchangedConversation reproduces the repeated
// "collapsed request" notice: four pass calls on an unchanged conversation
// with a multi-round tail must give one collapse, one spill file and one
// notice line.
func TestMidTurnCollapseRepeatOnUnchangedConversation(t *testing.T) {
	app, out := buildRepeatCollapseApp(t, 12)
	before := countSpills(t, app)
	for i := 0; i < 4; i++ {
		app.collapseMidTurnPrefix()
	}
	if got := app.midTurnCollapses; got != 1 {
		t.Errorf("midTurnCollapses = %d, want 1", got)
	}
	if got := strings.Count(out.String(), "collapsed request 1:"); got != 1 {
		t.Errorf("collapse notices = %d, want 1; out=%q", got, out.String())
	}
	if got := countSpills(t, app) - before; got != 1 {
		t.Errorf("spill files written = %d, want 1", got)
	}
}

// TestMidTurnCollapseTwoRealAdvancesGiveTwo checks the guard does not
// suppress a real advance: after one collapse, a new completed round moves
// the boundary and a second collapse runs.
func TestMidTurnCollapseTwoRealAdvancesGiveTwo(t *testing.T) {
	app, out := buildRepeatCollapseApp(t, 12)
	app.collapseMidTurnPrefix()
	if got := app.midTurnCollapses; got != 1 {
		t.Fatalf("first collapse: midTurnCollapses = %d, want 1", got)
	}
	// One new completed round moves the boundary once. A further pass with
	// no new round must add nothing. The new call ID must be unique in the
	// conversation: the record resolves its anchor by ID.
	id := "c13"
	app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{{ID: id, Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}}}})
	app.appendConv(proxy.Message{Role: "tool", ToolCallID: id, Name: "read_file", Content: StrPtr(strings.Repeat("small line of tool output\n", 60))})
	app.collapseMidTurnPrefix()
	app.collapseMidTurnPrefix()
	if got := app.midTurnCollapses; got != 2 {
		t.Errorf("after one new round: midTurnCollapses = %d, want 2", got)
	}
	if got := strings.Count(out.String(), "collapsed request 1:"); got != 2 {
		t.Errorf("collapse notices = %d, want 2; out=%q", got, out.String())
	}
}

// TestAutomaticCollapsePreservesExplicitCheckpoint covers an automatic
// boundary advance after the model saved a checkpoint in the same turn.
func TestAutomaticCollapsePreservesExplicitCheckpoint(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.curTurn, app.turnSeq = 1, 1
	app.Cfg.ContextCollapseCeilingTokens = 1000
	app.Cfg.ContextCollapseMinBytes = 1
	app.Out = new(strings.Builder)
	app.appendConv(proxy.Message{Role: "user", Content: StrPtr("original task")})
	for i := 1; i <= 4; i++ {
		id := fmt.Sprintf("c%d", i)
		app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{{ID: id, Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}}}})
		app.appendConv(proxy.Message{Role: "tool", ToolCallID: id, Name: "read_file", Content: StrPtr(strings.Repeat("result ", 1000))})
	}
	old := CollapseRecord{Turn: 1, Partial: true, BoundaryCallID: "c2", Summary: "explicit summary", NextSteps: "run the check", StashKey: "ctx/test/saved", StashDesc: "saved checkpoint", StashKeptFromPrior: true}
	app.collapsed = map[int]CollapseRecord{1: old}
	app.collapseMidTurnPrefix()
	got := app.collapsed[1]
	if got.BoundaryCallID == old.BoundaryCallID {
		t.Fatal("automatic boundary did not advance")
	}
	if got.Summary != old.Summary || got.NextSteps != old.NextSteps || got.StashKey != old.StashKey || got.StashDesc != old.StashDesc || !got.StashKeptFromPrior {
		t.Fatalf("explicit checkpoint lost: %+v", got)
	}
	assertNoOrphanedToolCalls(t, app.contextProjection())
}
