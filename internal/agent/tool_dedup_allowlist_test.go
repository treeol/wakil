package agent

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

func newDedupApp(exec *fakeExecutor) *App {
	cfg := config.DefaultConfig()
	cfg.ShellTimeoutSec = 0
	return &App{
		Cfg:       cfg,
		Exec:      exec,
		ToolCache: map[string]*toolDedupEntry{},
		Out:       io.Discard,
		Confirm:   func(_, _, _ string, _ bool) bool { return true },
	}
}

func dedupTC(name, args string) proxy.ToolCall {
	return proxy.ToolCall{Function: proxy.FunctionCall{Name: name, Arguments: args}}
}

func shellCount(exec *fakeExecutor) int {
	exec.mu.Lock()
	defer exec.mu.Unlock()
	return len(exec.shellCalls)
}

func TestDedupAllowsShellAfterWriteCheckWrite(t *testing.T) {
	exec := newFakeExecutor()
	app := newDedupApp(exec)
	ctx := context.Background()

	r := app.handleToolCall(ctx, dedupTC("write_file", `{"path":"answer.txt","content":"A"}`))
	if !r.ok {
		t.Fatalf("write A failed: %q", r.text)
	}
	r = app.handleToolCall(ctx, dedupTC("run_shell", `{"command":"./check.sh"}`))
	if !r.ok || !strings.Contains(r.text, "ran: ./check.sh") {
		t.Fatalf("first check should execute as run_shell, got ok=%v text=%q", r.ok, r.text)
	}
	r = app.handleToolCall(ctx, dedupTC("write_file", `{"path":"answer.txt","content":"B"}`))
	if !r.ok {
		t.Fatalf("write B failed: %q", r.text)
	}
	r = app.handleToolCall(ctx, dedupTC("run_shell", `{"command":"./check.sh"}`))
	if !r.ok || !strings.Contains(r.text, "ran: ./check.sh") {
		t.Fatalf("second check should execute after the intervening write, got ok=%v text=%q", r.ok, r.text)
	}
	if got := shellCount(exec); got != 2 {
		t.Fatalf("executor saw %d shell calls, want 2", got)
	}
}

func TestDedupExecutesSameShellThreeTimes(t *testing.T) {
	exec := newFakeExecutor()
	app := newDedupApp(exec)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		r := app.handleToolCall(ctx, dedupTC("run_shell", `{"command":"./check.sh"}`))
		if !r.ok || !strings.Contains(r.text, "ran: ./check.sh") {
			t.Fatalf("shell call %d should execute, got ok=%v text=%q", i+1, r.ok, r.text)
		}
	}
	if got := shellCount(exec); got != 3 {
		t.Fatalf("executor saw %d shell calls, want 3", got)
	}
}

func TestDedupRefusesRepeatedRead(t *testing.T) {
	exec := newFakeExecutor()
	exec.files["x.go"] = "package main"
	app := newDedupApp(exec)
	ctx := context.Background()

	r := app.handleToolCall(ctx, dedupTC("read_file", `{"path":"x.go"}`))
	if !r.ok {
		t.Fatalf("first read failed: %q", r.text)
	}
	r = app.handleToolCall(ctx, dedupTC("read_file", `{"path":"x.go"}`))
	if r.ok || !strings.Contains(r.text, "already called read_file") {
		t.Fatalf("second identical read should be refused, got ok=%v text=%q", r.ok, r.text)
	}
}

