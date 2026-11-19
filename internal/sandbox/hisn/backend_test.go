package hisn

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/sandbox"
)

func ctxT(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func openFake(t *testing.T) (*Backend, *fakeEnv) {
	t.Helper()
	fe := newFakeEnv(t)
	b, err := Open(ctxT(t), fe.options())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return b, fe
}

func run(t *testing.T, b *Backend, argv ...string) (string, sandbox.ExitInfo) {
	t.Helper()
	var out bytes.Buffer
	h, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: argv, Stdout: &out})
	if err != nil {
		t.Fatalf("exec %v: %v", argv, err)
	}
	info, err := h.Wait()
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), info
}

func TestPreflight(t *testing.T) {
	t.Run("ok", func(t *testing.T) {
		fe := newFakeEnv(t)
		if _, err := Open(ctxT(t), fe.options()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("too old", func(t *testing.T) {
		fe := newFakeEnv(t)
		_ = os.WriteFile(filepath.Join(fe.state, "version"), []byte("0.1.9"), 0o600)
		_, err := Open(ctxT(t), fe.options())
		if err == nil || !strings.Contains(err.Error(), "too old") {
			t.Fatalf("want too-old error, got %v", err)
		}
	})
	t.Run("newer ok", func(t *testing.T) {
		fe := newFakeEnv(t)
		_ = os.WriteFile(filepath.Join(fe.state, "version"), []byte("0.10.0"), 0o600)
		if _, err := Open(ctxT(t), fe.options()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("check fails", func(t *testing.T) {
		fe := newFakeEnv(t)
		_ = os.WriteFile(filepath.Join(fe.state, "check_fail"), nil, 0o600)
		_, err := Open(ctxT(t), fe.options())
		if err == nil || !strings.Contains(err.Error(), "cannot run hisn sessions") {
			t.Fatalf("want check failure, got %v", err)
		}
	})
	t.Run("missing binary", func(t *testing.T) {
		fe := newFakeEnv(t)
		o := fe.options()
		o.Binary = filepath.Join(t.TempDir(), "nope")
		if _, err := Open(ctxT(t), o); err == nil {
			t.Fatal("missing hisn must be an error, never a fallback")
		}
	})
	t.Run("ssh_agent unset", func(t *testing.T) {
		fe := newFakeEnv(t)
		_ = os.WriteFile(filepath.Join(fe.state, "version"), []byte("0.6.1"), 0o600)
		o := fe.options()
		o.SSHAgent = true
		o.SSHKeys = []string{"SHA256:abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"}
		t.Setenv("SSH_AUTH_SOCK", "")
		_, err := Open(ctxT(t), o)
		if err == nil || !strings.Contains(err.Error(), "ssh_agent") || !strings.Contains(err.Error(), "SSH_AUTH_SOCK") {
			t.Fatalf("want ssh_agent/SSH_AUTH_SOCK error, got %v", err)
		}
	})
	t.Run("ssh_agent not a socket", func(t *testing.T) {
		fe := newFakeEnv(t)
		_ = os.WriteFile(filepath.Join(fe.state, "version"), []byte("0.6.1"), 0o600)
		o := fe.options()
		o.SSHAgent = true
		o.SSHKeys = []string{"SHA256:abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"}
		// Point SSH_AUTH_SOCK at a regular file, not a socket.
		notSock := filepath.Join(t.TempDir(), "not-a-socket")
		_ = os.WriteFile(notSock, []byte("x"), 0o600)
		t.Setenv("SSH_AUTH_SOCK", notSock)
		_, err := Open(ctxT(t), o)
		if err == nil || !strings.Contains(err.Error(), "not a socket") {
			t.Fatalf("want not-a-socket error, got %v", err)
		}
	})
	t.Run("ssh_agent ok", func(t *testing.T) {
		fe := newFakeEnv(t)
		_ = os.WriteFile(filepath.Join(fe.state, "version"), []byte("0.6.1"), 0o600)
		o := fe.options()
		o.SSHAgent = true
		o.SSHKeys = []string{"SHA256:abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"}
		// Create a real Unix socket.
		sock := filepath.Join(t.TempDir(), "agent.sock")
		_ = os.Remove(sock)
		l, err := net.Listen("unix", sock)
		if err != nil {
			t.Skipf("cannot create unix socket: %v", err)
		}
		defer l.Close()
		t.Setenv("SSH_AUTH_SOCK", sock)
		if _, err := Open(ctxT(t), o); err != nil {
			t.Fatalf("ssh_agent with a valid socket must pass preflight: %v", err)
		}
	})
}

func TestParseVersion(t *testing.T) {
	v, err := ParseVersion("hisn 0.2.0\n")
	if err != nil || v != [3]int{0, 2, 0} {
		t.Fatalf("%v %v", v, err)
	}
	if _, err := ParseVersion("garbage"); err == nil {
		t.Error("garbage must not parse")
	}
	if !VersionAtLeast([3]int{0, 10, 0}, [3]int{0, 2, 0}) || VersionAtLeast([3]int{0, 1, 9}, [3]int{0, 2, 0}) {
		t.Error("VersionAtLeast wrong")
	}
}

func TestStartExecStop(t *testing.T) {
	b, fe := openFake(t)
	if _, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"true"}}); !errors.Is(err, sandbox.ErrNoSession) {
		t.Fatalf("exec before start = %v, want ErrNoSession", err)
	}
	if err := b.Start(ctxT(t), sandbox.Session{ID: "chat-1", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	if got := fe.sessionIDs(); len(got) != 1 || got[0] != "chat-1" {
		t.Fatalf("sessions = %v", got)
	}
	out, info := run(t, b, "echo", "hello")
	if strings.TrimSpace(out) != "hello" || info.Code != 0 {
		t.Fatalf("out=%q info=%+v", out, info)
	}
	if _, info := run(t, b, "sh", "-c", "exit 7"); info.Code != 7 || info.Lost || info.BackendError {
		t.Fatalf("exit code not propagated: %+v", info)
	}
	if err := b.Stop(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if got := fe.servedIDs(); len(got) != 0 {
		t.Fatalf("session survived Stop: %v", got)
	}
	// hisn 0.4.0: Wakil ends a session with `hisn stop` (audited, directory
	// kept as "stopped"), never with a kill.
	if got := fe.stoppedIDs(); len(got) != 1 || got[0] != "chat-1" {
		t.Fatalf("the stopped directory must stay for hisn's audit: %v", got)
	}
	if c := calls(t, fe); !strings.Contains(c, "stop --session chat-1") || strings.Contains(c, "kill --session chat-1") {
		t.Fatalf("Stop must use `hisn stop` and not `hisn kill`:\n%s", c)
	}
	if ids := b.listRecordIDs(); len(ids) != 0 {
		t.Fatalf("owner record survived Stop: %v", ids)
	}
}

func TestPolicyContents(t *testing.T) {
	b, fe := openFake(t)
	fe2 := fe.options()
	fe2.GrantableRW = []string{"/srv/out"}
	b2, err := New(fe2)
	if err != nil {
		t.Fatal(err)
	}
	_ = b
	if err := b2.Start(ctxT(t), sandbox.Session{ID: "p1"}); err != nil {
		t.Fatal(err)
	}
	defer b2.Close(context.Background())
	pol, _ := os.ReadFile(filepath.Join(fe.rt(), "p1", "policy"))
	s := string(pol)
	for _, want := range []string{`version = "hisn-policy/v1"`, `network = "host"`, `cwd = '` + fe.cwd + `'`, `"/srv/out"`, `"GOFLAGS"`} {
		if !strings.Contains(s, want) && !strings.Contains(s, strings.ReplaceAll(want, "'", `"`)) {
			t.Errorf("policy missing %s:\n%s", want, s)
		}
	}
	if !strings.Contains(s, "wakil-") {
		t.Errorf("policy missing instance hostname token:\n%s", s)
	}
}

func TestSequentialAndConcurrentExecs(t *testing.T) {
	b, fe := openFake(t)
	_ = b.Start(ctxT(t), sandbox.Session{ID: "c", HostCwd: fe.cwd})
	for i := 0; i < 3; i++ {
		if out, info := run(t, b, "echo", "seq"); info.Code != 0 || strings.TrimSpace(out) != "seq" {
			t.Fatalf("seq %d: %q %+v", i, out, info)
		}
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if out, info := run(t, b, "sh", "-c", "sleep 0.2; echo par"); info.Code != 0 || strings.TrimSpace(out) != "par" {
				t.Errorf("concurrent: %q %+v", out, info)
			}
		}()
	}
	wg.Wait()
}

func TestExecEnvAllowlist(t *testing.T) {
	b, fe := openFake(t)
	_ = b.Start(ctxT(t), sandbox.Session{ID: "c", HostCwd: fe.cwd})
	for _, k := range []string{"AWS_SECRET_ACCESS_KEY", "HOME", "GOCACHE", "XDG_DATA_HOME", "LD_PRELOAD"} {
		_, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"true"}, Env: map[string]string{k: "x"}})
		if err == nil {
			t.Errorf("env %s must be rejected", k)
		}
	}
	h, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"true"}, Env: map[string]string{"CI": "1"}})
	if err != nil {
		t.Fatalf("allowlisted env rejected: %v", err)
	}
	_, _ = h.Wait()
}

