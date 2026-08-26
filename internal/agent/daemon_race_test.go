package agent

// daemon_race_test.go: race-detector tests for the B2/B5 concurrency
// hardening. Each test runs concurrent goroutines that exercise the
// stateMu-protected exported methods (setters, getters, snapshots) and
// the turn goroutine's locked reads (activeThresholds, SnapshotSessionState).
//
// Run with: go test -race ./internal/agent/ -run TestRace
//
// These tests do NOT assert behavior — they assert that the race detector
// finds no data races when concurrent goroutines access the same App's
// state through the locked exported methods. A passing race test does
// not prove race freedom in all execution orders — it verifies that the
// specific access patterns exercised here are synchronized.

import (
	"sync"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// startBarrier is a closed channel used to ensure all goroutines begin
// execution simultaneously, maximizing the chance of overlapping access.
func startBarrier(n int) (chan struct{}, *sync.WaitGroup) {
	barrier := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)
	return barrier, &wg
}

// TestRaceSetModelVsSnapshot exercises concurrent SetModelOverride (RPC
// handler path) and SnapshotSessionState (GetSessionState path). Both
// acquire stateMu, so no race should be detected.
func TestRaceSetModelVsSnapshot(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Session = &Session{ChatID: "race-test"}

	barrier, wg := startBarrier(2)
	const N = 200

	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			app.SetModelOverride("model-a")
			app.SetModelOverride("model-b")
		}
	}()
	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			_ = app.SnapshotSessionState()
		}
	}()
	close(barrier)
	wg.Wait()
}

// TestRaceSetBackendVsSnapshot exercises concurrent SetBackendSelection and
// SnapshotSessionState + SelectedBackendLocked/SelectedModelLocked reads.
func TestRaceSetBackendVsSnapshot(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Session = &Session{ChatID: "race-test"}

	barrier, wg := startBarrier(3)
	const N = 200

	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			app.SetBackendSelection("backend-a/model-x")
			app.SetBackendSelection("backend-b")
		}
	}()
	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			_ = app.SnapshotSessionState()
		}
	}()
	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			_ = app.SelectedBackendLocked()
			_ = app.SelectedModelLocked()
		}
	}()
	close(barrier)
	wg.Wait()
}

// TestRaceSetRawToolsVsSnapshot exercises concurrent SetRawToolsValue and
// SnapshotSessionState + RawToolsLocked.
func TestRaceSetRawToolsVsSnapshot(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Session = &Session{ChatID: "race-test"}

	barrier, wg := startBarrier(3)
	const N = 200

	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			app.SetRawToolsValue(true)
			app.SetRawToolsValue(false)
		}
	}()
	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			_ = app.SnapshotSessionState()
		}
	}()
	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			_ = app.RawToolsLocked()
		}
	}()
	close(barrier)
	wg.Wait()
}

// TestRaceSetCounselModeVsSnapshot exercises concurrent SetCounselModeValue
// and SnapshotSessionState + CounselModeLocked/MaxCounselLocked.
func TestRaceSetCounselModeVsSnapshot(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Session = &Session{ChatID: "race-test"}

	barrier, wg := startBarrier(3)
	const N = 200

	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			app.SetCounselModeValue("auto", 3)
			app.SetCounselModeValue("off", 0)
		}
	}()
	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			_ = app.SnapshotSessionState()
		}
	}()
	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			_ = app.CounselModeLocked()
			_ = app.MaxCounselLocked()
		}
	}()
	close(barrier)
	wg.Wait()
}

// TestRaceSetMaxParallelVsSnapshot exercises concurrent SetMaxParallel and
// SnapshotSessionState + MaxParallelLocked.
func TestRaceSetMaxParallelVsSnapshot(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Session = &Session{ChatID: "race-test"}

	barrier, wg := startBarrier(3)
	const N = 200

	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			app.SetMaxParallel(4)
			app.SetMaxParallel(8)
		}
	}()
	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			_ = app.SnapshotSessionState()
		}
	}()
	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			_ = app.MaxParallelLocked()
		}
	}()
	close(barrier)
	wg.Wait()
}

// TestRaceSetMaxCtxVsActiveThresholds exercises concurrent SetMaxCtxOverride
// and activeThresholds (which reads EffectiveCtxMaxCharsOverride). Both
// must be under stateMu — if activeThresholds reads without the lock, the
// race detector flags it.
func TestRaceSetMaxCtxVsActiveThresholds(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Session = &Session{ChatID: "race-test"}
	app.CtxLimit = ContextLimit{NCtx: 128000, UsableCtx: 100000}

	barrier, wg := startBarrier(2)
	const N = 200

	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			app.SetMaxCtxOverride(200000)
			app.SetMaxCtxOverride(0)
		}
	}()
	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			_, _, _ = app.activeThresholds()
		}
	}()
	close(barrier)
	wg.Wait()
}

