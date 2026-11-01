package hisn

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
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
	ID   string `json:"id"`
	Live bool   `json:"live"`
}

// listSessions parses `hisn ls --json`.
func (b *Backend) listSessions(ctx context.Context) (map[string]lsEntry, error) {
	out, err := b.output(ctx, false, "ls", "--json")
	if err != nil {
		return nil, fmt.Errorf("hisn ls: %w: %s", err, strings.TrimSpace(out))
	}
	var rows []lsEntry
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &rows); err != nil {
		return nil, fmt.Errorf("hisn ls: unexpected output %q: %w", strings.TrimSpace(out), err)
	}
	m := make(map[string]lsEntry, len(rows))
	for _, r := range rows {
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
	if _, taken := sessions[s.ID]; taken {
		return nil, fmt.Errorf("%w: a hisn session %q already exists and was not created by this Wakil", sandbox.ErrSessionInUse, s.ID)
	}

	rec := record{ID: s.ID, Pid: os.Getpid(), PidStart: procStart(os.Getpid()), Token: newToken(), Cwd: s.HostCwd}
	if err := b.createRecord(rec); err != nil {
		if errors.Is(err, errSessionInUse) {
			return nil, fmt.Errorf("%w: %w", sandbox.ErrSessionInUse, err)
		}
		return nil, err
	}

	newLive := func() *live {
		lctx, cancel := context.WithCancel(context.Background())
		return &live{id: s.ID, token: rec.Token, ctx: lctx, cancel: cancel}
	}
	if err := b.runSession(ctx, rec); err != nil {
		if !b.abortStartLocked(rec) {
			// Cleanup did not complete: hand back an obligation so the backend
			// keeps retrying (Start/Stop/Close) instead of forgetting a session
			// that may still be running.
			return newLive(), err
		}
		return nil, err
	}
	rec.Started = true
	if err := b.rewriteRecord(rec); err != nil {
		if !b.abortStartLocked(rec) {
			return newLive(), fmt.Errorf("hisn: updating owner record: %w", err)
		}
		return nil, fmt.Errorf("hisn: updating owner record: %w", err)
	}
	return newLive(), nil
}

// runSession writes + validates the policy, starts the detached session and
// verifies it is the instance we just created.
func (b *Backend) runSession(ctx context.Context, rec record) error {
	if err := os.MkdirAll(b.opts.HomeRoot, 0o700); err != nil {
		return fmt.Errorf("hisn home_root: %w", err)
	}
	pol, err := buildPolicy(rec.Cwd, b.opts.HomeRoot, hostnameFor(rec.Token), b.opts.GrantableRW, b.opts.EnvPass)
	if err != nil {
		return err
	}
	polPath, err := writePolicy(b.sessionsDir(), rec.ID, pol)
	if err != nil {
		return fmt.Errorf("writing hisn policy: %w", err)
	}
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if out, err := b.output(vctx, false, "policy", "validate", polPath); err != nil {
		return fmt.Errorf("hisn policy validate failed: %w: %s", err, strings.TrimSpace(out))
	}
	rctx, rcancel := context.WithTimeout(ctx, 60*time.Second)
	defer rcancel()
	if out, err := b.output(rctx, true, "run", "--detach", "--session-id", rec.ID,
		"--policy", polPath, "--cwd", rec.Cwd, "--network", "host"); err != nil {
		return fmt.Errorf("hisn run --detach failed: %w: %s", err, strings.TrimSpace(out))
	}
	ok, err := b.verifyInstance(ctx, rec.ID, rec.Token)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("hisn session %s is not the instance Wakil created", rec.ID)
	}
	return nil
}

// abortStartLocked cleans up after a failed start WITHOUT ever touching a
// session Wakil did not create: it kills only an instance whose token matches
// and removes the record only when nothing of ours can still be running. It
// reports whether nothing of ours is left running (true) or a cleanup
// obligation remains (false; the record is kept).
func (b *Backend) abortStartLocked(rec record) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sessions, err := b.listSessions(ctx)
	if err != nil {
		return false // keep the record: recovery information
	}
	if _, present := sessions[rec.ID]; !present {
		b.removeRecord(rec.ID)
		return true
	}
	ok, err := b.verifyInstance(ctx, rec.ID, rec.Token)
	if err != nil {
		return false // present but unverifiable: keep the record
	}
	if !ok {
		b.removeRecord(rec.ID) // someone else's session; never ours
		return true
	}
	if err := b.killSession(ctx, rec.ID, rec.Token); err != nil {
		return false
	}
	b.removeRecord(rec.ID)
	return true
}

// stopLocked ends the session described by l. Caller holds the state lock.
// The record is removed only after the session is verifiably gone.
func (b *Backend) stopLocked(ctx context.Context, l *live) error {
	r, err := b.readRecord(l.id)
	if err != nil {
		return fmt.Errorf("hisn stop: owner record for %s unreadable: %w", l.id, err)
	}
	if r.Token != l.token || r.Pid != os.Getpid() {
		return fmt.Errorf("hisn stop: owner record for %s belongs to another instance; refusing to kill", l.id)
	}
	sessions, err := b.listSessions(ctx)
	if err != nil {
		return err
	}
	if _, present := sessions[l.id]; present {
		ok, err := b.verifyInstance(ctx, l.id, l.token)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("hisn stop: session %s is not the instance Wakil created; refusing to kill", l.id)
		}
		if err := b.killSession(ctx, l.id, l.token); err != nil {
			return err
		}
	}
	b.removeRecord(l.id)
	return nil
}

// Reconcile cleans up sessions left behind by dead Wakil processes. Only
// sessions with an owner record whose process is gone are touched; sessions
// without a record (other tools, other users of hisn) are never killed.
func (b *Backend) Reconcile(ctx context.Context) error {
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
	sessions, err := b.listSessions(ctx)
	if err != nil {
		return err
	}
	ent, present := sessions[r.ID]
	if !present || !ent.Live {
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
