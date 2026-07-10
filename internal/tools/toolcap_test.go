package tools

import (
	"fmt"
	"path/filepath"
	"strings"
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

// TestCapToolResultTruncationMarker verifies the truncation-feedback marker:
// a result over the cap ends with an explicit TRUNCATED marker, the total
// output never exceeds the cap, and read_file's marker carries the
// offset/limit hint (silent truncation is the re-read-loop failure this fixes).
func TestCapToolResultTruncationMarker(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("WAKIL_SESSIONS_DIR", "")

	const cap = 500
	big := ""
	for i := 0; len(big) < 3*cap; i++ {
		big += "line of tool output content\n"
	}
	total := len(big)

	out := CapToolResult(big, "read_file", "chat-cap-marker", cap)

	if len(out) > cap {
		t.Errorf("capped output is %d chars, exceeds cap %d — marker must fit within the cap", len(out), cap)
	}
	if !strings.HasSuffix(strings.TrimRight(out, " \t\r\n"), "]") {
		t.Errorf("output does not end with the bracketed marker: %q", out[max(0, len(out)-120):])
	}
	if !strings.Contains(out, "TRUNCATED") {
		t.Errorf("marker missing TRUNCATED signal: %q", out[max(0, len(out)-200):])
	}
	if !strings.Contains(out, "offset/limit") {
		t.Errorf("read_file marker missing offset/limit hint: %q", out[max(0, len(out)-200):])
	}
	wantTotal := fmt.Sprintf("of %d chars", total)
	if !strings.Contains(out, wantTotal) {
		t.Errorf("marker missing actual total size %q: %q", wantTotal, out[max(0, len(out)-200):])
	}
	// The spill path must still be recoverable from the marker.
	if p := ExtractSpillPath(out); p == "" {
		t.Errorf("ExtractSpillPath failed on the new marker format: %q", out[max(0, len(out)-200):])
	}
}

// TestCapToolResultMarkerNonRangedTool verifies that tools without offset/limit
// access get "result truncated" instead of the misleading offset/limit hint.
func TestCapToolResultMarkerNonRangedTool(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)
	t.Setenv("WAKIL_SESSIONS_DIR", "")

	big := strings.Repeat("x", 2000)
	out := CapToolResult(big, "run_shell", "chat-cap-shell", 500)

	if strings.Contains(out, "offset/limit") {
		t.Errorf("non-ranged tool marker must not suggest offset/limit: %q", out[max(0, len(out)-200):])
	}
	if !strings.Contains(out, "result truncated") {
		t.Errorf("non-ranged tool marker missing 'result truncated': %q", out[max(0, len(out)-200):])
	}
	if len(out) > 500 {
		t.Errorf("capped output is %d chars, exceeds cap 500", len(out))
	}
}

// TestCapToolResultNoMarkerUnderCap verifies results within the cap pass
// through unchanged — no marker, no spill.
func TestCapToolResultNoMarkerUnderCap(t *testing.T) {
	small := "short result"
	out := CapToolResult(small, "read_file", "chat-under", 500)
	if out != small {
		t.Errorf("under-cap result was modified: %q", out)
	}
	if strings.Contains(out, "TRUNCATED") {
		t.Error("under-cap result must not carry a truncation marker")
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
