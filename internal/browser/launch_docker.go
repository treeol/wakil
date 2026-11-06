package browser

// Docker launcher (0.4.4, B1/B2): Chromium inside the sandbox container, started
// on first use as a direct child of a `docker exec` session, stopped by killing
// its process group (TERM, 3 s, KILL), sweeping stragglers and removing the
// per-launch profile directory.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/treeol/wakil/internal/diag"
	"github.com/treeol/wakil/internal/safe"
)

// termGrace is how long a TERMed browser group gets before KILL.
const termGrace = 3 * time.Second

// dockerSandbox is the executor surface the docker launcher uses.
type dockerSandbox interface {
	SandboxExecutor
	interactiveStarter
}

type dockerLauncher struct {
	exe      dockerSandbox
	chromium string
	// sess is set for a sandbox session (hisn): the transport is the session's
	// CLI starter and the ownership marker is the session id. Nil for docker,
	// where the marker is the container name.
	sess sessionExecutor
}

// newSessionLauncher builds the in-session launcher for a sandbox backend with
// a CLI transport (hisn). The session may not be running yet, so the chromium
// probe runs at launch (sessionProbe), and the marker is taken per launch.
func newSessionLauncher(exe SandboxExecutor, se sessionExecutor, browserPath string) (launcher, error) {
	bin := browserPath
	if bin == "" {
		bin = "chromium"
	}
	return &dockerLauncher{exe: sessionShell{exe, se}, chromium: bin, sess: se}, nil
}

// sessionShell adapts a session executor to dockerSandbox: RunShell and the
// CLI starter go through the session; ContainerName is unused (the marker
// comes from the session id, see markerOf).
type sessionShell struct {
	SandboxExecutor
	se sessionExecutor
}

func (s sessionShell) StartInteractive(ctx context.Context, command string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, int, error) {
	return s.se.StartInteractiveCLI(ctx, command)
}

// markerOf is the ownership marker for host-side processes of this launcher:
// the container name for docker, the running session id for a session.
func (l *dockerLauncher) markerOf() string {
	if l.sess != nil {
		return l.sess.BrowserSessionID()
	}
	return l.exe.ContainerName()
}

// where names the sandbox for messages: the container, or the session.
func (l *dockerLauncher) where() string {
	if l.sess != nil {
		return "session " + l.sess.BrowserSessionID()
	}
	return "container " + l.exe.ContainerName()
}

// sessionProbe checks chromium inside a running session. Called at launch,
// because a session starts lazily on the first call.
func sessionProbe(ctx context.Context, exe SandboxExecutor, bin string) error {
	if _, err := exe.RunShell(ctx, "command -v "+quoteSh(bin)); err != nil {
		return fmt.Errorf("chromium binary %q not found in the sandbox session — set browser_enabled:false or add chromium to the sandbox rootfs", bin)
	}
	// The CDP tunnel forwards through `python3` inside the session; without
	// it the browser starts but no connection can ever reach it.
	if _, err := exe.RunShell(ctx, "command -v python3"); err != nil {
		return fmt.Errorf("python3 not found in the sandbox session — the browser tunnel needs it; add python3 to the sandbox rootfs or set browser_enabled:false")
	}
	return nil
}

// newDockerLauncher checks that chromium exists in the container; it launches
// nothing.
func newDockerLauncher(exe SandboxExecutor, browserPath string) (launcher, error) {
	ds, ok := exe.(dockerSandbox)
	if !ok {
		return nil, fmt.Errorf("browser: executor %T cannot start interactive processes", exe)
	}
	bin := browserPath
	if bin == "" {
		bin = "chromium"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := ds.RunShell(ctx, "command -v "+quoteSh(bin)); err != nil {
		return nil, fmt.Errorf("chromium binary %q not found in container %s — set browser_enabled:false or install chromium in the image", bin, ds.ContainerName())
	}
	return &dockerLauncher{exe: ds, chromium: bin}, nil
}

// ringBuffer keeps the last n bytes written (browser stdout/stderr, only used
// for launch-failure diagnostics). It always drains, so the browser can never
// block on a full pipe.
type ringBuffer struct {
	mu  sync.Mutex
	n   int
	buf []byte
}

func (r *ringBuffer) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.n {
		r.buf = r.buf[len(r.buf)-r.n:]
	}
	return len(p), nil
}

