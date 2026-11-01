package hisn

import (
	"bytes"
	"context"
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/sandbox"
)

// Integration tests against a REAL hisn. They SKIP (never fail) when hisn is
// missing, too old, or `hisn check` reports the host cannot run sessions
// (e.g. no unprivileged user namespaces, as in many build sandboxes).
//
// Run on a host with hisn >= 0.2.0:
//
//	go test -count=1 -v -run TestIntegration ./internal/sandbox/hisn/
//
// Set WAKIL_HISN_BIN to test a specific binary.

func realBackend(t *testing.T, grantable ...string) (*Backend, string) {
	t.Helper()
	bin := os.Getenv("WAKIL_HISN_BIN")
	if bin == "" {
		p, err := osexec.LookPath("hisn")
		if err != nil {
			t.Skip("hisn not found on PATH")
		}
		bin = p
	}
	root := t.TempDir()
	// hisn forbids cwd under its runtime dir (/tmp/hisn-$UID); a t.TempDir()
	// below /tmp is fine, but resolve symlinks like Wakil does.
	cwd, err := filepath.EvalSymlinks(filepath.Join(root, "ws"))
	if err != nil {
		if mkerr := os.MkdirAll(filepath.Join(root, "ws"), 0o755); mkerr != nil {
			t.Fatal(mkerr)
		}
		cwd, _ = filepath.EvalSymlinks(filepath.Join(root, "ws"))
	}
	b, err := New(Options{
		Binary: bin, StateDir: filepath.Join(root, "state"), HomeRoot: filepath.Join(root, "home"),
		HostCwd: cwd, GrantableRW: grantable, EnvPass: []string{"CI", "GOFLAGS"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := b.Preflight(ctx); err != nil {
		t.Skipf("hisn unusable on this host: %v", err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return b, cwd
}

func ictx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func irun(t *testing.T, b *Backend, cwd string, argv ...string) (string, sandbox.ExitInfo) {
	t.Helper()
	var out bytes.Buffer
	h, err := b.Exec(ictx(t), sandbox.ExecSpec{Argv: argv, Cwd: cwd, Stdout: &out})
	if err != nil {
		t.Fatalf("exec %v: %v", argv, err)
	}
	info, err := h.Wait()
	if err != nil {
		t.Fatal(err)
	}
	return out.String(), info
}

func TestIntegrationStartExec(t *testing.T) {
	b, cwd := realBackend(t)
	if err := b.Start(ictx(t), sandbox.Session{ID: "wakil-it-1", HostCwd: cwd}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ { // 3 sequential
		out, info := irun(t, b, "", "sh", "-c", "echo seq-$$ && pwd")
		if info.Code != 0 || !strings.Contains(out, "seq-") || !strings.Contains(out, cwd) {
			t.Fatalf("seq %d: %q %+v (cwd must be the identical host path %s)", i, out, info, cwd)
		}
	}
	var wg sync.WaitGroup // 3 concurrent
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, info := irun(t, b, "", "sh", "-c", "sleep 1; echo par")
			if info.Code != 0 || strings.TrimSpace(out) != "par" {
				t.Errorf("concurrent: %q %+v", out, info)
			}
		}()
	}
	wg.Wait()
	if _, info := irun(t, b, "", "sh", "-c", "exit 9"); info.Code != 9 {
		t.Errorf("exit code = %+v, want 9", info)
	}
	// The workspace is the only writable host location.
	if _, info := irun(t, b, "", "sh", "-c", "echo x > "+filepath.Join(cwd, "w.txt")); info.Code != 0 {
		t.Errorf("write into cwd failed: %+v", info)
	}
	if _, err := os.Stat(filepath.Join(cwd, "w.txt")); err != nil {
		t.Errorf("file written in the sandbox is not on the host: %v", err)
	}
}

func TestIntegrationEnvIsAllowlisted(t *testing.T) {
	b, cwd := realBackend(t)
	t.Setenv("WAKIL_IT_SECRET", "must-not-leak")
	t.Setenv("CI", "1")
	if err := b.Start(ictx(t), sandbox.Session{ID: "wakil-it-env", HostCwd: cwd}); err != nil {
		t.Fatal(err)
	}
	out, _ := irun(t, b, "", "sh", "-c", "env")
	if strings.Contains(out, "WAKIL_IT_SECRET") || strings.Contains(out, "must-not-leak") {
		t.Errorf("host environment leaked into the sandbox:\n%s", out)
	}
	if !strings.Contains(out, "CI=1") {
		t.Errorf("allowlisted CI missing:\n%s", out)
	}
}

func TestIntegrationGrantRevokeVisibility(t *testing.T) {
	b, cwd := realBackend(t)
	if err := b.Start(ictx(t), sandbox.Session{ID: "wakil-it-grant", HostCwd: cwd}); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	// t.TempDir lives under /tmp, which hisn masks with a private tmpfs; use a
	// directory under the user's cache instead when /tmp is the parent.
	if strings.HasPrefix(outside, "/tmp/") {
		home, _ := os.UserHomeDir()
		d, err := os.MkdirTemp(home, ".wakil-hisn-it-")
		if err != nil {
			t.Skipf("no usable directory outside /tmp: %v", err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(d) })
		outside = d
	}
	if err := os.WriteFile(filepath.Join(outside, "f.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, info := irun(t, b, "", "cat", filepath.Join(outside, "f.txt")); info.Code == 0 {
		t.Fatal("an ungranted host path must not exist in the sandbox (ENOENT)")
	}
	if err := b.Grant(ictx(t), outside, false); err != nil {
		t.Fatal(err)
	}
	if out, info := irun(t, b, "", "cat", filepath.Join(outside, "f.txt")); info.Code != 0 || out != "data" {
		t.Fatalf("granted path not readable: %q %+v", out, info)
	}
	if _, info := irun(t, b, "", "sh", "-c", "echo x > "+filepath.Join(outside, "w.txt")); info.Code == 0 {
		t.Error("a ro grant must reject writes (EROFS)")
	}
	if err := b.Revoke(ictx(t), outside); err != nil {
		t.Fatal(err)
	}
	if _, info := irun(t, b, "", "cat", filepath.Join(outside, "f.txt")); info.Code == 0 {
		t.Error("revoked path must be ENOENT again")
	}
	// rw outside every grantable prefix is E_NOT_GRANTABLE.
	if err := b.Grant(ictx(t), outside, true); !errors.Is(err, sandbox.ErrNotGrantable) {
		t.Errorf("rw outside grantable_rw = %v, want ErrNotGrantable", err)
	}
}

func TestIntegrationStopKillsSleeper(t *testing.T) {
	b, cwd := realBackend(t)
	if err := b.Start(ictx(t), sandbox.Session{ID: "wakil-it-kill", HostCwd: cwd}); err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(cwd, "sleeper.pid")
	// A detached sleeper that outlives its launcher exec.
	if _, info := irun(t, b, "", "sh", "-c", "setsid nohup sleep 300 >/dev/null 2>&1 & echo $! > "+pidfile); info.Code != 0 {
		t.Fatalf("launch: %+v", info)
	}
	out, info := irun(t, b, "", "sh", "-c", "kill -0 $(cat "+pidfile+") && echo alive")
	if info.Code != 0 || !strings.Contains(out, "alive") {
		t.Fatalf("background process did not survive its launcher exec: %q %+v", out, info)
	}
	if err := b.Stop(ictx(t)); err != nil {
		t.Fatal(err)
	}
	sessions, err := b.listSessions(ictx(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, still := sessions["wakil-it-kill"]; still {
		t.Fatal("session survived Stop")
	}
	// Nothing named sleep 300 may remain on the host.
	if o, _ := osexec.Command("pgrep", "-f", "sleep 300").Output(); strings.TrimSpace(string(o)) != "" {
		for _, f := range strings.Fields(string(o)) {
			var pid int
			_, _ = fmtSscan(f, &pid)
			if pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
		t.Errorf("sleeper survived the session: pids %s", o)
	}
}

func TestIntegrationOrphanReconcile(t *testing.T) {
	b, cwd := realBackend(t)
	if err := b.Start(ictx(t), sandbox.Session{ID: "wakil-it-orphan", HostCwd: cwd}); err != nil {
		t.Fatal(err)
	}
	// Forge a dead owner, as if the Wakil that created the session crashed.
	rec, err := b.readRecord("wakil-it-orphan")
	if err != nil {
		t.Fatal(err)
	}
	rec.Pid, rec.PidStart = 999999, ""
	if err := b.rewriteRecord(rec); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.cur = nil
	b.mu.Unlock()

	b2, err := New(b.opts)
	if err != nil {
		t.Fatal(err)
	}
	if err := b2.Reconcile(ictx(t)); err != nil {
		t.Fatal(err)
	}
	sessions, err := b2.listSessions(ictx(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, still := sessions["wakil-it-orphan"]; still {
		t.Fatal("orphan session was not reaped")
	}
}

func TestIntegrationStartRefusesForeignSession(t *testing.T) {
	b, cwd := realBackend(t)
	// A session created outside Wakil's control (no owner record).
	foreign := osexec.Command(b.opts.Binary, "run", "--detach", "--session-id", "wakil-it-foreign", "--cwd", cwd)
	if out, err := foreign.CombinedOutput(); err != nil {
		t.Skipf("cannot create a foreign session: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = osexec.Command(b.opts.Binary, "kill", "--session", "wakil-it-foreign").Run() })
	err := b.Start(ictx(t), sandbox.Session{ID: "wakil-it-foreign", HostCwd: cwd})
	if !errors.Is(err, sandbox.ErrSessionInUse) {
		t.Fatalf("want ErrSessionInUse, got %v", err)
	}
	sessions, _ := b.listSessions(ictx(t))
	if _, ok := sessions["wakil-it-foreign"]; !ok {
		t.Fatal("Wakil killed a session it did not create")
	}
}

func fmtSscan(s string, n *int) (int, error) {
	v := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, errors.New("not a number")
		}
		v = v*10 + int(c-'0')
	}
	*n = v
	return 1, nil
}
