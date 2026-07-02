package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"wakil/internal/config"
	"wakil/internal/exec"
	"wakil/internal/proxy"
	"wakil/internal/tools"
)

func TestFormatFileView(t *testing.T) {
	content := "alpha\nbravo\ncharlie\ndelta\n"

	full := formatFileView(content, 0, 0)
	if !strings.Contains(full, "     1\talpha") || !strings.Contains(full, "     4\tdelta") {
		t.Fatalf("full view missing numbered lines:\n%s", full)
	}
	if strings.Contains(full, "[lines") {
		t.Errorf("full read should not carry a range header:\n%s", full)
	}

	ranged := formatFileView(content, 2, 2)
	if !strings.Contains(ranged, "[lines 2-3 of 4]") {
		t.Errorf("missing/incorrect range header:\n%s", ranged)
	}
	if strings.Contains(ranged, "alpha") || strings.Contains(ranged, "delta") ||
		!strings.Contains(ranged, "bravo") || !strings.Contains(ranged, "charlie") {
		t.Errorf("range slice wrong:\n%s", ranged)
	}

	if got := formatFileView(content, 99, 0); !strings.Contains(got, "past end") {
		t.Errorf("offset past end = %q", got)
	}
	if got := formatFileView("", 0, 0); got != "(empty file)" {
		t.Errorf("empty file = %q", got)
	}
}

func TestReadFileRangedViaHandler(t *testing.T) {
	exec := newFakeExecutor()
	exec.files["a.go"] = "l1\nl2\nl3\nl4\nl5\n"
	app := &App{Exec: exec, Out: io.Discard}

	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file", Arguments: `{"path":"a.go","offset":2,"limit":2}`,
	}})
	if !strings.Contains(res, "[lines 2-3 of 5]") {
		t.Fatalf("ranged read header wrong: %q", res)
	}
	if !strings.Contains(res, "l2") || !strings.Contains(res, "l3") ||
		strings.Contains(res, "l1") || strings.Contains(res, "l4") {
		t.Fatalf("ranged read slice wrong: %q", res)
	}
}

func TestFindFilesBuildsConstrainedCommand(t *testing.T) {
	exec := newFakeExecutor()
	app := &App{Exec: exec, Out: io.Discard}

	app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "find_files", Arguments: `{"pattern":"*.go","path":"src"}`,
	}})
	if len(exec.shellCalls) != 1 {
		t.Fatalf("expected 1 shell call, got %v", exec.shellCalls)
	}
	cmd := exec.shellCalls[0]
	if !strings.Contains(cmd, "find 'src'") || !strings.Contains(cmd, "-name '*.go'") {
		t.Fatalf("find command not constrained as expected: %q", cmd)
	}

	if res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "find_files", Arguments: `{}`,
	}}); !strings.Contains(res, "pattern is required") {
		t.Errorf("missing pattern should error, got %q", res)
	}
}

func TestEditFileReplacesUnique(t *testing.T) {
	exec := newFakeExecutor()
	exec.files["a.go"] = "package main\n\nfunc foo() {}\n"
	app := &App{Exec: exec, Out: io.Discard, Confirm: func(_, _, _ string, _ bool) bool { return true }}

	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "edit_file", Arguments: `{"path":"a.go","old_string":"func foo() {}","new_string":"func bar() {}"}`,
	}})
	if !strings.Contains(res, "edited a.go") {
		t.Fatalf("unexpected result: %q", res)
	}
	if exec.files["a.go"] != "package main\n\nfunc bar() {}\n" {
		t.Fatalf("file not edited correctly: %q", exec.files["a.go"])
	}
}

func TestEditFileAmbiguousAndMissing(t *testing.T) {
	exec := newFakeExecutor()
	exec.files["a.go"] = "x\nx\n"
	app := &App{Exec: exec, Out: io.Discard, Confirm: func(_, _, _ string, _ bool) bool { return true }}

	// Non-unique without replace_all → corrective error, no write.
	if res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "edit_file", Arguments: `{"path":"a.go","old_string":"x","new_string":"y"}`,
	}}); !strings.Contains(res, "appears 2 times") {
		t.Errorf("ambiguous edit should error, got %q", res)
	}
	if exec.files["a.go"] != "x\nx\n" {
		t.Errorf("ambiguous edit must not write: %q", exec.files["a.go"])
	}

	// replace_all rewrites every occurrence.
	if res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "edit_file", Arguments: `{"path":"a.go","old_string":"x","new_string":"y","replace_all":true}`,
	}}); !strings.Contains(res, "2 replacement") {
		t.Errorf("replace_all result = %q", res)
	}
	if exec.files["a.go"] != "y\ny\n" {
		t.Errorf("replace_all wrong: %q", exec.files["a.go"])
	}

	// old_string not present → corrective error.
	if res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "edit_file", Arguments: `{"path":"a.go","old_string":"zzz","new_string":"q"}`,
	}}); !strings.Contains(res, "not found") {
		t.Errorf("missing old_string should error, got %q", res)
	}
}

