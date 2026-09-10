package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/config"
)

func TestNewHookEngine_NoHooks(t *testing.T) {
	h := NewHookEngine(config.HooksConfig{}, "/work")
	if h != nil {
		t.Error("expected nil when no hooks configured")
	}
}

func TestNewHookEngine_WithHooks(t *testing.T) {
	cfg := config.HooksConfig{
		PostTool: []config.HookConfig{{Command: "echo hi"}},
	}
	h := NewHookEngine(cfg, "/work")
	if h == nil {
		t.Fatal("expected non-nil hook engine")
	}
}

func TestToolMatches(t *testing.T) {
	tests := []struct {
		pattern, tool string
		want          bool
	}{
		{"", "write_file", true},          // empty matches all
		{"write_file", "write_file", true},  // exact
		{"write_file", "run_shell", false},  // mismatch
		{"write_file|edit_file", "edit_file", true},   // pipe alt
		{"write_file|edit_file", "write_file", true},  // pipe alt
		{"write_file|edit_file", "run_shell", false},  // no match
		{"write_file | edit_file", "edit_file", true}, // spaces
	}
	for _, tc := range tests {
		got := toolMatches(tc.pattern, tc.tool)
		if got != tc.want {
			t.Errorf("toolMatches(%q, %q) = %v, want %v", tc.pattern, tc.tool, got, tc.want)
		}
	}
}

func TestExtractFilePath(t *testing.T) {
	tests := []struct {
		tool, args, want string
	}{
		{"write_file", `{"path":"/foo/bar.go","content":"x"}`, "/foo/bar.go"},
		{"edit_file", `{"path":"/foo/bar.go","old":"a","new":"b"}`, "/foo/bar.go"},
		{"move_file", `{"src":"/a","dst":"/b"}`, "/a"},
		{"run_shell", `{"command":"ls"}`, ""},
		{"write_file", `{"content":"x"}`, ""}, // no path field
	}
	for _, tc := range tests {
		got := extractFilePath(tc.tool, tc.args)
		if got != tc.want {
			t.Errorf("extractFilePath(%q, %q) = %q, want %q", tc.tool, tc.args, got, tc.want)
		}
	}
}

func TestJsonField(t *testing.T) {
	tests := []struct {
		tool, args, want string
	}{
		{"write_file", `{"path":"foo.go","content":"x"}`, "foo.go"},
		{"write_file", `{"path": "foo.go", "content": "x"}`, "foo.go"},     // whitespace
		{"write_file", `{"content":"x"}`, ""},                               // no path field
		{"write_file", ``, ""},                                               // empty
		{"move_file", `{"src":"/a","dst":"/b"}`, "/a"},                      // move src
		{"edit_file", `{"path":"bar.go","old":"a","new":"b"}`, "bar.go"},    // edit
		{"run_shell", `{"command":"ls"}`, ""},                               // not a file tool
	}
	for _, tc := range tests {
		got := extractFilePath(tc.tool, tc.args)
		if got != tc.want {
			t.Errorf("extractFilePath(%q, %q) = %q, want %q", tc.tool, tc.args, got, tc.want)
		}
	}
}

func TestRunPreToolHooks_Blocks(t *testing.T) {
	dir := t.TempDir()
	cfg := config.HooksConfig{
		PreTool: []config.HookConfig{
			{Tool: "write_file", Command: "exit 1"},
		},
	}
	h := NewHookEngine(cfg, dir)
	if h == nil {
		t.Fatal("expected non-nil")
	}

	r := h.RunPreToolHooks(context.Background(), "write_file", `{"path":"/foo"}`, dir)
	if !r.blocked {
		t.Error("expected hook to block")
	}
	if r.blockMsg == "" {
		t.Error("expected block message")
	}
}

func TestRunPreToolHooks_Allows(t *testing.T) {
	dir := t.TempDir()
	cfg := config.HooksConfig{
		PreTool: []config.HookConfig{
			{Tool: "write_file", Command: "exit 0"},
		},
	}
	h := NewHookEngine(cfg, dir)

	r := h.RunPreToolHooks(context.Background(), "write_file", `{"path":"/foo"}`, dir)
	if r.blocked {
		t.Error("expected hook to allow")
	}
}

