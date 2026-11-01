package exec

import (
	"errors"
	"os"
)

var errNestedWorktree = errors.New("nested worktree isolation is not allowed")

// WorktreeHost for DockerExecutor: worktrees live in the container's /tmp.

func (d *DockerExecutor) WorktreeKind() WorktreeKind { return WorktreeSandboxTmp }
func (d *DockerExecutor) IsWorktreeChild() bool      { return false }
func (d *DockerExecutor) SandboxTempDir() string     { return "/tmp" }
func (d *DockerExecutor) NewWorktreeExecutor(dir string) (Executor, error) {
	return NewDockerWorktreeExecutor(d, dir), nil
}

// WorktreeHost for DirectExecutor: worktrees live on the host filesystem.

func (e *DirectExecutor) WorktreeKind() WorktreeKind { return WorktreeHostFS }
func (e *DirectExecutor) IsWorktreeChild() bool      { return false }
func (e *DirectExecutor) SandboxTempDir() string     { return osTempDir() }
func (e *DirectExecutor) NewWorktreeExecutor(dir string) (Executor, error) {
	return NewDirectExecutor(dir)
}

func osTempDir() string { return os.TempDir() }
