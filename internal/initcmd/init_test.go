package initcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fakeKey = "sk-test-FAKEKEY-0123456789"

// privacy404Body is the body OpenRouter returned in the observed failure.
const privacy404Body = `{"error":{"message":"0 endpoints out of 1 requested are available matching your guardrail restrictions and data policy. Enable \"Free endpoints that may train on inputs\" in https://openrouter.ai/settings/privacy to use free models.","code":404,"metadata":{"ineligibility_reasons":[{"provider_name":"Chutes","reason":"free-model-training-violation"}]}}}`

const toolCallReply = `{"choices":[{"message":{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"ping","arguments":"{}"}}]}}]}`
const textReply = `{"choices":[{"message":{"role":"assistant","content":"pong"}}]}`

const modelsReply = `{"data":[
 {"id":"acme/plain","context_length":8000,"pricing":{"prompt":"0.000001","completion":"0.000002"},"supported_parameters":["max_tokens"]},
 {"id":"nvidia/nemotron-3.5-super-vl:free","context_length":262144,"pricing":{"prompt":"0","completion":"0"},"supported_parameters":["tools"]},
 {"id":"acme/tooly","context_length":128000,"pricing":{"prompt":"0.000003","completion":"0.000015"},"supported_parameters":["tools","max_tokens"]}
]}`

// fakeServer serves /v1/models and /v1/chat/completions. chat decides the reply.
func fakeServer(t *testing.T, status int, chatBody string) (*httptest.Server, *[]string) {
	t.Helper()
	var auths []string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, "models:"+r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(modelsReply))
	})
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, "chat:"+r.Header.Get("Authorization"))
		b := new(bytes.Buffer)
		_, _ = b.ReadFrom(r.Body)
		if !strings.Contains(b.String(), `"tools"`) {
			t.Error("test call must carry a tool definition")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(chatBody))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &auths
}

func newEnv(t *testing.T, interactive bool, input string) (Env, *bytes.Buffer, string) {
	t.Helper()
	dir := t.TempDir()
	out := new(bytes.Buffer)
	return Env{
		In:          strings.NewReader(input),
		Out:         out,
		Interactive: interactive,
		ConfigPath:  filepath.Join(dir, "cfg", "config.json"),
		Now:         func() time.Time { return time.Date(2026, 11, 9, 12, 30, 0, 0, time.UTC) },
		Getenv:      func(k string) string { return map[string]string{"MY_KEY": fakeKey}[k] },
		ProbeSandbox: func(context.Context) (string, string) {
			return "0.4.2", ""
		},
		DockerAvailable: func() bool { return true },
		InContainer:     func() bool { return false },
		ReadSecret:      func(string) (string, error) { return fakeKey, nil },
	}, out, dir
}

func readCfg(t *testing.T, env Env) (string, map[string]any) {
	t.Helper()
	b, err := os.ReadFile(env.ConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("config is not JSON: %v\n%s", err, b)
	}
	return string(b), m
}

func TestNonInteractiveHappyPathWritesKeyEnvOnly(t *testing.T) {
	srv, auths := fakeServer(t, 200, toolCallReply)
	env, out, _ := newEnv(t, false, "")
	code := Run(context.Background(), Options{Provider: ProviderOpenAI, BaseURL: srv.URL + "/", Model: "acme/tooly", KeyEnv: "MY_KEY", Approval: "auto"}, env)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	txt, m := readCfg(t, env)
	if strings.Contains(txt, fakeKey) || strings.Contains(out.String(), fakeKey) {
		t.Fatal("the key leaked into the file or the output")
	}
	eps := m["endpoints"].(map[string]any)["local"].(map[string]any)
	if eps["api_key_env"] != "MY_KEY" || eps["model"] != "acme/tooly" || eps["base_url"] != srv.URL {
		t.Errorf("endpoint = %v", eps)
	}
	if _, has := eps["auth_header"]; has {
		t.Error("auth_header must not be written with --key-env")
	}
	if m["approval"] != "auto" || m["default_endpoint"] != "local" {
		t.Errorf("config = %v", m)
	}
	if _, has := m["sandbox"]; has {
		t.Error("sandbox.backend must not be written for the automatic choice")
	}
	fi, _ := os.Stat(env.ConfigPath)
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o", fi.Mode().Perm())
	}
	di, _ := os.Stat(filepath.Dir(env.ConfigPath))
	if di.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %o", di.Mode().Perm())
	}
	// The test call carried the key from the env var.
	found := false
	for _, a := range *auths {
		if a == "chat:Bearer "+fakeKey {
			found = true
		}
	}
	if !found {
		t.Errorf("test call auth = %v", *auths)
	}
}

