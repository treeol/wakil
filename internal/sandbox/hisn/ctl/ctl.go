// Package ctl is Wakil's client for a hisn session's control socket
// (POLICY-SPEC §7): one JSON object per line, one response line per request,
// in order, max line 64 KiB. `exec` requests carry the program's three stdio
// fds in the same sendmsg as the request line (SCM_RIGHTS); hisn accepts fds
// only on the first line of a connection, and only for exec.
//
// Lifecycle (docs/hisn-ctl.md §3): an exec owns ONE connection for its whole
// life. Signals travel on that connection — never a fresh dial — so a
// replacement session reusing the socket path can never be signalled with a
// stale exec_id. One reader goroutine (the dispatcher) owns the bufio.Reader
// and routes response lines and the async `{"exit":…}` line; writes are
// serialized by the conn mutex.
package ctl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ErrAmbiguousLaunch reports that the connection dropped before the exec was
// accepted: whether the program started is unknown. Never retried.
var ErrAmbiguousLaunch = errors.New("hisn ctl: exec launch ambiguous (connection lost before acceptance)")

// ErrNoSession reports that the session's socket path does not exist: there
// is no session to run in (hisn: "no such session"). A backend error for the
// exec's own outcome — the session was not necessarily ever Wakil's.
var ErrNoSession = errors.New("hisn ctl: no session (socket path missing)")

// ErrSessionDead reports that the session's control socket refused the
// connection: the supervisor behind it is gone, the session is over.
var ErrSessionDead = errors.New("hisn ctl: control socket refused (session is dead)")

// maxLine is hisn's protocol line limit.
const maxLine = 64 << 10

// SocketPath is the control socket path of session id under runtime root.
func SocketPath(root, id string) string { return filepath.Join(root, id, "control.sock") }

// Error is a hisn error response ({"ok":false,"error":CODE,"message":…}).
type Error struct {
	Op      string
	Code    string
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("hisn %s failed: %s: %s", e.Op, e.Code, e.Message) }

// IsNotFound reports an E_NOT_FOUND — a stale exec_id or recycled pid.
func (e *Error) IsNotFound() bool { return e.Code == "E_NOT_FOUND" }

// Client is one hisn session's control client. Every operation dials its own
// short-lived connection except exec, which holds one for the program's life.
type Client struct {
	sockPath string
}

// New builds a Client for the session's control socket path.
func New(sockPath string) *Client { return &Client{sockPath: sockPath} }

// dialConn dials the control socket with ctx. ECONNREFUSED is ErrSessionDead
// (the socket is there, nobody accepts: the supervisor is gone). ENOENT is
// ErrNoSession (no session dir at all: hisn's own diagnostic is
// "no such session", exit 125 — a backend error, not a dead-session case).
func (c *Client) dialConn(ctx context.Context) (net.Conn, error) {
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "unix", c.sockPath)
	if err != nil {
		var oe *net.OpError
		if errors.As(err, &oe) && oe.Op == "dial" && errors.Is(oe.Err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("%w: %w", ErrSessionDead, err)
		}
		if errors.As(err, &oe) && oe.Op == "dial" && errors.Is(oe.Err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %w", ErrNoSession, err)
		}
		return nil, fmt.Errorf("hisn ctl dial %s: %w", c.sockPath, err)
	}
	return conn, nil
}

// call dials, sends one request line, reads one response line, maps errors.
// call dials, sends one request line, reads one response line, maps errors.
// It returns "" (the response carries no body beyond ok/error/message).
func (c *Client) call(ctx context.Context, op string, body map[string]any) error {
	conn, err := c.dialConn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if err := writeLine(conn, op, body); err != nil {
		return err
	}
	return readResponse(conn, op, body == nil)
}

func writeLine(conn net.Conn, op string, body map[string]any) error {
	m := map[string]any{"op": op}
	for k, v := range body {
		m[k] = v
	}
	line, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("hisn ctl %s: write: %w", op, err)
	}
	return nil
}

// readResponse reads one `{"ok":…}` line; with full=false the entire rest of
// the response is the envelope (ok/error/message), with full=true the raw
// line is unmarshalled into body by the caller.
func readResponse(conn net.Conn, op string, _ bool) error {
	br := bufio.NewReaderSize(conn, maxLine+1)
	line, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("hisn ctl %s: read response: %w", op, err)
	}
	return parseResponse(line, op)
}

func parseResponse(line, op string) error {
	if len(line) > maxLine {
		return fmt.Errorf("hisn ctl %s: response line exceeds 64 KiB", op)
	}
	var r struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &r); err != nil {
		return fmt.Errorf("hisn ctl %s: malformed response %q: %w", op, strings.TrimSpace(line), err)
	}
	if !r.OK {
		return &Error{Op: op, Code: r.Error, Message: r.Message}
	}
	return nil
}

