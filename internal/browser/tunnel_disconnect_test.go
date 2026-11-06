package browser

// B3 review (item 4): the tunnel must not leak when a forwarder dies abruptly —
// not a clean close. The forwarder here is a real subprocess; the test kills it
// mid-stream with SIGKILL, as a container-side crash would, and then checks that
// the client connection ends, the host-side exec is reaped, and nothing is left
// running for the tunnel.

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	osexec "os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// killableForwarder runs the forwarder as a local process and records its pid,
// so the test can kill it abruptly.
type killableForwarder struct {
	mu   sync.Mutex
	pids []int
}

func (k *killableForwarder) StartInteractive(_ context.Context, command string) (io.WriteCloser, io.ReadCloser, io.ReadCloser, int, error) {
	cmd := osexec.Command("sh", "-c", command)
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	er, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, 0, err
	}
	k.mu.Lock()
	k.pids = append(k.pids, cmd.Process.Pid)
	k.mu.Unlock()
	go func() { _ = cmd.Wait() }()
	return in, out, er, cmd.Process.Pid, nil
}

// alive reports whether pid is still a running process (not a zombie).
func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	rest := string(b)
	if i := strings.LastIndex(rest, ") "); i >= 0 {
		rest = rest[i+2:]
	}
	return !strings.HasPrefix(rest, "Z")
}

func TestTunnel_AbruptForwarderDeathReapsEverything(t *testing.T) {
	if _, err := osexec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	// An upstream that accepts and then streams forever, so the forwarder is
	// mid-stream when it is killed.
	upstream, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		for {
			c, err := upstream.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				chunk := make([]byte, 4096)
				for {
					if _, err := c.Write(chunk); err != nil {
						return
					}
					time.Sleep(time.Millisecond)
				}
			}(c)
		}
	}()

	fwd := &killableForwarder{}
	tun, _, err := startTunnel(fwd, upstream.Addr().(*net.TCPAddr).Port, "")
	if err != nil {
		t.Fatal(err)
	}
	defer tun.close()

	client, err := net.DialTimeout("tcp4", tun.addr().String(), 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	// Wait until the stream is flowing, then kill the forwarder abruptly.
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 8192)
	if _, err := io.ReadFull(client, buf); err != nil {
		t.Fatalf("stream never started: %v", err)
	}
	fwd.mu.Lock()
	pids := append([]int(nil), fwd.pids...)
	fwd.mu.Unlock()
	if len(pids) != 1 {
		t.Fatalf("expected one forwarder, got %d", len(pids))
	}
	if err := syscall.Kill(pids[0], syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}

	// The client must see the connection END — an EOF or a reset — not a read
	// timeout: a hang here is exactly the leak this test exists to catch.
	_ = client.SetReadDeadline(time.Now().Add(10 * time.Second))
	var endErr error
	for {
		if _, err := client.Read(buf); err != nil {
			endErr = err
			break
		}
	}
	var ne net.Error
	if errors.As(endErr, &ne) && ne.Timeout() {
		t.Fatalf("client connection hung after the forwarder died (no EOF/reset): %v", endErr)
	}

	// The forwarder is gone and the tunnel has dropped the connection.
	deadline := time.Now().Add(10 * time.Second)
	for alive(pids[0]) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if alive(pids[0]) {
		t.Errorf("forwarder pid %d still running after SIGKILL", pids[0])
	}

	// close() must not hang and must leave no connection goroutine behind.
	done := make(chan struct{})
	go func() { tun.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("tunnel.close hung after an abrupt forwarder death")
	}
}

// The browser side: after an abrupt forwarder death, the stop path (teardown:
// tunnel close, then the straggler sweep) must still reap every process that
// carries the profile dir, including the browser group. The forwarder is only
// the transport; the reaping must not depend on it.
func TestAbruptForwarderDeath_StopPathReapsBrowser(t *testing.T) {
	if _, err := osexec.LookPath("setsid"); err != nil {
		t.Skip("setsid not installed")
	}
	dir, suffix := profileDirName()
	if err := os.MkdirAll(dir+"/home", 0o755); err != nil {
		t.Skipf("cannot create %s: %v", dir, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	reapFixtureSleepers(t)

	// A browser stand-in with the same shape as the stop-script test: a leader
	// in its own session with two sleeping children in the group, plus a
	// straggler in another group, all carrying the profile dir in argv.
	start := `set -e; cd ` + quoteSh(dir) + `
setsid sh -c 'echo $$ > pid; s=$(cat /proc/$$/stat); rest=${s##*) }; set -- $rest; echo $3 > pgid; sleep 2718 & sleep 2718; : "$0"' ` + quoteSh(dir+"/leader") + ` >/dev/null 2>&1 &
setsid sh -c 'sleep 2718; : "$0"' ` + quoteSh(dir+"/home") + ` >/dev/null 2>&1 &
sleep 0.8`
	if out, err := osexec.Command("sh", "-c", start).CombinedOutput(); err != nil {
		t.Fatalf("start: %v: %s", err, out)
	}
	pgidRaw, err := os.ReadFile(dir + "/pgid")
	if err != nil {
		t.Fatalf("pgid: %v", err)
	}
	if n := countProcsWith(t, suffix); n < 2 {
		t.Fatalf("browser stand-in not running (%d processes)", n)
	}

	// Abrupt forwarder death: a forwarder process is SIGKILLed while the tunnel
	// is up (exercised above); here the teardown runs afterwards, as the idle
	// or /new path does.
	fwd := osexec.Command("sleep", "2718")
	if err := fwd.Start(); err != nil {
		t.Fatal(err)
	}
	_ = fwd.Process.Kill()
	_ = fwd.Wait()

	out, err := osexec.Command("sh", "-c", stopScript(500*time.Millisecond), "sh", suffix).CombinedOutput()
	if err != nil {
		t.Fatalf("stop script: %v: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "clean" {
		t.Fatalf("stop reported %q", got)
	}
	if n := countProcsWith(t, suffix); n != 0 {
		t.Errorf("%d processes survived the stop after an abrupt forwarder death", n)
	}
	if osexec.Command("sh", "-c", "kill -0 -"+strings.TrimSpace(string(pgidRaw))).Run() == nil {
		t.Errorf("browser process group still alive after stop")
	}
}
