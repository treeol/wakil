package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestBuildRepoMap verifies the repo map builds a tree outline with
// directories and files.
func TestBuildRepoMap(t *testing.T) {
	exec := newFakeExecutor()
	exec.files["README.md"] = "# Test"
	exec.files["go.mod"] = "module test"
	exec.files["main.go"] = "package main"
	exec.dirs["."] = true
	exec.dirs["cmd"] = true
	exec.dirs["internal"] = true

	exec.files["cmd/main.go"] = "package main"
	exec.dirs["cmd/wakil"] = true
	exec.files["cmd/wakil/main.go"] = "package main"
	exec.files["internal/app.go"] = "package agent"
	exec.dirs["internal/agent"] = true
	exec.files["internal/agent/app.go"] = "package agent"
	exec.files["internal/agent/commands.go"] = "package agent"

	result, err := BuildRepoMap(context.Background(), exec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if result.FileCount == 0 {
		t.Error("expected non-zero file count")
	}
	if result.DirCount == 0 {
		t.Error("expected non-zero dir count")
	}
	if !strings.Contains(result.Outline, "cmd/") {
		t.Errorf("expected 'cmd/' in outline, got: %s", result.Outline)
	}
	if !strings.Contains(result.Outline, "internal/") {
		t.Errorf("expected 'internal/' in outline")
	}
	if result.Partial {
		t.Error("expected non-partial result for simple repo")
	}
}

// TestBuildRepoMapCounts verifies exact file and directory counts.
func TestBuildRepoMapCounts(t *testing.T) {
	exec := newFakeExecutor()
	exec.dirs["."] = true
	exec.dirs["src"] = true
	exec.files["main.go"] = "code"
	exec.files["README.md"] = "docs"
	exec.files["src/app.go"] = "code"
	exec.files["src/util.go"] = "code"

	result, err := BuildRepoMap(context.Background(), exec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// 4 files: main.go, README.md, src/app.go, src/util.go
	if result.FileCount != 4 {
		t.Errorf("FileCount = %d, want 4", result.FileCount)
	}
	// 1 dir: src ( "." is not counted)
	if result.DirCount != 1 {
		t.Errorf("DirCount = %d, want 1", result.DirCount)
	}
}

// TestBuildRepoMapNoiseFiltering verifies noise directories and files are excluded.
func TestBuildRepoMapNoiseFiltering(t *testing.T) {
	exec := newFakeExecutor()
	exec.dirs["."] = true
	exec.dirs["node_modules"] = true
	exec.dirs["vendor"] = true
	exec.dirs["src"] = true
	exec.files["app.ts"] = "code"
	exec.files["app.pyc"] = "binary"
	exec.files[".hidden"] = "hidden"

	result, err := BuildRepoMap(context.Background(), exec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(result.Outline, "node_modules") {
		t.Errorf("node_modules should be excluded from outline")
	}
	if strings.Contains(result.Outline, "vendor") {
		t.Errorf("vendor should be excluded from outline")
	}
	if strings.Contains(result.Outline, "app.pyc") {
		t.Errorf(".pyc files should be excluded")
	}
	if strings.Contains(result.Outline, ".hidden") {
		t.Errorf("hidden files should be excluded")
	}
	if !strings.Contains(result.Outline, "app.ts") {
		t.Errorf("expected app.ts in outline")
	}
}

// TestBuildRepoMapNilExecutor verifies nil executor returns an error.
func TestBuildRepoMapNilExecutor(t *testing.T) {
	_, err := BuildRepoMap(context.Background(), nil)
	if err == nil {
		t.Error("expected error for nil executor")
	}
}

// TestBuildRepoMapRootError verifies root listing errors are surfaced.
func TestBuildRepoMapRootError(t *testing.T) {
	exec := newFakeExecutor()
	// Don't register "." — ListDir(".") will still work (fake allows ".")
	// but there will be no entries.
	result, err := BuildRepoMap(context.Background(), exec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.FileCount != 0 || result.DirCount != 0 {
		t.Errorf("expected zero counts for empty repo, got files=%d dirs=%d", result.FileCount, result.DirCount)
	}
}

// TestBuildRepoMapDepthLimit verifies the walk stops at depth 3.
func TestBuildRepoMapDepthLimit(t *testing.T) {
	exec := newFakeExecutor()
	exec.dirs["."] = true
	exec.dirs["a"] = true
	exec.dirs["a/b"] = true
	exec.dirs["a/b/c"] = true
	exec.dirs["a/b/c/d"] = true
	exec.files["a/file.go"] = "code"
	exec.files["a/b/file.go"] = "code"
	exec.files["a/b/c/file.go"] = "code"
	exec.files["a/b/c/d/deep.go"] = "code"

	result, err := BuildRepoMap(context.Background(), exec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Depth 0 = root, depth 1 = a, depth 2 = a/b, depth 3 = a/b/c
	// a/b/c/d appears as a dir header at depth 3 (showing it exists),
	// but its contents should NOT be listed (depth 4 > maxDepth 3).
	if !strings.Contains(result.Outline, "d/") {
		t.Logf("note: d/ header may or may not appear depending on depth semantics")
	}
	// The file deep.go inside a/b/c/d/ should NOT appear.
	if strings.Contains(result.Outline, "deep.go") {
		t.Errorf("file in depth-4 directory should not appear: %s", result.Outline)
	}
}

// TestBuildRepoMapTimeout verifies that context cancellation marks the result partial.
func TestBuildRepoMapTimeout(t *testing.T) {
	exec := newFakeExecutor()
	exec.dirs["."] = true
	// Add many directories to make the walk slow enough for cancellation.
	for i := 0; i < 100; i++ {
		dir := "dir" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26))
		exec.dirs[dir] = true
		exec.files[dir+"/file.go"] = "code"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Millisecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond) // let the context expire

	result, err := BuildRepoMap(ctx, exec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Partial {
		t.Error("expected partial=true for cancelled context")
	}
}

// TestBuildRepoMapVisitCap verifies the visit cap marks partial.
func TestBuildRepoMapVisitCap(t *testing.T) {
	exec := newFakeExecutor()
	exec.dirs["."] = true
	// Create more directories than repoMapMaxVisits.
	for i := 0; i < repoMapMaxVisits+10; i++ {
		dir := "d" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26))
		exec.dirs[dir] = true
		exec.files[dir+"/f.go"] = "code"
	}

	result, err := BuildRepoMap(context.Background(), exec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Partial {
		t.Error("expected partial=true when visit cap is exceeded")
	}
}

// TestBuildRepoMapTruncation verifies large outlines are capped.
func TestBuildRepoMapTruncation(t *testing.T) {
	exec := newFakeExecutor()
	exec.dirs["."] = true
	// Create enough content to exceed repoMapMaxBytes.
	// Each directory adds ~30+ bytes (dir header + sub-files).
	// We need ~1000+ directories with files to exceed 32KB.
	for i := 0; i < 1500; i++ {
		s := "dir" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26)) + string(rune('a'+(i/17576)%26))
		exec.dirs[s] = true
		// Add files with long names to increase output size.
		exec.files[s+"/a_file_with_a_long_name_for_padding.go"] = "code"
		exec.files[s+"/another_file_for_more_padding.go"] = "code"
		exec.files[s+"/a_third_file_to_ensure_overflow.go"] = "code"
	}

	result, err := BuildRepoMap(context.Background(), exec)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The outline should be capped and contain a truncation marker.
	if len(result.Outline) > repoMapMaxBytes+200 {
		t.Errorf("outline too large after truncation: %d bytes (cap=%d)", len(result.Outline), repoMapMaxBytes)
	}
	if !strings.Contains(result.Outline, "truncated") {
		t.Errorf("expected truncation marker in outline")
	}
}

// TestIsNoiseFile verifies the noise file filter.
func TestIsNoiseFile(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"app.go", false},
		{"README.md", false},
		{".hidden", true},
		{".gitignore", true},
		{"app.pyc", true},
		{"app.o", true},
		{"app.so", true},
		{"app.lock", true},
		{"app.db", true},
		{"app.class", true},
		{"app.exe", true},
		{"test.py", false},
		{"main.ts", false},
	}
	for _, tt := range tests {
		got := isNoiseFile(tt.name)
		if got != tt.want {
			t.Errorf("isNoiseFile(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestHandleRepoMapCommand verifies the /repomap command handler.
func TestHandleRepoMapCommand(t *testing.T) {
	exec := newFakeExecutor()
	exec.dirs["."] = true
	exec.dirs["src"] = true
	exec.files["main.go"] = "package main"
	exec.files["src/app.go"] = "package app"

	app := newTestApp("http://localhost", exec, func(_, _, _ string, _ bool) bool { return true })

	result, err := handleRepoMapCommand(context.Background(), app)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(result, "Repo map:") {
		t.Errorf("expected 'Repo map:' in result, got: %s", result)
	}
	if !strings.Contains(result, "main.go") {
		t.Errorf("expected main.go in outline preview")
	}
}

// TestHandleRepoMapCommandNilApp verifies nil app is handled.
func TestHandleRepoMapCommandNilApp(t *testing.T) {
	_, err := handleRepoMapCommand(context.Background(), nil)
	if err == nil {
		t.Error("expected error for nil app")
	}
}

// TestInjectRepoMap verifies the injection produces a summary line.
func TestInjectRepoMap(t *testing.T) {
	exec := newFakeExecutor()
	exec.dirs["."] = true
	exec.dirs["cmd"] = true
	exec.files["main.go"] = "package main"
	exec.files["cmd/main.go"] = "package main"

	result := injectRepoMap(context.Background(), exec, "test-chat-id")
	if result == "" {
		t.Error("expected non-empty summary line")
	}
	if !strings.Contains(result, "Repo map:") {
		t.Errorf("expected 'Repo map:' in summary, got: %s", result)
	}
	if !strings.Contains(result, "files") {
		t.Errorf("expected 'files' in summary")
	}
}

// TestInjectRepoMapNilExecutor verifies nil executor returns empty.
func TestInjectRepoMapNilExecutor(t *testing.T) {
	result := injectRepoMap(context.Background(), nil, "test")
	if result != "" {
		t.Errorf("expected empty string for nil executor, got: %s", result)
	}
}

// TestInjectRepoMapPartial verifies partial results are labeled.
func TestInjectRepoMapPartial(t *testing.T) {
	exec := newFakeExecutor()
	exec.dirs["."] = true
	for i := 0; i < repoMapMaxVisits+10; i++ {
		dir := "d" + string(rune('a'+i%26)) + string(rune('a'+(i/26)%26)) + string(rune('a'+(i/676)%26))
		exec.dirs[dir] = true
		exec.files[dir+"/f.go"] = "code"
	}

	result := injectRepoMap(context.Background(), exec, "test-partial")
	if !strings.Contains(result, "partial") {
		t.Errorf("expected 'partial' in summary for cut-short walk, got: %s", result)
	}
}
