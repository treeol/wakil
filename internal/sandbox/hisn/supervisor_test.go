package hisn

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/sandbox"
)

func (f *fakeEnv) hasSocket(id string) bool {
	_, err := os.Lstat(filepath.Join(f.rt(), id, "control.sock"))
	return err == nil
}

func (b *Backend) supOf() *supervisorProc {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.cur == nil {
		return nil
	}
	return b.cur.sup
}

func killSupervisorHard(t *testing.T, b *Backend) {
	t.Helper()
	sup := b.supOf()
	_ = syscall.Kill(sup.pid(), syscall.SIGKILL)
	if !sup.waitExit(5 * time.Second) {
		t.Fatal("supervisor did not die")
	}
}

// ---- the supervisor blocks: Start must return once READY, not once hisn exits ----

func TestStartReturnsWhileSupervisorKeepsRunning(t *testing.T) {
	b, fe := openFake(t)
	start := time.Now()
	if err := b.Start(ctxT(t), sandbox.Session{ID: "blk", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Start took %v; it must return when the session is ready", d)
	}
	if sup := b.supOf(); sup == nil || sup.hasExited() {
		t.Fatal("the supervisor must still be running after Start returned (it IS the session)")
	}
	if !fe.hasSocket("blk") {
		t.Fatal("no control socket for a started session")
	}
	// The supervisor was started verbose and with the documented arguments.
	calls, _ := os.ReadFile(filepath.Join(fe.state, "calls.log"))
	if !strings.Contains(string(calls), "run --detach -v --session-id blk --policy ") ||
		!strings.Contains(string(calls), "--cwd "+fe.cwd+" --network host") {
		t.Fatalf("hisn run invocation wrong:\n%s", calls)
	}
}

// ---- readiness: socket AND `hisn ls --session`, bounded, never silent success ----

func TestStartFailureModes(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		want       []string // all must appear in the error
		ready      time.Duration
	}{
		{"exits immediately with a diagnostic", "fail",
			[]string{"exited before the session was ready", "exit status 125", "injected failure: cannot create sandbox"}, 0},
		{"never creates the control socket", "nosock",
			[]string{"not ready after", "readiness pipe", "init pid", ".supervisor.log"}, 700 * time.Millisecond},
		{"hangs without ever being ready", "hang",
			[]string{"not ready after", "starting"}, 700 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fe := newFakeEnv(t)
			b, err := Open(ctxT(t), fe.options())
			if err != nil {
				t.Fatal(err)
			}
			if tc.ready > 0 {
				b.readyTimeout = tc.ready
			}
			b.stopWait = 2 * time.Second
			fe.setMode(tc.mode)
			err = b.Start(ctxT(t), sandbox.Session{ID: "fm1", HostCwd: fe.cwd})
			if err == nil {
				t.Fatal("a session that never became ready must fail Start (no silent success)")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error lacks %q:\n%v", w, err)
				}
			}
			if b.ActiveID() != "" {
				t.Error("a failed start must not leave a current session")
			}
			if _, e := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"true"}}); !errors.Is(e, sandbox.ErrNoSession) {
				t.Errorf("Exec after a failed start = %v, want ErrNoSession", e)
			}
			if len(fe.sessionIDs()) != 0 {
				t.Errorf("failed start left session dirs behind: %v", fe.sessionIDs())
			}
			if len(b.listRecordIDs()) != 0 {
				t.Errorf("failed start left an owner record: %v", b.listRecordIDs())
			}
			_ = b.Close(context.Background())
		})
	}
}

// The production bounds are real caps: readiness never polls longer than 10 s,
// and each stop phase waits at most 5 s.
func TestProductionCapsAreBounded(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := New(fe.options())
	if b.readyTimeout > 10*time.Second || b.readyTimeout <= 0 {
		t.Fatalf("readiness cap = %v, must be within (0, 10s]", b.readyTimeout)
	}
	if b.stopWait > 5*time.Second || b.stopWait <= 0 {
		t.Fatalf("stop cap = %v, must be within (0, 5s]", b.stopWait)
	}
	if supervisorStderrCap != 64<<10 {
		t.Fatalf("stderr cap = %d, want 64 KiB", supervisorStderrCap)
	}
}

