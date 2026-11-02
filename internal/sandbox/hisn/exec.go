package hisn

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/treeol/wakil/internal/sandbox"
)

// Exec implements sandbox.Backend: `hisn exec --session ID [--cwd DIR]
// [--env K=V]... -- argv`, stdio piped, never a tty.
func (b *Backend) Exec(ctx context.Context, spec sandbox.ExecSpec) (sandbox.ExecHandle, error) {
	if len(spec.Argv) == 0 {
		return nil, fmt.Errorf("hisn exec: empty argv")
	}
	args := []string{"exec", "--session", ""}
	if spec.Cwd != "" {
		cwd, err := b.pm.ResolveExecCwd(spec.Cwd)
		if err != nil {
			return nil, err
		}
		args = append(args, "--cwd", cwd)
	}
	// Per-exec env is allowlisted: only env_pass names, never computed ones.
	allowed := map[string]bool{}
	for _, n := range b.opts.EnvPass {
		allowed[n] = true
	}
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		if !allowed[k] || isComputedEnv(k) {
			return nil, fmt.Errorf("hisn exec: env %q is not allowed (not in sandbox.hisn.env_pass or computed by hisn)", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--env", k+"="+spec.Env[k])
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
	args[2] = l.id
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
