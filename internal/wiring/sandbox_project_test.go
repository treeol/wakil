package wiring

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/sandbox/project"
)

const projFile = `{"hisn":{"forward":[{"name":"docker","socket":"/var/run/docker.sock","expose":"/run/docker.sock"}],"risk_ack":"root-equivalent"}}`

func projWorkspace(t *testing.T, content string) (config.Config, string) {
	t.Helper()
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	t.Setenv("WAKIL_WORKSPACE_PATH", "")
	ws, _ := filepath.EvalSymlinks(t.TempDir())
	if content != "" {
		p := filepath.Join(ws, ".wakil", "sandbox.json")
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return config.Config{ExecMode: "docker", HostWorkDir: ws, WorkDir: ws}, ws
}

type ttyString struct{ io.Reader }

func (ttyString) Close() error { return nil }

func tty(s string) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) { return ttyString{strings.NewReader(s)}, nil }
}

func TestProjectSurfaceAbsentIsEmpty(t *testing.T) {
	cfg, _ := projWorkspace(t, "")
	s, l, err := loadProjectSurface(cfg)
	if err != nil || l.Present || !s.Empty() {
		t.Fatalf("%+v %+v %v", s, l, err)
	}
}

func TestProjectSurfaceNeedsApproval(t *testing.T) {
	cfg, ws := projWorkspace(t, projFile)
	_, _, err := loadProjectSurface(cfg)
	var na *project.ErrNotApproved
	if !errors.As(err, &na) {
		t.Fatalf("an unapproved file asking for a docker forward must be refused, got %v", err)
	}
	// The user approves exactly this content ...
	var out bytes.Buffer
	if err := SandboxTrust(&out, ws, tty("yes\n")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "docker.sock") || !strings.Contains(out.String(), "root-equivalent") {
		t.Errorf("the user must be shown what they approve:\n%s", out.String())
	}
	s, _, err := loadProjectSurface(cfg)
	if err != nil {
		t.Fatalf("approved file must load: %v", err)
	}
	if s.RiskAck != "root-equivalent" || len(s.Forward) != 1 {
		t.Errorf("surface: %+v", s)
	}
	if len(s.Mask) != 1 || s.Mask[0] != filepath.Join(ws, ".wakil", "sandbox.json") {
		t.Errorf("the file must be masked inside the sandbox: %v", s.Mask)
	}
	// ... and an agent that edits it afterwards is back to unapproved.
	if err := os.WriteFile(filepath.Join(ws, ".wakil", "sandbox.json"), []byte(strings.Replace(projFile, "docker.sock", "other.sock", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadProjectSurface(cfg); !errors.As(err, &na) {
		t.Fatalf("a changed file must need approval again, got %v", err)
	}
}

func TestSandboxTrustRefusals(t *testing.T) {
	_, ws := projWorkspace(t, projFile)
	for name, answer := range map[string]string{"no": "no\n", "empty": "\n", "y": "y\n", "eof": ""} {
		if err := SandboxTrust(io.Discard, ws, tty(answer)); err == nil {
			t.Errorf("answer %q must not approve", name)
		}
	}
	// No controlling terminal: refuse, never approve.
	err := SandboxTrust(io.Discard, ws, func() (io.ReadCloser, error) { return nil, errors.New("no tty") })
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Errorf("no tty must refuse: %v", err)
	}
	cfg := config.Config{ExecMode: "docker", HostWorkDir: ws, WorkDir: ws}
	if _, _, err := loadProjectSurface(cfg); err == nil {
		t.Error("nothing may have been approved by the refusals")
	}
}

func TestSandboxTrustNothingToApprove(t *testing.T) {
	_, ws := projWorkspace(t, `{"hisn":{}}`)
	var out bytes.Buffer
	if err := SandboxTrust(&out, ws, tty("")); err != nil || !strings.Contains(out.String(), "needs no approval") {
		t.Errorf("%v %q", err, out.String())
	}
}

func TestNamedProfileProjectIsNotMasked(t *testing.T) {
	cfg, ws := projWorkspace(t, `{"hisn":{"profile":"deploy-smaragd","risk_ack":"root-equivalent"}}`)
	if err := SandboxTrust(io.Discard, ws, tty("yes\n")); err != nil {
		t.Fatal(err)
	}
	s, _, err := loadProjectSurface(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Mask) != 0 || s.Profile != "deploy-smaragd" {
		t.Errorf("named profile mode adds no mask (Wakil only adds grants): %+v", s)
	}
}
