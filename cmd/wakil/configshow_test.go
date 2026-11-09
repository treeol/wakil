package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestConfigShowRedactsAndShowsSources(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ILM_BASE_URL", "")
	t.Setenv("WAKIL_BASE_URL", "")
	t.Setenv("ILM_MODEL", "")
	t.Setenv("WAKIL_MODEL", "")
	p := writeTestCfg(t, `{"api_key":"sk-legacy-SECRET1234","approval":"auto",
 "endpoints":{"e":{"kind":"openai","base_url":"http://x:1","model":"m1","auth_header":"Bearer sk-header-SECRET9999","api_key_env":"MY_K"}},
 "default_endpoint":"e"}`)
	var out, errb bytes.Buffer
	code := runConfigShow(&out, &errb, []string{"--config", p, "--sandbox", "direct", "--model", "m2"}, envOf(map[string]string{"MY_K": "x"}))
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	s := out.String()
	for _, secret := range []string{"SECRET1234", "SECRET9999", "sk-legacy", "sk-header"} {
		if strings.Contains(s, secret) {
			t.Errorf("secret %q leaked:\n%s", secret, s)
		}
	}
	for _, want := range []string{
		"endpoint.auth_header  set (27 chars)  [file]",
		"api_key               set (20 chars)  [file]",
		"endpoint.model        m2  [flag]",
		"endpoint.base_url     http://x:1  [file]",
		"approval              auto  [file]",
		"sandbox.backend       direct  [flag]",
		"endpoint.api_key_env  MY_K (set)  [file]",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

func TestConfigShowJSONAndEnvSource(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ILM_MODEL", "")
	t.Setenv("WAKIL_MODEL", "env-model")
	t.Setenv("ILM_BASE_URL", "")
	t.Setenv("WAKIL_BASE_URL", "")
	p := writeTestCfg(t, `{"endpoints":{"e":{"kind":"openai","base_url":"http://x:1","model":"m1"}},"default_endpoint":"e"}`)
	var out, errb bytes.Buffer
	if code := runConfigShow(&out, &errb, []string{"--json", "--config", p, "--sandbox", "direct"}, os.Getenv); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	var rows []showRow
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, out.String())
	}
	got := map[string]showRow{}
	for _, r := range rows {
		got[r.Key] = r
	}
	if r := got["endpoint.model"]; r.Value != "env-model" || r.Source != "env" {
		t.Errorf("model row = %+v", r)
	}
	if r := got["approval"]; r.Value != "ask" || r.Source != "default" {
		t.Errorf("approval row = %+v", r)
	}
}

func TestConfigShowWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("WAKIL_CONFIG", "")
	t.Setenv("ILM_BASE_URL", "")
	t.Setenv("WAKIL_BASE_URL", "")
	var out, errb bytes.Buffer
	if code := runConfigShow(&out, &errb, nil, envOf(nil)); code == 0 {
		t.Fatal("no config: must not succeed")
	}
	if !strings.Contains(errb.String(), "wakil init") {
		t.Errorf("want the init hint: %s", errb.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".config")); err == nil {
		t.Error("config show must not create the config dir")
	}
}

func TestRedactSecretShowsOnlyPresenceAndLength(t *testing.T) {
	if redactSecret("") != "unset" || redactSecret("abcdefghijkl") != "set (12 chars)" || redactSecret("äöü") != "set (3 chars)" {
		t.Error("redactSecret")
	}
}

// TestConfigShowNeverPrintsAnySecretCharacter: for a secret that comes from the
// file, the environment and a flag, in text and JSON output, no fragment of it
// (of 3 or more characters, from any position) may appear.
func TestConfigShowNeverPrintsAnySecretCharacter(t *testing.T) {
	const apiKey = "sk-Zq7Xvb-QRSTUVWX1234"
	const header = "Bearer sk-Mw3Kd8-JKLMNOPQ9999"
	check := func(t *testing.T, name, out string, secrets ...string) {
		t.Helper()
		// The config_path row holds the random temp-dir name, whose digits can
		// coincide with a fragment; it is not a secret-bearing row.
		var kept []string
		for _, l := range strings.Split(out, "\n") {
			if strings.Contains(l, "config_path") || strings.Contains(l, `"config_path"`) {
				continue
			}
			kept = append(kept, l)
		}
		out = strings.Join(kept, "\n")
		for _, sec := range secrets {
			for i := 0; i+4 <= len(sec); i++ {
				for j := i + 4; j <= len(sec); j++ {
					if frag := sec[i:j]; strings.Contains(out, frag) {
						t.Fatalf("%s: fragment %q of a secret appears in:\n%s", name, frag, out)
					}
				}
			}
		}
	}
	base := func(extra string) string {
		return `{` + extra + `"endpoints":{"e":{"kind":"openai","base_url":"http://x:1","model":"m","auth_header":"` + header + `"}},"default_endpoint":"e"}`
	}
	cases := []struct {
		name string
		body string
		env  map[string]string
		args []string
	}{
		{"file", base(`"api_key":"` + apiKey + `",`), nil, nil},
		{"env", base(``), map[string]string{"WAKIL_API_KEY": apiKey}, nil},
		{"flag", base(``), nil, []string{"--api-key", apiKey}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("ILM_API_KEY", "")
			t.Setenv("WAKIL_API_KEY", "")
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			p := writeTestCfg(t, c.body)
			for _, asJSON := range []bool{false, true} {
				args := append([]string{"--config", p, "--sandbox", "direct"}, c.args...)
				if asJSON {
					args = append([]string{"--json"}, args...)
				}
				var out, errb bytes.Buffer
				if code := runConfigShow(&out, &errb, args, os.Getenv); code != 0 {
					t.Fatalf("exit %d: %s", code, errb.String())
				}
				check(t, c.name, out.String()+errb.String(), apiKey, header)
				if !strings.Contains(out.String(), "set (22 chars)") {
					t.Errorf("%s: api_key row should read set (22 chars):\n%s", c.name, out.String())
				}
			}
		})
	}
}
