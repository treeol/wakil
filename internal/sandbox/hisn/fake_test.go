package hisn

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The fake hisn is this test binary re-executed through a one-line shell
// wrapper (which carries HISN_FAKE_STATE, since Wakil scrubs the client env).
// State lives under $HISN_FAKE_STATE/s/<id>/.
func TestMain(m *testing.M) {
	if st := os.Getenv("HISN_FAKE_STATE"); st != "" {
		os.Exit(fakeMain(st, os.Args[1:]))
	}
	if os.Getenv(parentHelperEnv) != "" {
		os.Exit(parentHelperMain())
	}
	os.Exit(m.Run())
}

// sessRoot is where the fake keeps its sessions: hisn's own runtime directory
// layout, $XDG_RUNTIME_DIR/hisn/<id>/control.sock. (Tests always set
// XDG_RUNTIME_DIR; the state-dir fallback only serves stray invocations.)
func sessRoot(state string) string {
	if x := os.Getenv("XDG_RUNTIME_DIR"); x != "" {
		return filepath.Join(x, "hisn")
	}
	return filepath.Join(state, "s")
}

// dialable reports whether something accepts connections on the session socket.
func dialable(dir string) bool {
	if exists(filepath.Join(dir, "refuse")) { // test hook: the socket stops accepting
		return false
	}
	c, err := net.DialTimeout("unix", filepath.Join(dir, "control.sock"), time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
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
			v = []byte("0.2.1")
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
	case "profile":
		return fakeProfileCmd(args[1:])
	case "run":
		return fakeRun(state, args[1:])
	case "ls":
		return fakeLs(state, args[1:])
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

func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name {
			return true
		}
	}
	return false
}

// fakeRun is the supervisor: like the real `hisn run --detach` it does NOT
// return while the session lives. run_mode (a file in the fake state) selects a
// failure to inject: fail | hang | nosock | late:<ms> | crash:<ms> | ignoreterm.
func fakeRun(state string, args []string) int {
	id := flagVal(args, "--session-id")
	verbose := hasFlag(args, "-v")
	logf := func(format string, a ...any) {
		if verbose { // real hisn prints nothing to stderr without -v
			fmt.Fprintf(os.Stderr, "hisn: "+format+"\n", a...)
		}
	}
	modeB, _ := os.ReadFile(filepath.Join(state, "run_mode"))
	mode, arg, _ := strings.Cut(strings.TrimSpace(string(modeB)), ":")
	dir := filepath.Join(sessRoot(state), id)
	if mode == "fail" {
		fmt.Fprintln(os.Stderr, "hisn: injected failure: cannot create sandbox")
		return 125
	}
	if mode == "hang" { // never becomes ready, no socket
		logf("session %s starting", id)
		time.Sleep(time.Hour)
		return 125
	}
	var pol []byte
	var risks []fakeRisk
	if hasFlag(args, "--profile") {
		if hasFlag(args, "--policy") {
			fmt.Fprintln(os.Stderr, "hisn: E_PROFILE_POLICY: --profile and --policy are mutually exclusive")
			return 125
		}
		text, r, rc := fakeRunProfile(state, args)
		if rc != 0 {
			return rc
		}
		pol, risks = []byte(text), r
	} else {
		var err error
		pol, err = os.ReadFile(flagVal(args, "--policy"))
		if err != nil {
			fmt.Fprintln(os.Stderr, "hisn: no policy")
			return 125
		}
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return 125
	}
	// 0.2.1: a STALE dir under this id is replaced (removed) and the run
	// proceeds; a LIVE one (something answers) is E_EXISTS.
	if exists(dir) && dialable(dir) {
		fmt.Fprintln(os.Stderr, "hisn: session id in use")
		return 125
	}
	if exists(dir) {
		if verbose {
			logf("session %s was stale; replaced", id)
		}
		_ = os.RemoveAll(dir)
	}
	if err := os.Mkdir(dir, 0o700); err != nil { // fails only in a genuine race
		fmt.Fprintln(os.Stderr, "hisn: session id in use")
		return 125
	}
	_ = os.WriteFile(filepath.Join(dir, "policy"), pol, 0o600)
	rj, _ := json.Marshal(nonNilRisks(risks))
	_ = os.WriteFile(filepath.Join(dir, "risk.json"), rj, 0o600)
	// Real hisn writes the resolved policy before it binds the socket; Wakil
	// uses it (carrying the instance hostname token) as proof of ownership.
	_ = os.WriteFile(filepath.Join(dir, "policy.resolved.toml"), pol, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "cwd"), []byte(flagVal(args, "--cwd")), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "sup.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	logf("session %s init pid %d", id, os.Getpid())
	if mode == "nosock" { // dir but no socket, ever
		time.Sleep(time.Hour)
		return 125
	}
	if mode == "late" {
		ms, _ := strconv.Atoi(arg)
		time.Sleep(time.Duration(ms) * time.Millisecond)
	}
	ln, err := net.Listen("unix", filepath.Join(dir, "control.sock"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "hisn: binding control socket: %v\n", err)
		return 125
	}
	if mode == "stale" { // socket file exists, nobody accepts: `hisn ls` says "stale"
		ln.(*net.UnixListener).SetUnlinkOnClose(false)
		_ = ln.Close()
	}
	fakeCtlServe(ln, dir) // §7 protocol: exec/signal/grant/revoke/list/inspect/kill
	if mode == "flood" {  // ready, floods stderr (to jam a stalled log), then dies
		fmt.Fprint(os.Stderr, strings.Repeat("hisn: flood line\n", 20000))
		return 125
	}
	if mode == "crash" { // ready, then dies leaving its directory (like SIGKILL)
		// 0.2.1 semantics: --ready-fd gets "ready\n" once the socket accepts,
		// before any crash. A crash INSIDE the confirm window (before readyPoll
		// elapses) must be caught by waitReady; a crash after it is a session
		// that was ready and died later (ErrSessionDead, not a failed start).
		readyWrite(args)
		ms, _ := strconv.Atoi(arg)
		time.Sleep(time.Duration(ms) * time.Millisecond)
		fmt.Fprintln(os.Stderr, "hisn: injected crash: supervisor lost its init")
		return 125
	}
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	readyWrite(args) // 0.2.1: the session is live — announce it on the ready fd
	for range sig {
		if mode == "ignoreterm" {
			continue
		}
		killRecordedPids(dir) // init dying takes the whole sandbox with it
		_ = ln.Close()
		_ = os.RemoveAll(dir) // an orderly supervisor removes its own directory
		return 0
	}
	return 0
}

