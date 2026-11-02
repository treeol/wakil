package hisn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/treeol/wakil/internal/sandbox"
	"github.com/treeol/wakil/internal/sandbox/hisn/ctl"
)

// Exec implements sandbox.Backend. Two paths share one validation:
//   - ctl (default): the control-socket client (internal/sandbox/hisn/ctl);
//     exec_id-signalled, one connection per exec, stdio over SCM_RIGHTS.
//   - CLI (sandbox.hisn.exec_via_cli = true): `hisn exec --session ID …`,
//     one client process per call — kept one release as a fallback.
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

// execCLI is the fallback path: one `hisn exec` client process per call.
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
	sw := &syncWriter{w: out}
	dw := &diagWriter{w: sw}
	cmd := b.command(cctx, false, args...)
	cmd.Stdin = spec.Stdin
	cmd.Stdout = sw
	cmd.Stderr = dw
	if spec.Stderr != nil {
		cmd.Stderr = &diagWriter{w: &syncWriter{w: spec.Stderr}}
		dw = cmd.Stderr.(*diagWriter)
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

// DefaultStateDir returns <base>/hisn for owner records and policies.
func DefaultStateDir(base string) string { return filepath.Join(base, "hisn") }

// ---- ctl path ----

// ctlLostHandle is a failed ctl launch (ambiguous or refused): Exec succeeds
// so the failure surfaces in Wait exactly like the CLI path's, never as a
// lost start. Wait probes the session: unreachable means the program did
// // not run at all (typed session death); reachable means it may still be
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
	eh := &ctlExecHandle{b: b, l: l, ec: ec, argv: spec.Argv, done: done, release: release}
	eh.startCopiers(spec)
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
	var errw io.Writer = io.Discard // nil Stderr merges stderr into stdout? No —
	// ExitInfo contract: nil Stderr means merged via one pipe in CLI mode;
	// the ctl mode cannot merge (two distinct fds passed), so a nil Stderr
	// discards stderr, exactly like a nil Stdout would.
	if spec.Stderr != nil {
		errw = spec.Stderr
	}
	e.wg.Add(2)
	go func() { defer e.wg.Done(); _, _ = io.Copy(out, s.OutR) }()
	go func() { defer e.wg.Done(); _, _ = io.Copy(errw, s.ErrR) }()
}

func (e *ctlExecHandle) Pid() int    { return e.ec.Pid() }
func (e *ctlExecHandle) Kill() error { return e.cancel() }

// cancel TERMs then KILLs the exec via exec_id on its own connection.
func (e *ctlExecHandle) cancel() error {
	if err := e.ec.Signal("TERM"); err != nil {
		return err
	}
	// Bounded grace: cleanup budget independent of the caller's deadline.
	cctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ep, _ := e.ec.WaitExit(cctx)
	if ep != nil {
		return nil // TERMed in time
	}
	if err := e.ec.Signal("KILL"); err != nil {
		return err
	}
	cctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	ep, _ = e.ec.WaitExit(cctx2)
	if ep == nil {
		return fmt.Errorf("hisn exec kill: %s did not exit after KILL", strings.Join(e.argv, " "))
	}
	return nil
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
	e.ec.Close()
	e.done()
	e.release()
	// Bounded drain: a background child holding stdout must not hang Wait.
	waitTimeout(&e.wg, 2*time.Second)
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
