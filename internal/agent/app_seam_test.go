package agent

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/proxy"
	"github.com/treeol/wakil/internal/trace"
)

// ─── handleEditFile ──────────────────────────────────────────────────────────

func TestHandleEditFile_BadJSON(t *testing.T) {
	app := &App{
		Exec:    newFakeExecutor(),
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "edit_file", Arguments: `{bad json`}}
	got := app.handleEditFile(context.Background(), tc)
	if !strings.Contains(got, "ERROR: could not parse arguments") {
		t.Errorf("expected parse error, got: %s", got)
	}
}

func TestHandleEditFile_MissingPath(t *testing.T) {
	app := &App{
		Exec:    newFakeExecutor(),
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "edit_file", Arguments: `{"old_string":"x","new_string":"y"}`}}
	got := app.handleEditFile(context.Background(), tc)
	if !strings.Contains(got, "ERROR: path and old_string are required") {
		t.Errorf("expected missing-path error, got: %s", got)
	}
}

func TestHandleEditFile_MissingOldString(t *testing.T) {
	app := &App{
		Exec:    newFakeExecutor(),
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "edit_file", Arguments: `{"path":"a.go"}`}}
	got := app.handleEditFile(context.Background(), tc)
	if !strings.Contains(got, "ERROR: path and old_string are required") {
		t.Errorf("expected missing-old_string error, got: %s", got)
	}
}

func TestHandleEditFile_IdenticalStrings(t *testing.T) {
	app := &App{
		Exec:    newFakeExecutor(),
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "edit_file", Arguments: `{"path":"a.go","old_string":"same","new_string":"same"}`}}
	got := app.handleEditFile(context.Background(), tc)
	if !strings.Contains(got, "nothing to change") {
		t.Errorf("expected identical-strings error, got: %s", got)
	}
}

func TestHandleEditFile_ConfinePathRejected(t *testing.T) {
	exe := newFakeExecutor()
	exe.confineErrFn = func(path string) error {
		return errors.New("path is outside workspace")
	}
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "edit_file", Arguments: `{"path":"../escape.go","old_string":"x","new_string":"y"}`}}
	got := app.handleEditFile(context.Background(), tc)
	if !strings.Contains(got, "ERROR:") || !strings.Contains(got, "outside workspace") {
		t.Errorf("expected ConfinePath error, got: %s", got)
	}
}

func TestHandleEditFile_ReadFileError(t *testing.T) {
	app := &App{
		Exec:    newFakeExecutor(), // no files registered → ReadFile fails
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "edit_file", Arguments: `{"path":"missing.go","old_string":"x","new_string":"y"}`}}
	got := app.handleEditFile(context.Background(), tc)
	if !strings.Contains(got, "ERROR: could not read") {
		t.Errorf("expected read error, got: %s", got)
	}
}

func TestHandleEditFile_OldStringNotFound(t *testing.T) {
	exe := newFakeExecutor()
	exe.files["a.go"] = "package main\n"
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "edit_file", Arguments: `{"path":"a.go","old_string":"nonexistent","new_string":"y"}`}}
	got := app.handleEditFile(context.Background(), tc)
	if !strings.Contains(got, "old_string not found") {
		t.Errorf("expected not-found error, got: %s", got)
	}
}

func TestHandleEditFile_MultipleMatchesNoReplaceAll(t *testing.T) {
	exe := newFakeExecutor()
	exe.files["a.go"] = "foo\nfoo\nfoo\n"
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "edit_file", Arguments: `{"path":"a.go","old_string":"foo","new_string":"bar"}`}}
	got := app.handleEditFile(context.Background(), tc)
	if !strings.Contains(got, "appears 3 times") || !strings.Contains(got, "replace_all") {
		t.Errorf("expected multi-match error, got: %s", got)
	}
}