func TestEditFileDeclined(t *testing.T) {
	exec := newFakeExecutor()
	exec.files["a.go"] = "keep me\n"
	app := &App{Exec: exec, Out: io.Discard, Confirm: func(_, _, _ string, _ bool) bool { return false }}

	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "edit_file", Arguments: `{"path":"a.go","old_string":"keep me","new_string":"changed"}`,
	}})
	if res != "[declined by user]" {
		t.Fatalf("declined edit result = %q", res)
	}
	if exec.files["a.go"] != "keep me\n" {
		t.Fatalf("declined edit must not write: %q", exec.files["a.go"])
	}
}

func TestParentToolsetComplete(t *testing.T) {
	names := map[string]bool{}
	for _, tl := range tools.DefaultTools("/work") {
		names[tl.Function.Name] = true
	}
	for _, want := range []string{
		"read_file", "search_files", "find_files", "list_dir",
		"edit_file", "write_file",
		"delete_file", "move_file",
		"run_background", "kill_process", "read_process_log",
	} {
		if !names[want] {
			t.Errorf("defaultTools missing %q", want)
		}
	}
	for _, gated := range []string{"edit_file", "delete_file", "move_file", "run_background", "kill_process"} {
		if !tools.GatedTool(gated) {
			t.Errorf("%q must require confirmation", gated)
		}
	}
	if tools.GatedTool("read_process_log") {
		t.Error("read_process_log must NOT require confirmation")
	}
}

func TestContextPreambleIncludesTools(t *testing.T) {
	exec := newFakeExecutor()
	app := &App{
		Cfg:        config.DefaultConfig(),
		Exec:       exec,
		InjectDate: true,
	}
	// fakeExecutor.SandboxTools returns ""; preamble should omit the line silently.
	p := app.contextPreamble()
	if !strings.Contains(p, "Working directory:") {
		t.Errorf("preamble missing working directory: %q", p)
	}
	if strings.Contains(p, "Sandbox tools:") {
		t.Errorf("preamble should omit empty sandbox tools line: %q", p)
	}
}

// fakeExecutorWithTools wraps fakeExecutor and returns a canned sandbox tools line.
type fakeExecutorWithTools struct{ *fakeExecutor }

func (f *fakeExecutorWithTools) SandboxTools() string {
	return "Sandbox tools: git 2.39.5, node 20.20.2"
}

func TestContextPreambleWithTools(t *testing.T) {
	exec := &fakeExecutorWithTools{newFakeExecutor()}
	app := &App{
		Cfg:        config.DefaultConfig(),
		Exec:       exec,
		InjectDate: true,
	}
	p := app.contextPreamble()
	if !strings.Contains(p, "Sandbox tools: git 2.39.5") {
		t.Errorf("preamble missing tools line: %q", p)
	}
}

func TestDeleteFileConfinement(t *testing.T) {
	exec := newFakeExecutor()
	// Override ConfinePath to return error on traversal attempt.
	type confiner interface {
		setConfineFn(func(string) (string, error))
	}
	app := &App{
		Exec:    exec,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}

	// fakeExecutor.ConfinePath accepts everything; test via direct call
	// (confinement correctness is tested in exec_ops_test where real paths are used).
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "delete_file", Arguments: `{"path":"/work/file.go"}`,
	}})
	// fakeExecutor.DeletePath returns nil → success
	if !strings.Contains(res, "deleted:") {
		t.Errorf("unexpected delete result: %q", res)
	}
}

func TestMoveFileDeclined(t *testing.T) {
	exec := newFakeExecutor()
	app := &App{
		Exec:    exec,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return false },
	}
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "move_file", Arguments: `{"src":"/work/a.go","dst":"/work/b.go"}`,
	}})
	if res != "[declined by user]" {
		t.Errorf("declined move result = %q", res)
	}
}