func TestExecCwdMapping(t *testing.T) {
	b, fe := openFake(t)
	_ = b.Start(ctxT(t), sandbox.Session{ID: "c", HostCwd: fe.cwd})
	sub := filepath.Join(fe.cwd, "sub")
	_ = os.MkdirAll(sub, 0o755)
	var out bytes.Buffer
	h, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"pwd"}, Cwd: sub, Stdout: &out})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = h.Wait()
	if strings.TrimSpace(out.String()) != sub {
		t.Errorf("pwd = %q, want %q", out.String(), sub)
	}
	if _, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"true"}, Cwd: "/etc"}); !errors.Is(err, sandbox.ErrNotVisible) {
		t.Errorf("cwd outside the sandbox view = %v, want ErrNotVisible", err)
	}
	// The private /tmp is a valid exec cwd but has no host representation.
	tmp := t.TempDir()
	if _, err := b.MapPath(tmp); !errors.Is(err, sandbox.ErrNoHostPath) && !errors.Is(err, sandbox.ErrNotVisible) {
		t.Errorf("MapPath(%s) = %v", tmp, err)
	}
	if _, err := b.MapPath("/tmp/anything"); !errors.Is(err, sandbox.ErrNoHostPath) {
		t.Errorf("MapPath(/tmp/..) = %v, want ErrNoHostPath", err)
	}
	if h, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"true"}, Cwd: "/tmp"}); err != nil {
		// the fake runs the command in the REAL /tmp; only admission matters here
		t.Errorf("private /tmp cwd rejected: %v", err)
	} else {
		_, _ = h.Wait() // an unwaited handle would pin the session's in-flight count
	}
}

