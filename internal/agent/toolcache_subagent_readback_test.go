package agent

// Block 0 reproduction for the "archived subagent results unreadable
// after a collapse" bug (bench collapse-long-v2-1-022611): after a
// dispatch_subagents batch delivers, mid-turn collapses elide the
// working set, and the model then re-reads per-child spill files that
// Wakil handed out.
//
// The production symptom was four consecutive read_file calls on
// dispatch_subagent-*.txt spill files failing with the host error
// "statat <chatID>/dispatch_subagent-….txt: no such file or directory"
// (Go's (*os.Root).Stat PathError op). The post-Mashūra mechanism is:
// the calls DID reach the host intercept, and the requested file was
// absent under the root — so either nothing deletes those files (the
// deletion audit in the plan is exhaustive) and the model presented a
// string that does not match any file on disk, or something is still
// deleting them. This test pins the invariants a Wakil bug would have
// to violate; if every one of them holds, the ship-able fix is the
// clearer host-missing message (Block 1(a)), and the report must say
// the ENOENT came from the model presenting a non-matching string.
//
// Setup mirrors the bench run: a fake (host-side) executor, turn-1
// with two mid-turn collapses, and an async (or synchronously-rendered)
// dispatch_subagents batch whose per-child spill paths are handed out
// in the tool result.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// TestSubagentSpillReadbackThroughTwoCollapses is the Block 0
// reproduction, strengthened per Mashūra round 2: it drives the
// production-shaped sequence — results enter Conv (so the mid-turn
// collapse archives, notes and indexes them exactly as in production),
// the envelope-level async delivery is exercised, and EVERY toolcache
// path string that survives into the MODEL-VISIBLE projection
// (a.contextProjection()) after the collapses must be readable through
// read_file exactly as presented. A path Wakil itself mangled or
// elided between hand-out and the model's copy fails this test.
func TestSubagentSpillReadbackThroughTwoCollapses(t *testing.T) {
	isolateToolCache(t)

	exe := newFakeExecutor()
	exe.confineErrFn = func(path string) error { return &confinementRejection{path: path} }
	app := newTestApp("http://unused", exe, func(_, _, _ string, _ bool) bool { return true })
	app.Client.ChatID = "bench-parent-chat"
	// Long-session with a low mid-turn ceiling so collapses fire, kept
	// below the hard max so the guard never trips mid-test. ToolResultCap
	// stays ON (default 12000) so cap/stub/eviction — the path-rewriting
	// sites — are exercised, not bypassed.
	app.SetContextMode("long-session")
	app.Cfg.ContextCollapseCeilingTokens = 3000
	app.Cfg.ContextCollapseMinBytes = 1
	app.Cfg.HardMaxBytes = 400000
	app.Cfg.ReadFileSizeLimit = 1 << 20
	app.curTurn, app.turnSeq = 1, 1

	app.appendConv(proxy.Message{Role: "user", Content: StrPtr(strings.Repeat("big task ", 3000))})
	round := func(id, name, args, result string) {
		app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{{ID: id, Function: proxy.FunctionCall{Name: name, Arguments: args}}}})
		app.appendConv(proxy.Message{Role: "tool", ToolCallID: id, Name: name, Content: StrPtr(result)})
	}
	// Twelve small completed rounds — the same shape that fires the
	// repeat-collapse guard tests (multi-round kept tail at ceiling 3000).
	for i := 1; i <= 12; i++ {
		round(fmt.Sprintf("c%d", i), "read_file", `{"path":"a.go"}`, strings.Repeat("small line of tool output\n", 60))
	}

	// First collapse (boundary advances). The collapse must actually fire:
	// a no-op collapse would make this test vacuous.
	app.collapseMidTurnPrefix()
	if app.midTurnCollapses != 1 {
		t.Fatalf("expected the first mid-turn collapse to fire; midTurnCollapses = %d", app.midTurnCollapses)
	}

	// Deliver the subagent batch as the async path does: results enter
	// Conv as a tool message, and the envelope render spills the merged
	// text (renderAsyncLine). Both hand out paths.
	rendered := renderSubagentResult(app, "Determine the withdrawal mapping rule for docs/note-01.txt",
		SubagentSummary{Objective: "bench tally", Findings: []Finding{{Summary: "subtotal 17", Location: "docs/note-01", Kind: "fact", Weight: "high"}}}, nil)
	childPath := wtools.ExtractSpillPath(rendered)
	if childPath == "" {
		t.Fatalf("ExtractSpillPath recovered nothing from rendered %q", rendered)
	}
	if _, err := os.Stat(childPath); err != nil {
		t.Fatalf("child spill %q does not exist at hand-out: %v", childPath, err)
	}
	round("c3", "dispatch_subagents", `{"tasks":["tally docs/note-01.txt"]}`, rendered)

	// Second collapse (boundary advances again): one new completed round.
	round("c13", "read_file", `{"path":"a.go"}`, strings.Repeat("small line of tool output\n", 60))
	app.collapseMidTurnPrefix()
	if app.midTurnCollapses != 2 {
		t.Fatalf("expected a second mid-turn collapse; midTurnCollapses = %d", app.midTurnCollapses)
	}

	// Walk the MODEL-VISIBLE projection: every toolcache path string
	// that survives into what the model actually sees must be readable
	// through read_file exactly as presented.
	proj := app.contextProjection()
	var presented []string
	for _, m := range proj {
		if m.Content != nil {
			for _, marker := range []string{"full content at: ", "subagent summary at: ", "full record: "} {
				rest := *m.Content
				for {
					i := strings.Index(rest, marker)
					if i < 0 {
						break
					}
					tail := rest[i+len(marker):]
					// The path runs to the next space, bracket or newline.
					end := len(tail)
					for j, r := range tail {
						if r == ' ' || r == ']' || r == '\n' || r == '(' {
							end = j
							break
						}
					}
					p := tail[:end]
					if p != "" {
						presented = append(presented, p)
					}
					rest = tail[end:]
				}
			}
		}
	}
	if len(presented) == 0 {
		t.Fatalf("no toolcache paths found in the model-visible projection — the walk is vacuous; projection carries no spill surface")
	}
	for _, p := range presented {
		if !wtools.IsToolCacheHostPath(p) {
			continue // not a host-intercepted path by construction
		}
		res := app.handleToolCall(backgroundCtx(), proxy.ToolCall{Function: proxy.FunctionCall{
			Name:      "read_file",
			Arguments: mustJSON(map[string]interface{}{"path": p}),
		}})
		if !res.ok {
			t.Errorf("read_file(%q) — a path the model actually sees after two collapses — not ok: %q", p, Truncate(res.text, 200))
		}
	}

	// The specific bench invariant: the per-child spill file handed out
	// by renderSubagentResult survives until a re-read after both
	// collapses. Content must be intact (findings included).
	if _, err := os.Stat(childPath); err != nil {
		t.Fatalf("child spill %q vanished before the re-read window closed: %v", childPath, err)
	}
	res := app.handleToolCall(backgroundCtx(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name:      "read_file_full",
		Arguments: mustJSON(map[string]string{"path": childPath}),
	}})
	if !res.ok || !strings.Contains(res.text, "subtotal 17") {
		t.Errorf("read_file_full(%q) on the child spill: ok=%v %q", childPath, res.ok, Truncate(res.text, 200))
	}
}

