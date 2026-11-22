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
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/treeol/wakil/internal/diag"
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
	ch     chan []byte
	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool

	dropped atomic.Int64 // records dropped due to full channel
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
	// A session resumed after a crash may find a file whose last line was cut
	// mid-record (an older writer flushed inside a record). O_APPEND would glue
	// the new store_header onto that partial line, producing a broken line in
	// the MIDDLE of the file and hiding the header. Terminate the torn tail
	// first: the broken line stays (readers skip it), and the new record starts
	// on its own line. Existing bytes are never rewritten.
	if err := terminateCutTail(path); err != nil {
		return nil, err
	}
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
// full or the store is closed, the record is silently dropped so a slow disk
// or a racing Close never adds latency to a turn. sft_eligible is forced
// false. Safe for concurrent use.
func (s *Store) Write(r Record) {
	if s == nil {
		return
	}
	r.SftEligible = false
	r.Ts = time.Now().UTC().Format(time.RFC3339Nano)
	// Marshal outside the lock — it is the expensive part and needs no
	// mutual exclusion.
	b, err := json.Marshal(r)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	// The send must stay non-blocking while the mutex is held: a blocking
	// send here could deadlock against Close.
	select {
	case s.ch <- b:
	default: // channel full → drop and count
		s.dropped.Add(1)
	}
}

// Dropped returns the number of records dropped due to a full channel.
// Useful for monitoring trace data loss.
func (s *Store) Dropped() int64 {
	if s == nil {
		return 0
	}
	return s.dropped.Load()
}

// Close stops accepting new records, flushes records already queued to disk,
// and closes the file. Blocks until the background writer goroutine exits.
// Idempotent and safe for concurrent use; concurrent calls all wait for the
// same flush. Safe to call on a nil Store.
func (s *Store) Close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
	s.mu.Unlock()
	// Wait outside the lock: the drain goroutine never takes the mutex, and
	// concurrent closers must all block until the same flush completes.
	s.wg.Wait()
	// A full write channel drops records rather than blocking a turn. That is
	// the right trade for latency, but it must not be silent: a session that
	// lost turns says so once, so the trace is not trusted as complete when it
	// is not. Reported from Close so no caller outside this package is needed.
	if d := s.dropped.Load(); d > 0 {
		diag.Printf("trace: dropped %d record(s) (write channel was full); the trace is incomplete", d)
	}
}

func (s *Store) drainTo(f *os.File) {
	defer s.wg.Done()
	defer f.Close()
	// Write one record per syscall. A fixed-size bufio.Writer flushes at a byte
	// offset regardless of record boundaries, so a record straddling the flush
	// point used to be written in two parts — a kill between flushes left a
	// half-written final line (the 65536-byte trace files on users' hosts).
	// Writing the record and its newline as ONE buffer guarantees every flushed
	// byte range ends on a line boundary, at the cost of one write per record
	// (trace volume is small: ~67 lines per 64 KiB).
	for b := range s.ch {
		buf := make([]byte, 0, len(b)+1)
		buf = append(buf, b...)
		buf = append(buf, '\n')
		if _, err := f.Write(buf); err != nil {
			diag.Printf("trace: write error: %v", err)
			continue
		}
	}
}

// ReadFile parses every line of a trace file, skipping any line that does not
// parse (a torn tail, or a broken line in the middle after a resumed session)
// and returning how many were skipped. Later valid records are still returned:
// only the unparsable lines are dropped.
func ReadFile(path string) ([]Record, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	var recs []Record
	skipped := 0
	// bufio.Reader.ReadBytes has no token-size limit, unlike bufio.Scanner
	// (default 64 KiB), so a single record larger than the write buffer is
	// still read whole.
	br := bufio.NewReaderSize(f, 64*1024)
	for {
		line, readErr := br.ReadBytes('\n')
		if t := bytes.TrimSpace(line); len(t) > 0 {
			var r Record
			if err := json.Unmarshal(t, &r); err != nil {
				skipped++
			} else {
				recs = append(recs, r)
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				break
			}
			return recs, skipped, readErr
		}
	}
	return recs, skipped, nil
}

// terminateCutTail appends a newline to path when the file does not already
// end in one, so a record appended afterwards cannot be glued onto a partial
// line. It never rewrites or truncates existing bytes; a missing file is not an
// error (nothing to repair).
func terminateCutTail(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() == 0 {
		return nil
	}
	last := make([]byte, 1)
	if _, err := f.ReadAt(last, st.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	if _, err := f.WriteAt([]byte{'\n'}, st.Size()); err != nil {
		return err
	}
	return nil
}
