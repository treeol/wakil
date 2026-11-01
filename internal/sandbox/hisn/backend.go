package hisn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/treeol/wakil/internal/sandbox"
)

// MinVersion is the oldest supported hisn release.
const MinVersion = "0.2.0"

// computedEnv names hisn always sets itself inside the sandbox; an exec may
// not override them (hisn rejects the whole request if it tries).
var computedEnv = map[string]bool{
	"HOME": true, "CARGO_HOME": true, "GOMODCACHE": true, "GOCACHE": true,
	"npm_config_cache": true, "PIP_CACHE_DIR": true,
}

func isComputedEnv(name string) bool {
	return computedEnv[name] || strings.HasPrefix(name, "XDG_")
}

// Options configures a Backend.
type Options struct {
	Binary      string   // hisn executable; default "hisn"
	MinVersion  string   // default MinVersion
	StateDir    string   // owner records, policies, lock (required)
	HomeRoot    string   // persistent sandbox $HOME on the host (required)
	HostCwd     string   // absolute, symlink-free workspace (required)
	GrantableRW []string // prefixes allowed for rw grants
	EnvPass     []string // names passed into the sandbox
	Env         []string // base environment for hisn clients; nil = os.Environ()
}

// live is one running session instance.
type live struct {
	id     string
	token  string
	ctx    context.Context // cancelled when the session is being replaced/stopped
	cancel context.CancelFunc
	wg     sync.WaitGroup // in-flight hisn exec clients
}

// Backend drives hisn. Lifecycle transitions (Start/Stop/Close) take mu for
// writing; Exec/Grant/Revoke take it for reading only to ADMIT and capture the
// current session, so a stale call can never reach a replacement session.
type Backend struct {
	opts Options
	pm   *sandbox.PathMap

	mu      sync.RWMutex
	cur     *live
	orphans []*live // sessions whose stop failed; retried, never forgotten
	closed  bool
	epoch   uint64

	lostMu sync.Mutex
	lost   []string // argv summaries of execs whose status was lost
}

var _ sandbox.Backend = (*Backend)(nil)

// New builds a Backend without touching hisn. Use Open for the full startup
// (preflight + orphan reconcile).
func New(opts Options) (*Backend, error) {
	if opts.Binary == "" {
		opts.Binary = "hisn"
	}
	if opts.MinVersion == "" {
		opts.MinVersion = MinVersion
	}
	if opts.StateDir == "" || opts.HomeRoot == "" {
		return nil, errors.New("hisn backend: StateDir and HomeRoot are required")
	}
	if !filepath.IsAbs(opts.HostCwd) {
		return nil, fmt.Errorf("hisn backend: HostCwd %q must be absolute", opts.HostCwd)
	}
	opts.HostCwd = filepath.Clean(opts.HostCwd)
	return &Backend{
		opts: opts,
		// hisn mounts cwd at the identical path and gives the sandbox a
		// private /tmp: host /tmp is NOT the sandbox /tmp.
		pm: sandbox.NewPathMap(opts.HostCwd, opts.HostCwd, "/tmp"),
	}, nil
}

// Open is New plus the mandatory startup checks: preflight (version + host
// capability) and reconciliation of orphaned sessions. A failed preflight is
// an error; there is never a fallback.
func Open(ctx context.Context, opts Options) (*Backend, error) {
	b, err := New(opts)
	if err != nil {
		return nil, err
	}
	if err := b.Preflight(ctx); err != nil {
		return nil, err
	}
	if err := b.Reconcile(ctx); err != nil {
		// Reconcile problems are reported, never fatal: they must not block a
		// working session.
		fmt.Fprintf(errWriter, "hisn: orphan reconcile: %v\n", err)
	}
	return b, nil
}

// errWriter receives non-fatal diagnostics (replaced in tests).
var errWriter io.Writer = discard{}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// SetDiagnostics redirects non-fatal diagnostics (Wakil passes os.Stderr).
func SetDiagnostics(w io.Writer) { errWriter = w }

// Epoch advances on every session replacement.
func (b *Backend) Epoch() uint64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.epoch
}

// ActiveID returns the id of the running session ("" if none).
func (b *Backend) ActiveID() string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.cur == nil {
		return ""
	}
	return b.cur.id
}

// LostExecs returns summaries of execs whose exit status was lost; the
// programs may still be running until the session stops.
func (b *Backend) LostExecs() []string {
	b.lostMu.Lock()
	defer b.lostMu.Unlock()
	return append([]string(nil), b.lost...)
}

