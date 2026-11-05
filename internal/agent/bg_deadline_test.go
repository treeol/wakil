package agent

// T2 review (astra 4): a shell that completes AT its deadline must be reported
// as a finished job — its output inline — not as "no longer tracked". The
// reaper can finish the job and retire it between the deadline timer and the
// registry check; the barrier below forces exactly that interleaving.

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/exec"
)

func TestShellCompletingAtDeadlineIsNotLost(t *testing.T) {
	exe, err := exec.NewDirectExecutor(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { exe.Close() })
	cfg := config.DefaultConfig()
	cfg.ShellTimeoutSec = 1
	app := &App{
		Exec:    exe,
		Out:     testWriter{},
		Confirm: func(_, _, _ string, _ bool) bool { return true },
		Cfg:     cfg,
	}

	// The command finishes in ~0.3 s; the barrier below forces the timer to be
	// "fired" only once the reaper has already retired the job, which is the
	// interleaving astra-4 described.
	var once sync.Once
	var retired sync.WaitGroup
	retired.Add(1)
	shellDeadlineHook = func(bgID string) {
		once.Do(func() {
			// Wait until the job has left bgProcs (reaper finished + retired).
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				app.bgMu.RLock()
				_, live := app.bgProcs[bgID]
				app.bgMu.RUnlock()
				if !live {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			retired.Done()
		})
	}
	t.Cleanup(func() { shellDeadlineHook = nil })

	// A command that finishes with output just after the 1 s deadline. The
	// hook holds the deadline branch until the reaper has retired the job.
	res := callTool(app, "run_shell", `{"command":"sleep 1.05; echo finished-at-deadline"}`)
	retired.Wait()

	if strings.Contains(res, "no longer tracked") {
		t.Fatalf("a job that completed at its deadline was reported as lost: %s", res)
	}
	if !strings.Contains(res, "finished-at-deadline") {
		t.Errorf("the job's output must come back with the deadline result: %s", res)
	}
	// The deadline branch consumed the job: no retained entry, no registry entry.
	app.bgMu.RLock()
	nFin, nLive := len(app.bgFinished), len(app.bgProcs)
	app.bgMu.RUnlock()
	if nFin != 0 || nLive != 0 {
		t.Errorf("job not released after the deadline result: retained=%d live=%d", nFin, nLive)
	}
}

type testWriter struct{}

func (testWriter) Write(p []byte) (int, error) { return len(p), nil }
