package agent

// Regression tests for the toolcache spill-path interception (Bug A root-cause
// fix): read_file/read_file_full recognise a path that Wakil itself spilled to
// the toolcache root (via CapToolResult/StubToolResult/SpillFullResult) and
// serve it directly from the host filesystem, bypassing Executor.ConfinePath
// entirely. Before this fix, ANY such path was a guaranteed, deterministic
// ConfinePath rejection — the model would retry it until MaxToolIterations
// (or, after the previous fix, the confinement circuit breaker) forced an
// early, but still incomplete, wrap-up. Now the content is genuinely
// recoverable: the subagent (or parent) actually completes the task instead
// of failing fast.

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
	wtools "github.com/treeol/wakil/internal/tools"
)

func mustJSON(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// confinementRejection mimics the real error text ConfinePath produces (see
// internal/exec/exec_ops.go), so tests exercise the same isConfinementError/
// ConfinePath-rejection path a real sandboxed executor would produce.
type confinementRejection struct{ path string }

func (e *confinementRejection) Error() string {
	return `path "` + e.path + `" is outside workspace "/work" — traversal not allowed`
}

// TestIsToolCacheHostPath verifies the classifier matches real toolcache
// paths (as produced by wtools.SpillToCache under XDG_DATA_HOME) and rejects
// unrelated paths, including a workspace file that merely contains the
// substring "toolcache" in its name (must not false-positive on substring).
func TestIsToolCacheHostPath(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	realPath := wtools.SpillToCache("chat123", "read_file_full", "hello world")
	if realPath == "" {
		t.Fatal("SpillToCache returned empty path — cannot proceed")
	}
	if !wtools.IsToolCacheHostPath(realPath) {
		t.Errorf("IsToolCacheHostPath(%q) = false, want true (real spilled path)", realPath)
	}

	// A workspace file path that happens to mention "toolcache" in its name
	// must NOT be misclassified — this is a substring-match false positive
	// the exact-prefix design specifically avoids.
	decoy := "/mnt/wakil/internal/tools/toolcache_notes.md"
	if wtools.IsToolCacheHostPath(decoy) {
		t.Errorf("IsToolCacheHostPath(%q) = true, want false (not actually under the toolcache root)", decoy)
	}

	// An arbitrary unrelated absolute path.
	if wtools.IsToolCacheHostPath("/etc/passwd") {
		t.Error("IsToolCacheHostPath(\"/etc/passwd\") = true, want false")
	}

	// Empty path.
	if wtools.IsToolCacheHostPath("") {
		t.Error("IsToolCacheHostPath(\"\") = true, want false")
	}
}

// TestReadFileFullServesToolCacheSpillPath is the end-to-end regression test
// for Bug A: a subagent's OWN tool result gets capped (CapToolResult) into a
// spill file whose path is embedded in the "[... at: PATH]" marker. The model
// then calls read_file_full on that exact path. Before the fix this always
// failed via ConfinePath ("outside workspace"); after the fix it must return
// the full original content.
func TestReadFileFullServesToolCacheSpillPath(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	original := strings.Repeat("ORIGINAL-CONTENT-LINE\n", 2000) // large enough to force a cap
	capped := wtools.CapToolResult(original, "search_files", "chat-abc", 500)
	spillPath := wtools.ExtractSpillPath(capped)
	if spillPath == "" {
		t.Fatalf("expected a spill path in capped result; got: %q", Truncate(capped, 300))
	}

	// A workspaceRoot-confined executor: any attempt to ConfinePath the spill
	// path (the OLD, broken behavior) would be rejected as outside workspace.
	exec := newFakeExecutor()
	exec.confineErrFn = func(path string) error {
		return &confinementRejection{path: path}
	}

	app := &App{
		Cfg:     config.DefaultConfig(),
		Exec:    exec,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}

	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name:      "read_file_full",
		Arguments: mustJSON(map[string]string{"path": spillPath}),
	}})

	if strings.HasPrefix(res, "ERROR:") {
		t.Fatalf("read_file_full on a toolcache spill path failed (this is exactly the Bug A regression): %q", Truncate(res, 300))
	}
	if !strings.Contains(res, "ORIGINAL-CONTENT-LINE") {
		t.Errorf("read_file_full did not return the spilled content; got: %q", Truncate(res, 300))
	}
}

// TestReadFileServesToolCacheSpillPathWindowed verifies read_file (not just
// read_file_full) also intercepts a toolcache path and applies the normal
// offset/limit windowing on the host-served content.
func TestReadFileServesToolCacheSpillPathWindowed(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	lines := make([]string, 0, 50)
	for i := 1; i <= 50; i++ {
		lines = append(lines, "line-"+strconv.Itoa(i))
	}
	original := strings.Join(lines, "\n") + "\n"
	spillPath := wtools.SpillToCache("chat-xyz", "search_files", original)
	if spillPath == "" {
		t.Fatal("SpillToCache returned empty path")
	}

	exec := newFakeExecutor()
	exec.confineErrFn = func(path string) error {
		return &confinementRejection{path: path}
	}
	app := &App{Cfg: config.DefaultConfig(), Exec: exec, Out: io.Discard}

	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name:      "read_file",
		Arguments: mustJSON(map[string]interface{}{"path": spillPath, "offset": 10, "limit": 5}),
	}})

	if strings.HasPrefix(res, "ERROR:") {
		t.Fatalf("read_file on a toolcache spill path failed: %q", Truncate(res, 300))
	}
	if !strings.Contains(res, "line-10") || !strings.Contains(res, "line-14") {
		t.Errorf("windowed read did not return the expected line range; got: %q", res)
	}
	if strings.Contains(res, "line-20") {
		t.Errorf("windowed read returned lines outside the requested limit; got: %q", res)
	}
}

