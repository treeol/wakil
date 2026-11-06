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

func TestResolveSandboxFlipsOnlyWhenAllHold(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws := t.TempDir()
	cfg := defaultedCfg(t, ws)

	got, err := resolveSandbox(cfg, func() (config.HisnAvailability, error) {
		return config.HisnAvailability{Version: "0.3.1"}, nil
	})
	if err != nil || got.Sandbox.Backend != config.SandboxBackendHisn {
		t.Fatalf("hisn 0.3.1 + check ok + nothing explicit must flip, got %q err=%v (%s)", got.Sandbox.Backend, err, got.StartupBanner())
	}
	for name, probe := range map[string]func() (config.HisnAvailability, error){
		"too old": func() (config.HisnAvailability, error) {
			return config.HisnAvailability{Version: "0.2.2", Reason: "hisn 0.2.2 is older than 0.3.1"}, nil
		},
		"missing": func() (config.HisnAvailability, error) {
			return config.HisnAvailability{Reason: "hisn not found in PATH"}, nil
		},
	} {
		got, err := resolveSandbox(cfg, probe)
		if err != nil {
			t.Fatalf("%s: must not error on a hisn-availability reason: %v", name, err)
		}
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
	got, err := resolveSandbox(cfg, func() (config.HisnAvailability, error) {
		probed = true
		return config.HisnAvailability{Version: "0.3.1"}, nil
	})
	if err != nil || probed || got.Sandbox.Backend != config.SandboxBackendDocker || got.Sandbox.DefaultNote != "" {
		t.Errorf("an explicit backend is never second-guessed (probed=%v backend=%q note=%q err=%v)", probed, got.Sandbox.Backend, got.Sandbox.DefaultNote, err)
	}
}

// An unapproved .wakil/sandbox.json is a startup error: the config is returned
// unchanged (no backend fallback, no browser) and the error names the file and
// `wakil sandbox trust`. The chosen backend never differs from the global
// default on the error path.
func TestResolveSandboxUnapprovedProjectFileIsStartupError(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	if err := os.MkdirAll(filepath.Join(ws, ".wakil"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, ".wakil", "sandbox.json"), []byte(projFile), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := defaultedCfg(t, ws)

	// unapproved file → startup error, backend unchanged (docker default).
	got, err := resolveSandbox(cfg, func() (config.HisnAvailability, error) {
		return config.HisnAvailability{Version: "0.3.1"}, nil
	})
	if err == nil {
		t.Fatalf("unapproved file: must be a startup error, got backend=%q (%s)", got.Sandbox.Backend, got.StartupBanner())
	}
	if got.Sandbox.Backend != config.SandboxBackendDocker {
		t.Errorf("unapproved file: backend must stay docker on the error path, got %q", got.Sandbox.Backend)
	}
	if !strings.Contains(err.Error(), "approved") || !strings.Contains(err.Error(), "wakil sandbox trust") {
		t.Errorf("unapproved file: error must name `approved` and `wakil sandbox trust`: %q", err)
	}

	// Once the user approves it, the default flips.
	if err := SandboxTrust(nopWriter{}, ws, tty("yes\n")); err != nil {
		t.Fatal(err)
	}
	got, err = resolveSandbox(cfg, func() (config.HisnAvailability, error) {
		return config.HisnAvailability{Version: "0.3.1"}, nil
	})
	if err != nil || got.Sandbox.Backend != config.SandboxBackendHisn {
		t.Errorf("approved file: %q err=%v", got.StartupBanner(), err)
	}
}

// A malformed .wakil/sandbox.json is also a startup error (never a docker
// fallback). The backend stays unchanged from the global default.
func TestResolveSandboxBrokenProjectFileIsStartupError(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	_ = os.MkdirAll(filepath.Join(ws, ".wakil"), 0o755)
	_ = os.WriteFile(filepath.Join(ws, ".wakil", "sandbox.json"), []byte(`{"nope":1}`), 0o644)
	cfg := defaultedCfg(t, ws)

	// broken file + global default docker → startup error, backend docker.
	got, err := resolveSandbox(cfg, func() (config.HisnAvailability, error) {
		return config.HisnAvailability{Version: "0.3.1"}, nil
	})
	if err == nil {
		t.Fatalf("broken file: must be a startup error, got %q (%s)", got.Sandbox.Backend, got.StartupBanner())
	}
	if got.Sandbox.Backend != config.SandboxBackendDocker {
		t.Errorf("broken file: backend must stay docker on the error path, got %q", got.Sandbox.Backend)
	}
	if !strings.Contains(err.Error(), "sandbox.json") {
		t.Errorf("broken file: error must name sandbox.json: %q", err)
	}
}

// A workspace that contains Wakil's state directory (e.g. $HOME) would get its
// generated profile refused by hisn: that is a startup error, not a docker
// fallback.
func TestResolveSandboxWorkspaceContainingStateDir(t *testing.T) {
	home, _ := filepath.EvalSymlinks(t.TempDir())
	t.Setenv("XDG_DATA_HOME", filepath.Join(home, ".local", "share"))
	cfg := defaultedCfg(t, home)
	got, err := resolveSandbox(cfg, func() (config.HisnAvailability, error) {
		return config.HisnAvailability{Version: "0.3.1"}, nil
	})
	if err == nil {
		t.Fatalf("workspace containing state dir: must be a startup error, got %q (%s)", got.Sandbox.Backend, got.StartupBanner())
	}
	if got.Sandbox.Backend != config.SandboxBackendDocker {
		t.Errorf("backend must stay docker on the error path, got %q", got.Sandbox.Backend)
	}
	if !strings.Contains(err.Error(), "state directory") {
		t.Errorf("error must name the state directory: %q", err)
	}
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }
