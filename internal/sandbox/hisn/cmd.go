// Package hisn implements sandbox.Backend on top of the hisn CLI
// (github.com/treeol/hisn, >= 0.2.0). Wakil drives it exclusively through the
// command line: one detached session per chat, many `hisn exec` calls.
package hisn

import (
	"context"
	"os"
	osexec "os/exec"
	"strings"
)

// clientEnvAllow lists the variables a hisn CLI process may inherit from
// Wakil. Everything else (API keys, tokens, the rest of the host environment)
// is dropped: hisn scrubs again on its side, but Wakil must not rely on that.
var clientEnvAllow = map[string]bool{
	"PATH": true, "HOME": true, "XDG_RUNTIME_DIR": true,
	"LANG": true, "TZ": true, "TERM": true,
}

func clientEnvAllowed(name string) bool {
	return clientEnvAllow[name] || strings.HasPrefix(name, "LC_")
}

// scrubbedEnv builds the environment for a hisn CLI process from base
// (KEY=VALUE entries). When pass is non-nil the named variables are copied
// too — only `hisn run` needs them, because hisn snapshots its environment at
// session start.
func scrubbedEnv(base []string, pass []string) []string {
	want := map[string]bool{}
	for _, n := range pass {
		want[n] = true
	}
	var out []string
	for _, kv := range base {
		name, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if clientEnvAllowed(name) || want[name] {
			out = append(out, kv)
		}
	}
	return out
}

// command is THE factory for every hisn invocation (version, check, policy
// validate, run, exec, grant, revoke, ls, kill, inspect). Nothing else in the
// package may call exec.Command, so no hisn process can inherit the host
// environment by accident (a test enforces this).
func (b *Backend) command(ctx context.Context, withPass bool, args ...string) *osexec.Cmd {
	cmd := osexec.CommandContext(ctx, b.opts.Binary, args...)
	var pass []string
	if withPass {
		pass = b.opts.EnvPass
	}
	cmd.Env = scrubbedEnv(b.baseEnv(), pass)
	return cmd
}

func (b *Backend) baseEnv() []string {
	if b.opts.Env != nil {
		return b.opts.Env
	}
	return os.Environ()
}

// output runs a hisn command to completion and returns its combined output.
func (b *Backend) output(ctx context.Context, withPass bool, args ...string) (string, error) {
	out, err := b.command(ctx, withPass, args...).CombinedOutput()
	return string(out), err
}
