package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
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
