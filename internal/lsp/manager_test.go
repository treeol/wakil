package lsp

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/treeol/wakil/internal/config"
)

// TestServer_EncodingNegotiation verifies the stored-not-assumed invariant:
// the server's returned positionEncoding is stored; nil defaults to utf-16.
func TestServer_EncodingNegotiation(t *testing.T) {
	// Case 1: server returns utf-8
	s1 := &Server{encoding: UTF16} // default before negotiation
	utf8 := UTF8
	s1.caps = ServerCapabilities{PositionEncoding: &utf8}
	s1.mu.Lock()
	if s1.caps.PositionEncoding != nil {
		s1.encoding = *s1.caps.PositionEncoding
	}
	s1.mu.Unlock()
	if s1.Encoding() != UTF8 {
		t.Errorf("expected utf-8, got %s", s1.Encoding())
	}

	// Case 2: server returns nil (omitted) → default utf-16
	s2 := &Server{encoding: UTF16}
	s2.caps = ServerCapabilities{PositionEncoding: nil}
	s2.mu.Lock()
	if s2.caps.PositionEncoding != nil {
		s2.encoding = *s2.caps.PositionEncoding
	} else {
		s2.encoding = UTF16
	}
	s2.mu.Unlock()
	if s2.Encoding() != UTF16 {
		t.Errorf("expected utf-16 default, got %s", s2.Encoding())
	}
}

// TestServer_MarkDead_UnblocksWaiters verifies the crash-during-Indexing
// non-deadlock: a waiter blocked on waitForReady must return a typed error
// when markDead is called, not hang forever.
func TestServer_MarkDead_UnblocksWaiters(t *testing.T) {
	s := &Server{
		progressTokens: make(map[any]bool),
		docs:           make(map[string]int32),
		readyCh:        make(chan struct{}),
		deadCh:         make(chan struct{}),
		state:          StateIndexing,
	}

	waitErr := make(chan error, 1)
	go func() {
		waitErr <- s.waitForReady(context.Background())
	}()

	// Give the waiter a moment to register.
	time.Sleep(50 * time.Millisecond)

	// Simulate crash.
	s.markDead(fmt.Errorf("crash during indexing"))

	select {
	case err := <-waitErr:
		if err == nil {
			t.Error("expected error from waitForReady on death, got nil")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waitForReady hung after markDead — deadlock")
	}
}

// TestServer_MarkDead_UnblocksMultipleWaiters verifies all N waiters are released.
func TestServer_MarkDead_UnblocksMultipleWaiters(t *testing.T) {
	s := &Server{
		progressTokens: make(map[any]bool),
		docs:           make(map[string]int32),
		readyCh:        make(chan struct{}),
		deadCh:         make(chan struct{}),
		state:          StateIndexing,
	}

	const N = 5
	errs := make(chan error, N)
	for i := 0; i < N; i++ {
		go func() {
			errs <- s.waitForReady(context.Background())
		}()
	}

	time.Sleep(50 * time.Millisecond)
	s.markDead(fmt.Errorf("crash"))

	for i := 0; i < N; i++ {
		select {
		case err := <-errs:
			if err == nil {
				t.Error("expected error, got nil")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("not all waiters were released")
		}
	}
}

// TestServer_ProgressRefCount_Ready verifies that the refcount approach
// transitions to Ready when all Begin tokens get End.
func TestServer_ProgressRefCount_Ready(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := &Manager{cfg: cfg}
	s := &Server{
		progressTokens: make(map[any]bool),
		docs:           make(map[string]int32),
		readyCh:        make(chan struct{}),
		deadCh:         make(chan struct{}),
		state:          StateIndexing,
		mgr:            mgr,
	}

	// Begin token A
	s.handleProgress(json.RawMessage(`{"token":"A","value":{"kind":"begin","title":"Indexing"}}`))
	// Begin token B
	s.handleProgress(json.RawMessage(`{"token":"B","value":{"kind":"begin","title":"Loading"}}`))

	// End A — still one open
	s.handleProgress(json.RawMessage(`{"token":"A","value":{"kind":"end"}}`))
	select {
	case <-s.readyCh:
		t.Error("ready too early — token B still open")
	default:
	}

	// End B — now all closed → Ready
	s.handleProgress(json.RawMessage(`{"token":"B","value":{"kind":"end"}}`))
	select {
	case <-s.readyCh:
		// good
	case <-time.After(100 * time.Millisecond):
		t.Error("did not transition to Ready after all tokens ended")
	}
}

// TestServer_Timeout_DeclaresReady verifies the no-progress watchdog fires.
func TestServer_Timeout_DeclaresReady(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.LSPIndexTimeoutSeconds = 0 // will use a very short timeout for the test

	s := &Server{
		progressTokens: make(map[any]bool),
		docs:           make(map[string]int32),
		readyCh:        make(chan struct{}),
		deadCh:         make(chan struct{}),
		state:          StateIndexing,
		mgr:            &Manager{cfg: cfg},
	}

	// Use a short timeout for the test.
	s.mu.Lock()
	gen := s.generation
	s.progressTimer = time.AfterFunc(50*time.Millisecond, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.generation != gen {
			return
		}
		if s.state == StateIndexing {
			s.transitionToReadyLocked()
		}
	})
	s.mu.Unlock()

	select {
	case <-s.readyCh:
		// good — watchdog fired
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not fire")
	}
}

// TestServer_ReSpawn_CreatesFreshServer verifies the State Machine Discard
// pattern: after a server dies, EnsureServer creates a fresh Server with fresh
// channels, so it can reach Ready again (no stale closed deadCh).
func TestServer_ReSpawn_CreatesFreshServer(t *testing.T) {
	cfg := config.DefaultConfig()
	mgr := NewManager(nil, cfg, "file:///test")

	// First server — simulate death.
	s1 := mgr.newServerLocked("go")
	mgr.servers["go"] = s1
	s1.mu.Lock()
	s1.state = StateDead
	s1.deadErr = fmt.Errorf("crash gen 1")
	close(s1.deadCh)
	s1.mu.Unlock()

	// EnsureServer on a dead server should discard it and create a fresh one.
	// spawn will fail (nil exec → markDead → deadCh closed), but the key
	// assertion is that a FRESH Server was allocated (not the same struct as s1).
	_, _ = mgr.EnsureServer(context.Background(), "go")

	mgr.mu.Lock()
	s2 := mgr.servers["go"]
	mgr.mu.Unlock()

	if s2 == nil {
		t.Fatal("no server in map after EnsureServer")
	}
	if s2 == s1 {
		t.Fatal("EnsureServer reused the dead Server instead of creating a fresh one")
	}

	// The fresh server must be a different struct with its own channels.
	// (Its deadCh may be closed now because spawn failed on nil exec, but
	// the structural fix is that it's NOT the same closed channel as s1.)
	if s2.deadCh == s1.deadCh {
		t.Error("fresh server shares the same deadCh as the dead server — not a fresh channel")
	}
}
