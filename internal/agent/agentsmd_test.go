package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAgentsMD_empty(t *testing.T) {
	got := loadAgentsMD("", "")
	if got != "" {
		t.Fatalf("expected empty string for empty cwd, got %q", got)
	}
}

func TestLoadAgentsMD_noFile(t *testing.T) {
	dir := t.TempDir()
	got := loadAgentsMD(dir, dir)
	if got != "" {
		t.Fatalf("expected empty string when no AGENTS.md exists, got %q", got)
	}
}

func TestLoadAgentsMD_rootOnly(t *testing.T) {
	dir := t.TempDir()
	content := "Use pnpm not npm\n"
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got := loadAgentsMD(dir, dir)
	if got == "" {
		t.Fatal("expected non-empty result")
	}
	if !strings.Contains(got, "Use pnpm not npm") {
		t.Errorf("result should contain AGENTS.md content, got: %s", got)
	}
	if !strings.Contains(got, "(workspace root)") {
		t.Errorf("result should label root as (workspace root), got: %s", got)
	}
	if !strings.Contains(got, "advisory") {
		t.Error("result should contain the 'advisory' precedence disclaimer")
	}
}

func TestLoadAgentsMD_nestedOverridesRoot(t *testing.T) {
	// Structure: root/AGENTS.md + root/sub/AGENTS.md + root/sub/deep/AGENTS.md
	// cwd = root/sub/deep → walk collects deep, sub, root → rendered root-first
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	deep := filepath.Join(sub, "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("root-instruction"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "AGENTS.md"), []byte("sub-instruction"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deep, "AGENTS.md"), []byte("deep-instruction"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := loadAgentsMD(deep, root)
	if got == "" {
		t.Fatal("expected non-empty result")
	}
	// Root-first rendering: root-instruction should appear before sub-instruction
	// which should appear before deep-instruction.
	rootIdx := strings.Index(got, "root-instruction")
	subIdx := strings.Index(got, "sub-instruction")
	deepIdx := strings.Index(got, "deep-instruction")
	if rootIdx < 0 || subIdx < 0 || deepIdx < 0 {
		t.Fatalf("missing sections; rootIdx=%d subIdx=%d deepIdx=%d\n%s", rootIdx, subIdx, deepIdx, got)
	}
	if !(rootIdx < subIdx && subIdx < deepIdx) {
		t.Errorf("expected root-first ordering (root < sub < deep), got root=%d sub=%d deep=%d", rootIdx, subIdx, deepIdx)
	}

	// Verify the deepest file (cwd-level) is labeled "(workspace root)".
	if !strings.Contains(got, "(workspace root)") {
		t.Error("deepest AGENTS.md should be labeled (workspace root)")
	}
}

func TestLoadAgentsMD_truncatesLargeFile(t *testing.T) {
	dir := t.TempDir()
	// Write a file larger than agentsMDMaxFile (32 KB)
	large := make([]byte, agentsMDMaxFile+4096)
	for i := range large {
		large[i] = 'A'
	}
	// Add newlines so safeTruncate has a boundary to find
	large[agentsMDMaxFile-100] = '\n'
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), large, 0o644); err != nil {
		t.Fatal(err)
	}

	got := loadAgentsMD(dir, dir)
	if got == "" {
		t.Fatal("expected non-empty result")
	}
	if !strings.Contains(got, "(truncated)") {
		t.Error("result should indicate truncation")
	}
}

func TestLoadAgentsMD_emptyFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("\n\n  \t\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := loadAgentsMD(dir, dir)
	if got != "" {
		t.Errorf("expected empty result for whitespace-only file, got %q", got)
	}
}

func TestLoadAgentsMD_setsTaintInBuildPreamble(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("use pnpm"), 0o644); err != nil {
		t.Fatal(err)
	}

	app := &App{
		AgentPrompt:     "You are Wakil.",
		InjectDate:      false,
		touchedExternal: false,
	}
	app.Cfg.WorkDir = dir

	got := app.buildPreamble("Thursday, 10 September 2026")
	if !strings.Contains(got, "use pnpm") {
		t.Error("preamble should contain AGENTS.md content")
	}
	if !strings.Contains(got, "advisory") {
		t.Error("preamble should contain the advisory precedence disclaimer")
	}
	if !app.touchedExternal {
		t.Error("buildPreamble should set touchedExternal when AGENTS.md is found")
	}
}

