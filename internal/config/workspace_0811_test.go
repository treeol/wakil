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
// A test that needs an env value (e.g. WAKIL_HOST_WORKDIR) sets it BEFORE
// calling loadWS: the helper clears only when the test has NOT set one
// (the non-empty check below), so the test's value survives; it never
// re-clears on a later call in the same test.
func loadWS(t *testing.T, body string, args ...string) Config {
	t.Helper()
	// Clear the workspace layers so each test controls exactly one.
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
	chdir(t, filepath.Join(root, "cwd"))
	for _, d := range []string{"pos", "flag", "cfg"} {
		if err := os.Mkdir(filepath.Join(root, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	real := func(p string) string {
		r, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	body := `{"base_url":"http://proxy:11400","host_work_dir":"` + filepath.Join(root, "cfg") + `"}`
	cfg := loadWS(t, body, filepath.Join(root, "pos"), "--host-workdir", filepath.Join(root, "flag"))
	pos, flag := real(filepath.Join(root, "pos")), real(filepath.Join(root, "flag"))
	if cfg.HostWorkDir != pos {
		t.Errorf("positional must win over --host-workdir: HostWorkDir = %q, want %q (flag was %q)", cfg.HostWorkDir, pos, flag)
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
	flagReal, err := filepath.EvalSymlinks(filepath.Join(root, "flag"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAKIL_HOST_WORKDIR", filepath.Join(root, "env"))
	// Note: loadWS is called AFTER this Setenv, and its env guard respects
	// this non-empty value (clears only the unset layers).
	cfg := loadWS(t, "", "--host-workdir", filepath.Join(root, "flag"))
	if cfg.HostWorkDir != flagReal {
		t.Errorf("HostWorkDir = %q, want the flag %q", cfg.HostWorkDir, flagReal)
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
	envReal, err := filepath.EvalSymlinks(envDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAKIL_HOST_WORKDIR", envDir)
	cfg := loadWS(t, "")
	if cfg.HostWorkDir != envReal {
		t.Errorf("HostWorkDir = %q, want the env %q", cfg.HostWorkDir, envReal)
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
	// Direct mode: config work_dir ignored as well — the resolved workspace
	// is the cwd and the executor root follows it (empty WorkDir = cwd).
	dc := loadWS(t, `{"base_url":"http://proxy:11400","work_dir":"`+cfgDir+`"}`, "--sandbox", "direct")
	if dc.WorkDir != "" {
		t.Errorf("direct: WorkDir = %q, want empty (cwd fallback; config work_dir ignored)", dc.WorkDir)
	}
	if dc.ResolvedWorkspace != cwd {
		t.Errorf("direct: ResolvedWorkspace = %q, want cwd %q", dc.ResolvedWorkspace, cwd)
	}
	if dc.IgnoredWorkDirKeysLine() == "" {
		t.Error("direct: want the ignored-keys warning")
	}
	// WorkDirExplicit is true here: the config-file work_dir sets the
	// docker in-container path even though it no longer steers the
	// workspace. Direct mode has no in-container path, but the flag is
	// set before the direct branch runs, so it stays true.
	if !dc.Sandbox.WorkDirExplicit {
		t.Error("direct: WorkDirExplicit should stay true for a config work_dir (it still sets the docker in-container path)")
	}
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