func TestBgCapEnforced(t *testing.T) {
	exec := newFakeExecutor()
	app := &App{
		Exec:    exec,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     config.DefaultConfig(),
	}
	// IsProcessAlive returns false on fakeExecutor, so "live" count stays 0.
	// Override to simulate live processes.
	type aliver interface{ setAlive(bool) }

	// Run 5 successful starts to fill the cap.
	for i := 0; i < 5; i++ {
		app.bgCounter++
		if app.bgProcs == nil {
			app.bgProcs = make(map[string]*bgEntry)
		}
		id := "bg" + strings.Repeat("x", i) // unique IDs
		app.bgProcs[id] = &bgEntry{id: id, pid: 100 + i, generation: 1}
	}

	// Now patch IsProcessAlive to report them alive by using a custom executor.
	type aliveExec struct{ *fakeExecutor }
	aliveExec2 := &struct {
		*fakeExecutor
	}{exec}
	_ = aliveExec2

	// We can't easily override IsProcessAlive on fakeExecutor without a new type,
	// so instead directly verify the cap check logic: inject 5 entries with
	// generation==exec.Generation() and count them live via a custom executor.
	type aliveExecutor struct{ *fakeExecutor }
	type aliveExecutorT = struct{ *fakeExecutor }

	// Simplest approach: verify the 6th call returns the cap error when there are
	// already 5 live entries. Since fakeExecutor.IsProcessAlive returns false,
	// we manually count: replace all entries' generation to 0 (old) so count stays 0.
	// Instead just test that max-live detection blocks the 6th.
	// Here we test via the "live counter" path with a wrapped executor.
	app2 := &App{
		Exec:    &aliveExecutorImpl{fakeExecutor: exec},
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     config.DefaultConfig(),
		bgProcs: map[string]*bgEntry{
			"bg1": {pid: 101, generation: 1},
			"bg2": {pid: 102, generation: 1},
			"bg3": {pid: 103, generation: 1},
			"bg4": {pid: 104, generation: 1},
			"bg5": {pid: 105, generation: 1},
		},
		bgCounter: 5,
	}
	res := app2.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "run_background", Arguments: `{"command":"sleep 99","label":"test"}`,
	}})
	if !strings.Contains(res, "maximum of 5") {
		t.Errorf("6th background process should be rejected: %q", res)
	}
}

// aliveExecutorImpl wraps fakeExecutor but reports all processes alive.
type aliveExecutorImpl struct{ *fakeExecutor }

func (a *aliveExecutorImpl) IsProcessAlive(_ context.Context, pid int) bool { return true }

// selectiveAliveExec reports alive only for PIDs in the set.
type selectiveAliveExec struct {
	*fakeExecutor
	alivePids map[int]bool
}

func (s *selectiveAliveExec) IsProcessAlive(_ context.Context, pid int) bool {
	return s.alivePids[pid]
}

// logTailExec returns a fixed string from ReadFileTail, simulating a log file.
type logTailExec struct {
	*fakeExecutor
	logContent string
}

func (e *logTailExec) ReadFileTail(_ context.Context, _ string, maxBytes int64) (string, error) {
	c := e.logContent
	if int64(len(c)) > maxBytes {
		c = c[int64(len(c))-maxBytes:]
	}
	return c, nil
}
func (e *logTailExec) IsProcessAlive(_ context.Context, _ int) bool { return false }

func TestGenerationStaleness(t *testing.T) {
	exec := newFakeExecutor()
	app := &App{
		Exec: exec,
		Out:  io.Discard,
		bgProcs: map[string]*bgEntry{
			"bg1": {id: "bg1", pid: 999, generation: 0}, // old generation
		},
	}
	// read_process_log on stale entry should report "process lost"
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_process_log", Arguments: `{"id":"bg1"}`,
	}})
	if !strings.Contains(res, "process lost") {
		t.Errorf("stale entry should say 'process lost': %q", res)
	}
	// kill_process on stale entry should also report "process lost"
	app.bgProcs["bg1"] = &bgEntry{id: "bg1", pid: 999, generation: 0}
	app2 := &App{
		Exec:    exec,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		bgProcs: map[string]*bgEntry{
			"bg1": {id: "bg1", pid: 999, generation: 0},
		},
	}
	res2 := app2.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "kill_process", Arguments: `{"id":"bg1"}`,
	}})
	if !strings.Contains(res2, "process lost") {
		t.Errorf("stale kill should say 'process lost': %q", res2)
	}
}

// ── Item 1: read_process_log 8 KB hard cap ────────────────────────────────────

// TestReadProcessLogCapEnforced verifies:
//   - total returned payload never exceeds 8 KB + 256-byte header overhead
//   - the status-line prefix is present
//   - content is taken from the tail of the log, not the head
func TestReadProcessLogCapEnforced(t *testing.T) {
	const logHead = "LOGHEAD_MARKER"
	const logTail = "LOGTAIL_MARKER"
	// Build a log that's clearly >8 KB: ~2 KB head junk + ~9 KB tail content.
	// ReadFileTail(8192) must return bytes from the tail section only.
	head := strings.Repeat(logHead, 150) // 150*14 = 2100 bytes
	tail := strings.Repeat(logTail, 700) // 700*14 = 9800 bytes
	logContent := head + tail            // 11900 bytes total

	exe := &logTailExec{fakeExecutor: newFakeExecutor(), logContent: logContent}
	app := &App{
		Exec: exe,
		Out:  io.Discard,
		bgProcs: map[string]*bgEntry{
			"bg1": {id: "bg1", pid: 42, label: "srv", logPath: "/tmp/bg.log", generation: 1},
		},
	}

	result := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_process_log", Arguments: `{"id":"bg1"}`,
	}})

	const maxTotal = 8*1024 + 256
	if len(result) > maxTotal {
		t.Errorf("result len=%d exceeds hard cap %d", len(result), maxTotal)
	}
	// Status-line prefix must be present.
	wantPrefix := "[bg1 srv] exited pid=42\n"
	if !strings.HasPrefix(result, wantPrefix) {
		t.Errorf("missing status-line prefix; got prefix %q", result[:min(len(wantPrefix)+10, len(result))])
	}
	// Tail content must be present (ReadFileTail returns last N bytes).
	if !strings.Contains(result, logTail) {
		t.Error("result must contain tail content")
	}
	// Head content must NOT be present — we read from the end, not the start.
	if strings.Contains(result, logHead) {
		t.Error("result must NOT contain head content — ReadFileTail should return the tail")
	}
}