func TestLostExec(t *testing.T) {
	b, fe := openFake(t)
	_ = b.Start(ctxT(t), sandbox.Session{ID: "c", HostCwd: fe.cwd})
	_, info := run(t, b, "__lost")
	if !info.Lost || info.BackendError {
		t.Fatalf("125 without a diagnostic must be Lost: %+v", info)
	}
	if len(b.LostExecs()) != 1 {
		t.Errorf("lost exec not recorded: %v", b.LostExecs())
	}
	// 125 WITH a diagnostic is a hisn error, not a lost exec.
	h, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"true"}, Env: nil})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = h.Wait()
	_ = b.Stop(ctxT(t))
	// Exec against a vanished session: the fake prints "hisn: ..." and exits 125.
	b2, fe2 := openFake(t)
	b2.stopWait = 300 * time.Millisecond // its supervisor never sees the kill below
	_ = b2.Start(ctxT(t), sandbox.Session{ID: "gone", HostCwd: fe2.cwd})
	_ = os.RemoveAll(filepath.Join(fe2.rt(), "gone"))
	_, info2 := run(t, b2, "true")
	if !info2.BackendError || info2.Lost {
		t.Fatalf("hisn diagnostic + 125 must be BackendError: %+v", info2)
	}
}

func TestGrantRevokeAndNotGrantable(t *testing.T) {
	fe := newFakeEnv(t)
	o := fe.options()
	o.GrantableRW = []string{"/srv/out"}
	b, err := Open(ctxT(t), o)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(context.Background())
	if err := b.Grant(ctxT(t), "/srv/x", false); !errors.Is(err, sandbox.ErrNoSession) {
		t.Fatalf("grant before start = %v", err)
	}
	_ = b.Start(ctxT(t), sandbox.Session{ID: "g", HostCwd: fe.cwd})
	if err := b.Grant(ctxT(t), "/srv/ref", false); err != nil {
		t.Fatal(err)
	}
	if h, err := b.MapPath("/srv/ref/x"); err != nil || h != "/srv/ref/x" {
		t.Fatalf("granted path not mapped: %q %v", h, err)
	}
	if err := b.Grant(ctxT(t), "/srv/out/d", true); err != nil {
		t.Fatalf("rw under grantable prefix: %v", err)
	}
	if err := b.Grant(ctxT(t), "/etc/x", true); !errors.Is(err, sandbox.ErrNotGrantable) {
		t.Fatalf("rw outside prefix = %v, want ErrNotGrantable", err)
	}
	if err := b.Revoke(ctxT(t), "/srv/ref"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.MapPath("/srv/ref/x"); err == nil {
		t.Error("revoked path still maps")
	}
	// Grants die with the session.
	_ = b.Stop(ctxT(t))
	_ = b.Start(ctxT(t), sandbox.Session{ID: "g2", HostCwd: fe.cwd})
	if _, err := b.MapPath("/srv/out/d/x"); err == nil {
		t.Error("grant leaked into the next session")
	}
}

func TestStopKillsSleeper(t *testing.T) {
	b, fe := openFake(t)
	_ = b.Start(ctxT(t), sandbox.Session{ID: "c", HostCwd: fe.cwd})
	h, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan sandbox.ExitInfo, 1)
	go func() { i, _ := h.Wait(); done <- i }()
	time.Sleep(300 * time.Millisecond)
	if err := b.Stop(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Stop did not end the running exec")
	}
}

