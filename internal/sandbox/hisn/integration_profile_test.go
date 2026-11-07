package hisn

import (
	"errors"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/sandbox"
	"github.com/treeol/wakil/internal/staging"
)

// Integration tests for the hisn 0.3.1 surface against a REAL hisn >= 0.3.1.
// They skip (never fail) when hisn is missing, older, or `hisn check` fails.
//
//	go test -count=1 -v -timeout 10m -run 'TestIntegration(Profile|Risk|KVR|Override)' ./internal/sandbox/hisn/
//
// Set WAKIL_HISN_BIN to test a specific binary.

// realProfileBackend opens a Backend (full Open: preflight + plan) over a real
// hisn >= 0.3.1 with surface s.
func realProfileBackend(t *testing.T, mut func(*Options)) (*Backend, string, error) {
	t.Helper()
	bin := os.Getenv("WAKIL_HISN_BIN")
	if bin == "" {
		p, err := osexec.LookPath("hisn")
		if err != nil {
			t.Skip("hisn not found on PATH")
		}
		bin = p
	}
	out, err := osexec.Command(bin, "--version").Output()
	if err != nil {
		t.Skipf("hisn unusable: %v", err)
	}
	if v, err := ParseVersion(string(out)); err != nil || !VersionAtLeast(v, [3]int{0, 3, 1}) {
		t.Skipf("hisn >= 0.3.1 required (got %q)", strings.TrimSpace(string(out)))
	}
	if err := osexec.Command(bin, "check").Run(); err != nil {
		t.Skipf("hisn check fails on this host: %v", err)
	}
	root := t.TempDir()
	cwd := filepath.Join(root, "ws")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, _ = filepath.EvalSymlinks(cwd)
	o := Options{Binary: bin, StateDir: filepath.Join(root, "state"), HomeRoot: filepath.Join(root, "home"),
		HostCwd: cwd, EnvPass: []string{"CI", "GOFLAGS"}}
	if mut != nil {
		mut(&o)
	}
	b, err := Open(ictx(t), o)
	if err == nil {
		t.Cleanup(func() { _ = b.Close(t.Context()) })
	}
	return b, cwd, err
}

func TestIntegrationProfileStart(t *testing.T) {
	b, cwd, err := realProfileBackend(t, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !b.profileMode() {
		t.Fatal("hisn >= 0.3.1 must start in profile mode")
	}
	istart(t, b, "wakil-it-prof", cwd)
	if _, err := os.Stat(b.profilePath("wakil-it-prof")); err != nil {
		t.Errorf("generated profile must be in the state dir: %v", err)
	}
	if out, info := irun(t, b, cwd, "sh", "-c", "echo ok && pwd"); info.Code != 0 || !strings.Contains(out, cwd) {
		t.Fatalf("exec in a profile session: %q %+v", out, info)
	}
	if st := b.SandboxStatus(ictx(t)); st.Risk != "data" || st.Mode != "generated profile" || len(st.Capabilities) != 1 || st.Capabilities[0].Capability != "home-persist" {
		t.Errorf("status: %+v", st)
	}
}

func TestIntegrationRiskRefusal(t *testing.T) {
	// A docker.sock forward is root-equivalent: without the project's ack Wakil
	// refuses before any session exists. (The socket need not exist: hisn only
	// opens it when the session starts, and we never get that far.)
	_, _, err := realProfileBackend(t, func(o *Options) {
		o.Surface = Surface{Forward: []Forward{{Name: "docker", Socket: "/var/run/docker.sock", Expose: "/run/docker.sock"}}}
	})
	var re *RiskAckError
	if !errors.As(err, &re) || !strings.Contains(err.Error(), "wakil sandbox trust") {
		t.Fatalf("want a RiskAckError naming `wakil sandbox trust`, got %v", err)
	}
}

func TestIntegrationKVRService(t *testing.T) {
	bin, err := osexec.LookPath("kvr-server")
	if err != nil {
		t.Skip("kvr-server not installed")
	}
	real, _ := filepath.EvalSymlinks(bin)
	if _, ok := binaryVisible(real, nil); !ok {
		t.Skipf("kvr-server at %s is not under a root the sandbox sees (/usr, /opt, /nix)", real)
	}
	b, cwd, err := realProfileBackend(t, func(o *Options) { o.KVR = kvrOpts(real) })
	if err != nil {
		t.Fatal(err)
	}
	istart(t, b, "wakil-it-kvr", cwd)
	sock := b.KVRSocket()
	if sock == "" {
		t.Fatalf("staging must be on: %s", b.SandboxStatus(ictx(t)).KVR)
	}
	// The service is a sibling in the SAME sandbox: reachable from a second exec.
	c := staging.NewClient(sock)
	c.SetPathGuard(func() error { return GuardKVRSocket(cwd, sock) })
	defer c.Close()
	ctx := ictx(t)
	if err := c.Set(ctx, "it/k", "v"); err != nil {
		t.Fatal(err)
	}
	if got, found, err := c.Get(ctx, "it/k"); err != nil || !found || got != "v" {
		t.Fatalf("put/get through the service socket: %q %v %v", got, found, err)
	}
	// It dies with the session.
	if err := b.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if PingKVR(sock, 200*time.Millisecond) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Error("the kvr service must die with the session")
}

// Override precedence: the project file ADDS to sandbox.hisn.* config and
// never replaces it. env_set and mounts are visible inside; config's
// grantable_rw/env_pass still apply.
func TestIntegrationOverridePrecedence(t *testing.T) {
	ro := t.TempDir()
	if err := os.WriteFile(filepath.Join(ro, "f.txt"), []byte("ro-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	ro, _ = filepath.EvalSymlinks(ro)
	b, cwd, err := realProfileBackend(t, func(o *Options) {
		o.Surface = Surface{EnvSet: map[string]string{"WAKIL_PROJ": "from-project"}, MountsRO: []string{ro}}
		o.EnvPass = []string{"CI"}
	})
	if err != nil {
		t.Fatal(err)
	}
	istart(t, b, "wakil-it-ov", cwd)
	if out, info := irun(t, b, cwd, "sh", "-c", "echo $WAKIL_PROJ; cat "+ro+"/f.txt"); info.Code != 0 ||
		!strings.Contains(out, "from-project") || !strings.Contains(out, "ro-data") {
		t.Fatalf("project env_set and mounts_ro must be visible: %q %+v", out, info)
	}
	if _, info := irun(t, b, cwd, "sh", "-c", "echo x > "+ro+"/w.txt"); info.Code == 0 {
		t.Error("mounts_ro must be read-only")
	}
	// config's grantable_rw is untouched by the project file: an rw grant
	// outside it is still refused.
	if err := b.Grant(ictx(t), t.TempDir(), true); err == nil || !strings.Contains(err.Error(), sandbox.ErrNotGrantable.Error()) {
		t.Errorf("rw grant outside grantable_rw must still be refused: %v", err)
	}
}