// Start implements sandbox.Backend. It is idempotent for the running session
// and replaces it when asked for a different one.
func (b *Backend) Start(ctx context.Context, s sandbox.Session) error {
	if s.ID == "" {
		return errors.New("hisn backend: empty session id")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return errors.New("hisn backend: closed")
	}
	if b.cur != nil && b.cur.id == s.ID && len(b.orphans) == 0 {
		return nil
	}
	if err := b.stopAllLocked(ctx); err != nil {
		return fmt.Errorf("hisn backend: previous session not stopped: %w", err)
	}
	hostCwd := s.HostCwd
	if hostCwd == "" {
		hostCwd = b.opts.HostCwd
	}
	var l *live
	err := b.withLock(func() (err error) {
		l, err = b.startLocked(ctx, sandbox.Session{ID: s.ID, HostCwd: hostCwd})
		return err
	})
	if err != nil {
		return err
	}
	b.cur = l
	b.epoch++
	b.pm.ResetGrants()
	return nil
}

// stopAllLocked stops the current session and retries previously failed
// stops. Caller holds b.mu for writing.
func (b *Backend) stopAllLocked(ctx context.Context) error {
	var errs []error
	if b.cur != nil {
		b.orphans = append(b.orphans, b.cur)
		b.cur = nil
		b.epoch++
		b.pm.ResetGrants()
	}
	var still []*live
	for _, l := range b.orphans {
		l.cancel() // cancels every in-flight hisn exec client of this session
		waitTimeout(&l.wg, 10*time.Second)
		err := b.withLock(func() error { return b.stopLocked(ctx, l) })
		if err != nil {
			errs = append(errs, err)
			still = append(still, l)
		}
	}
	b.orphans = still
	return errors.Join(errs...)
}

func waitTimeout(wg *sync.WaitGroup, d time.Duration) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
	}
}

// Stop implements sandbox.Backend: kills the session and everything in it.
// On failure the session stays tracked for a later retry (Close, Start).
func (b *Backend) Stop(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.stopAllLocked(ctx)
}

// Close implements sandbox.Backend.
func (b *Backend) Close(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return b.stopAllLocked(ctx)
}

// admit captures the current session for one operation. The caller must call
// the returned release when the operation finishes.
func (b *Backend) admit() (*live, func(), error) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.closed || b.cur == nil {
		return nil, nil, sandbox.ErrNoSession
	}
	l := b.cur
	l.wg.Add(1)
	return l, l.wg.Done, nil
}

// MapPath implements sandbox.Backend.
func (b *Backend) MapPath(agentPath string) (string, error)  { return b.pm.MapPath(agentPath) }
func (b *Backend) UnmapPath(hostPath string) (string, error) { return b.pm.UnmapPath(hostPath) }

// AgentRoot implements sandbox.Backend (identical to the host path).
func (b *Backend) AgentRoot() string { return b.opts.HostCwd }

// SandboxTempDir implements sandbox.Backend: the session-private /tmp.
func (b *Backend) SandboxTempDir() string { return "/tmp" }

// Grant implements sandbox.Backend.
func (b *Backend) Grant(ctx context.Context, path string, rw bool) error {
	l, release, err := b.admit()
	if err != nil {
		return err
	}
	defer release()
	if !filepath.IsAbs(path) {
		return fmt.Errorf("hisn grant: path %q is not absolute", path)
	}
	args := []string{"grant", "--session", l.id}
	if rw {
		args = append(args, "--rw")
	}
	args = append(args, "--", path)
	out, err := b.output(ctx, false, args...)
	if err != nil {
		return classifyControlError("grant", path, out, err)
	}
	b.pm.AddGrant(path)
	return nil
}

// Revoke implements sandbox.Backend with a SOFT revoke: the path stops
// resolving but already-open descriptors may survive. See the design note.
func (b *Backend) Revoke(ctx context.Context, path string) error {
	l, release, err := b.admit()
	if err != nil {
		return err
	}
	defer release()
	out, err := b.output(ctx, false, "revoke", "--session", l.id, "--", path)
	if err != nil {
		return classifyControlError("revoke", path, out, err)
	}
	b.pm.RemoveGrant(path)
	return nil
}

func classifyControlError(op, path, out string, err error) error {
	msg := strings.TrimSpace(out)
	switch {
	case strings.Contains(msg, "E_NOT_GRANTABLE"):
		return fmt.Errorf("%w: %s", sandbox.ErrNotGrantable, path)
	case strings.Contains(msg, "E_PATH_FORBIDDEN"):
		return fmt.Errorf("%w: %s (%s)", sandbox.ErrPathForbidden, path, msg)
	}
	return fmt.Errorf("hisn %s %s: %w: %s", op, path, err, msg)
}

// syncWriter serialises concurrent writes from the stdout and stderr copiers.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}