// A slow-starting session (socket appears late) is accepted as soon as it is ready.
func TestStartWaitsForLateSocket(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	defer b.Close(context.Background())
	fe.setMode("late:600")
	start := time.Now()
	if err := b.Start(ctxT(t), sandbox.Session{ID: "late", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Fatal("Start returned before the control socket existed")
	}
	if out, info := run(t, b, "echo", "up"); strings.TrimSpace(out) != "up" || info.Code != 0 {
		t.Fatalf("late session not usable: %q %+v", out, info)
	}
}

// A control.sock that exists but nobody answers on is NOT ready: readiness needs
// `hisn ls --session` to succeed too.
func TestReadinessNeedsLsAnswerNotJustSocketFile(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	b.readyTimeout = 600 * time.Millisecond
	id := "sockfile"
	dir := filepath.Join(fe.rt(), id)
	_ = os.MkdirAll(dir, 0o700)
	_ = os.WriteFile(filepath.Join(dir, "control.sock"), nil, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "sup.pid"), []byte("1"), 0o600) // fake: ours, not live
	// Any long-running process stands in for the supervisor.
	sup, readyR, err := b.startSupervisor(filepath.Join(fe.sdir, "other.supervisor.log"), "", nil, "run", "--detach", "-v", "--session-id", "other", "--policy", "/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	_ = readyR.Close() // no "ready\n" will ever arrive: this supervisor has no socket
	err = b.waitReady(ctxT(t), id, sup, readyR)
	b.reapSupervisor(sup)
	if err == nil {
		t.Fatal("a stale socket file with no listener must not count as ready")
	}
}

// ---- stderr capture ----

func TestRingBufferKeepsTail(t *testing.T) {
	r := newRingBuffer(10)
	_, _ = r.Write([]byte("0123456789"))
	_, _ = r.Write([]byte("abcdef"))
	if got := r.String(); got != "6789abcdef" {
		t.Fatalf("ring = %q, want the last 10 bytes", got)
	}
	_, _ = r.Write([]byte(strings.Repeat("x", 100)))
	if got := r.String(); got != strings.Repeat("x", 10) {
		t.Fatalf("ring after a huge write = %q", got)
	}
}

// ---- Stop ----

func TestStopEndsSupervisorAndCleansUp(t *testing.T) {
	b, fe := openFake(t)
	_ = b.Start(ctxT(t), sandbox.Session{ID: "st", HostCwd: fe.cwd})
	sup := b.supOf()
	if err := b.Stop(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if !sup.hasExited() {
		t.Fatal("Stop returned while the supervisor was still running")
	}
	if len(fe.sessionIDs()) != 0 || len(b.listRecordIDs()) != 0 {
		t.Fatalf("leftovers: sessions=%v records=%v", fe.sessionIDs(), b.listRecordIDs())
	}
	if err := b.Stop(ctxT(t)); err != nil { // idempotent
		t.Fatalf("second Stop: %v", err)
	}
}

// A supervisor that ignores SIGTERM is SIGKILLed after the cap, and the id is
// reusable afterwards (the leftover runtime directory is removed).
func TestStopKillsStubbornSupervisorAndFreesTheID(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	b.stopWait = 500 * time.Millisecond
	fe.setMode("ignoreterm")
	if err := b.Start(ctxT(t), sandbox.Session{ID: "stub", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	sup := b.supOf()
	t0 := time.Now()
	if err := b.Stop(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if time.Since(t0) > 4*time.Second {
		t.Fatalf("Stop took %v with a 500ms cap", time.Since(t0))
	}
	if !sup.hasExited() || !strings.Contains(sup.exitStatus(), "killed") {
		t.Fatalf("supervisor not SIGKILLed: %s", sup.exitStatus())
	}
	if len(fe.sessionIDs()) != 0 {
		t.Fatalf("SIGKILLed supervisor's directory not cleaned: %v", fe.sessionIDs())
	}
	fe.setMode("")
	if err := b.Start(ctxT(t), sandbox.Session{ID: "stub", HostCwd: fe.cwd}); err != nil {
		t.Fatalf("id not reusable after a forced stop: %v", err)
	}
	_ = b.Close(context.Background())
}

// "session not found" from `hisn kill` is tolerated: the supervisor having
// exited is what counts.
func TestStopToleratesSessionNotFound(t *testing.T) {
	b, fe := openFake(t)
	b.stopWait = 400 * time.Millisecond
	_ = b.Start(ctxT(t), sandbox.Session{ID: "nf", HostCwd: fe.cwd})
	_ = os.RemoveAll(filepath.Join(fe.rt(), "nf")) // hisn can no longer find it
	if err := b.Stop(ctxT(t)); err != nil {
		t.Fatalf("Stop must tolerate a session hisn does not know: %v", err)
	}
	if len(b.listRecordIDs()) != 0 {
		t.Fatal("record kept for a session that is gone")
	}
}

// ---- health: a dead supervisor means a dead session ----

func TestExecAndGrantOnDeadSessionReturnTypedError(t *testing.T) {
	b, fe := openFake(t)
	_ = b.Start(ctxT(t), sandbox.Session{ID: "dd", HostCwd: fe.cwd})
	killSupervisorHard(t, b)

	_, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"true"}})
	if !errors.Is(err, sandbox.ErrSessionDead) {
		t.Fatalf("Exec on a dead session = %v, want ErrSessionDead", err)
	}
	if !strings.Contains(err.Error(), "signal: killed") || !strings.Contains(err.Error(), "stderr:") {
		t.Errorf("dead-session error lacks exit status / stderr: %v", err)
	}
	if err := b.Grant(ctxT(t), "/srv/x", false); !errors.Is(err, sandbox.ErrSessionDead) {
		t.Errorf("Grant on a dead session = %v, want ErrSessionDead", err)
	}
	if err := b.Revoke(ctxT(t), "/srv/x"); !errors.Is(err, sandbox.ErrSessionDead) {
		t.Errorf("Revoke on a dead session = %v, want ErrSessionDead", err)
	}
	// No hisn exec/grant/revoke client was even spawned for the dead session.
	calls, _ := os.ReadFile(filepath.Join(fe.state, "calls.log"))
	for _, c := range []string{"exec --session dd", "grant --session dd", "revoke --session dd"} {
		if strings.Contains(string(calls), c) {
			t.Errorf("a %q client was spawned against a dead session", c)
		}
	}
}

// A supervisor that exits by itself (not killed by us) is detected the same way,
// and its captured stderr is part of the error.
func TestSelfExitedSupervisorIsDeadWithStderr(t *testing.T) {
	b, fe := openFake(t)
	fe.setMode("crash:1500")
	if err := b.Start(ctxT(t), sandbox.Session{ID: "cr2", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	if !b.supOf().waitExit(5 * time.Second) {
		t.Fatal("injected crash did not happen")
	}
	_, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"true"}})
	if !errors.Is(err, sandbox.ErrSessionDead) ||
		!strings.Contains(err.Error(), "injected crash") || !strings.Contains(err.Error(), "exit status 125") {
		t.Fatalf("want ErrSessionDead with exit status and stderr, got: %v", err)
	}
}

func TestStartReplacesDeadSession(t *testing.T) {
	b, fe := openFake(t)
	_ = b.Start(ctxT(t), sandbox.Session{ID: "re", HostCwd: fe.cwd})
	e1 := b.Epoch()
	killSupervisorHard(t, b)
	// The same id, but the supervisor is dead: Start is NOT a no-op.
	if err := b.Start(ctxT(t), sandbox.Session{ID: "re", HostCwd: fe.cwd}); err != nil {
		t.Fatalf("restart after supervisor death: %v", err)
	}
	if b.Epoch() == e1 {
		t.Error("epoch must advance when a dead session is replaced")
	}
	if out, info := run(t, b, "echo", "again"); strings.TrimSpace(out) != "again" || info.Code != 0 {
		t.Fatalf("fresh session unusable: %q %+v", out, info)
	}
}

// Dead supervisor + orphan reconcile stays a fallback that works.
func TestReconcileAfterSupervisorDeathDropsRecord(t *testing.T) {
	b, fe := openFake(t)
	_ = b.Start(ctxT(t), sandbox.Session{ID: "rc", HostCwd: fe.cwd})
	killSupervisorHard(t, b)
	rec, _ := b.readRecord("rc")
	rec.Pid, rec.PidStart = 999999, ""
	_ = b.rewriteRecord(rec)
	b2, _ := New(fe.options())
	_ = b2.Reconcile(ctxT(t))
	if len(b2.listRecordIDs()) != 0 {
		t.Fatalf("record for a dead session not dropped: %v", b2.listRecordIDs())
	}
}

// ---- Pdeathsig, behaviourally: the supervisor dies with its Wakil ----

const parentHelperEnv = "HISN_PARENT_HELPER"

type parentHelperCfg struct {
	Options Options
	ID      string
}

// parentHelperMain runs in a re-executed child "Wakil": start a session, report
// the supervisor pid, then exit WITHOUT stopping anything (a crash).
func parentHelperMain() int {
	var cfg parentHelperCfg
	if err := json.Unmarshal([]byte(os.Getenv(parentHelperEnv)), &cfg); err != nil {
		return 2
	}
	b, err := New(cfg.Options)
	if err != nil {
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := b.Start(ctx, sandbox.Session{ID: cfg.ID, HostCwd: cfg.Options.HostCwd}); err != nil {
		_, _ = os.Stderr.WriteString("helper start: " + err.Error() + "\n")
		return 3
	}
	_, _ = os.Stdout.WriteString("started " + strconv.Itoa(b.supOf().pid()) + "\n")
	return 0 // no Stop, no Close: the process just ends
}

func TestSupervisorDiesWithParent(t *testing.T) {
	fe := newFakeEnv(t)
	cfg, _ := json.Marshal(parentHelperCfg{Options: fe.options(), ID: "orph"})
	self, _ := os.Executable()
	cmd := osexec.Command(self)
	cmd.Env = []string{parentHelperEnv + "=" + string(cfg), "PATH=" + os.Getenv("PATH")}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	_ = cmd.Wait()
	if !strings.HasPrefix(line, "started ") {
		t.Fatalf("helper did not start a session: %q %s", line, stderr.String())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if len(fe.sessionIDs()) == 0 {
			return // the supervisor was TERMed by the kernel and removed its directory
		}
		if time.Now().After(deadline) {
			t.Fatalf("the supervisor outlived its parent: sessions %v", fe.sessionIDs())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// ---- round-1 review: death in flight, foreign directories, hung kill client ----

// The supervisor dies while an exec is in flight: Wait reports the SESSION's
// death (typed, with stderr), never "Lost: may still be running".
func TestInFlightExecOnSupervisorDeathReportsSessionDead(t *testing.T) {
	b, fe := openFake(t)
	_ = b.Start(ctxT(t), sandbox.Session{ID: "fl", HostCwd: fe.cwd})
	h, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	type res struct {
		info sandbox.ExitInfo
		err  error
	}
	done := make(chan res, 1)
	go func() { i, e := h.Wait(); done <- res{i, e} }()
	time.Sleep(300 * time.Millisecond)
	killSupervisorHard(t, b)
	select {
	case r := <-done:
		if !errors.Is(r.err, sandbox.ErrSessionDead) {
			t.Fatalf("in-flight exec on a dead session: err=%v info=%+v, want ErrSessionDead", r.err, r.info)
		}
		if r.info.Lost {
			t.Error("a program of a dead sandbox must not be reported as possibly still running")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("an exec on a dead session never ended")
	}
}

// Death between admission and the client spawn: typed, and no client is spawned.
func TestDeathBetweenAdmissionAndSpawnIsTyped(t *testing.T) {
	b, fe := openFake(t)
	_ = b.Start(ctxT(t), sandbox.Session{ID: "bt", HostCwd: fe.cwd})
	l, release, err := b.admit()
	if err != nil {
		t.Fatal(err)
	}
	release()
	killSupervisorHard(t, b)
	deadline := time.Now().Add(5 * time.Second)
	for l.ctx.Err() == nil && time.Now().Before(deadline) { // supervisor exit cancels l.ctx
		time.Sleep(10 * time.Millisecond)
	}
	if l.ctx.Err() == nil {
		t.Fatal("supervisor death must cancel the session context")
	}
	_, err = b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"true"}})
	if !errors.Is(err, sandbox.ErrSessionDead) {
		t.Fatalf("Exec = %v, want ErrSessionDead", err)
	}
}

// A program that finished normally keeps its own status even if the session
// dies right afterwards.
func TestFinishedExecKeepsItsStatusAfterDeath(t *testing.T) {
	b, fe := openFake(t)
	_ = b.Start(ctxT(t), sandbox.Session{ID: "fk", HostCwd: fe.cwd})
	h, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"sh", "-c", "exit 3"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond) // the client has exited with 3
	killSupervisorHard(t, b)
	info, err := h.Wait()
	if err != nil || info.Code != 3 {
		t.Fatalf("finished exec lost its status: %+v %v", info, err)
	}
}

// 0.2.1: dir removal is hisn's (`ls --prune`, stale-kill, run-replaces-stale),
// so Wakil no longer removes directories by hand. What remains to verify:
// Stop never signals a session id that a FOREIGN session owns (a live socket
// after our supervisor is reaped is a replacement: left alone), and prune (the
// fake's) only removes dirs with a dead supervisor.
func TestStopNeverSignalsForeignReplacement(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	b.stopWait = 500 * time.Millisecond
	if err := b.Start(ctxT(t), sandbox.Session{ID: "frn", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	sup := b.supOf()
	_ = syscall.Kill(sup.pid(), syscall.SIGKILL) // our supervisor dies, dir stays
	if !sup.waitExit(5 * time.Second) {
		t.Fatal("supervisor did not die")
	}
	// A foreign fixture with a LIVE socket under the same id: stopLocked must
	// see `live` after our supervisor is reaped and NOT kill it. The fake
	// supervisor's socket file still exists (SIGKILL leaves it), so plant the
	// foreign listener on a socket file we create fresh after removing the old.
	fdir := filepath.Join(fe.rt(), "frn")
	fsock := filepath.Join(fdir, "control.sock")
	_ = os.Remove(fsock) // the SIGKILLed supervisor's leftover file
	fln, err := net.Listen("unix", fsock)
	if err != nil {
		t.Fatal(err)
	}
	defer fln.Close()
	go func() {
		for {
			c, err := fln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	_ = b.Stop(ctxT(t))
	if _, err := os.Stat(fsock); err != nil {
		t.Fatal("Wakil removed or disturbed a foreign session directory")
	}
}

// A hung `hisn kill` client cannot stop Stop from escalating to SIGKILL within
// the bound, even with a context that never expires.
func TestHungKillClientDoesNotBlockStop(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	b.stopWait = 500 * time.Millisecond
	_ = b.Start(context.Background(), sandbox.Session{ID: "hk", HostCwd: fe.cwd})
	_ = os.WriteFile(filepath.Join(fe.state, "kill_hang"), nil, 0o600)
	sup := b.supOf()
	done := make(chan error, 1)
	go func() { done <- b.Stop(context.Background()) }() // no deadline at all
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a hung kill client blocked Stop")
	}
	if !sup.hasExited() {
		t.Fatal("supervisor still running after Stop")
	}
}

// Stop works with an already-cancelled caller context (shutdown paths).
func TestStopWithCancelledContext(t *testing.T) {
	b, fe := openFake(t)
	_ = b.Start(ctxT(t), sandbox.Session{ID: "cc", HostCwd: fe.cwd})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sup := b.supOf()
	if err := b.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if !sup.hasExited() || len(fe.sessionIDs()) != 0 {
		t.Fatal("Stop with a cancelled ctx did not end the session")
	}
}

// A record that belongs to another instance does not stop Wakil from reaping
// the supervisor it spawned itself.
func TestStopReapsOwnSupervisorEvenIfRecordIsForeign(t *testing.T) {
	b, fe := openFake(t)
	_ = b.Start(ctxT(t), sandbox.Session{ID: "fr", HostCwd: fe.cwd})
	rec, _ := b.readRecord("fr")
	rec.Token = newToken()
	_ = b.rewriteRecord(rec)
	sup := b.supOf()
	if err := b.Stop(ctxT(t)); err == nil {
		t.Fatal("a foreign record must still be reported")
	}
	if !sup.hasExited() {
		t.Fatal("Wakil left its own supervisor running because of a record mismatch")
	}
	rec.Token = ""
	_ = os.Remove(b.recordPath("fr"))
	b.mu.Lock()
	b.orphans = nil
	b.mu.Unlock()
}

// ================= strict readiness, log file, lifetime, stale handling =================

func readLog(t *testing.T, b *Backend, id string) string {
	t.Helper()
	data, err := os.ReadFile(b.supervisorLogPath(id))
	if err != nil {
		t.Fatalf("supervisor log missing: %v", err)
	}
	return string(data)
}

// `stale` is not ready, ever: the supervisor runs, the socket FILE exists, but
// nothing accepts, so `hisn ls` says stale and Start must fail with the log path.
func TestReadinessRejectsStale(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	b.readyTimeout = 700 * time.Millisecond
	b.stopWait = 2 * time.Second
	fe.setMode("stale")
	err := b.Start(ctxT(t), sandbox.Session{ID: "stl", HostCwd: fe.cwd})
	if err == nil {
		t.Fatal("a session that `hisn ls` reports stale must NOT be ready")
	}
	for _, w := range []string{"not ready after", "hisn ls reports the session stale", "stl.supervisor.log", "init pid"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error lacks %q:\n%v", w, err)
		}
	}
	if b.ActiveID() != "" || len(b.listRecordIDs()) != 0 || len(fe.sessionIDs()) != 0 {
		t.Errorf("failed start left state: active=%q records=%v sessions=%v", b.ActiveID(), b.listRecordIDs(), fe.sessionIDs())
	}
}

// A supervisor that creates its socket and dies right after (tonight's fault)
// must not pass as ready, and the failure carries its stderr and exit status.
func TestReadinessRejectsDiesAfterSocket(t *testing.T) {
	for _, mode := range []string{"crash:0", "crash:40"} {
		t.Run(mode, func(t *testing.T) {
			fe := newFakeEnv(t)
			b, _ := Open(ctxT(t), fe.options())
			b.stopWait = 2 * time.Second
			fe.setMode(mode)
			err := b.Start(ctxT(t), sandbox.Session{ID: "das", HostCwd: fe.cwd})
			if err == nil {
				t.Fatal("a supervisor that died right after creating its socket passed as ready")
			}
			for _, w := range []string{"exit status 125", "injected crash", ".supervisor.log"} {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error lacks %q:\n%v", w, err)
				}
			}
			if b.ActiveID() != "" {
				t.Error("a failed start must not leave a current session")
			}
			if lg := readLog(t, b, "das"); !strings.Contains(lg, "injected crash") || !strings.Contains(lg, "supervisor exited: exit status 125") {
				t.Errorf("log lacks the supervisor's stderr / exit:\n%s", lg)
			}
		})
	}
}

// Supervisor stderr is written to <state>/<chat>.supervisor.log on EVERY start,
// success included, truncated per start, and error messages name the path.
func TestSupervisorLogFileEveryStartTruncated(t *testing.T) {
	b, fe := openFake(t)
	b.stopWait = 2 * time.Second
	want := filepath.Join(fe.sdir, "lg.supervisor.log")
	if got := b.supervisorLogPath("lg"); got != want {
		t.Fatalf("log path = %q, want %q", got, want)
	}
	if err := b.Start(ctxT(t), sandbox.Session{ID: "lg", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	first := readLog(t, b, "lg")
	if !strings.Contains(first, "wakil: starting ") || !strings.Contains(first, "run --detach -v --session-id lg") ||
		!strings.Contains(first, "init pid") {
		t.Fatalf("log of a SUCCESSFUL start lacks the supervisor's output:\n%s", first)
	}
	if err := b.Stop(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(ctxT(t), sandbox.Session{ID: "lg", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	second := readLog(t, b, "lg")
	if strings.Count(second, "wakil: starting ") != 1 {
		t.Fatalf("log not truncated per start:\n%s", second)
	}
	// The per-process ring buffer is a second copy of the same output.
	if !strings.Contains(b.supOf().stderrText(), "init pid") {
		t.Error("ring buffer lost the supervisor's stderr")
	}
	// The log is private.
	if fi, err := os.Stat(b.supervisorLogPath("lg")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("log mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	// A dead session's error names the log too.
	killSupervisorHard(t, b)
	_, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("dead-session error must name the log %s: %v", want, err)
	}
}

// A log that cannot be created is an error, never a silent loss of diagnostics.
func TestUnwritableSupervisorLogFailsStart(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	_ = os.MkdirAll(b.opts.StateDir, 0o700)
	// A directory where the log file must go.
	if err := os.MkdirAll(b.supervisorLogPath("nolog"), 0o700); err != nil {
		t.Fatal(err)
	}
	err := b.Start(ctxT(t), sandbox.Session{ID: "nolog", HostCwd: fe.cwd})
	if err == nil || !strings.Contains(err.Error(), "supervisor log") {
		t.Fatalf("want a supervisor-log error, got %v", err)
	}
	if len(fe.sessionIDs()) != 0 || len(b.listRecordIDs()) != 0 {
		t.Fatalf("failed start left state: %v %v", fe.sessionIDs(), b.listRecordIDs())
	}
}

// ---- lifetime: owned by the session, never by a tool call's context ----

func TestSupervisorSurvivesStartContextCancel(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	defer b.Close(context.Background())
	ctx, cancel := context.WithCancel(context.Background())
	if err := b.Start(ctx, sandbox.Session{ID: "ctxc", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	cancel() // the tool call that started the session ends
	time.Sleep(500 * time.Millisecond)
	if b.supOf().hasExited() {
		t.Fatalf("cancelling the starting call's context killed the supervisor: %s\n%s",
			b.supOf().exitStatus(), readLog(t, b, "ctxc"))
	}
	if out, info := run(t, b, "echo", "still-here"); strings.TrimSpace(out) != "still-here" || info.Code != 0 {
		t.Fatalf("exec after the start context was cancelled: %q %+v", out, info)
	}
}

// Even a start whose context is ALREADY cancelled when readiness begins must not
// leave the supervisor tied to it: the failure is clean and nothing leaks.
func TestStartWithCancelledContextFailsCleanly(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	b.stopWait = 2 * time.Second
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Start(ctx, sandbox.Session{ID: "cc", HostCwd: fe.cwd}); err == nil {
		t.Fatal("Start with a cancelled context must fail")
	}
	if len(fe.sessionIDs()) != 0 || len(b.listRecordIDs()) != 0 || b.ActiveID() != "" {
		t.Fatalf("leftovers: %v %v %q", fe.sessionIDs(), b.listRecordIDs(), b.ActiveID())
	}
}

// Stop is what ends the supervisor; the session-owned context is cancelled only
// by that teardown.
func TestStopCancelsSessionOwnedContext(t *testing.T) {
	b, fe := openFake(t)
	_ = b.Start(ctxT(t), sandbox.Session{ID: "own", HostCwd: fe.cwd})
	sup := b.supOf()
	if sup.cmd.Process == nil {
		t.Fatal("no process")
	}
	_ = b.Stop(ctxT(t))
	if !sup.hasExited() {
		t.Fatal("Stop left the supervisor running")
	}
}

// stdin and stdout are /dev/null: no descriptor of Wakil's terminal can reach hisn.
func TestSupervisorStdioIsDevNull(t *testing.T) {
	b, fe := openFake(t)
	_ = b.Start(ctxT(t), sandbox.Session{ID: "io", HostCwd: fe.cwd})
	sup := b.supOf()
	if sup.cmd.Stdin != nil || sup.cmd.Stdout != nil {
		t.Fatalf("stdin/stdout must be nil (/dev/null): %v %v", sup.cmd.Stdin, sup.cmd.Stdout)
	}
	if sup.cmd.Stderr == nil {
		t.Fatal("stderr must be the diagnostics pipe")
	}
	for _, fd := range []string{"0", "1"} {
		l, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(sup.pid()), "fd", fd))
		if err != nil {
			t.Skip("no /proc/<pid>/fd")
		}
		if l != "/dev/null" {
			t.Errorf("supervisor fd %s -> %s, want /dev/null", fd, l)
		}
	}
	if a := supervisorSysProcAttr(); a == nil || !a.Setsid {
		t.Error("the supervisor must run in its own session (no controlling terminal)")
	}
}

// ---- stale handling ----

func plantStale(t *testing.T, fe *fakeEnv, id string) string {
	t.Helper()
	dir := filepath.Join(fe.rt(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "sup.pid"), []byte("1"), 0o600) // fake: a supervisor session that is not live
	_ = os.WriteFile(filepath.Join(dir, "control.sock"), nil, 0o600)
	return dir
}

func TestStartReplacesStaleDirOfThisChat(t *testing.T) {
	var diag strings.Builder
	old := errWriter
	SetDiagnostics(&diag)
	t.Cleanup(func() { SetDiagnostics(old) })
	b, fe := openFake(t)
	stale := plantStale(t, fe, "chat-s")
	if err := b.Start(ctxT(t), sandbox.Session{ID: "chat-s", HostCwd: fe.cwd}); err != nil {
		t.Fatalf("a stale directory of this chat blocked the start: %v", err)
	}
	if _, err := os.Stat(filepath.Join(stale, "policy")); err != nil {
		t.Fatal("the fresh session did not take the id")
	}
	if !strings.Contains(diag.String(), "replacing stale session directory") {
		t.Errorf("no one-line notice: %q", diag.String())
	}
	if !strings.Contains(readLog(t, b, "chat-s"), "replacing stale session directory") {
		t.Error("the replacement is not recorded in the supervisor log")
	}
	if out, info := run(t, b, "echo", "ok"); strings.TrimSpace(out) != "ok" || info.Code != 0 {
		t.Fatalf("replacement session unusable: %q %+v", out, info)
	}
}

func TestStaleReplacementNeverTouchesLiveForeignOrOtherIDs(t *testing.T) {
	b, fe := openFake(t)
	fe.addForeignSession("chat-live") // live in the fake (no supervisor pid file)
	err := b.Start(ctxT(t), sandbox.Session{ID: "chat-live", HostCwd: fe.cwd})
	if !errors.Is(err, sandbox.ErrSessionInUse) || !strings.Contains(err.Error(), "LIVE") {
		t.Fatalf("a live foreign session must be refused: %v", err)
	}
	other := plantStale(t, fe, "someone-elses-stale")
	_ = b.Start(ctxT(t), sandbox.Session{ID: "chat-other", HostCwd: fe.cwd})
	if _, err := os.Stat(other); err != nil {
		t.Error("a stale directory of ANOTHER id was removed")
	}
	if !contains(fe.sessionIDs(), "chat-live") {
		t.Error("the live foreign session was removed")
	}
}

// A stale directory is not replaced while a live Wakil owns the chat.
func TestStaleReplacementRespectsLiveOwner(t *testing.T) {
	fe := newFakeEnv(t)
	b1, _ := Open(ctxT(t), fe.options())
	defer b1.Close(context.Background())
	_ = b1.Start(ctxT(t), sandbox.Session{ID: "own2", HostCwd: fe.cwd})
	_ = os.WriteFile(filepath.Join(fe.rt(), "own2", "refuse"), nil, 0o600) // now stale
	b2, _ := New(fe.options())
	if err := b2.Start(ctxT(t), sandbox.Session{ID: "own2", HostCwd: fe.cwd}); !errors.Is(err, sandbox.ErrSessionInUse) {
		t.Fatalf("a second Wakil must not replace a chat owned by a live one: %v", err)
	}
}

// ECONNREFUSED from a hisn client means dead: the call fails typed (the program
// did not run), and the next Start replaces the session without ever failing
// with "previous session not stopped".
func TestConnectionRefusedMeansDeadAndIsReplaced(t *testing.T) {
	b, fe := openFake(t)
	b.stopWait = 500 * time.Millisecond
	_ = b.Start(ctxT(t), sandbox.Session{ID: "ref", HostCwd: fe.cwd})
	// The supervisor process is alive but its socket no longer accepts.
	_ = os.WriteFile(filepath.Join(fe.rt(), "ref", "refuse"), nil, 0o600)
	var out bytes.Buffer
	h, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"echo", "must-not-run"}, Stdout: &out})
	if err != nil {
		t.Fatalf("admission of a still-running supervisor: %v", err)
	}
	info, werr := h.Wait()
	if !errors.Is(werr, sandbox.ErrSessionDead) {
		t.Fatalf("exec on a refusing socket = %+v, %v; want ErrSessionDead", info, werr)
	}
	if strings.Contains(out.String(), "must-not-run") {
		t.Fatal("the program ran")
	}
	// Grant on the same dead session is a typed error without a new client.
	if err := b.Grant(ctxT(t), "/srv/x", false); !errors.Is(err, sandbox.ErrSessionDead) {
		t.Errorf("Grant after a refused exec = %v, want ErrSessionDead", err)
	}
	// Start replaces it: same chat, fresh supervisor, no error.
	if err := b.Start(ctxT(t), sandbox.Session{ID: "ref", HostCwd: fe.cwd}); err != nil {
		t.Fatalf("replacing a dead session failed: %v", err)
	}
	if o, info := run(t, b, "echo", "fresh"); strings.TrimSpace(o) != "fresh" || info.Code != 0 {
		t.Fatalf("fresh session unusable: %q %+v", o, info)
	}
}

// Start replaces a dead session: the active grants must be replayed on the
// replacement so a stale-session replacement does not silently drop access.
func TestStartReplaysGrantsOnReplacement(t *testing.T) {
	fe := newFakeEnv(t)
	opts := fe.options()
	opts.GrantableRW = []string{"/srv/out"}
	b, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Preflight(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(ctxT(t), sandbox.Session{ID: "replay", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	// Grant a ro path and a rw path.
	if err := b.Grant(ctxT(t), "/srv/ref", false); err != nil {
		t.Fatal(err)
	}
	if err := b.Grant(ctxT(t), "/srv/out/sub", true); err != nil {
		t.Fatal(err)
	}
	// Verify the grants are in the path map.
	if _, err := b.pm.MapPath("/srv/ref/x"); err != nil {
		t.Fatalf("ro grant not in path map: %v", err)
	}
	if _, err := b.pm.MapPath("/srv/out/sub/x"); err != nil {
		t.Fatalf("rw grant not in path map: %v", err)
	}
	// Kill the supervisor → session is dead.
	killSupervisorHard(t, b)
	// Start replaces the dead session; grants must be replayed.
	if err := b.Start(ctxT(t), sandbox.Session{ID: "replay", HostCwd: fe.cwd}); err != nil {
		t.Fatalf("replacement start failed: %v", err)
	}
	// The grants must survive the replacement.
	if _, err := b.pm.MapPath("/srv/ref/x"); err != nil {
		t.Errorf("ro grant lost on replacement: %v", err)
	}
	if _, err := b.pm.MapPath("/srv/out/sub/x"); err != nil {
		t.Errorf("rw grant lost on replacement: %v", err)
	}
}

// Start never fails because the PREVIOUS session would not stop.
func TestStartNeverFailsOnUnstoppablePreviousSession(t *testing.T) {
	var diag strings.Builder
	old := errWriter
	SetDiagnostics(&diag)
	t.Cleanup(func() { SetDiagnostics(old) })
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	b.stopWait = 300 * time.Millisecond
	fe.setMode("ignoreterm")
	_ = b.Start(ctxT(t), sandbox.Session{ID: "prev", HostCwd: fe.cwd})
	prev := b.supOf()
	prev.killFn = func() {} // cannot be killed
	fe.setMode("")
	if err := b.Start(ctxT(t), sandbox.Session{ID: "next", HostCwd: fe.cwd}); err != nil {
		t.Fatalf("an unstoppable previous session failed a new start: %v", err)
	}
	if !strings.Contains(diag.String(), "previous session was not fully stopped") {
		t.Errorf("no one-line notice: %q", diag.String())
	}
	if b.ActiveID() != "next" {
		t.Fatalf("active = %q", b.ActiveID())
	}
	b.mu.RLock()
	tracked := len(b.orphans)
	b.mu.RUnlock()
	if tracked != 1 {
		t.Errorf("the unstoppable session must stay tracked for retry: %d", tracked)
	}
	prev.killFn = nil
	if err := b.Close(ctxT(t)); err != nil {
		t.Fatalf("Close must finish the stranded cleanup: %v", err)
	}
}

// Stop ignores "not found"/"refused" from kill and removes what is provably ours.
func TestStopIgnoresRefusedAndRemovesDir(t *testing.T) {
	b, fe := openFake(t)
	b.stopWait = 500 * time.Millisecond
	_ = b.Start(ctxT(t), sandbox.Session{ID: "sr", HostCwd: fe.cwd})
	_ = os.WriteFile(filepath.Join(fe.rt(), "sr", "refuse"), nil, 0o600) // kill gets "refused"
	if err := b.Stop(ctxT(t)); err != nil {
		t.Fatalf("Stop must tolerate a refusing socket: %v", err)
	}
	if len(fe.sessionIDs()) != 0 || len(b.listRecordIDs()) != 0 {
		t.Fatalf("leftovers: %v %v", fe.sessionIDs(), b.listRecordIDs())
	}
}

// ---- review round 1 regressions ----

// PROGRAM stderr must never be able to declare the session dead: a program that
// prints hisn's refusal text and exits 125 leaves a healthy session healthy.
func TestProgramStderrCannotMarkSessionDead(t *testing.T) {
	b, fe := openFake(t)
	_ = b.Start(ctxT(t), sandbox.Session{ID: "liar", HostCwd: fe.cwd})
	_, info := run(t, b, "sh", "-c", `echo "hisn: connecting to control socket: Connection refused (os error 111)" >&2; exit 125`)
	if info.Code != 125 {
		t.Fatalf("program exit not propagated: %+v", info)
	}
	b.mu.RLock()
	l := b.cur
	b.mu.RUnlock()
	if l == nil || l.dead() {
		t.Fatal("a program's stderr poisoned a healthy session")
	}
	if out, info := run(t, b, "echo", "still-fine"); strings.TrimSpace(out) != "still-fine" || info.Code != 0 {
		t.Fatalf("session unusable after a lying program: %q %+v", out, info)
	}
	// The same session, genuinely refusing, IS declared dead (see
	// TestConnectionRefusedMeansDeadAndIsReplaced): only the probe decides.
}

// A stalled supervisor log (a FIFO nobody reads) must neither wedge hisn's stderr
// nor delay publishing its death or the cleanup that depends on it.
func TestStalledLogDoesNotWedgeSupervisorNorDelayCleanup(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	b.stopWait = 2 * time.Second
	logPath := b.supervisorLogPath("jam")
	_ = os.MkdirAll(filepath.Dir(logPath), 0o700)
	if err := syscall.Mkfifo(logPath, 0o600); err != nil {
		t.Skipf("no fifo: %v", err)
	}
	rd, err := os.OpenFile(logPath, os.O_RDONLY|syscall.O_NONBLOCK, 0) // holds it open, never reads
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	fe.setMode("flood") // floods stderr (far beyond the pipe buffer), then exits 125
	t0 := time.Now()
	err = b.Start(ctxT(t), sandbox.Session{ID: "jam", HostCwd: fe.cwd})
	if err == nil {
		t.Fatal("a flooding, dying supervisor must fail Start")
	}
	if !strings.Contains(err.Error(), "exit status 125") {
		t.Errorf("the supervisor was wedged by the stalled log instead of exiting: %v", err)
	}
	if d := time.Since(t0); d > 15*time.Second {
		t.Fatalf("Start with a stalled log took %v", d)
	}
	if b.ActiveID() != "" || len(b.listRecordIDs()) != 0 {
		t.Errorf("failed start left state: %q %v", b.ActiveID(), b.listRecordIDs())
	}
}

// A stale directory whose resolved policy names no Wakil instance belongs to
// somebody else, whatever its id: never replaced.
// 0.2.1: `hisn run --session-id` replaces a provably STALE directory itself
// (socket refused AND supervisor pid gone; hisn's own check, fd-relative,
// runtime-root confined) — Wakil no longer refuses on policy ownership, the
// token check died with the hand-rolled dir removal. A stale dir of another
// id is still never touched by a Start for OUR id.
func TestStaleForeignPolicyDirNotDeleted(t *testing.T) {
	b, fe := openFake(t)
	dir := plantStale(t, fe, "chat-fp")
	_ = os.WriteFile(filepath.Join(dir, "policy.resolved.toml"), []byte("hostname = 'someone-else'\n"), 0o600)
	// 0.2.1: the stale dir (whoever made it) is replaced by `hisn run`.
	if err := b.Start(ctxT(t), sandbox.Session{ID: "chat-fp", HostCwd: fe.cwd}); err != nil {
		t.Fatalf("a provably stale directory must be replaced by hisn itself: %v", err)
	}
	if out, info := run(t, b, "echo", "ok"); strings.TrimSpace(out) != "ok" || info.Code != 0 {
		t.Fatalf("replacement session unusable: %q %+v", out, info)
	}
}

// A grant/revoke client that is refused marks the session dead (its output is
// hisn's own, not a program's), so the next Start replaces it.
func TestRefusedGrantAndRevokeMarkSessionDead(t *testing.T) {
	for _, op := range []string{"grant", "revoke"} {
		t.Run(op, func(t *testing.T) {
			b, fe := openFake(t)
			b.stopWait = 500 * time.Millisecond
			_ = b.Start(ctxT(t), sandbox.Session{ID: "gr-" + op, HostCwd: fe.cwd})
			_ = os.WriteFile(filepath.Join(fe.rt(), "gr-"+op, "refuse"), nil, 0o600)
			var err error
			if op == "grant" {
				err = b.Grant(ctxT(t), "/srv/x", false)
			} else {
				err = b.Revoke(ctxT(t), "/srv/x")
			}
			if !errors.Is(err, sandbox.ErrSessionDead) {
				t.Fatalf("%s on a refusing socket = %v, want ErrSessionDead", op, err)
			}
			b.mu.RLock()
			dead := b.cur.dead()
			b.mu.RUnlock()
			if !dead {
				t.Fatal("the session was not marked dead")
			}
			if _, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"true"}}); !errors.Is(err, sandbox.ErrSessionDead) {
				t.Errorf("Exec after a refused %s = %v, want ErrSessionDead", op, err)
			}
		})
	}
}
