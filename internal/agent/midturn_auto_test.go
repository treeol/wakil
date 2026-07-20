package agent

import (
	"sync"
	"testing"

	"github.com/treeol/wakil/internal/policy"
)

// TestConsentSnapshot_DefaultZero verifies a fresh App with no SetConsent call
// returns the zero value (all false) — the safe default (no auto-approval).
func TestConsentSnapshot_DefaultZero(t *testing.T) {
	a := &App{}
	c := a.Consent()
	if c.AutoApprove || c.AllowDestructive || c.AllowReads {
		t.Errorf("fresh App consent should be all-false, got %+v", c)
	}
}

// TestConsentSnapshot_SetAndGet verifies the round-trip through SetConsent/Consent.
func TestConsentSnapshot_SetAndGet(t *testing.T) {
	a := &App{}
	a.SetConsent(ConsentSnapshot{AutoApprove: true, AllowDestructive: false, AllowReads: true})
	c := a.Consent()
	if !c.AutoApprove {
		t.Error("AutoApprove should be true")
	}
	if c.AllowDestructive {
		t.Error("AllowDestructive should be false")
	}
	if !c.AllowReads {
		t.Error("AllowReads should be true")
	}
}

// TestConsentSnapshot_SingleFieldMutators verifies SetAutoApprove etc. preserve
// the other fields (load-modify-store).
func TestConsentSnapshot_SingleFieldMutators(t *testing.T) {
	a := &App{}
	a.SetConsent(ConsentSnapshot{AutoApprove: true, AllowDestructive: true, AllowReads: true})

	a.SetAutoApprove(false)
	c := a.Consent()
	if c.AutoApprove {
		t.Error("AutoApprove should be false after SetAutoApprove(false)")
	}
	if !c.AllowDestructive {
		t.Error("AllowDestructive should be preserved as true")
	}
	if !c.AllowReads {
		t.Error("AllowReads should be preserved as true")
	}

	a.SetAllowDestructive(false)
	c = a.Consent()
	if c.AllowDestructive {
		t.Error("AllowDestructive should be false after SetAllowDestructive(false)")
	}

	a.SetAllowReads(false)
	c = a.Consent()
	if c.AllowReads {
		t.Error("AllowReads should be false after SetAllowReads(false)")
	}
}

// TestConsentSnapshot_AutoOffClearsDestructive verifies the pair invariant:
// /auto OFF clears both AutoApprove and AllowDestructive in a single atomic
// store, so a reader never sees AutoApprove=false + AllowDestructive=true.
func TestConsentSnapshot_AutoOffClearsDestructive(t *testing.T) {
	a := &App{}
	a.SetConsent(ConsentSnapshot{AutoApprove: true, AllowDestructive: true, AllowReads: true})

	// Simulate the /auto OFF path from HandleTUICommand.
	a.SetConsent(ConsentSnapshot{
		AutoApprove:      false,
		AllowDestructive: false,
		AllowReads:       a.Consent().AllowReads,
	})

	c := a.Consent()
	if c.AutoApprove {
		t.Error("AutoApprove should be false after /auto OFF")
	}
	if c.AllowDestructive {
		t.Error("AllowDestructive should be cleared when /auto goes OFF (pair invariant)")
	}
	if !c.AllowReads {
		t.Error("AllowReads should be preserved (not part of the auto/destructive pair)")
	}
}

// TestConsentSnapshot_NoTearing_RaceFree verifies that concurrent reads during
// writes never observe a torn snapshot. With plain bools, a reader could see
// AutoApprove=false + AllowDestructive=true between two separate field writes.
// The atomic.Value guarantees one load = one consistent snapshot.
//
// This test runs under `go test -race` — a data race would fail the test.
func TestConsentSnapshot_NoTearing_RaceFree(t *testing.T) {
	a := &App{}

	// Writer goroutine: repeatedly toggles between two consent states.
	// The critical pair is AutoApprove + AllowDestructive: when AutoApprove
	// is false, AllowDestructive must ALWAYS be false (the pair invariant).
	// A torn read (plain bools) could observe AutoApprove=false +
	// AllowDestructive=true.
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10000; i++ {
			if i%2 == 0 {
				a.SetConsent(ConsentSnapshot{
					AutoApprove:      true,
					AllowDestructive: true,
					AllowReads:       true,
				})
			} else {
				a.SetConsent(ConsentSnapshot{
					AutoApprove:      false,
					AllowDestructive: false,
					AllowReads:       false,
				})
			}
		}
		close(done)
	}()

	// Reader goroutine: reads the snapshot and checks the pair invariant.
	// With plain bools, this would eventually observe a torn state.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			c := a.Consent()
			// The pair invariant: AllowDestructive is only meaningful when
			// AutoApprove is true. If we ever see AutoApprove=false +
			// AllowDestructive=true, that's a torn read.
			if !c.AutoApprove && c.AllowDestructive {
				t.Errorf("torn snapshot: AutoApprove=false + AllowDestructive=true")
				return
			}
		}
	}()

	wg.Wait()
}

