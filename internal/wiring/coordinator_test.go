package wiring

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// errTest is a sentinel error for callback failures.
var errTest = errors.New("test callback error")

func TestNewTransitionCoordinator(t *testing.T) {
	c := NewTransitionCoordinator()
	if c == nil {
		t.Fatal("NewTransitionCoordinator returned nil")
	}
}

func TestWithTransition_RunsCallbackWhenIdle(t *testing.T) {
	c := NewTransitionCoordinator()
	called := false
	err := c.WithTransition(func() error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("WithTransition: %v", err)
	}
	if !called {
		t.Error("callback was not called")
	}
}

func TestWithTransition_PropagatesCallbackError(t *testing.T) {
	c := NewTransitionCoordinator()
	err := c.WithTransition(func() error {
		return errTest
	})
	if !errors.Is(err, errTest) {
		t.Fatalf("expected errTest, got %v", err)
	}
}

func TestWithTransition_RejectsWhenTurnActive(t *testing.T) {
	c := NewTransitionCoordinator()
	if err := c.WithTurnStart(func() error { return nil }); err != nil {
		t.Fatalf("WithTurnStart: %v", err)
	}
	called := false
	err := c.WithTransition(func() error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrTurnActiveCoord) {
		t.Fatalf("expected ErrTurnActiveCoord, got %v", err)
	}
	if called {
		t.Error("callback should not run when turn is active")
	}
}

func TestWithIdleMaintenance_RunsCallbackWhenIdle(t *testing.T) {
	c := NewTransitionCoordinator()
	called := false
	err := c.WithIdleMaintenance(func() error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("WithIdleMaintenance: %v", err)
	}
	if !called {
		t.Error("callback was not called")
	}
}

func TestWithIdleMaintenance_PropagatesCallbackError(t *testing.T) {
	c := NewTransitionCoordinator()
	err := c.WithIdleMaintenance(func() error {
		return errTest
	})
	if !errors.Is(err, errTest) {
		t.Fatalf("expected errTest, got %v", err)
	}
}

func TestWithIdleMaintenance_RejectsWhenTurnActive(t *testing.T) {
	c := NewTransitionCoordinator()
	if err := c.WithTurnStart(func() error { return nil }); err != nil {
		t.Fatalf("WithTurnStart: %v", err)
	}
	called := false
	err := c.WithIdleMaintenance(func() error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrTurnActiveCoord) {
		t.Fatalf("expected ErrTurnActiveCoord, got %v", err)
	}
	if called {
		t.Error("callback should not run when turn is active")
	}
}

func TestWithTurnStart_SucceedsWhenIdle(t *testing.T) {
	c := NewTransitionCoordinator()
	called := false
	err := c.WithTurnStart(func() error {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("WithTurnStart: %v", err)
	}
	if !called {
		t.Error("callback was not called")
	}
}

func TestWithTurnStart_RejectsSecondStart(t *testing.T) {
	c := NewTransitionCoordinator()
	if err := c.WithTurnStart(func() error { return nil }); err != nil {
		t.Fatalf("first WithTurnStart: %v", err)
	}
	err := c.WithTurnStart(func() error { return nil })
	if !errors.Is(err, ErrTransitionActive) {
		t.Fatalf("expected ErrTransitionActive, got %v", err)
	}
}

func TestWithTurnStart_PropagatesCallbackError(t *testing.T) {
	c := NewTransitionCoordinator()
	err := c.WithTurnStart(func() error {
		return errTest
	})
	if !errors.Is(err, errTest) {
		t.Fatalf("expected errTest, got %v", err)
	}
	// Failed turn start must NOT set turnActive — subsequent start should succeed.
	if err := c.WithTurnStart(func() error { return nil }); err != nil {
		t.Fatalf("second WithTurnStart after failed callback: %v", err)
	}
}

func TestClearTurnActive_PermitsTransitionsAfterClear(t *testing.T) {
	c := NewTransitionCoordinator()
	if err := c.WithTurnStart(func() error { return nil }); err != nil {
		t.Fatalf("WithTurnStart: %v", err)
	}
	// Transition should be rejected while turn is active.
	if err := c.WithTransition(func() error { return nil }); !errors.Is(err, ErrTurnActiveCoord) {
		t.Fatalf("expected ErrTurnActiveCoord, got %v", err)
	}
	c.ClearTurnActive()
	// After clearing, transition should succeed.
	if err := c.WithTransition(func() error { return nil }); err != nil {
		t.Fatalf("WithTransition after ClearTurnActive: %v", err)
	}
}

func TestClearTurnActive_Idempotent(t *testing.T) {
	c := NewTransitionCoordinator()
	c.ClearTurnActive()
	c.ClearTurnActive()
	// Should still be able to start a turn.
	if err := c.WithTurnStart(func() error { return nil }); err != nil {
		t.Fatalf("WithTurnStart after double clear: %v", err)
	}
}

func TestWithTurnStart_WaitsForTransition(t *testing.T) {
	c := NewTransitionCoordinator()
	started := make(chan struct{})
	release := make(chan struct{})

	// Hold the coordinator lock with a long transition.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.WithTransition(func() error {
			close(started)
			<-release
			return nil
		})
	}()

	<-started // ensure the transition holds the lock

	// WithTurnStart should block until the transition releases the lock.
	done := make(chan error)
	go func() {
		done <- c.WithTurnStart(func() error { return nil })
	}()

	select {
	case <-done:
		t.Fatal("WithTurnStart did not block for transition")
	case <-time.After(50 * time.Millisecond):
		// Good — it's blocked.
	}

	close(release)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WithTurnStart after transition: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WithTurnStart did not complete after transition released")
	}
}

func TestWithTransition_BlocksTurnStart(t *testing.T) {
	c := NewTransitionCoordinator()
	started := make(chan struct{})
	release := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		c.WithTransition(func() error {
			close(started)
			<-release
			return nil
		})
	}()

	<-started

	// A turn start must wait until the transition finishes.
	done := make(chan error)
	go func() {
		done <- c.WithTurnStart(func() error { return nil })
	}()

	select {
	case <-done:
		t.Fatal("WithTurnStart did not block during transition")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	wg.Wait()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("WithTurnStart: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WithTurnStart timed out")
	}
}