func TestHandleEditFile_SuccessSingleReplacement(t *testing.T) {
	exe := newFakeExecutor()
	exe.files["a.go"] = "package main\n"
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "edit_file", Arguments: `{"path":"a.go","old_string":"package main","new_string":"package main // edited"}`}}
	got := app.handleEditFile(context.Background(), tc)
	if !strings.Contains(got, "edited a.go") || !strings.Contains(got, "1 replacement") {
		t.Errorf("expected success with 1 replacement, got: %s", got)
	}
	if exe.writeCalls["a.go"] != "package main // edited\n" {
		t.Errorf("file not correctly written, got: %q", exe.writeCalls["a.go"])
	}
}

func TestHandleEditFile_SuccessReplaceAll(t *testing.T) {
	exe := newFakeExecutor()
	exe.files["a.go"] = "foo\nfoo\nfoo\n"
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "edit_file", Arguments: `{"path":"a.go","old_string":"foo","new_string":"bar","replace_all":true}`}}
	got := app.handleEditFile(context.Background(), tc)
	if !strings.Contains(got, "3 replacement") {
		t.Errorf("expected 3 replacements, got: %s", got)
	}
	if exe.writeCalls["a.go"] != "bar\nbar\nbar\n" {
		t.Errorf("file not correctly written, got: %q", exe.writeCalls["a.go"])
	}
}

func TestHandleEditFile_DeclinedByUser(t *testing.T) {
	exe := newFakeExecutor()
	exe.files["a.go"] = "package main\n"
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return false }, // decline
	}
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "edit_file", Arguments: `{"path":"a.go","old_string":"package main","new_string":"package main // edited"}`}}
	got := app.handleEditFile(context.Background(), tc)
	if got != "[declined by user]" {
		t.Errorf("expected [declined by user], got: %s", got)
	}
	if _, written := exe.writeCalls["a.go"]; written {
		t.Error("file should NOT be written when declined")
	}
}

func TestHandleEditFile_WriteFileError(t *testing.T) {
	exe := &errWriteExecutor{newFakeExecutor()}
	exe.files["a.go"] = "package main\n"
	app := &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
	}
	tc := proxy.ToolCall{Function: proxy.FunctionCall{Name: "edit_file", Arguments: `{"path":"a.go","old_string":"package main","new_string":"edited"}`}}
	got := app.handleEditFile(context.Background(), tc)
	if !strings.Contains(got, "ERROR") {
		t.Errorf("expected write error, got: %s", got)
	}
	if !strings.Contains(got, "disk full") {
		t.Errorf("error should contain 'disk full' from the executor, got: %s", got)
	}
}

// errWriteExecutor wraps fakeExecutor but always fails WriteFile.
type errWriteExecutor struct{ *fakeExecutor }

func (e *errWriteExecutor) WriteFile(_ context.Context, _, _ string) (string, error) {
	return "", errors.New("disk full")
}

// ─── prepareTurn ─────────────────────────────────────────────────────────────

func TestPrepareTurn_ResetsExhaustionFlags(t *testing.T) {
	app := &App{
		Cfg:    config.DefaultConfig(),
		Client: &proxy.Client{Model: "test"},
	}
	app.exhausted = true
	app.stopReason = "iteration_limit"
	app.turnBudgetStubbed = true
	app.confinementTripped = true
	app.confinementPathsHit = []string{"/bad"}

	app.prepareTurn()

	if app.exhausted {
		t.Error("exhausted should be reset")
	}
	if app.stopReason != "" {
		t.Error("stopReason should be reset")
	}
	if app.turnBudgetStubbed {
		t.Error("turnBudgetStubbed should be reset")
	}
	if app.confinementTripped {
		t.Error("confinementTripped should be reset")
	}
	if len(app.confinementPathsHit) != 0 {
		t.Error("confinementPathsHit should be reset")
	}
}

func TestPrepareTurn_AppliesSelectedModel(t *testing.T) {
	app := &App{
		Cfg:           config.DefaultConfig(),
		Client:        &proxy.Client{Model: "default-model"},
		SelectedModel: "override-model",
	}
	app.prepareTurn()
	if app.Client.Model != "override-model" {
		t.Errorf("Client.Model = %q, want %q", app.Client.Model, "override-model")
	}
}

