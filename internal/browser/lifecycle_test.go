package browser

// B1–B3 (0.4.4): lazy start, idle stop, stop on /new + Close, per-launch
// profile dirs, loopback-only tunnel. Unit tests run against a fake launcher;
// the tunnel and the stop script run for real (host sockets / host shell).

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLauncher records launches and hands out ready sessions without Chromium.
type fakeLauncher struct {
	mu        sync.Mutex
	launches  int
	teardowns int
	failNext  error
	dirs      []string
}

func (f *fakeLauncher) launch(context.Context) (string, func() error, *session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNext != nil {
		err := f.failNext
		f.failNext = nil
		return "", nil, nil, err
	}
	f.launches++
	dir, _ := profileDirName()
	f.dirs = append(f.dirs, dir)
	ctx, cancel := context.WithCancel(context.Background())
	return "", nil, &session{
		ctx: ctx, ctxCancel: cancel,
		teardown: func() error { f.mu.Lock(); f.teardowns++; f.mu.Unlock(); return nil },
	}, nil
}

func (f *fakeLauncher) counts() (launches, teardowns int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.launches, f.teardowns
}

func newFakeManager(idle time.Duration) (*Manager, *fakeLauncher) {
	fl := &fakeLauncher{}
	m := &Manager{launcher: fl, idleTimeout: idle}
	return m, fl
}

func TestLazyStart_NothingLaunchedUntilFirstOperation(t *testing.T) {
	m, fl := newFakeManager(time.Minute)
	if n, _ := fl.counts(); n != 0 || m.Running() {
		t.Fatalf("constructing a manager must launch nothing (launches=%d running=%v)", n, m.Running())
	}
	_, end, err := m.beginOp()
	if err != nil {
		t.Fatal(err)
	}
	end()
	if n, _ := fl.counts(); n != 1 || !m.Running() {
		t.Fatalf("first operation must launch exactly one browser (launches=%d running=%v)", n, m.Running())
	}
	_, end, _ = m.beginOp()
	end()
	if n, _ := fl.counts(); n != 1 {
		t.Errorf("second operation must reuse the browser, launches=%d", n)
	}
}

func TestIdleStop_StopsAfterTimeoutAndRestartsOnNextCall(t *testing.T) {
	m, fl := newFakeManager(60 * time.Millisecond)
	_, end, _ := m.beginOp()
	end()
	deadline := time.Now().Add(3 * time.Second)
	for m.Running() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if m.Running() {
		t.Fatal("browser still running long after the idle timeout")
	}
	if _, td := fl.counts(); td != 1 {
		t.Errorf("teardowns = %d, want 1", td)
	}
	_, end, err := m.beginOp()
	if err != nil {
		t.Fatal(err)
	}
	end()
	if n, _ := fl.counts(); n != 2 {
		t.Errorf("a call after an idle stop must launch a fresh browser, launches=%d", n)
	}
	if fl.dirs[0] == fl.dirs[1] {
		t.Errorf("each launch needs its own profile dir, both %q", fl.dirs[0])
	}
}

// The idle timer must never stop the browser under a running operation.
func TestIdleStop_NeverFiresWhileOperationInFlight(t *testing.T) {
	m, fl := newFakeManager(40 * time.Millisecond)
	_, end, _ := m.beginOp()
	time.Sleep(250 * time.Millisecond) // several idle periods, op still running
	if _, td := fl.counts(); td != 0 || !m.Running() {
		t.Fatalf("browser stopped under a running operation (teardowns=%d running=%v)", td, m.Running())
	}
	end()
	deadline := time.Now().Add(3 * time.Second)
	for m.Running() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if m.Running() {
		t.Error("browser must stop once the operation ended and the idle period passed")
	}
}

func TestStop_OnNewAndClose(t *testing.T) {
	m, fl := newFakeManager(time.Hour)
	_, end, _ := m.beginOp()
	end()
	if err := m.Stop(); err != nil { // /new
		t.Fatal(err)
	}
	if m.Running() {
		t.Fatal("Stop must stop the browser")
	}
	if _, td := fl.counts(); td != 1 {
		t.Errorf("teardowns after Stop = %d", td)
	}
	// Still usable after Stop (a new chat): next op launches again.
	_, end, err := m.beginOp()
	if err != nil {
		t.Fatalf("manager must stay usable after Stop: %v", err)
	}
	end()
	if err := m.Close(); err != nil { // chat end / Wakil exit
		t.Fatal(err)
	}
	if m.Running() {
		t.Fatal("Close must stop the browser")
	}
	if _, _, err := m.beginOp(); err == nil {
		t.Error("a closed manager must refuse operations")
	}
	if err := m.Close(); err != nil {
		t.Errorf("Close must be idempotent: %v", err)
	}
	var nilMgr *Manager
	if nilMgr.Stop() != nil || nilMgr.Close() != nil {
		t.Error("nil manager Stop/Close must be no-ops")
	}
}

