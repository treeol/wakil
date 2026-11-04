package hisn

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/treeol/wakil/internal/sandbox"
	"github.com/treeol/wakil/internal/staging"
)

func kvrOpts(bin string) KVROptions {
	return KVROptions{Enabled: true, Binary: bin, MaxEntries: 1000, SweepIntervalSecs: 30, SnapshotSecs: 300}
}

func TestPlanKVRDefaultService(t *testing.T) {
	bin, _ := filepath.EvalSymlinks("/bin/sh") // under /usr or /bin: visible
	p, err := planKVR("/w", Surface{}, kvrOpts(bin), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Add || p.Socket != "/w/.wakil/kvr.sock" {
		t.Fatalf("%+v", p)
	}
	ex := strings.Join(p.Service.Exec, " ")
	for _, want := range []string{"/usr/bin/env", "KVR_SOCKET_PATH=/w/.wakil/kvr.sock", "KVR_SNAPSHOT_PATH=/w/.wakil/staging.kvr",
		"KVR_MAX_ENTRIES=1000", "KVR_SWEEP_INTERVAL_SECS=30", "KVR_SNAPSHOT_INTERVAL_SECS=300", bin} {
		if !strings.Contains(ex, want) {
			t.Errorf("exec missing %q: %s", want, ex)
		}
	}
	if p.Service.ReadySocket != "" {
		t.Error("a broken kvr must not be able to fail the whole session: no ready check")
	}
}

func TestPlanKVROffReasons(t *testing.T) {
	bin, _ := filepath.EvalSymlinks("/bin/sh")
	cases := map[string]struct {
		s       Surface
		o       KVROptions
		profile bool
		want    string
	}{
		"disabled":      {Surface{}, KVROptions{}, true, "kvr_disabled"},
		"0.2.x":         {Surface{}, kvrOpts(bin), false, "needs hisn >= 0.3.1"},
		"named profile": {Surface{Profile: "p"}, kvrOpts(bin), true, "named hisn profile"},
		"no binary":     {Surface{}, kvrOpts(""), true, "no kvr-server binary"},
		"not visible":   {Surface{}, kvrOpts(writeExe(t, t.TempDir())), true, "not visible inside the sandbox"},
	}
	for name, c := range cases {
		p, err := planKVR("/w", c.s, c.o, c.profile, nil)
		if err != nil || p.Socket != "" || p.Add || !strings.Contains(p.Note, c.want) {
			t.Errorf("%s: %+v %v (want note containing %q)", name, p, err, c.want)
		}
	}
	// A binary in a directory the project lists under mounts_ro IS visible.
	dir := t.TempDir()
	if p, err := planKVR("/w", Surface{}, kvrOpts(writeExe(t, dir)), true, []string{dir}); err != nil || !p.Add {
		t.Errorf("mounts_ro dir must make the binary visible: %+v %v", p, err)
	}
}

func writeExe(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "kvr-server")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(p)
	return real
}

func TestPlanKVRSymlinkedBinaryResolved(t *testing.T) {
	// A link under a visible root to a place that is not visible is NOT visible.
	hidden := writeExe(t, t.TempDir())
	link := filepath.Join(t.TempDir(), "kvr-link")
	if err := os.Symlink(hidden, link); err != nil {
		t.Fatal(err)
	}
	if _, ok := binaryVisible(link, nil); ok {
		t.Error("visibility must be judged on the resolved path")
	}
}

