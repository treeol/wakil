package hisn

// fake_control_test.go: the fake hisn supervisor's §7 control-socket server.
// One JSON object per line, one response line per request, in order, max 64
// KiB. Execs get exec_id "e-<n>" (monotonic); signal takes exec_id; fds come
// only on the first line of a connection, only for exec (SCM_RIGHTS).
// Stale-exec semantics: E_NOT_FOUND for an unknown exec_id or a recycled pid.

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type fakeCtl struct {
	mu    sync.Mutex
	next  int64                   // exec counter
	execs map[string]*fakeExecRec // exec_id → exec (finished ones kept: signal → E_NOT_FOUND)
}

type fakeExecRec struct {
	id  string
	pid int
}

func fakeCtlServe(ln net.Listener, dir string) {
	c := &fakeCtl{execs: map[string]*fakeExecRec{}}
	go func() {
		if exists(filepath.Join(dir, "refuse")) { // test hook: stop accepting (dial still works on a stream socket until the listener closes, so close it outright)
			_ = ln.Close()
			return
		}
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			if exists(filepath.Join(dir, "refuse")) { // became refusing mid-flight
				_ = conn.Close()
				_ = ln.Close()
				return
			}
			go c.serve(conn)
		}
	}()
}

func (c *fakeCtl) serve(conn net.Conn) {
	defer conn.Close()
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return
	}
	for {
		// First line may carry fds (exec only).
		line, fds, err := readMsg(uc)
		if err != nil {
			return
		}
		var req struct {
			Op     string            `json:"op"`
			Argv   []string          `json:"argv"`
			Cwd    string            `json:"cwd"`
			Env    map[string]string `json:"env"`
			Signal string            `json:"signal"`
			ExecID string            `json:"exec_id"`
			Path   string            `json:"path"`
			Mode   string            `json:"mode"`
			Row    json.RawMessage   `json:"-"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			writeLine(uc, map[string]any{"ok": false, "error": "E_BAD_REQUEST", "message": "malformed line"})
			return
		}
		switch req.Op {
		case "exec":
			c.serveExec(uc, req, fds)
			return // one exec per connection: the conn is its life
		case "signal":
			c.mu.Lock()
			ex, ok := c.execs[req.ExecID]
			c.mu.Unlock()
			if !ok {
				writeLine(uc, map[string]any{"ok": false, "error": "E_NOT_FOUND", "message": "no such exec_id"})
				return
			}
			if err := syscall.Kill(ex.pid, signalFromName(req.Signal)); err != nil {
				writeLine(uc, map[string]any{"ok": false, "error": "E_NOT_FOUND", "message": "exec gone"})
				return
			}
			writeLine(uc, map[string]any{"ok": true})
		case "grant", "revoke", "list", "inspect", "kill":
			writeLine(uc, map[string]any{"ok": true})
		default:
			writeLine(uc, map[string]any{"ok": false, "error": "E_BAD_REQUEST", "message": "unknown op " + req.Op})
		}
	}
}

func (c *fakeCtl) serveExec(uc *net.UnixConn, req struct {
	Op     string            `json:"op"`
	Argv   []string          `json:"argv"`
	Cwd    string            `json:"cwd"`
	Env    map[string]string `json:"env"`
	Signal string            `json:"signal"`
	ExecID string            `json:"exec_id"`
	Path   string            `json:"path"`
	Mode   string            `json:"mode"`
	Row    json.RawMessage   `json:"-"`
}, fds []int) {
	if len(fds) != 3 || len(req.Argv) == 0 {
		writeLine(uc, map[string]any{"ok": false, "error": "E_BAD_REQUEST", "message": "exec needs argv and 3 fds"})
		return
	}
	// Pass the received fds as the child's stdio.
	var stdin, stdout, stderr *os.File
	stdin = os.NewFile(uintptr(fds[0]), "stdin")
	stdout = os.NewFile(uintptr(fds[1]), "stdout")
	stderr = os.NewFile(uintptr(fds[2]), "stderr")
	if req.Argv[0] == "__lost" {
		// v0.2.0 semantics: the monitor is gone — no response, no exit line;
		// the connection just drops. The client reports Lost.
		return
	}
	cmd := exec.Command(req.Argv[0], req.Argv[1:]...)
	cmd.Dir = req.Cwd
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if err := cmd.Start(); err != nil {
		writeLine(uc, map[string]any{"ok": false, "error": "E_EXEC", "message": err.Error()})
		return
	}
	id := fmt.Sprintf("e-%d", atomic.AddInt64(&c.next, 1))
	c.mu.Lock()
	c.execs[id] = &fakeExecRec{id: id, pid: cmd.Process.Pid}
	c.mu.Unlock()
	writeLine(uc, map[string]any{"ok": true, "pid": cmd.Process.Pid, "exec_id": id})

	// Real hisn (DESIGN §14.7) does NOT read the exec connection after the
	// request line: the exec conn carries only the request, the acceptance
	// response, and the exit line. A signal written there would hang — and in
	// the fake it must hang too (a client that signals on the exec conn is
	// buggy and must be caught by tests). Do NOT read further lines here.
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	// Closing the connection cancels: TERM then KILL after 5 s (protocol §7);
	// the fake does not need to emulate that — the exit line below ends the
	// exec once the program does, and Wakil's Kill path signals on a fresh
	// connection.
	<-done
	writeLine(uc, map[string]any{"exit": exitPayloadOf(cmd)})
}

// exitPayloadOf is the §7 exit object: {"code":N} for a normal exit,
// {"signal":"KILL"} when the program was killed by a signal.
func exitPayloadOf(cmd *exec.Cmd) map[string]any {
	if cmd.ProcessState == nil {
		return map[string]any{"lost": true}
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		name := strings.TrimPrefix(strings.ToUpper(ws.Signal().String()), "SIG")
		switch ws.Signal() {
		case syscall.SIGKILL:
			name = "KILL"
		case syscall.SIGTERM:
			name = "TERM"
		}
		return map[string]any{"signal": name}
	}
	return map[string]any{"code": cmd.ProcessState.ExitCode()}
}

func signalFromName(s string) syscall.Signal {
	switch strings.ToUpper(s) {
	case "TERM", "":
		return syscall.SIGTERM
	case "KILL":
		return syscall.SIGKILL
	case "INT":
		return syscall.SIGINT
	case "HUP":
		return syscall.SIGHUP
	}
	return syscall.SIGTERM
}

// readMsg reads one line and any SCM_RIGHTS fds arriving with it. It reads
// ONLY via ReadMsgUnix on the raw conn — never through a bufio.Reader on the
// same conn (bufio would swallow the line into its buffer and ReadMsgUnix
// would then block forever, wedging the exec handshake). The OOB buffer is
// PER CALL: a shared one would let concurrent execs clobber each other's fds.
func readMsg(uc *net.UnixConn) (line []byte, fds []int, err error) {
	buf := make([]byte, 4096)
	oob := make([]byte, 512)
	for {
		n, oobn, _, _, rerr := uc.ReadMsgUnix(buf, oob)
		if rerr != nil {
			return nil, nil, rerr
		}
		if oobn > 0 {
			fds = append(fds, parseOOB(oob[:oobn])...)
		}
		if idx := indexByte(buf[:n], '\n'); idx >= 0 {
			line = append(line, buf[:idx]...)
			return line, fds, nil
		}
		line = append(line, buf[:n]...)
		if len(line) > 64<<10 {
			return nil, nil, fmt.Errorf("line too long")
		}
	}
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

func parseOOB(b []byte) []int {
	msgs, err := syscall.ParseSocketControlMessage(b)
	if err != nil {
		return nil
	}
	var fds []int
	for _, m := range msgs {
		if m.Header.Level == syscall.SOL_SOCKET && m.Header.Type == syscall.SCM_RIGHTS {
			for i := 0; i+4 <= len(m.Data); i += 4 {
				fds = append(fds, int(int32(uint32(m.Data[i])|uint32(m.Data[i+1])<<8|uint32(m.Data[i+2])<<16|uint32(m.Data[i+3])<<24)))
			}
		}
	}
	return fds
}

func writeLine(uc *net.UnixConn, v any) {
	b, _ := json.Marshal(v)
	_, _ = uc.Write(append(b, '\n'))
}

var _ = strconv.Itoa
var _ = filepath.Join
var _ = time.Now