func killRecordedPids(dir string) {
	b, err := os.ReadFile(filepath.Join(dir, "pids"))
	if err != nil {
		return
	}
	for _, f := range strings.Fields(string(b)) {
		if pid, _ := strconv.Atoi(f); pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// readyWrite implements `--ready-fd N` (hisn 0.2.1): the whole "ready\n" line
// to fd N once the session is live, then the fd is closed. Missing or unwritable
// fd → E_BAD_REQUEST on stderr, exit 125 (a start failure carries nothing).
func readyWrite(args []string) {
	n := 0
	for i, a := range args {
		if a == "--ready-fd" && i+1 < len(args) {
			n, _ = strconv.Atoi(args[i+1])
		}
	}
	if n <= 0 {
		return
	}
	if _, err := syscall.Write(n, []byte("ready\n")); err != nil {
		fmt.Fprintf(os.Stderr, "hisn: --ready-fd %d: E_BAD_REQUEST: %v\n", n, err)
	}
	_ = syscall.Close(n)
}

func fakeLs(state string, args []string) int {
	if id := flagVal(args, "--session"); id != "" {
		if !dialable(filepath.Join(sessRoot(state), id)) {
			fmt.Fprintf(os.Stderr, "hisn: no such session %s\n", id)
			return 125
		}
		fmt.Println(`{"ok":true,"grants":[]}`)
		return 0
	}
	if hasFlag(args, "--prune") { // 0.2.1: remove stale dirs (socket refused AND supervisor gone)
		ents, _ := os.ReadDir(sessRoot(state))
		for _, e := range ents {
			d := filepath.Join(sessRoot(state), e.Name())
			if exists(filepath.Join(d, "sup.pid")) && !dialable(d) {
				fmt.Fprintf(os.Stderr, "hisn: pruned stale session %s\n", e.Name())
				_ = os.RemoveAll(d)
			}
		}
		return 0
	}
	ents, _ := os.ReadDir(sessRoot(state))
	sessions := []map[string]any{}
	for _, e := range ents {
		d := filepath.Join(sessRoot(state), e.Name())
		// A session without a supervisor pid file is a foreign fixture: live.
		live := true
		if exists(filepath.Join(d, "sup.pid")) {
			live = dialable(d)
		}
		st := "stale"
		if live {
			st = "live"
		}
		cwd := ""
		if b, err := os.ReadFile(filepath.Join(d, "cwd")); err == nil {
			cwd = string(b)
		}
		sessions = append(sessions, map[string]any{"id": e.Name(), "state": st, "cwd": cwd, "execs": []any{}})
	}
	b, _ := json.Marshal(map[string]any{"schema": "hisn-ls/v1", "sessions": sessions})
	fmt.Println(string(b))
	return 0
}

func fakeInspect(state string, args []string) int {
	if exists(filepath.Join(state, "inspect_fail")) {
		fmt.Fprintln(os.Stderr, "hisn: inspect failed")
		return 125
	}
	pol, err := os.ReadFile(filepath.Join(sessRoot(state), flagVal(args, "--session"), "policy"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "hisn: no such session")
		return 125
	}
	var risks []fakeRisk
	if rj, err := os.ReadFile(filepath.Join(sessRoot(state), flagVal(args, "--session"), "risk.json")); err == nil {
		_ = json.Unmarshal(rj, &risks)
	}
	b, _ := json.Marshal(map[string]any{"ok": true, "mountinfo": "", "policy": string(pol),
		"risk": nonNilRisks(overrideRisk(state, risks)), "services": []any{}})
	fmt.Println(string(b))
	return 0
}

func fakeKill(state string, args []string) int {
	dir := filepath.Join(sessRoot(state), flagVal(args, "--session"))
	// 0.2.1: kill on a STALE session (socket refused) removes the dir and
	// exits 0 — even under kill_fail (a live-session kill failure injection).
	if exists(dir) && exists(filepath.Join(dir, "sup.pid")) && !dialable(dir) {
		fmt.Fprintf(os.Stderr, "hisn: session %s was stale; removed\n", flagVal(args, "--session"))
		_ = os.RemoveAll(dir)
		return 0
	}
	if exists(filepath.Join(state, "kill_hang")) {
		time.Sleep(time.Hour)
	}
	if exists(filepath.Join(state, "kill_fail")) {
		fmt.Fprintln(os.Stderr, "hisn: kill failed")
		return 125
	}
	if !exists(dir) {
		fmt.Fprintln(os.Stderr, "hisn: no such session")
		return 125
	}
	if b, err := os.ReadFile(filepath.Join(dir, "sup.pid")); err == nil {
		if !dialable(dir) {
			// 0.2.1: kill on a stale session removes the dir and exits 0.
			fmt.Fprintf(os.Stderr, "hisn: session %s was stale; removed\n", flagVal(args, "--session"))
			_ = os.RemoveAll(dir)
			return 0
		}
		if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
		return 0
	}
	killRecordedPids(dir) // foreign fixture without a supervisor process
	_ = os.RemoveAll(dir)
	return 0
}

func fakeGrant(state, op string, args []string) int {
	id := flagVal(args, "--session")
	dir := filepath.Join(sessRoot(state), id)
	if !exists(dir) {
		fmt.Fprintln(os.Stderr, "hisn: no such session")
		return 125
	}
	if exists(filepath.Join(dir, "sup.pid")) && !dialable(dir) {
		fmt.Fprintln(os.Stderr, "hisn: connecting to control socket: Connection refused (os error 111)")
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
	dir := filepath.Join(sessRoot(state), id)
	if !exists(dir) {
		fmt.Fprintln(os.Stderr, "hisn: exec: E_NOT_FOUND: no such session")
		return 125
	}
	if exists(filepath.Join(dir, "sup.pid")) && !dialable(dir) {
		fmt.Fprintln(os.Stderr, "hisn: connecting to control socket: Connection refused (os error 111)")
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
	xdg   string // XDG_RUNTIME_DIR handed to every hisn client
}

func newFakeEnv(t *testing.T) *fakeEnv {
	t.Helper()
	root := t.TempDir()
	state := filepath.Join(root, "fake")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "hisn")
	// GORACE: a -race binary sleeps 1 s at exit by default; every fake hisn call
	// is such a process, and readiness makes several per probe. The real hisn
	// has no such delay.
	script := fmt.Sprintf("#!/bin/sh\nGORACE=atexit_sleep_ms=0 HISN_FAKE_STATE='%s' exec '%s' \"$@\"\n", state, self)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(root, "ws")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	real, _ := filepath.EvalSymlinks(cwd)
	// Short path: unix socket paths are limited to ~100 bytes.
	xdg, err := os.MkdirTemp("", "hx")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(xdg) })
	return &fakeEnv{t: t, state: state, bin: bin, home: filepath.Join(root, "home"), cwd: real, sdir: filepath.Join(root, "wakil-state"), xdg: xdg}
}

func (f *fakeEnv) options() Options {
	return Options{
		Binary:   f.bin,
		StateDir: f.sdir,
		HomeRoot: f.home,
		HostCwd:  f.cwd,
		EnvPass:  []string{"GOFLAGS", "CI"},
		// A hostile host environment: none of this may reach hisn clients.
		Env: []string{"PATH=" + os.Getenv("PATH"), "HOME=/home/x", "LANG=C.UTF-8", "LC_ALL=C", "XDG_RUNTIME_DIR=" + f.xdg,
			"ANTHROPIC_API_KEY=sk-should-never-leak", "LD_PRELOAD=/evil.so", "GOFLAGS=-mod=mod", "CI=1"},
	}
}

// rt is the fake's session root (hisn's runtime directory).
func (f *fakeEnv) rt() string { return filepath.Join(f.xdg, "hisn") }

// setMode injects a supervisor failure mode for the next `hisn run`.
func (f *fakeEnv) setMode(m string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.state, "run_mode"), []byte(m), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fakeEnv) sessionIDs() []string {
	ents, _ := os.ReadDir(f.rt())
	var ids []string
	for _, e := range ents {
		ids = append(ids, e.Name())
	}
	return ids
}

func (f *fakeEnv) addForeignSession(id string) {
	f.t.Helper()
	dir := filepath.Join(f.rt(), id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(dir, "policy"), []byte(`hostname = "someone-else"`), 0o600)
}
