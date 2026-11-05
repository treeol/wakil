package browser

// Live B1–B3 check against a real hardened container (build-tag-free but
// skipped unless WAKIL_BROWSER_LIVE_IMAGE names a local image that contains
// chromium and python3, e.g. wakil-dev). The container mirrors production
// hardening: read-only rootfs, cap-drop=ALL, no-new-privileges, tmpfs /tmp, and
// the image's wakil-entrypoint.sh as PID 1 (the process that adopts and reaps
// the browser's orphaned children, as in production).

import (
	"context"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

type liveDocker struct{ name string }

func (d liveDocker) ContainerName() string { return d.name }
func (d liveDocker) RunShell(ctx context.Context, command string) (string, error) {
	out, err := osexec.CommandContext(ctx, "docker", "exec", d.name, "sh", "-c", command).CombinedOutput()
	return string(out), err
}
func (d liveDocker) StartInteractive(_ context.Context, command string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, int, error) {
	cmd := osexec.Command("docker", "exec", "-i", d.name, "sh", "-c", command)
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	er, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, 0, err
	}
	go func() { _ = cmd.Wait() }()
	return in, out, er, cmd.Process.Pid, nil
}

// aliveChromium counts non-zombie processes whose argv or name is chromium or
// carries a wakil-chrome profile dir.
func (d liveDocker) aliveChromium(t *testing.T) string {
	t.Helper()
	return d.sh(t, `n=0; for f in /proc/[0-9]*/stat; do s=$(cat $f 2>/dev/null) || continue; rest=${s##*) }; set -- $rest; [ "$1" = Z ] && continue; p=${f%/stat}; c=$(tr '\0' ' ' < $p/cmdline 2>/dev/null); case "$c" in *chromiu[m]*|*wakil-chrom[e]-*) n=$((n+1));; esac; done; echo $n`)
}

func (d liveDocker) sh(t *testing.T, script string) string {
	t.Helper()
	out, _ := d.RunShell(context.Background(), script)
	return strings.TrimSpace(out)
}

func startLiveContainer(t *testing.T) liveDocker {
	t.Helper()
	image := os.Getenv("WAKIL_BROWSER_LIVE_IMAGE")
	if image == "" {
		t.Skip("set WAKIL_BROWSER_LIVE_IMAGE (e.g. wakil-dev) to run the live browser lifecycle test")
	}
	name := fmt.Sprintf("wakil-browser-live-%d", time.Now().UnixNano()%1e9)
	out, err := osexec.Command("docker", "run", "-d", "--rm", "--name", name,
		"--user", fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid()),
		"--cap-drop=ALL", "--security-opt=no-new-privileges", "--read-only",
		"--tmpfs=/tmp:rw,exec,nosuid,nodev,size=1g", "--tmpfs=/etc:rw,nosuid,nodev,size=1m",
		"--entrypoint", "/usr/local/bin/wakil-entrypoint.sh", image, "sleep", "infinity").CombinedOutput()
	if err != nil {
		t.Skipf("cannot start container from %s: %v: %s", image, err, out)
	}
	t.Cleanup(func() { _ = osexec.Command("docker", "rm", "-f", name).Run() })
	// Production restores the image's /etc files into the fresh /etc tmpfs
	// right after start (exec.restoreEtcBackups); chromium's launcher script
	// aborts on an empty /etc/chromium.d. Same step here, as root.
	if out, err := osexec.Command("docker", "exec", "-u", "0", name, "sh", "-c",
		"if [ -d /usr/local/share/wakil-etc-backup/chromium.d ]; then mkdir -p /etc/chromium.d && "+
			"cp -a /usr/local/share/wakil-etc-backup/chromium.d/. /etc/chromium.d/; fi; "+
			"if [ -f /usr/local/share/wakil-etc-backup/ssl-certs/ca-certificates.crt ]; then mkdir -p /etc/ssl/certs && "+
			"cp /usr/local/share/wakil-etc-backup/ssl-certs/ca-certificates.crt /etc/ssl/certs/; fi").CombinedOutput(); err != nil {
		t.Skipf("cannot restore /etc backups in %s: %v: %s", image, err, out)
	}
	return liveDocker{name}
}

