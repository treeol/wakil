package hisn

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/sandbox"
)

// An empty base environment must yield an EMPTY (non-nil) Env: exec.Cmd treats
// nil as "inherit the parent's environment".
func TestScrubbedEnvNeverNil(t *testing.T) {
	if got := scrubbedEnv(nil, nil); got == nil || len(got) != 0 {
		t.Fatalf("scrubbedEnv(nil) = %#v, want empty non-nil slice", got)
	}
	if got := scrubbedEnv([]string{"SECRET=1", "LD_PRELOAD=/x"}, nil); got == nil || len(got) != 0 {
		t.Fatalf("fully filtered env = %#v, want empty non-nil slice", got)
	}
}

// With a hostile sentinel in Wakil's REAL process environment and an empty
// configured base env, no hisn client may see it.
func TestEmptyBaseEnvDoesNotInheritProcessEnv(t *testing.T) {
	t.Setenv("WAKIL_SENTINEL_SECRET", "must-not-leak")
	fe := newFakeEnv(t)
	o := fe.options()
	o.Env = []string{} // non-nil but empty
	b, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := b.output(ctxT(t), false, "--version"); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	data, _ := os.ReadFile(filepath.Join(fe.state, "last_env"))
	if strings.Contains(string(data), "WAKIL_SENTINEL_SECRET") {
		t.Fatalf("process environment leaked into a hisn client:\n%s", data)
	}
}

// A kill must re-verify the instance before each signal: an id reused by a
// foreign session after verification is never signalled.
func TestKillReverifiesInstance(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	fe.addForeignSession("reused-id") // foreign instance, hostname "someone-else"
	err := b.killSession(ctxT(t), "reused-id", newToken())
	if err == nil || !strings.Contains(err.Error(), "no longer the instance") {
		t.Fatalf("want refusal, got %v", err)
	}
	if !contains(fe.sessionIDs(), "reused-id") {
		t.Fatal("a foreign session was signalled")
	}
}

// A policy with no hostname at all is "unknown": the record must be kept.
func TestUnverifiableInstanceKeepsRecord(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	dir := filepath.Join(fe.state, "s", "nohost")
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "policy"), []byte("cwd = '/x'\n"), 0o600) // no hostname key
	rec := record{ID: "nohost", Pid: 999999, Token: newToken(), Cwd: fe.cwd, Started: true}
	_ = os.MkdirAll(b.sessionsDir(), 0o700)
	if err := b.createRecord(rec); err != nil {
		t.Fatal(err)
	}
	if err := b.Reconcile(ctxT(t)); err == nil {
		t.Fatal("an unverifiable session must be reported, not silently dropped")
	}
	if len(b.listRecordIDs()) != 1 || !contains(fe.sessionIDs(), "nohost") {
		t.Fatal("record or session discarded although ownership could not be disproved")
	}
}

// A failed start whose cleanup also fails must stay tracked, and Close must
// finish the job once the failure clears.
func TestFailedStartCleanupIsTrackedAndRetried(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	// Make verification fail after `run` succeeded (inspect cannot find it),
	// and make the abort kill fail too.
	_ = os.WriteFile(filepath.Join(fe.state, "inspect_fail"), nil, 0o600)
	_ = os.WriteFile(filepath.Join(fe.state, "kill_fail"), nil, 0o600)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := b.Start(ctx, sandbox.Session{ID: "half", HostCwd: fe.cwd}); err == nil {
		t.Fatal("start must fail")
	}
	b.mu.RLock()
	tracked := len(b.orphans)
	b.mu.RUnlock()
	if tracked != 1 {
		t.Fatalf("failed start left %d tracked obligations, want 1", tracked)
	}
	if len(b.listRecordIDs()) != 1 {
		t.Fatal("record must be kept while the session may still run")
	}
	_ = os.Remove(filepath.Join(fe.state, "inspect_fail"))
	_ = os.Remove(filepath.Join(fe.state, "kill_fail"))
	if err := b.Close(ctxT(t)); err != nil {
		t.Fatalf("Close must retry the stranded cleanup: %v", err)
	}
	if contains(fe.sessionIDs(), "half") || len(b.listRecordIDs()) != 0 {
		t.Fatal("stranded session not cleaned by Close")
	}
}

// A grant/revoke must not outlive its session or touch the replacement.
func TestGrantBoundToItsSession(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	_ = b.Start(ctxT(t), sandbox.Session{ID: "old", HostCwd: fe.cwd})
	l, release, err := b.admit()
	if err != nil {
		t.Fatal(err)
	}
	release()
	_ = b.Start(ctxT(t), sandbox.Session{ID: "new", HostCwd: fe.cwd}) // replaces "old"
	if b.stillCurrent(l) {
		t.Fatal("replaced session still reported current")
	}
	cctx, done := bindCtx(ctxT(t), l)
	defer done()
	if cctx.Err() == nil {
		t.Fatal("a replaced session's control context must be cancelled")
	}
	if _, ok := b.pm.MapPath("/srv/stale"); ok == nil {
		t.Fatal("unexpected mapping")
	}
	_ = b.Close(context.Background())
}

func TestStopCancelsInFlightExecsWithinBound(t *testing.T) {
	b, fe := openFake(t)
	_ = b.Start(ctxT(t), sandbox.Session{ID: "c", HostCwd: fe.cwd})
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		h, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"sleep", "60"}})
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = h.Wait() }()
	}
	if err := b.Stop(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("in-flight execs outlived Stop")
	}
	if _, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"true"}}); !errors.Is(err, sandbox.ErrNoSession) {
		t.Fatalf("exec after Stop = %v", err)
	}
}
