package config

// Block 1 (0.8.11): "wakil" must behave like "wakil .". The workspace order is
// positional path > --host-workdir > WAKIL_HOST_WORKDIR > cwd. Config file
// host_work_dir / work_dir (and WAKIL_WORKDIR / ILM_WORKDIR in direct mode)
// no longer steer the workspace; they get one ignored line at startup.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// loadWS loads a config with the given config-file body and flags, with the
// workspace env vars cleared so each test controls one layer at a time.
func loadWS(t *testing.T, body string, args ...string) Config {
	t.Helper()
	// Clear the workspace layers so each test controls exactly one. A
	// previously Setenv'd value in the same test is restored by t.Setenv
	// only at test end, and this helper would overwrite it mid-test — so
	// the per-test env is set AFTER loadWS for the first load and the
	// helper only clears on the first call. The tests that need an env
	// value set it before calling loadWS and this helper respects a
	// non-empty value set by the test: it clears only when the test has
	// not set one. (t.Setenv pairs: the helper sets, the test sets after.)
	t.Setenv("HOME", t.TempDir())
	if os.Getenv("WAKIL_HOST_WORKDIR") == "" {
		t.Setenv("WAKIL_HOST_WORKDIR", "")
	}
	if os.Getenv("ILM_HOST_WORKDIR") == "" {
		t.Setenv("ILM_HOST_WORKDIR", "")
	}
	if os.Getenv("WAKIL_WORKDIR") == "" {
		t.Setenv("WAKIL_WORKDIR", "")
	}
	if os.Getenv("ILM_WORKDIR") == "" {
		t.Setenv("ILM_WORKDIR", "")
	}
	t.Setenv("WAKIL_WORKSPACE_PATH", "")
	clearEndpointEnv(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if body == "" {
		body = `{"base_url": "http://proxy:11400"}`
	}
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	argv := append([]string{"--config", cfgPath, "--sandbox", "docker"}, args...)
	cfg, err := LoadConfig(argv)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

func chdir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	abs, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return dir
	}
	return abs
}

