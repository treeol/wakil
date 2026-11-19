package tools

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchHostCacheOwnDirOnly(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", "")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	a := SpillToCache("c1", "turn-1", "one\ntwo NEEDLE\nthree\n")
	b := SpillToCache("c1", "turn-2", "NEEDLE again\n")
	foreign := SpillToCache("c2", "turn-1", "NEEDLE of c2\n")
	dir := filepath.Dir(a)

	out, err := SearchHostCache("c1", dir, "NEEDLE", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, a+":2:two NEEDLE") || !strings.Contains(out, b+":1:NEEDLE again") || strings.Contains(out, "c2") {
		t.Fatalf("directory search: %q", out)
	}
	if out, err = SearchHostCache("c1", a, "NEEDLE", "", false); err != nil || strings.Count(out, "\n") != 0 {
		t.Fatalf("single-file search: %q, %v", out, err)
	}
	for _, p := range []string{foreign, filepath.Dir(foreign), ToolCacheRoot(), "/etc/passwd", ""} {
		if _, err := SearchHostCache("c1", p, "NEEDLE", "", false); !errors.Is(err, ErrHostCacheEscape) {
			t.Errorf("path %q: err = %v, want ErrHostCacheEscape", p, err)
		}
	}
	if _, err := SearchHostCache("../c2", foreign, "NEEDLE", "", false); err == nil {
		t.Error("a traversing chat ID must be refused")
	}
}

func TestSearchHostCacheBinaryCapAndPattern(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", "")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	dir := filepath.Join(ToolCacheRoot(), "c1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("bin.dat", "NEEDLE\x00\x01binary")
	write("many.txt", strings.Repeat("NEEDLE x\n", 500))
	write("regex.txt", "a(b\nfoo123\n")

	out, err := SearchHostCache("c1", dir, "NEEDLE", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "bin.dat") {
		t.Error("binary files are skipped (grep -I)")
	}
	if got := strings.Count(out, "many.txt:"); got != hostSearchMaxMatches || !strings.Contains(out, "capped at") {
		t.Errorf("matches = %d (want cap %d), capped note present = %v", got, hostSearchMaxMatches, strings.Contains(out, "capped at"))
	}
	// A regex works; an invalid one falls back to a literal match.
	if out, _ = SearchHostCache("c1", dir, `foo\d+`, "regex.txt", false); !strings.Contains(out, "foo123") {
		t.Errorf("regex: %q", out)
	}
	if out, _ = SearchHostCache("c1", dir, "a(b", "regex.txt", false); !strings.Contains(out, "a(b") {
		t.Errorf("literal fallback for an invalid regex: %q", out)
	}
	if _, err := SearchHostCache("c1", dir, "x", "[", false); err == nil {
		t.Error("a bad file_pattern must be an error")
	}
}

// The chat directory itself being a symlink — to another session's directory
// or to a directory outside the toolcache — is refused, not followed.
func TestSearchHostCacheRefusesSymlinkedChatDir(t *testing.T) {
	t.Setenv("WAKIL_SESSIONS_DIR", "")
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	foreign := SpillToCache("c2", "turn-1", "NEEDLE of c2\n")
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "f.txt"), []byte("NEEDLE outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"c-to-foreign": filepath.Dir(foreign), "c-to-outside": outside} {
		link := filepath.Join(ToolCacheRoot(), name)
		if err := os.Symlink(target, link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		out, err := SearchHostCache(name, link, "NEEDLE", "", false)
		if !errors.Is(err, ErrHostCacheEscape) || strings.Contains(out, "NEEDLE") {
			t.Errorf("%s: out=%q err=%v, want ErrHostCacheEscape and no content", name, out, err)
		}
	}
}