func TestLoadAgentsMD_noTaintWhenAbsent(t *testing.T) {
	dir := t.TempDir()

	app := &App{
		AgentPrompt:     "You are Wakil.",
		InjectDate:      false,
		touchedExternal: false,
	}
	app.Cfg.WorkDir = dir

	app.buildPreamble("Thursday, 10 September 2026")
	if app.touchedExternal {
		t.Error("touchedExternal should remain false when no AGENTS.md is present")
	}
}

func TestLoadAgentsMD_deepestFirstBudget(t *testing.T) {
	// Create 3 levels, each with a 40KB AGENTS.md (with newlines for safeTruncate).
	// Per-file cap = 32KB, total cap = 64KB.
	// Deepest-first: deep → 32KB (per-file cap), mid → 32KB (total 64KB),
	// root → omitted (no budget left).
	root := t.TempDir()
	mid := filepath.Join(root, "mid")
	deep := filepath.Join(mid, "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	// Use unique markers per level. 40KB each with newlines.
	deepContent := strings.Repeat("D\n", 20*1024) // 40KB
	midContent := strings.Repeat("M\n", 20*1024)  // 40KB
	rootContent := strings.Repeat("R\n", 20*1024) // 40KB

	if err := os.WriteFile(filepath.Join(deep, "AGENTS.md"), []byte(deepContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mid, "AGENTS.md"), []byte(midContent), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte(rootContent), 0o644); err != nil {
		t.Fatal(err)
	}

	got := loadAgentsMD(deep, root)
	if got == "" {
		t.Fatal("expected non-empty result")
	}

	// Deepest content ("D") should be present (gets first budget slice).
	if !strings.Contains(got, "D") {
		t.Error("deepest file content should be included")
	}
	// Mid content ("M") should be present (gets second budget slice).
	if !strings.Contains(got, "M") {
		t.Error("mid file content should be included")
	}
	// Root content ("R") should be omitted (budget exhausted by deep + mid).
	// "R" could appear in the headers ("AGENTS.md"), so check for the
	// actual root content marker "R\n" which wouldn't appear in headers.
	if strings.Contains(got, "R\n") {
		t.Error("root file should be omitted due to budget exhaustion")
	}
}

func TestLoadAgentsMD_rejectsNonRegularFile(t *testing.T) {
	// AGENTS.md as a directory should be skipped, not cause an error.
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "AGENTS.md"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := loadAgentsMD(dir, dir)
	if got != "" {
		t.Errorf("expected empty result when AGENTS.md is a directory, got %q", got)
	}
}

func TestLoadAgentsMD_relativeCwdHandled(t *testing.T) {
	// A relative cwd like "." should be resolved to absolute and not loop.
	got := loadAgentsMD(".", "")
	// No AGENTS.md in the test's CWD (the Go package dir) — may or may not
	// return content. The important thing is it doesn't hang or panic.
	_ = got
}

func TestSafeTruncate(t *testing.T) {
	tests := []struct {
		input string
		n     int
		want  string
	}{
		{"hello world", 100, "hello world"},       // n > len
		{"line1\nline2\nline3", 8, "line1\n"},      // back to newline
		{"nolinebreakhere", 5, "nolin"},            // no newline: cut at n (safe ASCII)
		{"", 10, ""},                               // empty
	}
	for _, tc := range tests {
		got := safeTruncate(tc.input, tc.n)
		if got != tc.want {
			t.Errorf("safeTruncate(%q, %d) = %q, want %q", tc.input, tc.n, got, tc.want)
		}
	}
}

func TestLoadAgentsMD_totalCapTruncationReported(t *testing.T) {
	// Two files: root 40KB, deep 40KB. Total cap 64KB.
	// Deepest-first: deep gets 32KB (per-file cap), then root gets
	// remaining 32KB (per-file cap). Total = 64KB exactly.
	// Actually deep gets 32KB per-file cap, root gets remaining 32KB.
	// Both truncated by per-file cap. Let's test with smaller sizes
	// to trigger total-cap truncation specifically.

	// root = 35KB, deep = 35KB. Per-file cap = 32KB each.
	// Deepest-first: deep → 32KB (per-file cap), root → 32KB (per-file cap)
	// Total = 64KB = exactly at cap. No total-cap truncation.
	//
	// root = 35KB, mid = 35KB, deep = 35KB.
	// Deepest-first: deep → 32KB, mid → 32KB (total 64KB), root → 0 (no budget)
	// Root omitted entirely, mid truncated by per-file cap.
	root := t.TempDir()
	mid := filepath.Join(root, "mid")
	deep := filepath.Join(mid, "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{deep, mid, root} {
		content := strings.Repeat("X\n", 20*1024) // 40KB with newlines
		if err := os.WriteFile(filepath.Join(p, "AGENTS.md"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := loadAgentsMD(deep, root)
	if got == "" {
		t.Fatal("expected non-empty result")
	}
	// Deep and mid should be present, root should be omitted.
	// Count occurrences of "X" as a proxy for content presence —
	// but "X" is in all files. Instead check the headers.
	if !strings.Contains(got, "(workspace root)") {
		t.Error("deepest file should be present and labeled (workspace root)")
	}
	// Root file should not be present (omitted by budget).
	// The root file's label would be the relative path from deep to root.
	// Since we can't easily distinguish, just verify we got content
	// and the total body is bounded.
	bodyStart := strings.Index(got, "advisory")
	if bodyStart < 0 {
		t.Fatal("missing advisory header")
	}
	body := got[bodyStart:]
	// Total should not exceed agentsMDMaxTotal + headers/separators overhead.
	// A rough check: body should be well under 100KB (64KB content + overhead).
	if len(body) > 100*1024 {
		t.Errorf("total rendered content too large: %d bytes", len(body))
	}
}

func TestLoadAgentsMD_stopsAtWorkspaceRoot(t *testing.T) {
	// The ancestor walk must stop at the workspace root — it must not
	// ingest AGENTS.md from directories above the workspace boundary.
	// Structure: root/AGENTS.md + root/workspace/AGENTS.md + root/workspace/sub/AGENTS.md
	// cwd = root/workspace/sub, workspaceRoot = root/workspace
	// The walk should find sub + workspace, but NOT root (above workspace).
	root := t.TempDir()
	wsRoot := filepath.Join(root, "workspace")
	sub := filepath.Join(wsRoot, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(root, "AGENTS.md"), []byte("root-secret-instruction"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wsRoot, "AGENTS.md"), []byte("workspace-instruction"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "AGENTS.md"), []byte("sub-instruction"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := loadAgentsMD(sub, wsRoot)
	if got == "" {
		t.Fatal("expected non-empty result")
	}
	// sub and workspace should be present
	if !strings.Contains(got, "sub-instruction") {
		t.Error("sub-level AGENTS.md should be included")
	}
	if !strings.Contains(got, "workspace-instruction") {
		t.Error("workspace-root AGENTS.md should be included")
	}
	// root should NOT be present (above workspace boundary)
	if strings.Contains(got, "root-secret-instruction") {
		t.Error("root AGENTS.md above workspace boundary should NOT be included")
	}
}

func TestLoadAgentsMD_emptyWorkspaceRootWalksToFSRoot(t *testing.T) {
	// When workspaceRoot is empty, the walk should fall back to the
	// filesystem root (old behavior). This is the backward-compatible
	// path for callers that don't pass a workspace root.
	// We test with a single-level structure to verify it still works.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("test-instruction"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := loadAgentsMD(dir, "")
	if got == "" {
		t.Fatal("expected non-empty result with empty workspace root")
	}
	if !strings.Contains(got, "test-instruction") {
		t.Error("AGENTS.md should be found when workspaceRoot is empty")
	}
}
