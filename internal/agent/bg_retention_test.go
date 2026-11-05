package agent

// T2 (0.4.4): finished background jobs stay readable through read_process_log
// for the rest of the chat; retention is bounded (count / bytes, oldest first)
// and eviction is reported, never mistaken for "no background process".

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/exec"
	"github.com/treeol/wakil/internal/proxy"
)

func newRetentionApp(t *testing.T) *App {
	t.Helper()
	exe, err := exec.NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { exe.Close() })
	return &App{
		Exec:    exe,
		Out:     io.Discard,
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     config.DefaultConfig(),
	}
}

func callTool(app *App, name, args string) string {
	return app.handleToolCall(context.Background(), proxy.ToolCall{Function: proxy.FunctionCall{
		Name: name, Arguments: args,
	}}).text
}

// startBg runs a run_background job and returns its id once it has left bgProcs.
func startBgAndWait(t *testing.T, app *App, command string) string {
	t.Helper()
	res := callTool(app, "run_background", fmt.Sprintf(`{"command":%q,"label":"ret"}`, command))
	var id string
	for _, line := range strings.Split(res, "\n") {
		if v, ok := strings.CutPrefix(line, "id: "); ok {
			id = strings.TrimSpace(v)
		}
	}
	if id == "" {
		t.Fatalf("no id in: %s", res)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		app.bgMu.RLock()
		_, live := app.bgProcs[id]
		fin := app.finishedBgLocked(id) != nil
		app.bgMu.RUnlock()
		if !live && fin {
			return id
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never reached bgFinished", id)
	return ""
}

// Regression for the documented incident: read_process_log on a finished job
// answered "no background process with id".
func TestReadProcessLog_FinishedJobStillReadable(t *testing.T) {
	app := newRetentionApp(t)
	id := startBgAndWait(t, app, "echo hello-retained; exit 7")
	got := callTool(app, "read_process_log", fmt.Sprintf(`{"id":%q}`, id))
	if strings.Contains(got, "no background process") {
		t.Fatalf("finished job not found: %s", got)
	}
	for _, want := range []string{"exited (code 7)", "hello-retained"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in: %s", want, got)
		}
	}
	// Reading twice works (retained, not consumed).
	if again := callTool(app, "read_process_log", fmt.Sprintf(`{"id":%q}`, id)); !strings.Contains(again, "hello-retained") {
		t.Errorf("second read lost the log: %s", again)
	}
	// kill_process on a finished job is a clean no-op message.
	if k := callTool(app, "kill_process", fmt.Sprintf(`{"id":%q}`, id)); !strings.Contains(k, "already exited") {
		t.Errorf("kill on finished job: %s", k)
	}
}

func TestBgRetention_EvictsOldestByCountAndReports(t *testing.T) {
	old := bgFinishedMaxJobs
	bgFinishedMaxJobs = 2
	t.Cleanup(func() { bgFinishedMaxJobs = old })
	app := newRetentionApp(t)
	ids := []string{
		startBgAndWait(t, app, "echo one"),
		startBgAndWait(t, app, "echo two"),
		startBgAndWait(t, app, "echo three"),
	}
	first := callTool(app, "read_process_log", fmt.Sprintf(`{"id":%q}`, ids[0]))
	if !strings.Contains(first, "evicted") || strings.Contains(first, "no background process") {
		t.Fatalf("oldest job should report eviction, got: %s", first)
	}
	if !strings.Contains(first, "last 2 jobs") {
		t.Errorf("eviction message should state the bound: %s", first)
	}
	for i, want := range []string{"two", "three"} {
		got := callTool(app, "read_process_log", fmt.Sprintf(`{"id":%q}`, ids[i+1]))
		if !strings.Contains(got, want) {
			t.Errorf("retained job %s lost output %q: %s", ids[i+1], want, got)
		}
	}
	// The evicted job's log file is gone from disk.
	app.bgMu.RLock()
	nFin := len(app.bgFinished)
	app.bgMu.RUnlock()
	if nFin != 2 {
		t.Errorf("bgFinished has %d entries, want 2", nFin)
	}
}

func TestBgRetention_EvictsByBytesButKeepsSingleLog(t *testing.T) {
	oldN, oldB := bgFinishedMaxJobs, bgFinishedMaxBytes
	bgFinishedMaxBytes = 1000
	t.Cleanup(func() { bgFinishedMaxJobs, bgFinishedMaxBytes = oldN, oldB })
	app := newRetentionApp(t)
	a := startBgAndWait(t, app, "head -c 3000 /dev/zero | tr '\\0' a")
	// A single oversized log is retained alone.
	if got := callTool(app, "read_process_log", fmt.Sprintf(`{"id":%q}`, a)); strings.Contains(got, "evicted") {
		t.Fatalf("single log must not evict itself: %s", got)
	}
	b := startBgAndWait(t, app, "head -c 3000 /dev/zero | tr '\\0' b")
	if got := callTool(app, "read_process_log", fmt.Sprintf(`{"id":%q}`, a)); !strings.Contains(got, "evicted") {
		t.Errorf("older log should be evicted by the byte bound: %s", got)
	}
	if got := callTool(app, "read_process_log", fmt.Sprintf(`{"id":%q}`, b)); strings.Contains(got, "evicted") {
		t.Errorf("newest log must survive: %s", got)
	}
}

func TestBgRetention_ReaderBlocksEviction(t *testing.T) {
	old := bgFinishedMaxJobs
	bgFinishedMaxJobs = 1
	t.Cleanup(func() { bgFinishedMaxJobs = old })
	app := newRetentionApp(t)
	first := startBgAndWait(t, app, "echo keep-me")
	app.bgMu.Lock()
	e := app.finishedBgLocked(first)
	e.readers++ // a reader is mid-read
	app.bgMu.Unlock()
	logPath := e.logPath
	startBgAndWait(t, app, "echo second")
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("log of an entry with a reader was deleted: %v", err)
	}
	app.bgMu.Lock()
	e.readers--
	app.bgMu.Unlock()
	app.trimBgFinished()
	if _, err := os.Stat(logPath); err == nil {
		t.Error("log should be evicted once the reader is gone")
	}
}