func TestFailureMessages(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"401", 401, `{"error":"bad key ` + fakeKey + `"}`, "key rejected"},
		{"403", 403, `{}`, "key rejected"},
		{"privacy 404", 404, privacy404Body, "https://openrouter.ai/settings/privacy"},
		{"other 404", 404, `{"error":"no such model"}`, "model not found"},
		{"429", 429, `{}`, "50 requests/day"},
		{"no tool call", 200, textReply, "model did not call the tool; tool calling may be unsupported"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := fakeServer(t, c.status, c.body)
			env, out, _ := newEnv(t, false, "")
			code := Run(context.Background(), Options{Provider: ProviderOpenAI, BaseURL: srv.URL, Model: "m", KeyEnv: "MY_KEY"}, env)
			if code == 0 {
				t.Fatalf("a failed test call must fail without --yes\n%s", out)
			}
			if !strings.Contains(out.String(), c.want) {
				t.Errorf("output lacks %q:\n%s", c.want, out)
			}
			if strings.Contains(out.String(), fakeKey) {
				t.Error("the key leaked into the output")
			}
			if c.name == "privacy 404" && (strings.Contains(out.String(), "ineligibility") || strings.Contains(out.String(), "guardrail")) {
				t.Error("the raw privacy JSON must not be shown")
			}
			if _, err := os.Stat(env.ConfigPath); err == nil {
				t.Error("no config may be written after a failed test")
			}
		})
	}
}

func TestYesSavesAfterFailedTestWithWarning(t *testing.T) {
	srv, _ := fakeServer(t, 429, `{}`)
	env, out, _ := newEnv(t, false, "")
	if code := Run(context.Background(), Options{Provider: ProviderOpenAI, BaseURL: srv.URL, Model: "m", Yes: true}, env); code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if !strings.Contains(out.String(), "saving anyway") {
		t.Errorf("missing warning:\n%s", out)
	}
	if _, err := os.Stat(env.ConfigPath); err != nil {
		t.Error("config not written")
	}
}

func TestSkipTestMakesNoChatCall(t *testing.T) {
	srv, auths := fakeServer(t, 500, `{}`)
	env, _, _ := newEnv(t, false, "")
	if code := Run(context.Background(), Options{Provider: ProviderOpenAI, BaseURL: srv.URL, Model: "m", SkipTest: true}, env); code != 0 {
		t.Fatal("exit != 0")
	}
	for _, a := range *auths {
		if strings.HasPrefix(a, "chat:") {
			t.Error("--skip-test must not call the chat endpoint")
		}
	}
}

func TestOpenRouterDefaultModelFromQuickstart(t *testing.T) {
	env, out, _ := newEnv(t, false, "")
	env.OpenRouterBase = "http://127.0.0.1:1" // never contacted with --skip-test
	if code := Run(context.Background(), Options{Provider: ProviderOpenRouter, KeyEnv: "OPENROUTER_API_KEY", SkipTest: true, Yes: true}, env); code != 0 {
		t.Fatalf("exit != 0\n%s", out)
	}
	_, m := readCfg(t, env)
	ep := m["endpoints"].(map[string]any)["openrouter"].(map[string]any)
	if ep["model"] != "nvidia/nemotron-3.5-super-vl:free" {
		t.Errorf("model = %v (quickstart default expected)", ep["model"])
	}
	if ep["api_key_env"] != "OPENROUTER_API_KEY" {
		t.Errorf("api_key_env = %v", ep["api_key_env"])
	}
}

func TestNoTTYWithoutProviderFails(t *testing.T) {
	env, out, _ := newEnv(t, false, "")
	if Run(context.Background(), Options{}, env) == 0 || !strings.Contains(out.String(), "--provider") {
		t.Errorf("want a --provider hint, got:\n%s", out)
	}
}

func TestKeyEnvRefusesAKey(t *testing.T) {
	env, out, _ := newEnv(t, false, "")
	if Run(context.Background(), Options{Provider: ProviderOpenAI, Model: "m", KeyEnv: fakeKey, SkipTest: true}, env) == 0 {
		t.Fatal("a key given as --key-env must be refused")
	}
	if strings.Contains(out.String(), fakeKey) {
		t.Error("the refused key was echoed")
	}
}