// ── Item 2: SIGKILL escalation ────────────────────────────────────────────────

// TestSIGKILLEscalation starts a SIGTERM-immune process via DirectExecutor,
// calls kill_process, and asserts that SIGKILL was used after ~5 s.
func TestSIGKILLEscalation(t *testing.T) {
	if testing.Short() {
		t.Skip("~6 s SIGKILL escalation test; run without -short")
	}
	tmp := t.TempDir()
	exe, err := exec.NewDirectExecutor(tmp)
	if err != nil {
		t.Fatal(err)
	}
	defer exe.Close()

	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     config.Config{ExecMode: "direct"},
	}

	// "trap '' TERM; sleep 300" sets SIG_IGN before exec-ing sleep, so the whole
	// process group ignores SIGTERM. We sleep 100 ms after starting to let the
	// shell install the trap before we send SIGTERM — without this there is a
	// race between process start and trap installation.
	startRes := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name:      "run_background",
		Arguments: `{"command":"trap '' TERM; sleep 300","label":"sigterm-immune"}`,
	}})
	time.Sleep(100 * time.Millisecond)
	if strings.HasPrefix(startRes, "ERROR:") {
		t.Fatalf("run_background failed: %s", startRes)
	}
	bgID := ""
	for _, line := range strings.Split(startRes, "\n") {
		if strings.HasPrefix(line, "id: ") {
			bgID = strings.TrimPrefix(line, "id: ")
			break
		}
	}
	if bgID == "" {
		t.Fatalf("could not parse bg id from: %s", startRes)
	}

	start := time.Now()
	killRes := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name:      "kill_process",
		Arguments: fmt.Sprintf(`{"id":%q}`, bgID),
	}})
	elapsed := time.Since(start)

	t.Logf("kill_process result: %q  elapsed: %s", killRes, elapsed)

	if !strings.Contains(killRes, "SIGKILL") {
		t.Errorf("expected SIGKILL escalation in result, got: %q", killRes)
	}
	// Should have waited ~5s for TERM; 4-8 s is a safe window.
	if elapsed < 4*time.Second || elapsed > 8*time.Second {
		t.Errorf("expected ~5 s TERM→KILL escalation, took %s", elapsed)
	}
	// bgProcs entry must be cleaned up after kill.
	if _, ok := app.bgProcs[bgID]; ok {
		t.Error("bgProcs entry must be deleted after kill_process")
	}
	// Verify the process group is truly gone via the executor.
	pid := 0
	fmt.Sscanf(startRes, "id: %*s\npid: %d", &pid)
	if pid > 0 && exe.IsProcessAlive(context.Background(), pid) {
		t.Errorf("process pid=%d still alive after SIGKILL", pid)
	}
}

// ── Item 3: dead entries must not count toward the 5-process cap ─────────────

// TestBgCapDeadEntriesDontCount: 5 entries in map, 2 exited, 6th run_background
// should succeed because only 3 are alive.
func TestBgCapDeadEntriesDontCount(t *testing.T) {
	exe := &selectiveAliveExec{
		fakeExecutor: newFakeExecutor(),
		// pids 101-103 alive; 104-105 dead (not in map → IsProcessAlive = false)
		alivePids: map[int]bool{101: true, 102: true, 103: true},
	}
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     config.DefaultConfig(),
		bgProcs: map[string]*bgEntry{
			"bg1": {pid: 101, generation: 1},
			"bg2": {pid: 102, generation: 1},
			"bg3": {pid: 103, generation: 1},
			"bg4": {pid: 104, generation: 1}, // dead
			"bg5": {pid: 105, generation: 1}, // dead
		},
		bgCounter: 5,
	}

	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "run_background", Arguments: `{"command":"sleep 1","label":"test"}`,
	}})
	// Live count == 3 < 5, so the 6th call must succeed.
	if strings.Contains(res, "maximum of 5") {
		t.Errorf("dead entries must not count toward cap; 6th process should succeed: %s", res)
	}
	if !strings.Contains(res, "id:") {
		t.Errorf("expected successful run_background result, got: %s", res)
	}
}

