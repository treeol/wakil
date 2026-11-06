package hisn

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/treeol/wakil/internal/sandbox"
	"github.com/treeol/wakil/internal/sandbox/hisn/ctl"
)

// MinVersion is the oldest supported hisn release: 0.2.1 (ready-fd,
// exec_id, prune, stale-kill, session.shm/tmp_size, frozen --json schemas).
const MinVersion = "0.2.1"

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
	Home        string   // sandbox.hisn.home: "persist" (default) | "tmpfs"
	Rootfs      string   // sandbox.hisn.rootfs (store name or abs path); "" = host toolchain
	SSHAgent    bool     // sandbox.hisn.ssh_agent: expose the host agent ([ssh], global only)
	SSHKeys     []string // sandbox.hisn.ssh_keys + ssh_signing key fingerprints (SHA256:…)
	HostCwd     string   // absolute, symlink-free workspace (required)
	GrantableRW []string // prefixes allowed for rw grants
	EnvPass     []string // names passed into the sandbox
	Env         []string // base environment for hisn clients; nil = os.Environ()
	// TmpSizeMB / ShmSizeMB size the session tmpfs mounts (MiB). Defaults
	// (1024 / 64) mirror hisn 0.2.1. Ranges are hisn's: tmp 16..65536,
	// shm 1..65536; validated here so a bad config fails before start.
	TmpSizeMB int64
	ShmSizeMB int64
	// ExecViaCLI runs tool-call execs through the `hisn exec` CLI (one client
	// process per call) instead of the control-socket client. Fallback for
	// one release; default false (ctl path).
	ExecViaCLI bool
	// Surface is the per-project request from .wakil/sandbox.json (services,
	// forwards, env, mounts, risk_ack, named profile). Zero = nothing extra.
	Surface Surface
	// KVR configures the in-sandbox staging store service (zero = off).
	KVR KVROptions
}

// live is one running session instance.
type live struct {
	id    string
	token string
	// runtimeRoot is hisn's runtime directory for this instance (the parent
	// of the session dir; ctl.SocketPath(root, id) is the control socket).
	runtimeRoot string
	ctx         context.Context // cancelled when the session is being replaced/stopped
	cancel      context.CancelFunc
	wg          sync.WaitGroup // in-flight hisn exec clients
	// sup is the `hisn run --detach` supervisor process: the session's lifetime
	// handle. nil only for a start that failed before the supervisor spawned.
	sup *supervisorProc
	// refused is set when a control request found the session's socket refusing
	// connections: whatever the process table says, the session is dead.
	refused atomic.Bool
	// kvr is the staging socket once the service answered; "" otherwise.
	kvr string
	// ctl is the session's control-socket client (lazily built).
	ctlOnce sync.Once
	ctlCli  *ctl.Client
}

// dead reports whether the session's supervisor has exited. A dead session can
// never come back: the supervisor holds the namespaces and the control socket.
func (l *live) dead() bool { return l.refused.Load() || (l.sup != nil && l.sup.hasExited()) }

// deadErr describes a dead session, with the supervisor's captured stderr and
// the path of its log.
func (l *live) deadErr() error {
	if l.sup == nil {
		return fmt.Errorf("%w: hisn session %s has no supervisor", sandbox.ErrSessionDead, l.id)
	}
	msg := l.sup.stderrText()
	if msg == "" {
		msg = "(no output; hisn prints diagnostics only with -v)"
	}
	state := "exited (" + l.sup.exitStatus() + ")"
	if !l.sup.hasExited() {
		state = "is running but its control socket refuses connections; it will be replaced"
	}
	return fmt.Errorf("%w: hisn supervisor for session %s %s; log %s; stderr: %s",
		sandbox.ErrSessionDead, l.id, state, l.sup.logPath, msg)
}

// noteRefused marks the session dead when a hisn client's diagnostic says the
// control socket refused the connection (the supervisor behind it is gone or
// wedged). It reports whether it did.
func (l *live) noteRefused(out string) bool {
	if !isConnRefused(out) {
		return false
	}
	l.refused.Store(true)
	return true
}

// ctl returns the session's control-socket client, building it once: the
// socket of THIS instance's session directory under hisn's runtime root.
func (l *live) ctl() *ctl.Client {
	l.ctlOnce.Do(func() {
		l.ctlCli = ctl.New(ctl.SocketPath(l.runtimeRoot, l.id))
	})
	return l.ctlCli
}

