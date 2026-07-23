package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestHeadlessWriter_BasicOutput(t *testing.T) {
	var buf bytes.Buffer
	hw := &headlessWriter{w: &buf}

	// Write a line with a newline — should emit one JSONL event.
	n, err := hw.Write([]byte("hello world\n"))
	if err != nil {
		t.Fatalf("Write error: %v", err)
	}
	if n != len("hello world\n") {
		t.Errorf("n = %d, want %d", n, len("hello world\n"))
	}

	// Should have one JSONL line.
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line, got %d", len(lines))
	}

	var event map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if event["type"] != "output" {
		t.Errorf("type = %v, want 'output'", event["type"])
	}
	if event["line"] != "hello world" {
		t.Errorf("line = %v, want 'hello world'", event["line"])
	}
}

func TestHeadlessWriter_StripsANSI(t *testing.T) {
	var buf bytes.Buffer
	hw := &headlessWriter{w: &buf}

	// Write ANSI-colored text.
	hw.Write([]byte("\x1b[31mred text\x1b[0m\n"))

	var event map[string]any
	json.Unmarshal(bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n"))[0], &event)
	if event["line"] != "red text" {
		t.Errorf("line = %v, want 'red text' (ANSI stripped)", event["line"])
	}
}

func TestHeadlessWriter_PartialLineBuffered(t *testing.T) {
	var buf bytes.Buffer
	hw := &headlessWriter{w: &buf}

	// Write without newline — should buffer, no output yet.
	hw.Write([]byte("partial"))
	if buf.Len() != 0 {
		t.Errorf("expected no output for partial line, got %d bytes", buf.Len())
	}

	// Complete the line.
	hw.Write([]byte(" complete\n"))
	if buf.Len() == 0 {
		t.Fatal("expected output after completing line")
	}

	var event map[string]any
	json.Unmarshal(bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n"))[0], &event)
	if event["line"] != "partial complete" {
		t.Errorf("line = %v, want 'partial complete'", event["line"])
	}
}

func TestHeadlessWriter_MultipleLines(t *testing.T) {
	var buf bytes.Buffer
	hw := &headlessWriter{w: &buf}

	hw.Write([]byte("line1\nline2\nline3\n"))

	output := strings.TrimRight(buf.String(), "\n")
	lines := strings.Split(output, "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}

	for i, l := range lines {
		var event map[string]any
		if err := json.Unmarshal([]byte(l), &event); err != nil {
			t.Fatalf("line %d unmarshal: %v", i, err)
		}
		want := "line" + string(rune('1'+i))
		if event["line"] != want {
			t.Errorf("line %d = %v, want %q", i, event["line"], want)
		}
	}
}

func TestHeadlessWriter_EmptyLinesSkipped(t *testing.T) {
	var buf bytes.Buffer
	hw := &headlessWriter{w: &buf}

	// Whitespace-only lines should be skipped.
	hw.Write([]byte("real\n   \nalso real\n"))

	output := strings.TrimRight(buf.String(), "\n")
	lines := strings.Split(output, "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines (empty skipped), got %d", len(lines))
	}
}

func TestHeadlessWriter_Flush(t *testing.T) {
	var buf bytes.Buffer
	hw := &headlessWriter{w: &buf}

	// Write without newline, then flush.
	hw.Write([]byte("unflushed content"))
	if buf.Len() != 0 {
		t.Fatal("expected no output before flush")
	}

	hw.flush()

	if buf.Len() == 0 {
		t.Fatal("expected output after flush")
	}

	var event map[string]any
	json.Unmarshal(bytes.Split(bytes.TrimRight(buf.Bytes(), "\n"), []byte("\n"))[0], &event)
	if event["line"] != "unflushed content" {
		t.Errorf("line = %v, want 'unflushed content'", event["line"])
	}
}

func TestHeadlessWriter_FlushEmpty(t *testing.T) {
	var buf bytes.Buffer
	hw := &headlessWriter{w: &buf}

	// Flush with nothing buffered — should produce no output.
	hw.flush()
	if buf.Len() != 0 {
		t.Errorf("expected no output for empty flush, got %d bytes", buf.Len())
	}
}

func TestEmitEvent(t *testing.T) {
	var buf bytes.Buffer
	emitEvent(&buf, map[string]any{"type": "done", "status": "pass"})

	var event map[string]any
	if err := json.Unmarshal(buf.Bytes(), &event); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if event["type"] != "done" {
		t.Errorf("type = %v, want 'done'", event["type"])
	}
	if event["status"] != "pass" {
		t.Errorf("status = %v, want 'pass'", event["status"])
	}
}

func TestParseRunArgs_AutoCounselDefault(t *testing.T) {
	task, _, flags, err := parseRunArgs([]string{"--auto-counsel", "do task"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task != "do task" {
		t.Errorf("task = %q", task)
	}
	if !flags.AutoCounsel {
		t.Error("AutoCounsel should be true")
	}
	if flags.MaxCounsel != 3 {
		t.Errorf("MaxCounsel = %d, want 3 (default)", flags.MaxCounsel)
	}
}

func TestParseRunArgs_MaxCounsel(t *testing.T) {
	_, _, flags, err := parseRunArgs([]string{"--auto-counsel", "--max-counsel", "5", "task"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if flags.MaxCounsel != 5 {
		t.Errorf("MaxCounsel = %d, want 5", flags.MaxCounsel)
	}
}

func TestParseRunArgs_MaxCounselInvalid(t *testing.T) {
	_, _, _, err := parseRunArgs([]string{"--max-counsel", "abc", "task"})
	if err == nil {
		t.Fatal("expected error for non-integer max-counsel")
	}
}

func TestParseRunArgs_MaxCounselNoValue(t *testing.T) {
	_, _, _, err := parseRunArgs([]string{"--max-counsel"})
	if err == nil {
		t.Fatal("expected error for --max-counsel without value")
	}
}

func TestParseRunArgs_TranscriptNoValue(t *testing.T) {
	_, _, _, err := parseRunArgs([]string{"--transcript"})
	if err == nil {
		t.Fatal("expected error for --transcript without value")
	}
}

func TestParseRunArgs_UnknownFlag(t *testing.T) {
	_, _, _, err := parseRunArgs([]string{"--bogus", "task"})
	if err == nil {
		t.Fatal("expected error for unknown flag")
	}
}

func TestParseRunArgs_DuplicateTask(t *testing.T) {
	_, _, _, err := parseRunArgs([]string{"task1", "task2"})
	if err == nil {
		t.Fatal("expected error for duplicate task argument")
	}
}

func TestParseRunArgs_NoTask(t *testing.T) {
	_, _, _, err := parseRunArgs([]string{"--auto"})
	if err == nil {
		t.Fatal("expected error when no task provided")
	}
}