// Verify os and fmt imports used by SIGKILL test.
var _ = os.TempDir
var _ = fmt.Sprintf

// ── P42: read_file size and binary guards ─────────────────────────────────────

func TestReadFileSizeGuardRefuses(t *testing.T) {
	exe := newFakeExecutor()
	// 2 MB of text — over the 1 MB default limit.
	exe.files["big.txt"] = strings.Repeat("x", 2<<20)

	app := &App{
		Cfg:  config.Config{ReadFileSizeLimit: 1 << 20},
		Exec: exe,
		Out:  io.Discard,
	}
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file", Arguments: `{"path":"big.txt"}`,
	}})
	if !strings.HasPrefix(res, "ERROR:") || !strings.Contains(res, "exceeds read limit") {
		t.Fatalf("expected size-guard error, got: %q", res)
	}
}

func TestReadFileSizeGuardAllowsSmall(t *testing.T) {
	exe := newFakeExecutor()
	exe.files["small.txt"] = "hello\nworld\n"

	app := &App{
		Cfg:  config.Config{ReadFileSizeLimit: 1 << 20},
		Exec: exe,
		Out:  io.Discard,
	}
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file", Arguments: `{"path":"small.txt"}`,
	}})
	if strings.HasPrefix(res, "ERROR:") {
		t.Fatalf("small file should not be refused; got: %q", res)
	}
	if !strings.Contains(res, "hello") {
		t.Fatalf("expected file content in result, got: %q", res)
	}
}

func TestReadFileSizeGuardAllowsRangedReadOnLargeFile(t *testing.T) {
	// A "large" file (over limit) must be readable when the model supplies a Limit.
	// The guard's error message says "specify a line/byte range" — that advice must
	// actually work, or the refusal is a dead end.
	exe := newFakeExecutor()
	exe.files["big.txt"] = strings.Repeat("line\n", 300_000) // well over 1 MB

	app := &App{
		Cfg:  config.Config{ReadFileSizeLimit: 1 << 20},
		Exec: exe,
		Out:  io.Discard,
	}
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file", Arguments: `{"path":"big.txt","offset":1,"limit":5}`,
	}})
	if strings.Contains(res, "exceeds read limit") {
		t.Fatalf("ranged read on a large file must not be blocked by size guard; got: %q", res)
	}
	if !strings.Contains(res, "line") {
		t.Fatalf("expected file content in ranged result, got: %q", res)
	}
}

func TestReadFileBinaryGuardRefuses(t *testing.T) {
	exe := newFakeExecutor()
	// Small file but binary (contains null byte).
	exe.files["a.bin"] = "ELF\x00binary content here"

	app := &App{
		Cfg:  config.Config{ReadFileSizeLimit: 1 << 20},
		Exec: exe,
		Out:  io.Discard,
	}
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file", Arguments: `{"path":"a.bin"}`,
	}})
	if !strings.HasPrefix(res, "ERROR:") || !strings.Contains(res, "binary file") {
		t.Fatalf("expected binary-guard error, got: %q", res)
	}
}

func TestReadFileSizeGuardMissingFileProceedsToReadFile(t *testing.T) {
	// When StatFile returns an error (file not found), the guard must not block —
	// ReadFile is called and returns the actual "no such file" error.
	exe := newFakeExecutor()

	app := &App{
		Cfg:  config.Config{ReadFileSizeLimit: 1 << 20},
		Exec: exe,
		Out:  io.Discard,
	}
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file", Arguments: `{"path":"nonexistent.txt"}`,
	}})
	if !strings.HasPrefix(res, "ERROR:") {
		t.Fatalf("missing file should still return an error, got: %q", res)
	}
	// Must NOT say "exceeds read limit" — that would mean the size guard fired
	// on a missing file, which it must not.
	if strings.Contains(res, "exceeds read limit") {
		t.Fatalf("size guard must not fire on missing file: %q", res)
	}
}

// ── read_file_full: full read with size, binary, and cap-exemption guards ──────

func TestReadFileFullReturnsCompleteContent(t *testing.T) {
	exe := newFakeExecutor()
	exe.files["small.go"] = "package main\n\nfunc main() {}\n"

	app := &App{
		Cfg:  config.Config{MaxFullReadBytes: 256 << 10},
		Exec: exe,
		Out:  io.Discard,
	}
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file_full", Arguments: `{"path":"small.go"}`,
	}})
	if strings.HasPrefix(res, "ERROR:") {
		t.Fatalf("small file should not be refused; got: %q", res)
	}
	// Must contain ALL lines — no [lines X-Y of Z] window header.
	if strings.Contains(res, "[lines ") {
		t.Fatalf("read_file_full must not window; got: %q", res)
	}
	if !strings.Contains(res, "package main") || !strings.Contains(res, "func main") {
		t.Fatalf("expected full file content, got: %q", res)
	}
}