func TestPrepareTurn_RestoresDefaultModelWhenNoOverride(t *testing.T) {
	app := &App{
		Cfg:    config.DefaultConfig(),
		Client: &proxy.Client{Model: "default-model"},
	}
	// First call sets defaultModel.
	app.prepareTurn()
	// Now set an override and prepare again.
	app.SelectedModel = "temp-override"
	app.prepareTurn()
	if app.Client.Model != "temp-override" {
		t.Errorf("Client.Model = %q, want %q", app.Client.Model, "temp-override")
	}
	// Clear the override and prepare again — should restore default.
	app.SelectedModel = ""
	app.prepareTurn()
	if app.Client.Model != "default-model" {
		t.Errorf("Client.Model = %q, want %q (restored default)", app.Client.Model, "default-model")
	}
}

func TestPrepareTurn_AppliesBackendAndAuxModel(t *testing.T) {
	app := &App{
		Cfg:             config.DefaultConfig(),
		Client:          &proxy.Client{Model: "test"},
		SelectedBackend: "llama",
	}
	app.Cfg.AuxModel = "aux-model"
	app.prepareTurn()
	if app.Client.Backend != "llama" {
		t.Errorf("Client.Backend = %q, want %q", app.Client.Backend, "llama")
	}
	if app.Client.AuxModel != "aux-model" {
		t.Errorf("Client.AuxModel = %q, want %q", app.Client.AuxModel, "aux-model")
	}
}

func TestPrepareTurn_ResetsCounselStateInTUIMode(t *testing.T) {
	app := &App{
		Cfg:         config.DefaultConfig(),
		Client:      &proxy.Client{Model: "test"},
		CounselMode: "suggest", // TUI mode
	}
	app.counselCalls = 5
	app.struggleSuggested = map[string]bool{"symptom": true}

	app.prepareTurn()

	if app.counselCalls != 0 {
		t.Errorf("counselCalls = %d, want 0 (reset in TUI mode)", app.counselCalls)
	}
	if len(app.struggleSuggested) != 0 {
		t.Errorf("struggleSuggested should be reset in TUI mode")
	}
}

func TestPrepareTurn_DoesNotResetCounselStateInHeadlessMode(t *testing.T) {
	app := &App{
		Cfg:         config.DefaultConfig(),
		Client:      &proxy.Client{Model: "test"},
		CounselMode: "", // headless mode — CounselMode empty
	}
	app.counselCalls = 5
	app.struggleSuggested = map[string]bool{"symptom": true}

	app.prepareTurn()

	if app.counselCalls != 5 {
		t.Errorf("counselCalls = %d, want 5 (not reset in headless mode)", app.counselCalls)
	}
}

// ─── streamTurn (direct unit test via sseServer) ───────────────────────────

// TestStreamTurn_NoToolCalls verifies that streamTurn returns the final text
// when the backend responds with no tool calls. This is the simplest path:
// stream → content → break.
func TestStreamTurn_NoToolCalls(t *testing.T) {
	srv := sseServer(t, []string{contentChunk("hello world")})
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.MaxToolIterations = 0 // unlimited

	var traceToolCalls []trace.ToolTrace
	final, err := app.streamTurn(context.Background(), "hi", nil, &traceToolCalls)
	if err != nil {
		t.Fatalf("streamTurn error: %v", err)
	}
	if final != "hello world" {
		t.Errorf("final = %q, want %q", final, "hello world")
	}
	// traceToolCalls is only populated when a.Trace != nil (see
	// finalizeToolResult in turn_phases.go). Without a Trace store set, it
	// stays empty — this is expected, not asserted here.
}

// TestStreamTurn_StreamError verifies that streamTurn propagates stream errors.
func TestStreamTurn_StreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}))
	defer srv.Close()

	app := newTestApp(srv.URL, newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.MaxToolIterations = 0

	var traceToolCalls []trace.ToolTrace
	_, err := app.streamTurn(context.Background(), "hi", nil, &traceToolCalls)
	if err == nil {
		t.Fatal("expected stream error, got nil")
	}
}

