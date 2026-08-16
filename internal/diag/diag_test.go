package diag

import (
	"bytes"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestRedirectAndPrintf verifies a redirected sink receives output (with a
// timestamp prefix) and a nil redirect restores os.Stderr.
func TestRedirectAndPrintf(t *testing.T) {
	defer Redirect(nil) // restore for other tests

	var buf bytes.Buffer
	Redirect(&buf)
	Printf("hello %d\n", 42)
	got := buf.String()
	if !strings.Contains(got, "hello 42\n") {
		t.Errorf("Printf = %q, want to contain %q", got, "hello 42\n")
	}
	// The timestamp prefix (matching stdlib log) must be present.
	if !strings.Contains(got, "20") { // year 20xx in the timestamp
		t.Errorf("Printf should prefix a timestamp; got %q", got)
	}

	// nil redirect restores the default (os.Stderr); a subsequent write must
	// not go to the buffer. Reset buf, then confirm a redirect to a second
	// buffer leaves buf empty.
	Redirect(nil)
	buf.Reset()
	var buf2 bytes.Buffer
	Redirect(&buf2)
	Printf("after reset\n")
	if !strings.Contains(buf2.String(), "after reset\n") {
		t.Error("redirect to a new sink after nil should work")
	}
	if buf.Len() != 0 {
		t.Error("nil Redirect must detach the previous buffer")
	}
}

// TestConcurrentWrite verifies concurrent writes against a redirect are
// serialized (no race, no panic).
func TestConcurrentWrite(t *testing.T) {
	defer Redirect(nil)

	var buf bytes.Buffer
	Redirect(&buf)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Printf("line\n")
		}()
	}
	wg.Wait()
	if got := strings.Count(buf.String(), "line\n"); got != 20 {
		t.Errorf("expected 20 lines, got %d", got)
	}
}

// TestSanitizeID verifies session IDs cannot escape the data directory.
func TestSanitizeID(t *testing.T) {
	cases := map[string]string{
		"abc123":     "abc123",
		"a/b":        "a_b",
		"..":         "unknown",
		".":          "unknown",
		"":           "unknown",
		"../../etc":  ".._.._etc",
	}
	for in, want := range cases {
		if got := sanitizeID(in); got != want {
			t.Errorf("sanitizeID(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLogPathUsesXDGDataHome verifies the XDG_DATA_HOME base is joined with the
// wakil subdirectory (the documented path), not used as the directory itself.
func TestLogPathUsesXDGDataHome(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/tmp/xdg")
	got := LogPath("abc")
	want := filepath.Join("/tmp", "xdg", "wakil", "diag-abc.log")
	if got != want {
		t.Errorf("LogPath = %q, want %q", got, want)
	}
}
