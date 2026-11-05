package browser

// Loopback-only CDP tunnel (0.4.4, B3).
//
// Chromium inside the container listens on the container's loopback only. The
// host reaches it through a listener on 127.0.0.1:<ephemeral>: every accepted
// connection starts one `docker exec -i … python3` forwarder inside the
// container that copies bytes between its stdio and 127.0.0.1:<chromium port>.
// No port is published and nothing listens on a non-loopback address, on the
// host or in the container. The tunnel is a transparent byte pipe: it rewrites
// nothing, so Chromium sees the Host header the client sent (127.0.0.1:<port>)
// and builds a webSocketDebuggerUrl that points back at the tunnel.

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/treeol/wakil/internal/safe"
)

// forwarderScript copies stdin→socket and socket→stdout byte for byte. Partial
// writes are looped; EOF on stdin half-closes the socket so the peer sees the
// end of the request stream; the process exits when the socket closes.
const forwarderScript = `import socket,sys,os,threading
s=socket.create_connection(("127.0.0.1",int(sys.argv[1])))
def up():
    try:
        while True:
            d=os.read(0,65536)
            if not d:
                break
            s.sendall(d)
    except Exception:
        pass
    try:
        s.shutdown(socket.SHUT_WR)
    except Exception:
        pass
threading.Thread(target=up,daemon=True).start()
try:
    while True:
        d=s.recv(65536)
        if not d:
            break
        v=memoryview(d)
        while len(v):
            n=os.write(1,v)
            v=v[n:]
except Exception:
    pass
`

// interactiveStarter is the part of the executor the tunnel needs.
type interactiveStarter interface {
	StartInteractive(ctx context.Context, command string) (stdin io.WriteCloser, stdout io.ReadCloser, stderr io.ReadCloser, pid int, err error)
}

// tunnel is one loopback listener and its live forwarders.
type tunnel struct {
	ln    net.Listener
	exe   interactiveStarter
	port  int // the in-container CDP port
	wg    sync.WaitGroup
	mu    sync.Mutex
	conns map[net.Conn]struct{}
	done  bool
}

// startTunnel listens on 127.0.0.1:0 and forwards each connection to the
// in-container 127.0.0.1:port. The returned URL is "http://127.0.0.1:<hostport>".
func startTunnel(exe interactiveStarter, port int) (*tunnel, string, error) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, "", fmt.Errorf("browser tunnel: listen: %w", err)
	}
	t := &tunnel{ln: ln, exe: exe, port: port, conns: map[net.Conn]struct{}{}}
	t.wg.Add(1)
	safe.Go("browser-tunnel-accept", func() { defer t.wg.Done(); t.acceptLoop() })
	return t, "http://" + ln.Addr().String(), nil
}

// addr returns the listener address (always 127.0.0.1).
func (t *tunnel) addr() net.Addr { return t.ln.Addr() }

func (t *tunnel) acceptLoop() {
	for {
		c, err := t.ln.Accept()
		if err != nil {
			return
		}
		if !t.track(c) {
			_ = c.Close()
			return
		}
		t.wg.Add(1)
		safe.Go("browser-tunnel-conn", func() { defer t.wg.Done(); t.serve(c) })
	}
}

func (t *tunnel) track(c net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return false
	}
	t.conns[c] = struct{}{}
	return true
}

// serve pipes one client connection through one in-container forwarder.
func (t *tunnel) serve(c net.Conn) {
	defer func() {
		t.mu.Lock()
		delete(t.conns, c)
		t.mu.Unlock()
		_ = c.Close()
	}()
	cmd := "exec python3 -c " + quoteSh(forwarderScript) + " " + strconv.Itoa(t.port)
	// The context is not cancelled after the call: an executor may tie the
	// started process to it (the docker executor does not; others might).
	stdin, stdout, stderr, pid, err := t.exe.StartInteractive(context.Background(), cmd)
	if err != nil {
		return
	}
	// stderr is not part of the byte stream: drain it so the forwarder can
	// never block on a full pipe, and drop it.
	go func() { _, _ = io.Copy(io.Discard, stderr) }()

	var cw sync.WaitGroup
	cw.Add(2)
	go func() { // client → container
		defer cw.Done()
		_, _ = io.Copy(stdin, c)
		_ = stdin.Close() // EOF → forwarder half-closes the socket
	}()
	go func() { // container → client
		defer cw.Done()
		_, _ = io.Copy(c, stdout)
		// The forwarder is gone (socket closed): end the client side too.
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	cw.Wait()
	_ = stdin.Close()
	_ = stdout.Close()
	killHostPid(pid)
}

// killHostPid ends the host-side `docker exec` client; the in-container
// forwarder exits on its own when its stdin closes.
func killHostPid(pid int) {
	if pid <= 0 {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// close stops accepting, closes every live connection and waits for every
// forwarder goroutine to finish.
func (t *tunnel) close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.done = true
	for c := range t.conns {
		_ = c.Close()
	}
	t.mu.Unlock()
	_ = t.ln.Close()
	waited := make(chan struct{})
	go func() { t.wg.Wait(); close(waited) }()
	select {
	case <-waited:
	case <-time.After(10 * time.Second):
	}
}
