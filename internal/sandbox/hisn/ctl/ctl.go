// Package ctl is Wakil's client for a hisn session's control socket
// (POLICY-SPEC §7): one JSON object per line, one response line per request,
// in order, max line 64 KiB. `exec` requests carry the program's three stdio
// fds in the same sendmsg as the request line (SCM_RIGHTS); hisn accepts fds
// only on the first line of a connection, and only for exec.
//
// Lifecycle (docs/hisn-ctl.md §3): an exec owns ONE connection for its whole
// life. Signals travel on their OWN fresh connections (DESIGN §14.7: hisn
// does not read the exec connection after the request line), so the exec
// connection carries only the request, the acceptance, and the async
// `{"exit":…}` line. A replacement session reusing the socket path can never
// be signalled with a stale exec_id: exec ids start fresh per session, so a
// stale id is E_NOT_FOUND, never a hit on a foreign process. One reader
// goroutine (the dispatcher) owns the bufio.Reader and routes response lines
// and the `{"exit":…}` line; writes are serialized by the conn mutex.
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
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"
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
	// label and inst identify this client in hisn's per-client audit (hisn
	// 0.4.0). An empty label sends no client object, which hisn answers with
	// its own derived label.
	label string
	inst  string
	seq   atomic.Uint64
}

// New builds a Client for the session's control socket path.
func New(sockPath string) *Client { return &Client{sockPath: sockPath} }

// MaxLabel is the longest client label sent, in bytes. hisn accepts longer
// ones, but a label is a diagnostic aid, not a place for an unbounded hostname.
const MaxLabel = 64

// TruncateLabel cuts label to MaxLabel bytes on a rune boundary and replaces
// invalid UTF-8, so the wire label is always valid and never longer than the
// limit (a byte slice through a multibyte rune would become U+FFFD and grow).
func TruncateLabel(label string) string {
	label = strings.ToValidUTF8(label, "?")
	if len(label) <= MaxLabel {
		return label
	}
	cut := MaxLabel
	for cut > 0 && !utf8.RuneStart(label[cut]) {
		cut--
	}
	return label[:cut]
}

// WithIdentity makes every request carry {"client":{"label":label,
// "req_id":"<inst>-<n>"}} (hisn >= 0.4.0 audits both). inst names this client
// instance, so request ids stay unique across Wakil processes. An empty label
// turns the client object off. It returns c.
func (c *Client) WithIdentity(label, inst string) *Client {
	c.label, c.inst = TruncateLabel(label), inst
	return c
}

// clientField is the "client" object of the next request, or nil when no
// identity is set. Each call yields a fresh req_id.
func (c *Client) clientField() map[string]any {
	if c.label == "" {
		return nil
	}
	f := map[string]any{"label": c.label}
	if c.inst != "" {
		f["req_id"] = fmt.Sprintf("%s-%d", c.inst, c.seq.Add(1))
	}
	return f
}

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
	if err := writeLine(conn, op, body, c.clientField()); err != nil {
		return err
	}
	return readResponse(conn, op, body == nil)
}

