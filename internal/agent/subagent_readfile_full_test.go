package agent

// Live-run verification for Build 2: read_file_full inside a subagent.
//
// This test file proves the exact path that was broken (Finding 5):
//   - A subagent has Session==nil → chatID() returned "" → spill no-op'd.
//   - Now chatID() falls back to Client.ChatID (per-dispatch-unique UUID).
//
// Tests:
//  1. Small file: full content in one call, no window header, no re-reads.
//  2. ~256KB file: real spill path written under the subagent's UNIQUE chatID,
//     in-context result carries the [full content at: PATH] marker, and the
//     path is readable.
//  3. Binary: binary sniff refuses it.
//  4. Two different dispatches resolve DIFFERENT chatIDs (parallel door open).
//  5. CapOrStub's SpillFullResult produces a real path for a subagent App.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

// makeSubagentApp builds a subagent-style App (Session=nil, IsSubagent=true)
// with a per-dispatch-unique Client.ChatID, just like dispatchSubagent does.
func makeSubagentApp(t *testing.T, dir string) (*App, string) {
	t.Helper()
	exe, err := exec.NewDirectExecutor(dir)
	if err != nil {
		t.Fatal(err)
	}
	subChatID := NewChatID()
	app := &App{
		Cfg: config.Config{
			MaxFullReadBytes: 256 << 10,
			ToolResultCap:    8000,
		},
		Client:     &proxy.Client{ChatID: subChatID},
		Exec:       exe,
		Out:        os.Stderr,
		IsSubagent: true,
		Session:    nil, // THE BUG CONDITION
	}
	return app, subChatID
}

// --- Test 1: small file returns FULL content in one call, no window header ---

func TestSubagentReadFileFullSmallFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small.go")
	content := "package main\n\nfunc main() {}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	app, _ := makeSubagentApp(t, dir)

	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file_full", Arguments: `{"path":"small.go"}`,
	}})

	if strings.HasPrefix(res, "ERROR:") {
		t.Fatalf("expected full content, got error: %q", res)
	}
	// Must contain all lines.
	if !strings.Contains(res, "func main() {}") {
		t.Fatalf("file content missing from result:\n%s", res)
	}
	// Must NOT have a window header.
	if strings.Contains(res, "[lines ") {
		t.Fatalf("read_file_full must not window inside a subagent: %q", res)
	}
	t.Logf("OK: subagent read_file_full returned full content (%d bytes), no window header", len(res))
}

// --- Test 2: ~256KB file spills to disk with a REAL path under the subagent's chatID ---

func TestSubagentReadFileFullSpillPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.go")
	// Content just over 200 bytes (SpillFullResult threshold) and well under
	// the 256KB ceiling — exercises the spill path without hitting the size guard.
	big := strings.Repeat("// line of code\n", 30) // ~450 bytes
	if err := os.WriteFile(path, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}

	app, subChatID := makeSubagentApp(t, dir)

	// CapOrStub is what calls SpillFullResult. Simulate the Send loop's path:
	// handleToolCall returns the raw result, then CapOrStub processes it.
	raw := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file_full", Arguments: `{"path":"big.go"}`,
	}})

	result := app.CapOrStub(raw, "read_file_full", 0)

	// The result MUST carry the [full content at: PATH] marker.
	if !strings.Contains(result, "[full content at:") {
		t.Fatalf("subagent spill marker missing — the Finding 5 bug is not fixed.\n"+
			"Result tail: %q", tail(result, 200))
	}

	// Extract the spill path and verify the file exists on disk.
	spillPath := wtools.ExtractSpillPath(result)
	if spillPath == "" {
		t.Fatalf("ExtractSpillPath returned empty — marker is present but unparseable.\nResult tail: %q", tail(result, 200))
	}
	if _, err := os.Stat(spillPath); err != nil {
		t.Fatalf("spill file does not exist at %q: %v", spillPath, err)
	}

	// The spill path MUST be under the subagent's UNIQUE chatID directory.
	if !strings.Contains(spillPath, subChatID) {
		t.Fatalf("spill path %q does not contain the subagent's chatID %q — collision risk.",
			spillPath, subChatID)
	}

	// Reading the spill path MUST return the content.
	spilled, err := os.ReadFile(spillPath)
	if err != nil {
		t.Fatalf("cannot read spill file: %v", err)
	}
	if !strings.Contains(string(spilled), "line of code") {
		t.Fatalf("spill file content does not match the original file content")
	}

	t.Logf("OK: subagent spill path written and recoverable:")
	t.Logf("  chatID:      %s", subChatID)
	t.Logf("  spill path:  %s", spillPath)
	t.Logf("  spill size:  %d bytes", len(spilled))
}

// --- Test 3: binary sniff refuses ---