func (r *ringBuffer) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return string(r.buf)
}

// profileDirName builds the per-launch profile dir. The random suffix is the
// handle every later step (port file, straggler scan, rm) keys on.
func profileDirName() (dir, suffix string) {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	suffix = hex.EncodeToString(b)
	return "/tmp/wakil-chrome-" + suffix, suffix
}

// startScript is what the `docker exec` session runs. HOME lives under the
// profile dir so crashpad's database (and any other $HOME state) carries the
// dir in its argv; exec keeps the pid, so $$ recorded here IS the browser's
// pid and — because docker exec makes the session leader — its pgid.
func startScript(dir, chromium string) string {
	return "set -e; umask 077; mkdir -p " + quoteSh(dir+"/home") +
		"; echo $$ > " + quoteSh(dir+"/pid") +
		"; s=$(cat /proc/$$/stat); rest=${s##*) }; set -- $rest; echo $3 > " + quoteSh(dir+"/pgid") +
		"; echo ${20} > " + quoteSh(dir+"/start") +
		"; HOME=" + quoteSh(dir+"/home") + " exec " + quoteSh(chromium) +
		" --headless=new --no-sandbox --disable-gpu --disable-dev-shm-usage" +
		" --disable-crash-reporter --disable-breakpad" +
		" --remote-debugging-port=0 --user-data-dir=" + quoteSh(dir)
}

func (l *dockerLauncher) launch(ctx context.Context) (string, func() error, *session, error) {
	dir, suffix := profileDirName()
	if l.sess != nil {
		// Session (hisn): probe chromium at launch, the session may only start now.
		if err := sessionProbe(ctx, l.exe, l.chromium); err != nil {
			return "", nil, nil, err
		}
	}
	marker := l.markerOf()
	if l.sess != nil && marker == "" {
		return "", nil, nil, fmt.Errorf("browser: no running sandbox session to own Chromium")
	}
	// The host exec client must outlive this launch: it is the browser's
	// process, torn down by the session's teardown, not by launchTimeout. Its
	// start is bound to the launch deadline only for the probe above.
	stdin, stdout, stderr, hostPid, err := l.exe.StartInteractive(context.WithoutCancel(ctx), startScript(dir, l.chromium))
	if err != nil {
		return "", nil, nil, fmt.Errorf("cannot start Chromium in %s (%w) — set browser_enabled:false or install chromium in the image", l.where(), err)
	}
	// Identity of the host-side exec client, taken now, before anything else
	// can happen to its pid (R2: teardown signals it only if still ours).
	hostID, hostOK := recordHostProcess(hostPid, marker)
	ring := &ringBuffer{n: 8 << 10}
	var drain sync.WaitGroup
	drain.Add(2)
	safe.Go("browser-drain-out", func() { defer drain.Done(); _, _ = io.Copy(ring, stdout) })
	safe.Go("browser-drain-err", func() { defer drain.Done(); _, _ = io.Copy(ring, stderr) })

	var tun *tunnel
	var once sync.Once
	teardownErr := error(nil)
	teardown := func() error {
		once.Do(func() {
			tun.close()
			_ = stdin.Close()
			teardownErr = l.stopBrowser(suffix)
			if hostOK {
				killOwned(hostID, diagf)
			}
			waitTimeout(&drain, 5*time.Second)
		})
		return teardownErr
	}

	port, err := l.waitDevToolsPort(ctx, dir)
	if err != nil {
		diag := strings.TrimSpace(ring.String())
		_ = teardown()
		if diag != "" {
			return "", nil, nil, fmt.Errorf("Chromium did not start in %s: %w\nbrowser output:\n%s", l.where(), err, diag)
		}
		return "", nil, nil, fmt.Errorf("Chromium did not start in %s: %w", l.where(), err)
	}
	var url string
	tun, url, err = startTunnel(l.exe, port, marker)
	if err != nil {
		_ = teardown()
		return "", nil, nil, err
	}
	return url, teardown, nil, nil
}

