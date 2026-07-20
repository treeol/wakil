package agent

// ConsentSnapshot is the atomic consent state for the session, replacing the
// former plain-bool fields AutoApprove, AllowDestructive, and AllowReads on
// App. One Consent() load returns a consistent view of all three bools — no
// tearing across the pair (e.g. AutoApprove=false + AllowDestructive=true
// observed between two separate field loads, which was possible when they
// were independent plain bools).
//
// The data race: those bools were written on the TUI goroutine (/auto toggle
// in HandleTUICommand) and the agent/turn goroutine (tuiConfirmer sets
// AllowReads after a confirm) while being read concurrently by the turn
// goroutine at every approval gate. Plain bool reads/writes are not
// synchronized — a Go data race under -race. The atomic.Value makes every
// load and store a single atomic operation, and the snapshot struct ensures
// a reader never sees a half-updated pair.
type ConsentSnapshot struct {
	AutoApprove      bool
	AllowDestructive bool
	AllowReads       bool
}

// Consent returns the current consent snapshot. One load = one consistent
// view of all three consent bools. Safe to call from any goroutine.
//
// Returns the zero value (all false) before the first SetConsent — the
// correct default for a fresh App (no auto-approval, no destructive grant,
// no read grant). App construction (app_builder.go) calls SetConsent
// immediately after the composite literal, so the zero-value path is only
// hit by tests that construct a bare &App{} without initializing consent.
func (a *App) Consent() ConsentSnapshot {
	v := a.consent.Load()
	if v == nil {
		return ConsentSnapshot{}
	}
	return v.(ConsentSnapshot)
}

// SetConsent atomically stores a new consent snapshot. Use this when the
// caller has already computed the full desired state (e.g. /auto OFF clears
// both AutoApprove and AllowDestructive in a single store). For single-field
// changes that must preserve the other fields, use the typed mutators
// (SetAutoApprove etc.), which use a CAS retry loop to avoid lost updates
// from concurrent writers.
func (a *App) SetConsent(s ConsentSnapshot) {
	a.consent.Store(s)
}

// updateConsent performs an atomic compare-and-swap retry loop to update a
// single field of the consent snapshot without losing concurrent updates to
// the other fields. The mutate callback receives the current snapshot and
// returns the desired one; if another goroutine stored between the load and
// the CAS, the loop reloads and retries.
//
// This fixes the lost-update problem: e.g. the agent goroutine calling
// SetAllowReads(true) concurrent with the TUI goroutine calling /auto OFF
// (which stores a full snapshot). Without CAS, the TUI's store could
// clobber the agent's AllowReads=true. With CAS, the TUI sees the updated
// AllowReads on retry and preserves it.
//
// The nil-stored case (consent never initialized via SetConsent) is handled
// by comparing against the raw Load() result, not the Consent() zero-value
// fallback — a fresh App's first mutator must Store, not CAS against nil.
func (a *App) updateConsent(mutate func(ConsentSnapshot) ConsentSnapshot) {
	for {
		raw := a.consent.Load()
		var old ConsentSnapshot
		if raw != nil {
			old = raw.(ConsentSnapshot)
		}
		new := mutate(old)
		if old == new {
			return // no change needed
		}
		if raw == nil {
			// First store on a fresh App — use Store, not CAS (CAS against
			// nil would loop forever since mutate produces a non-nil snapshot).
			a.consent.Store(new)
			return
		}
		if a.consent.CompareAndSwap(old, new) {
			return
		}
		// CAS failed — another goroutine stored between our load and CAS.
		// Reload and retry; the mutation is idempotent w.r.t. the target field.
	}
}

// SetAutoApprove atomically updates the AutoApprove field, preserving the
// other two via a CAS retry loop. Safe under concurrent writers to other
// fields.
func (a *App) SetAutoApprove(v bool) {
	a.updateConsent(func(s ConsentSnapshot) ConsentSnapshot {
		s.AutoApprove = v
		return s
	})
}

// SetAllowDestructive atomically updates the AllowDestructive field,
// preserving the other two via a CAS retry loop.
func (a *App) SetAllowDestructive(v bool) {
	a.updateConsent(func(s ConsentSnapshot) ConsentSnapshot {
		s.AllowDestructive = v
		return s
	})
}

// SetAllowReads atomically updates the AllowReads field, preserving the
// other two via a CAS retry loop.
func (a *App) SetAllowReads(v bool) {
	a.updateConsent(func(s ConsentSnapshot) ConsentSnapshot {
		s.AllowReads = v
		return s
	})
}

// RevokeAuto atomically clears both AutoApprove and AllowDestructive while
// preserving AllowReads. This is the /auto OFF operation: the destructive
// grant never outlives the auto session it was given for (pair invariant).
// Uses a CAS retry loop so a concurrent AllowReads write from the agent
// goroutine is not lost. Both fields are cleared in a single store, so a
// reader never observes AutoApprove=false + AllowDestructive=true.
func (a *App) RevokeAuto() {
	a.updateConsent(func(s ConsentSnapshot) ConsentSnapshot {
		s.AutoApprove = false
		s.AllowDestructive = false
		return s
	})
}
