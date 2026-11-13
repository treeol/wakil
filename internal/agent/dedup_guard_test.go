package agent

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// TestDedupEntryCreatedWithNormalizedPath verifies that after a successful
// read_file call, the ToolCache entry is created with the correct normalized
// path for edit invalidation.
func TestDedupEntryCreatedWithNormalizedPath(t *testing.T) {
	exec := newFakeExecutor()
	exec.files["a.go"] = "content"
	app := &App{Exec: exec, ToolCache: map[string]*toolDedupEntry{}, Out: io.Discard}

	app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`},
	})

	// The entry must exist and have the normalized path.
	key := app.toolDedupKey("read_file", `{"path":"a.go"}`)
	entry, ok := app.ToolCache[key]
	if !ok {
		t.Fatal("expected ToolCache entry after successful call")
	}
	if entry == nil {
		t.Fatal("entry is nil")
	}
	if entry.path != "/work/a.go" {
		t.Errorf("entry.path = %q, want %q", entry.path, "/work/a.go")
	}
}

// TestDedupEntryNotCreatedOnFailedCall verifies that a failed tool call
// (file not found) does not create a cache entry — the call can be retried.
func TestDedupEntryNotCreatedOnFailedCall(t *testing.T) {
	exec := newFakeExecutor()
	app := &App{Exec: exec, ToolCache: map[string]*toolDedupEntry{}, Out: io.Discard}

	r1 := app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"missing.go"}`},
	})
	if r1.ok {
		t.Fatalf("first call should fail, got ok=true: %q", r1.text)
	}

	// No cache entry should exist for a failed call.
	key := app.toolDedupKey("read_file", `{"path":"missing.go"}`)
	if _, ok := app.ToolCache[key]; ok {
		t.Error("failed call should not create a cache entry")
	}

	// Second call should also execute (not deduped).
	r2 := app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"missing.go"}`},
	})
	if strings.Contains(r2.text, "already called") {
		t.Fatalf("failed call should be retryable, got deduped: %q", r2.text)
	}
}

// TestDedupHitRecordsTraceEntry verifies that a repeat read answered from
// the dedup cache records a trace entry, so the struggle detector sees the
// identical read run twice with no progress.
func TestDedupHitRecordsTraceEntry(t *testing.T) {
	exec := newFakeExecutor()
	exec.files["a.go"] = "content"
	app := &App{Exec: exec, ToolCache: map[string]*toolDedupEntry{}, Out: io.Discard}

	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`}}
	app.handleToolCall(context.Background(), tc)
	if len(app.recentTraces) != 1 {
		t.Fatalf("recentTraces after first read = %d, want 1", len(app.recentTraces))
	}
	app.handleToolCall(context.Background(), tc)
	if len(app.recentTraces) != 2 {
		t.Fatalf("recentTraces after dedup hit = %d, want 2 (repeat recorded)", len(app.recentTraces))
	}
	second := app.recentTraces[1]
	if second.Command != app.recentTraces[0].Command || second.Abbrev != app.recentTraces[0].Abbrev {
		t.Errorf("dedup trace entry = (%q,%q), want repeat of (%q,%q)",
			second.Abbrev, second.Command, app.recentTraces[0].Abbrev, app.recentTraces[0].Command)
	}
	if _, detected := DetectStruggle(app.recentTraces); !detected {
		t.Error("DetectStruggle should fire on a deduped repeat read")
	}
}

// TestDedupHitWithoutSpillPath verifies that when the original result was
// small (no spill), the dedup message falls back to the generic notice without
// a spill path reference.
func TestDedupHitWithoutSpillPath(t *testing.T) {
	exec := newFakeExecutor()
	exec.files["a.go"] = "content"
	app := &App{Exec: exec, ToolCache: map[string]*toolDedupEntry{}, Out: io.Discard}

	// First call executes successfully.
	app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`},
	})

	// Second equivalent call is deduped with a generic message (no spill path).
	r2 := app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"./a.go"}`},
	})
	if !strings.Contains(r2.text, "already called") {
		t.Fatalf("equivalent path should be deduped, got %q", r2.text)
	}
	if strings.Contains(r2.text, "spilled to") {
		t.Errorf("small result should not reference a spill path, got %q", r2.text)
	}
}

