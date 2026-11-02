package ctl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// ---- test scaffolding: a fake §7 server ----

type fakeServer struct {
	ln   net.Listener
	sock string

	mu      sync.Mutex
	execs   int      // exec requests received
	fdsPer  [][]int  // fds arrived with each exec line (ONE sendmsg or not)
	exitIDs []string // exec_ids assigned
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	dir, err := os.MkdirTemp("", "ctl")
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "control.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeServer{ln: ln, sock: sock}
	t.Cleanup(func() {
		_ = ln.Close()
		_ = os.RemoveAll(dir)
	})
	return f
}

// serve serves connections forever: exec lines get acceptance + a relay
// (stdin→stdout verbatim), signal lines get ok/E_NOT_FOUND, other ops ok.
func (f *fakeServer) serve(t *testing.T) {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.serveConn(t, conn)
	}
}

func (f *fakeServer) serveConn(t *testing.T, conn net.Conn) {
	defer conn.Close()
	uc := conn.(*net.UnixConn)
	line, fds, err := readLineMsg(uc)
	if err != nil {
		return
	}
	var req struct {
		Op     string            `json:"op"`
		Argv   []string          `json:"argv"`
		Env    map[string]string `json:"env"`
		Cwd    string            `json:"cwd"`
		Tty    *bool             `json:"tty"`
		Signal string            `json:"signal"`
		ExecID string            `json:"exec_id"`
	}
	if err := json.Unmarshal(line, &req); err != nil {
		writeJSON(uc, map[string]any{"ok": false, "error": "E_BAD_REQUEST", "message": "malformed"})
		return
	}
	if req.Op != "exec" {
		switch req.Op {
		case "signal":
			f.mu.Lock()
			known := false
			for _, id := range f.exitIDs {
				if id == req.ExecID {
					known = true
				}
			}
			f.mu.Unlock()
			if !known {
				writeJSON(uc, map[string]any{"ok": false, "error": "E_NOT_FOUND", "message": "stale exec_id"})
				return
			}
			writeJSON(uc, map[string]any{"ok": true})
		default:
			writeJSON(uc, map[string]any{"ok": true})
		}
		return
	}
	if req.Tty == nil || *req.Tty || len(fds) != 3 {
		writeJSON(uc, map[string]any{"ok": false, "error": "E_BAD_REQUEST", "message": "want exec tty=false with 3 fds"})
		return
	}
	f.mu.Lock()
	f.execs++
	n := f.execs
	f.fdsPer = append(f.fdsPer, fds)
	f.mu.Unlock()
	id := "e-" + itoa(n)
	f.mu.Lock()
	f.exitIDs = append(f.exitIDs, id)
	f.mu.Unlock()
	writeJSON(uc, map[string]any{"ok": true, "pid": 4200 + n, "exec_id": id})

	stdin := os.NewFile(uintptr(fds[0]), "stdin")
	stdout := os.NewFile(uintptr(fds[1]), "stdout")
	stderr := os.NewFile(uintptr(fds[2]), "stderr")
	// Relay stdin → stdout verbatim, a note to stderr, exit 7 on stdin EOF.
	go func() {
		buf := make([]byte, 4096)
		var acc []byte
		for {
			n, err := stdin.Read(buf)
			acc = append(acc, buf[:n]...)
			if err != nil {
				break
			}
		}
		_, _ = stdout.Write(acc)
		_, _ = stderr.Write([]byte("note\n"))
		writeJSON(uc, map[string]any{"exit": map[string]any{"code": 7}})
	}()
	// Signal lines after acceptance on the same conn.
	for {
		ln, _, err := readLineMsg(uc)
		if err != nil {
			return
		}
		var sig struct {
			Op     string `json:"op"`
			ExecID string `json:"exec_id"`
		}
		_ = json.Unmarshal(ln, &sig)
		if sig.Op != "signal" {
			continue
		}
		f.mu.Lock()
		known := false
		for _, id := range f.exitIDs {
			if id == sig.ExecID {
				known = true
			}
		}
		f.mu.Unlock()
		if !known {
			writeJSON(uc, map[string]any{"ok": false, "error": "E_NOT_FOUND", "message": "stale exec_id"})
			continue
		}
		writeJSON(uc, map[string]any{"ok": true})
	}
}

func readLineMsg(uc *net.UnixConn) (line []byte, fds []int, err error) {
	buf := make([]byte, 4096)
	oob := make([]byte, 512)
	for {
		n, oobn, _, _, rerr := uc.ReadMsgUnix(buf, oob)
		if rerr != nil {
			return nil, nil, rerr
		}
		if oobn > 0 {
			fds = append(fds, parseOOBTest(oob[:oobn])...)
		}
		if idx := indexByte(buf[:n], '\n'); idx >= 0 {
			line = append(line, buf[:idx]...)
			return line, fds, nil
		}
	}
}

