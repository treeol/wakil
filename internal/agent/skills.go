package agent

import (
	"context"
	"fmt"
	"strings"

	"github.com/treeol/wakil/internal/memory"
)

// skillsProfile wraps the shared memory.Store engine with the skills policy:
// always durable (no TTL), anchors disabled, active-by-default for user/main
// writes. It is NOT a new storage engine — it reuses memory.Store's SQLite
// schema, FTS5 index, provenance fields, and supersedes history. The wrapper
// enforces the skill-specific invariants so the generic store is never
// misused as a memory clone.
//
// Rationale (Mashūra-reviewed): memory.Store is workspace-keyed and carries
// memory product policy (mid/durable tiers, proposed→promote, workspace
// anchors). Skills are GLOBAL user-authored reference docs with opposite
// defaults. Rebuilding a store from scratch duplicates weeks of battle-tested
// code; wrapping it with a frozen policy profile is the minimal clean reuse.
type skillsProfile struct {
	store *memory.Store
}

// skillValueMaxBytes is the per-skill content cap. Memory caps values at
// 64 KiB (maxValueLen in the store); skills raise this to 256 KiB because
// reference docs (templates, runbooks, specs) are larger than remembered
// facts. The cap is enforced HERE (not in the store) so the memory store's
// own invariant is untouched.
const skillValueMaxBytes = 256 * 1024

// newSkillsProfile wraps an already-open memory.Store for skill use.
// The unexported constructor keeps the package-internal wiring; NewSkillsProfile
// is the exported entry point used by the host startup code (cmd/wakil).
func newSkillsProfile(store *memory.Store) *skillsProfile {
	return &skillsProfile{store: store}
}

// NewSkillsProfile is the exported constructor for the host startup code.
func NewSkillsProfile(store *memory.Store) *skillsProfile {
	return newSkillsProfile(store)
}

// validateSkillKey rejects empty/overlong keys and keys containing path
// separators — skill keys are flat identifiers, not paths.
func validateSkillKey(key string) error {
	if key == "" {
		return fmt.Errorf("skill key is required")
	}
	if len(key) > 256 {
		return fmt.Errorf("skill key exceeds 256 bytes")
	}
	if strings.ContainsAny(key, "/\\") {
		return fmt.Errorf("skill key must not contain path separators: %q", key)
	}
	return nil
}

// validateSkillValue enforces the 256 KiB skill content cap.
func validateSkillValue(value string) error {
	if value == "" {
		return fmt.Errorf("skill value is required")
	}
	if len(value) > skillValueMaxBytes {
		return fmt.Errorf("skill value exceeds %d bytes (got %d)", skillValueMaxBytes, len(value))
	}
	return nil
}

// putActiveSkill writes an immediately-active durable skill, superseding any
// existing active entry for the same key. Used by save_skill (new key, no
// prior active) and update_skill (supersede existing active). Anchors are
// always nil — global skills have no stable workspace root.
//
// The underlying store enforces its own 64 KiB value cap via validateValue
// for memory entries. Skills need 256 KiB, so putActiveSkill routes through
// Store.PutSkillActive (internal/memory/store_skills.go), which skips the
// 64 KiB validation deliberately — the 256 KiB skill cap is enforced above
// in validateSkillValue.
func (p *skillsProfile) putActiveSkill(ctx context.Context, key, value, writer, sessionID string, tainted int, note string) (*memory.Entry, error) {
	if err := validateSkillKey(key); err != nil {
		return nil, err
	}
	if err := validateSkillValue(value); err != nil {
		return nil, err
	}
	return p.store.PutSkillActive(ctx, key, value, "skill", writer, sessionID, tainted, note)
}

// getActiveSkill returns the active skill for key, or memory.ErrNotFound.
func (p *skillsProfile) getActiveSkill(ctx context.Context, key string) (*memory.Entry, error) {
	if err := validateSkillKey(key); err != nil {
		return nil, err
	}
	return p.store.Get(ctx, key)
}

// forgetSkill tombstones the active skill for key (supersede + note), exactly
// like memory Forget — nothing is hard-deleted; the tombstone stays visible
// in skill_history.
func (p *skillsProfile) forgetSkill(ctx context.Context, key string) error {
	if err := validateSkillKey(key); err != nil {
		return err
	}
	return p.store.Forget(ctx, key)
}

// listActiveSkills returns all active skills (newest first).
func (p *skillsProfile) listActiveSkills(ctx context.Context) ([]*memory.Entry, error) {
	return p.store.List(ctx, "", memory.TierDurable, memory.StatusActive)
}

// searchSkills runs FTS5 search over active skills.
func (p *skillsProfile) searchSkills(ctx context.Context, query string) ([]*memory.Entry, error) {
	return p.store.Search(ctx, query, memory.TierDurable, false)
}

// historyForKey returns the full supersedes chain for key, newest→oldest,
// including tombstones. See skills_store.go for the chain-walking query.
func (p *skillsProfile) historyForKey(ctx context.Context, key string) ([]*memory.Entry, error) {
	if err := validateSkillKey(key); err != nil {
		return nil, err
	}
	return p.store.HistoryForKey(ctx, key)
}
