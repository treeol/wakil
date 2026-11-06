package browser

// Local (direct-mode) launcher: Chromium as a child of the Wakil process on the
// host via chromedp's ExecAllocator, now lazy and with the same idle/stop
// lifecycle as the docker launcher. Not used under a sandbox backend (hisn).

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/treeol/wakil/internal/safe"
)

type localLauncher struct {
	browserPath string
}

// newLocalLauncher checks that a browser binary can be found; it launches
// nothing.
func newLocalLauncher(browserPath string) (launcher, error) {
	if browserPath != "" {
		if _, err := os.Stat(browserPath); err != nil {
			return nil, fmt.Errorf("browser_path %q: %w — set browser_path to a Chrome/Chromium binary, or set browser_enabled:false", browserPath, err)
		}
		return &localLauncher{browserPath: browserPath}, nil
	}
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser", "chrome", "google-chrome-stable", "headless-shell"} {
		if _, err := exec.LookPath(name); err == nil {
			return &localLauncher{}, nil
		}
	}
	return nil, fmt.Errorf("no Chrome/Chromium binary found on PATH — set browser_path to a Chrome/Chromium binary, or set browser_enabled:false")
}

// liveEnv opts a test binary in to launching a real browser (headless, on a
// private profile). Without it a test binary never starts Chromium on the host:
// the first host run of 0.4.4 went wrong around a developer's running desktop
// browser, and no test may be able to reach it.
const liveEnv = "WAKIL_BROWSER_LIVE"

// errLaunchInTest is returned by launch in a test binary without liveEnv=1.
var errLaunchInTest = fmt.Errorf("refusing to start a real browser from a test binary (set %s=1 to opt in; it runs headless on a private profile)", liveEnv)

// realBinaryCandidates are where distributions install the real Chromium
// binary that their launcher script in PATH execs. The script reads
// distro-wide flags and may add its own (a display, a profile), so direct mode
// starts the binary itself.
var realBinaryCandidates = []string{
	"/usr/lib/chromium/chromium",
	"/usr/lib64/chromium-browser/chromium-browser",
	"/usr/lib/chromium-browser/chromium-browser",
	"/usr/lib64/chromium/chromium",
	"/opt/google/chrome/chrome",
	"/usr/lib/chromium/chrome",
}

// isScript reports whether path starts with a "#!" interpreter line.
func isScript(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var b [2]byte
	n, _ := f.Read(b[:])
	return n == 2 && b[0] == '#' && b[1] == '!'
}

// resolveBrowser returns the executable to start: the configured browser_path
// as given, otherwise the first PATH match — replaced by the real binary when
// the match is a distro wrapper script and a known real binary exists. When no
// real binary is found the wrapper is used (the post-launch checks still hold).
// An empty result lets chromedp search PATH itself.
func resolveBrowser(browserPath string) string {
	if browserPath != "" {
		return browserPath
	}
	for _, name := range []string{"google-chrome", "chromium", "chromium-browser", "chrome", "google-chrome-stable", "headless-shell"} {
		p, err := exec.LookPath(name)
		if err != nil {
			continue
		}
		if real, err := filepath.EvalSymlinks(p); err == nil {
			p = real
		}
		if !isScript(p) {
			return p
		}
		for _, c := range realBinaryCandidates {
			if st, err := os.Stat(c); err == nil && st.Mode().IsRegular() && !isScript(c) {
				return c
			}
		}
		return p
	}
	return ""
}

