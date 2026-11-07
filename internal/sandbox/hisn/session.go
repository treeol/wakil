package hisn

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/treeol/wakil/internal/sandbox"
)

func newToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// hostnameFor is the in-sandbox hostname that carries the instance token.
func hostnameFor(token string) string { return "wakil-" + token[:16] }

type lsEntry struct {
	ID    string `json:"id"`
	State string `json:"state"` // "live" | "stale" (0.2.1 frozen schema)
}

func (e lsEntry) Live() bool { return e.State == "live" }

// listSessions parses `hisn ls --json` (schema "hisn-ls/v1").
func (b *Backend) listSessions(ctx context.Context) (map[string]lsEntry, error) {
	out, err := b.output(ctx, false, "ls", "--json")
	if err != nil {
		return nil, fmt.Errorf("hisn ls: %w: %s", err, strings.TrimSpace(out))
	}
	var rows struct {
		Schema   string    `json:"schema"`
		Sessions []lsEntry `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		return nil, fmt.Errorf("hisn ls: unexpected output %q: %w", strings.TrimSpace(out), err)
	}
	m := make(map[string]lsEntry, len(rows.Sessions))
	for _, r := range rows.Sessions {
		m[r.ID] = r
	}
	return m, nil
}

// verifyInstance reports whether the live session id is the instance carrying
// token. verified=false with a nil error means a DIFFERENT session owns the id.
func (b *Backend) verifyInstance(ctx context.Context, id, token string) (verified bool, err error) {
	out, err := b.output(ctx, false, "inspect", "--session", id, "--json")
	if err != nil {
		return false, fmt.Errorf("hisn inspect %s: %w: %s", id, err, strings.TrimSpace(out))
	}
	var r struct {
		OK     bool   `json:"ok"`
		Policy string `json:"policy"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &r); err != nil || !r.OK {
		return false, fmt.Errorf("hisn inspect %s: unexpected output", id)
	}
	// The token is 128 random bits, so a substring match cannot collide with a
	// foreign session and does not depend on hisn's TOML quoting.
	if strings.Contains(r.Policy, hostnameFor(token)) {
		return true, nil
	}
	// A different instance is only proven when the policy NAMES a hostname and
	// it is not ours. A policy without any hostname is "unknown" (an error), so
	// callers keep their record instead of discarding ownership of a session
	// that may well be ours.
	if !strings.Contains(r.Policy, "hostname") {
		return false, fmt.Errorf("hisn inspect %s: resolved policy carries no hostname; cannot verify instance", id)
	}
	return false, nil
}