// Backend drives hisn. Lifecycle transitions (Start/Stop/Close) take mu for
// writing; Exec/Grant/Revoke take it for reading only to ADMIT and capture the
// current session, so a stale call can never reach a replacement session.
type Backend struct {
	opts Options
	pm   *sandbox.PathMap
	// ver is the hisn version seen by Preflight (zero until then); it selects
	// profile mode (>= ProfileMinVersion) or the bare --policy path.
	ver [3]int
	// planned is the plan-time risk evaluation (hisn profile explain).
	planned explainDoc
	// kvrNote is the one-line state of the in-sandbox staging store.
	kvrNote string
	// kvrSocket is the host path of the staging socket ("" = staging off).
	kvrSocket string
	// inst identifies THIS Wakil process in per-process names (the default kvr
	// socket), so concurrent Wakil processes on one workspace never collide.
	inst string

	mu      sync.RWMutex
	cur     *live
	orphans []*live // sessions whose stop failed; retried, never forgotten
	closed  bool
	epoch   uint64

	lostMu sync.Mutex
	lost   []string // argv summaries of execs whose status was lost

	// Bounds (fields so tests can shorten them; never above the documented caps).
	readyTimeout time.Duration // session readiness: socket + `hisn ls --session`
	stopWait     time.Duration // per phase of Stop: after `hisn kill`, after SIGKILL

	// afterSpawn, when set (tests only), sees every supervisor right after it
	// was spawned and before readiness is awaited.
	afterSpawn func(*supervisorProc)
}

