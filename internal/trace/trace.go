// Package trace implements the P38 rich JSONL session trace store.
//
// Each session opened with Open() gets a dedicated per-session .jsonl file.
// Every line is one JSON Record. The first line is always a "store_header" so
// readers can assert the file provenance without special-casing inner records.
// All records carry sft_eligible:false — capture ≠ consent-to-train.
//
// Writes are async: Write() sends to a buffered channel; a background goroutine
// drains it to disk. A 166 KB pre-cap tool result is recorded as a byte count,
// not the full content, so individual records stay small.
package trace

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Record is one JSONL line in a trace file.
// sft_eligible is always false and is never omitted (omitempty would make
// absence ambiguous — a reader would not know if the field was missing or
// explicitly false).
type Record struct {
	Type        string `json:"type"` // "store_header" | "turn"
	SessionID   string `json:"session_id"`
	Ts          string `json:"ts"`           // RFC3339Nano UTC
	SftEligible bool   `json:"sft_eligible"` // always false, never omitted

	// store_header fields
	Model     string `json:"model,omitempty"`
	Workspace string `json:"workspace,omitempty"`

	// turn fields
	TurnIndex       int         `json:"turn_index,omitempty"`
	TurnType        string      `json:"turn_type,omitempty"`       // "tool_loop" | "final"
	ReasoningChars  int         `json:"reasoning_chars,omitempty"` // streamed reasoning_content chars
	ToolCalls       []ToolTrace `json:"tool_calls,omitempty"`
	Backend         string      `json:"backend,omitempty"` // X-Ilm-Backend-Used
	InputTokens     int64       `json:"input_tokens,omitempty"`
	OutputTokens    int64       `json:"output_tokens,omitempty"`
	ReasoningTokens int64       `json:"reasoning_tokens,omitempty"` // from usage chunk
	Outcome         string      `json:"outcome,omitempty"`          // "complete" | "empty" | "stream_error"
	Grounding       []string    `json:"grounding,omitempty"`        // "type:label" pairs
}

// ToolTrace is the per-tool-call record within a turn.
// PreCapBytes is the raw result length from execution; PostCapBytes is what
// enters the conversation after CapOrStub. Capped=true when they differ.
type ToolTrace struct {
	Name         string `json:"name"`
	PreCapBytes  int    `json:"pre_cap_bytes"`
	PostCapBytes int    `json:"post_cap_bytes"`
	Capped       bool   `json:"capped"`
}

// Store is an async JSONL trace writer for one session. Write calls send
// records to a buffered channel; a background goroutine drains the channel
// to disk. Close flushes and waits for the goroutine to exit.
type Store struct {
	ch chan []byte
	wg sync.WaitGroup
}

// Open creates (or appends to) the per-session JSONL file under dir and
// starts the background writer. The first record written is a store_header
// with sft_eligible:false so any reader can assert the file provenance.
// Returns a nil *Store on error so callers can nil-check without branching.
func Open(dir, sessionID, model, workspace string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, sessionID+".jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	s := &Store{ch: make(chan []byte, 256)}
	s.wg.Add(1)
	go s.drainTo(f)

	hdr := Record{
		Type:        "store_header",
		SessionID:   sessionID,
		Ts:          time.Now().UTC().Format(time.RFC3339Nano),
		SftEligible: false,
		Model:       model,
		Workspace:   workspace,
	}
	b, _ := json.Marshal(hdr)
	s.ch <- b // goroutine is already draining; this will not block
	return s, nil
}

// Write enqueues r for async disk write. Non-blocking: if the channel is
// full (store closed or disk stalled) the record is silently dropped so
// a slow disk never adds latency to a turn. sft_eligible is forced false.
func (s *Store) Write(r Record) {
	if s == nil {
		return
	}
	r.SftEligible = false
	r.Ts = time.Now().UTC().Format(time.RFC3339Nano)
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	select {
	case s.ch <- b:
	default: // channel full → drop silently
	}
}

// Close flushes all queued records to disk and closes the file. Blocks until
// the background writer goroutine exits. Safe to call on a nil Store.
func (s *Store) Close() {
	if s == nil {
		return
	}
	close(s.ch)
	s.wg.Wait()
}

func (s *Store) drainTo(f *os.File) {
	defer s.wg.Done()
	defer f.Close()
	bw := bufio.NewWriterSize(f, 64*1024)
	for b := range s.ch {
		bw.Write(b)
		bw.WriteByte('\n')
	}
	bw.Flush()
}