// TestConsentSnapshot_ConcurrentReadWrite_RaceFree is a broader race test:
// multiple readers and writers hammering the snapshot simultaneously. Runs
// under -race; any data race fails the test.
func TestConsentSnapshot_ConcurrentReadWrite_RaceFree(t *testing.T) {
	a := &App{}
	a.SetConsent(ConsentSnapshot{})

	var wg sync.WaitGroup
	const writers = 4
	const readers = 8
	const iterations = 5000

	// Writers: each toggles a single field via the typed mutator.
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				a.SetAutoApprove(i%2 == 0)
			}
		}()
	}

	// Readers: read the snapshot and sanity-check it.
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				_ = a.Consent()
			}
		}()
	}

	wg.Wait()
}

// TestConsentSnapshot_NoLostUpdate_SetAllowReadsVsRevokeAuto verifies the CAS
// retry loop prevents lost updates: the agent goroutine setting AllowReads(true)
// concurrent with the TUI goroutine revoking auto (clearing AutoApprove +
// AllowDestructive) must not lose either update. Without CAS, the revoke's
// store could clobber the AllowReads grant (or vice versa).
//
// This is the exact scenario Mashūra flagged: a mid-turn /auto revoke must not
// be undone by a concurrent AllowReads grant from tuiConfirmer.
func TestConsentSnapshot_NoLostUpdate_SetAllowReadsVsRevokeAuto(t *testing.T) {
	a := &App{}
	a.SetConsent(ConsentSnapshot{AutoApprove: true, AllowDestructive: true, AllowReads: false})

	var wg sync.WaitGroup
	const iterations = 5000

	// Writer 1 (agent goroutine): repeatedly grants AllowReads.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			a.SetAllowReads(true)
		}
	}()

	// Writer 2 (TUI goroutine): repeatedly revokes auto.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			a.RevokeAuto()
		}
	}()

	wg.Wait()

	// After all writes complete, the final state must be consistent:
	// - AutoApprove and AllowDestructive are false (last writer was RevokeAuto
	//   or SetAllowReads, but SetAllowReads preserves them as false since CAS
	//   reloads the current state).
	// - AllowReads is true (SetAllowReads(true) was the last write to that
	//   field, and RevokeAuto preserves it via CAS).
	c := a.Consent()
	if c.AutoApprove {
		t.Error("AutoApprove should be false after concurrent revokes")
	}
	if c.AllowDestructive {
		t.Error("AllowDestructive should be false after concurrent revokes")
	}
	// AllowReads should be true — the SetAllowReads(true) calls must not have
	// been lost by the concurrent RevokeAuto calls.
	if !c.AllowReads {
		t.Error("AllowReads grant was lost — CAS retry loop failed to preserve it")
	}
}

// TestPolicy_SetNil_Deactivates verifies that SetPolicy(nil) does not panic
// (atomic.Value.Store panics on nil interface) and that Policy() returns nil
// after deactivation.
func TestPolicy_SetNil_Deactivates(t *testing.T) {
	a := &App{}
	a.SetPolicy(nil)
	if a.Policy() != nil {
		t.Error("Policy() should return nil after SetPolicy(nil)")
	}
}

// TestPolicy_SetAndGet verifies the round-trip through SetPolicy/Policy.
func TestPolicy_SetAndGet(t *testing.T) {
	a := &App{}
	p := policy.Profile("ci")
	a.SetPolicy(p)
	got := a.Policy()
	if got == nil {
		t.Fatal("Policy() returned nil after SetPolicy")
	}
	if got.Name != "ci" {
		t.Errorf("Policy name = %q, want ci", got.Name)
	}
}
