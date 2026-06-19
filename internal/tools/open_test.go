package tools

import (
	"runtime"
	"strings"
	"testing"
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