func TestLaunchFailureLeavesNothingRunning(t *testing.T) {
	m, fl := newFakeManager(time.Hour)
	fl.failNext = errors.New("boom")
	if _, _, err := m.beginOp(); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("launch error must surface, got %v", err)
	}
	if m.Running() {
		t.Error("a failed launch must not leave a session")
	}
	if _, end, err := m.beginOp(); err != nil {
		t.Errorf("manager must recover after a failed launch: %v", err)
	} else {
		end()
	}
}

func TestProfileDirsDiffer(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		d, suffix := profileDirName()
		if !strings.HasPrefix(d, "/tmp/wakil-chrome-") || !strings.HasSuffix(d, suffix) || len(suffix) != 16 {
			t.Fatalf("unexpected profile dir %q / %q", d, suffix)
		}
		if seen[d] {
			t.Fatalf("duplicate profile dir %q", d)
		}
		seen[d] = true
	}
}

func TestParseDevToolsActivePort(t *testing.T) {
	cases := map[string]struct {
		port int
		ok   bool
	}{
		"37411\n/devtools/browser/abc\n": {37411, true},
		"37411\n/devtools/browser/abc":   {37411, true},
		"37411\n":                        {0, false}, // partial write: second line missing
		"37411":                          {0, false},
		"":                               {0, false},
		"abc\n/devtools/browser/x\n":     {0, false},
		"0\n/x\n":                        {0, false},
		"70000\n/x\n":                    {0, false},
	}
	for in, want := range cases {
		p, ok := parseDevToolsActivePort(in)
		if p != want.port || ok != want.ok {
			t.Errorf("parse(%q) = %d,%v want %d,%v", in, p, ok, want.port, want.ok)
		}
	}
}

// ── tunnel: loopback-only bind + byte transparency ────────────────────────────

// localForwarder is a stand-in for `docker exec -i … python3`: it runs the real
// forwarder script with the host python3 (skipped when python3 is absent).
type localForwarder struct{}

func (localForwarder) StartInteractive(_ context.Context, command string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, int, error) {
	cmd := osexec.Command("sh", "-c", command)
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	er, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, 0, err
	}
	go func() { _ = cmd.Wait() }()
	return in, out, er, cmd.Process.Pid, nil
}

func TestTunnel_LoopbackOnlyAndTransparent(t *testing.T) {
	if _, err := osexec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	// An echo server standing in for Chromium's CDP port.
	srv, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	var serverGot atomic.Value
	go func() {
		c, err := srv.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 1<<20)
		n, _ := io.ReadFull(c, buf[:70000])
		serverGot.Store(append([]byte(nil), buf[:n]...))
		_, _ = c.Write(buf[:n]) // echo
	}()
	port := srv.Addr().(*net.TCPAddr).Port

	tun, url, err := startTunnel(localForwarder{}, port)
	if err != nil {
		t.Fatal(err)
	}
	defer tun.close()

	// B3: the listener is on 127.0.0.1 and nothing else.
	ta := tun.addr().(*net.TCPAddr)
	if !ta.IP.IsLoopback() || ta.IP.String() != "127.0.0.1" {
		t.Fatalf("tunnel listens on %v, want 127.0.0.1", ta)
	}
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Errorf("tunnel URL %q must be loopback", url)
	}

	// Binary-transparent, > one pipe buffer, both directions.
	payload := make([]byte, 70000)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	c, err := net.DialTimeout("tcp4", ta.String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(15 * time.Second))
	if _, err := c.Write(payload); err != nil {
		t.Fatal(err)
	}
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(c, echo); err != nil {
		t.Fatalf("reading echo: %v", err)
	}
	if string(echo) != string(payload) {
		t.Error("echoed bytes differ from the payload (tunnel is not transparent)")
	}
	if got, _ := serverGot.Load().([]byte); string(got) != string(payload) {
		t.Error("server received different bytes than were sent")
	}
}

// close() must end live connections and not hang.
func TestTunnel_CloseEndsConnections(t *testing.T) {
	if _, err := osexec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	srv, _ := net.Listen("tcp4", "127.0.0.1:0")
	defer srv.Close()
	go func() {
		c, err := srv.Accept()
		if err == nil {
			defer c.Close()
			_, _ = io.Copy(io.Discard, c)
		}
	}()
	tun, _, err := startTunnel(localForwarder{}, srv.Addr().(*net.TCPAddr).Port)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp4", tun.addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(200 * time.Millisecond)
	done := make(chan struct{})
	go func() { tun.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("tunnel.close hung")
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Error("client connection must end after tunnel.close")
	}
	if c2, err := net.DialTimeout("tcp4", tun.addr().String(), time.Second); err == nil {
		c2.Close()
		t.Error("listener must be closed")
	}
}

