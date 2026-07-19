package agent

import (
	"context"
	"fmt"
	"sync"
)

// recordingExecutor wraps *fakeExecutor and records every mutating call with
// its arguments, so gating tests can prove that a declined/malformed/
// confine-rejected action produced ZERO side effects — and, just as
// importantly, that an accepted action produced exactly the intended one
// (the vacuity rule: an accept-case assertion that the call WAS recorded
// guards against a forgotten override passing decline cases vacuously).
//
// Mutating methods (from the exec.Executor interface audit, WP-0):
//
//	RunShell, WriteFile, DeletePath, MovePath, StartBackground, KillPgid
//
// Every one of them is overridden here. All other Executor methods pass
// through to the embedded fake unchanged. Injectable errors simulate
// executor failures AFTER the gate, keeping "the gate let it through" and
// "the executor failed" as separately assertable facts.
//
// Contract:
//   - declined / malformed-args / confine-rejected → zero recorded calls
//   - accepted + executor success → exactly one recorded call
//   - accepted + injected executor error → exactly one recorded call (the
//     attempt) plus an error result
//
// Mutex-protected: safe under t.Parallel() and background-process tests
// that spawn goroutines polling IsProcessAlive.
type recordingExecutor struct {
	*fakeExecutor

	mu    sync.Mutex
	calls []recordedCall

	// Injectable errors, applied after recording (the attempt counts).
	runShellErr        error
	writeFileErr       error
	deletePathErr      error
	movePathErr        error
	startBackgroundErr error
	killPgidErr        error

	// bgPIDs issued by StartBackground (fakeExecutor hardcodes 1234; tests
	// that start several processes need distinct ids). killedPGIDs records
	// process groups that received a signal — IsProcessAlive reports them
	// dead, modelling the signal taking effect.
	nextPID     int
	killedPGIDs map[int]bool
}

type recordedCall struct {
	Method string
	Args   string
}

func newRecordingExecutor() *recordingExecutor {
	return &recordingExecutor{fakeExecutor: newFakeExecutor(), nextPID: 4100, killedPGIDs: map[int]bool{}}
}

// recorded returns a copy of the recorded calls (method names only).
func (r *recordingExecutor) recorded() []recordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedCall, len(r.calls))
	copy(out, r.calls)
	return out
}

// countMethod returns how many calls were recorded for a method.
func (r *recordingExecutor) countMethod(method string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

// totalCalls returns the number of recorded mutating calls.
func (r *recordingExecutor) totalCalls() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func (r *recordingExecutor) record(method, args string) {
	r.mu.Lock()
	r.calls = append(r.calls, recordedCall{Method: method, Args: args})
	r.mu.Unlock()
}

func (r *recordingExecutor) RunShell(ctx context.Context, cmd string) (string, error) {
	r.record("RunShell", cmd)
	if r.runShellErr != nil {
		return "", r.runShellErr
	}
	return r.fakeExecutor.RunShell(ctx, cmd)
}

func (r *recordingExecutor) WriteFile(ctx context.Context, path, content string) (string, error) {
	r.record("WriteFile", path)
	if r.writeFileErr != nil {
		return "", r.writeFileErr
	}
	return r.fakeExecutor.WriteFile(ctx, path, content)
}

func (r *recordingExecutor) DeletePath(ctx context.Context, path string) error {
	r.record("DeletePath", path)
	if r.deletePathErr != nil {
		return r.deletePathErr
	}
	return r.fakeExecutor.DeletePath(ctx, path)
}

func (r *recordingExecutor) MovePath(ctx context.Context, src, dst string) error {
	r.record("MovePath", src+" -> "+dst)
	if r.movePathErr != nil {
		return r.movePathErr
	}
	return r.fakeExecutor.MovePath(ctx, src, dst)
}

func (r *recordingExecutor) StartBackground(ctx context.Context, command, logPath string) (int, int, error) {
	r.record("StartBackground", command)
	if r.startBackgroundErr != nil {
		return 0, 0, r.startBackgroundErr
	}
	r.mu.Lock()
	pid := r.nextPID
	r.nextPID++
	r.mu.Unlock()
	return pid, pid, nil
}

func (r *recordingExecutor) KillPgid(ctx context.Context, pgid, sig int) error {
	r.record("KillPgid", fmt.Sprintf("pgid=%d sig=%d", pgid, sig))
	if r.killPgidErr != nil {
		return r.killPgidErr
	}
	r.mu.Lock()
	r.killedPGIDs[pgid] = true
	r.mu.Unlock()
	return r.fakeExecutor.KillPgid(ctx, pgid, sig)
}

// IsProcessAlive reports true for PIDs issued by StartBackground whose
// process group has not been signalled (fakeExecutor always returns false,
// which would make handleKillProcess report "already exited" and never test
// the signal path).
func (r *recordingExecutor) IsProcessAlive(_ context.Context, pid int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.killedPGIDs[pid] { // pgid == pid for this fake
		return false
	}
	return pid >= 4100 && pid < r.nextPID
}