// TestDedupHitWithSpillPath verifies that when the original result was spilled
// to disk (via CapOrStub), the dedup message includes the spill path so the
// child can recover the content via read_file.
func TestDedupHitWithSpillPath(t *testing.T) {
	exec := newFakeExecutor()
	// Large content that exceeds ToolResultCap to trigger spilling.
	exec.files["big.go"] = strings.Repeat("package main\n", 2000) // ~22K, 2000 lines, avoids preview heuristic
	cfg := config.DefaultConfig()
	cfg.ToolResultCap = 1000
	app := &App{
		Exec:      exec,
		Cfg:       cfg,
		ToolCache: map[string]*toolDedupEntry{},
		Out:       io.Discard,
		Client:    newTestClient("http://unused"),
	}

	// First call: execute and finalize with CapOrStub to populate spill path.
	r1 := app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"big.go"}`},
	})
	if !r1.ok {
		t.Fatalf("first call should succeed, got %q", r1.text)
	}

	// Simulate finalizeToolResult's CapOrStub + spill path extraction.
	cappedText := app.CapOrStub(r1.text, "read_file", 0)
	key := app.toolDedupKey("read_file", `{"path":"big.go"}`)
	if entry, ok := app.ToolCache[key]; ok && entry != nil {
		if sp := wtools.ExtractSpillPath(cappedText); sp != "" {
			entry.spillPath = sp
		}
	}

	// Second equivalent call is deduped with spill path reference.
	r2 := app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"./big.go"}`},
	})
	if !strings.Contains(r2.text, "already called") {
		t.Fatalf("equivalent path should be deduped, got %q", r2.text)
	}
	if !strings.Contains(r2.text, "spilled to") {
		t.Errorf("large result should reference a spill path, got %q", r2.text)
	}
}

// TestDedupEditInvalidatesRead verifies that after a successful edit to a
// file, the ToolCache entry for reading that file is invalidated, allowing
// the child to re-read the updated content.
func TestDedupEditInvalidatesRead(t *testing.T) {
	exec := newFakeExecutor()
	exec.files["a.go"] = "original"
	cfg := config.DefaultConfig()
	app := &App{
		Exec:      exec,
		Cfg:       cfg,
		ToolCache: map[string]*toolDedupEntry{},
		Out:       io.Discard,
		Confirm:   func(_, _, _ string, _ bool) bool { return true },
	}

	// Read a.go — creates a cache entry.
	app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`},
	})
	key := app.toolDedupKey("read_file", `{"path":"a.go"}`)
	if _, ok := app.ToolCache[key]; !ok {
		t.Fatal("expected cache entry after read")
	}

	// Edit a.go — should invalidate the read cache entry.
	app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{
			Name:      "edit_file",
			Arguments: `{"path":"a.go","old_string":"original","new_string":"updated"}`,
		},
	})

	if _, ok := app.ToolCache[key]; ok {
		t.Error("read cache entry should be invalidated after edit to same path")
	}

	// Re-read should execute (not be deduped).
	r3 := app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`},
	})
	if strings.Contains(r3.text, "already called") {
		t.Fatalf("re-read after edit should execute, got deduped: %q", r3.text)
	}
}

