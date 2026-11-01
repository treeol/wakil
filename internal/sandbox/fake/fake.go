// Package fake is an in-memory sandbox.Backend for tests of the tool layer.
// It never touches the host: execs are served by a user-supplied function.
package fake

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/treeol/wakil/internal/sandbox"
)

// RunFunc handles one exec. It writes to spec.Stdout/Stderr and returns the exit code.
type RunFunc func(spec sandbox.ExecSpec) int

// Backend is a recording fake.
type Backend struct {
	*sandbox.PathMap
	mu       sync.Mutex
	Run      RunFunc
	Started  []sandbox.Session
	Stopped  int
	Grants   map[string]bool // path -> rw
	Revoked  []string
	Execs    []sandbox.ExecSpec
	GrantErr error
	live     bool
	nextPid  int
}

// New returns a fake with the given roots; Private defaults to /tmp.
func New(hostRoot, agentRoot string) *Backend {
	return &Backend{
		PathMap: sandbox.NewPathMap(hostRoot, agentRoot, "/tmp"),
		Grants:  map[string]bool{},
		Run:     func(sandbox.ExecSpec) int { return 0 },
	}
}

var _ sandbox.Backend = (*Backend)(nil)

func (b *Backend) Start(_ context.Context, s sandbox.Session) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.live {
		return sandbox.ErrSessionInUse
	}
	b.live = true
	b.Started = append(b.Started, s)
	return nil
}

type handle struct {
	done chan sandbox.ExitInfo
	pid  int
}

func (h *handle) Wait() (sandbox.ExitInfo, error) { return <-h.done, nil }
func (h *handle) Kill() error                     { return nil }
func (h *handle) Pid() int                        { return h.pid }

func (b *Backend) Exec(_ context.Context, spec sandbox.ExecSpec) (sandbox.ExecHandle, error) {
	b.mu.Lock()
	if !b.live {
		b.mu.Unlock()
		return nil, sandbox.ErrNoSession
	}
	b.Execs = append(b.Execs, spec)
	b.nextPid++
	pid := 1000 + b.nextPid
	run := b.Run
	b.mu.Unlock()
	if spec.Cwd != "" {
		if _, err := b.ResolveExecCwd(spec.Cwd); err != nil {
			return nil, err
		}
	}
	h := &handle{done: make(chan sandbox.ExitInfo, 1), pid: pid}
	code := run(spec)
	h.done <- sandbox.ExitInfo{Code: code}
	return h, nil
}

func (b *Backend) Grant(_ context.Context, path string, rw bool) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.GrantErr != nil {
		return b.GrantErr
	}
	b.Grants[path] = rw
	b.AddGrant(path)
	return nil
}

func (b *Backend) Revoke(_ context.Context, path string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.Grants[path]; !ok {
		return errors.New("fake: not granted")
	}
	delete(b.Grants, path)
	b.Revoked = append(b.Revoked, path)
	b.RemoveGrant(path)
	return nil
}

func (b *Backend) Stop(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.live {
		b.Stopped++
	}
	b.live = false
	b.ResetGrants()
	b.Grants = map[string]bool{}
	return nil
}

func (b *Backend) Close(ctx context.Context) error { return b.Stop(ctx) }
func (b *Backend) AgentRoot() string               { return b.PathMap.AgentRoot }
func (b *Backend) SandboxTempDir() string          { return "/tmp" }

// Live reports whether a session is running.
func (b *Backend) Live() bool { b.mu.Lock(); defer b.mu.Unlock(); return b.live }

// Echo is a RunFunc that writes its argv to stdout and exits 0.
func Echo(spec sandbox.ExecSpec) int {
	for i, a := range spec.Argv {
		if i > 0 {
			_, _ = io.WriteString(spec.Stdout, " ")
		}
		_, _ = io.WriteString(spec.Stdout, a)
	}
	return 0
}
