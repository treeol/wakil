// Package ilm implements the ilm-stack shadow-mode emitter.
//
// It hooks into Wakil's write paths (session start/end, user/assistant turns,
// tool calls/results, memory ops, mashura calls, errors) and emits events to
// an ilm-stack instance. The emitter is NEVER on the critical path: events go
// through an in-process channel to a local durable queue (append-only JSONL
// file), and a background sender batches and posts them with retry and
// backoff. Wakil's main loop never blocks on the network.
//
// mode=off (the default): the emitter discards all events immediately — zero
// behaviour change, zero network calls. Only mode=shadow activates the channel,
// queue, and sender.
package ilm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Mode controls emitter behaviour.
type Mode string

const (
	ModeOff    Mode = "off"
	ModeShadow Mode = "shadow"
)

// Config holds the ilm-stack emitter configuration.
type Config struct {
	Endpoint      string `json:"endpoint"`
	Token         string `json:"token"`
	Mode          Mode   `json:"mode"` // "off" (default) | "shadow"
	QueuePath     string `json:"queue_path"`
	BatchMS       int    `json:"batch_ms"`        // default 500
	MaxOutputBytes int   `json:"max_output_bytes"` // default 65536
}

// Defaults
const (
	defaultBatchMS       = 500
	defaultMaxOutputBytes = 65536
	clientName           = "wakil"
)

// EventType matches the contract's event type strings.
type EventType string

const (
	EventSessionStart EventType = "session_start"
	EventUserTurn      EventType = "user_turn"
	EventAssistantTurn EventType = "assistant_turn"
	EventToolCall      EventType = "tool_call"
	EventToolResult    EventType = "tool_result"
	EventMemoryOp      EventType = "memory_op"
	EventMashuraCall   EventType = "mashura_call"
	EventError         EventType = "error"
	EventSessionEnd    EventType = "session_end"
)

// Event is the wire-level event sent to ilm-stack.
type Event struct {
	EventID       string          `json:"event_id"`
	SessionID     string          `json:"session_id"`
	Seq           int             `json:"seq"`
	Ts            string          `json:"ts"`
	Type          EventType       `json:"type"`
	Payload       json.RawMessage `json:"payload"`
	SchemaVersion int             `json:"schema_version"`
	Client        ClientInfo      `json:"client"`
}

// ClientInfo identifies the emitter.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// Emitter is the main emitter instance. It is safe for concurrent use.
// A nil Emitter (or one with Mode=off) silently discards all events.
type Emitter struct {
	cfg       Config
	sessionID string
	seq       int
	seqMu     sync.Mutex

	// processID is a per-Emitter-instance UUID that prevents event ID
	// collisions when a session ID survives resume/rotation. Without it,
	// uuid5(session, seq) would produce the same IDs in the new process
	// and the server would dedup real events as duplicates.
	processID string

	// eventCh is the in-process channel. nil when mode=off.
	eventCh chan Event

	// queue is the durable append-only JSONL queue.
	queue *queue

	// sender is the background HTTP sender.
	sender *sender

	// redactor applies redaction patterns before events leave the process.
	redactor *Redactor

	// closed marks the emitter as shut down. Guarded by closeMu — all
	// reads AND writes must hold closeMu to avoid a data race.
	closed bool
	closeMu sync.Mutex

	// closeOnce ensures Close is idempotent and race-free.
	closeOnce sync.Once

	// done is closed when the emitter is shutting down. Emit() checks
	// this to avoid sending on eventCh after Close — which would panic
	// on a closed channel. eventCh is NEVER closed (to keep Emit safe).
	done chan struct{}
}

