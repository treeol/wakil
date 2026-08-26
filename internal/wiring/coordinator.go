package wiring

// coordinator.go: TransitionCoordinator serializes session transitions
// (LoadSession, InitNewSession) and idle maintenance (Compact) against turn
// starts.
//
// Design: the coordinator holds a turnActive flag (set by WithTurnStart,
// cleared by ClearTurnActive when the turn ends). Transitions and idle
// maintenance reject with ErrTurnActive if a turn is active. Turn starts
// block if a transition or maintenance is in progress (coordinator.mu).
//
// The coordinator lock is NOT held during the turn body — only during
// claim+activate. This avoids deadlock risk if the turn continuation
// synchronously starts the next turn. The turnActive flag provides the
// exclusion: transitions and Compact see it set and reject, while the
// turn goroutine runs without holding the lock.
//
// Lock ordering: coordinator.mu is the top-level coordination lock. It is
// acquired before any App-level locks (stateMu, convMu, saveMu) and before
// hostTurn.mu. The coordinator is shared between the SessionStateHandler
// (transitions) and hostTurn (turn starts).

import (
	"errors"
	"sync"
)

// ErrTransitionActive is returned when a turn start is attempted while a
// transition or maintenance is in progress.
var ErrTransitionActive = errors.New("wiring: session transition in progress")

// ErrTurnActiveCoord is returned when a transition or maintenance is
// attempted while a turn is active.
var ErrTurnActiveCoord = errors.New("wiring: turn is active")

// TransitionCoordinator serializes session transitions against turn starts.
type TransitionCoordinator struct {
	mu         sync.Mutex
	turnActive bool
}

// NewTransitionCoordinator creates a coordinator. One per daemon lifetime —
// shared between the SessionStateHandler and the hostTurn.
func NewTransitionCoordinator() *TransitionCoordinator {
	return &TransitionCoordinator{}
}

// WithTransition runs fn while holding the coordinator lock. If a turn is
// active, it returns ErrTurnActiveCoord without running fn. Callers:
// LoadSession, InitNewSession handlers.
func (c *TransitionCoordinator) WithTransition(fn func() error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.turnActive {
		return ErrTurnActiveCoord
	}
	return fn()
}

// WithTurnStart runs fn while holding the coordinator lock. If a transition
// or another turn is in progress, fn blocks until the lock is available, then
// runs. If fn succeeds, turnActive is set to true. The caller MUST call
// ClearTurnActive when the turn ends (typically via defer in run).
func (c *TransitionCoordinator) WithTurnStart(fn func() error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.turnActive {
		return ErrTransitionActive
	}
	if err := fn(); err != nil {
		return err
	}
	c.turnActive = true
	return nil
}

// ClearTurnActive clears the turn-active flag. Called by the hostTurn's
// defer when the turn ends.
func (c *TransitionCoordinator) ClearTurnActive() {
	c.mu.Lock()
	c.turnActive = false
	c.mu.Unlock()
}

// WithIdleMaintenance runs fn while holding the coordinator lock. If a turn
// is active, it returns ErrTurnActiveCoord without running fn. Callers:
// Compact handler. This prevents Compact from racing with the turn
// goroutine's convMu writes.
func (c *TransitionCoordinator) WithIdleMaintenance(fn func() error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.turnActive {
		return ErrTurnActiveCoord
	}
	return fn()
}
