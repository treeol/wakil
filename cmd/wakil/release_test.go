package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/buildinfo"
	"github.com/treeol/wakil/internal/config"
)

const pinned = "ghcr.io/treeol/wakil-sandbox@sha256:" +
	"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func setPinned(t *testing.T, v string) {
	t.Helper()
	old := buildinfo.SandboxImage
	buildinfo.SandboxImage = v
	t.Cleanup(func() { buildinfo.SandboxImage = old })
}

func TestSandboxImageResolutionOrder(t *testing.T) {
	writeCfg := func(t *testing.T, body string) string {
		p := filepath.Join(t.TempDir(), "c.json")
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	base := []string{"--base-url", "http://x:1"}
	tests := []struct {
		name   string
		pinned string
		cfg    string
		env    string
		args   []string
		want   string
	}{
		{"dev fallback", "", "", "", nil, "wakil-dev"},
		{"pinned beats dev", pinned, "", "", nil, pinned},
		{"config sandbox.image beats pinned", pinned, `{"sandbox":{"image":"cfg/img:1"}}`, "", nil, "cfg/img:1"},
		{"sandbox.image beats legacy image", pinned, `{"image":"legacy:1","sandbox":{"image":"cfg/img:1"}}`, "", nil, "cfg/img:1"},
		{"legacy image beats pinned", pinned, `{"image":"legacy:1"}`, "", nil, "legacy:1"},
		{"flag beats config", pinned, `{"sandbox":{"image":"cfg/img:1"}}`, "", []string{"--sandbox-image", "flag/img:2"}, "flag/img:2"},
		{"env beats config", pinned, `{"sandbox":{"image":"cfg/img:1"}}`, "env/img:3", nil, "env/img:3"},
		{"flag beats env", pinned, "", "env/img:3", []string{"--sandbox-image", "flag/img:2"}, "flag/img:2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setPinned(t, tc.pinned)
			cfgFile := filepath.Join(t.TempDir(), "none.json")
			if tc.cfg != "" {
				cfgFile = writeCfg(t, tc.cfg)
			}
			t.Setenv("WAKIL_CONFIG", cfgFile)
			t.Setenv("WAKIL_IMAGE", tc.env)
			t.Setenv("ILM_CONTAINER_IMAGE", "")
			cfg, err := config.LoadConfig(append(append([]string{}, base...), tc.args...))
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Image != tc.want {
				t.Fatalf("Image = %q, want %q", cfg.Image, tc.want)
			}
		})
	}
}

func TestPrintVersion(t *testing.T) {
	oldV, oldC, oldD := buildinfo.Version, buildinfo.Commit, buildinfo.Date
	t.Cleanup(func() { buildinfo.Version, buildinfo.Commit, buildinfo.Date = oldV, oldC, oldD })
	buildinfo.Version, buildinfo.Commit, buildinfo.Date = "v1.2.3", "abc1234", "2026-01-02T03:04:05Z"
	setPinned(t, pinned)
	var b bytes.Buffer
	printVersion(&b)
	for _, want := range []string{"v1.2.3", "abc1234", "2026-01-02T03:04:05Z", pinned, "go1."} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("version output missing %q:\n%s", want, b.String())
		}
	}
	if !strings.Contains(b.String(), "sha256:") {
		t.Error("version output must show the full sandbox digest")
	}
}