// TestRaceSetSubagentOverridesVsSnapshot exercises concurrent
// SetSubagentEndpointOverride/SetSubagentModelOverride and
// SnapshotSessionState + locked getters.
func TestRaceSetSubagentOverridesVsSnapshot(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Session = &Session{ChatID: "race-test"}

	barrier, wg := startBarrier(3)
	const N = 200

	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			app.SetSubagentEndpointOverride("ep-a")
			app.SetSubagentModelOverride("model-x")
			app.SetSubagentEndpointOverride("")
			app.SetSubagentModelOverride("")
		}
	}()
	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			_ = app.SnapshotSessionState()
		}
	}()
	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			_ = app.SubagentEndpointOverrideLocked()
			_ = app.SubagentModelOverrideLocked()
		}
	}()
	close(barrier)
	wg.Wait()
}

// TestRaceSaveSessionVsSetSessionLabel exercises concurrent SaveSession
// (turn defer path, acquires saveMu → stateMu → convMu) and
// SetSessionLabelValue (RPC path, acquires stateMu then SaveSession).
// Both serialize on saveMu — the race detector should find no data race.
// This test does NOT verify persisted data integrity — it only checks
// that the race detector finds no unsynchronized memory access.
func TestRaceSaveSessionVsSetSessionLabel(t *testing.T) {
	dir := t.TempDir()
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Session = &Session{ChatID: "race-save-test", Workspace: dir}
	app.Conv = []proxy.Message{{Role: "user", Content: StrPtr("test")}}

	barrier, wg := startBarrier(2)
	const N = 100

	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			app.SaveSession()
		}
	}()
	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			app.SetSessionLabelValue("label-a")
			app.SetSessionLabelValue("label-b")
		}
	}()
	close(barrier)
	wg.Wait()
}

// TestRaceSetCtxLimitVsActiveThresholds exercises concurrent SetCtxLimit
// and activeThresholds (which reads CtxLimit). SetCtxLimit acquires
// stateMu; activeThresholds must also hold stateMu or the race detector
// flags the CtxLimit read.
func TestRaceSetCtxLimitVsActiveThresholds(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Session = &Session{ChatID: "race-test"}

	barrier, wg := startBarrier(2)
	const N = 200

	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			app.SetCtxLimit(ContextLimit{NCtx: 128000, UsableCtx: 100000})
			app.SetCtxLimit(ContextLimit{NCtx: 200000, UsableCtx: 180000})
		}
	}()
	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			_, _, _ = app.activeThresholds()
		}
	}()
	close(barrier)
	wg.Wait()
}

// TestRaceSnapshotVsSomeSetters exercises SnapshotSessionState against
// several concurrent setters — a race check for the GetSessionState handler
// path (one snapshot read vs concurrent RPC writes). Does not cover every
// setter; individual setter-vs-snapshot tests above cover the rest.
func TestRaceSnapshotVsSomeSetters(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Session = &Session{ChatID: "race-all", Workspace: t.TempDir()}
	app.CtxLimit = ContextLimit{NCtx: 128000, UsableCtx: 100000}

	barrier, wg := startBarrier(6)
	const N = 100

	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			app.SetModelOverride("model-a")
		}
	}()
	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			app.SetBackendSelection("backend-a/model-x")
		}
	}()
	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			app.SetRawToolsValue(true)
		}
	}()
	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			app.SetCounselModeValue("auto", 3)
		}
	}()
	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			app.SetMaxParallel(4)
		}
	}()
	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			_ = app.SnapshotSessionState()
		}
	}()
	close(barrier)
	wg.Wait()
}

// TestRaceRestoreRepoStateApplyVsSnapshot exercises concurrent
// RestoreRepoStateApply (which writes many App+Cfg fields under stateMu)
// and SnapshotSessionState (which reads those same fields under
// stateMu.RLock). This caught a real bug: SnapshotSessionState called
// SessionWorkspace() outside the lock, which invokes Cfg.WorkspacePath()
// — a value-receiver method that copies the entire Config struct, reading
// every field including those written by restoreEndpointIndependentLocked.
func TestRaceRestoreRepoStateApplyVsSnapshot(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.Session = &Session{ChatID: "race-restore", Workspace: t.TempDir()}

	st := &RepoState{
		SchemaVersion:       repoStateSchemaVersion,
		Workspace:           app.Session.Workspace,
		Model:               "test-model",
		Backend:             "test-backend",
		RawTools:            true,
		MaxParallelSubagents: 4,
		CounselMode:         "auto",
		MaxCounsel:          3,
		MashuraDefaultModel: "test-oracle",
	}

	barrier, wg := startBarrier(2)
	const N = 100

	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			_ = RestoreRepoStateApply(app, st)
		}
	}()
	go func() {
		defer wg.Done()
		<-barrier
		for i := 0; i < N; i++ {
			_ = app.SnapshotSessionState()
		}
	}()
	close(barrier)
	wg.Wait()
}