func TestSubagentReadFileFullBinaryRefusal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "wakil_bin")
	binary := []byte{0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00}
	for i := 0; i < 1000; i++ {
		binary = append(binary, 0x00, 0x01, 0x02, 0x03)
	}
	if err := os.WriteFile(path, binary, 0o755); err != nil {
		t.Fatal(err)
	}

	app, _ := makeSubagentApp(t, dir)

	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file_full", Arguments: `{"path":"wakil_bin"}`,
	}})

	if !strings.HasPrefix(res, "ERROR:") {
		t.Fatalf("expected binary-guard error, got: %q", res)
	}
	if !strings.Contains(res, "binary file") {
		t.Fatalf("expected 'binary file' in error, got: %q", res)
	}
	t.Logf("OK: subagent binary sniff refused: %q", res)
}

// --- Test 4: two dispatches resolve DIFFERENT chatIDs ---

func TestSubagentChatIDPerDispatchUnique(t *testing.T) {
	dir := t.TempDir()

	app1, id1 := makeSubagentApp(t, dir)
	app2, id2 := makeSubagentApp(t, dir)

	if id1 == "" || id2 == "" {
		t.Fatalf("chatIDs must not be empty: id1=%q id2=%q", id1, id2)
	}
	if id1 == id2 {
		t.Fatalf("two dispatches resolved the SAME chatID %q — spill dirs would collide", id1)
	}

	// Also confirm chatID() returns the Client.ChatID for each (not "").
	cid1 := app1.chatID()
	cid2 := app2.chatID()
	if cid1 != id1 {
		t.Errorf("app1.chatID()=%q, want %q (Client.ChatID)", cid1, id1)
	}
	if cid2 != id2 {
		t.Errorf("app2.chatID()=%q, want %q (Client.ChatID)", cid2, id2)
	}
	if cid1 == cid2 {
		t.Fatalf("chatID() returned the same value for two subagents: %q", cid1)
	}

	t.Logf("OK: two dispatches resolved different chatIDs:")
	t.Logf("  dispatch 1: %s", id1)
	t.Logf("  dispatch 2: %s", id2)
}

// --- Test 5: chatID() nil-safety (no panic when both Session and Client are nil) ---

func TestChatIDNoPanicWhenBothNil(t *testing.T) {
	app := &App{}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("chatID() panicked with nil Session and nil Client: %v", r)
		}
	}()
	got := app.chatID()
	if got != "" {
		t.Errorf("chatID() with nil Session and nil Client = %q, want empty", got)
	}
	t.Logf("OK: chatID() returns empty string (no panic) when Session and Client are both nil")
}

// --- Test 6: CapOrStub produces a real spill path for a subagent App (Stub path) ---

func TestSubagentCapOrStubStubPath(t *testing.T) {
	dir := t.TempDir()
	app, subChatID := makeSubagentApp(t, dir)
	// Set a low TurnToolBudget so the stub path fires.
	app.Cfg.TurnToolBudget = 100

	largeContent := strings.Repeat("x", 5000)
	result := app.CapOrStub(largeContent, "run_shell", 200)

	if !strings.Contains(result, "[budget —") {
		t.Fatalf("expected budget stub, got: %q", tail(result, 200))
	}
	if !strings.Contains(result, "at:") {
		t.Fatalf("budget stub must carry a spill path (Finding 5 bug). Got: %q", tail(result, 200))
	}

	spillPath := wtools.ExtractSpillPath(result)
	if spillPath == "" {
		t.Fatalf("ExtractSpillPath returned empty for subagent stub result.\nResult: %q", result)
	}
	if !strings.Contains(spillPath, subChatID) {
		t.Fatalf("stub spill path %q does not contain subagent chatID %q", spillPath, subChatID)
	}
	if _, err := os.Stat(spillPath); err != nil {
		t.Fatalf("stub spill file does not exist at %q: %v", spillPath, err)
	}
	t.Logf("OK: subagent StubToolResult produced a real spill path under chatID %s", subChatID)
}

// --- Test 7: CapToolResult produces a real spill path for a subagent App ---

func TestSubagentCapOrStubCapPath(t *testing.T) {
	dir := t.TempDir()
	app, subChatID := makeSubagentApp(t, dir)

	largeContent := strings.Repeat("y", 20000)
	// TurnToolBudget is 0 (unlimited) so the Cap path fires, not the Stub path.
	result := app.CapOrStub(largeContent, "read_file", 0)

	if !strings.Contains(result, "+") || !strings.Contains(result, "chars omitted") {
		t.Fatalf("expected cap truncation marker, got: %q", tail(result, 200))
	}
	if !strings.Contains(result, "full content at:") {
		t.Fatalf("cap result must carry a spill path (Finding 5 bug). Got: %q", tail(result, 200))
	}

	spillPath := wtools.ExtractSpillPath(result)
	if spillPath == "" {
		t.Fatalf("ExtractSpillPath returned empty for subagent cap result.\nResult: %q", result)
	}
	if !strings.Contains(spillPath, subChatID) {
		t.Fatalf("cap spill path %q does not contain subagent chatID %q", spillPath, subChatID)
	}
	if _, err := os.Stat(spillPath); err != nil {
		t.Fatalf("cap spill file does not exist at %q: %v", spillPath, err)
	}
	t.Logf("OK: subagent CapToolResult produced a real spill path under chatID %s", subChatID)
}

// tail returns the last n chars of s.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
