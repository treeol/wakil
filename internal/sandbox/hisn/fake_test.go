package hisn

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// The fake hisn is this test binary re-executed through a one-line shell
// wrapper (which carries HISN_FAKE_STATE, since Wakil scrubs the client env).
// State lives under $HISN_FAKE_STATE/s/<id>/.
func TestMain(m *testing.M) {
	if st := os.Getenv("HISN_FAKE_STATE"); st != "" {
		os.Exit(fakeMain(st, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeMain(state string, args []string) int {
	// Record the environment every hisn invocation received (for scrubbing tests).
	_ = os.WriteFile(filepath.Join(state, "last_env"), []byte(strings.Join(os.Environ(), "\n")), 0o600)
	appendLog(state, strings.Join(args, " "))
	if len(args) == 0 {
		return 125
	}
	switch args[0] {
	case "--version":
		v, err := os.ReadFile(filepath.Join(state, "version"))
		if err != nil {
			v = []byte("0.2.0")
		}
		fmt.Printf("hisn %s\n", strings.TrimSpace(string(v)))
		return 0
	case "check":
		if exists(filepath.Join(state, "check_fail")) {
			fmt.Println("[FAIL] user namespaces")
			return 2
		}
		return 0
	case "policy":
		return fakePolicyValidate(args[2:])
	case "run":
		return fakeRun(state, args[1:])
	case "ls":
		return fakeLs(state)
	case "inspect":
		return fakeInspect(state, args[1:])
	case "kill":
		return fakeKill(state, args[1:])
	case "exec":
		return fakeExec(state, args[1:])
	case "grant", "revoke":
		return fakeGrant(state, args[0], args[1:])
	}
	fmt.Fprintln(os.Stderr, "hisn: unknown command")
	return 125
}

func appendLog(state, line string) {
	f, err := os.OpenFile(filepath.Join(state, "calls.log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err == nil {
		fmt.Fprintln(f, line)
		f.Close()
	}
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func flagVal(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func fakePolicyValidate(args []string) int {
	if len(args) == 0 {
		return 125
	}
	b, err := os.ReadFile(args[0])
	if err != nil || !strings.Contains(string(b), `version = "hisn-policy/v1"`) {
		fmt.Fprintln(os.Stderr, "hisn: invalid policy")
		return 125
	}
	return 0
}

func fakeRun(state string, args []string) int {
	id := flagVal(args, "--session-id")
	dir := filepath.Join(state, "s", id)
	if exists(dir) {
		fmt.Fprintln(os.Stderr, "hisn: session id in use")
		return 125
	}
	pol, err := os.ReadFile(flagVal(args, "--policy"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "hisn: no policy")
		return 125
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return 125
	}
	_ = os.WriteFile(filepath.Join(dir, "policy"), pol, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "cwd"), []byte(flagVal(args, "--cwd")), 0o600)
	return 0
}

func fakeLs(state string) int {
	ents, _ := os.ReadDir(filepath.Join(state, "s"))
	rows := []map[string]any{}
	for _, e := range ents {
		rows = append(rows, map[string]any{"id": e.Name(), "live": true})
	}
	b, _ := json.Marshal(rows)
	fmt.Println(string(b))
	return 0
}

func fakeInspect(state string, args []string) int {
	if exists(filepath.Join(state, "inspect_fail")) {
		fmt.Fprintln(os.Stderr, "hisn: inspect failed")
		return 125
	}
	pol, err := os.ReadFile(filepath.Join(state, "s", flagVal(args, "--session"), "policy"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "hisn: no such session")
		return 125
	}
	b, _ := json.Marshal(map[string]any{"ok": true, "mountinfo": "", "policy": string(pol)})
	fmt.Println(string(b))
	return 0
}

func fakeKill(state string, args []string) int {
	if exists(filepath.Join(state, "kill_fail")) {
		fmt.Fprintln(os.Stderr, "hisn: kill failed")
		return 125
	}
	dir := filepath.Join(state, "s", flagVal(args, "--session"))
	if !exists(dir) {
		fmt.Fprintln(os.Stderr, "hisn: no such session")
		return 125
	}
	if b, err := os.ReadFile(filepath.Join(dir, "pids")); err == nil {
		for _, f := range strings.Fields(string(b)) {
			var pid int
			fmt.Sscan(f, &pid)
			if pid > 0 {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}
	_ = os.RemoveAll(dir)
	return 0
}

func fakeGrant(state, op string, args []string) int {
	id := flagVal(args, "--session")
	dir := filepath.Join(state, "s", id)
	if !exists(dir) {
		fmt.Fprintln(os.Stderr, "hisn: no such session")
		return 125
	}
	path := args[len(args)-1]
	rw := false
	for _, a := range args {
		if a == "--rw" {
			rw = true
		}
	}
	if op == "grant" && rw {
		pol, _ := os.ReadFile(filepath.Join(dir, "policy"))
		ok := false
		for _, line := range strings.Split(string(pol), "\n") {
			if strings.Contains(line, "grantable_rw") {
				// Prefix match against every quoted entry on or after this line.
				for _, q := range strings.Split(strings.SplitN(string(pol), "grantable_rw", 2)[1], `"`) {
					if strings.HasPrefix(q, "/") && (path == q || strings.HasPrefix(path, q+"/")) {
						ok = true
					}
				}
			}
		}
		if !ok {
			fmt.Fprintln(os.Stderr, "hisn: E_NOT_GRANTABLE: path outside grantable_rw")
			return 125
		}
	}
	_ = os.WriteFile(filepath.Join(dir, "grant_"+op), []byte(path), 0o600)
	return 0
}

func fakeExec(state string, args []string) int {
	id := flagVal(args, "--session")
	dir := filepath.Join(state, "s", id)
	if !exists(dir) {
		fmt.Fprintln(os.Stderr, "hisn: exec: E_NOT_FOUND: no such session")
		return 125
	}
	var cmdline []string
	cwd := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--session":
			i++
		case "--cwd":
			i++
			cwd = args[i]
		case "--env":
			i++
			k, _, _ := strings.Cut(args[i], "=")
			pol, _ := os.ReadFile(filepath.Join(dir, "policy"))
			if !strings.Contains(string(pol), `"`+k+`"`) {
				fmt.Fprintf(os.Stderr, "hisn: exec: E_BAD_REQUEST: env key %s not in env.pass\n", k)
				return 125
			}
		case "--":
			cmdline = args[i+1:]
			i = len(args)
		}
	}
	if len(cmdline) == 0 {
		return 125
	}
	if cmdline[0] == "__lost" {
		return 125 // v0.2.0: a lost exec exits 125 with no diagnostic
	}
	c := exec.Command(cmdline[0], cmdline[1:]...)
	c.Dir = cwd
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := c.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "hisn: exec: E_EXEC: %v\n", err)
		return 127
	}
	pf, _ := os.OpenFile(filepath.Join(dir, "pids"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if pf != nil {
		fmt.Fprintf(pf, "%d\n", c.Process.Pid)
		pf.Close()
	}
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		for s := range sig {
			_ = c.Process.Signal(s)
		}
	}()
	err := c.Wait()
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	return 125
}

// ---- test helpers ----

type fakeEnv struct {
	t     *testing.T
	state string
	bin   string
	home  string
	cwd   string
	sdir  string // wakil-side state dir
}

func newFakeEnv(t *testing.T) *fakeEnv {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "fake")
	if err := os.MkdirAll(filepath.Join(state, "s"), 0o700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "hisn")
	script := fmt.Sprintf("#!/bin/sh\nHISN_FAKE_STATE='%s' exec '%s' \"$@\"\n", state, self)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(root, "ws")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(cwd)
	return &fakeEnv{t: t, state: state, bin: bin, home: filepath.Join(root, "home"), cwd: real, sdir: filepath.Join(root, "wakil-state")}
}

func (f *fakeEnv) options() Options {
	return Options{
		Binary:   f.bin,
		StateDir: f.sdir,
		HomeRoot: f.home,
		HostCwd:  f.cwd,
		EnvPass:  []string{"GOFLAGS", "CI"},
		// A hostile host environment: none of this may reach hisn clients.
		Env: []string{"PATH=" + os.Getenv("PATH"), "HOME=/home/x", "LANG=C.UTF-8", "LC_ALL=C",
			"ANTHROPIC_API_KEY=sk-should-never-leak", "LD_PRELOAD=/evil.so", "GOFLAGS=-mod=mod", "CI=1"},
	}
}

func (f *fakeEnv) sessionIDs() []string {
	ents, _ := os.ReadDir(filepath.Join(f.state, "s"))
	var ids []string
	for _, e := range ents {
		ids = append(ids, e.Name())
	}
	return ids
}

func (f *fakeEnv) addForeignSession(id string) {
	f.t.Helper()
	dir := filepath.Join(f.state, "s", id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "policy"), []byte(`hostname = "someone-else"`), 0o600)
}