const (
	// readyCap is the longest a new session may take to become ready.
	readyCap = 10 * time.Second
	// readyPoll is the readiness polling interval.
	readyPoll = 100 * time.Millisecond
	// stopCap bounds each phase of stopping the supervisor.
	stopCap = 5 * time.Second
)

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
	if opts.TmpSizeMB == 0 {
		opts.TmpSizeMB = 1024
	}
	if opts.ShmSizeMB == 0 {
		opts.ShmSizeMB = 64
	}
	if opts.TmpSizeMB < 16 || opts.TmpSizeMB > 65536 {
		return nil, fmt.Errorf("hisn backend: tmp_size_mb %d out of range 16..65536", opts.TmpSizeMB)
	}
	if opts.ShmSizeMB < 1 || opts.ShmSizeMB > 65536 {
		return nil, fmt.Errorf("hisn backend: shm_size_mb %d out of range 1..65536", opts.ShmSizeMB)
	}
	if !filepath.IsAbs(opts.HostCwd) {
		return nil, fmt.Errorf("hisn backend: HostCwd %q must be absolute", opts.HostCwd)
	}
	opts.HostCwd = filepath.Clean(opts.HostCwd)
	surf, err := opts.Surface.Normalize(opts.HostCwd)
	if err != nil {
		return nil, fmt.Errorf("hisn backend: %w", err)
	}
	// The global sandbox.hisn.rootfs applies unless the project names one
	// (its name form was checked by the loader). Path form is global-only.
	if surf.Rootfs == "" {
		surf.Rootfs = opts.Rootfs
	}
	if err := ValidateRootfs(surf.Rootfs, true); err != nil {
		return nil, fmt.Errorf("hisn backend: %w", err)
	}
	// Global ssh settings (never from the project file): the agent and its
	// allowed fingerprints are the same switch for signing and transports.
	surf.SSHAgent = opts.SSHAgent
	surf.SSHKeys = append([]string(nil), opts.SSHKeys...)
	if surf.SSHAgent && len(surf.SSHKeys) == 0 {
		// [ssh] needs a non-empty allowlist (plan D7); an agent with no keys
		// would forward whatever the host agent holds.
		return nil, fmt.Errorf("hisn backend: sandbox.hisn.ssh_agent = true needs sandbox.hisn.ssh_keys (SHA256:… fingerprints) or ssh_signing with a .pub key")
	}
	opts.Surface = surf
	// The state directory is the user's trust root (approvals, profiles, owner
	// records): no writable surface may reach it, whatever put it there.
	for _, g := range opts.GrantableRW {
		if err := StateGuard(opts.StateDir, "sandbox.hisn.grantable_rw entry", g); err != nil {
			return nil, fmt.Errorf("hisn backend: %w", err)
		}
	}
	for _, m := range opts.Surface.MountsRW {
		if err := StateGuard(opts.StateDir, "hisn.mounts_rw entry", m); err != nil {
			return nil, fmt.Errorf("hisn backend: %w", err)
		}
	}
	if opts.HomeRoot != "" {
		if err := StateGuard(opts.StateDir, "sandbox.hisn.home_root", opts.HomeRoot); err != nil {
			return nil, fmt.Errorf("hisn backend: %w", err)
		}
	}
	return &Backend{
		inst:         newToken()[:8],
		opts:         opts,
		readyTimeout: readyCap,
		stopWait:     stopCap,
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
	// Plan-time risk check: a label above `none` without the project's own
	// acknowledgement is a hard error HERE, before any session or UI exists.
	if err := b.planKVR(); err != nil {
		return nil, err
	}
	if err := b.plan(ctx); err != nil {
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

// Grants returns the active grants of the current session: the backend's
// ledger, which is the single source of truth for what the sandbox can reach.
func (b *Backend) Grants() []sandbox.GrantEntry { return b.pm.Grants() }

var _ sandbox.GrantLister = (*Backend)(nil)

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
	// A session whose supervisor died is NOT running: it is replaced (cleaned
	// up and started fresh) rather than treated as already started.
	if b.cur != nil && b.cur.id == s.ID && len(b.orphans) == 0 && !b.cur.dead() {
		return nil
	}
	// Snapshot active grants before the replacement so a stale-session
	// replacement does not silently drop access the user already granted
	// (stopAllLocked clears the ledger, so the snapshot must come first). Grants
	// belong to ONE chat's session: they are replayed only into a replacement of
	// the SAME id. Starting a different id never inherits them; a line says so.
	var grants []sandbox.GrantEntry
	if held := b.pm.Grants(); len(held) > 0 {
		if b.cur != nil && b.cur.id == s.ID {
			grants = held
		} else {
			fmt.Fprintf(errWriter, "hisn: not carrying %d grant(s) over to session %s (they belonged to another session)\n", len(held), s.ID)
		}
	}
	if err := b.stopAllLocked(ctx); err != nil {
		// A tool call is never failed because the PREVIOUS session would not stop:
		// the new session is started regardless (a conflict on the SAME id is
		// still caught by startLocked) and the leftover stays tracked and is
		// retried by Stop/Close. One line says so.
		fmt.Fprintf(errWriter, "hisn: starting %s although the previous session was not fully stopped: %v\n", s.ID, err)
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
		if l != nil {
			// A failed start left a possibly-running session of ours behind:
			// keep the cleanup obligation so Stop/Close/Start retry it.
			b.orphans = append(b.orphans, l)
		}
		return err
	}
	b.cur = l
	b.epoch++
	b.pm.ResetGrants()
	// Replay grants on the replacement session. Both RO and RW grants are
	// re-issued through hisn so Landlock is updated; the policy (and thus
	// grantable_rw) does not change across replacements of the same session,
	// so this always succeeds for the same set.
	for _, g := range grants {
		args := []string{"grant", "--session", l.id}
		if g.RW {
			args = append(args, "--rw")
		}
		args = append(args, "--", g.Path)
		if _, err := b.output(ctx, false, args...); err != nil {
			// Best-effort: a failed replay is a missing grant, not a
			// start failure. The user re-grants if needed.
			fmt.Fprintf(errWriter, "hisn: grant replay for %s failed: %v\n", g.Path, err)
			continue
		}
		b.pm.AddGrant(g.Path, g.RW)
	}
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
	if l.dead() {
		// Never hand a dead session to a tool call. The caller learns it is
		// dead (typed error) and starts a fresh one; nothing is retried here.
		return nil, nil, l.deadErr()
	}
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

// bindCtx ties ctx to the session instance: it is cancelled when the session is
// being replaced or stopped, so a control operation cannot outlive its session.
func bindCtx(ctx context.Context, l *live) (context.Context, context.CancelFunc) {
	cctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(l.ctx, cancel)
	if l.ctx.Err() != nil {
		cancel() // already replaced: do not wait for the async AfterFunc
	}
	return cctx, func() { stop(); cancel() }
}

// stillCurrent reports whether l is still the running session.
func (b *Backend) stillCurrent(l *live) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.cur == l
}

// Grant implements sandbox.Backend.
func (b *Backend) Grant(ctx context.Context, path string, rw bool) error {
	l, release, err := b.admit()
	if err != nil {
		return err
	}
	defer release()
	ctx, done := bindCtx(ctx, l)
	defer done()
	if !filepath.IsAbs(path) {
		return fmt.Errorf("hisn grant: path %q is not absolute", path)
	}
	if rw {
		if err := b.guardStateNotWritable("rw grant", path); err != nil {
			return err
		}
	}
	args := []string{"grant", "--session", l.id}
	if rw {
		args = append(args, "--rw")
	}
	args = append(args, "--", path)
	out, err := b.output(ctx, false, args...)
	if err != nil {
		// `out` is hisn's own client output (no program is involved), so a
		// refused connection here is trustworthy evidence the session is dead.
		if l.noteRefused(out) || l.dead() { // or the supervisor died mid-request
			return l.deadErr()
		}
		return classifyControlError("grant", path, out, err)
	}
	// Only record the grant if the session it was made in is still the
	// current one; otherwise it would leak into the replacement's path map.
	if !b.stillCurrent(l) {
		return sandbox.ErrNoSession
	}
	b.pm.AddGrant(path, rw)
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
	ctx, done := bindCtx(ctx, l)
	defer done()
	out, err := b.output(ctx, false, "revoke", "--session", l.id, "--", path)
	if err != nil {
		// `out` is hisn's own client output (no program is involved), so a
		// refused connection here is trustworthy evidence the session is dead.
		if l.noteRefused(out) || l.dead() { // or the supervisor died mid-request
			return l.deadErr()
		}
		return classifyControlError("revoke", path, out, err)
	}
	if !b.stillCurrent(l) {
		return sandbox.ErrNoSession
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
