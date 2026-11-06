package browser

// Tests for the in-session launcher (hisn, 0.5.0 P4): the guard decision
// table, the chromium probe, and launch/stop/ownership against a fake session
// whose "exec" is a local sh, so the real start, stop and sweep scripts run.

import (
	"context"
	"errors"
	"io"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeSession stands for a sandbox session: RunShell and the CLI starter run
// locally. id is the ownership marker; running gates the session (ID "" =
// not up). chromiumOK gates the probe.
type fakeSession struct {
	id         string
	running    bool
	cli        bool // whether the backend exposes a CLI transport
	chromiumOK bool
	cliCalls   int
}

func (f *fakeSession) RunShell(_ context.Context, command string) (string, error) {
	if !f.running {
		return "", errors.New("no session")
	}
	if strings.HasPrefix(command, "command -v ") && !f.chromiumOK {
		return "", errors.New("not found")
	}
	out, err := osexec.Command("sh", "-c", command).CombinedOutput()
	return string(out), err
}

func (f *fakeSession) ContainerName() string { return "" }

func (f *fakeSession) BrowserSessionID() string {
	if !f.running {
		return ""
	}
	return f.id
}

func (f *fakeSession) StartInteractiveCLI(_ context.Context, command string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, int, error) {
	f.cliCalls++
	cmd := osexec.Command("sh", "-c", command)
	// A real session exec makes its command the leader of its own process
	// group (the start script relies on this). Mirror that here, so the stop
	// script's group signal reaches only the fake browser, never the test.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	er, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, 0, err
	}
	go func() { _ = cmd.Wait() }()
	return in, out, er, cmd.Process.Pid, nil
}

// fakeSessionCLI is fakeSession with the CLI transport present, as hisn has.
type fakeSessionCLI struct{ *fakeSession }

func (fakeSessionCLI) HostBrowserAllowed() bool { return true }

// fakeNoCLI is a backend with no CLI transport (the guard must refuse it).
type fakeNoCLI struct{ *fakeSession }

func (fakeNoCLI) HostBrowserAllowed() bool { return false }

// TestSessionGuardDecisionTable covers the browser guard across backends:
// docker and direct keep their behaviour; hisn is allowed only with a CLI
// transport; a backend without one is refused.
func TestSessionGuardDecisionTable(t *testing.T) {
	cases := []struct {
		name    string
		exe     SandboxExecutor
		wantErr bool
	}{
		{"hisn with CLI transport", fakeSessionCLI{&fakeSession{running: true, id: "s1", cli: true, chromiumOK: true}}, false},
		{"hisn with CLI transport, no session yet", fakeSessionCLI{&fakeSession{id: "", cli: true, chromiumOK: true}}, false},
		{"backend without CLI transport", fakeNoCLI{&fakeSession{running: true, id: "s1"}}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := NewManager(c.exe, "")
			if (err != nil) != c.wantErr {
				t.Fatalf("NewManager err = %v, wantErr %v", err, c.wantErr)
			}
			if err == nil && m == nil {
				t.Fatal("nil manager without error")
			}
		})
	}
}

// TestSessionProbeRefusesWithoutChromium: the probe runs at launch; a session
// without chromium fails the launch with a clear message, not a broken browser.
func TestSessionProbeRefusesWithoutChromium(t *testing.T) {
	f := &fakeSession{running: true, id: "s1", chromiumOK: false}
	l, err := newSessionLauncher(fakeSessionCLI{f}, fakeSessionCLI{f}, "")
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, err = l.launch(context.Background())
	if err == nil || !strings.Contains(err.Error(), "not found in the sandbox session") {
		t.Fatalf("want a chromium-not-found error, got %v", err)
	}
	if f.cliCalls != 0 {
		t.Fatalf("no browser process may start without chromium (CLI calls = %d)", f.cliCalls)
	}
}

