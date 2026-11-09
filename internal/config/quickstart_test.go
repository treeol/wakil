package config

import (
	"os"
	"os/exec"
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

// TestInstallScriptNeverHandlesTheKey: since 0.6.3 install.sh writes no config
// and never reads, stores or passes on the API key; it only tests whether
// OPENROUTER_API_KEY is set and hands the variable NAME to `wakil init`.
func TestInstallScriptNeverHandlesTheKey(t *testing.T) {
	script := readRepoFile(t, "install.sh")
	for _, banned := range []string{"KEY_FROM_ENV", "read_key_from_tty", "write_config", "CONFIG_PLACEHOLDER", "stty", "config.quickstart.json"} {
		if strings.Contains(script, banned) {
			t.Errorf("install.sh still contains %q: key handling and config writing moved into `wakil init`", banned)
		}
	}
	if !strings.Contains(script, `init --provider openrouter --key-env OPENROUTER_API_KEY --yes`) {
		t.Error("install.sh must run `wakil init --provider openrouter --key-env OPENROUTER_API_KEY --yes` when the variable is set")
	}
	if strings.Contains(script, `$OPENROUTER_API_KEY`) || strings.Contains(script, `${OPENROUTER_API_KEY}`) {
		t.Error("install.sh must not expand OPENROUTER_API_KEY (only test it with ${OPENROUTER_API_KEY:-})")
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

// TestQuickstartModelIsEmbedded: init and the docs read the default model from
// the embedded config.quickstart.json, the same bytes as the file on disk.
func TestQuickstartModelIsEmbedded(t *testing.T) {
	m, err := QuickstartModel()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(m, ":free") {
		t.Errorf("QuickstartModel = %q, want a :free model", m)
	}
	if !strings.Contains(readRepoFile(t, "config.quickstart.json"), `"model": "`+m+`"`) {
		t.Errorf("embedded model %q is not the one in config.quickstart.json", m)
	}
}

// TestInstallScriptTracedRunNeverPrintsTheKey runs install.sh's setup_config
// under `sh -x` with a sentinel key and a fake wakil: the sentinel must appear
// nowhere in the trace or the output, and the fake wakil must receive only the
// variable NAME.
func TestInstallScriptTracedRunNeverPrintsTheKey(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	_ = os.MkdirAll(bin, 0o755)
	logf := filepath.Join(dir, "args.log")
	fake := "#!/bin/sh\necho \"ARGS: $*\" >> \"$FAKE_LOG\"\n"
	if err := os.WriteFile(filepath.Join(bin, "wakil"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	script := readRepoFile(t, "install.sh")
	i := strings.Index(script, "tty_usable() {")
	j := strings.Index(script, "# ── uninstall")
	if i < 0 || j < i {
		t.Fatal("cannot extract the config functions from install.sh")
	}
	harness := script[i:j] + "\nCONFIG_RESULT=\"\"\nsetup_config\necho \"RESULT: $CONFIG_RESULT\"\n"
	hp := filepath.Join(dir, "h.sh")
	_ = os.WriteFile(hp, []byte(harness), 0o644)
	const sentinel = "SENTINEL-KEY-do-not-print-0123456789"
	cmd := exec.Command("sh", "-x", hp)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + dir, "DIR=" + bin, "FAKE_LOG=" + logf, "OPENROUTER_API_KEY=" + sentinel}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("harness: %v\n%s", err, out)
	}
	if strings.Contains(string(out), sentinel) {
		t.Errorf("the key appeared in the traced output:\n%s", out)
	}
	args, _ := os.ReadFile(logf)
	if strings.Contains(string(args), sentinel) || !strings.Contains(string(args), "--key-env OPENROUTER_API_KEY") {
		t.Errorf("wakil init must receive the NAME only: %q", args)
	}
}