// TestStreamTurn_ToolCallThenFinal verifies the tool-call loop: first stream
// returns a tool call, the tool is executed, second stream returns final text.
func TestStreamTurn_ToolCallThenFinal(t *testing.T) {
	srv := sseServer(t,
		toolCallFrames("c1", "run_shell", `{"command":"echo hi"}`),
		[]string{contentChunk("done")},
	)
	defer srv.Close()

	exe := newFakeExecutor()
	app := newTestApp(srv.URL, exe, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.MaxToolIterations = 0

	var traceToolCalls []trace.ToolTrace
	final, err := app.streamTurn(context.Background(), "go", nil, &traceToolCalls)
	if err != nil {
		t.Fatalf("streamTurn error: %v", err)
	}
	if final != "done" {
		t.Errorf("final = %q, want %q", final, "done")
	}
	if len(exe.shellCalls) != 1 {
		t.Errorf("expected 1 shell call, got %d", len(exe.shellCalls))
	}
	// traceToolCalls entries are only populated when a.Trace != nil (see
	// finalizeToolResult in turn_phases.go). Without a Trace store set, the
	// slice stays empty — the tool still ran (verified via shellCalls above).
}

// TestStreamTurn_MaxIterationsForcesFinish verifies that the iteration cap
// drops tools and forces a finish.
func TestStreamTurn_MaxIterationsForcesFinish(t *testing.T) {
	srv := sseServer(t, toolCallFrames("c1", "run_shell", `{"command":"echo hi"}`))
	defer srv.Close()

	exe := newFakeExecutor()
	app := newTestApp(srv.URL, exe, func(_, _, _ string, _ bool) bool { return true })
	app.Cfg.MaxToolIterations = 2

	var traceToolCalls []trace.ToolTrace
	final, err := app.streamTurn(context.Background(), "go", nil, &traceToolCalls)
	if err != nil {
		t.Fatalf("streamTurn error: %v", err)
	}
	// At iteration 2, tools are stripped; the model gets ToolLimitPrompt
	// and responds with the same tool call — but tools are nil so it
	// breaks with whatever content came back.
	_ = final
	if !app.exhausted {
		t.Error("app.exhausted should be true after hitting iteration cap")
	}
	if app.stopReason != "iteration_limit" {
		t.Errorf("stopReason = %q, want %q", app.stopReason, "iteration_limit")
	}
}

// ─── finalizeTurn ───────────────────────────────────────────────────────────

// TestFinalizeTurn_NoCompactionNeeded verifies the no-op path: when Conv is
// small, Compact returns (false, nil) and nothing is printed.
func TestFinalizeTurn_NoCompactionNeeded(t *testing.T) {
	app := &App{
		Cfg:    config.DefaultConfig(),
		Client: &proxy.Client{Model: "test"},
		Out:    &strings.Builder{},
		Conv:   []proxy.Message{{Role: "user", Content: StrPtr("hi")}},
	}
	app.Summarize = func(_ context.Context, _ string) (string, error) {
		t.Fatal("Summarize should not be called when Conv is small")
		return "", nil
	}
	// Should not panic.
	app.finalizeTurn(context.Background())
}

// TestFinalizeTurn_CompactionFailureWarnsOnce verifies that a compaction
// failure warns once per session. Uses explicit low thresholds (not relying
// on DefaultConfig values) so the test is robust to config changes.
func TestFinalizeTurn_CompactionFailureWarnsOnce(t *testing.T) {
	out := &strings.Builder{}
	// Use explicit low thresholds so the test doesn't depend on DefaultConfig.
	cfg := config.DefaultConfig()
	cfg.CompactAt = 100 // compact when transcript exceeds 100 chars
	cfg.KeepBytes = 50  // keep 50 chars verbatim
	cfg.HardMaxBytes = 200

	app := &App{
		Cfg:    cfg,
		Client: &proxy.Client{Model: "test"},
		Out:    out,
	}
	summarizeCalled := 0
	app.Summarize = func(_ context.Context, _ string) (string, error) {
		summarizeCalled++
		return "", errors.New("summarize failed")
	}
	// Two user turns with enough content to trigger compaction (> 100 chars).
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr(strings.Repeat("x", 150))},
		{Role: "assistant", Content: StrPtr("ok")},
		{Role: "user", Content: StrPtr("next turn")},
	}

	// First call: should warn.
	app.finalizeTurn(context.Background())
	if !strings.Contains(out.String(), "compaction failed") {
		t.Errorf("expected compaction-failed warning, got: %s", out.String())
	}
	if !app.compactFailed {
		t.Error("compactFailed flag should be set after first compaction failure")
	}
	if summarizeCalled == 0 {
		t.Error("Summarize should have been called at least once")
	}

	// Second call: should NOT warn again (compactFailed is sticky).
	out.Reset()
	summarizeCalled = 0
	app.finalizeTurn(context.Background())
	if strings.Contains(out.String(), "compaction failed") {
		t.Errorf("compaction-failed warning should not repeat, got: %s", out.String())
	}
}

