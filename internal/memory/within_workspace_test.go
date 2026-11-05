package memory

import (
	"os"
	"path/filepath"
	"testing"
)

// A missing file under a symlinked parent must be judged by where the parent
// really points. Before the fix, isWithinWorkspace fell back to a lexical check
// for the non-existent target and accepted a path that escapes the root.
func TestIsWithinWorkspaceRejectsMissingFileUnderSymlinkedParent(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{root, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	escaping := filepath.Join(root, "link", "missing", "x.txt")
	if isWithinWorkspace(root, escaping) {
		t.Fatalf("missing file under symlink to outside the root was accepted: %s", escaping)
	}
}

// Containment of a missing file under a real subdirectory must still hold.
func TestIsWithinWorkspaceAcceptsMissingFileInsideRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(root, "sub", "new", "x.txt")
	if !isWithinWorkspace(root, inside) {
		t.Fatalf("missing file inside the root was rejected: %s", inside)
	}
}