// waitGone polls `hisn ls` until id disappears or the deadline passes.
func (b *Backend) waitGone(ctx context.Context, id string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		m, err := b.listSessions(ctx)
		if err == nil {
			if _, ok := m[id]; !ok {
				return true
			}
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// killSession ends session id: TERM, then KILL, waiting for it to vanish. The
// instance is re-verified immediately before EACH signal, so an id that was
// reused by a foreign session between steps is never signalled. hisn offers no
// instance-bound kill, so a residual window of a single CLI round trip remains
// (a foreign session would need to reuse a UUID-sized id in that window).
func (b *Backend) killSession(ctx context.Context, id, token string) error {
	for _, sig := range []string{"TERM", "KILL"} {
		ok, err := b.verifyInstance(ctx, id, token)
		if err != nil {
			// inspect fails once the session is gone; that is success.
			if b.waitGone(ctx, id, time.Second) {
				return nil
			}
			return err
		}
		if !ok {
			return fmt.Errorf("hisn session %s is no longer the instance Wakil created; not signalling it", id)
		}
		_, _ = b.output(ctx, false, "kill", "--session", id, "--signal", sig)
		if b.waitGone(ctx, id, 5*time.Second) {
			return nil
		}
	}
	return fmt.Errorf("hisn session %s did not terminate", id)
}

// startLocked creates the session. Caller holds the state lock.
func (b *Backend) startLocked(ctx context.Context, s sandbox.Session) (*live, error) {
	// A record left by a dead owner is reconciled first; a live owner (or an
	// unreconcilable record) means the chat is active elsewhere.
	if r, err := b.readRecord(s.ID); err == nil {
		if ownerAlive(r) {
			return nil, fmt.Errorf("%w: chat %s is active in Wakil process %d", sandbox.ErrSessionInUse, s.ID, r.Pid)
		}
		if err := b.reconcileOneLocked(ctx, r); err != nil {
			return nil, fmt.Errorf("%w: stale session %s could not be cleaned: %w", sandbox.ErrSessionInUse, s.ID, err)
		}
	}
	sessions, err := b.listSessions(ctx)
	if err != nil {
		return nil, err
	}
	var notes []string
	if ent, taken := sessions[s.ID]; taken {
		if ent.Live() {
			return nil, fmt.Errorf("%w: a LIVE hisn session %q already exists and was not created by this Wakil", sandbox.ErrSessionInUse, s.ID)
		}
		// `stale` (0.2.1): the directory is there but provably dead (hisn's
		// own staleness check: socket refused AND supervisor pid gone or
		// starttime-mismatched). `hisn run --session-id` replaces it itself;
		// Wakil only says so in one line. No live Wakil owns this chat
		// (checked above, under the state lock), so a dead leftover of this
		// very chat id is replaced, never an obstacle.
		note := fmt.Sprintf("replacing stale session directory for %s (hisn %s removes it: nothing answered on its control socket)", s.ID, b.opts.Binary)
		fmt.Fprintf(errWriter, "hisn: %s\n", note)
		notes = append(notes, note)
	}

	rec := record{ID: s.ID, Pid: os.Getpid(), PidStart: procStart(os.Getpid()), Token: newToken(), Cwd: s.HostCwd}
	if err := b.createRecord(rec); err != nil {
		if errors.Is(err, errSessionInUse) {
			return nil, fmt.Errorf("%w: %w", sandbox.ErrSessionInUse, err)
		}
		return nil, err
	}

	sup, err := b.runSession(ctx, rec, notes)
	if err != nil {
		if sup != nil && !b.abortStartLocked(rec, sup) {
			// The supervisor could not be reaped: hand back an obligation so the
			// backend keeps retrying (Start/Stop/Close) instead of forgetting a
			// process that may still be running.
			return b.newLive(rec, sup), err
		}
		if sup == nil {
			b.removeRecord(rec.ID) // nothing was ever spawned
		}
		return nil, err
	}
	rec.Started = true
	if b.opts.Surface.Profile != "" {
		rec.Mode, rec.SupPid = "named", sup.pid()
		rec.SupStart = procStart(rec.SupPid)
	}
	if err := b.rewriteRecord(rec); err != nil {
		if !b.abortStartLocked(rec, sup) {
			return b.newLive(rec, sup), fmt.Errorf("hisn: updating owner record: %w", err)
		}
		return nil, fmt.Errorf("hisn: updating owner record: %w", err)
	}
	l := b.newLive(rec, sup)
	b.awaitKVR(l)
	return l, nil
}

func (b *Backend) newLive(rec record, sup *supervisorProc) *live {
	lctx, cancel := context.WithCancel(context.Background())
	l := &live{id: rec.ID, token: rec.Token, runtimeRoot: b.primaryRuntimeRoot(), ctx: lctx, cancel: cancel, sup: sup}
	// When the supervisor dies the session is over: end every in-flight hisn
	// client bound to it (they would otherwise be talking to a dead socket).
	go func() {
		<-sup.exited
		cancel()
	}()
	return l
}

// supervisorArgs is the `hisn run --detach` command line of the bare-policy path
// (hisn < 0.3.1). `-v` is REQUIRED: hisn prints nothing to stderr without it, so
// a failed start would be undiagnosable. `--ready-fd 3` (0.2.1): hisn writes
// "ready\n" to fd 3 once the session is live — the write end of the readiness
// pipe, passed as the child's fd 3 via ExtraFiles. Nothing weaker than that line
// plus a `live` confirm passes.
func supervisorArgs(id, policy, cwd string) []string {
	return []string{"run", "--detach", "-v", "--session-id", id,
		"--policy", policy, "--cwd", cwd, "--network", "host", "--ready-fd", "3"}
}

// launch is everything needed to start one session: the supervisor command
// line, its working directory and how the instance is identified afterwards.
type launch struct {
	args []string
	dir  string // supervisor process cwd ("" = inherit)
	mode string // "policy" | "profile" | "named"
	// persist is the generated profile's [home] persist path ("" when none, or
	// for a named profile, which Wakil does not generate).
	persist string
}

// prepareLaunch renders and validates what hisn is started with. Three modes:
//
//   - policy: hisn < 0.3.1, a bare hisn-policy/v1 file (`--policy`);
//   - profile: hisn >= 0.3.1, a generated hisn-profile/v1 file (`--profile`);
//   - named: a hisn profile chosen by the project (hisn.profile), used as is;
//     Wakil only adds initial grants.
//
// Wakil never writes a risk_ack: the profile carries whatever the user put in
// the project file, and hisn refuses a mismatch.
func (b *Backend) prepareLaunch(ctx context.Context, rec record) (launch, error) {
	s := b.opts.Surface
	if s.Profile != "" {
		// A named profile gets no session gitconfig. Signing or identity that
		// Wakil was asked for would be dropped silently, so refuse (review B2a).
		if b.opts.GitName != "" || b.opts.GitEmail != "" || b.opts.SigningKey != "" {
			return launch{}, fmt.Errorf("hisn named profile %q cannot carry git identity or ssh signing; drop sandbox.hisn.profile or unset ssh_signing and git_identity_*", s.Profile)
		}
		if err := os.MkdirAll(b.procCwd(), 0o700); err != nil {
			return launch{}, fmt.Errorf("hisn supervisor cwd: %w", err)
		}
		return launch{args: namedArgs(rec.ID, s.Profile, rec.Cwd, s.MountsRO, s.MountsRW), dir: b.procCwd(), mode: "named"}, nil
	}
	s, err := b.withGitConfig(s)
	if err != nil {
		return launch{}, err
	}
	pp := policyParams{Cwd: rec.Cwd, Home: b.opts.HomeRoot, Hostname: hostnameFor(rec.Token),
		GrantableRW: b.opts.GrantableRW, EnvPass: b.signingEnvPass(), ShmSize: b.opts.ShmSizeMB, TmpSize: b.opts.TmpSizeMB}
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if !b.profileMode() {
		pp.EnvSet, pp.MountsRO, pp.MountsRW = s.EnvSet, s.MountsRO, s.MountsRW
		pol, err := buildPolicy(pp)
		if err != nil {
			return launch{}, err
		}
		path, err := writePolicy(b.sessionsDir(), rec.ID, pol)
		if err != nil {
			return launch{}, fmt.Errorf("writing hisn policy: %w", err)
		}
		if out, err := b.output(vctx, false, "policy", "validate", path); err != nil {
			return launch{}, fmt.Errorf("hisn policy validate failed: %w: %s", err, strings.TrimSpace(out))
		}
		return launch{args: supervisorArgs(rec.ID, path, rec.Cwd), mode: "policy"}, nil
	}
	if b.kvrSocket != "" && b.opts.Surface.Profile == "" {
		// The socket directory must be a plain directory Wakil owns and no
		// stale socket may linger (hisn does not clean it).
		if err := PrepareKVRDir(rec.Cwd, b.kvrSocket); err != nil {
			if !errors.Is(err, ErrKVRInUse) {
				return launch{}, fmt.Errorf("staging (kvr): %w", err)
			}
			// A project-declared socket that another Wakil still serves: do
			// not unlink it and do not start a second server on it.
			b.dropKVRService()
			b.setKVRNote("off (" + err.Error() + ")")
		}
	}
	homeP, err := b.homePersist(s)
	if err != nil {
		return launch{}, err
	}
	prof, err := buildProfile(pp, s, homeP)
	if err != nil {
		return launch{}, err
	}
	if err := checkProfileTrust(b.profilePath(rec.ID), rec.Cwd, b.opts.HomeRoot, s.MountsRW); err != nil {
		return launch{}, err
	}
	if err := os.MkdirAll(b.procCwd(), 0o700); err != nil {
		return launch{}, fmt.Errorf("hisn supervisor cwd: %w", err)
	}
	if err := os.WriteFile(b.profilePath(rec.ID), prof, 0o600); err != nil {
		return launch{}, fmt.Errorf("writing hisn profile: %w", err)
	}
	if out, err := b.output(vctx, false, "profile", "validate", b.profilePath(rec.ID)); err != nil {
		return launch{}, rootfsError(validateError(err, out), s.Rootfs)
	}
	return launch{args: profileArgs(rec.ID, b.profilePath(rec.ID), rec.Cwd), dir: b.procCwd(), mode: "profile", persist: homeP}, nil
}

// validateError turns `hisn profile validate` output into an error; the risk
// codes get their own type so callers can name the project key.
func validateError(err error, out string) error {
	msg := strings.TrimSpace(out)
	if i := strings.Index(msg, "E_RISK_ACK_"); i >= 0 {
		return &RiskAckError{Detail: strings.TrimSpace(msg[i:]) + "; run `wakil sandbox trust` to review and acknowledge it"}
	}
	return fmt.Errorf("hisn profile validate failed: %w: %s", err, msg)
}

// rootfsError maps hisn's missing-rootfs failure to the actionable fix from
// the plan. Other errors pass through unchanged.
func rootfsError(err error, rootfs string) error {
	if rootfs == "" || err == nil {
		return err
	}
	if strings.Contains(err.Error(), "rootfs.path") && strings.Contains(err.Error(), "No such file") {
		if filepath.IsAbs(rootfs) {
			return fmt.Errorf("rootfs %q is not a directory: %w", rootfs, err)
		}
		return fmt.Errorf("rootfs %q is not in the hisn store; fix: hisn rootfs import %s --from-image %s: %w", rootfs, rootfs, rootfs, err)
	}
	return err
}

// RiskAckError means the profile needs a risk acknowledgement the project file
// does not carry (or carries wrongly). Wakil never supplies one itself.
type RiskAckError struct {
	Label  string // the computed label, when known
	Detail string // hisn's message
}

func (e *RiskAckError) Error() string {
	l := ""
	if e.Label != "" {
		l = " (label " + e.Label + ")"
	}
	return "sandbox: this workspace's sandbox risk is not acknowledged" + l +
		" — " + e.Detail + " (Wakil never acknowledges a risk on your behalf)"
}

// runSession prepares + validates the launch, spawns the supervisor (which IS
// the session and never returns while it lives), waits until the session is
// provably ready and verifies it is the instance we just created. The returned
// supervisor is non-nil whenever one was spawned, success or not, so the caller
// can reap it. On any error after the spawn the supervisor is already killed
// (best effort) and the error carries its exit status and captured stderr.
func (b *Backend) runSession(ctx context.Context, rec record, notes []string) (*supervisorProc, error) {
	if err := os.MkdirAll(b.opts.HomeRoot, 0o700); err != nil {
		return nil, fmt.Errorf("hisn home_root: %w", err)
	}
	l, err := b.prepareLaunch(ctx, rec)
	if err != nil {
		return nil, err
	}
	sup, readyR, err := b.startSupervisor(b.supervisorLogPath(rec.ID), l.dir, notes, l.args...)
	if err != nil {
		return nil, fmt.Errorf("hisn run --detach: %w", err)
	}
	defer readyR.Close()
	if b.afterSpawn != nil {
		b.afterSpawn(sup)
	}
	if err := b.waitReady(ctx, rec.ID, sup, readyR); err != nil {
		b.reapSupervisor(sup)
		return sup, b.withSupervisorStderr(err, sup)
	}
	var err2 error
	if l.mode != "named" {
		// (A named profile brings its own hostname, so the token cannot prove
		// the instance there; the supervisor process handle (exact, unreaped)
		// and the --ready-fd line do. The owner record keeps pid+starttime for
		// Reconcile.)
		var ok bool
		ok, err2 = b.verifyInstance(ctx, rec.ID, rec.Token)
		if err2 == nil && !ok {
			err2 = fmt.Errorf("hisn session %s is not the instance Wakil created", rec.ID)
		}
	}
	if err2 == nil && l.mode != "policy" {
		// The running session must not be riskier than what the user acknowledged.
		_, err2 = b.verifyRisk(ctx, rec.ID, l.persist)
	}
	if err := err2; err != nil {
		b.reapSupervisor(sup)
		return sup, b.withSupervisorStderr(err, sup)
	}
	return sup, nil
}

// withSupervisorStderr appends the supervisor's exit state and captured stderr.
func (b *Backend) withSupervisorStderr(err error, sup *supervisorProc) error {
	msg := sup.stderrText()
	if msg == "" {
		msg = "(no output)"
	}
	return fmt.Errorf("%w (supervisor %s; log %s); stderr: %s", err, sup.exitStatus(), sup.logPath, msg)
}

// runtimeRoots mirrors hisn's session-directory candidates, in preference order:
// $XDG_RUNTIME_DIR/hisn, then /tmp/hisn-$UID. XDG_RUNTIME_DIR is read from the
// environment hisn is actually given (see clientEnvAllow), not Wakil's own.
func (b *Backend) runtimeRoots() []string {
	var roots []string
	for _, kv := range b.baseEnv() {
		if v, ok := strings.CutPrefix(kv, "XDG_RUNTIME_DIR="); ok && strings.HasPrefix(v, "/") {
			roots = append(roots, filepath.Join(v, "hisn"))
			break
		}
	}
	return append(roots, fmt.Sprintf("/tmp/hisn-%d", os.Getuid()))
}

// primaryRuntimeRoot is where THIS instance's session directory lives (the
// first runtime root; hisn uses it for the run that just started).
func (b *Backend) primaryRuntimeRoot() string { return b.runtimeRoots()[0] }

// sessionState classifies id as hisn itself does: "live" (something answers on
// the control socket), "stale" (the directory exists, nothing answers) or
// "missing". `hisn ls --session ID` prints no state word (it lists grants), so
// the state comes from `hisn ls --json`.
func (b *Backend) sessionState(ctx context.Context, id string) (string, error) {
	m, err := b.listSessions(ctx)
	if err != nil {
		return "", err
	}
	ent, ok := m[id]
	switch {
	case !ok:
		return "missing", nil
	case ent.Live():
		return "live", nil
	}
	return "stale", nil
}

// ready is one readiness probe: the session must be reported `live` by `hisn ls`
// AND answer `hisn ls --session ID`. Anything else, including an error, is "not
// ready" with the reason.
func (b *Backend) ready(ctx context.Context, id string) (bool, string) {
	state, err := b.sessionState(ctx, id)
	if err != nil {
		return false, err.Error() // already prefixed "hisn ls: ..."
	}
	if state != "live" {
		return false, fmt.Sprintf("hisn ls reports the session %s", state)
	}
	if out, err := b.output(ctx, false, "ls", "--session", id); err != nil {
		return false, fmt.Sprintf("hisn ls --session: %v: %s", err, strings.TrimSpace(out))
	}
	return true, ""
}

// waitReady blocks until the session is READY, for at most b.readyTimeout.
// Ready means, all at once: the supervisor process has not exited, hisn wrote
// the whole "ready\n" line to the readiness pipe (`--ready-fd`, hisn >= 0.2.1),
// `hisn ls` reports the session `live`, and the same holds again one poll
// interval later (confirm probe). There is no weaker path: a partial line
// without "\n", `stale`, missing, an error, an exited supervisor or a timeout
// is "not ready". The 0.2.0 polling loop is gone: the pipe read replaces it.
func (b *Backend) waitReady(ctx context.Context, id string, sup *supervisorProc, readyR *os.File) error {
	deadline := time.Now().Add(b.readyTimeout)
	// readyLine reads the one "ready\n" line hisn writes to fd 3, with the
	// remaining budget. An EOF (all writers gone: supervisor exited or never
	// passed --ready-fd) is a definitive not-ready, not a poll timeout. It
	// returns eof=true so the caller still probes `hisn ls` once: the recorded
	// reason then says what hisn itself thinks of the session (e.g. `stale`).
	readReady := func() (ok bool, eof bool, why string) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, false, ""
		}
		if err := readyR.SetReadDeadline(time.Now().Add(remaining)); err != nil {
			return false, false, "setting readiness pipe deadline: " + err.Error()
		}
		line, err := bufio.NewReader(readyR).ReadString('\n')
		if err == nil && strings.TrimSpace(line) != "ready" {
			return false, false, fmt.Sprintf("unexpected readiness line %q", line)
		}
		if err == nil {
			return true, false, ""
		}
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return false, false, "readiness pipe: " + err.Error()
		}
		if sup.hasExited() {
			return false, false, "" // the exit check below reports it with stderr
		}
		// EOF: no "ready\n" will ever arrive. Probe `hisn ls` once for the
		// session's own verdict — usually "stale" — and record that.
		pctx, cancel := context.WithTimeout(ctx, min(time.Until(deadline), 2*time.Second))
		state, serr := b.sessionState(pctx, id)
		cancel()
		if serr == nil {
			return false, true, fmt.Sprintf("hisn ls reports the session %s (no readiness line)", state)
		}
		return false, true, "readiness pipe: " + err.Error()
	}
	probe := func() (bool, string) {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return false, "" // out of time: no probe, and no reason to overwrite the last one
		}
		pctx, cancel := context.WithTimeout(ctx, min(remaining, 2*time.Second))
		defer cancel()
		ok, why := b.ready(pctx, id)
		if !ok && pctx.Err() != nil {
			// The probe was cut short by the deadline: its failure says nothing
			// about the session, so it must not replace the last real reason.
			return false, ""
		}
		return ok, why
	}
	var last string
	gotReady := false // latch: the "ready\n" line was received once; don't re-read the pipe
	for {
		if sup.hasExited() {
			return fmt.Errorf("hisn run --detach exited before the session was ready (last probe: %s)", orNone(last))
		}
		if !gotReady {
			ok, _, why := readReady()
			if !ok && why != "" {
				last = why
			}
			if ok {
				gotReady = true
			}
		}
		if gotReady {
			// Confirm: `live` now, and still live one interval later.
			ok, why := probe()
			if !ok {
				if why != "" {
					last = why
				}
				if sup.hasExited() {
					return fmt.Errorf("hisn run --detach exited right after the readiness line")
				}
			} else {
				select {
				case <-sup.exited:
					return fmt.Errorf("hisn run --detach exited right after the session became ready")
				case <-ctx.Done():
					return fmt.Errorf("waiting for hisn session %s: %w", id, ctx.Err())
				case <-time.After(readyPoll):
				}
				if ok2, why2 := probe(); ok2 && !sup.hasExited() {
					return nil
				} else if sup.hasExited() {
					return fmt.Errorf("hisn run --detach exited right after the session became ready")
				} else if why2 != "" {
					last = "second probe failed: " + why2
				}
			}
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("hisn session %s not ready after %s (%s)", id, b.readyTimeout, orNone(last))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for hisn session %s: %w", id, ctx.Err())
		case <-sup.exited:
		case <-time.After(readyPoll):
		}
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// reapSupervisor makes sure the supervisor is gone: SIGKILL, then Wait (bounded).
// The supervisor holds the namespaces; killing it makes its init die through its
// own PDEATHSIG. It reports whether the process was reaped.
func (b *Backend) reapSupervisor(sup *supervisorProc) bool {
	if sup.hasExited() {
		return true
	}
	sup.kill()
	return sup.waitExit(b.stopWait)
}

// abortStartLocked cleans up after a failed start. The supervisor handle makes
// this exact: the process Wakil spawned is killed and reaped, never a session
// found by id. The runtime directory is hisn's to clean (0.2.1: `hisn kill` on
// a stale dir removes it, exit 0). It reports whether nothing of ours is left
// running (true) or a cleanup obligation remains (false; the record is kept).
func (b *Backend) abortStartLocked(rec record, sup *supervisorProc) bool {
	if !b.reapSupervisor(sup) {
		return false
	}
	// 0.2.1: after the reap the dir is stale by construction (socket refused,
	// supervisor gone); `hisn kill --session` removes it and exits 0 even on
	// a dir that never became live. Best effort here: a leftover is caught by
	// the next Start (hisn run replaces stale) or `ls --prune` at Reconcile.
	kctx, cancel := context.WithTimeout(context.Background(), b.stopWait)
	_, _ = b.output(kctx, false, "kill", "--session", rec.ID)
	cancel()
	b.removeRecord(rec.ID)
	return true
}

// stopLocked ends the session described by l. Caller holds the state lock.
// The supervisor handle is exact and Wakil spawned it, so it is reaped
// UNCONDITIONALLY: `hisn kill` asks the session to end, then the handle proves
// it (wait capped, SIGKILL on the cap, wait again). Only the bookkeeping is
// gated on the owner record: a record that belongs to another instance is left
// alone and reported. Idempotent: a session that is already gone (supervisor
// exited, or `hisn kill` reporting "no such session") is a finished stop.
func (b *Backend) stopLocked(ctx context.Context, l *live) error {
	if l.sup != nil && !l.sup.hasExited() {
		// While OUR supervisor is unreaped it holds the id, so a duplicate `hisn
		// run` for it fails and `hisn kill --session id` can only reach it (the
		// residual window is the CLI round trip after the exit check). The
		// request has its OWN bound and survives a cancelled caller ctx; its
		// outcome is irrelevant: the exit of the supervisor is what counts.
		kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.stopWait)
		_, _ = b.output(kctx, false, "kill", "--session", l.id)
		cancel()
		if !l.sup.waitExit(b.stopWait) {
			l.sup.kill()
			if !l.sup.waitExit(b.stopWait) {
				return fmt.Errorf("hisn stop: supervisor of session %s (pid %d) did not exit after SIGKILL", l.id, l.sup.pid())
			}
		}
	}
	r, err := b.readRecord(l.id)
	switch {
	case err == nil:
		if r.Token != l.token || r.Pid != os.Getpid() {
			return fmt.Errorf("hisn stop: owner record for %s belongs to another instance; supervisor stopped, record left in place", l.id)
		}
	case errors.Is(err, os.ErrNotExist):
		// Already cleaned up (idempotent).
	default:
		return fmt.Errorf("hisn stop: owner record for %s unreadable: %w", l.id, err)
	}
	// 0.2.1: the runtime directory is hisn's to clean. After OUR supervisor is
	// reaped, a `live` socket on the id is a REPLACEMENT by a foreign session:
	// leave it alone (the id is no longer ours). A `stale` dir is removed by
	// `hisn kill --session id` itself (stale-kill: remove + exit 0). The
	// residual window is the one CLI round trip killSession already documents.
	if l.sup != nil && !l.sup.hasExited() {
		// unreachable (reaped above), kept for clarity
		_ = l.sup
	}
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), b.stopWait)
	state, serr := b.sessionState(sctx, l.id)
	scancel()
	if serr == nil {
		switch state {
		case "stale":
			kctx, kcancel := context.WithTimeout(context.WithoutCancel(ctx), b.stopWait)
			_, _ = b.output(kctx, false, "kill", "--session", l.id)
			kcancel()
		case "live":
			fmt.Fprintf(errWriter, "hisn: session %s answers on its socket after our supervisor exited; a foreign session, not touched\n", l.id)
		}
	}
	b.removeRecord(l.id)
	return nil
}

