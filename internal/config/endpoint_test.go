package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeCfg writes a config JSON to a temp file and returns its path.
func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// clearIlmEnv guards the test from ambient ILM_* variables on the host.
func clearIlmEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"ILM_BASE_URL", "ILM_HOST", "ILM_PORT", "ILM_API_KEY", "ILM_MODEL", "WAKIL_CONFIG"} {
		t.Setenv(k, "")
	}
}

// TestEndpointsBlockParses verifies a full endpoints block resolves the
// default endpoint with kind, model, sampling fields, and mirrors base_url/
// model into the legacy fields.
func TestEndpointsBlockParses(t *testing.T) {
	clearIlmEnv(t)
	p := writeCfg(t, `{
		"endpoints": {
			"llama": {
				"kind": "openai",
				"base_url": "http://llama-host:11400",
				"model": "qwen3.6-35b",
				"temperature": 0.7,
				"max_tokens": 4096
			},
			"ilm": {
				"kind": "ilm-proxy",
				"base_url": "http://llama-host:11400"
			}
		},
		"default_endpoint": "llama"
	}`)

	cfg, err := LoadConfig([]string{"--config", p, "--exec", "direct"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.EndpointName != "llama" {
		t.Errorf("EndpointName = %q, want llama", cfg.EndpointName)
	}
	ep := cfg.ActiveEndpoint()
	if ep.Kind != EndpointKindOpenAI {
		t.Errorf("Kind = %q, want openai", ep.Kind)
	}
	if ep.Model != "qwen3.6-35b" {
		t.Errorf("Model = %q, want qwen3.6-35b", ep.Model)
	}
	if ep.Temperature == nil || *ep.Temperature != 0.7 {
		t.Errorf("Temperature = %v, want 0.7", ep.Temperature)
	}
	if ep.TopP != nil {
		t.Errorf("TopP should be nil (unset), got %v", *ep.TopP)
	}
	if ep.MaxTokens == nil || *ep.MaxTokens != 4096 {
		t.Errorf("MaxTokens = %v, want 4096", ep.MaxTokens)
	}
	// Legacy mirror: the rest of the code reads cfg.BaseURL / cfg.Model.
	if cfg.BaseURL != "http://llama-host:11400" {
		t.Errorf("cfg.BaseURL = %q", cfg.BaseURL)
	}
	if cfg.Model != "qwen3.6-35b" {
		t.Errorf("cfg.Model = %q, want qwen3.6-35b", cfg.Model)
	}
}

// TestKindDefaultsToOpenAI: kind omitted → "openai", so model becomes required.
func TestKindDefaultsToOpenAI(t *testing.T) {
	clearIlmEnv(t)
	p := writeCfg(t, `{
		"endpoints": {"e": {"base_url": "http://h:1", "model": "m"}},
		"default_endpoint": "e"
	}`)
	cfg, err := LoadConfig([]string{"--config", p, "--exec", "direct"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.ActiveEndpoint().Kind; got != EndpointKindOpenAI {
		t.Errorf("omitted kind = %q, want openai", got)
	}
}

// TestOpenAIKindRequiresModel: kind=openai without model must fail validation
// with a message naming the endpoint and the requirement.
func TestOpenAIKindRequiresModel(t *testing.T) {
	clearIlmEnv(t)
	p := writeCfg(t, `{
		"endpoints": {"llama": {"kind": "openai", "base_url": "http://h:1"}},
		"default_endpoint": "llama"
	}`)
	_, err := LoadConfig([]string{"--config", p, "--exec", "direct"})
	if err == nil {
		t.Fatal("want validation error for openai endpoint without model, got nil")
	}
	if !strings.Contains(err.Error(), "llama") || !strings.Contains(err.Error(), "model is required") {
		t.Errorf("error should name the endpoint and the missing model, got: %v", err)
	}
}

// TestIlmProxyKindDefaultsModelIlm: kind=ilm-proxy without model → "ilm".
func TestIlmProxyKindDefaultsModelIlm(t *testing.T) {
	clearIlmEnv(t)
	p := writeCfg(t, `{
		"endpoints": {"ilm": {"kind": "ilm-proxy", "base_url": "http://h:1"}},
		"default_endpoint": "ilm"
	}`)
	cfg, err := LoadConfig([]string{"--config", p, "--exec", "direct"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.ActiveEndpoint().Model; got != "ilm" {
		t.Errorf("ilm-proxy default model = %q, want ilm", got)
	}
}

// TestUnknownKindRejected guards against typos silently becoming openai.
func TestUnknownKindRejected(t *testing.T) {
	clearIlmEnv(t)
	p := writeCfg(t, `{
		"endpoints": {"e": {"kind": "opnai", "base_url": "http://h:1", "model": "m"}},
		"default_endpoint": "e"
	}`)
	_, err := LoadConfig([]string{"--config", p, "--exec", "direct"})
	if err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Errorf("want unknown-kind error, got: %v", err)
	}
}

// TestLegacyConfigSynthesizesIlmProxyEndpoint: top-level base_url, no
// endpoints block → one synthesized ilm-proxy endpoint with model "ilm"
// (the DefaultConfig model), exactly today's behavior.
func TestLegacyConfigSynthesizesIlmProxyEndpoint(t *testing.T) {
	clearIlmEnv(t)
	p := writeCfg(t, `{"base_url": "http://proxy-host:11400"}`)
	cfg, err := LoadConfig([]string{"--config", p, "--exec", "direct"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	ep := cfg.ActiveEndpoint()
	if ep.Kind != EndpointKindIlmProxy {
		t.Errorf("legacy Kind = %q, want ilm-proxy", ep.Kind)
	}
	if ep.Model != "ilm" {
		t.Errorf("legacy Model = %q, want ilm", ep.Model)
	}
	if ep.BaseURL != "http://proxy-host:11400" {
		t.Errorf("legacy BaseURL = %q", ep.BaseURL)
	}
	if cfg.Model != "ilm" || cfg.BaseURL != "http://proxy-host:11400" {
		t.Errorf("legacy top-level fields changed: model=%q base_url=%q", cfg.Model, cfg.BaseURL)
	}
}

// TestLegacyConfigStillRequiresBaseURL: the original missing-URL error is
// preserved verbatim for legacy configs.
func TestLegacyConfigStillRequiresBaseURL(t *testing.T) {
	clearIlmEnv(t)
	p := writeCfg(t, `{}`)
	_, err := LoadConfig([]string{"--config", p, "--exec", "direct"})
	if err == nil || !strings.Contains(err.Error(), "proxy address required") {
		t.Errorf("want proxy-address-required error, got: %v", err)
	}
}

// TestIlmModelEnvOverridesEndpointModel: ILM_MODEL wins over the endpoint's
// configured model (documented precedence: file < env < flags).
func TestIlmModelEnvOverridesEndpointModel(t *testing.T) {
	clearIlmEnv(t)
	t.Setenv("ILM_MODEL", "env-model")
	p := writeCfg(t, `{
		"endpoints": {"e": {"kind": "openai", "base_url": "http://h:1", "model": "cfg-model"}},
		"default_endpoint": "e"
	}`)
	cfg, err := LoadConfig([]string{"--config", p, "--exec", "direct"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.ActiveEndpoint().Model; got != "env-model" {
		t.Errorf("ILM_MODEL should override endpoint model: got %q, want env-model", got)
	}
}

// TestModelFlagOverridesEndpointModel: --model beats both file and env.
func TestModelFlagOverridesEndpointModel(t *testing.T) {
	clearIlmEnv(t)
	p := writeCfg(t, `{
		"endpoints": {"e": {"kind": "openai", "base_url": "http://h:1", "model": "cfg-model"}},
		"default_endpoint": "e"
	}`)
	cfg, err := LoadConfig([]string{"--config", p, "--exec", "direct", "--model", "flag-model"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.ActiveEndpoint().Model; got != "flag-model" {
		t.Errorf("--model should override endpoint model: got %q, want flag-model", got)
	}
}

// TestBaseURLFlagStillFunctions: the legacy --base-url flag overrides the
// endpoint's base_url (and remains the sole source for legacy configs).
func TestBaseURLFlagStillFunctions(t *testing.T) {
	clearIlmEnv(t)
	// With endpoints block:
	p := writeCfg(t, `{
		"endpoints": {"e": {"kind": "openai", "base_url": "http://cfg-host:1", "model": "m"}},
		"default_endpoint": "e"
	}`)
	cfg, err := LoadConfig([]string{"--config", p, "--exec", "direct", "--base-url", "http://flag-host:2"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.ActiveEndpoint().BaseURL; got != "http://flag-host:2" {
		t.Errorf("--base-url should override endpoint base_url: got %q", got)
	}

	// Legacy path (no endpoints block): --base-url is the only URL source.
	p2 := writeCfg(t, `{}`)
	cfg2, err := LoadConfig([]string{"--config", p2, "--exec", "direct", "--base-url", "http://flag-host:3"})
	if err != nil {
		t.Fatalf("LoadConfig legacy: %v", err)
	}
	if cfg2.ActiveEndpoint().BaseURL != "http://flag-host:3" {
		t.Errorf("legacy --base-url = %q", cfg2.ActiveEndpoint().BaseURL)
	}
	if cfg2.ActiveEndpoint().Kind != EndpointKindIlmProxy {
		t.Errorf("legacy kind = %q, want ilm-proxy", cfg2.ActiveEndpoint().Kind)
	}
}

// TestDefaultEndpointMissingEntry: default_endpoint naming an absent entry fails.
func TestDefaultEndpointMissingEntry(t *testing.T) {
	clearIlmEnv(t)
	p := writeCfg(t, `{
		"endpoints": {"a": {"kind": "openai", "base_url": "http://h:1", "model": "m"}},
		"default_endpoint": "nope"
	}`)
	_, err := LoadConfig([]string{"--config", p, "--exec", "direct"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("want not-found error, got: %v", err)
	}
}

// TestSingleEndpointNoDefaultSelected: one entry, no default_endpoint → it is used.
func TestSingleEndpointNoDefaultSelected(t *testing.T) {
	clearIlmEnv(t)
	p := writeCfg(t, `{
		"endpoints": {"only": {"kind": "openai", "base_url": "http://h:1", "model": "m"}}
	}`)
	cfg, err := LoadConfig([]string{"--config", p, "--exec", "direct"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.EndpointName != "only" {
		t.Errorf("EndpointName = %q, want only", cfg.EndpointName)
	}
}

// TestMultipleEndpointsRequireDefault: >1 entries without default_endpoint fails.
func TestMultipleEndpointsRequireDefault(t *testing.T) {
	clearIlmEnv(t)
	p := writeCfg(t, `{
		"endpoints": {
			"a": {"kind": "openai", "base_url": "http://h:1", "model": "m"},
			"b": {"kind": "ilm-proxy", "base_url": "http://h:2"}
		}
	}`)
	_, err := LoadConfig([]string{"--config", p, "--exec", "direct"})
	if err == nil || !strings.Contains(err.Error(), "default_endpoint") {
		t.Errorf("want set-default_endpoint error, got: %v", err)
	}
}

// TestEndpointAuthHeaderWinsOverAPIKey: endpoint-level auth_header is sent
// verbatim, beating the legacy api_key Bearer synthesis.
func TestEndpointAuthHeaderWinsOverAPIKey(t *testing.T) {
	clearIlmEnv(t)
	p := writeCfg(t, `{
		"api_key": "legacy-key",
		"endpoints": {"e": {"kind": "openai", "base_url": "http://h:1", "model": "m", "auth_header": "Bearer ep-token"}},
		"default_endpoint": "e"
	}`)
	cfg, err := LoadConfig([]string{"--config", p, "--exec", "direct"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg.AuthHeader(); got != "Bearer ep-token" {
		t.Errorf("AuthHeader = %q, want endpoint auth_header verbatim", got)
	}

	// Without endpoint auth_header, legacy api_key still applies.
	p2 := writeCfg(t, `{
		"api_key": "legacy-key",
		"endpoints": {"e": {"kind": "openai", "base_url": "http://h:1", "model": "m"}},
		"default_endpoint": "e"
	}`)
	cfg2, err := LoadConfig([]string{"--config", p2, "--exec", "direct"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if got := cfg2.AuthHeader(); got != "Bearer legacy-key" {
		t.Errorf("AuthHeader fallback = %q, want Bearer legacy-key", got)
	}
}

// TestAppAttributionFieldsParse: app_referer and app_title parse from JSON,
// and unset (omitted) vs empty string ("") is distinguishable.
func TestAppAttributionFieldsParse(t *testing.T) {
	clearIlmEnv(t)
	p := writeCfg(t, `{
		"endpoints": {
			"with-attr": {
				"kind": "openai",
				"base_url": "https://openrouter.ai/api",
				"model": "m",
				"app_referer": "https://my.app",
				"app_title": "my-agent"
			},
			"with-empty": {
				"kind": "openai",
				"base_url": "https://openrouter.ai/api",
				"model": "m",
				"app_referer": "",
				"app_title": ""
			},
			"without-attr": {
				"kind": "openai",
				"base_url": "https://openrouter.ai/api",
				"model": "m"
			}
		},
		"default_endpoint": "with-attr"
	}`)

	cfg, err := LoadConfig([]string{"--config", p, "--exec", "direct"})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	// with-attr: both fields explicitly set.
	ep := cfg.Endpoints["with-attr"]
	if ep.AppReferer == nil || *ep.AppReferer != "https://my.app" {
		t.Errorf("with-attr AppReferer = %v, want %q", ep.AppReferer, "https://my.app")
	}
	if ep.AppTitle == nil || *ep.AppTitle != "my-agent" {
		t.Errorf("with-attr AppTitle = %v, want %q", ep.AppTitle, "my-agent")
	}

	// with-empty: both fields explicitly empty string (opt-out).
	ep2 := cfg.Endpoints["with-empty"]
	if ep2.AppReferer == nil || *ep2.AppReferer != "" {
		t.Errorf("with-empty AppReferer = %v, want non-nil empty string", ep2.AppReferer)
	}
	if ep2.AppTitle == nil || *ep2.AppTitle != "" {
		t.Errorf("with-empty AppTitle = %v, want non-nil empty string", ep2.AppTitle)
	}

	// without-attr: both fields unset (nil).
	ep3 := cfg.Endpoints["without-attr"]
	if ep3.AppReferer != nil {
		t.Errorf("without-attr AppReferer = %v, want nil", ep3.AppReferer)
	}
	if ep3.AppTitle != nil {
		t.Errorf("without-attr AppTitle = %v, want nil", ep3.AppTitle)
	}
}
