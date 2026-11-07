package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func loadDefaulted(t *testing.T, cfgJSON string, env map[string]string, args ...string) Config {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	if _, ok := env["WAKIL_SANDBOX_BACKEND"]; !ok {
		t.Setenv("WAKIL_SANDBOX_BACKEND", "")
	}
	for _, k := range []string{"WAKIL_EXEC_MODE", "ILM_EXEC_MODE", "WAKIL_WORKDIR", "ILM_WORKDIR"} {
		if _, ok := env[k]; !ok {
			t.Setenv(k, "")
		}
	}
	cfg, err := loadSandboxCfg(t, cfgJSON, append([]string{"--host-workdir", t.TempDir()}, args...)...)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

var usable = HisnAvailability{Version: "0.3.1"}

func TestBackendDefaultedFlag(t *testing.T) {
	cases := []struct {
		name string
		json string
		env  map[string]string
		args []string
		want bool
	}{
		{"nothing set", `{}`, nil, nil, true},
		{"file backend docker", `{"sandbox":{"backend":"docker"}}`, nil, nil, false},
		{"file backend hisn", `{"sandbox":{"backend":"hisn"}}`, nil, nil, false},
		{"env backend", `{}`, map[string]string{"WAKIL_SANDBOX_BACKEND": "docker"}, nil, false},
		{"flag backend", `{}`, nil, []string{"--sandbox", "docker"}, false},
		{"exec_mode docker", `{"exec_mode":"docker"}`, nil, nil, false},
		{"exec flag", `{}`, nil, []string{"--exec", "docker"}, false},
		{"direct", `{"sandbox":{"backend":"direct"}}`, nil, nil, false},
	}
	for _, c := range cases {
		cfg := loadDefaulted(t, c.json, c.env, c.args...)
		if cfg.Sandbox.BackendDefaulted != c.want {
			t.Errorf("%s: BackendDefaulted = %v, want %v", c.name, cfg.Sandbox.BackendDefaulted, c.want)
		}
		if c.want && cfg.Sandbox.Backend != SandboxBackendDocker {
			t.Errorf("%s: LoadConfig stays pure — docker until ApplyDefaultBackend, got %q", c.name, cfg.Sandbox.Backend)
		}
	}
}

// The default becomes hisn ONLY when all three hold.
func TestApplyDefaultBackendMatrix(t *testing.T) {
	ok := loadDefaulted(t, `{}`, nil)
	got := ApplyDefaultBackend(ok, usable, "")
	if got.Sandbox.Backend != SandboxBackendHisn || got.ExecMode != "docker" {
		t.Fatalf("all conditions hold: backend = %q", got.Sandbox.Backend)
	}
	real, _ := filepath.EvalSymlinks(ok.HostWorkDir)
	if got.HostWorkDir != real || got.WorkDir != real {
		t.Errorf("flipped workspace must be the identical host path: %q / %q", got.HostWorkDir, got.WorkDir)
	}
	if !strings.Contains(got.StartupBanner(), "backend=hisn") || !strings.Contains(got.StartupBanner(), "default: hisn 0.3.1") {
		t.Errorf("banner: %q", got.StartupBanner())
	}
	if got.BrowserEnabled {
		t.Error("browser is forced off under hisn")
	}

	stays := map[string]HisnAvailability{
		"not found": {Reason: "hisn not found in PATH"},
		"too old":   {Version: "0.2.2", Reason: "hisn 0.2.2 is older than 0.3.1"},
		"check":     {Version: "0.3.1", Reason: "hisn check failed: user namespaces"},
	}
	for name, av := range stays {
		g := ApplyDefaultBackend(ok, av, "")
		if g.Sandbox.Backend != SandboxBackendDocker {
			t.Errorf("%s: must stay docker, got %q", name, g.Sandbox.Backend)
		}
		if !strings.Contains(g.StartupBanner(), "backend=docker") || !strings.Contains(g.StartupBanner(), "default: docker ("+av.Reason+")") {
			t.Errorf("%s: the reason must be in the banner: %q", name, g.StartupBanner())
		}
		if g.WorkDir != ok.WorkDir {
			t.Errorf("%s: docker workspace must be untouched: %q", name, g.WorkDir)
		}
	}
	if g := ApplyDefaultBackend(ok, usable, "sandbox.json is not approved"); g.Sandbox.Backend != SandboxBackendDocker ||
		!strings.Contains(g.StartupBanner(), "not approved") {
		t.Errorf("an extra reason keeps docker and shows up: %q", g.StartupBanner())
	}
	// --workdir is docker-only.
	wd := loadDefaulted(t, `{}`, nil, "--workdir", "/work")
	if g := ApplyDefaultBackend(wd, usable, ""); g.Sandbox.Backend != SandboxBackendDocker || !strings.Contains(g.StartupBanner(), "--workdir") {
		t.Errorf("explicit workdir: %q", g.StartupBanner())
	}
}

// An explicit choice is never second-guessed, in either direction.
func TestApplyDefaultBackendRespectsExplicit(t *testing.T) {
	for name, cfg := range map[string]Config{
		"explicit docker": loadDefaulted(t, `{"sandbox":{"backend":"docker"}}`, nil),
		"explicit direct": loadDefaulted(t, `{"sandbox":{"backend":"direct"}}`, nil),
		"exec_mode":       loadDefaulted(t, `{"exec_mode":"docker"}`, nil),
		"env":             loadDefaulted(t, `{}`, map[string]string{"WAKIL_SANDBOX_BACKEND": "docker"}),
	} {
		before := cfg.Sandbox.Backend
		g := ApplyDefaultBackend(cfg, usable, "")
		if g.Sandbox.Backend != before || g.Sandbox.DefaultNote != "" {
			t.Errorf("%s: backend %q → %q, note %q; an explicit backend is never changed", name, before, g.Sandbox.Backend, g.Sandbox.DefaultNote)
		}
	}
	// An explicit hisn stays hisn even when the probe says it is not usable
	// (the backend's own preflight reports that with no fallback).
	h := loadDefaulted(t, `{"sandbox":{"backend":"hisn"}}`, nil)
	if g := ApplyDefaultBackend(h, HisnAvailability{Reason: "hisn not found in PATH"}, ""); g.Sandbox.Backend != SandboxBackendHisn {
		t.Errorf("explicit hisn must not be downgraded: %q", g.Sandbox.Backend)
	}
}

func TestApplyDefaultBackendKeepsKVRState(t *testing.T) {
	cfg := loadDefaulted(t, `{}`, nil)
	if cfg.KVRDisabled {
		t.Fatal("docker default has kvr on")
	}
	if g := ApplyDefaultBackend(cfg, usable, ""); g.KVRDisabled {
		t.Error("under hisn kvr is decided by the backend plan, not disabled by config")
	}
}

// TestMinHisnDefaultVersionNotBelowMinHisnVersion pins the invariant that a
// host can never be auto-selected as the hisn default and then refused by the
// explicit-backend version gate.
func TestMinHisnDefaultVersionNotBelowMinHisnVersion(t *testing.T) {
	if MinHisnDefaultVersion != MinHisnVersion {
		t.Fatalf("MinHisnDefaultVersion %q must equal MinHisnVersion %q",
			MinHisnDefaultVersion, MinHisnVersion)
	}
}
