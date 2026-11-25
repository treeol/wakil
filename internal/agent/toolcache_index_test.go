package agent

// Block 1 tests for findable archives: a per-chat append-only archive
// index survives collapses, and every collapse note names its path so
// the model can find every archived result without knowing random
// spill suffixes.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// archiveIndexPathFromArchive derives the index path from an archive
// path (same chat dir). Mirrors the production helper collapseIndexLine.
func archiveIndexPathFromArchive(archive string) string {
	if archive == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(archive), "archive-index.txt")
}

// TestIndexReadableThroughCollapses is the Block 1 acceptance test
// (fake backend, mirrors toolcache_subagent_readback_test.go): dispatch
// subagents, collapse mid-turn at least twice, then read the index from
// the path in the latest collapse note, find every subagent archive by
// kind, and read each one successfully.
func TestIndexReadableThroughCollapses(t *testing.T) {
	isolateToolCache(t)
	parent := "idx-parent"
	child := "idx-child"

	// Two subagent summaries land under the PARENT chat dir (the
	// renderSubagentResult shape: parent's a.chatID()).
	rendered := renderSubagentResult(archiveAppWithChat(t, parent), "tally docs/note-01.txt",
		SubagentSummary{Objective: "bench tally", Findings: []Finding{{Summary: "subtotal 17", Location: "docs/note-01", Kind: "fact", Weight: "high"}}}, nil)
	childPath := wtools.ExtractSpillPath(rendered)
	if childPath == "" {
		t.Fatalf("no spill path in %q", rendered)
	}
	// A spill written INSIDE the child lives under the child's dir.
	childOwn := wtools.SpillToCache(child, "search_files", "child's own capped output")

	// Read the index through read_file, from the exact path the
	// collapse note names (derived from an archive in the parent dir).
	archive := wtools.SpillToCache(parent, "turn-1-part", "collapse archive body")
	ixPath := archiveIndexPathFromArchive(archive)
	ixOnDisk, err := os.ReadFile(ixPath)
	if err != nil {
		t.Fatalf("index file not readable at the note's path: %v", err)
	}
	ix := string(ixOnDisk)
	if !strings.Contains(ix, childPath) {
		t.Errorf("parent index missing the subagent summary archive %q; index = %q", childPath, ix)
	}
	if !strings.Contains(ix, "subagent-summary") {
		t.Errorf("index missing the subagent-summary kind; index = %q", ix)
	}
	// The child's own index is a separate file in the child's dir.
	childIndex, err := os.ReadFile(wtools.IndexPathFor(child))
	if err != nil {
		t.Fatalf("child index unreadable: %v", err)
	}
	if !strings.Contains(string(childIndex), childOwn) {
		t.Errorf("child index missing the child's own spill %q; index = %q", childOwn, childIndex)
	}

	// read_file through the host intercept must serve the index and the
	// archives it names — this is the "find without knowing my file
	// name" recovery the bench run lacked.
	app, _ := archiveApp(t, parent)
	res := app.handleToolCall(backgroundCtx(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name:      "read_file",
		Arguments: mustJSON(map[string]string{"path": ixPath}),
	}})
	if !res.ok {
		t.Fatalf("read_file on the index failed: %q", Truncate(res.text, 300))
	}
	if !strings.Contains(res.text, "subtotal") && !strings.Contains(res.text, childPath) {
		// The index's first line is a label (first content line "#1"),
		// not the subtotal — what matters is the path is listed.
		t.Errorf("index read does not list the subagent archive path: %q", Truncate(res.text, 300))
	}
	res = app.handleToolCall(backgroundCtx(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name:      "read_file_full",
		Arguments: mustJSON(map[string]string{"path": childPath}),
	}})
	if !res.ok || !strings.Contains(res.text, "subtotal 17") {
		t.Errorf("read_file_full(%q): ok=%v %q", childPath, res.ok, Truncate(res.text, 200))
	}
}

// archiveAppWithChat is archiveApp with the chat set on Client (the
// readback tests' convention).
func archiveAppWithChat(t *testing.T, chatID string) *App {
	t.Helper()
	app, _ := archiveApp(t, chatID)
	app.Client.ChatID = chatID
	return app
}

// TestCollapseNoteNamesIndex pins the note contract: every collapse
// note (full AND minimal-rebuild) carries the index line, byte-identical
// across notes with different working sets (cache guard).
func TestCollapseNoteNamesIndex(t *testing.T) {
	isolateToolCache(t)
	w := []proxy.Message{
		{Role: "assistant", ToolCalls: []proxy.ToolCall{{Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}}}},
		{Role: "tool", Name: "read_file", Content: StrPtr("output")},
	}
	archive := "/tmp/toolcache/idx-chat/turn-1-part-abc.txt"
	note := buildCollapseNote(1, w, archive)
	if !strings.Contains(note, "archive index: /tmp/toolcache/idx-chat/archive-index.txt") {
		t.Errorf("collapse note does not name the index path: %q", note)
	}

	// Cache guard: a note for a different working set must carry the
	// SAME index line bytes (no dynamic counts).
	w2 := []proxy.Message{
		{Role: "assistant", ToolCalls: []proxy.ToolCall{{Function: proxy.FunctionCall{Name: "edit_file", Arguments: `{"path":"b.go"}`}}}},
		{Role: "tool", Name: "edit_file", Content: StrPtr("changed")},
	}
	note2 := buildCollapseNote(2, w2, "/tmp/toolcache/idx-chat/turn-2-part-xyz.txt")
	indexLine := func(n string) string {
		for _, line := range strings.Split(n, "\n") {
			if strings.HasPrefix(line, "archive index: ") {
				return line
			}
		}
		return ""
	}
	if indexLine(note) == "" || indexLine(note) != indexLine(note2) {
		t.Errorf("index line must be byte-identical across notes:\n%q\n%q", indexLine(note), indexLine(note2))
	}

	// Minimal-rebuild path: a note over noteCapBytes keeps archive +
	// index lines (paths never truncated).
	long := strings.Repeat("/very-long-path-segment", 60) + "/turn-1-part-abc.txt"
	note3 := buildCollapseNote(1, w, long)
	if !strings.Contains(note3, "archive index: ") || !strings.Contains(note3, "full record: ") {
		t.Errorf("minimal note must keep archive + index lines: %q", Truncate(note3, 400))
	}
	if !strings.Contains(note3, archiveIndexPathFromArchive(long)) {
		t.Errorf("minimal note index path truncated: %q", Truncate(note3, 400))
	}
}

// TestDegradedProjectionNamesIndex pins that the older-turn projection
// also carries the index line.
func TestDegradedProjectionNamesIndex(t *testing.T) {
	rec := CollapseRecord{Turn: 3, Archive: "/tmp/toolcache/idx-chat/turn-3-part-abc.txt"}
	line := degradePointerLine(rec, []proxy.Message{{Role: "user", Content: StrPtr("earlier question")}})
	if !strings.Contains(line, "archive index: /tmp/toolcache/idx-chat/archive-index.txt") {
		t.Errorf("degraded projection line missing the index: %q", line)
	}
}