// TestDedupEditDoesNotInvalidateOtherPaths is superseded by the whole-cache
// clear rule: a state-changing edit clears every read entry, including
// entries for other paths. The old per-path scoping was the source of the
// stale-read bug (see TestDedupWriteInvalidatesPreviousRead).
func TestDedupEditDoesNotInvalidateOtherPaths(t *testing.T) {
	exec := newFakeExecutor()
	exec.files["a.go"] = "original"
	exec.files["b.go"] = "content"
	cfg := config.DefaultConfig()
	app := &App{
		Exec:      exec,
		Cfg:       cfg,
		ToolCache: map[string]*toolDedupEntry{},
		Out:       io.Discard,
		Confirm:   func(_, _, _ string, _ bool) bool { return true },
	}

	// Read both files.
	app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`},
	})
	app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"b.go"}`},
	})
	keyB := app.toolDedupKey("read_file", `{"path":"b.go"}`)

	// Edit a.go — clears the whole cache, including the b.go entry. A
	// path-scoped clear would leave a b.go entry whose file state may have
	// been changed by a concurrent or side-effecting edit; the spec requires
	// a whole-cache clear.
	app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{
			Name:      "edit_file",
			Arguments: `{"path":"a.go","old_string":"original","new_string":"updated"}`,
		},
	})

	if _, ok := app.ToolCache[keyB]; ok {
		t.Error("b.go cache entry should be cleared by the whole-cache clear after editing a.go")
	}

	// Re-read b.go must execute, not be deduped.
	r3 := app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"b.go"}`},
	})
	if strings.Contains(r3.text, "already called") {
		t.Errorf("b.go should execute after the whole-cache clear following the edit, got %q", r3.text)
	}
}

// TestDedupEditInvalidatesAllToolTypesForPath verifies that editing a file
// invalidates ALL cached tool calls for that path — not just read_file.
// search_files, list_dir, etc. are equally stale after a mutation.
func TestDedupEditInvalidatesAllToolTypesForPath(t *testing.T) {
	exec := newFakeExecutor()
	exec.files["a.go"] = "original content\nwith pattern"
	cfg := config.DefaultConfig()
	app := &App{
		Exec:      exec,
		Cfg:       cfg,
		ToolCache: map[string]*toolDedupEntry{},
		Out:       io.Discard,
		Confirm:   func(_, _, _ string, _ bool) bool { return true },
	}

	// Search a.go — creates a cache entry.
	app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{
			Name:      "search_files",
			Arguments: `{"pattern":"pattern","path":"a.go"}`,
		},
	})
	searchKey := app.toolDedupKey("search_files", `{"pattern":"pattern","path":"a.go"}`)
	if _, ok := app.ToolCache[searchKey]; !ok {
		t.Fatal("expected search cache entry")
	}

	// Edit a.go — should invalidate the search entry too.
	app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{
			Name:      "edit_file",
			Arguments: `{"path":"a.go","old_string":"pattern","new_string":"changed"}`,
		},
	})

	if _, ok := app.ToolCache[searchKey]; ok {
		t.Error("search cache entry should be invalidated after edit to same path")
	}
}

// TestDedupEditInvalidatesAllPathVariants verifies that editing a file
// invalidates cache entries for all path variants (relative, absolute,
// with/without trailing slash) — not just one exact key.
func TestDedupEditInvalidatesAllPathVariants(t *testing.T) {
	exec := newFakeExecutor()
	// Store under the absolute path so the edit (which uses /work/a.go) can
	// read it. The fakeExecutor uses the raw path as the map key for ReadFile.
	exec.files["/work/a.go"] = "original"
	cfg := config.DefaultConfig()
	app := &App{
		Exec:      exec,
		Cfg:       cfg,
		ToolCache: map[string]*toolDedupEntry{},
		Out:       io.Discard,
		Confirm:   func(_, _, _ string, _ bool) bool { return true },
	}

	// Read using different path variants — each creates a distinct key
	// but all should map to the same normalized path. The fakeExecutor's
	// ConfinePath returns the path as-is, so ReadFile uses the raw argument.
	// Register files for each variant the read handler will see.
	for _, p := range []string{"a.go", "./a.go", "/work/a.go"} {
		if _, ok := exec.files[p]; !ok {
			exec.files[p] = "original"
		}
		app.handleToolCall(context.Background(), proxy.ToolCall{
			Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"` + p + `"}`},
		})
	}

	// Edit using absolute path — should invalidate ALL variants.
	app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{
			Name:      "edit_file",
			Arguments: `{"path":"/work/a.go","old_string":"original","new_string":"updated"}`,
		},
	})

	for _, p := range []string{"a.go", "./a.go", "/work/a.go"} {
		key := app.toolDedupKey("read_file", `{"path":"`+p+`"}`)
		if _, ok := app.ToolCache[key]; ok {
			t.Errorf("cache entry for path variant %q should be invalidated after edit", p)
		}
	}
}

