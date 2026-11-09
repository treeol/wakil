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
		"endpoint.auth_header  [redacted]  [file]",
		"api_key               …1234  [file]",
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

func TestRedactTail(t *testing.T) {
	if redactTail("abcdefghijkl") != "…ijkl" || redactTail("short") != "[redacted]" || redactTail("") != "" {
		t.Error("redactTail")
	}
}