// ─── StopAllBackgroundProcs ──────────────────────────────────────────────────

// TestStopAllBackgroundProcs_EmptyRegistry verifies the early-return path.
func TestStopAllBackgroundProcs_EmptyRegistry(t *testing.T) {
	app := &App{
		Exec: newFakeExecutor(),
		Out:  io.Discard,
		Cfg:  config.DefaultConfig(),
	}
	start := time.Now()
	app.StopAllBackgroundProcs()
	elapsed := time.Since(start)
	if elapsed > 10*time.Millisecond {
		t.Errorf("empty StopAllBackgroundProcs took %s; expected <10ms", elapsed)
	}
}

// TestStopAllBackgroundProcs_GenerationFilter verifies that entries from an
// older executor generation are skipped (not signaled).
func TestStopAllBackgroundProcs_GenerationFilter(t *testing.T) {
	// Use selectiveKillExec to track which PIDs get killed.
	killTracker := &killTrackingExec{
		fakeExecutor:    newFakeExecutor(),
		alivePids:       map[int]bool{100: true, 200: true},
		GenerationValue: 2,
	}
	app := &App{
		Exec: killTracker,
		Out:  io.Discard,
		Cfg:  config.DefaultConfig(),
		bgRegistry: bgRegistry{bgProcs: map[string]*bgEntry{
			"bg-old": {id: "bg-old", pid: 100, pgid: 100, generation: 1}, // stale gen
			"bg-new": {id: "bg-new", pid: 200, pgid: 200, generation: 2}, // current gen
		}},
	}
	app.StopAllBackgroundProcs()
	// Only pid 200 (generation 2) should have been killed; pid 100 (gen 1) skipped.
	if killTracker.killedPids[100] {
		t.Error("pid 100 (stale generation) should NOT be killed")
	}
	if !killTracker.killedPids[200] {
		t.Error("pid 200 (current generation) should be killed")
	}
}