// TestSessionLauncherRefusesWithoutSession: a launch with no running session
// has no ownership marker, so it must not start anything.
func TestSessionLauncherRefusesWithoutSession(t *testing.T) {
	f := &fakeSession{running: false, id: "", chromiumOK: true}
	l, err := newSessionLauncher(fakeSessionCLI{f}, fakeSessionCLI{f}, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := l.launch(context.Background()); err == nil {
		t.Fatal("launch without a running session must fail")
	}
	if f.cliCalls != 0 {
		t.Fatalf("nothing may start without a session (CLI calls = %d)", f.cliCalls)
	}
}

// fakeChromiumMode is the helper mode that stands in for chromium: it takes
// --user-data-dir=<dir> from its own argv, writes DevToolsActivePort there, and
// sleeps until its parent goes away (or a deadline passes).
const fakeChromiumMode = "fake-chromium"

// TestFakeChromium is not a test: when the helper env selects fakeChromiumMode
// it runs as the stand-in browser. Its argv is the test binary's argv, so the
// profile dir is in it exactly as a real chromium's would be.
func TestFakeChromium(t *testing.T) {
	if os.Getenv(helperEnv) != fakeChromiumMode {
		t.Skip("helper process, not a test")
	}
	defer os.Exit(0)
	var dir string
	for _, a := range os.Args {
		if strings.HasPrefix(a, "--user-data-dir=") {
			dir = strings.TrimPrefix(a, "--user-data-dir=")
		}
	}
	if dir == "" {
		os.Exit(2)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(filepath.Join(dir, "DevToolsActivePort"), []byte("9222\n/devtools/browser/x\n"), 0o600); err != nil {
		os.Exit(4)
	}
	parent := os.Getppid()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) && os.Getppid() == parent {
		time.Sleep(50 * time.Millisecond)
	}
	os.Exit(0)
}

// fakeChromiumScript returns a wrapper that execs the fake chromium helper with
// the given argv, so the sweep marker (the profile dir) stays in the process
// argv. The wrapper is a shell script so the launch script's `exec <path>`
// works as it does for a real browser binary.
func fakeChromiumScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "chromium")
	// The helper env is set here, on the child only, never in the test process.
	script := "#!/bin/sh\n" + helperEnv + "=" + fakeChromiumMode + " exec '" + os.Args[0] +
		"' -test.run='^TestFakeChromium$' -- \"$@\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestSessionStartStopSweep runs the real start and stop scripts through the
// fake session: the launcher records pid/pgid, the stop script TERMs the group,
// sweeps by the per-launch profile dir and leaves no straggler. A hard deadline
// fails the test instead of letting a hang block the run.
func TestSessionStartStopSweep(t *testing.T) {
	if _, err := osexec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}
	fake := fakeChromiumScript(t)
	f := &fakeSession{running: true, id: "sess-abc", chromiumOK: true}
	l, err := newSessionLauncher(fakeSessionCLI{f}, fakeSessionCLI{f}, fake)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	done := make(chan error, 1)
	var url string
	var teardown func() error
	go func() {
		var lerr error
		url, teardown, _, lerr = l.launch(ctx)
		done <- lerr
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("launch: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("launch did not complete before the deadline (fixture or launch hang)")
	}
	if !strings.HasPrefix(url, "http://127.0.0.1:") {
		t.Fatalf("tunnel url %q is not loopback", url)
	}
	if got := l.(*dockerLauncher).markerOf(); got != "sess-abc" {
		t.Fatalf("ownership marker = %q, want the session id", got)
	}
	matches, _ := filepathGlob("/tmp/wakil-chrome-*/DevToolsActivePort")
	var mine []string
	for _, m := range matches {
		if fileContains(m, "/devtools/browser/x") {
			mine = append(mine, m)
		}
	}
	if len(mine) == 0 {
		t.Fatal("no profile dir found for this launch (the start script did not run?)")
	}
	if err := teardown(); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	for _, m := range mine {
		if _, err := os.Stat(m); !os.IsNotExist(err) {
			t.Fatalf("profile file %s survived teardown", m)
		}
	}
}

// TestHostProcessOwnershipNeedsMarker: the host-side exec client is signalled
// only when its cmdline carries the session id (the marker). A process without
// it is never killed.
func TestHostProcessOwnershipNeedsMarker(t *testing.T) {
	pid := startHelper(t, "", "wakil-sess-OTHER")
	h, ok := recordHostProcess(pid, "sess-abc")
	if !ok {
		t.Fatal("record failed")
	}
	if killOwned(h, func(string, ...any) {}) {
		t.Fatal("a process without the session id in argv must not be signalled")
	}
	if !processAlive(pid) {
		t.Fatal("process without the marker was killed")
	}
}

// processAlive reports whether pid exists (signal 0).
func processAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func filepathGlob(p string) ([]string, error) { return filepath.Glob(p) }

func fileContains(path, s string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(b), s)
}
