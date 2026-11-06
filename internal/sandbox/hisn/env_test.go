package hisn

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/sandbox"
)

func TestScrubbedEnv(t *testing.T) {
	base := []string{"PATH=/bin", "HOME=/h", "LC_ALL=C", "LANG=x", "OPENAI_API_KEY=sk-1", "LD_PRELOAD=/e.so", "GOFLAGS=-v", "CI=1", "TERM=xterm"}
	got := strings.Join(scrubbedEnv(base, nil), "\n")
	for _, bad := range []string{"OPENAI_API_KEY", "LD_PRELOAD", "GOFLAGS", "CI="} {
		if strings.Contains(got, bad) {
			t.Errorf("%s leaked into a hisn client env:\n%s", bad, got)
		}
	}
	for _, good := range []string{"PATH=", "HOME=", "LC_ALL=", "LANG=", "TERM="} {
		if !strings.Contains(got, good) {
			t.Errorf("%s missing:\n%s", good, got)
		}
	}
	withPass := strings.Join(scrubbedEnv(base, []string{"GOFLAGS", "CI"}), "\n")
	if !strings.Contains(withPass, "GOFLAGS=-v") || strings.Contains(withPass, "OPENAI_API_KEY") || strings.Contains(withPass, "LD_PRELOAD") {
		t.Errorf("env_pass handling wrong:\n%s", withPass)
	}
}

// Every hisn invocation (preflight, validate, run, exec, ls, inspect, kill,
// grant, revoke) must carry a scrubbed environment.
func TestAllInvocationsScrubbed(t *testing.T) {
	fe := newFakeEnv(t)
	o := fe.options()
	o.GrantableRW = []string{"/srv/out"}
	b, err := Open(ctxT(t), o)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(ctxT(t))
	check := func(step string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(fe.state, "last_env"))
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		env := string(data)
		for _, bad := range []string{"ANTHROPIC_API_KEY", "sk-should-never-leak", "LD_PRELOAD"} {
			if strings.Contains(env, bad) {
				t.Errorf("%s: %s reached a hisn client:\n%s", step, bad, env)
			}
		}
	}
	check("preflight")
	_ = b.Start(ctxT(t), sandbox.Session{ID: "e", HostCwd: fe.cwd})
	check("start")
	run(t, b, "true")
	check("exec")
	_ = b.Grant(ctxT(t), "/srv/ref", false)
	check("grant")
	_ = b.Revoke(ctxT(t), "/srv/ref")
	check("revoke")
	_ = b.Stop(ctxT(t))
	check("stop")
}

// Only `hisn run` receives env_pass names (hisn snapshots them at session start).
func TestEnvPassOnlyOnRun(t *testing.T) {
	fe := newFakeEnv(t)
	b, _ := Open(ctxT(t), fe.options())
	defer b.Close(ctxT(t))
	_ = b.Start(ctxT(t), sandbox.Session{ID: "e", HostCwd: fe.cwd})
	_ = b.Stop(ctxT(t)) // last invocation is `kill`/`ls`: no pass-through
	data, _ := os.ReadFile(filepath.Join(fe.state, "last_env"))
	if strings.Contains(string(data), "GOFLAGS") {
		t.Errorf("env_pass leaked to a non-run hisn client:\n%s", data)
	}
}

// One command factory: no other call site may build a process.
func TestNoStrayExecCommand(t *testing.T) {
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, e := range ents {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, n, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			if imp.Path.Value == `"os/exec"` && filepath.Base(n) != "cmd.go" {
				t.Errorf("%s imports os/exec; only cmd.go (the command factory) may", n)
			}
		}
	}
}

// F9: validateExecEnv allows GIT_CONFIG_COUNT, GIT_CONFIG_KEY_*, and
// GIT_CONFIG_VALUE_* when the session carries signing (SigningKey != ""). They
// are Wakil's own process environment (not command text), so the F6 guard is
// unaffected. When signing is off, they are rejected like any other
// non-allowlisted env var.
func TestValidateExecEnvSigningConfig(t *testing.T) {
	// Signing on: GIT_CONFIG_* env vars are allowed.
	b := &Backend{opts: Options{
		SigningKey: "ssh-ed25519 AAAA test",
		EnvPass:    []string{"CI"},
	}}
	env := map[string]string{
		"GIT_CONFIG_COUNT":   "5",
		"GIT_CONFIG_KEY_0":   "user.signingkey",
		"GIT_CONFIG_VALUE_0": "key::ssh-ed25519 AAAA test",
	}
	out, err := b.validateExecEnv(env)
	if err != nil {
		t.Fatalf("signing on: validateExecEnv failed: %v", err)
	}
	for k, v := range env {
		if out[k] != v {
			t.Errorf("signing on: out[%q] = %q, want %q", k, out[k], v)
		}
	}

	// Signing off: GIT_CONFIG_* env vars are rejected.
	b2 := &Backend{opts: Options{
		EnvPass: []string{"CI"},
	}}
	_, err = b2.validateExecEnv(env)
	if err == nil {
		t.Fatal("signing off: GIT_CONFIG_* should be rejected")
	}
	if !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("signing off: error should say 'not allowed', got: %v", err)
	}
}

// F9: TestIntegrationEnvIsAllowlisted must still pass with GIT_CONFIG_* env
// vars injected — they are allowed by validateExecEnv when signing is on, and
// the in-sandbox env stays allowlisted (the host socket path must not appear).
// This is verified by the existing TestIntegrationEnvIsAllowlisted test.
