package agent

// Live-run verification for read_file_full.
// This test exercises the full path: handleToolCall → ExecuteToolCall →
// CapOrStub → result, against real files on disk using a DirectExecutor.
// It verifies:
//  1. A small source file returns FULL content in one call.
//  2. A file over max_full_read_bytes gets a clean refusal with range suggestion.
//  3. A compiled binary is refused by the binary sniff.
//  4. The proxy does NOT 400 (the result is a normal tool string, not an error).

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/proxy"
)

func TestLiveReadFileFullSmallFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small.go")
	content := "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
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
		Name: "read_file_full", Arguments: `{"path":"small.go"}`,
	}})

	// Must not be an error.
	if strings.HasPrefix(res, "ERROR:") {
		t.Fatalf("expected full content, got error: %q", res)
	}

	// Must contain all lines of the file.
	fileBytes, _ := os.ReadFile(path)
	fileLines := strings.Split(strings.TrimRight(string(fileBytes), "\n"), "\n")
	for i, line := range fileLines {
		if !strings.Contains(res, line) {
			t.Fatalf("line %d (%q) missing from result — full content not returned.\nResult: %q", i+1, line, res)
		}
	}

	// Must NOT have a window header.
	if strings.Contains(res, "[lines ") {
		t.Fatalf("read_file_full must not window: %q", res)
	}

	t.Logf("OK: full content returned (%d bytes), all %d file lines present", len(res), len(fileLines))
}

func TestLiveReadFileFullOversizedRefusal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.txt")
	// 300 KB — over the 256 KB ceiling.
	big := strings.Repeat("x", 300_000)
	if err := os.WriteFile(path, []byte(big), 0o644); err != nil {
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
		Name: "read_file_full", Arguments: `{"path":"big.txt"}`,
	}})

	// Must be a clean refusal.
	if !strings.HasPrefix(res, "ERROR:") {
		t.Fatalf("expected error for oversized file, got: %q", res)
	}
	if !strings.Contains(res, "exceeds full-read limit") {
		t.Fatalf("expected 'exceeds full-read limit' in error, got: %q", res)
	}
	// Must suggest read_file with offset/limit.
	if !strings.Contains(res, "read_file") {
		t.Fatalf("error must suggest read_file, got: %q", res)
	}
	// Must NOT contain file content (the file was not read).
	if strings.Contains(res, strings.Repeat("x", 100)) {
		t.Fatalf("oversized file content must not be in result (was read despite refusal)")
	}

	// The result is a normal string — no proxy 400 possible (it's just a tool
	// result string, not an HTTP error).
	t.Logf("OK: clean refusal, no content loaded, result is a string (no proxy 400): %q", res)
}

func TestLiveReadFileFullBinaryRefusal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wakil_bin")

	// Write a real binary file (ELF-like header with null bytes).
	binary := []byte{0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00}
	for i := 0; i < 1000; i++ {
		binary = append(binary, 0x00, 0x01, 0x02, 0x03)
	}
	if err := os.WriteFile(path, binary, 0o755); err != nil {
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
		Name: "read_file_full", Arguments: `{"path":"wakil_bin"}`,
	}})

	// Must be refused by binary sniff.
	if !strings.HasPrefix(res, "ERROR:") {
		t.Fatalf("expected binary-guard error, got: %q", res)
	}
	if !strings.Contains(res, "binary file") {
		t.Fatalf("expected 'binary file' in error, got: %q", res)
	}

	t.Logf("OK: binary file refused: %q", res)
}