// TestDedupEntrySpillPathNotOverwrittenByDedupHit verifies that a dedup hit
// (which returns an errResult and never goes through CapOrStub) does NOT
// overwrite the original entry's spill path with an empty one. The
// finalizeToolResult code only UPDATES existing entries' spill paths when a
// non-empty path is extracted — it never clears them.
func TestDedupEntrySpillPathNotOverwrittenByDedupHit(t *testing.T) {
	exec := newFakeExecutor()
	exec.files["a.go"] = "content"
	app := &App{Exec: exec, ToolCache: map[string]*toolDedupEntry{}, Out: io.Discard}

	// First call — creates entry with a mock spill path.
	app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`},
	})
	key := app.toolDedupKey("read_file", `{"path":"a.go"}`)
	app.ToolCache[key].spillPath = "/fake/spill/path"

	// Second call — dedup hit.
	r2 := app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`},
	})
	if !strings.Contains(r2.text, "already called") {
		t.Fatalf("second call should be deduped")
	}

	// Simulate what finalizeToolResult does: extract spill path from the
	// dedup message text. The dedup message has no spill marker, so this
	// returns "". Our finalizeToolResult code only sets entry.spillPath when
	// the extracted path is non-empty — so the original survives.
	sp := wtools.ExtractSpillPath(r2.text)
	if sp != "" {
		t.Fatalf("dedup message should have no spill path, got %q", sp)
	}

	// The entry's spill path should still be the original value.
	entry, ok := app.ToolCache[key]
	if !ok || entry == nil {
		t.Fatal("entry should still exist")
	}
	if entry.spillPath != "/fake/spill/path" {
		t.Errorf("spill path should not be overwritten, got %q", entry.spillPath)
	}
}

// TestInvalidateCachedPathNoopWhenCacheNil verifies that invalidation is a
// no-op when ToolCache is nil (parent agent, or subagent without dedup).
func TestInvalidateCachedPathNoopWhenCacheNil(t *testing.T) {
	app := &App{Exec: newFakeExecutor()}
	app.invalidateCachedPath("/work/a.go") // should not panic
}

// TestInvalidateCachedPathNoopWhenPathEmpty verifies that invalidation is a
// no-op when the canonical path is empty.
func TestInvalidateCachedPathNoopWhenPathEmpty(t *testing.T) {
	app := &App{
		Exec:      newFakeExecutor(),
		ToolCache: map[string]*toolDedupEntry{},
	}
	app.invalidateCachedPath("") // should not panic or modify cache
}

// TestInvalidateCachedPathAncestorMatch verifies that editing a file
// invalidates directory-scoped entries whose path is an ancestor of the
// edited file (e.g. search_files{path:"."} has entry.path="/work", editing
// "/work/a.go" should invalidate it).
func TestInvalidateCachedPathAncestorMatch(t *testing.T) {
	exec := newFakeExecutor()
	exec.files["a.go"] = "original"
	cfg := config.DefaultConfig()
	app := &App{
		Exec:      exec,
		Cfg:       cfg,
		ToolCache: map[string]*toolDedupEntry{},
		Out:       io.Discard,
		Confirm:   func(_, _, _ string, _ bool) bool { return true },
	}

	// Search using "." — entry.path will be "/work" (workspace root).
	app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{
			Name:      "search_files",
			Arguments: `{"pattern":"x","path":"."}`,
		},
	})
	searchKey := app.toolDedupKey("search_files", `{"pattern":"x","path":"."}`)
	if _, ok := app.ToolCache[searchKey]; !ok {
		t.Fatal("expected search cache entry")
	}

	// Edit a.go — should invalidate the search entry because /work is an
	// ancestor of /work/a.go.
	app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{
			Name:      "edit_file",
			Arguments: `{"path":"a.go","old_string":"original","new_string":"updated"}`,
		},
	})

	if _, ok := app.ToolCache[searchKey]; ok {
		t.Error("directory-scoped search entry should be invalidated when a child file is edited")
	}
}

// TestIsPathAncestor verifies the isPathAncestor helper.
func TestIsPathAncestor(t *testing.T) {
	tests := []struct {
		ancestor string
		path     string
		want     bool
	}{
		{"/work", "/work/a.go", true},
		{"/work", "/work/sub/b.go", true},
		{"/work", "/other/a.go", false},
		{"/work", "/work", false}, // same path, not ancestor
		{"", "/work/a.go", false},
		{"/work", "", false},
	}
	for _, tt := range tests {
		got := isPathAncestor(tt.ancestor, tt.path)
		if got != tt.want {
			t.Errorf("isPathAncestor(%q, %q) = %v, want %v", tt.ancestor, tt.path, got, tt.want)
		}
	}
}