// Reconcile cleans up sessions left behind by dead Wakil processes. First
// `hisn ls --prune` (0.2.1): hisn itself removes every provably stale dir
// (socket refused AND supervisor pid gone or starttime-mismatched, checked
// twice 300 ms apart, fd-relative, owner-checked, runtime-root confined) —
// no live session is touched, only dead directories, so no owner record is
// needed for it. Then the owner-record reconcile: only sessions with an
// owner record whose process is gone are touched; sessions without a record
// (other tools, other users of hisn) are never killed.
func (b *Backend) Reconcile(ctx context.Context) error {
	pctx, pcancel := context.WithTimeout(ctx, 30*time.Second)
	_, _ = b.output(pctx, false, "ls", "--prune")
	pcancel()
	return b.withLock(func() error {
		var errs []error
		for _, id := range b.listRecordIDs() {
			r, err := b.readRecord(id)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if ownerAlive(r) {
				continue
			}
			if err := b.reconcileOneLocked(ctx, r); err != nil {
				errs = append(errs, err)
			}
		}
		return errors.Join(errs...)
	})
}

// reconcileOneLocked handles one record whose owner is dead. The record is
// re-read first so a session replaced by a new owner is left alone.
func (b *Backend) reconcileOneLocked(ctx context.Context, r record) error {
	cur, err := b.readRecord(r.ID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if cur.Token != r.Token || ownerAlive(cur) {
		return nil // replaced or revived: not ours to clean
	}
	if cur.Mode == "named" {
		return b.reconcileNamedLocked(ctx, cur)
	}
	sessions, err := b.listSessions(ctx)
	if err != nil {
		return err
	}
	ent, present := sessions[r.ID]
	if !present || !ent.Live() {
		// Deliberately NOT removing a non-live directory here: unlike Stop, no
		// supervisor of ours was reaped in this process, so it could belong to a
		// foreign session that is still starting (no socket yet). A stale
		// directory therefore blocks the id until removed (see Known limits).
		b.removeRecord(r.ID)
		return nil
	}
	ok, err := b.verifyInstance(ctx, r.ID, r.Token)
	if err != nil {
		return err // unverifiable: keep the record
	}
	if !ok {
		b.removeRecord(r.ID) // the id was reused by a session that is not ours
		return nil
	}
	if err := b.killSession(ctx, r.ID, r.Token); err != nil {
		return err // keep the record
	}
	b.removeRecord(r.ID)
	return nil
}

// reconcileNamedLocked cleans up a session started from a named hisn profile.
// Its policy carries no instance token, so identity is the supervisor process
// Wakil spawned: pid AND /proc starttime. Only that exact process is signalled.
func (b *Backend) reconcileNamedLocked(ctx context.Context, r record) error {
	if r.SupPid > 0 && r.SupStart != "" && procStart(r.SupPid) == r.SupStart {
		_ = syscall.Kill(r.SupPid, syscall.SIGTERM)
		gone := func() bool { return procStart(r.SupPid) != r.SupStart }
		for i := 0; i < 50 && !gone(); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		if !gone() {
			_ = syscall.Kill(r.SupPid, syscall.SIGKILL)
			for i := 0; i < 50 && !gone(); i++ {
				time.Sleep(100 * time.Millisecond)
			}
			if !gone() {
				return fmt.Errorf("hisn: supervisor %d of named-profile session %s did not exit", r.SupPid, r.ID)
			}
		}
	}
	// The supervisor is gone: a stale directory is removed by hisn itself.
	kctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.stopWait)
	_, _ = b.output(kctx, false, "kill", "--session", r.ID)
	cancel()
	b.removeRecord(r.ID)
	return nil
}

// homePersist returns the [home] persist path for this launch (see
// homePersistFor). The project override wins over sandbox.hisn.home.
func (b *Backend) homePersist(s Surface) (string, error) {
	mode := b.opts.Home
	if s.Home != "" {
		mode = s.Home
	}
	return homePersistFor(mode, b.opts.HomeRoot, b.ver)
}
