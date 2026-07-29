package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// TestConvSnapshot_Empty returns nil for empty Conv.
func TestConvSnapshot_Empty(t *testing.T) {
	app := &App{}
	snap := app.ConvSnapshot()
	if snap != nil {
		t.Errorf("expected nil for empty Conv, got %d messages", len(snap))
	}
}

// TestConvSnapshot_CompleteBoundary returns a copy when the last message
// is a complete turn (no dangling tool calls).
func TestConvSnapshot_CompleteBoundary(t *testing.T) {
	app := &App{
		Conv: []proxy.Message{
			{Role: "system", Content: StrPtr("sys")},
			{Role: "user", Content: StrPtr("hello")},
			{Role: "assistant", Content: StrPtr("hi")},
		},
	}
	snap := app.ConvSnapshot()
	if len(snap) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(snap))
	}
	// Verify it's a copy, not the same slice.
	snap[0].Role = "modified"
	if app.Conv[0].Role == "modified" {
		t.Error("snapshot should be a copy, not alias the original")
	}
}

// TestConvSnapshot_DanglingToolCalls trims trailing assistant+tool_calls.
func TestConvSnapshot_DanglingToolCalls(t *testing.T) {
	app := &App{
		Conv: []proxy.Message{
			{Role: "system", Content: StrPtr("sys")},
			{Role: "user", Content: StrPtr("run ls")},
			{Role: "assistant", Content: StrPtr(""), ToolCalls: []proxy.ToolCall{{ID: "tc1", Type: "function", Function: proxy.FunctionCall{Name: "run_shell", Arguments: `{}`}}}},
		},
	}
	snap := app.ConvSnapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 messages (system+user), got %d", len(snap))
	}
	if snap[1].Role != "user" {
		t.Errorf("last message should be user, got %s", snap[1].Role)
	}
}

// TestConvSnapshot_PartialToolResults trims trailing partial tool results
// and the assistant message that issued them.
func TestConvSnapshot_PartialToolResults(t *testing.T) {
	app := &App{
		Conv: []proxy.Message{
			{Role: "system", Content: StrPtr("sys")},
			{Role: "user", Content: StrPtr("run ls")},
			{Role: "assistant", Content: StrPtr(""), ToolCalls: []proxy.ToolCall{{ID: "tc1"}, {ID: "tc2"}}},
			{Role: "tool", ToolCallID: "tc1", Content: StrPtr("output1")},
			// tc2 result is missing — incomplete block
		},
	}
	snap := app.ConvSnapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 messages (system+user, incomplete block trimmed), got %d", len(snap))
	}
}

// TestSanitizeConv_NilInput returns nil.
func TestSanitizeConv_NilInput(t *testing.T) {
	result := sanitizeConvForSideQuestion(nil)
	if result != nil {
		t.Errorf("expected nil, got %d messages", len(result))
	}
}

// TestSanitizeConv_AssistantNoToolCalls returns full copy when assistant
// message has no tool calls.
func TestSanitizeConv_AssistantNoToolCalls(t *testing.T) {
	conv := []proxy.Message{
		{Role: "user", Content: StrPtr("hi")},
		{Role: "assistant", Content: StrPtr("hello")},
	}
	result := sanitizeConvForSideQuestion(conv)
	if len(result) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(result))
	}
}

// TestFindLastCompleteBoundary_AllDangling returns nil when the entire
// conversation is one incomplete tool-call block.
func TestFindLastCompleteBoundary_AllDangling(t *testing.T) {
	conv := []proxy.Message{
		{Role: "assistant", ToolCalls: []proxy.ToolCall{{ID: "tc1"}}},
	}
	result := findLastCompleteBoundary(conv)
	if result != nil {
		t.Errorf("expected nil for all-dangling, got %d messages", len(result))
	}
}

// TestCloneClientForSideQuestion_NilClient returns nil.
func TestCloneClientForSideQuestion_NilClient(t *testing.T) {
	app := &App{}
	client := app.cloneClientForSideQuestion()
	if client != nil {
		t.Error("expected nil for nil client")
	}
}