var listenRe = regexp.MustCompile(`(?m)^\s*\d+:\s+([0-9A-F]+):([0-9A-F]+)\s+[0-9A-F:]+\s+0A\b`)

// nonLoopbackListeners returns the listening TCP sockets in the container that
// are bound to something other than loopback (127.x / ::1).
func nonLoopbackListeners(t *testing.T, d liveDocker) []string {
	t.Helper()
	var bad []string
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		for _, m := range listenRe.FindAllStringSubmatch(d.sh(t, "cat "+f), -1) {
			addr := m[1]
			loop := strings.HasSuffix(addr, "0100007F") && len(addr) == 8 || // 127.0.0.1
				addr == "00000000000000000000000001000000" // ::1
			if !loop {
				bad = append(bad, f+":"+addr+":"+m[2])
			}
		}
	}
	return bad
}

func TestLive_BrowserLifecycleInHardenedContainer(t *testing.T) {
	d := startLiveContainer(t)
	for _, bin := range []string{"chromium", "python3"} {
		if out := d.sh(t, "command -v "+bin); out == "" {
			t.Skipf("image lacks %s", bin)
		}
	}

	// B1: nothing runs at container start.
	if n := d.sh(t, `grep -l -a "chromiu[m]" /proc/[0-9]*/cmdline 2>/dev/null | wc -l`); n != "0" {
		t.Fatalf("chromium running right after container start: %s", n)
	}
	t.Logf("process list at container start:\n%s", d.sh(t, "ps -eo pid,ppid,pgid,sid,args | cut -c1-110"))

	m, err := NewManager(d, "", WithIdleTimeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	if m.Running() {
		t.Fatal("NewManager must not launch the browser")
	}
	if n := d.sh(t, `grep -l -a "remote-debuggin[g]" /proc/[0-9]*/cmdline 2>/dev/null | wc -l`); n != "0" {
		t.Fatalf("browser launched by NewManager: %s processes", n)
	}

	// First tool call launches it; a real CDP action goes through the tunnel
	// (/json/version discovery AND the websocket).
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	title, final, err := m.Navigate(ctx, "data:text/html,<title>live-ok</title><h1 id=x>hello</h1>")
	if err != nil {
		t.Fatalf("Navigate through the loopback tunnel: %v", err)
	}
	if title != "live-ok" || !strings.HasPrefix(final, "data:") {
		t.Errorf("title=%q final=%q", title, final)
	}
	if txt, err := m.GetText(ctx, "#x"); err != nil || txt != "hello" {
		t.Errorf("GetText = %q, %v", txt, err)
	}
	if !m.Running() {
		t.Fatal("browser must be running after a tool call")
	}

	ps := d.sh(t, "ps -eo pid,ppid,pgid,sid,args | cut -c1-200")
	t.Logf("process list while running:\n%s", ps)
	profile := d.sh(t, `ls -d /tmp/wakil-chrome-* 2>/dev/null`)
	if strings.Count(profile, "\n") != 0 || !strings.HasPrefix(profile, "/tmp/wakil-chrome-") {
		t.Fatalf("expected exactly one per-launch profile dir, got %q", profile)
	}
	if strings.Contains(ps, "wakil-chrome-profile") {
		t.Error("fixed profile dir /tmp/wakil-chrome-profile must not be used")
	}
	// B3: nothing listens on a non-loopback address inside the container.
	if bad := nonLoopbackListeners(t, d); len(bad) != 0 {
		t.Errorf("non-loopback listeners in the container: %v", bad)
	}
	// The leader is the recorded pid, its own group leader, with the flags in argv.
	leader := d.sh(t, "cat "+profile+"/pid")
	pgid := d.sh(t, "cat "+profile+"/pgid")
	if leader == "" || leader != pgid {
		t.Errorf("leader pid %q must equal its pgid %q (direct child, own group)", leader, pgid)
	}
	argv := d.sh(t, "tr '\\0' ' ' < /proc/"+leader+"/cmdline")
	for _, want := range []string{"--disable-crash-reporter", "--disable-breakpad", "--remote-debugging-port=0", "--user-data-dir=" + profile, "--headless=new"} {
		if !strings.Contains(argv, want) {
			t.Errorf("leader argv lacks %q:\n%s", want, argv)
		}
	}
	if strings.Contains(argv, "remote-debugging-address") || strings.Contains(argv, "0.0.0.0") {
		t.Errorf("leader must not open a non-loopback debugging address:\n%s", argv)
	}
	t.Logf("crashpad handlers still spawned despite the flags: %s",
		d.sh(t, `grep -l -a "chrome_crashpad_handle[r]" /proc/[0-9]*/cmdline 2>/dev/null | wc -l`))

	// B2 idle: after the idle timeout nothing carrying the profile dir remains.
	deadline := time.Now().Add(40 * time.Second)
	for m.Running() && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
	}
	if m.Running() {
		t.Fatal("browser did not stop after the idle timeout")
	}
	// Running() turns false when the stop begins; the teardown (TERM, grace,
	// KILL, sweep, rm) holds lifeMu until it is done.
	m.lifeMu.Lock()
	m.lifeMu.Unlock() //nolint:staticcheck // empty critical section: waits for the teardown
	left := d.sh(t, `grep -l -a "wakil-chrom[e]-" /proc/[0-9]*/cmdline 2>/dev/null | wc -l`)
	t.Logf("process list after idle stop:\n%s", d.sh(t, "ps -eo pid,ppid,pgid,sid,args | cut -c1-110"))
	if left != "0" {
		t.Errorf("%s process(es) still carry a wakil-chrome dir after the idle stop", left)
	}
	if n := d.aliveChromium(t); n != "0" {
		t.Errorf("%s live (non-zombie) chromium process(es) after idle stop", n)
	}
	if n := d.sh(t, `pgrep -fa "chromiu[m]" | wc -l`); n != "0" {
		t.Errorf("pgrep -fa chromium inside the container shows %s process(es) after idle stop (zombies count)", n)
	}
	if got := d.sh(t, `ls -d /tmp/wakil-chrome-* 2>/dev/null | wc -l`); got != "0" {
		t.Errorf("profile dir not removed: %s left", got)
	}

	// A call after the stop launches a fresh browser in a new dir.
	if _, _, err := m.Navigate(ctx, "about:blank"); err != nil {
		t.Fatalf("relaunch after idle stop: %v", err)
	}
	dir2 := d.sh(t, `ls -d /tmp/wakil-chrome-* 2>/dev/null`)
	if dir2 == "" || dir2 == profile {
		t.Errorf("relaunch must use a new profile dir (old %q new %q)", profile, dir2)
	}

	// B2 /new: Stop() ends it and removes the dir.
	if err := m.Stop(); err != nil {
		t.Errorf("Stop: %v", err)
	}
	if n := d.aliveChromium(t); n != "0" {
		t.Errorf("%s live chromium process(es) survived Stop()", n)
	}
	if n := d.sh(t, `pgrep -fa "chromiu[m]" | wc -l`); n != "0" {
		t.Errorf("pgrep -fa chromium shows %s process(es) after Stop() (zombies count)", n)
	}
	if got := d.sh(t, `ls -d /tmp/wakil-chrome-* 2>/dev/null | wc -l`); got != "0" {
		t.Errorf("profile dir survived Stop(): %s", got)
	}

	// Close (Wakil exit) after another launch.
	if _, _, err := m.Navigate(ctx, "about:blank"); err != nil {
		t.Fatalf("relaunch: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	if n := d.aliveChromium(t); n != "0" {
		t.Errorf("%s live chromium process(es) survived Close()", n)
	}
	if n := d.sh(t, `pgrep -fa "chromiu[m]" | wc -l`); n != "0" {
		t.Errorf("pgrep -fa chromium shows %s process(es) after Close() (zombies count)", n)
	}
	t.Logf("process list at the end:\n%s", d.sh(t, "ps -eo pid,ppid,stat,args | cut -c1-110"))
}