func TestPlanKVRProjectService(t *testing.T) {
	good := Surface{Services: []Service{{Name: "kvr", Exec: []string{"x"}, Socket: "/w/.wakil/k.sock", ReadySocket: "/w/.wakil/k.sock"}}}
	if p, err := planKVR("/w", good, kvrOpts(""), true, nil); err != nil || p.Socket != "/w/.wakil/k.sock" || p.Add {
		t.Fatalf("project kvr is used as is: %+v %v", p, err)
	}
	bad := map[string]Surface{
		"no socket":     {Services: []Service{{Name: "kvr", Exec: []string{"x"}}}},
		"elsewhere":     {Services: []Service{{Name: "kvr", Exec: []string{"x"}, Socket: "/run/user/1000/ssh-agent.sock"}}},
		"nested":        {Services: []Service{{Name: "kvr", Exec: []string{"x"}, Socket: "/w/.wakil/sub/k.sock"}}},
		"outside cwd":   {Services: []Service{{Name: "kvr", Exec: []string{"x"}, Socket: "/w/other/k.sock"}}},
		"ready differs": {Services: []Service{{Name: "kvr", Exec: []string{"x"}, Socket: "/w/.wakil/k.sock", ReadySocket: "/w/.wakil/o.sock"}}},
	}
	for name, s := range bad {
		if _, err := planKVR("/w", s, kvrOpts(""), true, nil); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}

func TestGuardKVRSocket(t *testing.T) {
	cwd, _ := filepath.EvalSymlinks(t.TempDir())
	dir := filepath.Join(cwd, ".wakil")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "kvr.sock")
	ln := listenUnix(t, sock)
	defer ln.Close()
	if err := GuardKVRSocket(cwd, sock); err != nil {
		t.Fatalf("a plain owned socket: %v", err)
	}
	if err := GuardKVRSocket(cwd, filepath.Join(cwd, "kvr.sock")); err == nil {
		t.Error("a socket outside .wakil must be refused")
	}
	// A symlink standing in for the socket (e.g. to docker.sock / ssh-agent).
	link := filepath.Join(dir, "evil.sock")
	if err := os.Symlink(sock, link); err != nil {
		t.Fatal(err)
	}
	if err := GuardKVRSocket(cwd, link); err == nil {
		t.Error("a symlink must be refused")
	}
	// A regular file.
	reg := filepath.Join(dir, "reg.sock")
	_ = os.WriteFile(reg, nil, 0o600)
	if err := GuardKVRSocket(cwd, reg); err == nil {
		t.Error("a regular file must be refused")
	}
	// .wakil replaced by a symlink to elsewhere.
	cwd2, _ := filepath.EvalSymlinks(t.TempDir())
	other := t.TempDir()
	if err := os.Symlink(other, filepath.Join(cwd2, ".wakil")); err != nil {
		t.Fatal(err)
	}
	ln2 := listenUnix(t, filepath.Join(other, "kvr.sock"))
	defer ln2.Close()
	if err := GuardKVRSocket(cwd2, filepath.Join(cwd2, ".wakil", "kvr.sock")); err == nil {
		t.Error(".wakil as a symlink must be refused")
	}
}