// TestDedupEntryHasNoSpillPathWhenRawTools verifies that when RawTools is
// enabled (bypassing CapOrStub), the dedup entry does not get a spill path.
// The extraction logic is inside the !rawTools block, so RawTools entries
// never receive spill paths.
func TestDedupEntryHasNoSpillPathWhenRawTools(t *testing.T) {
	exec := newFakeExecutor()
	exec.files["a.go"] = "content"
	app := &App{Exec: exec, ToolCache: map[string]*toolDedupEntry{}, Out: io.Discard}
	app.RawTools = true

	app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"a.go"}`},
	})

	key := app.toolDedupKey("read_file", `{"path":"a.go"}`)
	entry, ok := app.ToolCache[key]
	if !ok || entry == nil {
		t.Fatal("expected cache entry")
	}
	// Even if we simulate what finalizeToolResult does, the !rawTools guard
	// prevents the extraction from running. The spill path stays empty.
	if entry.spillPath != "" {
		t.Errorf("RawTools entry should not have a spill path, got %q", entry.spillPath)
	}
}

// TestDedupHitWithSpillPathRecoverable verifies that the spill path in the
// dedup message is a real file that exists on disk. This is a best-effort
// recovery test — the spill file is created by CapOrStub and should be
// readable.
func TestDedupHitWithSpillPathRecoverable(t *testing.T) {
	exec := newFakeExecutor()
	// Large content that exceeds ToolResultCap to trigger spilling.
	exec.files["big.go"] = strings.Repeat("package main\n", 2000) // ~22K, 2000 lines, avoids preview heuristic
	cfg := config.DefaultConfig()
	cfg.ToolResultCap = 1000
	app := &App{
		Exec:      exec,
		Cfg:       cfg,
		ToolCache: map[string]*toolDedupEntry{},
		Out:       io.Discard,
		Client:    newTestClient("http://unused"),
	}

	// First call: execute and finalize with CapOrStub to populate spill path.
	r1 := app.handleToolCall(context.Background(), proxy.ToolCall{
		Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"big.go"}`},
	})
	if !r1.ok {
		t.Fatalf("first call should succeed, got %q", r1.text)
	}

	// Simulate finalizeToolResult's CapOrStub + spill path extraction.
	cappedText := app.CapOrStub(r1.text, "read_file", 0)
	key := app.toolDedupKey("read_file", `{"path":"big.go"}`)
	if entry, ok := app.ToolCache[key]; ok && entry != nil {
		if sp := wtools.ExtractSpillPath(cappedText); sp != "" {
			entry.spillPath = sp
		}
	}

	// Verify the spill path exists and has content.
	entry, ok := app.ToolCache[key]
	if !ok || entry == nil {
		t.Fatal("expected cache entry")
	}
	if entry.spillPath == "" {
		t.Fatal("expected non-empty spill path for large result")
	}
	// The spill file should exist on the host filesystem.
	if _, err := wtools.StatHostCacheFile(entry.spillPath); err != nil {
		t.Errorf("spill file should exist at %s: %v", entry.spillPath, err)
	}
	// The spill file should contain the original content.
	content, err := wtools.ReadHostCacheFile(entry.spillPath)
	if err != nil {
		t.Fatalf("failed to read spill file: %v", err)
	}
	// The spill file should contain the content (it's the full pre-cap result,
	// which includes line number formatting from the read_file handler).
	if len(content) < 20000 {
		t.Errorf("spill file content length = %d, want >= 20000", len(content))
	}
	if !strings.Contains(content, "package main") {
		t.Error("spill file content should contain the original data")
	}
}

// TestExtractPathArg verifies the extractPathArg helper extracts and
// normalizes the path argument correctly.
func TestExtractPathArg(t *testing.T) {
	app := &App{Exec: newFakeExecutor()} // WorkspaceRoot == "/work"

	tests := []struct {
		name     string
		argsJSON string
		want     string
	}{
		{"relative path", `{"path":"a.go"}`, "/work/a.go"},
		{"dot-relative path", `{"path":"./a.go"}`, "/work/a.go"},
		{"absolute path", `{"path":"/work/a.go"}`, "/work/a.go"},
		{"trailing slash", `{"path":"/work/"}`, "/work"},
		{"no path arg", `{"pattern":"x"}`, ""},
		{"empty path", `{"path":""}`, ""},
		{"invalid json", `not json`, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := app.extractPathArg("read_file", tt.argsJSON)
			if got != tt.want {
				t.Errorf("extractPathArg(%s) = %q, want %q", tt.name, got, tt.want)
			}
		})
	}
}