func TestBgRetention_RetireIsIdempotentAndDeclinedIDIsNotEvicted(t *testing.T) {
	app := newRetentionApp(t)
	id := startBgAndWait(t, app, "echo x")
	app.bgMu.Lock()
	app.retireBgLocked(id) // already retired: no-op
	app.retireBgLocked(id)
	n := len(app.bgFinished)
	app.bgMu.Unlock()
	if n != 1 {
		t.Fatalf("double retire inserted %d entries, want 1", n)
	}
	// A declined launch consumes an id but must read as "no background process".
	app.Confirm = func(_, _, _ string, _ bool) bool { return false }
	if res := callTool(app, "run_background", `{"command":"echo y","label":"d"}`); !strings.Contains(res, "declined") {
		t.Fatalf("expected decline, got %s", res)
	}
	got := callTool(app, "read_process_log", `{"id":"bg2"}`)
	if !strings.Contains(got, "no background process") || strings.Contains(got, "evicted") {
		t.Errorf("declined id must not read as evicted: %s", got)
	}
}

func TestBgRetention_ShutdownRemovesRetainedLogs(t *testing.T) {
	app := newRetentionApp(t)
	id := startBgAndWait(t, app, "echo bye")
	app.bgMu.RLock()
	logPath := app.finishedBgLocked(id).logPath
	app.bgMu.RUnlock()
	if _, err := os.Stat(logPath); err != nil {
		t.Fatal(err)
	}
	app.StopAllBackgroundProcs()
	if _, err := os.Stat(logPath); err == nil {
		t.Error("retained log survived StopAllBackgroundProcs")
	}
}
