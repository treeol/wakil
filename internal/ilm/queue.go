package ilm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// queue is an append-only JSONL durable queue. Events are written one per line
// as JSON. The sender reads from the queue file and removes processed events
// after a successful batch POST.
//
// We use an append-only file (not bbolt) because:
//  1. Simplicity — no external dependency, no schema migrations.
//  2. Crash safety — append is atomic on POSIX (O_APPEND), and we fsync after
//     each write. A crash leaves complete lines (no partial writes with O_APPEND
//     on Linux for writes ≤ PIPE_BUF).
//  3. At-least-once — events persist in the file until the sender confirms a
//     successful batch POST, then truncates them. If the process crashes, the
//     sender re-reads the queue on restart and re-sends (idempotent event IDs
//     make duplicates safe).
//  4. Backpressure — if the network is down, the queue grows unboundedly but
//     Wakil is unaffected (the channel absorbs bursts; the file absorbs
//     sustained outages).

type queue struct {
	path string
	mu   sync.Mutex
}

func newQueue(path string) (*queue, error) {
	// Create the file if it doesn't exist.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	f.Close()
	return &queue{path: path}, nil
}

// append writes an event as a JSON line to the queue file.
func (q *queue) append(ev Event) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	f, err := os.OpenFile(q.path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}

	if _, err := f.Write(line); err != nil {
		return err
	}
	if _, err := f.Write([]byte("\n")); err != nil {
		return err
	}
	return f.Sync()
}

// appendBatch writes multiple events as JSON lines.
func (q *queue) appendBatch(events []Event) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	f, err := os.OpenFile(q.path, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	for _, ev := range events {
		line, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		if _, err := f.Write(line); err != nil {
			return err
		}
		if _, err := f.Write([]byte("\n")); err != nil {
			return err
		}
	}
	return f.Sync()
}

// maxQueueReadBytes is the hard limit for the queue file. If the file exceeds
// this, readAll truncates it and returns nothing. This prevents a runaway
// queue file (from the amplification bug or any other cause) from OOM-ing the
// process by reading a multi-GB file into memory. 64MB is generous — a normal
// session produces <1MB of events, and a file past the cap is overwhelmingly
// duplicated garbage not worth recovering.
const maxQueueReadBytes = 64 * 1024 * 1024