func TestDoctorNoRuntime(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no docker/podman
	t.Setenv("WAKIL_CONFIG", filepath.Join(t.TempDir(), "none.json"))
	var b bytes.Buffer
	run := func(context.Context, string, ...string) (string, error) { return "", errors.New("unexpected") }
	if rc := runDoctor(&b, nil, run); rc == 0 {
		t.Fatalf("exit code = 0, want non-zero; output:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "no usable container runtime") {
		t.Errorf("missing runtime failure message:\n%s", b.String())
	}
}

func TestDoctorRuntimeAndMissingImage(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	setPinned(t, pinned)
	cfgFile := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(cfgFile, []byte(`{"base_url":"http://x:1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	run := func(_ context.Context, _ string, args ...string) (string, error) {
		if len(args) > 0 && args[0] == "image" {
			return "No such image", errors.New("exit 1")
		}
		return "id", nil
	}
	var b bytes.Buffer
	if rc := runDoctor(&b, []string{"--config", cfgFile}, run); rc != 0 {
		t.Fatalf("exit code = %d, want 0:\n%s", rc, b.String())
	}
	for _, want := range []string{"daemon reachable", cfgFile + " loaded", "docker pull " + pinned} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("doctor output missing %q:\n%s", want, b.String())
		}
	}
}

// fakeHisnDir returns a PATH directory holding a `hisn` that reports ver and
// whose `check` exits checkRC.
func fakeHisnDir(t *testing.T, ver string, checkRC int) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\ncase \"$1\" in\n--version) echo 'hisn " + ver + "';;\ncheck) " +
		"[ " + strconv.Itoa(checkRC) + " -ne 0 ] && echo '[FAIL] user namespaces' ; exit " + strconv.Itoa(checkRC) + ";;\nesac\n"
	if err := os.WriteFile(filepath.Join(dir, "hisn"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestDoctorHisnDefaultNeedsNoDocker(t *testing.T) {
	t.Setenv("PATH", fakeHisnDir(t, "0.4.2", 0)) // no docker/podman, hisn usable
	for _, k := range []string{"WAKIL_SANDBOX_BACKEND", "WAKIL_EXEC_MODE", "ILM_EXEC_MODE"} {
		t.Setenv(k, "")
	}
	cfgFile := filepath.Join(t.TempDir(), "c.json")
	_ = os.WriteFile(cfgFile, []byte(`{"base_url":"http://x:1"}`), 0o600)
	var b bytes.Buffer
	run := func(context.Context, string, ...string) (string, error) { return "", errors.New("unexpected") }
	if rc := runDoctor(&b, []string{"--config", cfgFile}, run); rc != 0 {
		t.Fatalf("with hisn as the default sandbox a missing docker is not fatal, rc=%d:\n%s", rc, b.String())
	}
	for _, want := range []string{"hisn 0.4.2", "the default sandbox backend", "[warn] no usable container runtime"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("doctor output missing %q:\n%s", want, b.String())
		}
	}
}

func TestDoctorExplicitDockerStillNeedsDocker(t *testing.T) {
	t.Setenv("PATH", fakeHisnDir(t, "0.4.2", 0))
	for _, k := range []string{"WAKIL_SANDBOX_BACKEND", "WAKIL_EXEC_MODE", "ILM_EXEC_MODE"} {
		t.Setenv(k, "")
	}
	cfgFile := filepath.Join(t.TempDir(), "c.json")
	_ = os.WriteFile(cfgFile, []byte(`{"base_url":"http://x:1","sandbox":{"backend":"docker"}}`), 0o600)
	var b bytes.Buffer
	run := func(context.Context, string, ...string) (string, error) { return "", errors.New("unexpected") }
	if rc := runDoctor(&b, []string{"--config", cfgFile}, run); rc == 0 || !strings.Contains(b.String(), "[FAIL] no usable container runtime") {
		t.Fatalf("an explicit docker backend still needs docker, rc=%d:\n%s", rc, b.String())
	}
}

func TestDoctorHisnTooOldFallsBackToDockerRequirement(t *testing.T) {
	t.Setenv("PATH", fakeHisnDir(t, "0.2.2", 0))
	t.Setenv("WAKIL_CONFIG", filepath.Join(t.TempDir(), "none.json"))
	var b bytes.Buffer
	run := func(context.Context, string, ...string) (string, error) { return "", errors.New("unexpected") }
	if rc := runDoctor(&b, nil, run); rc == 0 {
		t.Fatalf("hisn 0.2.2 is not the default, so docker is required:\n%s", b.String())
	}
	if !strings.Contains(b.String(), "not the default sandbox backend") || !strings.Contains(b.String(), "older than 0.4.2") {
		t.Errorf("the reason must be printed:\n%s", b.String())
	}
}
