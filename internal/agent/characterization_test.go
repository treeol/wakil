package agent

// Characterization tests for ExecuteToolCall and shared gates.
//
// These tests assert `string` return values from ExecuteToolCall.
// When WP-6.8 changes the return type to `toolResult`, these tests must be
// updated. See implementation-plan.md WP-5.4 for details.

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
)

// TestExecuteToolCall_ReadFile verifies the read_file tool returns file contents.
func TestExecuteToolCall_ReadFile(t *testing.T) {
	fe := newFakeExecutor()
	fe.files["test.txt"] = "line1\nline2\nline3"
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file", Arguments: `{"path":"test.txt"}`,
	}})
	if !strings.Contains(res, "line1") {
		t.Errorf("read_file result missing content: %s", res)
	}
}

// TestExecuteToolCall_ListDir verifies the list_dir tool returns directory entries.
func TestExecuteToolCall_ListDir(t *testing.T) {
	fe := newFakeExecutor()
	fe.files["a.txt"] = "a"
	fe.files["b.go"] = "b"
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "list_dir", Arguments: `{"path":"."}`,
	}})
	if !strings.Contains(res, "a.txt") || !strings.Contains(res, "b.go") {
		t.Errorf("list_dir result missing entries: %s", res)
	}
}

// TestExecuteToolCall_SearchFiles verifies the search_files tool returns matches.
func TestExecuteToolCall_SearchFiles(t *testing.T) {
	fe := newFakeExecutor()
	fe.files["test.txt"] = "hello world\nfoo bar"
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "search_files", Arguments: `{"pattern":"hello","path":"test.txt"}`,
	}})
	if !strings.Contains(res, "hello") {
		t.Errorf("search_files result missing match: %s", res)
	}
}

// TestExecuteToolCall_FindFiles verifies the find_files tool returns found files.
func TestExecuteToolCall_FindFiles(t *testing.T) {
	fe := newFakeExecutor()
	fe.files["main.go"] = "package main"
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "find_files", Arguments: `{"pattern":"*.go"}`,
	}})
	// find_files uses RunShell under the hood; fake executor returns "ran: <cmd>"
	// The important thing is it doesn't error.
	if strings.HasPrefix(res, "ERROR:") {
		t.Errorf("find_files returned error: %s", res)
	}
}

// TestExecuteToolCall_WriteFile verifies the write_file tool writes content.
func TestExecuteToolCall_WriteFile(t *testing.T) {
	fe := newFakeExecutor()
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "write_file", Arguments: `{"path":"out.txt","content":"hello"}`,
	}})
	if strings.HasPrefix(res, "ERROR:") {
		t.Errorf("write_file returned error: %s", res)
	}
	if fe.writeCalls["out.txt"] != "hello" {
		t.Errorf("write_file did not write expected content; got %q", fe.writeCalls["out.txt"])
	}
}

// TestExecuteToolCall_EditFile verifies the edit_file tool modifies content.
func TestExecuteToolCall_EditFile(t *testing.T) {
	fe := newFakeExecutor()
	fe.files["test.txt"] = "old content here"
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "edit_file", Arguments: `{"path":"test.txt","old_string":"old","new_string":"new"}`,
	}})
	if strings.HasPrefix(res, "ERROR:") {
		t.Errorf("edit_file returned error: %s", res)
	}
	if !strings.Contains(fe.files["test.txt"], "new content") {
		t.Errorf("edit_file did not modify content; got %q", fe.files["test.txt"])
	}
}

// TestExecuteToolCall_DeleteFile verifies the delete_file tool works.
func TestExecuteToolCall_DeleteFile(t *testing.T) {
	fe := newFakeExecutor()
	fe.files["doomed.txt"] = "bye"
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "delete_file", Arguments: `{"path":"doomed.txt"}`,
	}})
	if strings.HasPrefix(res, "ERROR:") {
		t.Errorf("delete_file returned error: %s", res)
	}
}

// TestExecuteToolCall_MoveFile verifies the move_file tool works.
func TestExecuteToolCall_MoveFile(t *testing.T) {
	fe := newFakeExecutor()
	fe.files["src.txt"] = "content"
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "move_file", Arguments: `{"src":"src.txt","dst":"dst.txt"}`,
	}})
	if strings.HasPrefix(res, "ERROR:") {
		t.Errorf("move_file returned error: %s", res)
	}
}

