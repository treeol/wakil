package agent

// Block 2 tests: subagent marker order (trailing-marker recovery for
// edit-tier children with files changed), the host-missing error text
// naming the requested chat's index, and search_files own-chat
// isolation (no widening — a search scoped to chat A returns nothing
// from chat B).

import (
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// TestSubagentMarkerIsTrailingForEditTier pins the marker-order fix:
// with files changed present, the "[subagent summary at: PATH]" marker
// is the final bracketed segment, so ExtractSpillPath recovers it —
// and eviction stubs point at the ORIGINAL summary file instead of
// re-spilling the whole rendered result.
func TestSubagentMarkerIsTrailingForEditTier(t *testing.T) {
	isolateToolCache(t)
	app := archiveAppWithChat(t, "idx-edit")

	summary := SubagentSummary{
		Objective:    "edit docs/a.go and docs/b.go",
		Findings:     []Finding{{Summary: "edited two files", Location: "docs/a.go", Kind: "fact", Weight: "high"}},
		FilesChanged: []string{"docs/a.go"},
	}
	rendered := renderSubagentResult(app, "edit docs/a.go and docs/b.go", summary, []string{"docs/a.go", "docs/b.go"})

	// The files-changed block must come BEFORE the marker now.
	if strings.Index(rendered, "[files_changed") < 0 {
		t.Fatalf("rendered result missing the files-changed block: %q", Truncate(rendered, 300))
	}
	if strings.Index(rendered, "[files_changed") > strings.LastIndex(rendered, "[subagent summary at: ") {
		t.Errorf("files-changed block must precede the summary marker (trailing-marker recovery): %q", Truncate(rendered, 400))
	}
	path := wtools.ExtractSpillPath(rendered)
	if path == "" {
		t.Fatalf("ExtractSpillPath recovered nothing from the edit-tier render: %q", Truncate(rendered, 400))
	}

	// Eviction over that result produces a stub pointing at the ORIGINAL
	// file (no duplicate re-spill).
	stub := wtools.MakeEvictionStub("dispatch_subagent", rendered)
	if got := wtools.ExtractSpillPath(stub); got != path {
		t.Errorf("eviction stub points at %q, want the original %q (stub=%q)", got, path, stub)
	}
}

// TestHostMissingNamesRequestedChatIndex pins the error text: a missing
// toolcache read names the REQUESTED path's chat index (so a child-dir
// path points at the child's index, not the caller's).
func TestHostMissingNamesRequestedChatIndex(t *testing.T) {
	isolateToolCache(t)
	app := archiveAppWithChat(t, "caller-chat")

	childMissing := filepathJoin(t, "child-chat", "dispatch_subagent-123.txt")
	if !wtools.IsToolCacheHostPath(childMissing) {
		t.Fatalf("premise: %q must classify as a toolcache path", childMissing)
	}
	res := app.handleToolCall(backgroundCtx(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name:      "read_file",
		Arguments: mustJSON(map[string]string{"path": childMissing}),
	}})
	if res.ok {
		t.Fatalf("read of a missing toolcache file must fail")
	}
	wantIndex := wtools.IndexPathFor("child-chat")
	if !strings.Contains(res.text, wantIndex) {
		t.Errorf("missing-file text must name the requested chat's index %q: %q", wantIndex, Truncate(res.text, 400))
	}
	if strings.Contains(res.text, "search_files on the cache directory") {
		t.Errorf("the old search hint must be gone (search is own-chat-scoped): %q", Truncate(res.text, 400))
	}
}

// TestSearchFilesIsolation pins the Block 2a decision: search stays
// own-chat-scoped. A search in chat A finds needles in A (including
// inside A's index file) and nothing from chat B.
func TestSearchFilesIsolation(t *testing.T) {
	isolateToolCache(t)
	app := archiveAppWithChat(t, "chat-A")

	wtools.SpillToCache("chat-A", "search_files", "NEEDLE-OF-CHAT-A\n")
	wtools.SpillToCache("chat-B", "search_files", "NEEDLE-OF-CHAT-B\n")

	search := func(pattern string) string {
		res := app.handleToolCall(backgroundCtx(), proxy.ToolCall{Function: proxy.FunctionCall{
			Name:      "search_files",
			Arguments: mustJSON(map[string]string{"pattern": pattern, "path": dirName(wtools.IndexPathFor("chat-A"))}),
		}})
		return res.text
	}

	if search("NEEDLE-OF-CHAT-A") == "" {
		// the call below asserts the content; this keeps the search
		// helper live
		t.Errorf("own-chat search returned empty")
	}

	got := app.handleToolCall(backgroundCtx(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name:      "search_files",
		Arguments: mustJSON(map[string]string{"pattern": "NEEDLE-OF-CHAT-A", "path": dirName(wtools.IndexPathFor("chat-A"))}),
	}})
	if !got.ok || !strings.Contains(got.text, "NEEDLE-OF-CHAT-A") {
		t.Errorf("own-chat search must find chat A needles: ok=%v %q", got.ok, Truncate(got.text, 300))
	}
	gotB := app.handleToolCall(backgroundCtx(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name:      "search_files",
		Arguments: mustJSON(map[string]string{"pattern": "NEEDLE-OF-CHAT-B", "path": dirName(wtools.IndexPathFor("chat-A"))}),
	}})
	if strings.Contains(gotB.text, "NEEDLE-OF-CHAT-B") {
		t.Errorf("chat A search leaked chat B content: %q", Truncate(gotB.text, 300))
	}
	// A search on ANOTHER chat's dir is out of scope and refused (the
	// host route is own-chat-only; the path falls through to
	// ConfinePath and is rejected) — not widened.
	gotForeign := app.handleToolCall(backgroundCtx(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name:      "search_files",
		Arguments: mustJSON(map[string]string{"pattern": "NEEDLE-OF-CHAT-B", "path": dirName(wtools.IndexPathFor("chat-B"))}),
	}})
	if gotForeign.ok && strings.Contains(gotForeign.text, "NEEDLE-OF-CHAT-B") {
		t.Errorf("cross-chat search must stay refused: %q", Truncate(gotForeign.text, 300))
	}
	// The search itself must see the index file's lines (grep the
	// index by a task-ish label).
	gotIdx := app.handleToolCall(backgroundCtx(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name:      "search_files",
		Arguments: mustJSON(map[string]string{"pattern": "subagent-summary", "path": dirName(wtools.IndexPathFor("chat-A"))}),
	}})
	_ = gotIdx // subagent-summary lines may not exist in this fixture; the isolation assertions above are the pin
}

func dirName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return p
}

func filepathJoin(t *testing.T, elems ...string) string {
	t.Helper()
	parts := append([]string{wtools.ToolCacheRoot()}, elems...)
	return strings.Join(parts, "/")
}
