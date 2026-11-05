package exec

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// B1 regression: a workspace reached through a symlink must accept its own
// paths. Before the fix the unresolved root rejected every in-workspace path
// once the candidate resolved to the real directory.
func TestDirectExecutorSymlinkedWorkspaceRootAcceptsInside(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "Coding", "proj")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "coding")
	if err := os.Symlink(filepath.Join(base, "Coding"), alias); err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(alias, "proj")
	e, err := NewDirectExecutor(ws)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "a.go"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"a.go", filepath.Join(ws, "a.go"), filepath.Join(real, "a.go"), filepath.Join(ws, "new", "file.go")} {
		if _, err := e.ConfinePath(context.Background(), p); err != nil {
			t.Errorf("ConfinePath(%q) rejected an in-workspace path: %v", p, err)
		}
	}
	// Escapes are still rejected, through either spelling.
	for _, p := range []string{filepath.Join(ws, "..", "..", "etc", "passwd"), "/etc/passwd"} {
		if _, err := e.ConfinePath(context.Background(), p); err == nil {
			t.Errorf("ConfinePath(%q) accepted an escape", p)
		}
	}
}

// B1 escape guard with a SYMLINKED workspace root: a symlink inside the
// workspace that points outside must still be refused after the root is
// resolved. Resolving the root must not weaken containment.
func TestDirectExecutorSymlinkedRootStillRefusesOutwardLink(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "Coding", "proj")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(real, "escape")); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "coding")
	if err := os.Symlink(filepath.Join(base, "Coding"), alias); err != nil {
		t.Fatal(err)
	}
	e, err := NewDirectExecutor(filepath.Join(alias, "proj"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.ConfinePath(context.Background(), "escape"); err == nil {
		t.Error("symlink inside a symlinked workspace root pointing outside was accepted")
	}
	if _, err := e.ConfinePath(context.Background(), filepath.Join(outside, "x")); err == nil {
		t.Error("absolute path outside the workspace was accepted")
	}
}

// The "/" workspace root is a real containment case: every absolute path is
// inside it. isInsideWorkspace must not reject descendants of "/".
func TestIsInsideWorkspaceRootSlash(t *testing.T) {
	for _, c := range []struct {
		p, root string
		want    bool
	}{
		{"/", "/", true},
		{"/a", "/", true},
		{"/etc/passwd", "/", true},
		{"/a", "/b", false},
		{"/bx", "/b", false},
		{"/b/x", "/b", true},
	} {
		if got := isInsideWorkspace(c.p, c.root); got != c.want {
			t.Errorf("isInsideWorkspace(%q, %q) = %v, want %v", c.p, c.root, got, c.want)
		}
	}
}
