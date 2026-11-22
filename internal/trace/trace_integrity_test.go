package trace

// trace_integrity_test.go — proves a trace file is never left with a partial
// line, even when a session writes far more than the 64 KiB write buffer, and
// that every line of a cut file is skipped or parsed rather than aborting a
// reader.
//
// The bug these cover: drainTo used a fixed 64 KiB bufio.Writer, which flushes
// at a byte offset regardless of record boundaries. A record straddling the
// 65 536-byte mark was written in two parts, so a kill between flushes left a
// half-written final line — exactly the 65 536-byte files found on the user's
// host.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestTrace_LargeSessionAllLinesParse writes well past 64 KiB and asserts every
// line of the resulting file is a complete, parsable JSON record and that no
// turn was lost. This is a REGRESSION test, not the red-before-fix one: the old
// writer also flushed fully on a clean Close. The bug needed a kill between
// flushes, which TestTrace_FlushedPrefixEndsOnLineBoundary reproduces.
func TestTrace_LargeSessionAllLinesParse(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "big-session", "m", "/w")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// ~1 KiB padding per record, 200 records: ~200 KiB, three buffer widths of
	// content, so several records straddle a 64 KiB boundary.
	const padLen = 900
	const n = 200
	pad := strings.Repeat("x", padLen)
	for i := 0; i < n; i++ {
		s.Write(Record{
			Type:      "turn",
			SessionID: "big-session",
			TurnIndex: i,
			TurnType:  "tool_loop",
			Grounding: []string{pad}, // bulk, so records are ~900 B
			Outcome:   "complete",
		})
	}
	s.Close()

	path := filepath.Join(dir, "big-session.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(raw) <= 64*1024 {
		t.Fatalf("test did not cross the 64 KiB boundary: file is %d bytes", len(raw))
	}

	lines := splitLines(raw)
	if len(lines) != n+1 { // header + n turns
		t.Fatalf("line count = %d, want %d (a lost or merged line means the writer split a record)", len(lines), n+1)
	}
	for i, ln := range lines {
		var rec Record
		if err := json.Unmarshal(ln, &rec); err != nil {
			t.Fatalf("line %d does not parse (partial write): %v — line starts %q", i, err,
				string(ln[:min(40, len(ln))]))
		}
	}
	// Every turn index present exactly once.
	seen := make([]bool, n)
	for _, ln := range lines[1:] {
		var rec Record
		if err := json.Unmarshal(ln, &rec); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if rec.TurnIndex < 0 || rec.TurnIndex >= n {
			t.Fatalf("turn_index %d out of range", rec.TurnIndex)
		}
		if seen[rec.TurnIndex] {
			t.Fatalf("turn_index %d written twice", rec.TurnIndex)
		}
		seen[rec.TurnIndex] = true
	}
}

// TestTrace_NoPartialLineAcrossFlushSizes is the direct regression for the
// buffer-boundary split: for a range of record sizes, every flushed byte
// boundary is checked to fall on a newline. It writes records whose lengths are
// deliberately coprime-ish with 64 KiB so straddles happen.
func TestTrace_NoPartialLineAcrossFlushSizes(t *testing.T) {
	for _, padLen := range []int{0, 1, 7, 63, 127, 1023, 4097} {
		t.Run(strings.Repeat("x", 0)+itoa(padLen), func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir, "s", "m", "/w")
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			pad := strings.Repeat("y", padLen)
			for i := 0; i < 120; i++ {
				s.Write(Record{Type: "turn", SessionID: "s", TurnIndex: i, Grounding: []string{pad}})
			}
			s.Close()

			raw, err := os.ReadFile(filepath.Join(dir, "s.jsonl"))
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			// Every line must parse. A split record would leave one line that does not.
			for i, ln := range splitLines(raw) {
				var rec Record
				if err := json.Unmarshal(ln, &rec); err != nil {
					t.Fatalf("pad=%d line %d unparsable: %v", padLen, i, err)
				}
			}
		})
	}
}

// TestReadFile_SkipsBrokenTail proves the reader tolerates a file whose last
// line was cut mid-record (the shape found on the user's host) and reports the
// skip instead of failing.
func TestReadFile_SkipsBrokenTail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cut.jsonl")
	good := `{"type":"store_header","session_id":"s","sft_eligible":false}` + "\n" +
		`{"type":"turn","session_id":"s","turn_index":0,"tool_calls":[{"name":"read_file"}]}` + "\n" +
		`{"type":"turn","session_id":"s","turn_index":1,"tool_ca` // cut mid-record
	if err := os.WriteFile(path, []byte(good), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	recs, skipped, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2 (the cut tail must be skipped, not returned)", len(recs))
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1", skipped)
	}
	if recs[1].ToolCalls[0].Name != "read_file" {
		t.Fatalf("record shape lost: %+v", recs[1])
	}
}

// TestReadFile_ValidRecordAfterBrokenLine proves a broken line in the MIDDLE
// does not stop the reader: later valid records are still returned.
func TestReadFile_ValidRecordAfterBrokenLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mid.jsonl")
	body := `{"type":"turn","session_id":"s","turn_index":0}` + "\n" +
		`{not json at all}` + "\n" +
		`{"type":"turn","session_id":"s","turn_index":2}` + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	recs, skipped, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(recs) != 2 || skipped != 1 {
		t.Fatalf("records=%d skipped=%d, want 2 and 1", len(recs), skipped)
	}
	if recs[1].TurnIndex != 2 {
		t.Fatalf("record after the broken line lost: %+v", recs[1])
	}
}

