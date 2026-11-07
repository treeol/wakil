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
