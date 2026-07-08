package tools

import (
	"path/filepath"
	"testing"
)

// TestToolCacheRootUsesXDGDataHome verifies ToolCacheRoot resolves under
// XDG_DATA_HOME/wakil/toolcache, matching toolCacheDir's own precedence so
// the two never point at different directories.
func TestToolCacheRootUsesXDGDataHome(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("WAKIL_SESSIONS_DIR", "")

	root := ToolCacheRoot()
	want := filepath.Join(tmpDir, "wakil", "toolcache")
	if root != want {
		t.Errorf("ToolCacheRoot() = %q, want %q", root, want)
	}

	// toolCacheDir for a given chatID must be a subdirectory of the root.
	dir := toolCacheDir("chat1")
	if filepath.Dir(dir) != root {
		t.Errorf("toolCacheDir(%q) = %q, parent is not ToolCacheRoot() %q", "chat1", dir, root)
	}
}

// TestIsToolCacheHostPathExactPrefix verifies the exact-prefix design: a path
// equal to the root, a path properly nested under it, and paths that merely
// share a string prefix (without the path separator) are all classified
// correctly — no naive strings.HasPrefix(p, root) substring bug.
func TestIsToolCacheHostPathExactPrefix(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("WAKIL_SESSIONS_DIR", "")

	root := ToolCacheRoot()

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"exact root", root, true},
		{"nested file", filepath.Join(root, "chat1", "read_file_full-123.txt"), true},
		{"sibling with shared string prefix but no separator", root + "-decoy", false},
		{"unrelated absolute path", "/etc/passwd", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		got := IsToolCacheHostPath(c.path)
		if got != c.want {
			t.Errorf("%s: IsToolCacheHostPath(%q) = %v, want %v", c.name, c.path, got, c.want)
		}
	}
}

// TestReadHostCacheFileRoundTrip verifies ReadHostCacheFile reads back exactly
// what SpillToCache wrote, and StatHostCacheFile reports the same size.
func TestReadHostCacheFileRoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("WAKIL_SESSIONS_DIR", "")

	content := "hello from the host cache\nsecond line\n"
	path := SpillToCache("chat42", "read_file_full", content)
	if path == "" {
		t.Fatal("SpillToCache returned empty path")
	}

	got, err := ReadHostCacheFile(path)
	if err != nil {
		t.Fatalf("ReadHostCacheFile error: %v", err)
	}
	if got != content {
		t.Errorf("ReadHostCacheFile = %q, want %q", got, content)
	}

	size, err := StatHostCacheFile(path)
	if err != nil {
		t.Fatalf("StatHostCacheFile error: %v", err)
	}
	if size != int64(len(content)) {
		t.Errorf("StatHostCacheFile = %d, want %d", size, len(content))
	}
}

// TestReadHostCacheFileMissing verifies a nonexistent path under the toolcache
// root returns a real error rather than panicking or silently succeeding.
func TestReadHostCacheFileMissing(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("WAKIL_SESSIONS_DIR", "")

	missing := filepath.Join(ToolCacheRoot(), "nonexistent-chat", "nope.txt")
	if _, err := ReadHostCacheFile(missing); err == nil {
		t.Error("expected an error reading a nonexistent toolcache file")
	}
	if _, err := StatHostCacheFile(missing); err == nil {
		t.Error("expected an error statting a nonexistent toolcache file")
	}
}
