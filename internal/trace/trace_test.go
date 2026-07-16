package trace

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTrace_OpenWriteCloseRoundTrip verifies the core lifecycle:
// Open creates a file with a store_header, Write enqueues turn records,
// Close flushes everything to disk. Re-reading the file should yield
// the header plus all written records in order.
func TestTrace_OpenWriteCloseRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "test-session", "test-model", "/work")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Write a few turn records.
	for i := 0; i < 3; i++ {
		s.Write(Record{
			Type:      "turn",
			SessionID: "test-session",
			TurnIndex: i,
			TurnType:  "tool_loop",
			ToolCalls: []ToolTrace{
				{Name: "run_shell", PreCapBytes: 100, PostCapBytes: 100, Capped: false},
			},
			Outcome: "complete",
		})
	}
	s.Close()

	// Read back and verify.
	path := filepath.Join(dir, "test-session.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 4 { // 1 header + 3 turns
		t.Fatalf("expected 4 lines, got %d", len(lines))
	}

	// First line must be a store_header.
	var hdr Record
	if err := json.Unmarshal([]byte(lines[0]), &hdr); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if hdr.Type != "store_header" {
		t.Errorf("first line type = %q, want %q", hdr.Type, "store_header")
	}
	if hdr.Model != "test-model" {
		t.Errorf("header model = %q, want %q", hdr.Model, "test-model")
	}
	if hdr.SftEligible {
		t.Error("sft_eligible must always be false")
	}

	// Subsequent lines are turns.
	for i, line := range lines[1:] {
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("unmarshal turn %d: %v", i, err)
		}
		if rec.Type != "turn" {
			t.Errorf("line %d type = %q, want %q", i+1, rec.Type, "turn")
		}
		if rec.TurnIndex != i {
			t.Errorf("turn %d TurnIndex = %d, want %d", i, rec.TurnIndex, i)
		}
		if rec.SftEligible {
			t.Error("sft_eligible must always be false")
		}
		if len(rec.ToolCalls) != 1 || rec.ToolCalls[0].Name != "run_shell" {
			t.Errorf("turn %d tool_calls = %v, want one run_shell", i, rec.ToolCalls)
		}
	}
}

// TestTrace_NilStoreSafe verifies that Write and Close on a nil Store are safe.
func TestTrace_NilStoreSafe(t *testing.T) {
	var s *Store
	// Must not panic.
	s.Write(Record{Type: "turn"})
	s.Close()
}

// TestTrace_FloodWritesNoDeadlock verifies that writing many records rapidly
// does not block (the non-blocking select with default prevents deadlock).
// Note: this test does NOT prove records were dropped — the drain goroutine
// may keep up. The dropped-record counter mentioned in the plan does not exist
// in the current implementation (drops are silent). This test confirms the
// non-blocking write path and that Close succeeds after a flood.
func TestTrace_FloodWritesNoDeadlock(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "flood-session", "m", "/work")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	// Write many records rapidly — the non-blocking select ensures Write
	// never blocks even if the 256-buffer fills faster than the drain goroutine
	// can flush. This test verifies no deadlock and that Close succeeds.
	for i := 0; i < 10000; i++ {
		s.Write(Record{Type: "turn", TurnIndex: i})
	}
	s.Close()

	// File should exist and have at least the header.
	path := filepath.Join(dir, "flood-session.jsonl")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 1 {
		t.Errorf("expected at least header, got %d lines", len(lines))
	}
	// First line must be the header.
	var hdr Record
	if err := json.Unmarshal([]byte(lines[0]), &hdr); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if hdr.Type != "store_header" {
		t.Errorf("first line type = %q, want %q", hdr.Type, "store_header")
	}
}

// TestTrace_WriteForcesSftEligibleFalse verifies that Write always sets
// sft_eligible to false regardless of what the caller passes.
func TestTrace_WriteForcesSftEligibleFalse(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "sft-test", "m", "/work")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s.Write(Record{Type: "turn", SftEligible: true}) // try to set true
	s.Close()

	path := filepath.Join(dir, "sft-test.jsonl")
	raw, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	for _, line := range lines {
		var rec Record
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec.SftEligible {
			t.Error("sft_eligible must be forced to false by Write")
		}
	}
}