func TestRunPreToolHooks_NoMatch(t *testing.T) {
	dir := t.TempDir()
	cfg := config.HooksConfig{
		PreTool: []config.HookConfig{
			{Tool: "write_file", Command: "exit 1"},
		},
	}
	h := NewHookEngine(cfg, dir)

	// run_shell doesn't match the write_file pattern — hook doesn't fire.
	r := h.RunPreToolHooks(context.Background(), "run_shell", `{"command":"ls"}`, dir)
	if r.blocked {
		t.Error("hook should not fire for non-matching tool")
	}
}

func TestRunPreToolHooks_EmptyPatternMatchesAll(t *testing.T) {
	dir := t.TempDir()
	cfg := config.HooksConfig{
		PreTool: []config.HookConfig{
			{Command: "exit 1"}, // no Tool field — matches all
		},
	}
	h := NewHookEngine(cfg, dir)

	for _, tool := range []string{"write_file", "run_shell", "read_file", "edit_file"} {
		r := h.RunPreToolHooks(context.Background(), tool, "{}", dir)
		if !r.blocked {
			t.Errorf("empty pattern should match all tools, %s not blocked", tool)
		}
	}
}

func TestRunPostToolHooks_InjectsOutput(t *testing.T) {
	dir := t.TempDir()
	cfg := config.HooksConfig{
		PostTool: []config.HookConfig{
			{Tool: "write_file", Command: "echo formatted-ok"},
		},
	}
	h := NewHookEngine(cfg, dir)

	r := h.RunPostToolHooks(context.Background(), "write_file", `{"path":"/foo"}`, dir)
	if r.blocked {
		t.Error("post-hook should not block")
	}
	if r.output == "" {
		t.Error("expected output from post-hook")
	}
	if !hookContains(r.output, "formatted-ok") {
		t.Errorf("output should contain 'formatted-ok', got: %s", r.output)
	}
}

func TestRunPostToolHooks_CannotBlock(t *testing.T) {
	dir := t.TempDir()
	cfg := config.HooksConfig{
		PostTool: []config.HookConfig{
			{Command: "exit 1"}, // non-zero, but post-hook cannot block
		},
	}
	h := NewHookEngine(cfg, dir)

	r := h.RunPostToolHooks(context.Background(), "write_file", `{}`, dir)
	if r.blocked {
		t.Error("post-hook should not block even on non-zero exit")
	}
}

func TestRunSessionHooks(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "session_started.txt")
	cfg := config.HooksConfig{
		SessionStart: []config.HookConfig{
			{Command: "touch " + marker},
		},
	}
	h := NewHookEngine(cfg, dir)

	h.RunSessionHooks(context.Background(), hookSessionStart)
	time.Sleep(100 * time.Millisecond) // give the touch time to complete

	if _, err := os.Stat(marker); err != nil {
		t.Error("session_start hook should have created marker file")
	}
}

func TestHookTimeout(t *testing.T) {
	dir := t.TempDir()
	cfg := config.HooksConfig{
		PreTool: []config.HookConfig{
			{Command: "sleep 60"}, // would exceed 2s timeout
		},
	}
	h := NewHookEngine(cfg, dir)
	h.SetTimeout(2 * time.Second) // short timeout for testing

	start := time.Now()
	r := h.RunPreToolHooks(context.Background(), "write_file", `{}`, dir)
	elapsed := time.Since(start)

	if !r.blocked {
		t.Error("expected timeout to produce blocked result")
	}
	if elapsed > 5*time.Second {
		t.Errorf("timeout not enforced: took %v", elapsed)
	}
}

func TestHookEnvironment(t *testing.T) {
	dir := t.TempDir()
	cfg := config.HooksConfig{
		PostTool: []config.HookConfig{
			{Command: "echo $WAKIL_TOOL_NAME:$WAKIL_FILE_PATH:$WAKIL_CWD"},
		},
	}
	h := NewHookEngine(cfg, dir)

	r := h.RunPostToolHooks(context.Background(), "write_file", `{"path":"test.go"}`, "/work")
	if !hookContains(r.output, "write_file") {
		t.Errorf("expected WAKIL_TOOL_NAME in output, got: %s", r.output)
	}
	if !hookContains(r.output, "test.go") {
		t.Errorf("expected WAKIL_FILE_PATH in output, got: %s", r.output)
	}
}

func hookContains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(substr) == 0 ||
		(len(s) > 0 && len(substr) > 0 && indexOf(s, substr) >= 0))
}

func indexOf(s, substr string) int {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