func writeLine(conn net.Conn, op string, body map[string]any, client map[string]any) error {
	m := map[string]any{"op": op}
	for k, v := range body {
		m[k] = v
	}
	if client != nil {
		m["client"] = client
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
	if err := writeLine(conn, "inspect", nil, c.clientField()); err != nil {
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

// NewStdio builds the three stdio pipes. If a later pipe cannot be created the
// earlier ones are closed again, so a failure leaks no descriptors.
func NewStdio() (*Stdio, error) {
	s := &Stdio{}
	var err error
	if s.InR, s.InW, err = os.Pipe(); err != nil {
		return nil, err
	}
	if s.OutR, s.OutW, err = os.Pipe(); err != nil {
		s.CloseEnds()
		return nil, err
	}
	if s.ErrR, s.ErrW, err = os.Pipe(); err != nil {
		s.CloseEnds()
		return nil, err
	}
	return s, nil
}

// CloseEnds closes every end (failure paths only).
func (s *Stdio) CloseEnds() {
	for _, f := range []*os.File{s.InR, s.InW, s.OutR, s.OutW, s.ErrR, s.ErrW} {
		if f != nil {
			_ = f.Close()
		}
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
	conn net.Conn
	dial func(ctx context.Context) (net.Conn, error) // the client's dialer, for signal connections
	// client yields the client object of a signal request (nil = none).
	client func() map[string]any
	wmu    sync.Mutex // serializes signal requests
	stdio  *Stdio

	execIDMu sync.RWMutex
	execID   string
	pid      int

	// Latched outcomes. Each is written EXACTLY ONCE (Once), then its done
	// channel is closed: any number of readers, in any order, concurrently
	// or later, observe the same value after <-done. Nothing here is ever
	// received from a channel that carries the value itself — a consumed
	// single-shot send was the "Wait hangs after cancel()" bug.
	acceptOnce sync.Once
	acceptDone chan struct{}
	acceptVal  acceptReply // valid once acceptDone is closed

	exitOnce sync.Once
	exitDone chan struct{}
	exitVal  *ExitPayload // valid once exitDone is closed; never nil there
}

type acceptReply struct {
	res ExecResult
	err error
}

// latchAccept records the acceptance outcome once; later calls are no-ops.
func (ec *ExecConn) latchAccept(r acceptReply) {
	ec.acceptOnce.Do(func() {
		ec.acceptVal = r
		close(ec.acceptDone)
	})
}

// latchExit records the terminal exit outcome once; later calls are no-ops.
func (ec *ExecConn) latchExit(p *ExitPayload) {
	ec.exitOnce.Do(func() {
		ec.exitVal = p
		close(ec.exitDone)
	})
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
	if cf := c.clientField(); cf != nil {
		req["client"] = cf
	}
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

	ec := &ExecConn{conn: conn, dial: c.dialConn, client: c.clientField, stdio: stdio, acceptDone: make(chan struct{}), exitDone: make(chan struct{})}
	go ec.dispatch()

	// Wait for the latched acceptance under ctx.
	select {
	case <-ctx.Done():
		// Ambiguous launch: close the conn — hisn cancels (TERM, then KILL
		// after 5 s) — and report lost. Never retry the launch.
		ec.closeLost()
		return nil, nil, ctx.Err()
	case <-ec.acceptDone:
	}
	a := ec.acceptVal
	if a.err != nil {
		_ = conn.Close()
		stdio.closeKept()
		return nil, nil, a.err
	}
	ec.setExecID(a.res.ExecID)
	ec.setPid(a.res.Pid)
	// The caller's deadline bounded the dial and the acceptance only. From here
	// the exec's life is governed by the program and by cancellation through
	// exec_id (TERM, then KILL, on fresh connections): a read deadline left on
	// the exec connection would make the dispatcher latch "lost" at the
	// deadline, racing the real exit line of a program that is being stopped.
	_ = conn.SetDeadline(time.Time{})
	return ec, &a.res, nil
}

// closeLost closes the connection and latches a lost terminal event. The kept
// stdio ends are closed too: a caller that cancels before acceptance will never
// read from them, and leaking the pipe fds would keep the pipe open until GC.
func (ec *ExecConn) closeLost() {
	_ = ec.conn.Close()
	ec.stdio.closeKept()
	ec.latchExit(&ExitPayload{Lost: true})
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

// dispatch is the ONE reader of the exec connection: it latches the first
// response line as the acceptance and the async `{"exit":…}` line as the
// exit. Any later response lines (signal replies) are consumed and dropped —
// only the exit line carries state that matters. Both outcomes are latched
// (written once, broadcast by closing a done channel), never sent.
func (ec *ExecConn) dispatch() {
	br := bufio.NewReaderSize(ec.conn, maxLine+1)
	accepted := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			// EOF/transport failure before an exit line: the status is lost.
			// Session death is NOT inferred from this EOF — a probe decides.
			// Before acceptance the launch was ambiguous: latch that too, so
			// StartExec does not block on ctx.
			ec.latchExit(&ExitPayload{Lost: true})
			if !accepted {
				ec.latchAccept(acceptReply{err: fmt.Errorf("%w", ErrAmbiguousLaunch)})
			}
			return
		}
		if len(line) > maxLine {
			ec.latchExit(&ExitPayload{Lost: true})
			if !accepted {
				ec.latchAccept(acceptReply{err: fmt.Errorf("%w: response line exceeds 64 KiB", ErrAmbiguousLaunch)})
			}
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
			ec.latchExit(&ep)
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
		ec.latchAccept(reply) // first response wins; later ones are no-ops
	}
}

// ExitDone is closed once the exit (or a lost status) has been latched.
func (ec *ExecConn) ExitDone() <-chan struct{} { return ec.exitDone }

// WaitExit blocks until the exit is latched (or ctx is done) and returns the
// latched value. It is safe to call from any number of goroutines, any number
// of times, before or after the exit: every call observes the same payload.
func (ec *ExecConn) WaitExit(ctx context.Context) (*ExitPayload, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-ec.exitDone:
		return ec.exitVal, nil
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
	sreq := map[string]any{"op": "signal", "exec_id": id, "signal": signal}
	if ec.client != nil {
		if cf := ec.client(); cf != nil {
			sreq["client"] = cf
		}
	}
	line, err := json.Marshal(sreq)
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("hisn ctl signal %s: write: %w", signal, err)
	}
	// Read the ONE response line and RETURN ITS FULL BODY — never swallow
	// it: an {"ok":false,"error":…} here is exactly what diagnostics need
	// (and what the integration test failure must show). Only the EXIT line
	// (on the exec connection) decides the exec's outcome; this body is
	// diagnostic, not verdict.
	br := bufio.NewReaderSize(conn, maxLine+1)
	resp, err := br.ReadString('\n')
	if err != nil {
		return fmt.Errorf("hisn ctl signal %s: read response: %w", signal, err)
	}
	var r struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(resp)), &r); err != nil {
		return fmt.Errorf("hisn ctl signal %s: malformed response %q: %w", signal, strings.TrimSpace(resp), err)
	}
	if !r.OK {
		return &Error{Op: "signal", Code: r.Error, Message: r.Message}
	}
	return nil
}

// CloseConn closes only the exec connection (hisn cancels the exec if still
// running: TERM, then KILL after 5 s), leaving the kept stdio ends open so a
// caller can drain what the program already wrote. Close the ends with Close.
func (ec *ExecConn) CloseConn() { _ = ec.conn.Close() }

// Close closes the exec connection (hisn cancels the exec if still running:
// TERM, then KILL after 5 s) and the kept stdio ends. Closing a kept read end
// DISCARDS whatever is still buffered in its pipe: drain the copiers first.
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