func TestReadFileFullRefusesOversized(t *testing.T) {
	exe := newFakeExecutor()
	// 512 KB — over the 256 KB default ceiling.
	exe.files["big.txt"] = strings.Repeat("x", 512<<10)

	app := &App{
		Cfg:  config.Config{MaxFullReadBytes: 256 << 10},
		Exec: exe,
		Out:  io.Discard,
	}
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file_full", Arguments: `{"path":"big.txt"}`,
	}})
	if !strings.HasPrefix(res, "ERROR:") || !strings.Contains(res, "exceeds full-read limit") {
		t.Fatalf("expected full-read size-guard error, got: %q", res)
	}
	// Must suggest read_file with offset/limit.
	if !strings.Contains(res, "read_file") {
		t.Fatalf("error must suggest read_file with offset/limit, got: %q", res)
	}
}

func TestReadFileFullBinaryGuardRefuses(t *testing.T) {
	exe := newFakeExecutor()
	// Small file but binary (contains null byte).
	exe.files["a.bin"] = "ELF\x00binary content here"

	app := &App{
		Cfg:  config.Config{MaxFullReadBytes: 256 << 10},
		Exec: exe,
		Out:  io.Discard,
	}
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file_full", Arguments: `{"path":"a.bin"}`,
	}})
	if !strings.HasPrefix(res, "ERROR:") || !strings.Contains(res, "binary file") {
		t.Fatalf("expected binary-guard error, got: %q", res)
	}
}

func TestReadFileFullMissingFileProceedsToReadFile(t *testing.T) {
	// When StatFile returns an error (file not found), the pre-read size guard
	// must not block — ReadFile is called and returns the actual "no such file"
	// error.
	exe := newFakeExecutor()

	app := &App{
		Cfg:  config.Config{MaxFullReadBytes: 256 << 10},
		Exec: exe,
		Out:  io.Discard,
	}
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file_full", Arguments: `{"path":"nonexistent.txt"}`,
	}})
	if !strings.HasPrefix(res, "ERROR:") {
		t.Fatalf("missing file should still return an error, got: %q", res)
	}
	if strings.Contains(res, "exceeds full-read limit") {
		t.Fatalf("size guard must not fire on missing file: %q", res)
	}
}

// TestReadFileFullBoundarySize verifies that a file exactly at the ceiling
// passes (guard is strict-greater-than), and one byte over refuses.
func TestReadFileFullBoundarySize(t *testing.T) {
	exe := newFakeExecutor()
	limit := 256 << 10
	exe.files["exact.txt"] = strings.Repeat("x", limit)
	exe.files["over.txt"] = strings.Repeat("x", limit+1)

	app := &App{
		Cfg:  config.Config{MaxFullReadBytes: limit},
		Exec: exe,
		Out:  io.Discard,
	}
	// Exactly at ceiling — must pass.
	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file_full", Arguments: `{"path":"exact.txt"}`,
	}})
	if strings.HasPrefix(res, "ERROR:") {
		t.Fatalf("file at exact ceiling must not be refused: %q", res)
	}
	// One byte over — must refuse.
	res = app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file_full", Arguments: `{"path":"over.txt"}`,
	}})
	if !strings.Contains(res, "exceeds full-read limit") {
		t.Fatalf("file one byte over ceiling must be refused: %q", res)
	}
}

// TestReadFileFullCapOrStubExemption verifies that CapOrStub does NOT cap a
// read_file_full result to ToolResultCap — the full content is preserved.
func TestReadFileFullCapOrStubExemption(t *testing.T) {
	// Create content larger than ToolResultCap (8K) but under MaxFullReadBytes.
	content := strings.Repeat("line of content here\n", 500) // ~10 KB
	exe := newFakeExecutor()
	exe.files["medium.go"] = content

	app := &App{
		Cfg:  config.Config{MaxFullReadBytes: 256 << 10, ToolResultCap: 8000},
		Exec: exe,
		Out:  io.Discard,
	}
	// Call ExecuteToolCall (not handleToolCall) so CapOrStub is applied.
	res := app.ExecuteToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: "read_file_full", Arguments: `{"path":"medium.go"}`,
	}})
	// Must not be capped — no "chars omitted" marker from CapToolResult.
	if strings.Contains(res, "chars omitted") {
		t.Fatalf("read_file_full result must not be capped by ToolResultCap: got %q (len=%d)", res[:100], len(res))
	}
	// Must contain the full content (or at least a substantial portion).
	if len(res) < len(content)/2 {
		t.Fatalf("read_file_full result too short: %d vs content %d — likely capped", len(res), len(content))
	}
}