// ---- control operations ----

// Grant asks for a dynamic grant (mode "ro" | "rw").
func (c *Client) Grant(ctx context.Context, path, mode string) error {
	return c.call(ctx, "grant", map[string]any{"path": path, "mode": mode})
}

// Revoke withdraws a grant (soft: already-open descriptors may survive).
func (c *Client) Revoke(ctx context.Context, path string) error {
	return c.call(ctx, "revoke", map[string]any{"path": path})
}

// Kill ends the whole session (signal name or number; default TERM).
func (c *Client) Kill(ctx context.Context, signal string) error {
	body := map[string]any{}
	if signal != "" {
		body["signal"] = signal
	}
	return c.call(ctx, "kill", body)
}

// Inspect returns mountinfo + the resolved policy text.
func (c *Client) Inspect(ctx context.Context) (mountinfo, policy string, err error) {
	conn, err := c.dialConn(ctx)
	if err != nil {
		return "", "", err
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	if err := writeLine(conn, "inspect", nil); err != nil {
		return "", "", err
	}
	br := bufio.NewReaderSize(conn, maxLine+1)
	line, err := br.ReadString('\n')
	if err != nil {
		return "", "", fmt.Errorf("hisn ctl inspect: read: %w", err)
	}
	var raw struct {
		OK        bool   `json:"ok"`
		Error     string `json:"error"`
		Message   string `json:"message"`
		Mountinfo string `json:"mountinfo"`
		Policy    string `json:"policy"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &raw); err != nil {
		return "", "", fmt.Errorf("hisn ctl inspect: malformed response: %w", err)
	}
	if !raw.OK {
		return "", "", &Error{Op: "inspect", Code: raw.Error, Message: raw.Message}
	}
	return raw.Mountinfo, raw.Policy, nil
}

// ---- exec ----

// ExecResult is the synchronous acceptance of an exec.
type ExecResult struct {
	Pid    int    `json:"pid"` // pid inside the sandbox pid namespace
	ExecID string `json:"exec_id"`
}

// ExitPayload is the async terminal `{"exit":…}` line of an exec.
type ExitPayload struct {
	Code   *int    `json:"code"`
	Signal *string `json:"signal"`
	Lost   bool    `json:"lost"`
}

// Stdio is the program's three pipes. The PASSED ends (InR, OutW, ErrW)
// become the program's stdio via SCM_RIGHTS; the KEPT ends (InW, OutR, ErrR)
// stay in Wakil for the stdio copiers.
type Stdio struct {
	InR, InW   *os.File
	OutR, OutW *os.File
	ErrR, ErrW *os.File
}

// NewStdio builds the three stdio pipes.
func NewStdio() (*Stdio, error) {
	s := &Stdio{}
	var err error
	if s.InR, s.InW, err = os.Pipe(); err != nil {
		return nil, err
	}
	if s.OutR, s.OutW, err = os.Pipe(); err != nil {
		return nil, err
	}
	if s.ErrR, s.ErrW, err = os.Pipe(); err != nil {
		return nil, err
	}
	return s, nil
}

// CloseEnds closes every end (failure paths only).
func (s *Stdio) CloseEnds() {
	for _, f := range []*os.File{s.InR, s.InW, s.OutR, s.OutW, s.ErrR, s.ErrW} {
		_ = f.Close()
	}
}

// closePassed closes only the passed ends (before/without a sendmsg).
func (s *Stdio) closePassed() {
	_ = s.InR.Close()
	_ = s.OutW.Close()
	_ = s.ErrW.Close()
}

// closeKept closes only the kept ends.
func (s *Stdio) closeKept() {
	_ = s.InW.Close()
	_ = s.OutR.Close()
	_ = s.ErrR.Close()
}

// ExecConn is one accepted exec: one exec connection for the program's whole
// life (the request line, the acceptance response, the async exit line —
// nothing else; hisn does not read it further), one dispatcher reading it,
// the accepted exec_id, and the kept stdio ends. Signals open their OWN
// short-lived connections (see Signal).
type ExecConn struct {
	conn  net.Conn
	dial  func(ctx context.Context) (net.Conn, error) // the client's dialer, for signal connections
	wmu   sync.Mutex                                  // serializes signal requests
	stdio *Stdio

	execIDMu sync.RWMutex
	execID   string
	pid      int

	acceptCh chan acceptReply // buffered 1; the first response line
	exitCh   chan *ExitPayload
	exitOnce sync.Once
}

type acceptReply struct {
	res ExecResult
	err error
}

// StartExec dials, sends the exec request line WITH the three passed fds in
// ONE sendmsg, waits for acceptance (exec_id), and returns the live conn.
// On acceptance the caller owns ec and must Wait/Cancel it. The passed ends'
// Wakil-side copies are closed right after the sendmsg — SCM_RIGHTS does not
// consume the sender's descriptors, and copier EOF must not depend on a
// descriptor Wakil still holds.
func (c *Client) StartExec(ctx context.Context, argv []string, cwd string, env map[string]string, stdio *Stdio) (*ExecConn, *ExecResult, error) {
	conn, err := c.dialConn(ctx)
	if err != nil {
		stdio.CloseEnds()
		return nil, nil, err
	}
	req := map[string]any{"op": "exec", "argv": argv, "tty": false}
	if cwd != "" {
		req["cwd"] = cwd
	}
	if len(env) > 0 {
		req["env"] = env
	}
	line, err := json.Marshal(req)
	if err != nil {
		stdio.CloseEnds()
		_ = conn.Close()
		return nil, nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		stdio.CloseEnds()
		_ = conn.Close()
		return nil, nil, fmt.Errorf("hisn ctl exec: not a unix conn")
	}
	// ONE sendmsg: the JSON line and the three fds travel together. hisn
	// accepts fds only here (first line, exec only). A failed write transfers
	// nothing: everything closes, and a possibly-accepted exec is NEVER
	// retried with rights re-sent. EPIPE on a just-dialled conn is the same
	// ambiguity as an EOF before acceptance (the peer hung up mid-handshake).
	if err := writeWithFds(uc, append(line, '\n'), stdio.InR, stdio.OutW, stdio.ErrW); err != nil {
		stdio.CloseEnds()
		_ = conn.Close()
		if errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ECONNRESET) {
			return nil, nil, fmt.Errorf("%w: %w", ErrAmbiguousLaunch, err)
		}
		return nil, nil, fmt.Errorf("hisn ctl exec: write: %w", err)
	}
	// Transfer succeeded: hisn owns the passed ends now. Close ours.
	stdio.closePassed()

	ec := &ExecConn{conn: conn, dial: c.dialConn, stdio: stdio, acceptCh: make(chan acceptReply, 1), exitCh: make(chan *ExitPayload, 1)}
	go ec.dispatch()

	// Wait for acceptance under ctx.
	type result struct {
		ec  *ExecConn
		res *ExecResult
		err error
	}
	ch := make(chan result, 1)
	go func() {
		a := <-ec.acceptCh
		if a.err != nil {
			_ = conn.Close()
			stdio.closeKept()
			ch <- result{nil, nil, a.err}
			return
		}
		ec.setExecID(a.res.ExecID)
		ec.setPid(a.res.Pid)
		ch <- result{ec, &a.res, nil}
	}()
	select {
	case <-ctx.Done():
		// Ambiguous launch: close the conn — hisn cancels (TERM, then KILL
		// after 5 s) — and report lost. Never retry the launch.
		ec.closeLost()
		// Drain the acceptance goroutine so it does not leak its channel.
		go func() {
			select {
			case <-ch:
			case <-time.After(time.Second):
			}
		}()
		return nil, nil, ctx.Err()
	case r := <-ch:
		return r.ec, r.res, r.err
	}
}

// closeLost closes the connection and publishes a lost terminal event.
func (ec *ExecConn) closeLost() {
	_ = ec.conn.Close()
	ec.publishLost()
}

func (ec *ExecConn) publishLost() {
	ec.exitOnce.Do(func() { ec.exitCh <- &ExitPayload{Lost: true} })
}

func (ec *ExecConn) setExecID(id string) { ec.execIDMu.Lock(); ec.execID = id; ec.execIDMu.Unlock() }

// ExecID returns the accepted exec_id ("" before acceptance).
func (ec *ExecConn) ExecID() string {
	ec.execIDMu.RLock()
	defer ec.execIDMu.RUnlock()
	return ec.execID
}

// Pid is the program's pid inside the sandbox pid namespace (0 before acceptance).
func (ec *ExecConn) Pid() int {
	ec.execIDMu.RLock()
	defer ec.execIDMu.RUnlock()
	return ec.pid
}

// Stdio exposes the KEPT ends (InW, OutR, ErrR) for the stdio copiers.
func (ec *ExecConn) Stdio() *Stdio { return ec.stdio }

// setPid records the accepted pid.
func (ec *ExecConn) setPid(pid int) { ec.execIDMu.Lock(); ec.pid = pid; ec.execIDMu.Unlock() }

// dispatch is the ONE reader of the exec connection: it routes the first
// response line to acceptCh and the async `{"exit":…}` line to exitCh.
// Any later response lines (signal replies) are consumed and dropped — only
// the exit line carries state that matters.
func (ec *ExecConn) dispatch() {
	br := bufio.NewReaderSize(ec.conn, maxLine+1)
	accepted := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			// EOF/transport failure before an exit line: the status is lost.
			// Session death is NOT inferred from this EOF — a probe decides.
			// Before acceptance the launch was ambiguous: it goes to the
			// acceptance channel too, so StartExec does not block on ctx.
			ec.exitOnce.Do(func() { ec.exitCh <- &ExitPayload{Lost: true} })
			if !accepted {
				select {
				case ec.acceptCh <- acceptReply{err: fmt.Errorf("%w", ErrAmbiguousLaunch)}:
				default:
				}
			}
			return
		}
		if len(line) > maxLine {
			ec.exitOnce.Do(func() { ec.exitCh <- &ExitPayload{Lost: true} })
			return
		}
		var probe struct {
			Exit json.RawMessage `json:"exit"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &probe); err != nil {
			continue // skip malformed lines
		}
		if len(probe.Exit) > 0 {
			var ep ExitPayload
			if err := json.Unmarshal(probe.Exit, &ep); err != nil {
				ep = ExitPayload{Lost: true}
			}
			ec.exitOnce.Do(func() { ec.exitCh <- &ep })
			return
		}
		// A response line: the acceptance (first), or a signal reply (later).
		var acc struct {
			OK      bool   `json:"ok"`
			Error   string `json:"error"`
			Message string `json:"message"`
			Pid     int    `json:"pid"`
			ExecID  string `json:"exec_id"`
		}
		_ = json.Unmarshal([]byte(strings.TrimSpace(line)), &acc)
		reply := acceptReply{res: ExecResult{Pid: acc.Pid, ExecID: acc.ExecID}}
		if !acc.OK {
			reply.err = &Error{Op: "exec", Code: acc.Error, Message: acc.Message}
		} else {
			accepted = true
		}
		select {
		case ec.acceptCh <- reply:
		default: // acceptance already consumed: a signal reply, dropped
		}
	}
}

