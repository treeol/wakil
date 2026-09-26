package main

import (
	"testing"
	"time"
)

func TestParseRunArgsContinuousRequiresBounds(t *testing.T) {
	cases := [][]string{
		{"--continue", "task"},
		{"--continue", "--max-turns", "3", "task"},
		{"--continue", "--max-time", "30m", "task"},
	}
	for _, args := range cases {
		if _, _, _, err := parseRunArgs(args); err == nil {
			t.Errorf("args %v: expected error", args)
		}
	}
}

func TestParseRunArgsContinuousAccepted(t *testing.T) {
	task, plan, flags, err := parseRunArgs([]string{
		"--continue", "--max-turns", "5", "--max-time", "30m",
		"--verify-cmd", "go test ./...", "--verify-cmd", "go vet ./...", "do it",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task != "do it" || plan {
		t.Fatalf("task=%q plan=%v", task, plan)
	}
	if flags.Continuous == nil {
		t.Fatal("continuous options not set")
	}
	if flags.Continuous.MaxTurns != 5 || flags.Continuous.MaxTime != 30*time.Minute {
		t.Fatalf("bounds = %+v", flags.Continuous)
	}
	if len(flags.Continuous.VerifyCommands) != 2 {
		t.Fatalf("verify commands = %v", flags.Continuous.VerifyCommands)
	}
}

func TestParseRunArgsContinuousFlagsRequireContinue(t *testing.T) {
	for _, args := range [][]string{
		{"--max-turns", "3", "task"},
		{"--max-time", "30m", "task"},
		{"--verify-cmd", "true", "task"},
	} {
		if _, _, _, err := parseRunArgs(args); err == nil {
			t.Errorf("args %v: expected error without --continue", args)
		}
	}
}

func TestParseRunArgsRejectsInvalidBounds(t *testing.T) {
	for _, args := range [][]string{
		{"--continue", "--max-turns", "0", "--max-time", "30m", "task"},
		{"--continue", "--max-turns", "-1", "--max-time", "30m", "task"},
		{"--continue", "--max-turns", "abc", "--max-time", "30m", "task"},
		{"--continue", "--max-turns", "3", "--max-time", "soon", "task"},
		{"--continue", "--max-turns", "3", "--max-time", "0s", "task"},
	} {
		if _, _, _, err := parseRunArgs(args); err == nil {
			t.Errorf("args %v: expected error", args)
		}
	}
}

func TestOrdinaryRunArgsUnaffectedByContinuous(t *testing.T) {
	task, plan, flags, err := parseRunArgs([]string{"--verify", "--budget", "2.50", "plain"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if task != "plain" || plan || !flags.Verify || flags.BudgetUSD != 2.50 {
		t.Fatalf("task=%q plan=%v flags=%+v", task, plan, flags)
	}
	if flags.Continuous != nil {
		t.Fatal("ordinary run must not enable continuous mode")
	}
}