// TestReadFileFullBudgetExhaustionStubs verifies that when the per-turn tool
// budget is exhausted, read_file_full IS stubbed (to a pointer) — it is NOT
// exempt from TurnToolBudget. This is intentional: a 256KB read that arrives
// after the budget is blown would flood context, so it gets a recoverable
// stub pointer instead. The stub still spills to disk, so content is
// recoverable via the embedded path.
func TestReadFileFullBudgetExhaustionStubs(t *testing.T) {
	content := strings.Repeat("line of content here\n", 500) // ~10 KB
	exe := newFakeExecutor()
	exe.files["medium.go"] = content

	app := &App{
		Cfg:  config.Config{MaxFullReadBytes: 256 << 10, ToolResultCap: 8000, TurnToolBudget: 1000},
		Exec: exe,
		Out:  io.Discard,
	}
	// turnToolBytes=2000 >= TurnToolBudget=1000 → budget exhausted.
	res := app.CapOrStub(content, "read_file_full", 2000)
	// Must be stubbed (budget exhausted), not full content.
	if strings.Contains(res, "line of content here") {
		t.Fatalf("read_file_full must be stubbed when budget exhausted, got full content (len=%d)", len(res))
	}
	// Must be a budget stub with a recoverable pointer.
	if !strings.HasPrefix(res, "[budget —") {
		t.Fatalf("expected budget stub, got: %q", res[:min(80, len(res))])
	}
}

// TestReadFileFullNotGated verifies read_file_full is not a gated tool (no
// confirmation needed — it's read-only, same as read_file).
func TestReadFileFullNotGated(t *testing.T) {
	if tools.GatedTool("read_file_full") {
		t.Fatal("read_file_full must not be gated (read-only tool)")
	}
}

// TestReadFileFullInDefaultTools verifies read_file_full is in the default
// tool set and has the right name.
func TestReadFileFullInDefaultTools(t *testing.T) {
	names := map[string]bool{}
	for _, tl := range tools.DefaultTools("/work") {
		names[tl.Function.Name] = true
	}
	if !names["read_file_full"] {
		t.Fatal("read_file_full missing from DefaultTools")
	}
}

// TestReadFileFullInDiscoveryTools verifies read_file_full IS in the
// subagent toolset. It is a read-only tool, same class as the existing four
// (read_file, search_files, find_files, list_dir). The lean principle is
// capability minimization (no run_shell for security, no dispatch_subagent
// for recursion depth) — it is NOT about making the subagent inefficient at
// reading files, which is its core job. read_file_full reduces tool-call
// count and kills the windowed re-read churn that subagents were doing.
func TestReadFileFullInDiscoveryTools(t *testing.T) {
	found := false
	for _, tl := range tools.DiscoveryTools("/work") {
		if tl.Function.Name == "read_file_full" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("read_file_full must be in DiscoveryTools (subagent read efficiency)")
	}
}

// TestSpillFullResultRoundTrip verifies the full round-trip: SpillFullResult
// embeds a path that ExtractSpillPath can recover. This is the critical
// recovery chain — if it breaks, eviction and pre-send trim produce
// non-recoverable stubs.
func TestSpillFullResultRoundTrip(t *testing.T) {
	// Content large enough to trigger the spill (> 200 bytes).
	content := strings.Repeat("package main\n", 50) // ~650 bytes
	result := tools.SpillFullResult(content, "read_file_full", "test-chat")

	// If the spill path was embedded, ExtractSpillPath must find it.
	path := tools.ExtractSpillPath(result)
	if path == "" {
		// Spill may be skipped if cache dir is unavailable (e.g. no XDG_DATA_HOME
		// in CI). In that case the result is returned without a marker — verify
		// that at least the full content is intact.
		if !strings.Contains(result, "package main") {
			t.Fatalf("result missing original content: %q", result[:min(100, len(result))])
		}
		t.Skip("spill path not embedded (cache dir unavailable in this env) — content intact, skipping path extraction test")
	}

	// The extracted path must point to a real file on disk.
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("ExtractSpillPath returned %q but file does not exist: %v", path, err)
	}

	// The spill file must contain the original content.
	spilled, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read spill file %q: %v", path, err)
	}
	if !strings.Contains(string(spilled), content) {
		t.Fatalf("spill file does not contain original content")
	}

	// The original content must be in the result (not truncated).
	if !strings.Contains(result, "package main") {
		t.Fatalf("result missing original content: %q", result[:min(100, len(result))])
	}

	// The marker must be at the end.
	if !strings.HasSuffix(result, "]") {
		t.Fatalf("result must end with ']' (marker), got: %q", result[len(result)-min(50, len(result)):])
	}
}

