package config

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// quickstartPlaceholder is the token install.sh replaces with the user's key.
const quickstartPlaceholder = "REPLACE_WITH_OPENROUTER_API_KEY"

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestQuickstartConfigLoads parses config.quickstart.json with the real
// loader, the way it looks after install.sh injected a key.
func TestQuickstartConfigLoads(t *testing.T) {
	raw := readRepoFile(t, "config.quickstart.json")
	if strings.Count(raw, quickstartPlaceholder) != 1 {
		t.Fatalf("config.quickstart.json must contain the key placeholder %q exactly once", quickstartPlaceholder)
	}
	const key = "sk-or-v1-test-key"
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.json")
	if err := os.WriteFile(cfgPath, []byte(strings.Replace(raw, quickstartPlaceholder, key, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())

	cfg, err := LoadConfig([]string{"--config", cfgPath, "--sandbox", "direct"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.EndpointName != "openrouter" {
		t.Errorf("EndpointName = %q, want openrouter", cfg.EndpointName)
	}
	ep := cfg.Endpoint
	if ep.Kind != EndpointKindOpenAI {
		t.Errorf("kind = %q, want %q", ep.Kind, EndpointKindOpenAI)
	}
	if ep.BaseURL != "https://openrouter.ai/api" {
		t.Errorf("base_url = %q", ep.BaseURL)
	}
	if !strings.HasSuffix(ep.Model, ":free") {
		t.Errorf("model = %q, the quickstart model must be a :free one", ep.Model)
	}
	if ep.AuthHeader != "Bearer "+key {
		t.Errorf("auth_header does not carry the injected key as a Bearer token")
	}
	if ep.AppTitle == nil || *ep.AppTitle != "wakil" {
		t.Errorf("app_title = %v, want wakil", ep.AppTitle)
	}
}

// TestQuickstartConfigIsMinimal keeps the quickstart file to the one endpoint:
// no mashura or sandbox settings, so built-in defaults apply.
func TestQuickstartConfigIsMinimal(t *testing.T) {
	raw := readRepoFile(t, "config.quickstart.json")
	for _, banned := range []string{"mashura", "oracle", "\"sandbox\""} {
		if strings.Contains(strings.ToLower(raw), banned) {
			t.Errorf("config.quickstart.json must not set %s", banned)
		}
	}
	if !strings.Contains(raw, "weak at tool calling") {
		t.Error("_comment must say plainly that a free model is weak at tool calling")
	}
}

// TestInstallScriptConfigPlaceholderMatches guards the contract between
// install.sh (which substitutes the key) and the quickstart template.
func TestInstallScriptConfigPlaceholderMatches(t *testing.T) {
	script := readRepoFile(t, "install.sh")
	want := `CONFIG_PLACEHOLDER="` + quickstartPlaceholder + `"`
	if !strings.Contains(script, want) {
		t.Fatalf("install.sh must define %s", want)
	}
}

// TestInstallScriptKvrVersionMatchesDockerfile keeps the installer's
// KVR_VERSION on the kvrust tag the sandbox image is built from.
func TestInstallScriptKvrVersionMatchesDockerfile(t *testing.T) {
	script := readRepoFile(t, "install.sh")
	m := regexp.MustCompile(`(?m)^KVR_VERSION="([0-9]+\.[0-9]+\.[0-9]+)"$`).FindStringSubmatch(script)
	if m == nil {
		t.Fatal("install.sh: KVR_VERSION not found")
	}
	docker := readRepoFile(t, "Dockerfile")
	if !strings.Contains(docker, "v"+m[1]) {
		t.Errorf("Dockerfile does not mention kvrust v%s (install.sh KVR_VERSION)", m[1])
	}
}
