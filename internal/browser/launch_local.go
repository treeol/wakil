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
	userDataDir, err := os.MkdirTemp("", "wakil-chrome-profile-")
	if err != nil {
		return "", nil, nil, fmt.Errorf("create user-data-dir: %w", err)
	}
	// teardown (R3) is set below, once the allocator exists: it cancels the
	// allocator, waits (bounded) for Chromium to exit, KILLs whatever still
	// carries this launch's --user-data-dir, and only then removes the dir.
	var teardown func() error

	allocOpts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.Flag("headless", "new"),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-crash-reporter", true),
		chromedp.Flag("disable-breakpad", true),
		chromedp.UserDataDir(userDataDir),
		chromedp.Env("HOME="+userDataDir),
	)
	if l.browserPath != "" {
		allocOpts = append(allocOpts, chromedp.ExecPath(l.browserPath))
	}
	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), allocOpts...)
	ctx, ctxCancel := chromedp.NewContext(allocCtx)
	teardown = func() error {
		ctxCancel()
		allocCancel()
		waitGoneBounded(userDataDir, localExitWait)
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
