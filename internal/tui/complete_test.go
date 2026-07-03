package tui

import (
	"os"
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
	if err := os.WriteFile(filepath.Join(base, ".hidden"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
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
	st := computeCompletion(newTA("see @m"), compSources{mentionBase: base})
	if !st.active {
		t.Fatal("expected active picker")
	}
	if st.leafLen != 1 {
		t.Fatalf("leafLen = %d, want 1", st.leafLen)
	}
	// "m" matches main.go, mainframe.txt, models/ — dirs first, prefix-ranked.
	if len(st.cands) != 3 {
		t.Fatalf("cands = %+v", st.cands)
	}
	if !st.cands[0].isDir || st.cands[0].name != "models" {
		t.Fatalf("directory should rank first, got %+v", st.cands[0])
	}
}

func TestComputeCompletionHidesDotfiles(t *testing.T) {
	base := compTree(t)
	st := computeCompletion(newTA("@"), compSources{mentionBase: base})
	for _, c := range st.cands {
		if c.name == ".hidden" {
			t.Fatal("dotfiles should be hidden without a dot prefix")
		}
	}
}

func TestComputeCompletionNoTokenWhenNoAt(t *testing.T) {
	base := compTree(t)
	if st := computeCompletion(newTA("just text"), compSources{mentionBase: base}); st.active {
		t.Fatal("no '@' → inactive")
	}
	if st := computeCompletion(newTA("mid a@b word"), compSources{mentionBase: base}); st.active {
		t.Fatal("mid-word '@' → inactive")
	}
}

func TestAcceptCompletionInsertsFile(t *testing.T) {
	base := compTree(t)
	m := tuiModel{app: &agent.App{Cfg: config.Config{MentionBase: base}}, ta: newTA("see @other")}
	m.comp = computeCompletion(m.ta, compSources{mentionBase: base})
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
	m.comp = computeCompletion(m.ta, compSources{mentionBase: base})
	m = m.acceptCompletion()
	if got := m.ta.Value(); got != "@models/" {
		t.Fatalf("dir accept value = %q, want %q", got, "@models/")
	}
	if !m.comp.active {
		t.Fatal("accepting a directory should keep the picker open")
	}
}
