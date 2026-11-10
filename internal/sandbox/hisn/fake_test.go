package hisn

import (
	"encoding/json"
	"errors"
	"flag"
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

// TestMain dispatches the fake hisn and its detach041 child supervisor.
func TestMain(m *testing.M) {
	if st := os.Getenv("HISN_FAKE_STATE"); st != "" {
		if supervisor := os.Getenv("HISN_FAKE_SUPERVISOR"); supervisor != "" {
			os.Exit(fakeSupervisorMain(supervisor, st, os.Args[1:]))
		}
		os.Exit(fakeMain(st, os.Args[1:]))
	}
	if os.Getenv(parentHelperEnv) != "" {
		os.Exit(parentHelperMain())
	}
	flag.Parse() // integrationRun reads -test.run
	if left, err := sweepITSessions(); err != nil || len(left) > 0 {
		fmt.Fprintf(os.Stderr, "hisn: integration leftovers before the run: %v %v\n", left, err)
	}
	code := m.Run()
	if left, err := sweepITSessions(); err != nil || len(left) > 0 {
		fmt.Fprintf(os.Stderr, "FAIL: hisn sessions left after the run: %v %v\n", left, err)
		if code == 0 {
			code = 1
		}
	}
	removeLeanFake()
	os.Exit(code)
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
			v = []byte("0.4.2")
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
	case "stop":
		return fakeStop(state, args[1:])
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
// return while the session lives (0.4.0). 0.4.1: the run process exits 0
// after printing the ready line; the supervisor lives on as a child that
// serves the control socket. run_mode (a file in the fake state) selects a
// failure to inject: fail | hang | nosock | late:<ms> | crash:<ms> | ignoreterm
// | detach041.
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
	name := flagVal(args, "--name")
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
	// hisn 0.4.0: a name is unique per user and stays reserved by a stopped or
	// crashed directory until `hisn ls --prune` removes it (measured against
	// the real binary). A name equal to an existing id is refused too.
	if name != "" {
		if !validFakeName(name) {
			fmt.Fprintf(os.Stderr, "hisn: invalid session name %q: use letters, digits, '.', '_' or '-' (no leading '.')\n", name)
			return 125
		}
		if owner := fakeNameOwner(state, name); owner != "" {
			fmt.Fprintf(os.Stderr, "hisn: name %q is already in use\n", name)
			return 125
		}
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
	if name != "" {
		_ = os.WriteFile(filepath.Join(dir, "name"), []byte(name), 0o600)
	}
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
	if mode == "stale" { // socket file exists, nobody accepts: `hisn ls` says "crashed"
		ln.(*net.UnixListener).SetUnlinkOnClose(false)
		_ = ln.Close()
	}
	if hasFlag(args, "--profile") {
		// hisn starts [[services]] before the session is ready.
		defer fakeServices(string(pol))()
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
	if mode == "detach041" || mode == "detach041badpid" {
		// 0.4.1: the run process forks a child "supervisor" that serves the
		// control socket, writes "ready\n" to --ready-fd, prints "<id> <name>-
		// ready" on stdout, writes supervisor.pid with the CHILD's pid, and
		// exits 0. The child keeps the socket alive until SIGTERM (or
		// `hisn stop`), then exits.
		supPid := fakeForkSupervisor(ln, dir, args)
		_ = os.WriteFile(filepath.Join(dir, "sup.pid"), []byte(strconv.Itoa(supPid)), 0o600)
		// Real 0.4.1 format: "<pid> <starttime>".
		pidLine := fmt.Sprintf("%d %s\n", supPid, procStart(supPid))
		if mode == "detach041badpid" { // a format Wakil cannot parse: tracking fails
			pidLine = "garbage\n"
		}
		_ = os.WriteFile(filepath.Join(dir, "supervisor.pid"), []byte(pidLine), 0o600)
		readyWrite(args)
		sname := name
		if sname == "" {
			sname = "-"
		}
		fmt.Printf("%s %s ready\n", id, sname)
		return 0
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
		if exists(filepath.Join(dir, "stopped")) {
			return 0 // `hisn stop`: the directory is kept, state stopped (0.4.0)
		}
		_ = os.RemoveAll(dir) // a TERM'd supervisor removes its own directory
		return 0
	}
	return 0
}

// fakeForkSupervisor forks a child process (this test binary re-exec'd) that
// becomes the 0.4.1 supervisor: it serves the control socket and handles
// SIGTERM. Returns the child's pid. The child inherits the listener fd; the
// parent must NOT close it before the child has taken it (the child gets it
// via the fork).
func fakeForkSupervisor(ln net.Listener, dir string, args []string) int {
	self, _ := os.Executable()
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(),
		"HISN_FAKE_STATE="+os.Getenv("HISN_FAKE_STATE"),
		"HISN_FAKE_SUPERVISOR="+dir,
	)
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.ExtraFiles = []*os.File{lnFile(ln)} // fd 3: the listener
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "hisn: fork supervisor: %v\n", err)
		return 0
	}
	return cmd.Process.Pid
}

