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

// putActiveSkill writes an immediately-active durable skill, enforcing the
// create/update invariant atomically in the store transaction. expectExists:
// false for save_skill (create-only — fails with memory.ErrSkillExists if an
// active skill already exists), true for update_skill (requires-existing —
// fails with memory.ErrSkillNotFound if none). Anchors are always nil —
// global skills have no stable workspace root.
//
// The store enforces the 256 KiB skill cap at its boundary (see
// Store.PutSkillActive); validateSkillValue above is the wrapper-level
// pre-check that produces a clean error before the confirm prompt.
func (p *skillsProfile) putActiveSkill(ctx context.Context, key, value, writer, sessionID string, tainted int, expectExists bool, note string) (*memory.Entry, error) {
	if err := validateSkillKey(key); err != nil {
		return nil, err
	}
	if err := validateSkillValue(value); err != nil {
		return nil, err
	}
	return p.store.PutSkillActive(ctx, key, value, "skill", writer, sessionID, tainted, expectExists, note)
}

// getActiveSkill returns the active skill for key, or memory.ErrNotFound.
func (p *skillsProfile) getActiveSkill(ctx context.Context, key string) (*memory.Entry, error) {
	if err := validateSkillKey(key); err != nil {
		return nil, err
	}
	return p.store.Get(ctx, key)
}

// getSkillByID returns a skill entry by ID regardless of status (superseded
// versions stay readable until the store's auditability window prunes them).
// Used to reconstruct the exact template version a workspace file came from.
func (p *skillsProfile) getSkillByID(ctx context.Context, id int64) (*memory.Entry, error) {
	e, err := p.store.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if e.Kind != "skill" {
		return nil, memory.ErrNotFound
	}
	return e, nil
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

// searchSkills runs FTS5 search over active skills. The query is a raw user
// string — it is sanitized inside Store.Search.
func (p *skillsProfile) searchSkills(ctx context.Context, query string) ([]*memory.Entry, error) {
	return p.store.Search(ctx, query, memory.TierDurable, false)
}

// searchSkillsRaw runs FTS5 search over active skills with a caller-prepared
// FTS5 MATCH expression (already quoted/escaped). Used by the turn-entry
// retrieval pass, which builds its own OR-of-phrases query.
func (p *skillsProfile) searchSkillsRaw(ctx context.Context, matchExpr string) ([]*memory.Entry, error) {
	return p.store.SearchRaw(ctx, matchExpr, memory.TierDurable, false)
}

// historyForKey returns the full supersedes chain for key, newest→oldest,
// including tombstones. See skills_store.go for the chain-walking query.
func (p *skillsProfile) historyForKey(ctx context.Context, key string) ([]*memory.Entry, error) {
	if err := validateSkillKey(key); err != nil {
		return nil, err
	}
	return p.store.HistoryForKey(ctx, key)
}

// Auto-created skill bookkeeping. Skills written by the skill-creation pipeline
// carry a note starting with one of these prefixes so retrieval and the /skills
// commands can tell them apart from hand-written skills.
const (
	// autoSkillNoteUnconfirmed marks an auto-created active skill that has not
	// been confirmed by the user yet: listed/searchable (with a marker) but NOT
	// auto-injected into turns by the retrieval pass, and load_skill /
	// apply_skill_template / check_skill_template refuse it until the user runs
	// /skills confirm.
	autoSkillNoteUnconfirmed = "auto-unconfirmed"
	// autoSkillNoteConfirmed is set by /skills confirm.
	autoSkillNoteConfirmed = "auto-confirmed"
	// autoDraftNotePrefix marks drafts produced by the pipeline.
	autoDraftNotePrefix = "auto-draft"
)

// isUnconfirmedAutoSkill reports whether e is an auto-created skill still
// awaiting /skills confirm.
func isUnconfirmedAutoSkill(e *memory.Entry) bool {
	return e != nil && strings.HasPrefix(e.Note, autoSkillNoteUnconfirmed)
}

// unconfirmedSkillMarker is the inline marker shown in model-facing listings.
const unconfirmedSkillMarker = " (unconfirmed auto-created skill — not usable until the user runs /skills confirm)"

// unconfirmedSkillRefusal is the model-facing refusal for content access.
func unconfirmedSkillRefusal(key string) string {
	return "not available: skill " + key + " was created automatically and has not been confirmed by the user yet — ask the user to review it and run /skills confirm " + key
}

// isAutoSkill reports whether e was created by the pipeline (confirmed or not).
func isAutoSkill(e *memory.Entry) bool {
	// An ACTIVE row still carrying the draft prefix was approved by the user
	// before approval rewrote the note — it counts as confirmed.
	return e != nil && (strings.HasPrefix(e.Note, autoSkillNoteUnconfirmed) || strings.HasPrefix(e.Note, autoSkillNoteConfirmed) ||
		(e.Status == memory.StatusActive && strings.HasPrefix(e.Note, autoDraftNotePrefix)))
}

// putSkillDraft stores a pending draft (status proposed).
func (p *skillsProfile) putSkillDraft(ctx context.Context, key, value, writer, sessionID string, tainted int, note string) (*memory.Entry, error) {
	if err := validateSkillKey(key); err != nil {
		return nil, err
	}
	if err := validateSkillValue(value); err != nil {
		return nil, err
	}
	return p.store.PutSkillDraft(ctx, key, value, "skill", writer, sessionID, tainted, note)
}

// listSkillDrafts returns pending skill drafts, newest first.
func (p *skillsProfile) listSkillDrafts(ctx context.Context) ([]*memory.Entry, error) {
	return p.store.ListSkillDrafts(ctx, "skill")
}

// rejectSkillDraft rejects a pending draft by ID.
func (p *skillsProfile) rejectSkillDraft(ctx context.Context, id int64, reason string) error {
	return p.store.Reject(ctx, id, reason)
}

// promoteSkillDraftAtomic activates a draft in one store transaction: it
// refuses (memory.ErrSkillExists, with the blocking entry) when an active skill
// holds the key and allowReplace is false, and rewrites the note when note != "".
func (p *skillsProfile) promoteSkillDraftAtomic(ctx context.Context, id int64, allowReplace bool, note, by string) (promoted, replaced *memory.Entry, err error) {
	return p.store.PromoteSkillDraft(ctx, id, allowReplace, note, by)
}

// setSkillNote replaces the note of one skill row.
func (p *skillsProfile) setSkillNote(ctx context.Context, id int64, note string) error {
	return p.store.SetEntryNote(ctx, id, note)
}

// reserveDaily reserves one unit of a named per-day budget.
func (p *skillsProfile) reserveDaily(ctx context.Context, name, day string, max int) (bool, error) {
	return p.store.ReserveDaily(ctx, name, day, max)
}

// releaseDaily returns a unit reserved by reserveDaily.
func (p *skillsProfile) releaseDaily(ctx context.Context, name, day string) {
	p.store.ReleaseDaily(ctx, name, day)
}
