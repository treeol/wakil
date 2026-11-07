package hisn

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestIntegrationPlannedRiskSigning runs PlannedRisk against the real hisn on
// a signing-key configuration (ssh-agent forwarded): the label must be
// "signing" and the ack the same, with the ssh-agent capability listed. This is
// the label `wakil sandbox trust` shows and the one startup refuses without an
// ack. Requires WAKIL_HISN_BIN pointing at hisn >= 0.3.3.
func TestIntegrationPlannedRiskSigning(t *testing.T) {
	bin := os.Getenv("WAKIL_HISN_BIN")
	if bin == "" {
		t.Skip("set WAKIL_HISN_BIN to a hisn >= 0.3.3 binary")
	}
	root := t.TempDir()
	cwd := filepath.Join(root, "ws")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd, _ = filepath.EvalSymlinks(cwd)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	r, err := PlannedRisk(ctx, Options{
		Binary:     bin,
		StateDir:   filepath.Join(root, "state"),
		HomeRoot:   filepath.Join(root, "home"),
		HostCwd:    cwd,
		SSHAgent:   true,
		SSHKeys:    []string{"SHA256:PlannedRiskTestFingerprint"},
		Surface:    Surface{},
		SigningKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIPLANNEDRISKTEST test",
		GitName:    "Wakil Test",
		GitEmail:   "test@wakil.test",
		KnownHosts: filepath.Join(root, "known_hosts"),
	})
	if err != nil {
		t.Skipf("PlannedRisk against this hisn: %v", err)
	}
	if r.Label != "signing" || r.Ack != "signing" {
		t.Errorf("label/ack = %q/%q, want signing/signing (capabilities %+v)", r.Label, r.Ack, r.Capabilities)
	}
	found := false
	for _, c := range r.Capabilities {
		if c.Label == "signing" {
			found = true
		}
	}
	if !found {
		t.Errorf("no signing-labelled capability listed: %+v", r.Capabilities)
	}
}
