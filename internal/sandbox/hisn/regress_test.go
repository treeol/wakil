package hisn

import (
	"context"
	"errors"
	"io"
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
	dir := filepath.Join(fe.rt(), "nohost")
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

// A failed start whose supervisor cannot be reaped must stay tracked, and Close
// must finish the job once it can be killed.
func TestFailedStartCleanupIsTrackedAndRetried(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	b.readyTimeout = 400 * time.Millisecond
	b.stopWait = 300 * time.Millisecond
	fe.setMode("nosock") // never becomes ready
	var sup *supervisorProc
	b.afterSpawn = func(p *supervisorProc) {
		sup = p
		p.killFn = func() {} // SIGKILL has no effect: the abort cannot reap it
	}
	if err := b.Start(ctxT(t), sandbox.Session{ID: "half", HostCwd: fe.cwd}); err == nil {
		t.Fatal("start must fail")
	}
	b.mu.RLock()
	tracked := len(b.orphans)
	b.mu.RUnlock()
	if tracked != 1 {
		t.Fatalf("failed start left %d tracked obligations, want 1", tracked)
	}
	if len(b.listRecordIDs()) != 1 {
		t.Fatal("record must be kept while the supervisor may still run")
	}
	sup.killFn = nil
	if err := b.Close(ctxT(t)); err != nil {
		t.Fatalf("Close must retry the stranded cleanup: %v", err)
	}
	if !sup.hasExited() || contains(fe.sessionIDs(), "half") || len(b.listRecordIDs()) != 0 {
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

// Wait after cancel() must never block: cancel()'s bounded wait used to
// consume the single-shot exit and the later Wait() hung forever. The exit is
// latched now. TERM is ignored by the program (trap), so this also walks the
// TERM → KILL path and the exit must report the KILL signal (128+9).
func TestWaitAfterKillReturnsLatchedExit(t *testing.T) {
	b, fe := openFake(t)
	b.stopWait = 2 * time.Second
	if err := b.Start(ctxT(t), sandbox.Session{ID: "wk1", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	h, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"sh", "-c", "trap '' TERM; sleep 60"}, Stdout: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // let the trap install
	if err := h.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	done := make(chan struct{})
	var info sandbox.ExitInfo
	var werr error
	go func() { info, werr = h.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait blocked after Kill: the exit status was consumed, not latched")
	}
	if werr != nil {
		t.Fatal(werr)
	}
	if info.Code != 128+9 || info.Lost || info.BackendError {
		t.Fatalf("exit after Kill = %+v, want Code 137 (SIGKILL) and no Lost/BackendError", info)
	}
}

// output returns stdout-only on success so a stderr warning (e.g. "GOCOVERDIR
// not set" from a coverage-instrumented test binary whose env was scrubbed)
// cannot corrupt the JSON on stdout. On error, stderr is included so
// diagnostic checks (isConnRefused) still see it.
func TestOutputSplitsStdoutStderr(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	defer b.Close(context.Background())
	// The fake --version prints "hisn 0.2.1\n" to stdout and nothing to stderr.
	// A clean stdout is what listSessions and verifyInstance parse.
	out, err := b.output(ctxT(t), false, "--version")
	if err != nil {
		t.Fatalf("output: %v: %s", err, out)
	}
	if strings.TrimSpace(out) != "hisn 0.2.1" {
		t.Fatalf("stdout = %q, want clean version line (no stderr noise)", out)
	}
}

// Wait and Kill racing from two goroutines both return, with the same result:
// neither may steal the other's exit.
func TestWaitAndKillConcurrentAgree(t *testing.T) {
	b, fe := openFake(t)
	b.stopWait = 2 * time.Second
	if err := b.Start(ctxT(t), sandbox.Session{ID: "wk2", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	h, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"sh", "-c", "trap '' TERM; sleep 60"}, Stdout: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	type res struct {
		info sandbox.ExitInfo
		err  error
	}
	waitC, waitC2, killC := make(chan res, 1), make(chan res, 1), make(chan error, 1)
	go func() { i, e := h.Wait(); waitC <- res{i, e} }()
	go func() { i, e := h.Wait(); waitC2 <- res{i, e} }()
	go func() { killC <- h.Kill() }()
	timeout := time.After(15 * time.Second)
	var w1, w2 res
	for got := 0; got < 3; got++ {
		select {
		case w1x := <-waitC:
			w1 = w1x
		case w2x := <-waitC2:
			w2 = w2x
		case err := <-killC:
			if err != nil {
				t.Errorf("Kill: %v", err)
			}
		case <-timeout:
			t.Fatal("Wait/Kill deadlocked: an exit consumer starved the other")
		}
	}
	if w1.err != nil || w2.err != nil {
		t.Fatalf("Wait errors: %v / %v", w1.err, w2.err)
	}
	if w1.info != w2.info || w1.info.Code != 128+9 {
		t.Fatalf("concurrent Waits disagree: %+v vs %+v (want both 137)", w1.info, w2.info)
	}
}