func TestExistingConfigIsRefusedWithoutForce(t *testing.T) {
	env, out, _ := newEnv(t, false, "")
	_ = os.MkdirAll(filepath.Dir(env.ConfigPath), 0o700)
	orig := `{"base_url":"http://mine:1"}`
	_ = os.WriteFile(env.ConfigPath, []byte(orig), 0o600)
	if Run(context.Background(), Options{Provider: ProviderOpenAI, Model: "m", SkipTest: true}, env) == 0 {
		t.Fatal("must refuse")
	}
	if b, _ := os.ReadFile(env.ConfigPath); string(b) != orig {
		t.Error("existing config was modified")
	}
	if !strings.Contains(out.String(), "--force") {
		t.Errorf("no --force hint:\n%s", out)
	}
}

func TestForceBacksUpFirst(t *testing.T) {
	env, out, _ := newEnv(t, false, "")
	_ = os.MkdirAll(filepath.Dir(env.ConfigPath), 0o700)
	orig := `{"base_url":"http://mine:1"}`
	_ = os.WriteFile(env.ConfigPath, []byte(orig), 0o644)
	if code := Run(context.Background(), Options{Provider: ProviderOpenAI, Model: "m", SkipTest: true, Force: true}, env); code != 0 {
		t.Fatalf("exit != 0\n%s", out)
	}
	bak := env.ConfigPath + ".bak-20261109T123000Z"
	b, err := os.ReadFile(bak)
	if err != nil || string(b) != orig {
		t.Fatalf("backup missing or different: %v %q", err, b)
	}
	if fi, _ := os.Stat(bak); fi.Mode().Perm() != 0o600 {
		t.Errorf("backup mode = %o", fi.Mode().Perm())
	}
	if !strings.Contains(out.String(), "Backed up the previous config to "+bak) {
		t.Errorf("backup not announced:\n%s", out)
	}
	if fi, _ := os.Stat(env.ConfigPath); fi.Mode().Perm() != 0o600 {
		t.Errorf("new config mode = %o", fi.Mode().Perm())
	}
	if nb, _ := os.ReadFile(env.ConfigPath); string(nb) == orig {
		t.Error("config not replaced")
	}
}

func TestForceWithFailingBackupLeavesOriginal(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	_ = os.WriteFile(path, []byte("orig"), 0o600)
	now := time.Date(2026, 11, 9, 12, 30, 0, 0, time.UTC)
	// Occupy the backup name: O_EXCL makes the backup fail.
	_ = os.WriteFile(path+".bak-20261109T123000Z", []byte("x"), 0o600)
	if _, err := writeConfig(path, []byte("new"), true, now, false); err == nil {
		t.Fatal("must fail when the backup cannot be made")
	}
	if b, _ := os.ReadFile(path); string(b) != "orig" {
		t.Error("original was modified although the backup failed")
	}
}

func TestInteractivePasteKeyStoresAuthHeaderAndNeverPrintsIt(t *testing.T) {
	srv, _ := fakeServer(t, 200, toolCallReply)
	// provider=2, base url, key mode=paste, model number 1, approval default, sandbox: hisn usable -> no question
	env, out, _ := newEnv(t, true, "2\n"+srv.URL+"\npaste\n1\n\n")
	if code := Run(context.Background(), Options{}, env); code != 0 {
		t.Fatalf("exit != 0\n%s", out)
	}
	txt, m := readCfg(t, env)
	if strings.Contains(out.String(), fakeKey) {
		t.Fatal("the key was printed")
	}
	ep := m["endpoints"].(map[string]any)["local"].(map[string]any)
	if ep["auth_header"] != "Bearer "+fakeKey {
		t.Errorf("auth_header missing: %s", txt)
	}
	if m["approval"] != "ask" {
		t.Errorf("approval = %v", m["approval"])
	}
}

func TestInteractiveRefusesKeyWhenEchoCannotBeDisabled(t *testing.T) {
	srv, _ := fakeServer(t, 200, toolCallReply)
	env, out, _ := newEnv(t, true, "2\n"+srv.URL+"\npaste\n")
	env.ReadSecret = func(string) (string, error) { return "", os.ErrInvalid }
	if Run(context.Background(), Options{}, env) == 0 {
		t.Fatal("must refuse")
	}
	if !strings.Contains(out.String(), "without echo") {
		t.Errorf("no explanation:\n%s", out)
	}
	if _, err := os.Stat(env.ConfigPath); err == nil {
		t.Error("nothing may be written")
	}
}