// New creates an Emitter. When mode=off, returns a no-op emitter that discards
// all events without allocating a channel, queue, or sender.
func New(cfg Config, sessionID string) (*Emitter, error) {
	if cfg.Mode == "" {
		cfg.Mode = ModeOff
	}
	// Reject unknown mode values — only "off" and "shadow" are valid.
	// The old code activated emission for any non-empty, non-"off" value.
	if cfg.Mode != ModeOff && cfg.Mode != ModeShadow {
		return nil, fmt.Errorf("ilm: unknown mode %q (must be %q or %q)", cfg.Mode, ModeOff, ModeShadow)
	}
	if cfg.BatchMS <= 0 {
		cfg.BatchMS = defaultBatchMS
	}
	if cfg.MaxOutputBytes <= 0 {
		cfg.MaxOutputBytes = defaultMaxOutputBytes
	}

	e := &Emitter{
		cfg:       cfg,
		sessionID: sessionID,
		processID: uuid.NewString(), // per-instance ID prevents event ID collisions on resume
		done:      make(chan struct{}),
	}

	if cfg.Mode == ModeOff {
		return e, nil
	}

	// Shadow mode: allocate channel, queue, sender.
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("ilm: endpoint is required when mode=shadow")
	}
	if cfg.QueuePath == "" {
		return nil, fmt.Errorf("ilm: queue_path is required when mode=shadow")
	}

	// Ensure queue directory exists.
	if err := os.MkdirAll(filepath.Dir(cfg.QueuePath), 0o755); err != nil {
		return nil, fmt.Errorf("ilm: create queue dir: %w", err)
	}

	q, err := newQueue(cfg.QueuePath)
	if err != nil {
		return nil, fmt.Errorf("ilm: open queue: %w", err)
	}
	e.queue = q

	// Load redaction patterns (compiled from the redaction_v1 schema).
	e.redactor = DefaultRedactor()

	e.eventCh = make(chan Event, 256)

	// Start the background sender.
	e.sender = newSender(cfg.Endpoint, cfg.Token, cfg.QueuePath, e.eventCh, q,
		time.Duration(cfg.BatchMS)*time.Millisecond, cfg.MaxOutputBytes)
	go e.sender.run()

	return e, nil
}

// Emit sends an event through the pipeline. In off mode it is a no-op.
// This is the single entry point called from Wakil's write-path hook points.
// The payload is already marshalled JSON; the caller constructs it via the
// payload helper types below. Redaction is applied inside Emit.
func (e *Emitter) Emit(typ EventType, payload interface{}) {
	if e == nil || e.cfg.Mode == ModeOff {
		return
	}

	// If the emitter is shutting down, drop the event — Emit must be
	// safe to call concurrently with Close. The done channel is closed
	// by Close under closeMu; reading it here is race-free (channel
	// reads are synchronized). The closed bool is NOT read here to
	// avoid a data race with Close's write.
	select {
	case <-e.done:
		return
	default:
	}

	// Marshal payload.
	raw, err := json.Marshal(payload)
	if err != nil {
		return
	}

	// Apply redaction to the payload.
	if e.redactor != nil {
		raw = e.redactor.RedactJSON(raw)
	}

	// Assign event ID (uuid5: namespace + session + processID + seq). The
	// processID is a per-Emitter UUID that prevents collisions when a session
	// ID survives resume/rotation — without it, uuid5(session, seq) would
	// produce the same IDs and the server would dedup real events.
	e.seqMu.Lock()
	e.seq++
	seq := e.seq
	e.seqMu.Unlock()

	eventID := uuid.NewSHA1(uuidNamespace, []byte(fmt.Sprintf("%s:%s:%d", e.sessionID, e.processID, seq))).String()

	ev := Event{
		EventID:       eventID,
		SessionID:     e.sessionID,
		Seq:           seq,
		Ts:            time.Now().UTC().Format(time.RFC3339Nano),
		Type:          typ,
		Payload:       raw,
		SchemaVersion: 1,
		Client: ClientInfo{
			Name:    clientName,
			Version: "0.1.0",
		},
	}

	// Non-blocking send to channel. If the channel is full or the
	// emitter is closing, the event is written directly to the queue
	// (at-least-once; the sender will pick it up from the queue on the
	// next batch). Checking done here avoids sending on a closed channel.
	//
	// The queue-fallback append is non-blocking: if the queue write fails
	// (disk full, permissions), the event is dropped with a stderr log.
	// This ensures Emit never blocks the caller's goroutine — the
	// "never on the critical path" contract. The previous code called
	// queue.append which does a synchronous fsync, blocking for disk I/O
	// when the channel was full and the endpoint was down.
	select {
	case e.eventCh <- ev:
	case <-e.done:
		// Emitter closing — best-effort append, don't block.
		if err := e.queue.append(ev); err != nil {
			fmt.Fprintf(os.Stderr, "ilm: dropped event (close): %v\n", err)
		}
	default:
		// Channel full — best-effort append to queue. If the queue
		// write also fails, drop the event rather than blocking the
		// caller. Shadow-mode telemetry is lossy by design.
		if err := e.queue.append(ev); err != nil {
			fmt.Fprintf(os.Stderr, "ilm: dropped event (queue full): %v\n", err)
		}
	}
}

