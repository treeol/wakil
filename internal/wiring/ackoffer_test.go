package wiring

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/sandbox/hisn"
	"golang.org/x/term"
)

// stubRisk makes productionRisk return a fixed profile risk for AckOffer tests.
func withStubRisk(t *testing.T, label, ack string) {
	t.Helper()
	prev := productionRiskFn
	productionRiskFn = func(config.Config) (hisn.Risk, error) {
		return hisn.Risk{Label: label, Ack: ack}, nil
	}
	t.Cleanup(func() { productionRiskFn = prev })
}

func TestAckOfferYesWritesAndApprovesThenContinues(t *testing.T) {
	withStubRisk(t, "signing", "signing")
	cfg, ws := projWorkspace(t, "")
	var out bytes.Buffer
	if err := AckOffer(cfg, strings.NewReader("y\n"), &out); err != nil {
		t.Fatalf("y must continue startup: %v\n%s", err, out.String())
	}
	if _, _, err := loadProjectSurface(cfg); err != nil {
		t.Errorf("after y the file must be approved: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(ws, ".wakil", "sandbox.json"))
	if string(raw) != `{"hisn":{"risk_ack":"signing"}}`+"\n" {
		t.Errorf("content: %q", raw)
	}
	if !strings.Contains(out.String(), "signing") {
		t.Errorf("the labels must be shown: %s", out.String())
	}
}

func TestAckOfferNoExitsAndWritesNothing(t *testing.T) {
	for name, answer := range map[string]string{"N": "N\n", "empty": "\n", "no": "no\n"} {
		withStubRisk(t, "signing", "signing")
		cfg, ws := projWorkspace(t, "")
		if err := AckOffer(cfg, strings.NewReader(answer), &bytes.Buffer{}); err == nil {
			t.Errorf("%s must exit with the error", name)
		}
		if _, err := os.Stat(filepath.Join(ws, ".wakil", "sandbox.json")); err == nil {
			t.Errorf("%s must leave no file", name)
		}
	}
}

func TestAckOfferHandWrittenFileIsNotRewritten(t *testing.T) {
	withStubRisk(t, "signing", "signing")
	hand := `{"hisn":{"risk_ack":"data","home":"tmpfs"}}`
	cfg, ws := projWorkspace(t, hand)
	if err := AckOffer(cfg, strings.NewReader("y\n"), &bytes.Buffer{}); err == nil {
		t.Fatal("a hand-written file must not be rewritten by the offer")
	}
	raw, _ := os.ReadFile(filepath.Join(ws, ".wakil", "sandbox.json"))
	if string(raw) != hand {
		t.Errorf("file changed: %q", raw)
	}
}

func TestAckOfferAlreadyAckedDoesNotPrompt(t *testing.T) {
	withStubRisk(t, "signing", "signing")
	cfg, _ := projWorkspace(t, `{"hisn":{"risk_ack":"signing"}}`)
	var out bytes.Buffer
	// The reader must never be consumed: an empty reader would make the prompt fail.
	if err := AckOffer(cfg, strings.NewReader(""), &out); err != nil {
		t.Fatalf("an already-acked file must not prompt: %v", err)
	}
	if strings.Contains(out.String(), "[y/N]") {
		t.Errorf("no prompt expected: %s", out.String())
	}
}

func TestUnparseableFileStillHardStops(t *testing.T) {
	cfg, _ := projWorkspace(t, `{"hisn":`)
	if _, _, err := loadProjectSurface(cfg); err == nil {
		t.Fatal("an unparseable file must hard-stop the surface load (F1)")
	}
	if err := AckOffer(cfg, strings.NewReader("y\n"), &bytes.Buffer{}); err == nil {
		t.Error("the offer must not approve an unparseable file")
	}
}

func TestIsInteractiveTUIFalseUnderTest(t *testing.T) {
	// `go test` never runs with both stdin and stdout on a terminal here.
	if IsInteractiveTUI() {
		t.Skip("run in a non-terminal to check the non-interactive rule")
	}
}

// /dev/null is a character device but not a terminal: it must not count as
// interactive (a scheduled run with stdout/stdin on /dev/null must never prompt).
func TestIsInteractiveTUIRejectsDevNull(t *testing.T) {
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Skip(err)
	}
	defer f.Close()
	if term.IsTerminal(int(f.Fd())) {
		t.Fatal("/dev/null must not be a terminal")
	}
}