func TestInteractiveEnvNameRejectsAPastedKey(t *testing.T) {
	srv, _ := fakeServer(t, 200, toolCallReply)
	env, out, _ := newEnv(t, true, "2\n"+srv.URL+"\nenv\n"+fakeKey+"\nMY_KEY\n1\n\n")
	if code := Run(context.Background(), Options{}, env); code != 0 {
		t.Fatalf("exit != 0\n%s", out)
	}
	if strings.Contains(out.String(), fakeKey) {
		t.Fatal("a key typed as the variable name was echoed back")
	}
	txt, _ := readCfg(t, env)
	if strings.Contains(txt, fakeKey) || !strings.Contains(txt, `"api_key_env": "MY_KEY"`) {
		t.Errorf("config:\n%s", txt)
	}
}

func TestInteractiveFailureThenPickAnother(t *testing.T) {
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(modelsReply)) })
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(privacy404Body))
			return
		}
		_, _ = w.Write([]byte(toolCallReply))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	// provider 2, url, key none, model 1 (fails), pick, model 2, approval ask
	env, out, _ := newEnv(t, true, "2\n"+srv.URL+"\nnone\n1\npick\n2\n\n")
	if code := Run(context.Background(), Options{}, env); code != 0 {
		t.Fatalf("exit != 0\n%s", out)
	}
	if !strings.Contains(out.String(), "https://openrouter.ai/settings/privacy") {
		t.Errorf("privacy message missing:\n%s", out)
	}
	if calls != 2 {
		t.Errorf("chat calls = %d", calls)
	}
}

func TestInteractiveFailureThenAbortWritesNothing(t *testing.T) {
	srv, _ := fakeServer(t, 401, `{}`)
	env, out, _ := newEnv(t, true, "2\n"+srv.URL+"\nnone\n1\nabort\n")
	if Run(context.Background(), Options{}, env) == 0 {
		t.Fatal("abort must be an error exit")
	}
	if _, err := os.Stat(env.ConfigPath); err == nil {
		t.Errorf("config written after abort\n%s", out)
	}
}