// WaitExit blocks for the terminal exit line, or ctx done.
func (ec *ExecConn) WaitExit(ctx context.Context) (*ExitPayload, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case ep := <-ec.exitCh:
		return ep, nil
	}
}

// Signal sends {"op":"signal","exec_id":…} on a NEW connection to the
// session socket, reads the one response line, closes. hisn (DESIGN §14.7)
// does NOT read the exec connection after the request line: the exec
// connection carries only the request, the acceptance response, and the
// async exit line — a signal written there would hang forever. The
// replacement-identity hazard the same-conn design closed is not reopened:
// the signal is keyed by exec_id, and a replacement session's own exec ids
// start fresh; a stale id in a replacement is E_NOT_FOUND, never a hit.
// E_NOT_FOUND here means the exec already exited — the exit line decides
// the outcome.
func (ec *ExecConn) Signal(ctx context.Context, signal string) error {
	id := ec.ExecID()
	if id == "" {
		return fmt.Errorf("hisn ctl signal: exec not yet accepted")
	}
	ec.wmu.Lock()
	defer ec.wmu.Unlock()
	conn, err := ec.dial(ctx)
	if err != nil {
		return fmt.Errorf("hisn ctl signal %s: %w", signal, err)
	}
	defer conn.Close()
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	line, err := json.Marshal(map[string]any{"op": "signal", "exec_id": id, "signal": signal})
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("hisn ctl signal %s: write: %w", signal, err)
	}
	// One response line, then close. Its body is irrelevant: only the exit
	// line (on the exec connection) decides the outcome.
	br := bufio.NewReaderSize(conn, maxLine+1)
	if _, err := br.ReadString('\n'); err != nil {
		return fmt.Errorf("hisn ctl signal %s: read response: %w", signal, err)
	}
	return nil
}

// Close closes the exec connection (hisn cancels the exec if still running:
// TERM, then KILL after 5 s) and the kept stdio ends.
func (ec *ExecConn) Close() {
	_ = ec.conn.Close()
	ec.stdio.closeKept()
}

// writeWithFds writes data on the unix socket with fds attached, in one
// sendmsg (SCM_RIGHTS).
func writeWithFds(conn *net.UnixConn, data []byte, fds ...*os.File) error {
	if len(fds) == 0 {
		_, err := conn.Write(data)
		return err
	}
	rights := []byte{}
	for _, f := range fds {
		rights = append(rights, syscall.UnixRights(int(f.Fd()))...)
	}
	_, _, err := conn.WriteMsgUnix(data, rights, nil)
	return err
}
