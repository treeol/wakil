package hisn

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
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
			[]string{"not ready after", "control socket not created yet", "stderr:", "init pid"}, 700 * time.Millisecond},
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
	sup, err := b.startSupervisor("run", "--detach", "-v", "--session-id", "other", "--policy", "/nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	err = b.waitReady(ctxT(t), id, sup)
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

// Stop and failed-start cleanup only remove a directory carrying OUR token. A
// foreign same-id directory (no socket) in either runtime root survives.
func TestCleanupNeverRemovesForeignDirectory(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	foreign := filepath.Join(fe.rt(), "frn")
	_ = os.MkdirAll(foreign, 0o700)
	_ = os.WriteFile(filepath.Join(foreign, "policy.resolved.toml"), []byte(`hostname = "someone-else"`), 0o600)
	b.removeOwnSessionDir("frn", newToken())
	if _, err := os.Stat(foreign); err != nil {
		t.Fatal("a directory without our instance token was removed")
	}
	nopol := filepath.Join(fe.rt(), "nopol") // started, policy not written yet
	_ = os.MkdirAll(nopol, 0o700)
	b.removeOwnSessionDir("nopol", newToken())
	if _, err := os.Stat(nopol); err != nil {
		t.Fatal("a directory without a resolved policy was removed")
	}
	tok := newToken() // ours, leftover of a SIGKILLed supervisor: removed
	mine := filepath.Join(fe.rt(), "mine")
	_ = os.MkdirAll(mine, 0o700)
	_ = os.WriteFile(filepath.Join(mine, "policy.resolved.toml"), []byte(`hostname = "`+hostnameFor(tok)+`"`), 0o600)
	b.removeOwnSessionDir("mine", tok)
	if _, err := os.Stat(mine); err == nil {
		t.Fatal("our own leftover directory was not removed")
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