func TestModelListOrderAndCap(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"data":[`)
	for i := 0; i < 30; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		tools := `["max_tokens"]`
		if i%2 == 1 {
			tools = `["tools"]`
		}
		sb.WriteString(`{"id":"m` + string(rune('a'+i%26)) + string(rune('a'+i/26)) + `","context_length":1000,"supported_parameters":` + tools + `}`)
	}
	sb.WriteString(`]}`)
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(sb.String())) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	env, out, _ := newEnv(t, true, "2\n"+srv.URL+"\nnone\n1\n\n")
	_ = Run(context.Background(), Options{SkipTest: true}, env)
	if !strings.Contains(out.String(), "15 shown of 30") {
		t.Errorf("cap/count line missing:\n%s", out)
	}
	if strings.Count(out.String(), "tools\n") < 15 {
		t.Errorf("the first 15 must all be tool-capable:\n%s", out)
	}
}

func TestFreeModelNoteAndPricing(t *testing.T) {
	srv, _ := fakeServer(t, 200, toolCallReply)
	env, out, _ := newEnv(t, true, "2\n"+srv.URL+"\nnone\n1\n\n")
	_ = Run(context.Background(), Options{SkipTest: true}, env)
	for _, w := range []string{"free: may train on prompts; blocked if your OpenRouter privacy settings forbid that; 50 requests/day without credits", "$3/$15 per 1M in/out", "128k ctx"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("missing %q:\n%s", w, out)
		}
	}
}

func TestSandboxStepContainerHintAndChoice(t *testing.T) {
	srv, _ := fakeServer(t, 200, toolCallReply)
	env, out, _ := newEnv(t, true, "2\n"+srv.URL+"\nnone\n1\n\ndirect\n")
	env.ProbeSandbox = func(context.Context) (string, string) {
		return "0.4.2", "hisn check failed: [FAIL] user namespaces"
	}
	env.InContainer = func() bool { return true }
	if code := Run(context.Background(), Options{SkipTest: true}, env); code != 0 {
		t.Fatalf("exit != 0\n%s", out)
	}
	if !strings.Contains(out.String(), "running in a container: Docker's default seccomp profile blocks user namespaces") {
		t.Errorf("container hint missing:\n%s", out)
	}
	_, m := readCfg(t, env)
	if m["sandbox"].(map[string]any)["backend"] != "direct" {
		t.Errorf("sandbox = %v", m["sandbox"])
	}
}

func TestSandboxFlagEqualToAutomaticIsNotWritten(t *testing.T) {
	env, _, _ := newEnv(t, false, "")
	if code := Run(context.Background(), Options{Provider: ProviderOpenAI, Model: "m", SkipTest: true, Sandbox: "hisn"}, env); code != 0 {
		t.Fatal("exit != 0")
	}
	if _, m := readCfg(t, env); m["sandbox"] != nil {
		t.Error("the automatic choice must not be written")
	}
}

func TestGoldenConfigShape(t *testing.T) {
	cases := map[string]plan{
		"openrouter_env.json": {EndpointName: "openrouter", BaseURL: "https://openrouter.ai/api", Model: "nvidia/nemotron-3.5-super-vl:free", KeyEnv: "OPENROUTER_API_KEY", Approval: "ask"},
		"local_direct.json":   {EndpointName: "local", BaseURL: "http://localhost:11434", Model: "qwen3", Approval: "auto", Backend: "direct"},
	}
	for name, p := range cases {
		got, err := renderConfig(p)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("testdata", name)
		if *update {
			_ = os.WriteFile(path, got, 0o644)
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(want) {
			t.Errorf("%s differs:\n--- got\n%s--- want\n%s", name, got, want)
		}
		if strings.Contains(string(got), "_comment") {
			t.Error("no _comment keys allowed")
		}
	}
}

var update = flag.Bool("update", false, "rewrite golden files")

// A server that echoes the key inside a long body must not leak any fragment,
// even when the 200-byte cut would land inside the key.
func TestEchoedKeyIsScrubbedBeforeTruncation(t *testing.T) {
	pad := strings.Repeat("x", 185)
	srv, _ := fakeServer(t, 500, pad+fakeKey+" tail")
	env, out, _ := newEnv(t, false, "")
	_ = Run(context.Background(), Options{Provider: ProviderOpenAI, BaseURL: srv.URL, Model: "m", KeyEnv: "MY_KEY"}, env)
	for i := 6; i <= len(fakeKey); i++ {
		if strings.Contains(out.String(), fakeKey[:i]) {
			t.Fatalf("a key fragment (%d chars) reached the output:\n%s", i, out)
		}
	}
}

func TestExistingPermissiveDirIsTightenedForAKey(t *testing.T) {
	env, out, dir := newEnv(t, false, "")
	d := filepath.Join(dir, "wakil")
	_ = os.MkdirAll(d, 0o755)
	_ = os.Chmod(d, 0o755)
	env.ConfigPath = filepath.Join(d, "config.json")
	env.ReadSecret = func(string) (string, error) { return fakeKey, nil }
	in := "openai-compatible\nhttp://127.0.0.1:1\npaste\nsome-model\n\n"
	env.In = strings.NewReader(in)
	env.Interactive = true
	// --skip-test keeps the run offline.
	if code := Run(context.Background(), Options{SkipTest: true}, env); code != 0 {
		t.Fatalf("exit != 0\n%s", out)
	}
	if fi, _ := os.Stat(d); fi.Mode().Perm() != 0o700 {
		t.Errorf("dir mode = %o, want 0700", fi.Mode().Perm())
	}
}

func TestForeignPermissiveDirIsRefusedForAKey(t *testing.T) {
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o755)
	path := filepath.Join(dir, "config.json")
	fi, _ := os.Stat(dir)
	if err := secureDir(dir, fi); err == nil || !strings.Contains(err.Error(), "--key-env") {
		t.Fatalf("want a refusal pointing at --key-env, got %v", err)
	}
	if _, err := writeConfig(path, []byte("x"), false, time.Now(), true); err == nil {
		t.Error("writeConfig must refuse a shared directory when the file holds a key")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("nothing may be written")
	}
	// Without a key in the file the directory is not our concern.
	if _, err := writeConfig(path, []byte("x"), false, time.Now(), false); err != nil {
		t.Errorf("no-key write must work: %v", err)
	}
}

func TestKeyShapedStringIsNotAnEnvName(t *testing.T) {
	long := "sk0123456789abcdef0123456789abcdef0123"
	if validEnvName(long) || !validEnvName("OPENROUTER_API_KEY") || validEnvName("a-b") {
		t.Error("validEnvName")
	}
	env, out, _ := newEnv(t, false, "")
	if Run(context.Background(), Options{Provider: ProviderOpenAI, Model: "m", KeyEnv: long, SkipTest: true}, env) == 0 {
		t.Fatal("must refuse")
	}
	if strings.Contains(out.String(), long) {
		t.Error("the refused value was echoed")
	}
}
