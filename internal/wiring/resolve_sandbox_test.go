package wiring

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
)

func defaultedCfg(t *testing.T, ws string) config.Config {
	t.Helper()
	cfgFile := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(cfgFile, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"WAKIL_SANDBOX_BACKEND", "WAKIL_EXEC_MODE", "ILM_EXEC_MODE", "WAKIL_WORKDIR", "ILM_WORKDIR"} {
		t.Setenv(k, "")
	}
	cfg, err := config.LoadConfig([]string{"--config", cfgFile, "--base-url", "http://x:1", "--host-workdir", ws})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

var (
	hisnOK  = func() config.HisnAvailability { return config.HisnAvailability{Version: "0.3.1"} }
	hisnOld = func() config.HisnAvailability {
		return config.HisnAvailability{Version: "0.2.2", Reason: "hisn 0.2.2 is older than 0.3.1"}
	}
	hisnMissing = func() config.HisnAvailability { return config.HisnAvailability{Reason: "hisn not found in PATH"} }
)

func TestResolveSandboxFlipsOnlyWhenAllHold(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	cfg := defaultedCfg(t, ws)

	if got := resolveSandbox(cfg, hisnOK); got.Sandbox.Backend != config.SandboxBackendHisn {
		t.Fatalf("hisn 0.3.1 + check ok + nothing explicit must flip, got %q (%s)", got.Sandbox.Backend, got.StartupBanner())
	}
	for name, probe := range map[string]func() config.HisnAvailability{"too old": hisnOld, "missing": hisnMissing} {
		got := resolveSandbox(cfg, probe)
		if got.Sandbox.Backend != config.SandboxBackendDocker {
			t.Errorf("%s: must stay docker", name)
		}
		if !strings.Contains(got.StartupBanner(), "default: docker (") {
			t.Errorf("%s: the reason must be in the banner: %q", name, got.StartupBanner())
		}
	}
}

func TestResolveSandboxNeverProbesWhenExplicit(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	cfgFile := filepath.Join(t.TempDir(), "c.json")
	_ = os.WriteFile(cfgFile, []byte(`{"sandbox":{"backend":"docker"}}`), 0o644)
	cfg, err := config.LoadConfig([]string{"--config", cfgFile, "--base-url", "http://x:1", "--host-workdir", t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	probed := false
	got := resolveSandbox(cfg, func() config.HisnAvailability { probed = true; return config.HisnAvailability{Version: "0.3.1"} })
	if probed || got.Sandbox.Backend != config.SandboxBackendDocker || got.Sandbox.DefaultNote != "" {
		t.Errorf("an explicit backend is never second-guessed (probed=%v backend=%q note=%q)", probed, got.Sandbox.Backend, got.Sandbox.DefaultNote)
	}
}

// An unapproved .wakil/sandbox.json must not turn a working docker setup into
// a hard error through the default alone: stay docker, say why.
func TestResolveSandboxUnapprovedProjectFileKeepsDocker(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.MkdirAll(filepath.Join(ws, ".wakil"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".wakil", "sandbox.json"), []byte(projFile), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := defaultedCfg(t, ws)
	got := resolveSandbox(cfg, hisnOK)
	if got.Sandbox.Backend != config.SandboxBackendDocker || !strings.Contains(got.StartupBanner(), "not approved") {
		t.Errorf("unapproved file: %q", got.StartupBanner())
	}
	// Once the user approves it, the default flips.
	if err := SandboxTrust(nopWriter{}, ws, tty("yes\n")); err != nil {
		t.Fatal(err)
	}
	if got := resolveSandbox(cfg, hisnOK); got.Sandbox.Backend != config.SandboxBackendHisn {
		t.Errorf("approved file: %q", got.StartupBanner())
	}
}

// A malformed project file also keeps docker rather than aborting the start.
func TestResolveSandboxBrokenProjectFileKeepsDocker(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	_ = os.MkdirAll(filepath.Join(ws, ".wakil"), 0o755)
	_ = os.WriteFile(filepath.Join(ws, ".wakil", "sandbox.json"), []byte(`{"nope":1}`), 0o644)
	got := resolveSandbox(defaultedCfg(t, ws), hisnOK)
	if got.Sandbox.Backend != config.SandboxBackendDocker || !strings.Contains(got.StartupBanner(), "sandbox.json") {
		t.Errorf("banner: %q", got.StartupBanner())
	}
}

// A workspace that contains Wakil's state directory (e.g. $HOME) would get its
// generated profile refused by hisn: stay docker.
func TestResolveSandboxWorkspaceContainingStateDir(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	got := resolveSandbox(defaultedCfg(t, home), hisnOK)
	if got.Sandbox.Backend != config.SandboxBackendDocker || !strings.Contains(got.StartupBanner(), "state directory") {
		t.Errorf("banner: %q", got.StartupBanner())
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