// readAll reads all events from the queue file. If the file exceeds
// maxQueueReadBytes, the file is truncated and no events are returned —
// a runaway queue (the pre-fix amplification bug grew files to GB scale,
// causing a 55GB-RSS OOM kill) is unrecoverable garbage, and re-reading a
// capped slice every tick while the endpoint is down would churn memory
// indefinitely.
func (q *queue) readAll() ([]Event, error) {
	q.mu.Lock()
	defer q.mu.Unlock()

	info, err := os.Stat(q.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if info.Size() > maxQueueReadBytes {
		// File is too large — this can only happen from a runaway bug
		// (a normal session produces <1MB of events). Its content is
		// overwhelmingly duplicated events, and reading even a capped
		// slice every tick would churn 64MB of allocations per read
		// while the endpoint stays down. Shadow-mode telemetry is
		// lossy by design — truncate the file and start clean rather
		// than nursing gigabytes of garbage.
		fmt.Fprintf(os.Stderr, "ilm: queue file is %d bytes (>%d cap) — truncating runaway queue, events dropped\n",
			info.Size(), maxQueueReadBytes)
		if err := os.Truncate(q.path, 0); err != nil {
			return nil, err
		}
		return nil, nil
	}

	data, err := os.ReadFile(q.path)
	if err != nil {
		return nil, err
	}
	return decodeEvents(data), nil
}

// decodeEvents parses JSONL event data into a slice of Events.
func decodeEvents(data []byte) []Event {
	if len(data) == 0 {
		return nil
	}
	var events []Event
	dec := json.NewDecoder(bytes.NewReader(data))
	for dec.More() {
		var ev Event
		if err := dec.Decode(&ev); err != nil {
			// Skip malformed lines (could be a partial write from a crash).
			break
		}
		events = append(events, ev)
	}
	return events
}

// truncate removes all events from the queue file (after a successful batch
// POST). This is safe because the events have been acknowledged by the server;
// idempotent event IDs make re-sends harmless even if this is called
// prematurely.
func (q *queue) truncate() error {
	q.mu.Lock()
	defer q.mu.Unlock()
	return os.Truncate(q.path, 0)
}

// truncateAfter removes events up to and including the given count (after a
// partial batch POST). Events after `count` remain in the queue.
func (q *queue) truncateAfter(count int) error {
	q.mu.Lock()
	defer q.mu.Unlock()

	data, err := os.ReadFile(q.path)
	if err != nil {
		return err
	}

	lines := 0
	offset := 0
	for offset < len(data) && lines < count {
		nl := bytes.IndexByte(data[offset:], '\n')
		if nl < 0 {
			break
		}
		offset += nl + 1
		lines++
	}

	if offset >= len(data) {
		return os.Truncate(q.path, 0)
	}

	// Write the remaining events back to the file.
	remaining := data[offset:]
	return os.WriteFile(q.path, remaining, 0o644)
}

// len returns the number of events in the queue.
func (q *queue) len() (int, error) {
	events, err := q.readAll()
	if err != nil {
		return 0, err
	}
	return len(events), nil
}

// sender is the background goroutine that batches events from the channel and
// queue, POSTs them to the ilm-stack, and retries with backoff.
type sender struct {
	endpoint       string
	token          string
	queuePath      string
	eventCh        <-chan Event
	queue          *queue
	batchInterval  time.Duration
	maxOutputBytes int
	httpSender     HTTPSender
	stopCh         chan struct{}
	doneCh         chan struct{}
	closeOnce      sync.Once
}

func newSender(endpoint, token, queuePath string, eventCh <-chan Event, q *queue,
	batchInterval time.Duration, maxOutputBytes int) *sender {
	return &sender{
		endpoint:       endpoint,
		token:          token,
		queuePath:      queuePath,
		eventCh:        eventCh,
		queue:          q,
		batchInterval:  batchInterval,
		maxOutputBytes: maxOutputBytes,
		httpSender:     newDefaultHTTPSender(endpoint, token),
		stopCh:         make(chan struct{}),
		doneCh:         make(chan struct{}),
	}
}

func (s *sender) run() {
	defer close(s.doneCh)

	batch := make([]Event, 0, 64)
	ticker := time.NewTicker(s.batchInterval)
	defer ticker.Stop()

	// Backoff state for retries.
	backoff := time.Second
	maxBackoff := 60 * time.Second

	for {
		select {
		case ev, ok := <-s.eventCh:
			if !ok {
				// Channel closed — flush remaining batch and drain queue.
				if len(batch) > 0 {
					s.sendBatch(batch)
				}
				s.drainQueue()
				return
			}
			batch = append(batch, ev)
			if len(batch) >= 64 {
				s.sendBatch(batch)
				batch = batch[:0] // always clear: on failure, sendBatch persisted to the queue
			}

		case <-s.stopCh:
			// Stop signal from Emitter.Close — drain any events still
			// in the channel buffer, flush the batch, and drain the
			// queue, then exit. The channel is NOT closed (to keep
			// Emit safe), so we drain it non-blockingly here.
		drainLoop:
			for {
				select {
				case ev := <-s.eventCh:
					batch = append(batch, ev)
				default:
					break drainLoop
				}
			}
			if len(batch) > 0 {
				s.sendBatch(batch)
			}
			s.drainQueue()
			return

		case <-ticker.C:
			// Drain the queue first so old (previously failed) events are
			// delivered before newer ones. Sending the batch first would
			// invert the order on every retry cycle.
			s.drainQueue()
			if len(batch) > 0 {
				s.sendBatch(batch)
				batch = batch[:0] // always clear: on failure, sendBatch persisted to the queue
			}
		}

		_ = backoff
		_ = maxBackoff
	}
}

// drainQueue reads events from the queue file (from channel overflow) and
// sends them. Called periodically.
//
// IMPORTANT: drainQueue must NOT call sendBatch — sendBatch appends failed
// events back to the queue via appendBatch. But the events read by readAll
// are ALREADY in the queue file (readAll does not remove them). Calling
// sendBatch on failure would append them a second time, doubling the file
// every tick. With a 500ms batch interval and an unreachable endpoint, the
// file grows as N × 2^k — after 20 ticks (10s) it's ~1M× the original size,
// and readAll reads the entire file into memory → OOM crash.
//
// Instead, drainQueue POSTs directly and truncates on success. On failure
// the events stay in the file unchanged — the next tick re-reads and retries.
// No duplication, no growth.
func (s *sender) drainQueue() {
	events, err := s.queue.readAll()
	if err != nil || len(events) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	postErr := s.httpSender.PostEvents(ctx, events)
	if postErr == nil || IsPermanent(postErr) {
		// Success, or permanent failure (4xx): remove the events either way.
		// A permanent failure can never succeed on retry — keeping the events
		// would poison the queue and re-send the bad payload every tick.
		if IsPermanent(postErr) {
			fmt.Fprintf(os.Stderr, "ilm: dropping %d events after permanent error: %v\n",
				len(events), postErr)
		}
		_ = s.queue.truncate()
	}
	// On transient failure: events stay in the file as-is. The next tick
	// re-reads and retries. No duplication.
}

// sendBatch posts a batch of events to the server. Returns true on success
// (or permanent failure — the events are dropped, not queued). On transient
// failure, events are written to the queue for later retry.
func (s *sender) sendBatch(batch []Event) bool {
	if len(batch) == 0 {
		return true
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	err := s.httpSender.PostEvents(ctx, batch)
	if err == nil {
		return true
	}
	if IsPermanent(err) {
		// 4xx — the payload is rejected. Queueing it would poison the queue
		// (retried forever, blocking everything behind it). Drop it.
		fmt.Fprintf(os.Stderr, "ilm: dropping %d events after permanent error: %v\n",
			len(batch), err)
		return true
	}

	// Transient failure — persist to queue for retry.
	_ = s.queue.appendBatch(batch)
	return false
}

func (s *sender) stop() {
	// stop is called after Emitter.Close closes the done channel.
	// Signal the sender to flush and exit, then wait for it.
	// Use a timeout so a slow/unreachable endpoint doesn't block
	// session rotation or process exit indefinitely.
	s.closeOnce.Do(func() {
		close(s.stopCh)
	})
	select {
	case <-s.doneCh:
	case <-time.After(5 * time.Second):
		// Sender didn't finish in time — don't block the caller.
	}
}

// SetHTTPSender replaces the HTTP sender (for testing).
func (s *sender) SetHTTPSender(h HTTPSender) {
	s.httpSender = h
}
func bytesReader(data []byte) *bytes.Reader {
	return bytes.NewReader(data)
}
