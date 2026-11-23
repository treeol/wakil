package wiring

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/sandbox/hisn"
	"github.com/treeol/wakil/internal/sandbox/project"
)

// fakeRisk returns a fixed risk, so the bootstrap flow is tested without hisn.
func fakeRisk(label, ack string) riskFunc {
	return func(config.Config, hisn.Surface) (hisn.Risk, error) {
		return hisn.Risk{Label: label, Ack: ack, Capabilities: nil}, nil
	}
}

func trustWith(t *testing.T, ws, answer string, risk riskFunc) (string, error) {
	t.Helper()
	var out bytes.Buffer
	l, err := project.Load(ws)
	if err != nil {
		t.Fatal(err)
	}
	err = trustBootstrap(&out, testCfg(ws), l, tty(answer), risk)
	return out.String(), err
}

func TestBootstrapMissingFileYesWritesAndApproves(t *testing.T) {
	_, ws := projWorkspace(t, "")
	out, err := trustWith(t, ws, "yes\n", fakeRisk("signing", "signing"))
	if err != nil {
		t.Fatalf("yes must approve: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(ws, ".wakil", "sandbox.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"hisn":{"risk_ack":"signing"}}`+"\n" {
		t.Errorf("bootstrap content: %q", raw)
	}
	if _, _, err := loadProjectSurface(testCfg(ws)); err != nil {
		t.Errorf("the written file must now load as approved: %v", err)
	}
}

func TestBootstrapMissingFileNoLeavesNothing(t *testing.T) {
	for name, answer := range map[string]string{"no": "no\n", "empty": "\n", "y": "y\n", "eof": ""} {
		_, ws := projWorkspace(t, "")
		if _, err := trustWith(t, ws, answer, fakeRisk("signing", "signing")); err == nil {
			t.Errorf("%s must not approve", name)
		}
		if _, err := os.Stat(filepath.Join(ws, ".wakil", "sandbox.json")); err == nil {
			t.Errorf("%s: the file written before the answer must be removed", name)
		}
		if _, _, err := loadProjectSurface(testCfg(ws)); err != nil {
			t.Errorf("%s: nothing may be approved: %v", name, err)
		}
	}
}

func TestBootstrapMismatchUpdatesOnlyAckWhenBootstrapForm(t *testing.T) {
	_, ws := projWorkspace(t, `{"hisn":{"risk_ack":"data"}}`)
	out, err := trustWith(t, ws, "yes\n", fakeRisk("signing", "signing"))
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	raw, _ := os.ReadFile(filepath.Join(ws, ".wakil", "sandbox.json"))
	if !strings.Contains(string(raw), `"risk_ack":"signing"`) || strings.Contains(string(raw), `"data"`) {
		t.Errorf("ack not updated: %q", raw)
	}
}

func TestBootstrapHandWrittenFileIsNeverRewritten(t *testing.T) {
	hand := `{"hisn":{"risk_ack":"data","home":"tmpfs"}}`
	_, ws := projWorkspace(t, hand)
	out, err := trustWith(t, ws, "yes\n", fakeRisk("signing", "signing"))
	if err == nil {
		t.Fatalf("a hand-written file must not be rewritten: %s", out)
	}
	raw, _ := os.ReadFile(filepath.Join(ws, ".wakil", "sandbox.json"))
	if string(raw) != hand {
		t.Errorf("hand-written file changed: %q", raw)
	}
	if !strings.Contains(out, `"signing"`) {
		t.Errorf("the required ack must be named so the user can change it: %s", out)
	}
}

func TestBootstrapAlreadyAckedAndApprovedNeedsNothing(t *testing.T) {
	cfg, ws := projWorkspace(t, `{"hisn":{"risk_ack":"signing"}}`)
	// Approve the exact file first (what an earlier trust did).
	if _, err := trustWith(t, ws, "yes\n", fakeRisk("signing", "signing")); err != nil {
		t.Fatal(err)
	}
	out, err := trustWith(t, ws, "", fakeRisk("signing", "signing"))
	if err != nil || !strings.Contains(out, "already acknowledges") {
		t.Errorf("%v %s", err, out)
	}
	_ = cfg
}

// A matching ack that is NOT approved yet must still be offered for approval.
func TestBootstrapMatchingUnapprovedIsOffered(t *testing.T) {
	_, ws := projWorkspace(t, `{"hisn":{"risk_ack":"signing"}}`)
	out, err := trustWith(t, ws, "", fakeRisk("signing", "signing"))
	if err == nil || !strings.Contains(out, "Approve this exact content") {
		t.Errorf("an unapproved matching file must be offered: %v %s", err, out)
	}
}

func TestBootstrapNoAckNeededReportsNothingToDo(t *testing.T) {
	_, ws := projWorkspace(t, "")
	out, err := trustWith(t, ws, "", fakeRisk("none", "none"))
	if err != nil || !strings.Contains(out, "needs no approval") {
		t.Errorf("%v %s", err, out)
	}
	if _, err := os.Stat(filepath.Join(ws, ".wakil", "sandbox.json")); err == nil {
		t.Error("nothing may be written when no ack is needed")
	}
}

func TestBootstrapNoTTYRefusesAndWritesNothing(t *testing.T) {
	_, ws := projWorkspace(t, "")
	l, _ := project.Load(ws)
	var out bytes.Buffer
	err := trustBootstrap(&out, testCfg(ws), l,
		func() (io.ReadCloser, error) { return nil, io.EOF }, fakeRisk("signing", "signing"))
	if err == nil || !strings.Contains(err.Error(), "interactive terminal") {
		t.Errorf("no tty must refuse: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".wakil", "sandbox.json")); err == nil {
		t.Error("no tty must not leave a file")
	}
}

func TestBootstrapFormDetection(t *testing.T) {
	cases := map[string]bool{
		`{"hisn":{"risk_ack":"signing"}}`:               true,
		"{ \"hisn\" : { \"risk_ack\" : \"signing\" } }": true,
		`{"hisn":{}}`: false,
		`{"hisn":{"risk_ack":"x","home":"tmpfs"}}`: false,
		`{"hisn":{"risk_ack":"x"},"extra":1}`:      false,
		`not json`:                                 false,
	}
	for raw, want := range cases {
		if got := bootstrapEligible([]byte(raw)); got != want {
			t.Errorf("bootstrapEligible(%q)=%v want %v", raw, got, want)
		}
	}
}

// testCfg is the config the trust tests pass: only the workspace matters, the
// risk function is stubbed.
func testCfg(ws string) config.Config {
	return config.Config{ExecMode: "docker", HostWorkDir: ws, WorkDir: ws}
}

// An ack-only file that the user has not approved (e.g. cloned, or the
// approval store was wiped) must still be reviewable by trust: the risk is
// computed from the file's own surface, and approving it records the approval.
func TestBootstrapUnapprovedAckOnlyFileCanBeApproved(t *testing.T) {
	_, ws := projWorkspace(t, `{"hisn":{"risk_ack":"signing"}}`)
	out, err := trustWith(t, ws, "yes\n", fakeRisk("signing", "signing"))
	if err != nil {
		t.Fatalf("trust must be able to approve an unapproved ack-only file: %v\n%s", err, out)
	}
	if _, _, err := loadProjectSurface(testCfg(ws)); err != nil {
		t.Errorf("after approval the file must load: %v", err)
	}
}

// startup stays fail-closed for the same unapproved ack-only file.
func TestStartupStillRefusesUnapprovedAckOnlyFile(t *testing.T) {
	_, ws := projWorkspace(t, `{"hisn":{"risk_ack":"signing"}}`)
	if _, _, err := loadProjectSurface(testCfg(ws)); err == nil {
		t.Fatal("an unapproved file must be refused by startup")
	}
}

// TestSandboxTrustPositionalOverridesConfigWorkDir pins the trust bug fix:
// when config.json configures a fixed host_work_dir (different from <path>),
// `wakil sandbox trust <path>` must write .wakil/sandbox.json in <path> AND
// store the approval under <path>'s workspace key (not the config's work_dir),
// and print "workspace: <path>" before asking. A subsequent run in <path>
// (WAKIL_HOST_WORKDIR=<path>) must then load as approved.
func TestSandboxTrustPositionalOverridesConfigWorkDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	t.Setenv("WAKIL_WORKSPACE_PATH", "")
	t.Setenv("WAKIL_HOST_WORKDIR", "")
	t.Setenv("ILM_HOST_WORKDIR", "")
	t.Setenv("WAKIL_WORKDIR", "")
	t.Setenv("ILM_WORKDIR", "")

	configuredDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	targetDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	cfgFile := filepath.Join(t.TempDir(), "config.json")
	cfgJSON := `{"endpoints":{"local":{"kind":"ilm-proxy","base_url":"http://127.0.0.1:1","model":"test"}},"default_endpoint":"local","host_work_dir":` +
		`"` + configuredDir + `","sandbox":{"backend":"docker"}}`
	if err := os.WriteFile(cfgFile, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAKIL_CONFIG", cfgFile)

	var out bytes.Buffer
	err = sandboxTrustWith(&out, targetDir, tty("yes\n"), fakeRisk("signing", "signing"))
	if err != nil {
		t.Fatalf("sandboxTrustWith(%s) failed: %v\nout:\n%s", targetDir, err, out.String())
	}
	if !strings.Contains(out.String(), "workspace: "+targetDir+"\n") {
		t.Errorf("must print resolved workspace before asking; got:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(targetDir, ".wakil", "sandbox.json")); err != nil {
		t.Fatalf("expected .wakil/sandbox.json in targetDir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(configuredDir, ".wakil", "sandbox.json")); err == nil {
		t.Fatalf("must not write .wakil/sandbox.json in configuredDir")
	}

	// Simulate `WAKIL_HOST_WORKDIR=<targetDir> wakil run`: LoadConfig resolves
	// targetDir and loadProjectSurface must see it as approved.
	t.Setenv("WAKIL_HOST_WORKDIR", targetDir)
	runCfg, err := config.LoadConfig(nil)
	if err != nil {
		t.Fatalf("LoadConfig for run: %v", err)
	}
	if _, _, err := loadProjectSurface(runCfg); err != nil {
		t.Fatalf("subsequent run in targetDir must see approval: %v", err)
	}
}

// TestSandboxTrustNoArgUsesConfigPrecedence verifies that calling trust with
// no positional argument resolves WAKIL_HOST_WORKDIR > cwd — the config
// file's host_work_dir no longer steers since 0.8.11 (matching `wakil run`)
// — and prints "workspace: <resolved>".
func TestSandboxTrustNoArgUsesConfigPrecedence(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("WAKIL_WORKSPACE_KEY", "")
	t.Setenv("WAKIL_WORKSPACE_PATH", "")
	t.Setenv("WAKIL_HOST_WORKDIR", "")
	t.Setenv("ILM_HOST_WORKDIR", "")
	t.Setenv("WAKIL_WORKDIR", "")
	t.Setenv("ILM_WORKDIR", "")

	configuredDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	envDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	cfgFile := filepath.Join(t.TempDir(), "config.json")
	cfgJSON := `{"endpoints":{"local":{"kind":"ilm-proxy","base_url":"http://127.0.0.1:1","model":"test"}},"default_endpoint":"local","host_work_dir":` +
		`"` + configuredDir + `","sandbox":{"backend":"docker"}}`
	if err := os.WriteFile(cfgFile, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAKIL_CONFIG", cfgFile)

	// Without env or positional arg, the cwd wins (config host_work_dir is
	// ignored since 0.8.11). The test runs in an explicit cwd (a temp dir)
	// so the expectation is deterministic.
	cwd1, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prevWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd1); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD) })
	var out1 bytes.Buffer
	if err := sandboxTrustWith(&out1, "", tty("yes\n"), fakeRisk("signing", "signing")); err != nil {
		t.Fatalf("no-arg trust failed: %v\nout:\n%s", err, out1.String())
	}
	if !strings.Contains(out1.String(), "workspace: "+cwd1+"\n") {
		t.Errorf("no-arg trust must print the cwd %q (config host_work_dir ignored since 0.8.11); got:\n%s", cwd1, out1.String())
	}
	if strings.Contains(out1.String(), "workspace: "+configuredDir+"\n") {
		t.Errorf("no-arg trust must NOT resolve the config folder %q; got:\n%s", configuredDir, out1.String())
	}
	runCfg1, err := config.LoadConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if runCfg1.ResolvedWorkspace != cwd1 {
		t.Errorf("run ResolvedWorkspace = %q, want cwd %q", runCfg1.ResolvedWorkspace, cwd1)
	}

	// With WAKIL_HOST_WORKDIR set and no positional arg, env wins over config.
	t.Setenv("WAKIL_HOST_WORKDIR", envDir)
	var out2 bytes.Buffer
	if err := sandboxTrustWith(&out2, "", tty("yes\n"), fakeRisk("signing", "signing")); err != nil {
		t.Fatalf("env trust failed: %v\nout:\n%s", err, out2.String())
	}
	if !strings.Contains(out2.String(), "workspace: "+envDir+"\n") {
		t.Errorf("env trust must print envDir; got:\n%s", out2.String())
	}
	runCfg2, err := config.LoadConfig(nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadProjectSurface(runCfg2); err != nil {
		t.Fatalf("run in envDir must see approval: %v", err)
	}

	// Without any config file and no argument, trust resolves cwd as before.
	cwdDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WAKIL_HOST_WORKDIR", "")
	t.Setenv("WAKIL_CONFIG", filepath.Join(t.TempDir(), "nonexistent.json"))
	prevWD2, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwdDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prevWD2) })
	p := filepath.Join(cwdDir, ".wakil", "sandbox.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(projFile), 0o644); err != nil {
		t.Fatal(err)
	}
	var out3 bytes.Buffer
	if err := SandboxTrust(&out3, "", tty("yes\n")); err != nil {
		t.Fatalf("no-arg hand-written trust in cwd failed: %v\nout:\n%s", err, out3.String())
	}
	if !strings.Contains(out3.String(), "workspace: "+cwdDir+"\n") {
		t.Errorf("no-arg cwd trust must print cwdDir; got:\n%s", out3.String())
	}
	if _, _, err := loadProjectSurface(testCfg(cwdDir)); err != nil {
		t.Fatalf("loadProjectSurface in cwdDir must see approval: %v", err)
	}
}
