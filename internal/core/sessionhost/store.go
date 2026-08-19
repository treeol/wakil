package sessionhost

import (
	"context"
	"fmt"
	"sync"

	"github.com/treeol/wakil/internal/core"
	"github.com/treeol/wakil/internal/core/event"
)

// Store is the seam the host builds against: the D3 store contracts. P1 swaps
// MemLog for a SQLite-backed implementation without touching the service layer.
type Store interface {
	core.EventAppender
	core.EventLog
}

// MemLog is the P0 in-memory implementation of Store. It holds per-session
// durable-event logs with contiguous sequence assignment.
//
// Concurrency: Append/Read/LastSeq are all safe for concurrent use. A single
// RWMutex serializes appends across all sessions — a deliberate P0
// simplification (one process, few sessions). Per-session contiguity is
// preserved because Append atomically assigns nextSeq and stores inside one
// critical section, and per-session ordering is preserved by the host's
// single-producer invariant (see host.go): the executor is the only producer of
// a session's turn/session events; the only pre-executor appends (SessionCreated
// on create, SessionError{daemon_restart} on recovery) happen BEFORE the
// executor goroutine is started, so no producer ever races the executor.
type MemLog struct {
	mu sync.RWMutex
	// sessions maps a session to its durable log. A missing entry is equivalent
	// to an empty log: Read/LastSeq return empty/0 rather than an error, matching
	// the EventLog contract (a session with no events yet is valid).
	sessions map[event.SessionID]*memSession
}

type memSession struct {
	seq    event.Seq
	events []event.Event
}

// NewMemLog returns an empty in-memory log.
func NewMemLog() *MemLog {
	return &MemLog{sessions: make(map[event.SessionID]*memSession)}
}

// Append implements core.EventAppender. It validates the draft, rejects
// ephemeral drafts, assigns the next contiguous sequence for the session,
// stores the event, and returns the committed event. Seq and durability become
// visible as one atomic step.
func (m *MemLog) Append(ctx context.Context, draft event.Event) (event.Event, error) {
	if err := ctx.Err(); err != nil {
		return event.Event{}, err
	}
	if err := draft.ValidateDraft(); err != nil {
		return event.Event{}, err
	}
	if draft.Kind.Class() == event.ClassEphemeral {
		return event.Event{}, fmt.Errorf("sessionhost: append rejected ephemeral kind %q (durable log only)", draft.Kind)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	ms := m.sessions[draft.SessionID]
	if ms == nil {
		ms = &memSession{}
		m.sessions[draft.SessionID] = ms
	}
	ms.seq++
	committed := draft
	committed.Seq = ms.seq
	ms.events = append(ms.events, committed)
	return committed, nil
}

// Read implements core.EventLog. It returns durable events with seq > after,
// ascending, up to limit entries. limit <= 0 means "no limit" (bounded only by
// the session's history). A missing session returns nil (empty log).
func (m *MemLog) Read(ctx context.Context, sessionID event.SessionID, after event.Seq, limit int) ([]event.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	ms := m.sessions[sessionID]
	if ms == nil {
		return nil, nil
	}

	var out []event.Event
	for _, e := range ms.events {
		if e.Seq <= after {
			continue
		}
		out = append(out, e)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// LastSeq implements core.EventLog. It returns the highest committed durable
// seq for the session, or 0 if none (or the session does not exist).
func (m *MemLog) LastSeq(ctx context.Context, sessionID event.SessionID) (event.Seq, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	if ms := m.sessions[sessionID]; ms != nil {
		return ms.seq, nil
	}
	return 0, nil
}
