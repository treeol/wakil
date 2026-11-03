package sandbox

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
)

// Tier is the capability class asking for outside-workspace access.
type Tier int

const (
	// TierRead is the read-only (discovery) tier: outside-cwd access is a ro grant.
	TierRead Tier = iota
	// TierEdit is the edit tier: outside-cwd access is a rw grant, permitted
	// only under a configured grantable prefix.
	TierEdit
)

// GrantManager maps capability tiers onto backend grants. It is policy only;
// the backend performs the mount.
type GrantManager struct {
	b           Backend
	cwd         string
	grantableRW []string

	mu     sync.Mutex
	active map[string]bool // path -> rw
}

// NewGrantManager builds a manager for b. cwd is the workspace (needs no
// grant); grantableRW are the prefixes under which rw grants may be made.
func NewGrantManager(b Backend, cwd string, grantableRW []string) *GrantManager {
	cleaned := make([]string, 0, len(grantableRW))
	for _, g := range grantableRW {
		cleaned = append(cleaned, filepath.Clean(g))
	}
	return &GrantManager{b: b, cwd: filepath.Clean(cwd), grantableRW: cleaned, active: map[string]bool{}}
}

// Ensure makes path accessible for tier. Paths inside cwd need nothing. An rw
// request outside every grantable prefix returns ErrNotGrantable without any
// retry or widening.
func (g *GrantManager) Ensure(ctx context.Context, path string, tier Tier) error {
	if err := requireAbs(path); err != nil {
		return err
	}
	p := filepath.Clean(path)
	if Within(g.cwd, p) {
		return nil
	}
	rw := tier == TierEdit
	if rw && !g.underGrantable(p) {
		return fmt.Errorf("%w: %s", ErrNotGrantable, p)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	cur, ok := g.active[p]
	if ok && (cur || !rw) {
		return nil // already granted with at least the requested access
	}
	if ok { // ro -> rw: modes are never changed in place
		if err := g.b.Revoke(ctx, p); err != nil {
			return err
		}
		delete(g.active, p)
	}
	if err := g.b.Grant(ctx, p, rw); err != nil {
		return err
	}
	g.active[p] = rw
	return nil
}

// Withdraw revokes a previously granted path (soft revoke).
func (g *GrantManager) Withdraw(ctx context.Context, path string) error {
	p := filepath.Clean(path)
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.active[p]; !ok {
		return nil
	}
	if err := g.b.Revoke(ctx, p); err != nil {
		return err
	}
	delete(g.active, p)
	return nil
}

// Reset forgets all grants (the session ended and took them with it).
func (g *GrantManager) Reset() {
	g.mu.Lock()
	g.active = map[string]bool{}
	g.mu.Unlock()
}

// Sync makes the manager's ledger equal to entries. It is how the manager
// follows a backend that restored grants on its own (a replacement session
// replays them): the backend's ledger is the single source of truth, so the
// manager must neither forget them (a later Withdraw would silently do
// nothing) nor keep ones the backend dropped.
func (g *GrantManager) Sync(entries []GrantEntry) {
	active := make(map[string]bool, len(entries))
	for _, e := range entries {
		active[filepath.Clean(e.Path)] = e.RW
	}
	g.mu.Lock()
	g.active = active
	g.mu.Unlock()
}

// Grants returns a snapshot of the manager's ledger, sorted by path.
func (g *GrantManager) Grants() []GrantEntry {
	g.mu.Lock()
	out := make([]GrantEntry, 0, len(g.active))
	for p, rw := range g.active {
		out = append(out, GrantEntry{Path: p, RW: rw})
	}
	g.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// GrantLister is implemented by backends that keep their own grant ledger and
// may restore grants when they replace a session.
type GrantLister interface {
	Grants() []GrantEntry
}

func (g *GrantManager) underGrantable(p string) bool {
	for _, pre := range g.grantableRW {
		if Within(pre, p) {
			return true
		}
	}
	return false
}
