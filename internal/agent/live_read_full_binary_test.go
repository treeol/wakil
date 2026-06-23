package agent

// Live-run verification: read_file_full against a real compiled binary.
// Confirms the binary sniff refuses a real ELF executable (not just a
// synthetic one). Uses a small compiled binary (under the 256 KB ceiling)
// so the size guard doesn't fire first — the binary sniff is what must
// catch it.

import (
	"context"
	"os"
	"strings"
	"testing"

	"wakil/internal/config"
	"wakil/internal/exec"
	"wakil/internal/proxy"
)

func TestLiveReadFileFullRealBinary(t *testing.T) {
	binPath := "/tmp/tiny_bin"
	info, err := os.Stat(binPath)
	if err != nil {
		t.Skipf("test binary not found at %s: %v (compile with: gcc -o /tmp/tiny_bin -x c -<<< 'int main(){return 0;}')", binPath, err)
	}
	if info.Size() > 256<<10 {
		t.Fatalf("test binary too large (%d bytes) — must be under 256KB so the size guard doesn't fire first", info.Size())
	}
	t.Logf("Testing against real ELF binary: %s (%d bytes)", binPath, info.Size())

	// Copy the binary into the executor's workspace.
	dir := t.TempDir()
	src, err := os.ReadFile(binPath)
	if err != nil {
		t.Fatal(err)
	}
	dst := dir + "/tiny_bin"
	if err := os.WriteFile(dst, src, 0o755); err != nil {
		t.Fatal(err)
	}

	exe, err := exec.NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{
		Cfg:  config.Config{MaxFullReadBytes: 256 << 10, ToolResultCap: 8000},
		Exec: exe,
		Out:  os.Stderr,
	}

	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file_full", Arguments: `{"path":"tiny_bin"}`,
	}})

	// The size guard must NOT fire (file is under ceiling). The binary sniff
	// must refuse it.
	if strings.Contains(res, "exceeds full-read limit") {
		t.Fatalf("size guard must not fire for small binary — the binary sniff should catch it. Got: %q", res)
	}
	if !strings.HasPrefix(res, "ERROR:") {
		t.Fatalf("expected binary-guard error for real ELF, got result of length %d", len(res))
	}
	if !strings.Contains(res, "binary file") {
		t.Fatalf("expected 'binary file' in error, got: %q", res)
	}

	t.Logf("OK: real ELF binary (%d bytes) refused by binary sniff: %q", info.Size(), res)
}
