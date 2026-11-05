package agent

// T2 review (astra 3): a read of a background job must hold its log for the whole
// read, even when the job is retired and evicted while the read is in progress.
// The barrier below pauses the read inside the executor at the point where it
// reads the log, lets retirement and eviction run, then resumes the read.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/exec"
)

// barrierExec wraps an executor for the whole test (installed before any job
// starts, never swapped). Once armed, the first ReadFileTail on the path under
// test blocks until released. Armed/path are guarded by mu.
type barrierExec struct {
	exec.Executor
	mu      sync.Mutex
	path    string
	reached chan struct{}
	release chan struct{}
	fired   bool
}

// arm makes the next ReadFileTail of path a barrier.
func (b *barrierExec) arm(path string) {
	b.mu.Lock()
	b.path, b.reached, b.release, b.fired = path, make(chan struct{}), make(chan struct{}), false
	b.mu.Unlock()
}

func (b *barrierExec) ReadFileTail(ctx context.Context, path string, max int64) (string, error) {
	b.mu.Lock()
	block := !b.fired && b.path != "" && path == b.path
	var reached, release chan struct{}
	if block {
		b.fired = true
		reached, release = b.reached, b.release
	}
	b.mu.Unlock()
	if block {
		close(reached)
		<-release
	}
	return b.Executor.ReadFileTail(ctx, path, max)
}

// Finished-job read under eviction pressure: the read must keep its log.
func TestReadPinnedAgainstRetirementAndEviction(t *testing.T) {
	old := bgFinishedMaxJobs
	bgFinishedMaxJobs = 1
	t.Cleanup(func() { bgFinishedMaxJobs = old })

	base := newRetentionApp(t)
	barrier := &barrierExec{Executor: base.Exec}
	app := base
	app.Exec = barrier // installed before any job starts; never swapped again

	// A job that is still running (active entry) when the read starts.
	res := callTool(app, "run_background", `{"command":"echo held-output; sleep 1; echo tail-line","label":"pin"}`)
	var id string
	for _, line := range strings.Split(res, "\n") {
		if v, ok := strings.CutPrefix(line, "id: "); ok {
			id = strings.TrimSpace(v)
		}
	}
	if id == "" {
		t.Fatalf("no id: %s", res)
	}
	app.bgMu.RLock()
	logPath := app.bgProcs[id].logPath
	app.bgMu.RUnlock()
	barrier.arm(logPath)

	readOut := make(chan string, 1)
	go func() {
		readOut <- callTool(app, "read_process_log", fmt.Sprintf(`{"id":%q}`, id))
	}()

	// The read is now inside the executor, holding the log path.
	barrier.mu.Lock()
	reached := barrier.reached
	barrier.mu.Unlock()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("read never reached the log")
	}

	// While it is held: the job finishes, is retired, and two more jobs push it
	// out of the retention bound. The pin must keep this job's log.
	waitFinished(t, app, id) // registry reads only; no Exec access
	// Push two more finished jobs into the registry (retained bound is 1). They
	// are inserted directly so no tool call races the blocked read.
	app.bgMu.Lock()
	for i := 0; i < 2; i++ {
		e := &bgEntry{id: fmt.Sprintf("bgX%d", i), logPath: logPath + ".other"}
		app.bgFinished = append(app.bgFinished, e)
	}
	app.bgMu.Unlock()
	app.trimBgFinished()
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("log of a job being read was removed by eviction: %v", err)
	}

	barrier.mu.Lock()
	release := barrier.release
	barrier.mu.Unlock()
	close(release)
	got := <-readOut
	if !strings.Contains(got, "held-output") {
		t.Fatalf("read lost its log mid-read; got: %s", got)
	}
	if strings.Contains(got, "log not yet available") || strings.Contains(got, "evicted") {
		t.Fatalf("read degraded: %s", got)
	}

	// After the read is released the job is evictable like any other.
	app.bgMu.RLock()
	pinned := app.finishedBgLocked(id)
	app.bgMu.RUnlock()
	if pinned != nil && pinned.readers != 0 {
		t.Errorf("reader count not released: %d", pinned.readers)
	}
	app.trimBgFinished()
	app.bgMu.RLock()
	stillThere := app.finishedBgLocked(id) != nil
	app.bgMu.RUnlock()
	if stillThere {
		t.Error("job is still retained after its read finished and the bound was exceeded")
	}
}

func waitFinished(t *testing.T, app *App, id string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		app.bgMu.RLock()
		_, live := app.bgProcs[id]
		fin := app.finishedBgLocked(id) != nil
		app.bgMu.RUnlock()
		if !live && fin {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s never finished", id)
}