func TestRebindReplacesSession(t *testing.T) {
	b, fe := openFake(t)
	_ = b.Start(ctxT(t), sandbox.Session{ID: "a", HostCwd: fe.cwd})
	e1 := b.Epoch()
	if err := b.Start(ctxT(t), sandbox.Session{ID: "a", HostCwd: fe.cwd}); err != nil || b.Epoch() != e1 {
		t.Fatalf("same-id Start must be a no-op (epoch %d→%d, err %v)", e1, b.Epoch(), err)
	}
	if err := b.Start(ctxT(t), sandbox.Session{ID: "b", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	if b.Epoch() == e1 {
		t.Error("epoch must advance on every replacement")
	}
	if got := fe.servedIDs(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("old session not stopped: %v", got)
	}
}

func TestStaleExecCannotReachReplacement(t *testing.T) {
	b, fe := openFake(t)
	_ = b.Start(ctxT(t), sandbox.Session{ID: "a", HostCwd: fe.cwd})
	h, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"sleep", "60"}})
	if err != nil {
		t.Fatal(err)
	}
	_ = b.Start(ctxT(t), sandbox.Session{ID: "b", HostCwd: fe.cwd})
	done := make(chan struct{})
	go func() { _, _ = h.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("exec of the replaced session was not cancelled")
	}
	_ = b.Stop(ctxT(t))
	if _, err := b.Exec(ctxT(t), sandbox.ExecSpec{Argv: []string{"true"}}); !errors.Is(err, sandbox.ErrNoSession) {
		t.Fatalf("Exec after Stop must NOT lazily start a session: %v", err)
	}
}

