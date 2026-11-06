package browser

// Direct-mode launch safety (first host run of 0.4.4): a launch must never be
// able to reach a browser the user is running, must fail loudly when the
// process it started is not the browser it is talking to, and must never run
// from a test binary unless a test opts in.

import (
	"context"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// privateTmp points TMPDIR at a fresh per-test directory and returns it. A launch
// creates its profile dir with os.MkdirTemp("", ...), which follows TMPDIR, so the
// only profile dirs that can appear there are the ones this test's launch made; a
// stale /tmp/wakil-chrome-profile-* from an earlier run, or one a concurrent
// process creates, cannot be mistaken for them.
func privateTmp(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	return tmp
}

func profileDirsIn(t *testing.T, tmp string) []string {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(tmp, "wakil-chrome-profile-*"))
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

// Without WAKIL_BROWSER_LIVE=1 a test binary cannot start any browser.
func TestLocalLaunch_RefusedInTestBinaryWithoutOptIn(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "started")
	fake := filepath.Join(dir, "chromium")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho started >> "+log+"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(liveEnv, "")
	tmp := privateTmp(t)
	l := &localLauncher{browserPath: fake}
	_, _, _, err := l.launch(context.Background())
	if err == nil || !strings.Contains(err.Error(), liveEnv) {
		t.Fatalf("launch = %v, want a refusal naming %s", err, liveEnv)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(log); err == nil {
		t.Fatal("the browser binary was started")
	}
	if left := profileDirsIn(t, tmp); len(left) > 0 {
		t.Errorf("a profile dir was created before the refusal: %v", left)
	}
}

// A browser that hands off to an existing instance exits at once. The launch
// must fail with a clear error, leave no profile dir, and the arguments it
// passed must keep the browser private: a fresh --user-data-dir, headless=new,
// and a DevTools pipe (no TCP port).
func TestLocalLaunch_ImmediateExitIsAFailureNotASuccess(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("linux only")
	}
	dir := t.TempDir()
	argsLog := filepath.Join(dir, "args")
	fake := filepath.Join(dir, "fake-chromium")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsLog + "\nexit 0\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(liveEnv, "1")
	tmp := privateTmp(t)
	l := &localLauncher{browserPath: fake}
	start := time.Now()
	_, _, sess, err := l.launch(context.Background())
	if err == nil {
		_ = sess.close()
		t.Fatal("a browser that exited immediately was reported as a successful launch")
	}
	if el := time.Since(start); el > 14*time.Second {
		t.Errorf("failure took %v; an immediate exit must not wait for the launch timeout", el)
	}
	if !strings.Contains(err.Error(), "Chromium") {
		t.Errorf("error does not say what failed: %v", err)
	}
	b, rerr := os.ReadFile(argsLog)
	if rerr != nil {
		t.Fatalf("the fake browser never ran: %v", rerr)
	}
	args := string(b)
	for _, want := range []string{"--headless=new", "--remote-debugging-pipe", "--no-first-run"} {
		if !strings.Contains(args, want) {
			t.Errorf("launch args lack %s:\n%s", want, args)
		}
	}
	if !strings.Contains(args, "--user-data-dir="+tmp+"/wakil-chrome-profile-") {
		t.Errorf("launch did not pass a fresh private --user-data-dir:\n%s", args)
	}
	if strings.Contains(args, "--remote-debugging-port") {
		t.Errorf("a TCP debugging port was requested:\n%s", args)
	}
	if left := profileDirsIn(t, tmp); len(left) > 0 {
		t.Errorf("a failed launch left its profile dir behind: %v", left)
	}
}

// A distro wrapper script in PATH is replaced by the real binary when one is
// known; a real (non-script) binary is used as it is; browser_path is never
// rewritten.
func TestResolveBrowser(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "chromium")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec /nonexistent \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if !isScript(script) {
		t.Fatal("isScript missed a #! file")
	}
	if got := resolveBrowser("/some/explicit/path"); got != "/some/explicit/path" {
		t.Errorf("browser_path was rewritten to %q", got)
	}
	elf, err := osexec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep binary")
	}
	if isScript(elf) {
		t.Skip("sleep is a script here")
	}
	// A non-script chromium on PATH is returned (symlinks resolved).
	bin := t.TempDir()
	if err := os.Symlink(elf, filepath.Join(bin, "chromium")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	got := resolveBrowser("")
	if want, _ := filepath.EvalSymlinks(elf); got != want {
		t.Errorf("resolveBrowser = %q, want the binary %q", got, want)
	}
}