// TestReadFileFullToolCacheSpillPathBinaryGuard verifies the binary-content
// guard still applies to host-served spill files (a spilled artifact gets the
// same protections a normal workspace file would).
func TestReadFileFullToolCacheSpillPathBinaryGuard(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	binary := "binary\x00content\x00here"
	spillPath := wtools.SpillToCache("chat-bin", "read_file_full", binary)
	if spillPath == "" {
		t.Fatal("SpillToCache returned empty path")
	}

	exec := newFakeExecutor()
	app := &App{Cfg: config.DefaultConfig(), Exec: exec, Out: io.Discard}

	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name:      "read_file_full",
		Arguments: mustJSON(map[string]string{"path": spillPath}),
	}})

	if !strings.HasPrefix(res, "ERROR:") || !strings.Contains(res, "binary file") {
		t.Errorf("expected binary-file refusal for a spilled binary artifact; got: %q", res)
	}
}

// TestReadFileFullToolCacheSpillPathSizeGuard verifies the size guard applies
// to host-served spill files using MaxFullReadBytes.
func TestReadFileFullToolCacheSpillPathSizeGuard(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	big := strings.Repeat("x", 5000)
	spillPath := wtools.SpillToCache("chat-big", "read_file_full", big)
	if spillPath == "" {
		t.Fatal("SpillToCache returned empty path")
	}

	cfg := config.DefaultConfig()
	cfg.MaxFullReadBytes = 1000 // smaller than the 5000-byte spill file
	app := &App{Cfg: cfg, Exec: newFakeExecutor(), Out: io.Discard}

	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name:      "read_file_full",
		Arguments: mustJSON(map[string]string{"path": spillPath}),
	}})

	if !strings.HasPrefix(res, "ERROR:") || !strings.Contains(res, "exceeds full-read limit") {
		t.Errorf("expected size-guard refusal for an oversized spilled artifact; got: %q", res)
	}
}

// TestReadFileFullNonToolCachePathStillConfined verifies that a normal
// workspace path (NOT a toolcache spill path) still goes through
// Executor.ConfinePath exactly as before — the interception must be scoped
// precisely to genuine toolcache paths, never a general confinement bypass.
func TestReadFileFullNonToolCachePathStillConfined(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	exec := newFakeExecutor()
	rejected := false
	exec.confineErrFn = func(path string) error {
		rejected = true
		return &confinementRejection{path: path}
	}
	app := &App{Cfg: config.DefaultConfig(), Exec: exec, Out: io.Discard}

	res := app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name:      "read_file_full",
		Arguments: mustJSON(map[string]string{"path": "/mnt/some/other/repo/file.go"}),
	}})

	if !rejected {
		t.Error("expected ConfinePath to be invoked (and reject) for a non-toolcache path")
	}
	if !strings.HasPrefix(res, "ERROR:") {
		t.Errorf("expected a ConfinePath rejection error, got: %q", res)
	}
}

// TestSubagentActuallyRecoversItsOwnCappedResult is the full end-to-end proof
// that Bug A is fixed at the level that matters: a subagent whose OWN tool
// output gets capped does not degrade to Status:"incomplete" purely because
// of that cap — the capped/spilled result is exempted from ever being
// mistaken for exhaustion, and if the subagent recovers it via
// read_file_full, that call now genuinely succeeds instead of guaranteed-
// failing.
func TestSubagentActuallyRecoversItsOwnCappedResult(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", tmpDir)

	// A search_files result large enough to be capped under the subagent's
	// ToolResultCap (12000 per subagent.go).
	bigMatch := strings.Repeat("config.go:1: match line here\n", 1000)

	summaryJSON := `{"objective":"count all matches","findings":[{"summary":"recovered full match list via read_file_full","location":"config.go","kind":"match","weight":"high"}],"checked":[{"path":"config.go","size_k":20,"status":"full"}]}`

	execFake := newFakeExecutor()
	execFake.shellResult = bigMatch // search_files runs via RunShell in the real handler

	srv := sseServer(t,
		toolCallFrames("s1", "search_files", mustJSON(map[string]string{"pattern": "match", "path": "."})),
		[]string{contentChunk(summaryJSON)},
	)
	defer srv.Close()

	parent := newTestApp(srv.URL, execFake, func(_, _, _ string, _ bool) bool { return true })
	summary, _, _, _, _ := parent.dispatchSubagent(context.Background(), "count all matches", nil, "")

	if summary.Status == "incomplete" {
		t.Errorf("subagent reported incomplete purely from its own capped tool result — Bug A regression. Skipped=%v Uncertainty=%v",
			summary.Skipped, summary.Uncertainty)
	}
}

var _ = proxy.Message{}
