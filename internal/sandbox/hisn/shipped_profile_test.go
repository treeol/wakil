package hisn

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// profiles/deploy-smaragd.toml ships in the repo. Wakil never acknowledges a
// risk for the user, so the shipped file must carry NO active risk_ack, only
// the commented lines to uncomment — and uncommenting exactly those must give
// a profile whose acks match the labels hisn computes (docker socket =
// root-equivalent).
func TestShippedDeployProfile(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "profiles", "deploy-smaragd.toml"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	var doc map[string]any
	if _, err := toml.Decode(text, &doc); err != nil {
		t.Fatalf("shipped profile must be valid TOML: %v", err)
	}
	if doc["version"] != "hisn-profile/v1" {
		t.Errorf("version = %v", doc["version"])
	}
	// As shipped: no active ack anywhere (hisn refuses it until the user acts).
	if regexp.MustCompile(`(?m)^\s*risk_ack\s*=`).MatchString(text) {
		t.Fatal("the shipped profile must not contain an active risk_ack")
	}
	if _, err := fakeProfileRisks(text); err == nil || !strings.Contains(err.Error(), "E_RISK_ACK_REQUIRED") {
		t.Errorf("as shipped it must be refused for the missing ack, got %v", err)
	}
	// The documented lines, exactly, appear twice (forward + [docker]).
	const line = `# risk_ack = "root-equivalent"`
	if n := strings.Count(text, line); n != 2 {
		t.Fatalf("want 2 commented ack lines, found %d", n)
	}
	acked := strings.ReplaceAll(text, line, `risk_ack = "root-equivalent"`)
	risks, err := fakeProfileRisks(acked)
	if err != nil {
		t.Fatalf("with both lines uncommented it must load: %v", err)
	}
	if highest(risks) != "root-equivalent" {
		t.Errorf("risk = %q", highest(risks))
	}
	// The forward is the docker daemon socket and docker T2 is declared.
	fw := tables(doc, "forward")
	if len(fw) != 1 || str(fw[0], "socket") != "/var/run/docker.sock" {
		t.Errorf("forward: %v", fw)
	}
	if d, _ := doc["docker"].(map[string]any); str(d, "tier") != "socket" {
		t.Errorf("docker: %v", doc["docker"])
	}
}