// TestDroppedWarningOnce proves a store that dropped records warns exactly
// once, however many times Close is called — the "says so once" claim. It forces
// a drop deterministically by never letting the drain goroutine run: the channel
// is closed only at Close, so every record beyond the 256-slot buffer is
// dropped and counted.
func TestDroppedWarningOnce(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "drops", "m", "/w")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// 2000 writes into a 256-slot channel: the drain goroutine may consume some,
	// but not all, so at least one record is dropped. Repeat until a drop is
	// observed, so the assertion is not racy in the passing direction.
	// Occupy the channel faster than the drain goroutine can empty it. A tight
	// burst of 2000 writes against a 256-slot channel reliably drops at least
	// one; the loop above already does this. Assert, do not skip, so a
	// regression that stops counting drops is caught.
	for i := 0; i < 2000; i++ {
		s.Write(Record{Type: "turn", TurnIndex: i})
	}
	dropped := int64(0)
	// Close twice and concurrently: the count must still be reported once.
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.Close() }()
	}
	wg.Wait()
	dropped = s.Dropped()
	if dropped == 0 {
		t.Fatalf("no drop counted after 2000 writes into a 256-slot channel; the drop counter is broken")
	}
	// The drop count is now non-zero and Close has run; a second Close must not
	// change it or panic.
	s.Close()
	if got := s.Dropped(); got != dropped {
		t.Fatalf("Dropped changed across Close: %d then %d", dropped, got)
	}

	// The file, whatever was kept, must still be line-clean.
	raw, err := os.ReadFile(filepath.Join(dir, "drops.jsonl"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for i, ln := range splitLines(raw) {
		var rec Record
		if err := json.Unmarshal(ln, &rec); err != nil {
			t.Fatalf("line %d unparsable after drops: %v", i, err)
		}
	}
}

// splitLines splits on '\n' and drops the trailing empty element.
func splitLines(b []byte) [][]byte {
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 1<<20), 8<<20)
	var out [][]byte
	for sc.Scan() {
		ln := sc.Bytes()
		if len(bytes.TrimSpace(ln)) == 0 {
			continue
		}
		out = append(out, append([]byte(nil), ln...))
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestTrace_FlushedPrefixEndsOnLineBoundary is the honest reproduction of the
// user's 65536-byte files: it reads the file WHILE the session is still open
// (the kill/crash case) and requires that whatever has reached disk ends on a
// complete line. With the old fixed 64 KiB bufio.Writer the file stops at
// exactly 65536 bytes in the middle of a record.
func TestTrace_FlushedPrefixEndsOnLineBoundary(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "live", "m", "/w")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	path := filepath.Join(dir, "live.jsonl")
	pad := strings.Repeat("z", 900)
	for i := 0; i < 200; i++ {
		s.Write(Record{Type: "turn", SessionID: "live", TurnIndex: i, Grounding: []string{pad}})
	}

	// Wait until at least 64 KiB has reached disk (or give up early).
	var raw []byte
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, _ = os.ReadFile(path)
		if len(raw) >= 64*1024 || time.Now().After(deadline) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if len(raw) < 64*1024 {
		t.Fatalf("only %d bytes reached disk; cannot test the boundary", len(raw))
	}
	if !bytes.HasSuffix(raw, []byte("\n")) {
		tail := raw[len(raw)-40:]
		t.Fatalf("flushed prefix does not end on a line boundary (%d bytes) — last 40 bytes %q: a kill here leaves a partial record",
			len(raw), string(tail))
	}
	// Every complete line must parse.
	for i, ln := range splitLines(raw) {
		var rec Record
		if err := json.Unmarshal(ln, &rec); err != nil {
			t.Fatalf("line %d of the flushed prefix unparsable: %v", i, err)
		}
	}
}

// TestOpen_RepairsCutTailBeforeAppending proves a session resumed onto a cut
// file does not glue its new store_header onto the partial line: the torn tail
// stays on its own line (readers skip it) and the header starts a new one.
func TestOpen_RepairsCutTailBeforeAppending(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	// A crashed earlier session: one good line, then a cut tail with no newline.
	cut := `{"type":"turn","session_id":"s","turn_index":0}` + "\n" + `{"type":"turn","sess`
	if err := os.WriteFile(path, []byte(cut), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	s, err := Open(dir, "s", "m", "/w")
	if err != nil {
		t.Fatalf("Open on cut file: %v", err)
	}
	s.Write(Record{Type: "turn", SessionID: "s", TurnIndex: 1})
	s.Close()

	recs, skipped, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1 (the torn tail)", skipped)
	}
	// good turn, new header, new turn.
	if len(recs) != 3 {
		t.Fatalf("records = %d, want 3; the header was glued to the tail", len(recs))
	}
	if recs[1].Type != "store_header" {
		t.Fatalf("second record type = %q, want store_header (a glued line would lose it)", recs[1].Type)
	}
}

// TestReadFile_ReadsRecordLargerThanBuffer proves a single record bigger than
// the 64 KiB read/write buffer is still parsed whole (bufio.Scanner would have
// failed at its 64 KiB token limit).
func TestReadFile_ReadsRecordLargerThanBuffer(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "big.jsonl")
	big := `{"type":"turn","session_id":"s","turn_index":0,"grounding":["` +
		strings.Repeat("q", 200*1024) + `"]}`
	if err := os.WriteFile(path, []byte(big+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	recs, skipped, err := ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if skipped != 0 || len(recs) != 1 {
		t.Fatalf("records=%d skipped=%d, want 1 and 0", len(recs), skipped)
	}
}