func TestDedupShellInvalidatesPreviousRead(t *testing.T) {
	exec := newFakeExecutor()
	exec.files["x.go"] = "before"
	app := newDedupApp(exec)
	ctx := context.Background()

	r := app.handleToolCall(ctx, dedupTC("read_file", `{"path":"x.go"}`))
	if !r.ok || !strings.Contains(r.text, "before") {
		t.Fatalf("first read should return before, got ok=%v text=%q", r.ok, r.text)
	}
	exec.files["x.go"] = "after"
	r = app.handleToolCall(ctx, dedupTC("run_shell", `{"command":"echo change"}`))
	if !r.ok {
		t.Fatalf("shell failed: %q", r.text)
	}
	r = app.handleToolCall(ctx, dedupTC("read_file", `{"path":"x.go"}`))
	if !r.ok || !strings.Contains(r.text, "after") {
		t.Fatalf("read after shell should execute and see after, got ok=%v text=%q", r.ok, r.text)
	}
	if strings.Contains(r.text, "already called") {
		t.Fatalf("read after shell must not be deduped, got %q", r.text)
	}
}

func TestDedupWriteInvalidatesPreviousRead(t *testing.T) {
	exec := newFakeExecutor()
	exec.files["x.go"] = "before"
	app := newDedupApp(exec)
	ctx := context.Background()

	r := app.handleToolCall(ctx, dedupTC("read_file", `{"path":"x.go"}`))
	if !r.ok || !strings.Contains(r.text, "before") {
		t.Fatalf("first read should return before, got ok=%v text=%q", r.ok, r.text)
	}
	r = app.handleToolCall(ctx, dedupTC("write_file", `{"path":"y.go","content":"written"}`))
	if !r.ok {
		t.Fatalf("write failed: %q", r.text)
	}
	r = app.handleToolCall(ctx, dedupTC("read_file", `{"path":"x.go"}`))
	if !r.ok || !strings.Contains(r.text, "before") {
		t.Fatalf("read after write should execute, got ok=%v text=%q", r.ok, r.text)
	}
	if strings.Contains(r.text, "already called") {
		t.Fatalf("read after write must not be deduped, got %q", r.text)
	}
}

func TestDedupExecutesIdenticalWritesTwice(t *testing.T) {
	exec := newFakeExecutor()
	app := newDedupApp(exec)
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		r := app.handleToolCall(ctx, dedupTC("write_file", `{"path":"same.txt","content":"identical"}`))
		if !r.ok {
			t.Fatalf("write %d failed: %q", i+1, r.text)
		}
	}
	exec.mu.Lock()
	defer exec.mu.Unlock()
	if got := exec.writeCalls["same.txt"]; got != "identical" {
		t.Fatalf("writeCalls[same.txt] = %q, want identical", got)
	}
}

func TestDedupAllowlistAndStateChangingClassification(t *testing.T) {
	allow := []string{
		"read_file", "read_file_full", "list_dir", "search_files", "find_files",
		"git_status", "git_diff", "git_log", "git_show", "git_blame",
	}
	for _, name := range allow {
		if !dedupToolAllowed(name) {
			t.Errorf("dedupToolAllowed(%q) = false, want true", name)
		}
	}
	reject := []string{
		"run_shell", "run_background", "write_file", "write_binary_file",
		"edit_file", "delete_file", "move_file", "mashura__review",
		"dispatch_subagent", "memory_put", "memory_get", "staging_put",
		"lsp_definition", "browser_navigate", "google_search", "searxng_search",
		"math_eval", "unknown_tool", "trello__get_boards",
	}
	for _, name := range reject {
		if dedupToolAllowed(name) {
			t.Errorf("dedupToolAllowed(%q) = true, want false", name)
		}
	}
	mutating := []string{
		"write_file", "write_binary_file", "edit_file", "delete_file",
		"move_file", "run_shell", "run_background", "trello__create_card",
		"trello__get_boards",
	}
	for _, name := range mutating {
		if !stateChangingTool(name) {
			t.Errorf("stateChangingTool(%q) = false, want true", name)
		}
	}
	nonMutating := []string{
		"read_file", "search_files", "memory_put", "staging_put",
		"mashura__review", "dispatch_subagent", "math_eval",
	}
	for _, name := range nonMutating {
		if stateChangingTool(name) {
			t.Errorf("stateChangingTool(%q) = true, want false", name)
		}
	}
}