// TestCloneClientForSideQuestion_CopiesFields returns a client with
// the same essential fields as the parent.
func TestCloneClientForSideQuestion_CopiesFields(t *testing.T) {
	parent := &proxy.Client{
		BaseURL:         "http://test",
		Model:           "test-model",
		AuthHeader:      "Bearer test",
		Backend:         "test-backend",
		Kind:            "openai",
		ConfiguredModel: "configured-model",
		AuxModel:        "aux-model",
		MaxRequestBytes: 12345,
	}
	app := &App{Client: parent}
	clone := app.cloneClientForSideQuestion()
	if clone == nil {
		t.Fatal("expected non-nil clone")
	}
	if clone.BaseURL != parent.BaseURL {
		t.Errorf("BaseURL: got %q, want %q", clone.BaseURL, parent.BaseURL)
	}
	if clone.Model != parent.Model {
		t.Errorf("Model: got %q, want %q", clone.Model, parent.Model)
	}
	if clone.ConfiguredModel != parent.ConfiguredModel {
		t.Errorf("ConfiguredModel: got %q, want %q", clone.ConfiguredModel, parent.ConfiguredModel)
	}
	if clone.AuxModel != parent.AuxModel {
		t.Errorf("AuxModel: got %q, want %q", clone.AuxModel, parent.AuxModel)
	}
	if clone.MaxRequestBytes != parent.MaxRequestBytes {
		t.Errorf("MaxRequestBytes: got %d, want %d", clone.MaxRequestBytes, parent.MaxRequestBytes)
	}
	if clone.ChatID == parent.ChatID {
		t.Error("clone should have a different ChatID from parent")
	}
	if !clone.NoMemoryWrite {
		t.Error("clone should have NoMemoryWrite=true")
	}
}

// TestRunShellWithDeadline_FastCommand runs a command that finishes within
// the deadline and returns its output.
func TestRunShellWithDeadline_FastCommand(t *testing.T) {
	// Use an executor that creates the log file so ReadFile can find it.
	exec := &logWritingExec{fakeExecutor: newFakeExecutor(), content: "fast output"}
	app := newTestApp("http://unused.invalid", exec, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ShellTimeoutSec = 5
	result := app.runShellWithDeadline(context.Background(), "echo hello", true)
	if !strings.Contains(result, "fast output") {
		t.Errorf("expected 'fast output' from fast command, got: %q", result)
	}
}

// logWritingExec wraps fakeExecutor but writes the log file on StartBackground
// so ReadFile can find it after the process "exits" (IsProcessAlive=false).
type logWritingExec struct {
	*fakeExecutor
	content string
}

func (e *logWritingExec) StartBackground(_ context.Context, command, logPath string) (int, int, error) {
	// Write the log file so ReadFile can find it.
	_ = os.WriteFile(logPath, []byte(e.content), 0600)
	// Also store in fakeExecutor's files map so ReadFile finds it.
	e.fakeExecutor.files[logPath] = e.content
	return e.fakeExecutor.StartBackground(context.Background(), command, logPath)
}

// TestRunShellWithDeadline_Disabled falls back to blocking when
// ShellTimeoutSec is 0 (tested via handleRunShell integration).
func TestRunShellWithDeadline_BgLimitFallback(t *testing.T) {
	app := newTestApp("http://unused.invalid", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ShellTimeoutSec = 5
	// Fill up the bg registry to trigger the fallback path.
	app.bgMu.Lock()
	app.bgProcs = make(map[string]*bgEntry)
	for i := 0; i < 5; i++ {
		app.bgProcs[fmt.Sprintf("bg%d", i+1)] = &bgEntry{
			id:         fmt.Sprintf("bg%d", i+1),
			pid:        1000 + i,
			pgid:       1000 + i,
			generation: 1,
		}
	}
	app.bgMu.Unlock()
	// fakeExecutor.IsProcessAlive returns false, so live count is 0.
	// We need alivePids to return true — use aliveExecutorImpl.
	app.Exec = &aliveExecutorImpl{fakeExecutor: newFakeExecutor()}
	// Now all 5 are "alive" → the 6th should fall back to blocking.
	result := app.runShellWithDeadline(context.Background(), "echo fallback", true)
	if !strings.Contains(result, "ran: echo fallback") {
		t.Errorf("expected fallback to blocking, got: %q", result)
	}
}

// TestRunShellWithDeadline_TimeoutBackgrounds when the command doesn't
// finish before the deadline, it returns a "still running" pointer.
func TestRunShellWithDeadline_TimeoutBackgrounds(t *testing.T) {
	app := newTestApp("http://unused.invalid", &aliveExecutorImpl{fakeExecutor: newFakeExecutor()}, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.ShellTimeoutSec = 1
	result := app.runShellWithDeadline(context.Background(), "sleep 60", false)
	if !strings.Contains(result, "still running as bg") {
		t.Errorf("expected 'still running' message, got: %q", result)
	}
	// Clean up the background process.
	app.StopAllBackgroundProcs()
}

// TestStartSideQuestion_NilClient sends a done event with an error.
func TestStartSideQuestion_NilClient(t *testing.T) {
	app := &App{
		Cfg: config.DefaultConfig(),
		Out: io.Discard,
	}
	var received SideQuestionDoneMsg
	app.EventSink = func(msg interface{}) {
		if done, ok := msg.(SideQuestionDoneMsg); ok {
			received = done
		}
	}
	cancel := app.StartSideQuestion(context.Background(), "test question")
	defer cancel()
	if received.Err == nil {
		t.Error("expected error for nil client")
	}
}
