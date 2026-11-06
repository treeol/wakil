package hisn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/treeol/wakil/internal/sandbox"
	"github.com/treeol/wakil/internal/sandbox/hisn/ctl"
)

// Exec implements sandbox.Backend. Two paths share one validation:
//   - ctl (default): the control-socket client (internal/sandbox/hisn/ctl);
//     exec_id-signalled, one connection per exec, stdio over SCM_RIGHTS.
//   - CLI (sandbox.hisn.exec_via_cli = true): `hisn exec --session ID …`,
//     one client process per call. It is also the ownership transport for
//     the browser (ExecCLI), so it stays until a control-socket ownership
//     protocol replaces it (0.5.x).
func (b *Backend) Exec(ctx context.Context, spec sandbox.ExecSpec) (sandbox.ExecHandle, error) {
	if len(spec.Argv) == 0 {
		return nil, fmt.Errorf("hisn exec: empty argv")
	}
	cwd, err := b.resolveExecCwd(spec.Cwd)
	if err != nil {
		return nil, err
	}
	// Per-exec env is allowlisted: only env_pass names, never computed ones.
	env, err := b.validateExecEnv(spec.Env)
	if err != nil {
		return nil, err
	}

	l, release, err := b.admit()
	if err != nil {
		return nil, err
	}
	if l.ctx.Err() != nil { // died or replaced between admission and here
		release()
		if l.dead() {
			return nil, l.deadErr()
		}
		return nil, sandbox.ErrNoSession
	}

	if b.opts.ExecViaCLI {
		return b.execCLI(ctx, l, release, spec, cwd, env)
	}
	return b.execCtl(ctx, l, release, spec, cwd, env)
}

