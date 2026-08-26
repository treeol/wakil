package wiring

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/agent"
	"github.com/treeol/wakil/internal/config"
	"github.com/treeol/wakil/internal/core/event"
	"github.com/treeol/wakil/internal/core/sessionhost"
	"github.com/treeol/wakil/internal/proxy"
)

// testApp builds a minimal *agent.App for factory lifecycle tests. It does not
// need a real backend or executor — the TurnFunc is never called in these
// tests (we only test claim/release, not turn execution).
func testApp() *agent.App {
	return &agent.App{
		Cfg:    config.DefaultConfig(),
		Client: &proxy.Client{ChatID: "test-chat"},
	}
}

// TestFactoryClaimReleaseReclaim verifies the 7b1 release path: claim an App,
// release it, then claim the same pointer again. In production rotation, a
// FRESH App pointer is built for the new conversation (so reclaim of the same
// pointer does not happen in practice). This test verifies the lifecycle
// mechanism: that Release properly cleans the appOwners entry so a claim
// succeeds after release, and that the released handle's TurnFunc rejects
// new turns.
func TestFactoryClaimReleaseReclaim(t *testing.T) {
	// Ensure a clean appOwners state (other tests may have claimed apps).
	appOwnersMu.Lock()
	appOwners = map[*agent.App]*hostTurn{}
	appOwnersMu.Unlock()

	app := testApp()
	h1, err := NewHostTurnHandle(app)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}

	// A second claim of the same App must fail (ErrAppInUse).
	if _, err := NewHostTurnHandle(app); !errors.Is(err, ErrAppInUse) {
		t.Fatalf("second claim should fail with ErrAppInUse, got %v", err)
	}

	// Release the first claim.
	if err := h1.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	// After release, a new claim of the same App must succeed.
	h2, err := NewHostTurnHandle(app)
	if err != nil {
		t.Fatalf("reclaim after release: %v", err)
	}
	defer h2.Release()
}

// TestReleaseIdempotent verifies that calling Release twice is safe.
func TestReleaseIdempotent(t *testing.T) {
	appOwnersMu.Lock()
	appOwners = map[*agent.App]*hostTurn{}
	appOwnersMu.Unlock()

	app := testApp()
	h, err := NewHostTurnHandle(app)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := h.Release(); err != nil {
		t.Fatalf("first release: %v", err)
	}
	// Second release is a no-op, not an error.
	if err := h.Release(); err != nil {
		t.Fatalf("second release should be no-op, got %v", err)
	}
}

// TestReleaseRejectsActiveTurn verifies that Release returns ErrTurnActive when
// a turn is in flight. We simulate an active turn by marking turnActive directly
// (the run method sets/clears it around the turn execution).
func TestReleaseRejectsActiveTurn(t *testing.T) {
	appOwnersMu.Lock()
	appOwners = map[*agent.App]*hostTurn{}
	appOwnersMu.Unlock()

	app := testApp()
	h, err := NewHostTurnHandle(app)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Simulate an active turn.
	h.ht.mu.Lock()
	h.ht.turnActive = true
	h.ht.mu.Unlock()

	// Release must reject.
	if err := h.Release(); !errors.Is(err, ErrTurnActive) {
		t.Fatalf("release during active turn should return ErrTurnActive, got %v", err)
	}

	// Clear the active flag and release again — must succeed.
	h.ht.mu.Lock()
	h.ht.turnActive = false
	h.ht.mu.Unlock()

	if err := h.Release(); err != nil {
		t.Fatalf("release after clearing turnActive: %v", err)
	}
}

// TestRunRejectsReleasedTurn verifies that a TurnFunc whose handle was released
// before the turn started returns an internal error (the turn goroutine must
// not run against a freed App).
func TestRunRejectsReleasedTurn(t *testing.T) {
	appOwnersMu.Lock()
	appOwners = map[*agent.App]*hostTurn{}
	appOwnersMu.Unlock()

	app := testApp()
	h, err := NewHostTurnHandle(app)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Release before the turn starts.
	if err := h.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	// The TurnFunc must reject the turn.
	_, err = h.Turn(context.Background(), sessionhost.TurnInput{
		SessionID: "ses_test_1",
		TurnID:   "trn_test_1",
		Emit:     &nullEmitter{},
	})
	if err == nil {
		t.Fatal("TurnFunc should error on released handle")
	}
	if !errors.Is(err, sessionhost.ErrInternal) {
		t.Errorf("error should wrap ErrInternal, got %v", err)
	}
}

// TestConcurrentClaimRelease verifies the factory is safe under concurrent
// claim/release. Multiple goroutines racing to claim/release different Apps
// must not panic or leave appOwners in an inconsistent state.
func TestConcurrentClaimRelease(t *testing.T) {
	appOwnersMu.Lock()
	appOwners = map[*agent.App]*hostTurn{}
	appOwnersMu.Unlock()

	const N = 20
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			app := testApp()
			h, err := NewHostTurnHandle(app)
			if err != nil {
				// ErrAppInUse is fine if another goroutine claimed the same
				// pointer (unlikely with distinct testApp() calls, but the
				// map is global — guard against it).
				if errors.Is(err, ErrAppInUse) {
					return
				}
				t.Errorf("claim: %v", err)
				return
			}
			time.Sleep(time.Millisecond)
			if err := h.Release(); err != nil {
				t.Errorf("release: %v", err)
			}
		}()
	}
	wg.Wait()
}

// nullEmitter is a no-op Emitter for tests that never actually emit events.
type nullEmitter struct{}

func (n *nullEmitter) Emit(kind event.Kind, payload any) error { return nil }
func (n *nullEmitter) Notify(kind event.Kind, payload any)     {}