package main

import (
	"testing"
)

// TestParseRunArgs_VerifyFlag tests that --verify is parsed correctly.
func TestParseRunArgs_VerifyFlag(t *testing.T) {
	task, planMode, flags, err := parseRunArgs([]string{"--plan", "--verify", "--auto", "do the task"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task != "do the task" {
		t.Errorf("expected task 'do the task', got %q", task)
	}
	if !planMode {
		t.Error("expected planMode=true")
	}
	if !flags.Verify {
		t.Error("expected Verify=true")
	}
	if !flags.Auto {
		t.Error("expected Auto=true")
	}
}

// TestParseRunArgs_VerifyAlone tests --verify without --plan.
func TestParseRunArgs_VerifyAlone(t *testing.T) {
	task, planMode, flags, err := parseRunArgs([]string{"--verify", "task"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task != "task" {
		t.Errorf("expected task 'task', got %q", task)
	}
	if planMode {
		t.Error("expected planMode=false (no --plan)")
	}
	if !flags.Verify {
		t.Error("expected Verify=true")
	}
}

// TestParseRunArgs_NoVerify tests the default (no --verify).
func TestParseRunArgs_NoVerify(t *testing.T) {
	_, _, flags, err := parseRunArgs([]string{"--plan", "--auto", "task"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if flags.Verify {
		t.Error("expected Verify=false by default")
	}
}

// TestParseRunArgs_VerifyUsage tests that --verify is included in the usage string.
func TestParseRunArgs_VerifyUsage(t *testing.T) {
	_, _, _, err := parseRunArgs(nil)
	if err == nil {
		t.Fatal("expected error for no task")
	}
	if !containsStr(err.Error(), "--verify") {
		t.Errorf("expected usage to mention --verify, got: %s", err.Error())
	}
}

func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || (len(s) > 0 && containsStrHelper(s, sub)))
}

func containsStrHelper(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