// waitDevToolsPort polls <dir>/DevToolsActivePort until it holds two lines
// (port, then the browser path). A one-line file is a partial write: keep
// polling. The port is chosen by Chromium (--remote-debugging-port=0), so a
// stale browser from an earlier run can never be mistaken for this one.
func (l *dockerLauncher) waitDevToolsPort(ctx context.Context, dir string) (int, error) {
	for {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		out, err := l.exe.RunShell(pctx, "cat "+quoteSh(dir+"/DevToolsActivePort")+" 2>/dev/null")
		cancel()
		if err == nil {
			if port, ok := parseDevToolsActivePort(out); ok {
				return port, nil
			}
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("no DevToolsActivePort after launch timeout")
		case <-time.After(150 * time.Millisecond):
		}
	}
}

// parseDevToolsActivePort accepts the file only when complete: first line a
// valid port, and a second (non-empty) line present.
func parseDevToolsActivePort(s string) (int, bool) {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) < 2 || strings.TrimSpace(lines[1]) == "" {
		return 0, false
	}
	p, err := strconv.Atoi(strings.TrimSpace(lines[0]))
	if err != nil || p <= 0 || p > 65535 {
		return 0, false
	}
	return p, true
}

// stopScript implements the stop: TERM the recorded group, wait, KILL, sweep
// stragglers (any process whose argv carries the profile dir — crashpad's
// handler and anything that left the group), remove the dir, verify. Prints
// "clean" or "leftover: <pids>".
//
// The scan must not match the shells running it: the profile dir is therefore
// never written into this script. The random suffix arrives as $1 and the dir
// is rebuilt from it with the literal split ("wakil-chrom" "e-"), and the grep
// pattern uses a bracket expression ("wakil-chrom[e]-"), so neither this
// script's argv nor the wrapper shell's argv contains the matchable string.
// `kill -SIG -PGID` has no "--": dash's kill builtin rejects "-- -N".
func stopScript(grace time.Duration) string {
	ticks := strconv.Itoa(int(grace/time.Second) * 10)
	return `S="$1"; D=/tmp/wakil-chrom"e-$S"
pids() { grep -l -a -- "wakil-chrom[e]-$S" /proc/[0-9]*/cmdline 2>/dev/null | sed 's#/proc/\([0-9]*\)/cmdline#\1#'; }
pg=$(cat "$D/pgid" 2>/dev/null || true)
case "$pg" in ''|*[!0-9]*|0|1) pg= ;; esac
# Signal the recorded group only if its leader is still the recorded browser
# (pid and start time): a reused pid must never direct a group kill at
# unrelated processes.
lp=$(cat "$D/pid" 2>/dev/null || true); ls=$(cat "$D/start" 2>/dev/null || true)
if [ -n "$pg" ]; then
  cur=$(cat /proc/$lp/stat 2>/dev/null) || cur=
  rest=${cur##*) }; set -- $rest; cs=${20:-}
  [ -n "$cur" ] && [ "$cs" = "$ls" ] && [ -n "$ls" ] || pg=
fi
[ -n "$pg" ] && kill -TERM -$pg 2>/dev/null
i=0; while [ $i -lt ` + ticks + ` ]; do [ -z "$(pids)" ] && break; sleep 0.1; i=$((i+1)); done
[ -n "$pg" ] && kill -KILL -$pg 2>/dev/null
for p in $(pids); do kill -KILL "$p" 2>/dev/null; done
sleep 0.2
rm -rf "$D"
left=$(pids | tr '\n' ' ')
if [ -z "$left" ] && [ ! -e "$D" ]; then echo clean; else echo "leftover: ${left}$([ -e "$D" ] && echo " dir")"; fi`
}

// stopBrowser runs the stop script in the container and reports a leftover.
func (l *dockerLauncher) stopBrowser(suffix string) error {
	ctx, cancel := context.WithTimeout(context.Background(), termGrace+20*time.Second)
	defer cancel()
	script := stopScript(termGrace)
	out, err := l.exe.RunShell(ctx, "sh -c "+quoteSh(script)+" sh "+quoteSh(suffix))
	if err != nil {
		return fmt.Errorf("browser stop: %w", err)
	}
	if got := strings.TrimSpace(out); got != "clean" {
		return fmt.Errorf("browser stop: %s", got)
	}
	return nil
}

// diagf routes ownership mismatches to the diagnostic sink (the TUI-safe seam;
// direct os.Stderr writes are forbidden outside the seam packages).
func diagf(format string, args ...any) {
	diag.Printf(format+"\n", args...)
}

// waitTimeout waits for wg for at most d.
func waitTimeout(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}
