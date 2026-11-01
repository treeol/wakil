// Package sandbox defines the backend-neutral contract between Wakil's tool
// layer and a concrete sandbox implementation (hisn, docker, ...). It is a
// leaf package: it imports nothing from Wakil so every layer can depend on it.
package sandbox

import (
	"context"
	"errors"
	"io"
)

// Sentinel errors shared by all backends.
var (
	// ErrNotSupported is returned by operations a backend cannot perform
	// (e.g. dynamic grants on docker, whose mounts are fixed at start).
	ErrNotSupported = errors.New("sandbox: operation not supported by this backend")
	// ErrNoSession is returned when an operation needs a started session.
	ErrNoSession = errors.New("sandbox: no active session")
	// ErrSessionDead is returned when the session existed but its supervisor
	// has exited (crash, OOM, external kill). The session is gone for good: the
	// caller must start a fresh one before the next tool call.
	ErrSessionDead = errors.New("sandbox: session is dead")
	// ErrNoHostPath is returned by MapPath for a path that is visible inside
	// the sandbox but has no host representation (e.g. the private /tmp).
	ErrNoHostPath = errors.New("sandbox: path has no host representation")
	// ErrNotVisible is returned for a path the sandbox cannot see at all.
	ErrNotVisible = errors.New("sandbox: path is not visible inside the sandbox")
	// ErrNotGrantable is returned when a read-write grant is requested outside
	// every configured grantable prefix. Callers must surface it to the user;
	// they must not retry with a different path or widen the policy.
	ErrNotGrantable = errors.New("sandbox: path is not grantable read-write (outside sandbox.hisn.grantable_rw)")
	// ErrPathForbidden is returned when the backend refuses a path outright.
	ErrPathForbidden = errors.New("sandbox: path forbidden by the sandbox")
	// ErrSessionInUse is returned when the session id is already taken by a
	// session Wakil did not create, or by another live Wakil process.
	ErrSessionInUse = errors.New("sandbox: session id already in use")
)

// Session identifies the sandbox session a Backend manages. One session
// belongs to one Wakil chat; ID is the chat ID.
type Session struct {
	ID      string // chat ID
	HostCwd string // absolute, symlink-free host workspace directory
}

// ExecSpec describes one command to run inside the session. Stdio is always
// piped, never a tty.
type ExecSpec struct {
	Argv []string
	// Cwd is an AGENT-side path. It may be host-representable (workspace,
	// grants) or sandbox-private (/tmp); the backend decides which.
	Cwd string
	// Env carries per-exec variables. Only names the backend's allowlist
	// accepts are permitted; anything else rejects the whole call.
	Env   map[string]string
	Stdin io.Reader
	// Stdout receives the program's stdout. A nil Stderr merges stderr into
	// Stdout over a single pipe, preserving interleaving.
	Stdout io.Writer
	Stderr io.Writer
}

// ExitInfo is the outcome of one exec.
type ExitInfo struct {
	// Code is the program's exit status (128+sig if signalled). Meaningful
	// only when neither Lost nor BackendError is set.
	Code int
	// Lost means the supervisor lost track of the program: it MAY STILL BE
	// RUNNING inside the session. Callers must not treat it as an exit.
	Lost bool
	// BackendError means the sandbox tooling itself failed (policy, session
	// or confinement problem), not the program.
	BackendError bool
}

// ExecHandle is a started exec.
type ExecHandle interface {
	// Wait blocks until the exec finishes.
	Wait() (ExitInfo, error)
	// Kill asks the program to stop (TERM, then KILL after a grace period).
	Kill() error
	// Pid is the pid of the HOST-side client process driving the exec. It is
	// not a pid inside the sandbox.
	Pid() int
}

// Backend is one sandbox implementation. A Backend manages at most one
// session at a time and is restartable: Start may be called again after Stop
// (with the same or another session). Implementations must be safe for
// concurrent Exec calls.
type Backend interface {
	// Start brings up the session. It must not adopt or disturb a session it
	// did not create.
	Start(ctx context.Context, s Session) error
	// Exec starts a command in the running session.
	Exec(ctx context.Context, spec ExecSpec) (ExecHandle, error)
	// Grant makes a host path visible inside the session (read-only unless rw).
	Grant(ctx context.Context, path string, rw bool) error
	// Revoke withdraws a grant (soft: the path stops resolving; already-open
	// descriptors may survive).
	Revoke(ctx context.Context, path string) error
	// Stop ends the session and everything inside it. Failure must leave the
	// recovery information needed to stop it later.
	Stop(ctx context.Context) error
	// MapPath translates an AGENT path to a HOST path. Returns ErrNoHostPath
	// for sandbox-private paths and ErrNotVisible for invisible ones.
	MapPath(agentPath string) (hostPath string, err error)
	// UnmapPath is the inverse of MapPath.
	UnmapPath(hostPath string) (agentPath string, err error)
	// AgentRoot is the workspace root as the agent sees it.
	AgentRoot() string
	// SandboxTempDir is the sandbox-private temp directory (never a host path).
	SandboxTempDir() string
	// Close releases the backend for good (retrying any failed stop).
	Close(ctx context.Context) error
}