// ── stop script: group kill, straggler sweep, dir removal, no self-match ──────

func TestStopScript_KillsGroupSweepsStragglersRemovesDir(t *testing.T) {
	if _, err := osexec.LookPath("setsid"); err != nil {
		t.Skip("setsid not installed")
	}
	base := t.TempDir()
	// The script hardcodes /tmp/wakil-chrome-<suffix>; use a real suffix there.
	dir, suffix := profileDirName()
	if err := os.MkdirAll(filepath.Join(dir, "home"), 0o755); err != nil {
		t.Skipf("cannot create %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	_ = base

	// A "browser" in its own session: a leader shell (argv carries the dir)
	// with two sleeping children in the group, plus a straggler in ANOTHER
	// group that only carries the dir in its argv (like crashpad's handler).
	// `: "$0"` after the sleep keeps each sh alive as the argv-carrying parent.
	start := `set -e; cd ` + quoteSh(dir) + `
setsid sh -c 'echo $$ > pid; s=$(cat /proc/$$/stat); rest=${s##*) }; set -- $rest; echo $3 > pgid; sleep 300 & sleep 300; : "$0"' ` + quoteSh(dir+"/leader") + ` >/dev/null 2>&1 &
setsid sh -c 'sleep 300; : "$0"' ` + quoteSh(dir+"/home") + ` >/dev/null 2>&1 &
sleep 0.6`
	if out, err := osexec.Command("sh", "-c", start).CombinedOutput(); err != nil {
		t.Fatalf("start: %v: %s", err, out)
	}
	if n := countProcsWith(t, suffix); n < 2 {
		t.Fatalf("expected the leader and the straggler to carry the dir, found %d", n)
	}
	pgidRaw, err := os.ReadFile(filepath.Join(dir, "pgid"))
	if err != nil {
		t.Fatalf("pgid file: %v", err)
	}
	pgid := strings.TrimSpace(string(pgidRaw))
	if osexec.Command("sh", "-c", "kill -0 -"+pgid).Run() != nil {
		t.Fatalf("group %s should be alive before the stop", pgid)
	}

	script := stopScript(500 * time.Millisecond)
	out, err := osexec.Command("sh", "-c", script, "sh", suffix).CombinedOutput()
	if err != nil {
		t.Fatalf("stop script: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "clean" {
		t.Fatalf("stop script reported %q, want clean", got)
	}
	if n := countProcsWith(t, suffix); n != 0 {
		t.Errorf("%d processes still carry the profile dir after stop", n)
	}
	if osexec.Command("sh", "-c", "kill -0 -"+pgid).Run() == nil {
		t.Errorf("process group %s (leader + children) still alive after stop", pgid)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("profile dir not removed")
	}
}

// The scanner must not find itself: with nothing running it reports clean.
func TestStopScript_DoesNotMatchItself(t *testing.T) {
	_, suffix := profileDirName()
	out, err := osexec.Command("sh", "-c", stopScript(200*time.Millisecond), "sh", suffix).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "clean" {
		t.Fatalf("scan matched itself or something else: %q", got)
	}
}

func countProcsWith(t *testing.T, suffix string) int {
	t.Helper()
	out, _ := osexec.Command("sh", "-c", `grep -l -a -- "wakil-chrom[e]-`+suffix+`" /proc/[0-9]*/cmdline 2>/dev/null | wc -l`).Output()
	n := 0
	for _, c := range strings.TrimSpace(string(out)) {
		n = n*10 + int(c-'0')
	}
	return n
}

func TestStartScriptShape(t *testing.T) {
	s := startScript("/tmp/wakil-chrome-abc", "chromium")
	for _, want := range []string{
		"exec 'chromium'", "--remote-debugging-port=0", "--disable-crash-reporter", "--disable-breakpad",
		"--user-data-dir='/tmp/wakil-chrome-abc'", "HOME='/tmp/wakil-chrome-abc/home'",
		"/tmp/wakil-chrome-abc/pid",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("start script missing %q:\n%s", want, s)
		}
	}
	for _, bad := range []string{"0.0.0.0", "9222", "9223", "wakil-chrome-profile", "&\n", "nohup"} {
		if strings.Contains(s, bad) {
			t.Errorf("start script must not contain %q:\n%s", bad, s)
		}
	}
}