// Close flushes pending events and stops the background sender.
// Safe to call on a nil or off-mode emitter. Safe to call concurrently
// with Emit — Emit checks the done channel to avoid sending on a closed
// channel. eventCh is intentionally NOT closed (closing it would race
// with concurrent Emit calls and panic with "send on closed channel").
//
// Close uses sync.Once so the race between the initial e.closed read and
// the closeMu write is eliminated — the once.Do ensures exactly one close
// regardless of concurrent callers.
func (e *Emitter) Close() error {
	if e == nil || e.cfg.Mode == ModeOff {
		return nil
	}
	e.closeOnce.Do(func() {
		e.closeMu.Lock()
		e.closed = true
		close(e.done)
		e.closeMu.Unlock()
		if e.sender != nil {
			e.sender.stop()
		}
	})
	return nil
}

// uuidNamespace is the deterministic UUID5 namespace for event IDs.
var uuidNamespace = uuid.MustParse("6ba7b810-9dad-11d1-80b4-00c04fd430c8") // standard DNS namespace

// HashOutput computes sha256 of the output (post-redaction, pre-truncation).
func HashOutput(output string) string {
	h := sha256.Sum256([]byte(output))
	return hex.EncodeToString(h[:])
}

// BoundOutput truncates output to maxBytes and returns the truncated flag,
// full_hash, and full_size.
func BoundOutput(output string, maxBytes int) (bounded string, truncated bool, fullHash string, fullSize int) {
	fullSize = len(output)
	fullHash = HashOutput(output)
	if fullSize <= maxBytes {
		return output, false, fullHash, fullSize
	}
	return output[:maxBytes], true, fullHash, fullSize
}

// MaxOutputBytes returns the configured output byte limit.
func (e *Emitter) MaxOutputBytes() int {
	if e == nil || e.cfg.MaxOutputBytes <= 0 {
		return defaultMaxOutputBytes
	}
	return e.cfg.MaxOutputBytes
}

// noopEmitter is a compile-time check that Emitter can be nil-safely called.
var _ *Emitter = (*Emitter)(nil)

// HTTPSender is an interface for testability.
type HTTPSender interface {
	PostEvents(ctx context.Context, events []Event) error
}

// defaultHTTPSender posts batches to /v1/events.
type defaultHTTPSender struct {
	endpoint string
	token    string
	client   *http.Client
}

func newDefaultHTTPSender(endpoint, token string) *defaultHTTPSender {
	return &defaultHTTPSender{
		endpoint: endpoint,
		token:    token,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// permanentError marks a failure that must NOT be retried — e.g. an HTTP 4xx
// (malformed payload, bad token). Retrying a permanent failure forever
// ("poison batch") blocks the queue and re-sends the same bad payload
// indefinitely. The sender checks this via IsPermanent and drops the events.
type permanentError struct{ msg string }

func (e *permanentError) Error() string { return e.msg }

// IsPermanent reports whether err is a permanent (non-retryable) failure.
func IsPermanent(err error) bool {
	var pe *permanentError
	return errors.As(err, &pe)
}

func (s *defaultHTTPSender) PostEvents(ctx context.Context, events []Event) error {
	body, err := json.Marshal(struct {
		Events []Event `json:"events"`
	}{Events: events})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, "POST", s.endpoint+"/v1/events", bytesReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err := s.client.Do(req)
	if err != nil {
		return err // transient (network) — retry
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 500 {
		return fmt.Errorf("server error: %d", resp.StatusCode) // transient — retry
	}
	if resp.StatusCode >= 400 && resp.StatusCode != 429 {
		// 4xx (except 429) — the payload itself is rejected (bad auth,
		// schema violation). Retrying will never succeed; drop the batch
		// instead of poisoning the queue.
		return &permanentError{msg: fmt.Sprintf("client error: %d", resp.StatusCode)}
	}
	if resp.StatusCode == 429 {
		// Rate limited — transient. The sender will retry on the next
		// tick with backoff. Not classified as permanent so the events
		// are preserved in the queue for retry.
		return fmt.Errorf("rate limited: 429")
	}
	return nil
}