// launch starts the browser through chromedp's ExecAllocator and returns the
// ready session.
//
// The first chromedp.Run must use the manager-owned context DIRECTLY, with no
// derived timeout/cancel wrapping it: chromedp ties the browser process's
// lifetime to whichever context is used for the FIRST Run, so canceling a
// derived context later would kill the browser. The 15 s limit is enforced by
// racing a timer in a goroutine instead. On timeout or error the whole
// allocator is torn down — a deliberate abort, not a mid-session cancel.
//
// A writable user-data-dir + HOME are required on a --read-only rootfs
// (crashpad needs a writable database dir); both point at a private temp dir.
func (l *localLauncher) launch(_ context.Context) (string, func() error, *session, error) {
	if testing.Testing() && os.Getenv(liveEnv) != "1" {
		return "", nil, nil, errLaunchInTest
	}
	userDataDir, err := os.MkdirTemp("", "wakil-chrome-profile-")
	if err != nil {
		return "", nil, nil, fmt.Errorf("create user-data-dir: %w", err)
	}
	// teardown (R3) is set below, once the allocator exists: it cancels the
	// allocator, waits (bounded) for Chromium to exit, KILLs whatever still
	// carries this launch's --user-data-dir, and only then removes the dir.
	var teardown func() error
	var owned hostProcess // the browser we started, identified at spawn (pidfd)
	var haveOwned bool

	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", "new"),
		// The DevTools channel is a private pipe (chromedp's default): no TCP
		// port exists at all, so nothing else can attach to this browser, and
		// it can never be mistaken for the user's own debugging port.
		chromedp.Flag("remote-debugging-pipe", true),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("no-default-browser-check", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-crash-reporter", true),
		chromedp.Flag("disable-breakpad", true),
		chromedp.UserDataDir(userDataDir),
		chromedp.Env("HOME="+userDataDir),
	)
	if exe := resolveBrowser(l.browserPath); exe != "" {
		allocOpts = append(allocOpts, chromedp.ExecPath(exe))
	}
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), allocOpts...)
	ctx, ctxCancel := chromedp.NewContext(allocCtx)
	teardown = func() error {
		ctxCancel()
		allocCancel()
		waitGoneBounded(userDataDir, localExitWait)
		if haveOwned {
			killOwned(owned, diagf) // through the pidfd: never a reused pid
		}
		killUsingDir(userDataDir)
		return os.RemoveAll(userDataDir)
	}
	sess := &session{ctx: ctx, ctxCancel: ctxCancel, allocCancel: allocCancel, teardown: teardown}

	triedMsg := "PATH search (google-chrome, chromium, etc.)"
	if l.browserPath != "" {
		triedMsg = fmt.Sprintf("browser_path=%q", l.browserPath)
	}
	done := make(chan error, 1)
	safe.Go("browser-launch", func() { _, err := chromedp.Run(ctx, chromedp.Navigate("about:blank")); done <- err })
	select {
	case err := <-done:
		if err != nil {
			_ = sess.close()
			return "", nil, nil, fmt.Errorf("cannot start Chromium, tried %s (%w) — set browser_path to a Chrome/Chromium binary, or set browser_enabled:false", triedMsg, err)
		}
	case <-time.After(15 * time.Second):
		_ = sess.close()
		return "", nil, nil, fmt.Errorf("timed out launching Chromium after 15s, tried %s — set browser_path to a Chrome/Chromium binary, or set browser_enabled:false", triedMsg)
	}
	// The browser must be the process we started, alive, on our profile. A
	// chromium that hands its URL to an already-running instance exits at once;
	// the connection above would then have failed, but this closes the gap
	// explicitly instead of reporting a launch that is not ours.
	proc := chromedp.FromContext(ctx).Browser.Process()
	if proc == nil {
		_ = sess.close()
		return "", nil, nil, fmt.Errorf("cannot start Chromium, tried %s: no browser process was started", triedMsg)
	}
	rec, ok := recordHostProcess(proc.Pid, "--user-data-dir="+userDataDir)
	if !ok {
		_ = sess.close()
		return "", nil, nil, fmt.Errorf("Chromium (pid %d) exited immediately, tried %s — it probably handed off to an already-running browser; nothing was opened there. Set browser_path to a Chrome/Chromium binary, or set browser_enabled:false", proc.Pid, triedMsg)
	}
	if okOwn, why := rec.owns(); !okOwn {
		rec.release()
		_ = sess.close()
		return "", nil, nil, fmt.Errorf("Chromium pid %d is not the browser started for this launch (%s), tried %s", proc.Pid, why, triedMsg)
	}
	owned, haveOwned = rec, true
	return "", nil, sess, nil
}

// localExitWait bounds how long direct-mode teardown waits for Chromium to exit
// on its own before it is KILLed (R3).
const localExitWait = 3 * time.Second

// waitGoneBounded polls until no process carries --user-data-dir=dir, or the
// bound expires. It never kills; the caller decides what to do afterwards.
func waitGoneBounded(dir string, bound time.Duration) {
	deadline := time.Now().Add(bound)
	for time.Now().Before(deadline) {
		if countUsingDir(dir) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// countUsingDir counts processes whose argv carries --user-data-dir=dir,
// excluding this process.
func countUsingDir(dir string) int {
	return scanUsingDir(dir, false)
}

// killUsingDir SIGKILLs every process whose argv carries --user-data-dir=dir
// (the wakil process itself is excluded) and returns how many it signalled.
func killUsingDir(dir string) int {
	return scanUsingDir(dir, true)
}

// scanUsingDir walks /proc for processes whose argv contains --user-data-dir=dir.
// With kill, each match gets SIGKILL. The chromedp allocator does the same on
// Linux; this makes the rule explicit and covers platforms where it does not.
func scanUsingDir(dir string, kill bool) int {
	want := "--user-data-dir=" + dir
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	self := os.Getpid()
	n := 0
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		for _, arg := range strings.Split(string(b), "\x00") {
			if arg == want {
				n++
				if kill {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
				break
			}
		}
	}
	return n
}