// ---- ownership & reconcile ----

func TestStartRefusesForeignSession(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	fe.addForeignSession("chat-x")
	err := b.Start(ctxT(t), sandbox.Session{ID: "chat-x", HostCwd: fe.cwd})
	if !errors.Is(err, sandbox.ErrSessionInUse) {
		t.Fatalf("want ErrSessionInUse, got %v", err)
	}
	if !contains(fe.sessionIDs(), "chat-x") {
		t.Fatal("a foreign session was killed")
	}
	if ids := b.listRecordIDs(); len(ids) != 0 {
		t.Fatalf("failed start left an owner record: %v", ids)
	}
}

func TestFailedRunNeverKillsForeignInstance(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	// The session id appears (someone else's) between our ls check and run:
	// simulate by making `run` collide — the fake refuses an existing id.
	fe.addForeignSession("race")
	_ = b.withLock(func() error {
		_, err := b.startLocked(ctxT(t), sandbox.Session{ID: "race", HostCwd: fe.cwd})
		if err == nil {
			t.Error("expected failure")
		}
		return nil
	})
	if !contains(fe.sessionIDs(), "race") {
		t.Fatal("abortStart killed a session whose instance token does not match")
	}
}

func TestLiveOwnerCollision(t *testing.T) {
	fe := newFakeEnv(t)
	b1, _ := Open(ctxT(t), fe.options())
	if err := b1.Start(ctxT(t), sandbox.Session{ID: "same", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	b2, _ := New(fe.options()) // second "process" (same pid, so owner is alive)
	err := b2.Start(ctxT(t), sandbox.Session{ID: "same", HostCwd: fe.cwd})
	if !errors.Is(err, sandbox.ErrSessionInUse) {
		t.Fatalf("two owners on one chat: %v", err)
	}
	if !contains(fe.sessionIDs(), "same") {
		t.Fatal("collision killed the first owner's session")
	}
	_ = b1.Close(context.Background())
}

func TestReconcileKillsOnlyDeadOwnersOrphans(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	// A session "owned" by a dead Wakil: create it, then forge a dead owner.
	if err := b.Start(ctxT(t), sandbox.Session{ID: "orphan", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	rec, _ := b.readRecord("orphan")
	rec.Pid, rec.PidStart = 999999, ""
	if err := b.rewriteRecord(rec); err != nil {
		t.Fatal(err)
	}
	fe.addForeignSession("someone-elses") // no record: must survive
	b.mu.Lock()
	b.cur = nil // pretend the owning process is gone
	b.mu.Unlock()

	b2, _ := New(fe.options())
	if err := b2.Reconcile(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	ids := fe.servedIDs()
	if contains(ids, "orphan") {
		t.Errorf("dead owner's session not reaped: %v", ids)
	}
	if !contains(ids, "someone-elses") {
		t.Errorf("foreign session without a record was killed: %v", ids)
	}
	if len(b2.listRecordIDs()) != 0 {
		t.Errorf("record not removed: %v", b2.listRecordIDs())
	}
}

func TestReconcileLiveOwnerUntouched(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	_ = b.Start(ctxT(t), sandbox.Session{ID: "mine", HostCwd: fe.cwd})
	b2, _ := New(fe.options())
	if err := b2.Reconcile(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if !contains(fe.sessionIDs(), "mine") {
		t.Fatal("reconcile killed a session whose owner is alive")
	}
	_ = b.Close(context.Background())
}

func TestReconcileIDReuseNotKilled(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	// Dead owner's record exists, but the id now belongs to a foreign session
	// (token mismatch): the foreign session must survive, the record is dropped.
	rec := record{ID: "reused", Pid: 999999, Token: newToken(), Cwd: fe.cwd, Started: true}
	_ = os.MkdirAll(b.sessionsDir(), 0o700)
	if err := b.createRecord(rec); err != nil {
		t.Fatal(err)
	}
	fe.addForeignSession("reused")
	if err := b.Reconcile(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if !contains(fe.sessionIDs(), "reused") {
		t.Fatal("reconcile killed a reused id that is not our instance")
	}
	if len(b.listRecordIDs()) != 0 {
		t.Error("stale record kept")
	}
}

// A failing `hisn stop` is not fatal: the supervisor handle is authoritative, so
// Stop falls back to SIGKILL on the supervisor and still ends the session.
func TestFailedHisnKillFallsBackToSupervisorKill(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	b.stopWait = 400 * time.Millisecond
	_ = b.Start(ctxT(t), sandbox.Session{ID: "stuck", HostCwd: fe.cwd})
	_ = os.WriteFile(filepath.Join(fe.state, "stop_fail"), nil, 0o600)
	sup := b.supOf()
	if err := b.Stop(ctxT(t)); err != nil {
		t.Fatalf("Stop must succeed through the SIGKILL fallback: %v", err)
	}
	if !sup.hasExited() || !strings.Contains(sup.exitStatus(), "killed") {
		t.Fatalf("supervisor not SIGKILLed: %s", sup.exitStatus())
	}
	if len(b.listRecordIDs()) != 0 || contains(fe.servedIDs(), "stuck") {
		t.Fatal("leftovers after a forced stop")
	}
}

// A supervisor that cannot be reaped keeps the owner record (recovery
// information) and stays tracked; a later Close finishes once it can be killed.
func TestUnreapableSupervisorKeepsRecoveryUntilRetry(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	b.stopWait = 300 * time.Millisecond
	fe.setMode("ignoreterm")
	_ = b.Start(ctxT(t), sandbox.Session{ID: "stuck", HostCwd: fe.cwd})
	sup := b.supOf()
	sup.killFn = func() {} // simulate D-state: SIGKILL has no effect
	if err := b.Stop(ctxT(t)); err == nil {
		t.Fatal("Stop must report a supervisor that did not exit")
	}
	if len(b.listRecordIDs()) != 1 {
		t.Fatal("unreaped supervisor discarded the owner record (recovery information lost)")
	}
	b.mu.RLock()
	tracked := len(b.orphans)
	b.mu.RUnlock()
	if tracked != 1 {
		t.Fatalf("unstopped session not tracked: %d", tracked)
	}
	sup.killFn = nil
	if err := b.Close(ctxT(t)); err != nil {
		t.Fatalf("retry after failed stop: %v", err)
	}
	if contains(fe.servedIDs(), "stuck") || len(b.listRecordIDs()) != 0 || !sup.hasExited() {
		t.Fatal("retry did not clean up")
	}
}

func TestNetworkModeGetterAndSetter(t *testing.T) {
	_, fe := openFake(t)
	opts := fe.options()
	b, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.NetworkMode(); got != "host" {
		t.Fatalf("default NetworkMode = %q, want host", got)
	}
	b.SetNetworkMode("open")
	if got := b.NetworkMode(); got != "open" {
		t.Fatalf("after SetNetworkMode(open) = %q, want open", got)
	}
	b.SetNetworkMode("host")
	if got := b.NetworkMode(); got != "host" {
		t.Fatalf("after SetNetworkMode(host) = %q, want host", got)
	}
	// Network reset with no project value returns to the default (host).
	b.SetNetworkMode("open")
	b.SetNetworkMode("")
	if got := b.NetworkMode(); got != "host" {
		t.Fatalf("after SetNetworkMode(\"\") with no project value = %q, want host (reset to the default)", got)
	}
	// Reset with a project "open" returns to open (the project candidate is kept).
	b2, err := New(func() Options {
		o := fe.options()
		o.Surface.Network = "open"
		return o
	}())
	if err != nil {
		t.Fatal(err)
	}
	b2.SetNetworkMode("host")
	if got := b2.NetworkMode(); got != "host" {
		t.Fatalf("project open + /network host = %q, want host", got)
	}
	b2.SetNetworkMode("")
	if got := b2.NetworkMode(); got != "open" {
		t.Fatalf("after /network reset with project open = %q, want open", got)
	}
	if got := b2.ProjectNetworkMode(); got != "open" {
		t.Fatalf("ProjectNetworkMode = %q, want open", got)
	}

	// Project-surface narrowing to "open" is reported immediately by NetworkMode().
	optsOpen := fe.options()
	optsOpen.Surface.Network = "open"
	bOpen, err := New(optsOpen)
	if err != nil {
		t.Fatal(err)
	}
	if got := bOpen.NetworkMode(); got != "open" {
		t.Fatalf("surface-narrowed NetworkMode = %q, want open", got)
	}
}

// TestNetworkMode_LiveVsQueuedWhileSessionRunning verifies that a running
// session keeps its original live NetworkMode() across SetNetworkMode until
// the session is replaced, and SandboxStatus reports both Network (live) and
// NextNetwork (queued) when they differ.
func TestNetworkMode_LiveVsQueuedWhileSessionRunning(t *testing.T) {
	_, fe := openFake(t)
	opts := fe.options()
	b, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.NetworkMode(); got != "host" {
		t.Fatalf("default NetworkMode = %q, want host", got)
	}
	if live, ok := b.LiveNetworkMode(); ok || live != "" {
		t.Fatalf("before Start: LiveNetworkMode = (%q, %v), want (\"\", false)", live, ok)
	}
	if err := b.Start(context.Background(), sandbox.Session{ID: "chat-live-net"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })

	if got := b.NetworkMode(); got != "host" {
		t.Fatalf("running host session: NetworkMode = %q, want host", got)
	}
	if live, ok := b.LiveNetworkMode(); !ok || live != "host" {
		t.Fatalf("running host session: LiveNetworkMode = (%q, %v), want (host, true)", live, ok)
	}

	// Queue "open" for the next session while the host session is still live.
	b.SetNetworkMode("open")
	if got := b.NetworkMode(); got != "host" {
		t.Fatalf("after SetNetworkMode(open) on running host session: NetworkMode = %q, want host", got)
	}
	if live, ok := b.LiveNetworkMode(); !ok || live != "host" {
		t.Fatalf("after SetNetworkMode(open) on running host session: LiveNetworkMode = (%q, %v), want (host, true)", live, ok)
	}
	st := b.SandboxStatus(context.Background())
	if st.Network != "host" || st.NextNetwork != "open" {
		t.Fatalf("SandboxStatus while queued = (Network:%q, NextNetwork:%q), want (host, open)", st.Network, st.NextNetwork)
	}

	// Replace the session (new chat id): the new session starts with "open"
	// and NextNetwork clears because live and queued now match.
	if err := b.Start(context.Background(), sandbox.Session{ID: "chat-live-net-2"}); err != nil {
		t.Fatalf("Start replacement: %v", err)
	}
	if got := b.NetworkMode(); got != "open" {
		t.Fatalf("after session replacement: NetworkMode = %q, want open", got)
	}
	st2 := b.SandboxStatus(context.Background())
	if st2.Network != "open" || st2.NextNetwork != "" {
		t.Fatalf("SandboxStatus after replacement = (Network:%q, NextNetwork:%q), want (open, empty)", st2.Network, st2.NextNetwork)
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}