// TestExecuteToolCall_DeclinedGate verifies that a declined confirm gate
// returns "[declined by user]".
func TestExecuteToolCall_DeclinedGate(t *testing.T) {
	fe := newFakeExecutor()
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return false })

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "write_file", Arguments: `{"path":"out.txt","content":"hello"}`,
	}})
	if !strings.Contains(res, "declined") {
		t.Errorf("declined write_file should say 'declined': %s", res)
	}
}

// TestExecuteToolCall_InvalidArgs verifies that malformed JSON arguments
// return an ERROR: string.
func TestExecuteToolCall_InvalidArgs(t *testing.T) {
	fe := newFakeExecutor()
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file", Arguments: `{invalid json}`,
	}})
	if !strings.HasPrefix(res, "ERROR:") {
		t.Errorf("invalid args should return ERROR: %s", res)
	}
}

// TestExecuteToolCall_UnknownTool verifies that an unknown tool name
// returns an ERROR: string (or routes to MCP if MCP is configured).
func TestExecuteToolCall_UnknownTool(t *testing.T) {
	fe := newFakeExecutor()
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "nonexistent_tool", Arguments: `{}`,
	}})
	if !strings.HasPrefix(res, "ERROR:") {
		t.Errorf("unknown tool should return ERROR: %s", res)
	}
}

// TestExecuteToolCall_RunShell verifies the run_shell tool returns output.
func TestExecuteToolCall_RunShell(t *testing.T) {
	fe := newFakeExecutor()
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "run_shell", Arguments: `{"command":"echo hello"}`,
	}})
	if !strings.Contains(res, "ran:") {
		t.Errorf("run_shell result unexpected: %s", res)
	}
}

// TestExecuteToolCall_StagingPut verifies the staging_put tool works.
func TestExecuteToolCall_StagingPut(t *testing.T) {
	fe := newFakeExecutor()
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "staging_put", Arguments: `{"key":"test","value":"hello"}`,
	}})
	if strings.HasPrefix(res, "ERROR:") {
		t.Errorf("staging_put returned error: %s", res)
	}
}

// TestExecuteToolCall_MemoryPut verifies the memory_put tool works.
func TestExecuteToolCall_MemoryPut(t *testing.T) {
	fe := newFakeExecutor()
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "memory_put", Arguments: `{"key":"test/char","value":"hello","kind":"note"}`,
	}})
	if strings.HasPrefix(res, "ERROR:") {
		t.Errorf("memory_put returned error: %s", res)
	}
}

// TestExecuteToolCall_ErrorFormat verifies that errors use the "ERROR:" prefix.
// This is the string protocol that WP-6.8 will replace with typed toolResult.
func TestExecuteToolCall_ErrorFormat(t *testing.T) {
	fe := newFakeExecutor()
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	// read_file on a nonexistent file.
	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file", Arguments: `{"path":"nonexistent.xyz"}`,
	}})
	if !strings.HasPrefix(res, "ERROR:") {
		t.Errorf("error result should start with 'ERROR:': %s", res)
	}
}

// TestExecuteToolCall_MissingRequiredArg verifies that missing arguments
// are handled gracefully (no panic). The tool may execute with empty/zero
// values rather than returning an error — this test documents that behavior.
func TestExecuteToolCall_MissingRequiredArg(t *testing.T) {
	fe := newFakeExecutor()
	app := newTestApp("", fe, func(_, _, _ string, _ bool) bool { return true })

	// run_shell without "command" field — the tool runs with an empty command.
	// This documents the current behavior: no schema validation at dispatch time.
	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "run_shell", Arguments: `{}`,
	}})
	// Must not panic and must return a string.
	if res == "" {
		t.Error("missing required arg should still return a non-empty string")
	}
}

// TestExecuteToolCall_ReadProcessLog verifies the read_process_log tool
// works with a fake executor.
func TestExecuteToolCall_ReadProcessLog(t *testing.T) {
	fe := newFakeExecutor()
	app := &App{
		Exec:    fe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     config.DefaultConfig(),
		bgProcs: map[string]*bgEntry{
			"bg1": {id: "bg1", pid: 42, label: "srv", logPath: "/tmp/bg.log", generation: 1},
		},
	}

	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_process_log", Arguments: `{"id":"bg1"}`,
	}})
	// fakeExecutor reports IsProcessAlive=false, so status should be "exited".
	if !strings.Contains(res, "exited") {
		t.Errorf("read_process_log should report 'exited' for dead process: %s", res)
	}
}