// TestChildSpillExistsAtHandoffAndThroughTeardown is the
// exists-at-hand-out / after-child-teardown half of the Block 0 pin:
// renderSubagentResult must hand out a path that exists at hand-out,
// and the child chat's toolcache dir must not be cleaned up by anything
// dispatched after the result is rendered (verified here by driving a
// second dispatch cycle between the stat checks).
func TestChildSpillExistsAtHandoffAndThroughTeardown(t *testing.T) {
	isolateToolCache(t)
	app, _ := archiveApp(t, "bench-parent")
	app.Client.ChatID = "bench-parent"

	rendered := renderSubagentResult(app, "tally docs/note-02.txt",
		SubagentSummary{Objective: "bench tally", Findings: []Finding{{Summary: "subtotal 23", Location: "docs/note-02", Kind: "fact", Weight: "high"}}}, nil)
	path := wtools.ExtractSpillPath(rendered)
	if path == "" {
		t.Fatalf("ExtractSpillPath on %q", rendered)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("child spill %q does not exist at hand-out: %v", path, err)
	}
	// Second dispatch cycle (a further render + a collapse write) in
	// between the two stats — the interval this test guards.
	_ = renderSubagentResult(app, "tally docs/note-03.txt",
		SubagentSummary{Objective: "bench tally", Findings: []Finding{{Summary: "subtotal 29", Location: "docs/note-03", Kind: "fact", Weight: "high"}}}, nil)
	_ = wtools.SpillToCache("bench-parent", "turn-1-part", "later collapse archive")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("child spill %q vanished across a second dispatch + collapse cycle: %v", path, err)
	}
}

// TestHostCacheMissingFileMessage pins the clearer host-missing error:
// a classified toolcache path whose file is absent must yield an
// actionable sentence, not the raw "statat <rel>: …" stdlib op name.
// The Errors.Is gate means only ENOENT is rewritten — escape and
// permission failures keep their precise text (covered by the existing
// ErrHostCacheEscape tests in internal/tools).
func TestHostCacheMissingFileMessage(t *testing.T) {
	isolateToolCache(t)
	app, _ := archiveApp(t, "bench-parent")
	app.Client.ChatID = "bench-parent"

	missing := filepath.Join(wtools.ToolCacheRoot(), "bench-parent", "dispatch_subagent-1.txt")
	if !wtools.IsToolCacheHostPath(missing) {
		t.Fatalf("test premise: %q must classify as a toolcache path", missing)
	}
	for _, tool := range []string{"read_file", "read_file_full"} {
		res := app.handleToolCall(backgroundCtx(), proxy.ToolCall{Function: proxy.FunctionCall{
			Name:      tool,
			Arguments: mustJSON(map[string]string{"path": missing}),
		}})
		if res.ok {
			t.Fatalf("%s on a missing toolcache file must fail", tool)
		}
		if strings.Contains(res.text, "statat") {
			t.Errorf("%s leaked the raw stdlib op name: %q", tool, Truncate(res.text, 200))
		}
		if !strings.Contains(res.text, "no file at this exact path in the tool cache") {
			t.Errorf("%s missing-file text = %q, want the actionable sentence", tool, Truncate(res.text, 250))
		}
		if !strings.Contains(res.text, missing) {
			t.Errorf("%s missing-file text must name the requested path: %q", tool, Truncate(res.text, 250))
		}
	}
}

// backgroundCtx returns a context for handleToolCall invocations inside
// these tests.
func backgroundCtx() context.Context { return context.Background() }