func parseOOBTest(b []byte) (fds []int) {
	msgs, err := syscall.ParseSocketControlMessage(b)
	if err != nil {
		return nil
	}
	for _, m := range msgs {
		if m.Header.Level == syscall.SOL_SOCKET && m.Header.Type == syscall.SCM_RIGHTS {
			for i := 0; i+4 <= len(m.Data); i += 4 {
				fds = append(fds, int(int32(uint32(m.Data[i])|uint32(m.Data[i+1])<<8|uint32(m.Data[i+2])<<16|uint32(m.Data[i+3])<<24)))
			}
		}
	}
	return
}

func writeJSON(uc *net.UnixConn, v any) {
	b, _ := json.Marshal(v)
	_, _ = uc.Write(append(b, '\n'))
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var d []byte
	for ; n > 0; n /= 10 {
		d = append([]byte{byte('0' + n%10)}, d...)
	}
	return string(d)
}

func ctxT(t *testing.T, d time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), d)
}

// ---- tests ----

// The exec request line and the 3 fds arrive in ONE sendmsg: the server's
// first ReadMsgUnix sees both the JSON line and the rights. Stdio flows
// through the passed fds; the async exit line reaches WaitExit.
func TestStartExecFramingStdioAndExit(t *testing.T) {
	f := newFakeServer(t)
	go f.serve(t)
	c := New(f.sock)
	stdio, err := NewStdio()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := ctxT(t, 5*time.Second)
	defer cancel()
	ec, res, err := c.StartExec(ctx, []string{"cat"}, "", nil, stdio)
	if err != nil {
		t.Fatalf("StartExec: %v", err)
	}
	if res.ExecID != "e-1" || res.Pid == 0 {
		t.Fatalf("acceptance = %+v", res)
	}
	f.mu.Lock()
	execs, fdn := f.execs, len(f.fdsPer[0])
	f.mu.Unlock()
	if execs != 1 {
		t.Fatalf("execs = %d, want 1", execs)
	}
	if fdn != 3 {
		t.Fatalf("fds in one sendmsg = %d, want 3", fdn)
	}
	if _, err := stdio.InW.Write([]byte("hello-ctl\n")); err != nil {
		t.Fatal(err)
	}
	_ = stdio.InW.Close() // stdin EOF → relay → exit {"code":7}
	ep, err := ec.WaitExit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ep == nil || ep.Code == nil || *ep.Code != 7 {
		t.Fatalf("exit payload = %+v, want code 7", ep)
	}
	out, _ := bufio.NewReader(stdio.OutR).ReadString('\n')
	if !strings.Contains(out, "hello-ctl") {
		t.Fatalf("stdout relay = %q", out)
	}
	ec.Close()
}

// Signal travels on the exec's OWN connection (the server's per-exec loop
// answers it), and the dispatcher never misroutes it as the exit line.
func TestSignalOnOwnConn(t *testing.T) {
	f := newFakeServer(t)
	go f.serve(t)
	c := New(f.sock)
	stdio, _ := NewStdio()
	ctx, cancel := ctxT(t, 5*time.Second)
	defer cancel()
	ec, _, err := c.StartExec(ctx, []string{"cat"}, "", nil, stdio)
	if err != nil {
		t.Fatal(err)
	}
	if err := ec.Signal("TERM"); err != nil {
		t.Fatalf("signal on own conn: %v", err)
	}
	// The reply (ok) is consumed by the dispatcher and dropped; the exec is
	// still waiting for its exit line, not the signal reply.
	_ = stdio.InW.Close()
	ep, err := ec.WaitExit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ep == nil || ep.Code == nil || *ep.Code != 7 {
		t.Fatalf("exit after signal = %+v", ep)
	}
	ec.Close()
}

// A refused dial (socket file present, nothing listening) is ErrSessionDead;
// a missing socket path is ErrNoSession.
func TestDialClassification(t *testing.T) {
	c := New(filepath.Join(t.TempDir(), "no-such", "control.sock"))
	if err := c.call(context.Background(), "list", nil); !errors.Is(err, ErrNoSession) {
		t.Fatalf("missing socket = %v, want ErrNoSession", err)
	}
	sock := filepath.Join(t.TempDir(), "control.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close() // file remains (Go unlinks by default), nothing accepts → ECONNREFUSED
	c2 := New(sock)
	if err := c2.call(context.Background(), "list", nil); !errors.Is(err, ErrSessionDead) {
		t.Fatalf("refused dial = %v, want ErrSessionDead", err)
	}
}
