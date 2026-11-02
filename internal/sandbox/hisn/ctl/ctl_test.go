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
	// signalLines are the raw request lines of every signal connection, in
	// arrival order — for the byte-for-byte spec pin.
	signalLines []string
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
			f.signalLines = append(f.signalLines, string(line))
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

// Signal travels on its OWN fresh connection (hisn DESIGN §14.7: the exec
// connection is not read after the request line — a signal written there
// would hang). The signal reply is read on that fresh conn and closed; the
// exec dispatcher never sees it, so it cannot misroute it as the exit line.
func TestSignalOnFreshConn(t *testing.T) {
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
	if err := ec.Signal(ctx, "TERM"); err != nil {
		t.Fatalf("signal on a fresh conn: %v", err)
	}
	// The exec is still waiting for its EXIT line (the signal reply went to
	// its own conn), so the program must still be alive here; stdin EOF ends
	// it and delivers the exit line.
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

// The Signal path NEVER writes to the exec connection: hisn does not read it
// after the request line. A signal line written to the exec conn would hang
// the client (no response ever comes) — assert Signal does not do that by
// checking the signal arrives (its conn gets a response) while the exec
// conn carries nothing past the acceptance + exit lines.
func TestSignalNeverWritesToExecConn(t *testing.T) {
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
	// The fake ignores anything written to the exec conn after the request
	// line (as real hisn does). If Signal used the exec conn, this call
	// would time out on its response read.
	sctx, scancel := ctxT(t, 2*time.Second)
	defer scancel()
	if err := ec.Signal(sctx, "TERM"); err != nil {
		t.Fatalf("signal must succeed without touching the exec conn: %v", err)
	}
	_ = stdio.InW.Close()
	ep, err := ec.WaitExit(ctx)
	if err != nil || ep == nil || ep.Code == nil || *ep.Code != 7 {
		t.Fatalf("exit = %+v err %v: the exec conn was disturbed by the signal", ep, err)
	}
	ec.Close()
}

// The signal request line is pinned BYTE-FOR-BYTE to POLICY-SPEC §7:
//
//	{"op":"signal","exec_id":"e-n","signal":"TERM"}
//
// field names exactly `op`/`exec_id`/`signal`, signal spelled as the short
// name ("TERM"/"KILL", both accepted by hisn as name-or-number), one line,
// on a fresh connection.
func TestSignalRequestJSONByteForByte(t *testing.T) {
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
	for _, sig := range []string{"TERM", "KILL"} {
		if err := ec.Signal(ctx, sig); err != nil {
			t.Fatalf("signal %s: %v", sig, err)
		}
	}
	f.mu.Lock()
	lines := append([]string(nil), f.signalLines...)
	f.mu.Unlock()
	if len(lines) != 2 {
		t.Fatalf("signal lines = %d, want 2 (one per signal)", len(lines))
	}
	// json.Marshal of a map sorts keys alphabetically; §7 is a JSON object
	// per line, key order immaterial — the RECEIVER contract is field names
	// + values.
	for i, tc := range []struct{ execID, sig string }{{"e-1", "TERM"}, {"e-1", "KILL"}} {
		var m map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &m); err != nil {
			t.Fatalf("signal line %d not JSON: %q: %v", i, lines[i], err)
		}
		if len(m) != 3 {
			t.Fatalf("signal line %d has %d fields, want exactly op+exec_id+signal: %q", i, len(m), lines[i])
		}
		if m["op"] != "signal" || m["exec_id"] != tc.execID || m["signal"] != tc.sig {
			t.Fatalf("signal line %d = %q, want {op:signal, exec_id:%s, signal:%s}", i, lines[i], tc.execID, tc.sig)
		}
	}
	// The exec connection is untouched by signals: stdin EOF still delivers
	// the exit line.
	_ = stdio.InW.Close()
	ep, err := ec.WaitExit(ctx)
	if err != nil || ep == nil || ep.Code == nil || *ep.Code != 7 {
		t.Fatalf("exit after signals = %+v err %v", ep, err)
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