// lnFile extracts the underlying *os.File from a net.Listener (unix socket).
func lnFile(ln net.Listener) *os.File {
	type fileLike interface {
		File() (*os.File, error)
	}
	if fl, ok := ln.(fileLike); ok {
		f, _ := fl.File()
		return f
	}
	return nil
}

// fakeSupervisorMain is the 0.4.1 child supervisor: it serves the control
// socket (passed as ExtraFiles fd 3) and handles SIGTERM. When `hisn stop`
// writes "stopped" to the session dir, it exits 0 (keeping the directory);
// otherwise it exits 0 on TERM too (removing the directory, like the 0.4.0
// supervisor).
func fakeSupervisorMain(dir, state string, args []string) int {
	f := os.NewFile(3, "control.sock")
	ln, err := net.FileListener(f)
	_ = f.Close()
	if err != nil {
		return 1
	}
	defer ln.Close()
	fakeCtlServe(ln, dir)
	sig := make(chan os.Signal, 4)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	<-sig
	killRecordedPids(dir)
	_ = ln.Close()
	if !exists(filepath.Join(dir, "stopped")) {
		_ = os.RemoveAll(dir)
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

// fakeResolve maps a session reference (a name or an id) to its directory id.
// An exact id wins; a name resolves to the session that holds it (a stopped
// directory no longer does, 0.4.0). "" = no such session.
func fakeResolve(state, ref string) string {
	if ref == "" {
		return ""
	}
	if exists(filepath.Join(sessRoot(state), ref)) {
		return ref
	}
	if id := fakeNameOwner(state, ref); id != "" {
		return id
	}
	return ""
}

// fakeNameOwner returns the id of the session directory that holds name ("" if
// none). Measured against hisn 0.4.0: running and CRASHED directories hold a
// name (a crashed one until `hisn ls --prune`); a directory ended with `hisn
// stop` releases it at once.
func fakeNameOwner(state, name string) string {
	ents, _ := os.ReadDir(sessRoot(state))
	for _, e := range ents {
		d := filepath.Join(sessRoot(state), e.Name())
		if fakeState(d) == "stopped" {
			continue
		}
		if n, _ := os.ReadFile(filepath.Join(d, "name")); strings.TrimSpace(string(n)) == name {
			return e.Name()
		}
	}
	return ""
}

func validFakeName(n string) bool {
	if n == "" || len(n) > 64 || n[0] == '.' {
		return false
	}
	for _, r := range n {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// fakeState is the hisn-ls/v2 state of a session directory. A directory with a
// supervisor pid file that nobody answers is "crashed", unless `hisn stop`
// marked it. A foreign fixture (no pid file) is running.
func fakeState(d string) string {
	if v, err := os.ReadFile(filepath.Join(d, "state")); err == nil { // test hook: any v2 state word
		return strings.TrimSpace(string(v))
	}
	if exists(filepath.Join(d, "stopped")) {
		return "stopped"
	}
	if exists(filepath.Join(d, "sup.pid")) && !dialable(d) {
		return "crashed"
	}
	return "running"
}

func fakeLs(state string, args []string) int {
	if raw, err := os.ReadFile(filepath.Join(state, "ls_raw")); err == nil && hasFlag(args, "--json") { // test hook: verbatim document
		fmt.Println(strings.TrimSpace(string(raw)))
		return 0
	}
	if ref := flagVal(args, "--session"); ref != "" {
		id := fakeResolve(state, ref)
		if id == "" || !dialable(filepath.Join(sessRoot(state), id)) {
			fmt.Fprintf(os.Stderr, "hisn: no such session %s\n", ref)
			return 125
		}
		fmt.Println(`{"ok":true,"grants":[]}`)
		return 0
	}
	if hasFlag(args, "--prune") { // remove dead dirs (stopped/crashed: socket refused AND supervisor gone)
		ents, _ := os.ReadDir(sessRoot(state))
		for _, e := range ents {
			d := filepath.Join(sessRoot(state), e.Name())
			if exists(filepath.Join(d, "sup.pid")) && !dialable(d) {
				fmt.Fprintf(os.Stderr, "hisn: %s stale; archived and removed\n", e.Name())
				_ = os.RemoveAll(d)
			}
		}
		return 0
	}
	ents, _ := os.ReadDir(sessRoot(state))
	sessions := []map[string]any{}
	for _, e := range ents {
		d := filepath.Join(sessRoot(state), e.Name())
		cwd := ""
		if b, err := os.ReadFile(filepath.Join(d, "cwd")); err == nil {
			cwd = string(b)
		}
		name := ""
		if b, err := os.ReadFile(filepath.Join(d, "name")); err == nil {
			name = strings.TrimSpace(string(b))
		}
		row := map[string]any{"id": e.Name(), "name": name, "state": fakeState(d), "cwd": cwd, "dir": d, "execs": []any{}}
		if exists(filepath.Join(state, "ls_v1")) { // pre-0.4.0 shape, for the v1 tests
			row = map[string]any{"id": e.Name(), "state": map[bool]string{true: "live", false: "stale"}[fakeState(d) == "running"], "cwd": cwd, "execs": []any{}}
		}
		sessions = append(sessions, row)
	}
	schema := "hisn-ls/v2"
	if exists(filepath.Join(state, "ls_v1")) {
		schema = "hisn-ls/v1"
	}
	if v, err := os.ReadFile(filepath.Join(state, "ls_schema")); err == nil { // test hook: any schema string
		schema = strings.TrimSpace(string(v))
	}
	b, _ := json.Marshal(map[string]any{"schema": schema, "sessions": sessions})
	fmt.Println(string(b))
	return 0
}

func fakeInspect(state string, args []string) int {
	if exists(filepath.Join(state, "inspect_fail")) {
		fmt.Fprintln(os.Stderr, "hisn: inspect failed")
		return 125
	}
	sid := fakeResolve(state, flagVal(args, "--session"))
	pol, err := os.ReadFile(filepath.Join(sessRoot(state), sid, "policy"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "hisn: no such session")
		return 125
	}
	var risks []fakeRisk
	if rj, err := os.ReadFile(filepath.Join(sessRoot(state), sid, "risk.json")); err == nil {
		_ = json.Unmarshal(rj, &risks)
	}
	b, _ := json.Marshal(map[string]any{"ok": true, "mountinfo": "", "policy": string(pol),
		"risk": nonNilRisks(overrideRisk(state, risks)), "services": []any{}})
	fmt.Println(string(b))
	return 0
}

// fakeStop is `hisn stop`: mark the directory stopped, end the supervisor, keep
// the directory and its name (0.4.0). A session whose supervisor is already
// dead is reported stopped too (measured).
func fakeStop(state string, args []string) int {
	id := fakeResolve(state, flagVal(args, "--session"))
	dir := filepath.Join(sessRoot(state), id)
	if id == "" || !exists(dir) {
		fmt.Fprintf(os.Stderr, "hisn: no such session %s\n", flagVal(args, "--session"))
		return 125
	}
	if exists(filepath.Join(state, "stop_fail")) {
		fmt.Fprintln(os.Stderr, "hisn: stop failed")
		return 125
	}
	_ = os.WriteFile(filepath.Join(dir, "stopped"), nil, 0o600)
	if b, err := os.ReadFile(filepath.Join(dir, "sup.pid")); err == nil {
		if pid, _ := strconv.Atoi(strings.TrimSpace(string(b))); pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	} else {
		killRecordedPids(dir)
	}
	fmt.Printf("session %s stopped\n", flagVal(args, "--session"))
	return 0
}

func fakeKill(state string, args []string) int {
	if id := fakeResolve(state, flagVal(args, "--session")); id != "" {
		args = append(append([]string(nil), args...), "--session", id)
	}
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
	id := fakeResolve(state, flagVal(args, "--session"))
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
	id := fakeResolve(state, flagVal(args, "--session"))
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
		case "--session", "--client":
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
	self := fakeBinary(t)
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

// servedIDs are the session directories that are not ended: everything except
// the "stopped" ones `hisn stop` leaves behind (0.4.0 keeps them, audited,
// until `hisn ls --prune`).
func (f *fakeEnv) servedIDs() []string {
	var ids []string
	for _, id := range f.sessionIDs() {
		if !exists(filepath.Join(f.rt(), id, "stopped")) {
			ids = append(ids, id)
		}
	}
	return ids
}

// stoppedIDs are the directories ended with `hisn stop`.
func (f *fakeEnv) stoppedIDs() []string {
	var ids []string
	for _, id := range f.sessionIDs() {
		if exists(filepath.Join(f.rt(), id, "stopped")) {
			ids = append(ids, id)
		}
	}
	return ids
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
