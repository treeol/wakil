package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	agent "github.com/treeol/wakil/internal/agent"

	"github.com/treeol/wakil/internal/config"

	"github.com/charmbracelet/bubbles/textarea"
)

func compTree(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	for _, f := range []string{"main.go", "mainframe.txt", "other.go"} {
		if err := os.WriteFile(filepath.Join(base, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(base, "models"), 0o755); err != nil {
		t.Fatal(err)
	}
	// models/ needs a non-dotfile for git-based indexing to synthesize the
	// parent dir entry (git does not track empty directories). This matches
	// real-world usage where directories contain at least one file.
	if err := os.WriteFile(filepath.Join(base, "models", "data.json"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, ".hidden"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Reset the global file index cache so each test builds a fresh index
	// for its own temp dir, avoiding cross-test cache contamination.
	globalFileIndex = repoFileIndex{}
	return base
}

func newTA(value string) textarea.Model {
	ta := textarea.New()
	ta.SetWidth(80)
	ta.Focus() // textarea ignores key events (e.g. backspace on accept) unless focused
	ta.SetValue(value)
	ta.CursorEnd()
	return ta
}

func TestComputeCompletionActiveToken(t *testing.T) {
	base := compTree(t)
	st := computeCompletion(newTA("see @m"), compSources{mentionBase: base}, nil)
	if !st.active {
		t.Fatal("expected active picker")
	}
	if st.leafLen != 1 {
		t.Fatalf("leafLen = %d, want 1", st.leafLen)
	}
	// "m" matches models/ (dir), main.go, mainframe.txt, and models/data.json
	// (relPath contains "m" in "models"). Dirs first, prefix-ranked.
	if len(st.cands) != 4 {
		t.Fatalf("cands = %+v", st.cands)
	}
	if !st.cands[0].isDir || st.cands[0].name != "models" {
		t.Fatalf("directory should rank first, got %+v", st.cands[0])
	}
}

func TestComputeCompletionHidesDotfiles(t *testing.T) {
	base := compTree(t)
	st := computeCompletion(newTA("@"), compSources{mentionBase: base}, nil)
	for _, c := range st.cands {
		if c.name == ".hidden" {
			t.Fatal("dotfiles should be hidden without a dot prefix")
		}
	}
}

func TestComputeCompletionNoTokenWhenNoAt(t *testing.T) {
	base := compTree(t)
	if st := computeCompletion(newTA("just text"), compSources{mentionBase: base}, nil); st.active {
		t.Fatal("no '@' → inactive")
	}
	if st := computeCompletion(newTA("mid a@b word"), compSources{mentionBase: base}, nil); st.active {
		t.Fatal("mid-word '@' → inactive")
	}
}

func TestAcceptCompletionInsertsFile(t *testing.T) {
	base := compTree(t)
	m := tuiModel{app: &agent.App{Cfg: config.Config{MentionBase: base}}, ta: newTA("see @other")}
	m.comp = computeCompletion(m.ta, compSources{mentionBase: base}, nil)
	if len(m.comp.cands) != 1 {
		t.Fatalf("expected exactly other.go, got %+v", m.comp.cands)
	}
	m = m.acceptCompletion()
	if got := m.ta.Value(); got != "see @other.go" {
		t.Fatalf("after accept value = %q, want %q", got, "see @other.go")
	}
	if m.comp.active {
		t.Fatal("accepting a file should close the picker")
	}
}

func TestAcceptCompletionDirDrillsIn(t *testing.T) {
	base := compTree(t)
	m := tuiModel{app: &agent.App{Cfg: config.Config{MentionBase: base}}, ta: newTA("@models")}
	m.comp = computeCompletion(m.ta, compSources{mentionBase: base}, nil)
	m = m.acceptCompletion()
	if got := m.ta.Value(); got != "@models/" {
		t.Fatalf("dir accept value = %q, want %q", got, "@models/")
	}
	if !m.comp.active {
		t.Fatal("accepting a directory should keep the picker open")
	}
}

// TestComputeCompletionRecursiveSubdirMatch verifies that typing a bare leaf
// (no "/" prefix) at the repo root surfaces matches from subdirectories, not
// just the top-level directory listing. This is the core acceptance criterion
// for the recursive fuzzy @file feature.
func TestComputeCompletionRecursiveSubdirMatch(t *testing.T) {
	base := t.TempDir()
	// Create a subdirectory with files that match a leaf query.
	subDir := filepath.Join(base, "internal", "memory")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"store.go", "store_skills.go"} {
		if err := os.WriteFile(filepath.Join(subDir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Create a top-level file that also matches, to verify both are returned.
	if err := os.WriteFile(filepath.Join(base, "store.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create a non-matching file.
	if err := os.WriteFile(filepath.Join(base, "other.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Reset the global file index cache so the new temp dir is indexed fresh.
	globalFileIndex = repoFileIndex{}

	st := computeAtCompletion(newTA("@store"), base)
	if !st.active {
		t.Fatal("expected active picker")
	}
	// Should find all three store* files (top-level + 2 in subdir).
	if len(st.cands) != 3 {
		t.Fatalf("expected 3 candidates matching 'store', got %d: %+v", len(st.cands), st.cands)
	}
	// Top-level store.go (shorter path) should rank before subdir files.
	if st.cands[0].name != "store.go" {
		t.Errorf("expected store.go first (shorter path), got %q", st.cands[0].name)
	}
	// Subdir files should appear with their workspace-relative path.
	foundSubdir := false
	for _, c := range st.cands {
		if c.name == "internal/memory/store.go" {
			foundSubdir = true
			break
		}
	}
	if !foundSubdir {
		t.Errorf("expected to find internal/memory/store.go in candidates, got: %+v", st.cands)
	}
}

// TestComputeCompletionRecursiveVsDrillIn verifies that typing a "/" prefix
// still uses the single-level drill-in behavior (not recursive), preserving
// the existing navigation UX.
func TestComputeCompletionRecursiveVsDrillIn(t *testing.T) {
	base := t.TempDir()
	subDir := filepath.Join(base, "src")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "app.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A file in a different subdirectory that would match in recursive mode.
	otherDir := filepath.Join(base, "lib")
	if err := os.MkdirAll(otherDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(otherDir, "app.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	globalFileIndex = repoFileIndex{}

	// With a "/" prefix, only the specified subdirectory is listed.
	st := computeAtCompletion(newTA("@src/app"), base)
	if !st.active {
		t.Fatal("expected active picker")
	}
	// Should only find src/app.go, not lib/app.go (drill-in, not recursive).
	if len(st.cands) != 1 {
		t.Fatalf("drill-in should find only src/app.go, got %d: %+v", len(st.cands), st.cands)
	}
	if st.cands[0].name != "app.go" {
		t.Errorf("expected app.go, got %q", st.cands[0].name)
	}
}

// TestComputeCompletionRecursiveGitIndex verifies that the git ls-files path
// includes untracked files (not just tracked ones). This is the primary real-
// world code path — the fallback WalkDir path is only exercised in test temp
// dirs without a .git directory.
func TestComputeCompletionRecursiveGitIndex(t *testing.T) {
	// Skip if git is not available.
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	base := t.TempDir()
	// Initialize a git repo so buildFileIndexGit is used.
	if err := exec.Command("git", "init", base).Run(); err != nil {
		t.Skipf("git init failed: %v", err)
	}
	// Create a tracked file in a subdir.
	subDir := filepath.Join(base, "internal", "memory")
	if err := os.MkdirAll(subDir, 0o755); err != nil {
		t.Fatal(err)
	}
	trackedFile := filepath.Join(subDir, "store.go")
	if err := os.WriteFile(trackedFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec.Command("git", "-C", base, "add", "internal/memory/store.go").Run()
	exec.Command("git", "-C", base, "commit", "-m", "test").Run()

	// Create an untracked file in the same subdir.
	untrackedFile := filepath.Join(subDir, "store_new.go")
	if err := os.WriteFile(untrackedFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	globalFileIndex = repoFileIndex{}

	st := computeAtCompletion(newTA("@store"), base)
	if !st.active {
		t.Fatal("expected active picker")
	}
	// Both tracked and untracked files should appear.
	names := make(map[string]bool)
	for _, c := range st.cands {
		names[c.name] = true
	}
	if !names["internal/memory/store.go"] {
		t.Errorf("tracked file internal/memory/store.go not found, got: %+v", st.cands)
	}
	if !names["internal/memory/store_new.go"] {
		t.Errorf("untracked file internal/memory/store_new.go not found, got: %+v", st.cands)
	}
}