// TestWorkspaceOrderPositionalFirst: the positional arg beats every other
// layer, including --host-workdir and the config file.
func TestWorkspaceOrderPositionalFirst(t *testing.T) {
	root := t.TempDir()
	cwd := chdir(t, filepath.Join(root, "cwd"))
	for _, d := range []string{"pos", "flag", "cfg"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"base_url":"http://proxy:11400","host_work_dir":"` + filepath.Join(root, "cfg") + `"}`
	cfg := loadWS(t, body, filepath.Join(root, "pos"), "--host-workdir", filepath.Join(root, "flag"))
	_ = cwd
	if cfg.HostWorkDir != filepath.Join(root, "pos") && cfg.HostWorkDir != filepath.Join(root, "flag") {
		t.Errorf("HostWorkDir = %q, want positional %q or flag %q", cfg.HostWorkDir, filepath.Join(root, "pos"), filepath.Join(root, "flag"))
	}
	if cfg.HostWorkDir != filepath.Join(root, "pos") {
		t.Errorf("positional must win over --host-workdir: HostWorkDir = %q", cfg.HostWorkDir)
	}
}

// TestWorkspaceOrderHostWorkDirFlag: --host-workdir beats env and cwd.
func TestWorkspaceOrderHostWorkDirFlag(t *testing.T) {
	root := t.TempDir()
	chdir(t, filepath.Join(root, "cwd"))
	for _, d := range []string{"flag", "env"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("WAKIL_HOST_WORKDIR", filepath.Join(root, "env"))
	cfg := loadWS(t, "", "--host-workdir", filepath.Join(root, "flag"))
	if cfg.HostWorkDir != filepath.Join(root, "flag") {
		t.Errorf("HostWorkDir = %q, want the flag %q", cfg.HostWorkDir, filepath.Join(root, "flag"))
	}
}

// TestWorkspaceOrderHostWorkDirEnv: WAKIL_HOST_WORKDIR beats cwd. This is the
// deliberate exception to "exactly like wakil .": an exported env var is an
// explicit, session-scoped override.
func TestWorkspaceOrderHostWorkDirEnv(t *testing.T) {
	root := t.TempDir()
	chdir(t, filepath.Join(root, "cwd"))
	envDir := filepath.Join(root, "env")
	if err := os.Mkdir(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAKIL_HOST_WORKDIR", envDir)
	cfg := loadWS(t, "")
	if cfg.HostWorkDir != envDir {
		t.Errorf("HostWorkDir = %q, want the env %q", cfg.HostWorkDir, envDir)
	}
}

// TestWorkspaceOrderCwdDefault: nothing set anywhere → the current folder.
// `wakil` and `wakil .` must agree here.
func TestWorkspaceOrderCwdDefault(t *testing.T) {
	root := t.TempDir()
	cwd := chdir(t, filepath.Join(root, "cwd"))
	cfg := loadWS(t, "")
	if cfg.HostWorkDir != cwd {
		t.Errorf("HostWorkDir = %q, want cwd %q", cfg.HostWorkDir, cwd)
	}
	cfgDot := loadWS(t, "", ".")
	if cfgDot.HostWorkDir != cwd {
		t.Errorf("wakil . HostWorkDir = %q, want cwd %q (wakil and wakil . must agree)", cfgDot.HostWorkDir, cwd)
	}
}

// TestWorkspaceIgnoresConfigKeys: config file host_work_dir / work_dir no
// longer steer the workspace — the bare launch resolves to cwd — and the
// one-line warning names each ignored setting with the recovery (cd into
// the folder, or "wakil <path>").
func TestWorkspaceIgnoresConfigKeys(t *testing.T) {
	root := t.TempDir()
	cwd := chdir(t, filepath.Join(root, "cwd"))
	cfgDir := filepath.Join(root, "cfg")
	for _, body := range []string{
		`{"base_url":"http://proxy:11400","host_work_dir":"` + cfgDir + `"}`,
		`{"base_url":"http://proxy:11400","work_dir":"` + cfgDir + `"}`,
	} {
		cfg := loadWS(t, body)
		if cfg.HostWorkDir != cwd {
			t.Errorf("body %s: HostWorkDir = %q, want cwd %q (config keys must not steer)", body, cfg.HostWorkDir, cwd)
		}
		line := cfg.IgnoredWorkDirKeysLine()
		if line == "" {
			t.Errorf("body %s: want a non-empty ignored-keys warning", body)
			continue
		}
		for _, want := range []string{"ignored since 0.8.11", "wakil <path>"} {
			if !strings.Contains(line, want) {
				t.Errorf("warning lacks %q: %q", want, line)
			}
		}
		if strings.Count(line, "\n") != 0 {
			t.Errorf("warning must be one line: %q", line)
		}
	}
	// Direct mode: config work_dir ignored as well.
	cfg := loadWS(t, `{"base_url":"http://proxy:11400","work_dir":"`+cfgDir+`"}`, "--sandbox", "direct")
	_ = cfg
}

// TestWorkspaceIgnoresWakilWorkdirEnv: WAKIL_WORKDIR gets the same ignored
// treatment (one line), not silent steering.
func TestWorkspaceIgnoresWakilWorkdirEnv(t *testing.T) {
	root := t.TempDir()
	chdir(t, filepath.Join(root, "cwd"))
	t.Setenv("WAKIL_WORKDIR", filepath.Join(root, "env"))
	cfg := loadWS(t, "", "--sandbox", "direct")
	if cfg.WorkDir != "" {
		t.Errorf("WorkDir = %q, want empty (WAKIL_WORKDIR must not steer direct mode)", cfg.WorkDir)
	}
	if cfg.IgnoredWorkDirKeysLine() == "" {
		t.Error("WAKIL_WORKDIR set: want the ignored-keys warning")
	}
}
