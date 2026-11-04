package exec

import (
	"context"
	"strconv"

	"github.com/treeol/wakil/internal/sandbox"
)

// The interfaces below are OPTIONAL executor capabilities. The tool layer
// type-asserts them instead of switching on concrete executor types, so it
// never learns which sandbox backend is running.

// WorktreeKind says where an executor's worktree temp directories live.
type WorktreeKind int

const (
	// WorktreeHostFS: temp dirs are on the host filesystem (direct mode); the
	// host may use os.* on them.
	WorktreeHostFS WorktreeKind = iota
	// WorktreeSandboxTmp: temp dirs live in the sandbox-private /tmp. They have
	// no host representation; every operation must go through RunShell.
	WorktreeSandboxTmp
)

// WorktreeHost is implemented by executors that can host isolated git
// worktrees for edit-tier subagents.
type WorktreeHost interface {
	WorktreeKind() WorktreeKind
	// IsWorktreeChild is true for an executor already re-rooted at a worktree;
	// nested worktree dispatch from it is refused.
	IsWorktreeChild() bool
	// SandboxTempDir is the temp directory in the executor's own namespace.
	SandboxTempDir() string
	// NewWorktreeExecutor returns an executor re-rooted at dir (an
	// executor-visible path). It shares the parent's sandbox; closing it is a
	// no-op for shared sandboxes.
	NewWorktreeExecutor(dir string) (Executor, error)
}

// ChatBinder is implemented by executors whose sandbox session belongs to a
// chat. BindChat is called whenever the main conversation's chat ID changes
// (new, resume, handoff); binding a different ID ends the previous chat's
// session. Subagents never bind: they share the parent's session.
type ChatBinder interface {
	BindChat(ctx context.Context, chatID string) error
}

// SandboxResetter is implemented by executors that can restart their sandbox
// session in place (used by /rewind). Everything running inside is killed and
// Generation() advances.
type SandboxResetter interface {
	ResetSandbox(ctx context.Context) error
}

// SandboxStatuser is implemented by executors that can describe their sandbox
// (profile, risk label, capabilities, services, grants) for the startup banner
// and /sandbox.
type SandboxStatuser interface {
	SandboxStatus(ctx context.Context) sandbox.Status
}

// AccessGranter is implemented by executors that can make host paths outside
// the workspace visible inside the sandbox (hisn grants). Docker mounts are
// fixed at start and do not implement it.
type AccessGranter interface {
	EnsureAccess(ctx context.Context, path string, tier sandbox.Tier) error
	WithdrawAccess(ctx context.Context, path string) error
}

// ExitStatusError is the error returned for a non-zero exit by executors that
// do not run a host os/exec child per call. Its text matches os/exec's
// "exit status N" so textual fallbacks keep working.
type ExitStatusError struct{ Code int }

func (e *ExitStatusError) Error() string { return "exit status " + strconv.Itoa(e.Code) }

// ExitCode lets errors.As(err, &interface{ExitCode() int}) see the status.
func (e *ExitStatusError) ExitCode() int { return e.Code }