// TestStopAllBackgroundProcs_DoneChannelExitsCleanly verifies that when all
// done channels are already closed, StopAllBackgroundProcs returns quickly.
func TestStopAllBackgroundProcs_DoneChannelExitsCleanly(t *testing.T) {
	// Use killTrackingExec with Generation=1 to match the entries' generation,
	// so the entries are NOT skipped by the generation filter.
	exe := &killTrackingExec{
		fakeExecutor:    newFakeExecutor(),
		alivePids:       map[int]bool{100: true, 200: true},
		GenerationValue: 1,
	}
	// Pre-close the done channels so the wait loop returns immediately.
	done1 := make(chan struct{})
	close(done1)
	done2 := make(chan struct{})
	close(done2)
	app := &App{
		Exec: exe,
		Out:  io.Discard,
		Cfg:  config.DefaultConfig(),
		bgRegistry: bgRegistry{bgProcs: map[string]*bgEntry{
			"bg1": {id: "bg1", pid: 100, pgid: 100, generation: 1, done: done1},
			"bg2": {id: "bg2", pid: 200, pgid: 200, generation: 1, done: done2},
		}},
	}
	start := time.Now()
	app.StopAllBackgroundProcs()
	elapsed := time.Since(start)
	// Should return near-instantly since all done channels are already closed.
	if elapsed > 100*time.Millisecond {
		t.Errorf("StopAllBackgroundProcs with closed-done channels took %s; expected <100ms", elapsed)
	}
	// Verify SIGTERM was actually sent to both process groups (not vacuously skipped).
	if !exe.killedPids[100] {
		t.Error("SIGTERM should have been sent to pgid 100")
	}
	if !exe.killedPids[200] {
		t.Error("SIGTERM should have been sent to pgid 200")
	}
	// SIGKILL should NOT have been sent (all done channels were already closed).
	// Since killTrackingExec records all kills regardless of signal, we just
	// verify the count: each pgid should appear exactly once (SIGTERM only).
	if exe.killCount[100] != 1 {
		t.Errorf("pgid 100 should have received exactly 1 signal (SIGTERM), got %d", exe.killCount[100])
	}
	if exe.killCount[200] != 1 {
		t.Errorf("pgid 200 should have received exactly 1 signal (SIGTERM), got %d", exe.killCount[200])
	}
}

// TestStopAllBackgroundProcs_BgLogDirCleanup verifies that the bgLogDir is
// removed when there are entries in the registry. Note: when bgProcs is empty,
// StopAllBackgroundProcs returns early (before the cleanup code) — this is a
// known limitation documented in the test, not a test of that path.
func TestStopAllBackgroundProcs_BgLogDirCleanup(t *testing.T) {
	// Create a real temp directory to track cleanup.
	logDir := t.TempDir()
	// Create a file inside to verify the directory is actually removed.
	logFile := logDir + "/proc.log"
	if err := os.WriteFile(logFile, []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}
	exe := &killTrackingExec{
		fakeExecutor:    newFakeExecutor(),
		alivePids:       map[int]bool{},
		GenerationValue: 1,
	}
	app := &App{
		Exec: exe,
		Out:  io.Discard,
		Cfg:  config.DefaultConfig(),
		bgRegistry: bgRegistry{
			bgProcs: map[string]*bgEntry{
				// Entry with a nil done channel — skipped in the wait loop,
				// but the entry makes the registry non-empty so cleanup runs.
				"bg1": {id: "bg1", pid: 100, pgid: 100, generation: 1},
			},
			bgLogDir: logDir,
		},
	}
	app.StopAllBackgroundProcs()
	if app.bgLogDir != "" {
		t.Errorf("bgLogDir should be cleared after StopAllBackgroundProcs, got: %q", app.bgLogDir)
	}
	if _, err := os.Stat(logDir); !os.IsNotExist(err) {
		t.Errorf("bgLogDir should have been removed, but os.Stat returned: %v", err)
	}
}

// killTrackingExec tracks which PIDs KillPgid was called on and how many times.
type killTrackingExec struct {
	*fakeExecutor
	alivePids       map[int]bool
	GenerationValue int
	killedPids      map[int]bool
	killCount       map[int]int
}

func (k *killTrackingExec) IsProcessAlive(_ context.Context, pid int) bool {
	return k.alivePids[pid]
}
func (k *killTrackingExec) IsProcessGroupAlive(_ context.Context, pgid int) bool {
	return k.alivePids[pgid]
}

func (k *killTrackingExec) KillPgid(_ context.Context, pgid, sig int) error {
	if k.killedPids == nil {
		k.killedPids = map[int]bool{}
	}
	if k.killCount == nil {
		k.killCount = map[int]int{}
	}
	k.killedPids[pgid] = true
	k.killCount[pgid]++
	return nil
}

func (k *killTrackingExec) Generation() int { return k.GenerationValue }