// validateExecEnv builds the per-exec env map: every key must be in
// sandbox.hisn.env_pass and not computed by hisn, else the WHOLE call fails.
func (b *Backend) validateExecEnv(e map[string]string) (map[string]string, error) {
	allowed := map[string]bool{}
	for _, n := range b.opts.EnvPass {
		allowed[n] = true
	}
	keys := make([]string, 0, len(e))
	for k := range e {
		if !allowed[k] || isComputedEnv(k) {
			return nil, fmt.Errorf("hisn exec: env %q is not allowed (not in sandbox.hisn.env_pass or computed by hisn)", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := map[string]string{}
	for _, k := range keys {
		out[k] = e[k]
	}
	return out, nil
}

// resolveExecCwd maps the AGENT-side Cwd to the execution path.
func (b *Backend) resolveExecCwd(cwd string) (string, error) {
	if cwd == "" {
		return "", nil
	}
	return b.pm.ResolveExecCwd(cwd)
}

// ExecCLI implements sandbox.CLIExecer: it runs spec through the `hisn exec
// --session <id>` client, so the host process carries the session id in its
// argv (the ownership marker for long-lived processes such as the browser).
// The CLI path is NOT a fallback for this use: it is the ownership transport
// until a control-socket ownership protocol replaces it (0.5.x, see
// docs/PLAN-0.5.0.md). Tool calls keep the control socket.
func (b *Backend) ExecCLI(ctx context.Context, spec sandbox.ExecSpec) (sandbox.ExecHandle, error) {
	if len(spec.Argv) == 0 {
		return nil, fmt.Errorf("hisn exec: empty argv")
	}
	cwd, err := b.resolveExecCwd(spec.Cwd)
	if err != nil {
		return nil, err
	}
	env, err := b.validateExecEnv(spec.Env)
	if err != nil {
		return nil, err
	}
	l, release, err := b.admit()
	if err != nil {
		return nil, err
	}
	if l.ctx.Err() != nil {
		release()
		if l.dead() {
			return nil, l.deadErr()
		}
		return nil, sandbox.ErrNoSession
	}
	return b.execCLI(ctx, l, release, spec, cwd, env)
}

// ActiveSessionID implements sandbox.CLIExecer.
func (b *Backend) ActiveSessionID() string { return b.ActiveID() }

// execCLI runs one `hisn exec --session <id>` client process for spec.
func (b *Backend) execCLI(ctx context.Context, l *live, release func(), spec sandbox.ExecSpec, cwd string, env map[string]string) (sandbox.ExecHandle, error) {
	args := []string{"exec", "--session", l.id}
	if cwd != "" {
		args = append(args, "--cwd", cwd)
	}
	for k, v := range env {
		args = append(args, "--env", k+"="+v)
	}
	args = append(args, "--")
	args = append(args, spec.Argv...)

	// The exec client dies with the caller's ctx AND with the session epoch.
	cctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(l.ctx, cancel)

	var out io.Writer = io.Discard
	if spec.Stdout != nil {
		out = spec.Stdout
	}
	// One gate for both streams: Stdout and Stderr are routinely the same writer.
	var gate copyGate
	dw := &diagWriter{w: gate.wrap(out)}
	cmd := b.command(cctx, false, args...)
	cmd.Stdin = spec.Stdin
	cmd.Stdout = gate.wrap(out)
	cmd.Stderr = dw
	if spec.Stderr != nil {
		dw = &diagWriter{w: gate.wrap(spec.Stderr)}
		cmd.Stderr = dw
	}
	h, err := sandbox.StartCmd(cmd)
	if err != nil {
		stop()
		cancel()
		release()
		return nil, err
	}
	return &execHandle{b: b, l: l, h: h, dw: dw, argv: spec.Argv, cleanup: func() { stop(); cancel(); release() }}, nil
}

// execHandle post-processes the hisn client's exit into a sandbox.ExitInfo.
type execHandle struct {
	b       *Backend
	l       *live
	h       *sandbox.CmdHandle
	dw      *diagWriter
	argv    []string
	cleanup func()

	once sync.Once
	info sandbox.ExitInfo
	err  error
}

func (e *execHandle) Pid() int    { return e.h.Pid() }
func (e *execHandle) Kill() error { return e.h.Kill() }

func (e *execHandle) Wait() (sandbox.ExitInfo, error) {
	e.once.Do(func() {
		e.info, e.err = e.h.Wait()
		e.cleanup()
		// A supervisor that died while the exec was in flight ended the whole
		// sandbox, program included: nothing "may still be running" and the
		// failure is the session's, not the program's. Only a failed run is
		// reinterpreted; a program that already finished keeps its status.
		// The exec client's stderr also carries the PROGRAM's stderr, so a text
		// match alone proves nothing: a program may print "hisn: connection
		// refused" and exit 125. Only Wakil's own probe of the session decides.
		if e.info.Code == exitHisnError && e.dw.Refused() && !e.l.dead() && e.b.sessionUnreachable(e.l) {
			e.l.refused.Store(true)
		}
		if e.l.dead() && (e.err != nil || e.info.Code == exitHisnError || e.info.Code >= 128) {
			e.info.Lost, e.info.BackendError = false, true
			e.err = e.l.deadErr()
			return
		}
		// hisn exit 125 is hisn's own error. v0.2.0 prints a "hisn: ..."
		// diagnostic for real errors but nothing for a lost exec, so 125
		// without one means the program MAY STILL BE RUNNING.
		if e.err == nil && e.info.Code == exitHisnError {
			if e.dw.Seen() {
				e.info.BackendError = true
			} else {
				e.info.Lost = true
				e.b.recordLost(e.argv)
			}
		}
	})
	return e.info, e.err
}

// sessionUnreachable probes l's control socket with a client of Wakil's own
// (never program output): it reports true only when `hisn ls --session` fails
// with a refused connection or `hisn ls` no longer lists the session `live`.
// An inconclusive probe (timeout, other error) is NOT proof of death.
func (b *Backend) sessionUnreachable(l *live) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := b.output(ctx, false, "ls", "--session", l.id)
	if err == nil {
		return false
	}
	if isConnRefused(out) {
		return true
	}
	state, serr := b.sessionState(ctx, l.id)
	return serr == nil && state != "live"
}

// exitHisnError is hisn's reserved exit status for its own failures.
const exitHisnError = 125

func (b *Backend) recordLost(argv []string) {
	s := strings.Join(argv, " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	b.lostMu.Lock()
	b.lost = append(b.lost, s)
	b.lostMu.Unlock()
}

// ---- ctl path ----

// ctlLostHandle is a failed ctl launch (ambiguous or refused): Exec succeeds
// so the failure surfaces in Wait exactly like the CLI path's, never as a
// lost start. Wait probes the session: unreachable means the program did
// not run at all (typed session death); reachable means it may still be
// running (Lost, recorded).
type ctlLostHandle struct {
	b    *Backend
	l    *live
	argv []string
	info sandbox.ExitInfo
	dead bool
	once sync.Once
}

func (h *ctlLostHandle) Pid() int    { return 0 }
func (h *ctlLostHandle) Kill() error { return nil }

func (h *ctlLostHandle) Wait() (sandbox.ExitInfo, error) {
	h.once.Do(func() {
		switch {
		case h.info.BackendError:
			// Already decided (no session): a backend error for this exec.
		case h.dead, h.b.sessionUnreachable(h.l):
			h.l.refused.Store(true)
			h.dead = true
			h.info = sandbox.ExitInfo{}
		default:
			h.b.recordLost(h.argv)
		}
	})
	if h.dead {
		return h.info, h.l.deadErr()
	}
	return h.info, nil
}

// execCtl runs one exec through the control-socket client.
func (b *Backend) execCtl(ctx context.Context, l *live, release func(), spec sandbox.ExecSpec, cwd string, env map[string]string) (sandbox.ExecHandle, error) {
	stdio, err := ctl.NewStdio()
	if err != nil {
		release()
		return nil, fmt.Errorf("hisn exec: stdio pipes: %w", err)
	}
	// The exec connection dies with the caller's ctx AND with the session
	// epoch (bindCtx): a session replacement tears it down; hisn then TERMs
	// (then KILLs after 5 s) whatever remains of the program.
	callerCtx := ctx
	ctx, done := bindCtx(ctx, l)
	ec, res, err := l.ctl().StartExec(ctx, spec.Argv, cwd, env, stdio)
	if err != nil {
		done()
		release()
		if errors.Is(err, ctl.ErrAmbiguousLaunch) {
			// The conn dropped before acceptance: the program may have
			// started. A HANDLE, not an error — Wait probes the session
			// (an unreachable session is DEAD, not lost: the program did
			// not survive the sandbox) and otherwise reports Lost,
			// mirroring the CLI path. Never retried.
			return &ctlLostHandle{b: b, l: l, argv: spec.Argv, info: sandbox.ExitInfo{Code: exitHisnError, Lost: true}}, nil
		}
		if errors.Is(err, ctl.ErrSessionDead) {
			// The socket refused: the program did NOT run. A HANDLE — Wait
			// reports the typed session death, mirroring the CLI path
			// (diagWriter + probe), so the failing call and the recorded
			// death behave identically.
			l.refused.Store(true)
			return &ctlLostHandle{b: b, l: l, dead: true}, nil
		}
		if errors.Is(err, ctl.ErrNoSession) {
			// No session at all: hisn's own diagnostic is "no such session".
			// A backend error for this exec (the program did not run), not a
			// session-death claim — the session may never have been ours.
			return &ctlLostHandle{b: b, l: l, info: sandbox.ExitInfo{Code: exitHisnError, BackendError: true}}, nil
		}
		return nil, fmt.Errorf("hisn exec: %w", err)
	}
	_ = res
	eh := &ctlExecHandle{b: b, l: l, ec: ec, argv: spec.Argv, done: done, release: release, callerCtx: callerCtx}
	eh.startCopiers(spec)
	eh.watchCancel(ctx)
	return eh, nil
}

// ctlExecHandle post-processes the ctl exec into a sandbox.ExitInfo.
type ctlExecHandle struct {
	b       *Backend
	l       *live
	ec      *ctl.ExecConn
	argv    []string
	done    func()
	release func()

	once sync.Once
	info sandbox.ExitInfo
	err  error
	wg   sync.WaitGroup
	out  copyGate // serialises the stdout/stderr copiers; tracks their progress

	// callerCtx is the CALLER's context (not the session-bound one, which
	// finish cancels): the drain after the exit never outlives it.
	callerCtx context.Context

	cancelOnce sync.Once
	cancelErr  error
}

// watchCancel ties the accepted exec to ctx (the caller's context AND the
// session instance, see bindCtx): when ctx ends before the program does, the
// exec is stopped through the SAME TERM → grace → KILL sequence Kill uses, so
// a cancelled tool call never leaves the program running or Wait hanging. If
// the sequence cannot obtain the exit line, the exec connection is closed (hisn
// then cancels on its own) and the status latches as lost. The watcher ends
// with the exec.
func (e *ctlExecHandle) watchCancel(ctx context.Context) {
	go func() {
		select {
		case <-e.ec.ExitDone():
			return
		case <-ctx.Done():
		}
		select {
		case <-e.ec.ExitDone(): // finished first; ctx ended only because Wait cleaned up
			return
		default:
		}
		if err := e.cancel(); err != nil {
			e.ec.Close() // last resort: lets Wait return (lost) instead of hanging
		}
	}()
}

// startCopiers wires spec.Stdin → InW and OutR/ErrR → spec.Stdout/Stderr.
func (e *ctlExecHandle) startCopiers(spec sandbox.ExecSpec) {
	s := e.ec.Stdio()
	if spec.Stdin != nil {
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			_, _ = io.Copy(s.InW, spec.Stdin)
			_ = s.InW.Close() // program's stdin EOF
		}()
	} else {
		_ = s.InW.Close()
	}
	var out io.Writer = io.Discard
	if spec.Stdout != nil {
		out = spec.Stdout
	}
	// A nil Stderr discards stderr: the ctl mode passes two distinct fds and
	// cannot merge them itself (BackendExecutor passes the SAME writer for both).
	var errw io.Writer = io.Discard
	if spec.Stderr != nil {
		errw = spec.Stderr
	}
	// Both copiers write through ONE gate: the caller's writer is routinely the
	// same buffer for both streams, and an unsynchronised bytes.Buffer corrupts
	// itself (an idle stderr copier's ReadFrom rewinds the length over what the
	// stdout copier appended — output, and the exit marker at its end, vanish).
	ow, ew := e.out.wrap(out), e.out.wrap(errw)
	e.wg.Add(2)
	go func() { defer e.wg.Done(); _, _ = io.Copy(ow, s.OutR) }()
	go func() { defer e.wg.Done(); _, _ = io.Copy(ew, s.ErrR) }()
}

// Bounds of the drain after the program exited. The exit status is reported
// only once both pipes reached EOF, so no output the program wrote is lost to
// a copier that is still catching up. The wait ends early only when a stray
// child that inherited the pipes holds them open: no progress for drainIdle,
// drainMax in total, or the caller's own context (the tool call's timeout or
// cancellation) — whichever comes first.
const (
	drainIdle = 2 * time.Second
	drainMax  = 30 * time.Second
)

// copyGate serialises writes of several copiers to caller-supplied writers and
// records the last time any of them made progress.
type copyGate struct {
	mu   sync.Mutex
	last atomic.Int64 // unix nanos of the last write start/end
}

func (g *copyGate) wrap(w io.Writer) io.Writer { return &gatedWriter{g: g, w: w} }

type gatedWriter struct {
	g *copyGate
	w io.Writer
}

func (gw *gatedWriter) Write(p []byte) (int, error) {
	gw.g.last.Store(time.Now().UnixNano())
	gw.g.mu.Lock()
	n, err := gw.w.Write(p)
	gw.g.mu.Unlock()
	gw.g.last.Store(time.Now().UnixNano())
	return n, err
}

// drain waits until every copier has reached EOF, bounded as described at
// drainIdle. A large or slowly consumed output is never cut while it makes
// progress (up to drainMax).
func (g *copyGate) drain(wg *sync.WaitGroup, ctx context.Context) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	start := time.Now()
	tick := time.NewTicker(25 * time.Millisecond)
	defer tick.Stop()
	var ctxDone <-chan struct{}
	if ctx != nil {
		ctxDone = ctx.Done()
	}
	for {
		select {
		case <-done:
			return
		case <-ctxDone:
			return
		case <-tick.C:
			if time.Since(start) > drainMax {
				return
			}
			since := start
			if l := time.Unix(0, g.last.Load()); l.After(since) {
				since = l
			}
			if time.Since(since) > drainIdle {
				return
			}
		}
	}
}

func (e *ctlExecHandle) Pid() int    { return e.ec.Pid() }
func (e *ctlExecHandle) Kill() error { return e.cancel() }

// cancel TERMs then KILLs the exec via exec_id, each on its own fresh signal
// connection (hisn does not read the exec connection after the request
// line). The exec connection stays open until the exit line arrives or the
// bounded post-KILL wait (10 s) expires — closing it earlier LOSES the exit
// status. Closing the exec connection is the LAST RESORT (close = hisn
// TERMs then KILLs after 5 s on its own), not part of the normal sequence.
// A signal error is never swallowed: it propagates, carrying hisn's answer.
func (e *ctlExecHandle) cancel() error {
	// Kill and the ctx watcher may both ask: run the sequence once, share the
	// outcome.
	e.cancelOnce.Do(func() { e.cancelErr = e.cancelSeq() })
	return e.cancelErr
}

func (e *ctlExecHandle) cancelSeq() error {
	// Cleanup budget per step: independent of the cancelled caller's deadline.
	sctx, scancel := context.WithTimeout(context.Background(), e.b.stopWait)
	terr := e.ec.Signal(sctx, "TERM")
	scancel()
	if terr == nil {
		wctx, wcancel := context.WithTimeout(context.Background(), 5*time.Second)
		ep, _ := e.ec.WaitExit(wctx)
		wcancel()
		if ep != nil {
			return nil // TERMed in time: the exit line arrived
		}
	}
	// TERM failed or was ignored (trap): KILL on a fresh conn. A failed TERM
	// does not preclude a KILL — different connection, same exec_id.
	kctx, kcancel := context.WithTimeout(context.Background(), e.b.stopWait)
	kerr := e.ec.Signal(kctx, "KILL")
	kcancel()
	if kerr != nil {
		// Last resort: close the exec conn — hisn TERMs then KILLs after 5 s
		// on its own — and keep waiting for the exit line.
		e.ec.Close()
		wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
		ep, _ := e.ec.WaitExit(wctx)
		wcancel()
		if ep != nil {
			return nil
		}
		return fmt.Errorf("hisn exec kill: %s did not exit after KILL (TERM: %w; KILL: %w)",
			strings.Join(e.argv, " "), terr, kerr)
	}
	// KILL accepted: the exit line must follow within the bounded wait.
	wctx, wcancel := context.WithTimeout(context.Background(), 10*time.Second)
	ep, _ := e.ec.WaitExit(wctx)
	wcancel()
	if ep != nil {
		return nil
	}
	return fmt.Errorf("hisn exec kill: %s did not exit after KILL (hisn accepted TERM=%v, KILL=%v, but no exit line arrived)",
		strings.Join(e.argv, " "), terr == nil, kerr == nil)
}

func (e *ctlExecHandle) Wait() (sandbox.ExitInfo, error) {
	e.once.Do(func() {
		ep, werr := e.ec.WaitExit(context.Background())
		e.info, e.err = e.translate(ep, werr)
		e.finish()
	})
	return e.info, e.err
}

func (e *ctlExecHandle) finish() {
	// Close only the connection: closing a kept read end would DISCARD output
	// still buffered in its pipe (the exit line routinely arrives before the
	// copiers have read the last of it).
	e.ec.CloseConn()
	e.done()
	e.release()
	e.out.drain(&e.wg, e.callerCtx)
	e.ec.Close()
}

// translate maps the exit payload into the ExitInfo contract.
func (e *ctlExecHandle) translate(ep *ctl.ExitPayload, werr error) (sandbox.ExitInfo, error) {
	if werr != nil { // WaitExit never errors with Background ctx; defensive
		e.info = sandbox.ExitInfo{Code: exitHisnError, BackendError: true}
		return e.info, nil
	}
	if ep == nil || ep.Lost {
		// The status is unknown: the conn dropped without an exit line. The
		// program MAY still be running — but first check whether the whole
		// session died (supervisor exit races the EOF by microseconds): a
		// program of a dead sandbox is over, not "lost". Only Wakil's own
		// probe decides, never the EOF alone.
		if e.l.dead() || e.b.sessionUnreachable(e.l) {
			e.l.refused.Store(true)
			e.info = sandbox.ExitInfo{}
			return e.info, e.l.deadErr()
		}
		e.info = sandbox.ExitInfo{Code: exitHisnError, Lost: true}
		e.b.recordLost(e.argv)
		return e.info, nil
	}
	if ep.Code != nil {
		return sandbox.ExitInfo{Code: *ep.Code}, nil
	}
	if ep.Signal != nil {
		if n := signalNumber(*ep.Signal); n > 0 {
			return sandbox.ExitInfo{Code: 128 + n}, nil
		}
	}
	e.info = sandbox.ExitInfo{Code: exitHisnError, Lost: true}
	e.b.recordLost(e.argv)
	return e.info, nil
}

func signalNumber(name string) int {
	switch strings.ToUpper(name) {
	case "TERM":
		return 15
	case "KILL":
		return 9
	case "INT":
		return 2
	case "HUP":
		return 1
	}
	return 0
}
