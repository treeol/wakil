package main

import (
	"os"
	"path/filepath"
	"testing"

	"wakil/internal/config"
)

// isolateConfig points WAKIL_CONFIG at a nonexistent path so LoadConfig doesn't
// pick up the developer's real ~/.config/wakil/config.json during tests.
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv("WAKIL_CONFIG", filepath.Join(t.TempDir(), "none.json"))
}

func TestWorkspacePositionalDocker(t *testing.T) {
	isolateConfig(t)
	dir := t.TempDir()
	cfg, err := config.LoadConfig([]string{"--base-url", "http://x:1", dir})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HostWorkDir != dir {
		t.Fatalf("HostWorkDir = %q, want %q", cfg.HostWorkDir, dir)
	}
	want := "/mnt/" + filepath.Base(dir)
	if cfg.WorkDir != want {
		t.Fatalf("WorkDir = %q, want %q", cfg.WorkDir, want)
	}
}

func TestWorkspaceDefaultsToCwdDocker(t *testing.T) {
	isolateConfig(t)
	cfg, err := config.LoadConfig([]string{"--base-url", "http://x:1"})
	if err != nil {
		t.Fatal(err)
	}
	wd, _ := os.Getwd()
	if cfg.HostWorkDir != wd {
		t.Fatalf("HostWorkDir = %q, want cwd %q", cfg.HostWorkDir, wd)
	}
}

func TestWorkspacePositionalDirect(t *testing.T) {
	isolateConfig(t)
	dir := t.TempDir()
	cfg, err := config.LoadConfig([]string{"--base-url", "http://x:1", "--exec", "direct", dir})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkDir != dir {
		t.Fatalf("WorkDir = %q, want %q", cfg.WorkDir, dir)
	}
}

func TestWorkspacePositionalOverridesFlag(t *testing.T) {
	isolateConfig(t)
	dir := t.TempDir()
	cfg, err := config.LoadConfig([]string{"--base-url", "http://x:1", "--host-workdir", "/some/other", dir})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HostWorkDir != dir {
		t.Fatalf("positional should win: HostWorkDir = %q, want %q", cfg.HostWorkDir, dir)
	}
}

func TestWorkspaceRejectsNonDir(t *testing.T) {
	isolateConfig(t)
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	if _, err := config.LoadConfig([]string{"--base-url", "http://x:1", missing}); err == nil {
		t.Fatal("expected error for non-directory workspace path")
	}
}

func TestTraceConfig(t *testing.T) {
	// trace_sessions: false by default — Trace off, TraceDir empty.
	isolateConfig(t)
	cfg, err := config.LoadConfig([]string{"--base-url", "http://x:1"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Trace {
		t.Error("Trace should be false by default")
	}
	if cfg.TraceDir != "" {
		t.Errorf("TraceDir should be empty when tracing is off; got %q", cfg.TraceDir)
	}

	// --trace flag enables tracing and derives a default TraceDir.
	t.Setenv("HOME", t.TempDir()) // isolate XDG fallback
	t.Setenv("XDG_DATA_HOME", "")
	cfg, err = config.LoadConfig([]string{"--base-url", "http://x:1", "--trace"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Trace {
		t.Error("--trace flag should set Trace=true")
	}
	if cfg.TraceDir == "" {
		t.Error("TraceDir should be derived when --trace is set without --trace-dir")
	}

	// --trace-dir overrides the default.
	explicit := t.TempDir()
	cfg, err = config.LoadConfig([]string{"--base-url", "http://x:1", "--trace", "--trace-dir", explicit})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TraceDir != explicit {
		t.Errorf("TraceDir = %q, want %q", cfg.TraceDir, explicit)
	}

	// Config file trace_sessions:true → Trace=true without --trace flag.
	dir := t.TempDir()
	cfgFile := filepath.Join(dir, "config.json")
	traceStore := filepath.Join(dir, "traces")
	if err := os.WriteFile(cfgFile, []byte(`{"base_url":"http://x:1","trace_sessions":true,"trace_dir":"`+traceStore+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAKIL_CONFIG", cfgFile)
	cfg, err = config.LoadConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Trace {
		t.Error("trace_sessions:true in config should set Trace=true")
	}
	if cfg.TraceDir != traceStore {
		t.Errorf("TraceDir = %q, want %q", cfg.TraceDir, traceStore)
	}

	// WAKIL_TRACE_SESSIONS env var enables tracing.
	t.Setenv("WAKIL_CONFIG", filepath.Join(t.TempDir(), "none.json"))
	t.Setenv("WAKIL_TRACE_SESSIONS", "true")
	t.Setenv("WAKIL_TRACE_DIR", "/custom/traces")
	cfg, err = config.LoadConfig([]string{"--base-url", "http://x:1"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Trace {
		t.Error("WAKIL_TRACE_SESSIONS=true should set Trace=true")
	}
	if cfg.TraceDir != "/custom/traces" {
		t.Errorf("TraceDir = %q, want /custom/traces", cfg.TraceDir)
	}
}

func TestDockerSocketDefault(t *testing.T) {
	isolateConfig(t)
	// Docker socket is on by default; image stays unchanged (no auto-switch).
	cfg, err := config.LoadConfig([]string{"--base-url", "http://x:1"})
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DockerSocket {
		t.Fatal("DockerSocket should be true by default")
	}
	if cfg.Image != "wakil-dev" {
		t.Fatalf("Image = %q, want wakil-dev", cfg.Image)
	}

	// --docker-sock=false disables passthrough.
	cfg, err = config.LoadConfig([]string{"--base-url", "http://x:1", "--docker-sock=false"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DockerSocket {
		t.Fatal("--docker-sock=false should disable socket passthrough")
	}

	// Explicit image is always respected.
	cfg, err = config.LoadConfig([]string{"--base-url", "http://x:1", "--image", "myorg/node-docker:latest"})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Image != "myorg/node-docker:latest" {
		t.Fatalf("explicit image overridden: got %q", cfg.Image)
	}
}
