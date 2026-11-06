package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadSandboxCfg writes cfgJSON and loads it with the given extra args.
func loadSandboxCfg(t *testing.T, cfgJSON string, args ...string) (Config, error) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, []byte(cfgJSON), 0o644); err != nil {
		t.Fatal(err)
	}
	return LoadConfig(append([]string{"--config", p, "--base-url", "http://proxy:11400"}, args...))
}

func TestSandboxBackendDefaultsToDocker(t *testing.T) {
	cfg, err := loadSandboxCfg(t, `{}`, "--host-workdir", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Sandbox.Backend != SandboxBackendDocker {
		t.Errorf("backend = %q, want docker", cfg.Sandbox.Backend)
	}
	if !strings.HasPrefix(cfg.WorkDir, "/mnt/") {
		t.Errorf("docker WorkDir = %q, want /mnt/<name> (behaviour must be unchanged)", cfg.WorkDir)
	}
}

func TestSandboxBackendUnknownIsError(t *testing.T) {
	for _, via := range []struct {
		name string
		cfg  string
		args []string
	}{
		{"file", `{"sandbox":{"backend":"hsin"}}`, nil},
		{"flag", `{}`, []string{"--sandbox", "hsin"}},
	} {
		t.Run(via.name, func(t *testing.T) {
			_, err := loadSandboxCfg(t, via.cfg, append(via.args, "--host-workdir", t.TempDir())...)
			if err == nil || !strings.Contains(err.Error(), "invalid sandbox backend") {
				t.Fatalf("want invalid sandbox backend error, got %v", err)
			}
		})
	}
	t.Run("env", func(t *testing.T) {
		t.Setenv("WAKIL_SANDBOX_BACKEND", "hsin")
		_, err := loadSandboxCfg(t, `{}`, "--host-workdir", t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "invalid sandbox backend") {
			t.Fatalf("want invalid sandbox backend error, got %v", err)
		}
	})
}

func TestSandboxHisnWorkdirIsHostPath(t *testing.T) {
	dir := t.TempDir()
	real, _ := filepath.EvalSymlinks(dir)
	cfg, err := loadSandboxCfg(t, `{"sandbox":{"backend":"hisn"}}`, "--host-workdir", dir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkDir != real || cfg.HostWorkDir != real {
		t.Errorf("WorkDir=%q HostWorkDir=%q, want both %q", cfg.WorkDir, cfg.HostWorkDir, real)
	}
	if cfg.KVRDisabled {
		t.Error("under hisn kvr is decided when the backend is planned (in-sandbox service), not forced off by config")
	}
	if got := cfg.Sandbox.Hisn.Binary; got != "hisn" {
		t.Errorf("binary default = %q", got)
	}
	if len(cfg.Sandbox.Hisn.EnvPass) != len(DefaultHisnEnvPass) {
		t.Errorf("env_pass default = %v", cfg.Sandbox.Hisn.EnvPass)
	}
}

func TestSandboxHisnResolvesSymlinkedWorkspace(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(dir, link); err != nil {
		t.Skip("symlinks unavailable")
	}
	real, _ := filepath.EvalSymlinks(dir)
	cfg, err := loadSandboxCfg(t, `{"sandbox":{"backend":"hisn"}}`, "--host-workdir", link)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkDir != real {
		t.Errorf("WorkDir = %q, want canonical %q", cfg.WorkDir, real)
	}
}

func TestSandboxHisnRejections(t *testing.T) {
	ws := t.TempDir()
	for _, tc := range []struct {
		name, cfg, want string
		args            []string
	}{
		{"direct mode", `{"sandbox":{"backend":"hisn"}}`, "conflicts with exec mode", []string{"--exec", "direct"}},
		{"workdir flag", `{"sandbox":{"backend":"hisn"}}`, "conflicts with sandbox backend hisn", []string{"--workdir", "/mnt/x"}},
		{"relative home_root", `{"sandbox":{"backend":"hisn","hisn":{"home_root":"rel"}}}`, "home_root", nil},
		{"grantable root", `{"sandbox":{"backend":"hisn","hisn":{"grantable_rw":["/"]}}}`, "grantable_rw", nil},
		{"grantable relative", `{"sandbox":{"backend":"hisn","hisn":{"grantable_rw":["out"]}}}`, "grantable_rw", nil},
		{"env LD_PRELOAD", `{"sandbox":{"backend":"hisn","hisn":{"env_pass":["LD_PRELOAD"]}}}`, "not allowed", nil},
		{"env PATH", `{"sandbox":{"backend":"hisn","hisn":{"env_pass":["PATH"]}}}`, "not allowed", nil},
		{"env bad name", `{"sandbox":{"backend":"hisn","hisn":{"env_pass":["A=B"]}}}`, "invalid name", nil},
		{"env dup", `{"sandbox":{"backend":"hisn","hisn":{"env_pass":["CI","CI"]}}}`, "duplicate", nil},
		{"rootfs bad form", `{"sandbox":{"backend":"hisn","hisn":{"rootfs":"has space"}}}`, "sandbox.hisn.rootfs", nil},
		{"rootfs relative path", `{"sandbox":{"backend":"hisn","hisn":{"rootfs":"rel/dir"}}}`, "sandbox.hisn.rootfs", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{}, tc.args...)
			if tc.name != "direct mode" {
				args = append(args, "--host-workdir", ws)
			}
			_, err := loadSandboxCfg(t, tc.cfg, args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

// TestExecModeDeprecatedAlias: exec_mode maps onto sandbox.backend (with a
// warning) and ExecMode is derived from the backend afterwards.
func TestExecModeDeprecatedAlias(t *testing.T) {
	for _, tc := range []struct {
		name, mode, backend   string
		wantBackend, wantMode string
		wantWarn              bool
	}{
		{"defaults", "", "", "docker", "docker", false},
		{"exec docker", "docker", "", "docker", "docker", true},
		{"exec direct", "direct", "", "direct", "direct", true},
		{"backend direct", "", "direct", "direct", "direct", false},
		{"backend hisn", "", "hisn", "hisn", "docker", false},
		{"exec docker + hisn", "docker", "hisn", "hisn", "docker", true},
		{"exec direct + direct", "direct", "direct", "direct", "direct", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{ExecMode: tc.mode, Sandbox: SandboxConfig{Backend: tc.backend}}
			warn, err := resolveBackend(&cfg)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Sandbox.Backend != tc.wantBackend || cfg.ExecMode != tc.wantMode {
				t.Errorf("backend=%q mode=%q, want %q/%q", cfg.Sandbox.Backend, cfg.ExecMode, tc.wantBackend, tc.wantMode)
			}
			if (warn != "") != tc.wantWarn {
				t.Errorf("warning = %q, want present=%v", warn, tc.wantWarn)
			}
			if warn != "" && strings.Count(warn, "\n") != 0 {
				t.Errorf("warning must be one line: %q", warn)
			}
		})
	}
	for _, bad := range []struct{ mode, backend, want string }{
		{"direct", "hisn", "conflicts with exec mode"},
		{"direct", "docker", "conflicts with exec mode"},
		{"docker", "direct", "conflicts with exec mode"},
		{"bare", "", "invalid exec mode"},
		{"", "hsin", "invalid sandbox backend"},
	} {
		cfg := Config{ExecMode: bad.mode, Sandbox: SandboxConfig{Backend: bad.backend}}
		if _, err := resolveBackend(&cfg); err == nil || !strings.Contains(err.Error(), bad.want) {
			t.Errorf("%q/%q: want error %q, got %v", bad.mode, bad.backend, bad.want, err)
		}
	}
}

// TestSandboxHisnForcesBrowserOff: one config serves both backends — under
// hisn browser_enabled is switched off with a reason, not a config error;
// under docker it stays on. The banner states backend + browser state.
func TestSandboxHisnForcesBrowserOff(t *testing.T) {
	cfg, err := loadSandboxCfg(t, `{"browser_enabled":true,"sandbox":{"backend":"hisn"}}`, "--host-workdir", t.TempDir())
	if err != nil {
		t.Fatalf("browser_enabled under hisn must not be an error: %v", err)
	}
	if cfg.BrowserEnabled || cfg.BrowserForcedOff == "" {
		t.Errorf("BrowserEnabled=%v ForcedOff=%q, want false + reason", cfg.BrowserEnabled, cfg.BrowserForcedOff)
	}
	b := cfg.StartupBanner()
	if !strings.Contains(b, "backend=hisn") || !strings.Contains(b, "browser tools: off (") || strings.Contains(b, "\n") {
		t.Errorf("banner = %q", b)
	}

	cfg, err = loadSandboxCfg(t, `{"browser_enabled":true}`, "--host-workdir", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.BrowserEnabled || cfg.BrowserForcedOff != "" {
		t.Errorf("docker: BrowserEnabled=%v ForcedOff=%q, want unchanged", cfg.BrowserEnabled, cfg.BrowserForcedOff)
	}
	if b := cfg.StartupBanner(); b != "sandbox: backend=docker · browser tools: on" {
		t.Errorf("docker banner = %q", b)
	}
}

func TestSandboxBackendDirectViaConfig(t *testing.T) {
	ws := t.TempDir()
	cfg, err := loadSandboxCfg(t, `{"sandbox":{"backend":"direct"}}`, ws)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ExecMode != "direct" || cfg.WorkDir != ws {
		t.Errorf("ExecMode=%q WorkDir=%q, want direct/%q", cfg.ExecMode, cfg.WorkDir, ws)
	}
	if !cfg.KVRDisabled {
		t.Error("kvr must be disabled outside docker")
	}
}

func TestSandboxHisnEnvPassOverride(t *testing.T) {
	cfg, err := loadSandboxCfg(t, `{"sandbox":{"backend":"hisn","hisn":{"env_pass":["CI"],"grantable_rw":["/srv/out/"]}}}`, "--host-workdir", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Sandbox.Hisn.EnvPass; len(got) != 1 || got[0] != "CI" {
		t.Errorf("env_pass = %v", got)
	}
	if got := cfg.Sandbox.Hisn.GrantableRW; len(got) != 1 || got[0] != "/srv/out" {
		t.Errorf("grantable_rw = %v (must be cleaned)", got)
	}
}

// TestAllowUsernsBothKeys: the documented sandbox.docker.allow_userns and the
// legacy top-level docker_allow_userns both enable the flag (the documented
// key used to be silently ignored).
func TestAllowUsernsBothKeys(t *testing.T) {
	for _, js := range []string{`{"sandbox":{"docker":{"allow_userns":true}}}`, `{"docker_allow_userns":true}`} {
		cfg, err := loadSandboxCfg(t, js, "--host-workdir", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.DockerAllowUserns || !cfg.Sandbox.Docker.AllowUserns {
			t.Errorf("%s: DockerAllowUserns=%v Sandbox.Docker.AllowUserns=%v", js, cfg.DockerAllowUserns, cfg.Sandbox.Docker.AllowUserns)
		}
	}
	cfg, _ := loadSandboxCfg(t, `{}`, "--host-workdir", t.TempDir())
	if cfg.DockerAllowUserns {
		t.Error("default must be off")
	}
}

// P3: signing needs the agent under hisn; the agent needs a fingerprint list.
func TestSandboxHisnSSHSigningNeedsAgent(t *testing.T) {
	ws := t.TempDir()
	cfg := `{"sandbox":{"backend":"hisn"}}`
	_, err := loadSandboxCfg(t, cfg, "--host-workdir", ws, "--ssh-signing", "/tmp/k.pub")
	if err == nil || !strings.Contains(err.Error(), "ssh_signing needs the agent") {
		t.Fatalf("signing without agent must be a load error, got %v", err)
	}
	if _, err := loadSandboxCfg(t, `{"sandbox":{"backend":"hisn","hisn":{"ssh_agent":true,"ssh_keys":["SHA256:abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG"]}}}`, "--host-workdir", ws, "--ssh-signing", "/tmp/k.pub"); err != nil {
		t.Fatalf("signing with agent+key must load: %v", err)
	}
}

func TestSandboxHisnSSHKeyFormat(t *testing.T) {
	ws := t.TempDir()
	_, err := loadSandboxCfg(t, `{"sandbox":{"backend":"hisn","hisn":{"ssh_keys":["ssh-ed25519 AAAA"]}}}`, "--host-workdir", ws)
	if err == nil || !strings.Contains(err.Error(), "SHA256") {
		t.Fatalf("ssh_keys must be fingerprints, got %v", err)
	}
}
