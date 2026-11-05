package exec

import (
	"context"
	"os/exec"
	"sync"
	"time"
)

// UserShellProber is an optional Executor capability: which shell the
// run_shell / run_background tools should use for the model's own commands.
// Helper scripts the executors build themselves always stay `sh`; only user
// commands move to bash, and only when the sandbox has it.
//
// UserShell returns "bash" when bash exists in the sandbox, otherwise "sh". The
// probe runs lazily on first use (never at construction, so a lazily started
// sandbox is not started early) and is cached per executor generation.
type UserShellProber interface {
	UserShell(ctx context.Context) string
	// PeekUserShell returns the cached answer without probing: "" until the
	// first UserShell call for the current generation. Used to word the tool
	// description without starting a lazily started sandbox early.
	PeekUserShell() string
}

// userShellCache caches one probe result per executor generation.
type userShellCache struct {
	mu  sync.Mutex
	gen int
	val string
}

// get returns the cached shell for generation gen, probing with probe on a miss
// or when the generation changed (e.g. the container was recreated).
func (c *userShellCache) get(gen int, probe func() bool) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.val != "" && c.gen == gen {
		return c.val
	}
	c.val = "sh"
	if probe() {
		c.val = "bash"
	}
	c.gen = gen
	return c.val
}

// peek returns the cached value for generation gen, or "" if none.
func (c *userShellCache) peek(gen int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen == gen {
		return c.val
	}
	return ""
}

const userShellProbeCmd = "command -v bash >/dev/null 2>&1"

// UserShell implements UserShellProber.
func (d *DockerExecutor) UserShell(ctx context.Context) string {
	return d.userShell.get(d.Generation(), func() bool {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, err := d.execCtx(pctx, false, "sh", "-c", userShellProbeCmd)
		return err == nil
	})
}

// PeekUserShell implements UserShellProber.
func (d *DockerExecutor) PeekUserShell() string { return d.userShell.peek(d.Generation()) }

// PeekUserShell implements UserShellProber.
func (e *DirectExecutor) PeekUserShell() string { return e.userShell.peek(e.Generation()) }

// PeekUserShell implements UserShellProber.
func (e *BackendExecutor) PeekUserShell() string { return e.userShell.peek(e.Generation()) }

// UserShell implements UserShellProber.
func (e *DirectExecutor) UserShell(_ context.Context) string {
	return e.userShell.get(e.Generation(), func() bool {
		_, err := exec.LookPath("bash")
		return err == nil
	})
}

// UserShell implements UserShellProber.
func (e *BackendExecutor) UserShell(ctx context.Context) string {
	return e.userShell.get(e.Generation(), func() bool {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_, err := e.sh(pctx, userShellProbeCmd)
		return err == nil
	})
}

// BashCommand wraps command so a POSIX sh parses only the outer quoted string
// and bash parses the command itself: `bash -c '<command>'`. The wrapper that
// WrapExitMarker adds around it therefore stays valid under sh whatever bash
// syntax the command uses, and an `exit N` inside the command ends the bash
// child while the wrapper still records N. A syntax error is reported by bash
// (exit 2) instead of failing the outer parse.
func BashCommand(command string) string {
	return "bash -c " + shQuote(command)
}

// UserCommand returns the command string to hand to RunShell / WrapExitMarker
// for a model-supplied command: bash-wrapped when e prefers bash, unchanged
// otherwise. Executors that do not implement UserShellProber (test doubles) get
// the command unchanged.
func UserCommand(ctx context.Context, e Executor, command string) string {
	if p, ok := e.(UserShellProber); ok && p.UserShell(ctx) == "bash" {
		return BashCommand(command)
	}
	return command
}
