package tools

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestHostOpenCmd(t *testing.T) {
	cmd := hostOpenCmd("http://localhost:23000")
	got := strings.Join(cmd.Args, " ")
	var want string
	switch runtime.GOOS {
	case "darwin":
		want = "open http://localhost:23000"
	case "windows":
		want = "start  http://localhost:23000" // cmd /c start "" <url>
	default:
		want = "xdg-open http://localhost:23000"
	}
	if !strings.HasSuffix(got, want) {
		t.Fatalf("args = %q, want suffix %q", got, want)
	}
}

func TestOpenOnHostEmpty(t *testing.T) {
	if _, err := OpenOnHost("   "); err == nil {
		t.Fatal("expected error for empty target")
	}
}

// A test binary must never reach the user's desktop: with a recording xdg-open
// first on PATH, OpenOnHost must refuse and must not run it.
func TestOpenOnHostRefusesInTestBinary(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "launched")
	for _, name := range []string{"xdg-open", "open"} {
		script := "#!/bin/sh\necho \"$0 $*\" >> " + log + "\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if _, err := OpenOnHost("https://example.com"); !errors.Is(err, errHostOpenInTest) {
		t.Fatalf("OpenOnHost in a test binary = %v, want errHostOpenInTest", err)
	}
	time.Sleep(200 * time.Millisecond) // a launcher, if it had started, would have logged by now
	if _, err := os.Stat(log); err == nil {
		b, _ := os.ReadFile(log)
		t.Fatalf("a host launcher was started from a test: %s", b)
	}
}