func TestPrepareKVRDir(t *testing.T) {
	cwd, _ := filepath.EvalSymlinks(t.TempDir())
	sock := filepath.Join(cwd, ".wakil", "kvr.sock")
	if err := PrepareKVRDir(cwd, sock); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(cwd, ".wakil")); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("dir: %v %v", fi, err)
	}
	ln := listenUnix(t, sock)
	ln.(interface{ SetUnlinkOnClose(bool) }).SetUnlinkOnClose(false)
	_ = ln.Close() // a stale socket file stays behind, like a dead session's
	if err := PrepareKVRDir(cwd, sock); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(sock); !os.IsNotExist(err) {
		t.Error("a stale socket must be removed")
	}
	// A non-socket at that path is never unlinked.
	if err := os.WriteFile(sock, []byte("precious"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PrepareKVRDir(cwd, sock); err == nil {
		t.Error("a regular file must be refused, not removed")
	}
	if b, _ := os.ReadFile(sock); string(b) != "precious" {
		t.Error("the file was touched")
	}
}

// ---- lifecycle through the fake supervisor ----

func kvrBackend(t *testing.T, fe *fakeEnv, bin string, mut func(*Options)) *Backend {
	t.Helper()
	setVersion(t, fe, "0.3.1")
	b, err := openWith(t, fe, func(o *Options) {
		o.KVR = kvrOpts(bin)
		if mut != nil {
			mut(o)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestKVRServiceRoundTrip(t *testing.T) {
	fe := newFakeEnv(t)
	bin, _ := filepath.EvalSymlinks("/bin/sh")
	b := kvrBackend(t, fe, bin, nil)
	if err := startChat(t, b, fe, "k1"); err != nil {
		t.Fatal(err)
	}
	sock := b.KVRSocket()
	if sock != filepath.Join(fe.cwd, ".wakil", "kvr.sock") {
		t.Fatalf("socket = %q", sock)
	}
	if err := PingKVR(sock, 2*time.Second); err != nil {
		t.Fatalf("the service must answer through the socket: %v", err)
	}
	// The service is part of the profile that was risk-checked and started.
	prof, _ := os.ReadFile(filepath.Join(fe.sdir, "k1.profile.toml"))
	var doc struct {
		Services []struct{ Name, Socket string }
	}
	if _, err := toml.Decode(string(prof), &doc); err != nil || len(doc.Services) != 1 || doc.Services[0].Name != "kvr" {
		t.Errorf("profile services: %+v %v\n%s", doc, err, prof)
	}
	if st := b.SandboxStatus(ctxT(t)); !strings.HasPrefix(st.KVR, "on") {
		t.Errorf("status KVR = %q", st.KVR)
	}
	// Session end: the socket is gone from the provider and the next start
	// cleans the stale file.
	if err := b.Stop(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if b.KVRSocket() != "" {
		t.Error("no socket after Stop")
	}
	if err := startChat(t, b, fe, "k2"); err != nil {
		t.Fatalf("restart over a stale socket file: %v", err)
	}
	if b.KVRSocket() == "" {
		t.Error("staging must be back after a restart")
	}
}

func TestKVRBrokenServiceNeverFailsSession(t *testing.T) {
	fe := newFakeEnv(t)
	// A binary whose service never binds a socket (hisn would not know).
	dir := t.TempDir()
	p := filepath.Join(dir, "broken-kvr")
	_ = os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755)
	b := kvrBackend(t, fe, p, func(o *Options) {
		o.Surface.MountsRO = []string{dir}
	})
	old := kvrWait
	kvrWait = 300 * time.Millisecond
	defer func() { kvrWait = old }()
	if err := startChat(t, b, fe, "kb"); err != nil {
		t.Fatalf("a dead kvr must not fail the session: %v", err)
	}
	if b.KVRSocket() != "" {
		t.Error("staging must be off")
	}
	if st := b.SandboxStatus(ctxT(t)); !strings.Contains(st.KVR, "off (service did not answer") {
		t.Errorf("the reason must be visible: %q", st.KVR)
	}
	if _, info := run(t, b, "true"); info.Code != 0 {
		t.Error("the session itself works")
	}
}

func TestKVRRefusesSwappedSocket(t *testing.T) {
	fe := newFakeEnv(t)
	bin, _ := filepath.EvalSymlinks("/bin/sh")
	b := kvrBackend(t, fe, bin, nil)
	// A hostile main command replaces the service socket with a symlink to
	// some other socket before Wakil's first connect.
	other := filepath.Join(t.TempDir(), "agent.sock")
	ln := listenUnix(t, other)
	defer ln.Close()
	b.afterSpawn = func(*supervisorProc) {
		sock := filepath.Join(fe.cwd, ".wakil", "kvr.sock")
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Lstat(sock); err == nil {
				_ = os.Remove(sock)
				_ = os.Symlink(other, sock)
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	if err := b.Start(ctxT(t), sandbox.Session{ID: "ks", HostCwd: fe.cwd}); err != nil {
		t.Fatal(err)
	}
	if b.KVRSocket() != "" {
		t.Error("a swapped (symlinked) socket must never be dialled")
	}
	if st := b.SandboxStatus(ctxT(t)); !strings.Contains(st.KVR, "not a plain unix socket") {
		t.Errorf("status: %q", st.KVR)
	}
}

// A real kvr-server behind the service socket: staging put/get/delete through
// the same client the tools use.
func TestKVRStagingRoundTripThroughServiceSocket(t *testing.T) {
	bin, err := exec.LookPath("kvr-server")
	if err != nil {
		t.Skip("kvr-server not installed")
	}
	real, _ := filepath.EvalSymlinks(bin)
	if _, ok := binaryVisible(real, nil); !ok {
		t.Skipf("kvr-server at %s is not under a sandbox-visible root", real)
	}
	fe := newFakeEnv(t)
	b := kvrBackend(t, fe, real, nil)
	if err := startChat(t, b, fe, "rt"); err != nil {
		t.Fatal(err)
	}
	sock := b.KVRSocket()
	if sock == "" {
		t.Fatalf("staging must be on: %s", b.SandboxStatus(ctxT(t)).KVR)
	}
	c := staging.NewClient(sock)
	c.SetPathGuard(func() error { return GuardKVRSocket(fe.cwd, sock) })
	defer c.Close()
	ctx := ctxT(t)
	if err := c.Set(ctx, "agent/k", "value-1"); err != nil {
		t.Fatal(err)
	}
	got, found, err := c.Get(ctx, "agent/k")
	if err != nil || !found || got != "value-1" {
		t.Fatalf("get = %q found=%v err=%v", got, found, err)
	}
	if err := c.Set(ctx, "agent/k", "value-2"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := c.Get(ctx, "agent/k"); got != "value-2" {
		t.Errorf("overwrite: %q", got)
	}
	// The snapshot lives under the workspace's .wakil, not on the host's /run.
	if _, err := os.Stat(filepath.Join(fe.cwd, ".wakil")); err != nil {
		t.Errorf(".wakil: %v", err)
	}
}
