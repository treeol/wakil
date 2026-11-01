package exec

import (
	"context"
	osexec "os/exec"

	"github.com/treeol/wakil/internal/sandbox"
)

// dockerBackend adapts a running DockerExecutor to sandbox.Backend. The
// container is started by NewDockerExecutor, so Start is a no-op and mounts
// cannot change afterwards (Grant/Revoke are unsupported). The tool layer keeps
// using the DockerExecutor directly; this adapter exists so docker satisfies
// the same lower-level contract as other backends.
type dockerBackend struct {
	d  *DockerExecutor
	pm *sandbox.PathMap // nil when no host directory is mounted
}

// Backend returns the sandbox.Backend view of the executor.
func (d *DockerExecutor) Backend() sandbox.Backend {
	b := &dockerBackend{d: d}
	if d.hostMount != "" {
		b.pm = sandbox.NewPathMap(d.hostMount, d.workspaceRoot, "/tmp")
	}
	return b
}

var _ sandbox.Backend = (*dockerBackend)(nil)

func (b *dockerBackend) Start(context.Context, sandbox.Session) error { return nil }

func (b *dockerBackend) Exec(ctx context.Context, spec sandbox.ExecSpec) (sandbox.ExecHandle, error) {
	args := []string{"exec"}
	if spec.Stdin != nil {
		args = append(args, "-i")
	}
	if spec.Cwd != "" {
		args = append(args, "-w", spec.Cwd)
	}
	for k, v := range spec.Env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, b.d.container)
	args = append(args, spec.Argv...)
	cmd := osexec.CommandContext(ctx, "docker", args...)
	cmd.Stdin = spec.Stdin
	cmd.Stdout = spec.Stdout
	cmd.Stderr = spec.Stderr
	if cmd.Stderr == nil {
		cmd.Stderr = spec.Stdout
	}
	return sandbox.StartCmd(cmd)
}

func (b *dockerBackend) Grant(context.Context, string, bool) error { return sandbox.ErrNotSupported }
func (b *dockerBackend) Revoke(context.Context, string) error      { return sandbox.ErrNotSupported }
func (b *dockerBackend) Stop(context.Context) error                { return b.d.Close() }
func (b *dockerBackend) Close(ctx context.Context) error           { return b.Stop(ctx) }
func (b *dockerBackend) AgentRoot() string                         { return b.d.workspaceRoot }
func (b *dockerBackend) SandboxTempDir() string                    { return "/tmp" }

func (b *dockerBackend) MapPath(agentPath string) (string, error) {
	if b.pm == nil {
		return "", sandbox.ErrNoHostPath
	}
	return b.pm.MapPath(agentPath)
}

func (b *dockerBackend) UnmapPath(hostPath string) (string, error) {
	if b.pm == nil {
		return "", sandbox.ErrNoHostPath
	}
	return b.pm.UnmapPath(hostPath)
}