// TestSpillFullResultMarkerCollision verifies that file content containing
// " at: " does NOT break path extraction. The marker is always appended last,
// so LastIndex must find the real spill path, not a false positive from the
// file body.
func TestSpillFullResultMarkerCollision(t *testing.T) {
	// Content that contains " at: " sequences (plausible in source code, logs).
	content := `// See the config at: /etc/wakil/config.json]
// Also check data at: /var/lib/wakil/data]
` + strings.Repeat("line\n", 50) // ensure > 200 bytes

	result := tools.SpillFullResult(content, "read_file_full", "test-chat")
	path := tools.ExtractSpillPath(result)

	if path == "" {
		t.Skip("spill path not embedded (cache dir unavailable in this env)")
	}

	// The extracted path must NOT be one of the fake paths from the file content.
	if path == "/etc/wakil/config.json" || path == "/var/lib/wakil/data" {
		t.Fatalf("ExtractSpillPath picked up a fake path from file content instead of the real spill path: %q", path)
	}

	// The extracted path must point to a real file (the spill file).
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("extracted path %q does not point to a real file: %v", path, err)
	}

	// The spill file must contain the original content (with the fake paths).
	spilled, _ := os.ReadFile(path)
	if !strings.Contains(string(spilled), "/etc/wakil/config.json") {
		t.Fatalf("spill file should contain original content with 'at:' lines")
	}
}

// TestExtractSpillPathNoFalsePositiveOnFileContent verifies that when a tool
// result contains " at: /path]" in its body (but was NOT spilled — no real
// marker appended), ExtractSpillPath returns "" rather than a bogus path.
// This is the critical safety property: the extraction must only match markers
// that Wakil actually emitted, not arbitrary text in file content.
func TestExtractSpillPathNoFalsePositiveOnFileContent(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{
			"generic at: in body",
			"// See config at: /etc/wakil/config.json]\n" + strings.Repeat("line\n", 50),
		},
		{
			"full marker prefix in body (not at end)",
			"// full content at: /fake/path]\n" + strings.Repeat("line\n", 50),
		},
		{
			"full marker prefix at end but not in bracketed segment",
			"some text full content at: /fake/path]\nmore text after",
		},
		{
			"budget marker prefix in body",
			"[budget — 5000 chars at: /fake/path]\n" + strings.Repeat("line\n", 50),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tools.ExtractSpillPath(tc.content)
			if path != "" {
				t.Fatalf("ExtractSpillPath must return empty for unspilled content, got: %q (content ends with: %q)", path, tc.content[min(0, len(tc.content))-min(50, len(tc.content)):])
			}
		})
	}
}

// TestExtractSpillPathMatchesRealMarkers verifies that all the real marker
// formats emitted by CapToolResult, StubToolResult, SpillFullResult, and
// MakeEvictionStub are correctly extracted.
func TestExtractSpillPathMatchesRealMarkers(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{
			"SpillFullResult",
			"file content here\n[full content at: /cache/spill-1.txt]",
			"/cache/spill-1.txt",
		},
		{
			"CapToolResult",
			"first 8000 chars…\n… [+N chars omitted — full content at: /cache/spill-2.txt]",
			"/cache/spill-2.txt",
		},
		{
			"StubToolResult",
			"[budget — 5000 chars at: /cache/spill-3.txt]",
			"/cache/spill-3.txt",
		},
		{
			"MakeEvictionStub",
			"[evicted — 5000 chars — full content at: /cache/spill-4.txt]",
			"/cache/spill-4.txt",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tools.ExtractSpillPath(tc.content)
			if got != tc.want {
				t.Fatalf("ExtractSpillPath(%q) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// TestSpillFullResultUnavailableNoFalsePositive verifies that when the spill
// cache is unavailable (empty chatID → empty cache dir), SpillFullResult
// returns content WITHOUT a marker, and ExtractSpillPath returns "" even if
// the file content contains " at: /fake/path]".
func TestSpillFullResultUnavailableNoFalsePositive(t *testing.T) {
	// Content with " at: " in the body, large enough to trigger spill.
	content := `// See config at: /etc/wakil/config.json]
` + strings.Repeat("line\n", 50)

	// Empty chatID → toolCacheDir returns "" → spillToDisk returns "" → no marker.
	result := tools.SpillFullResult(content, "read_file_full", "")

	// Must NOT have a spill marker (spill was unavailable).
	if strings.Contains(result, "full content at: ") {
		// If a marker was somehow added, the path must be real (not a false positive).
		path := tools.ExtractSpillPath(result)
		if path == "/etc/wakil/config.json" {
			t.Fatalf("bogus path from file body extracted as spill path!")
		}
	}

	// ExtractSpillPath must NOT return the fake path from the file body.
	path := tools.ExtractSpillPath(result)
	if path == "/etc/wakil/config.json" {
		t.Fatalf("ExtractSpillPath returned a bogus path from file body: %q", path)
	}
}
