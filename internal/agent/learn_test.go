package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsSubstantiveQuery(t *testing.T) {
	substantive := []string{
		"how does the arrange retriever pick documents",
		"explain the grounding header semantics please",
	}
	trivial := []string{
		"",
		"hi",
		"thanks!",
		"/help",
		"ok cool",        // 2 words / short
		"what is up now", // 14 runes (<16)
	}
	for _, q := range substantive {
		if !isSubstantiveQuery(q) {
			t.Errorf("expected substantive: %q", q)
		}
	}
	for _, q := range trivial {
		if isSubstantiveQuery(q) {
			t.Errorf("expected trivial: %q", q)
		}
	}
}

func readLearnLog(t *testing.T, home string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(home, ".wakil", "learn-candidates.log"))
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(b)
}

func TestMaybeLogLearnCandidate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	app := &App{}

	query := "how does the arrange retriever rank candidate documents"

	// Attempted + low score (< gate) + substantive → logged.
	app.maybeLogLearnCandidate(query, true, 0.22)
	got := readLearnLog(t, home)
	if !strings.Contains(got, query) || !strings.Contains(got, learnLogNote) {
		t.Fatalf("expected the query logged with note, got: %q", got)
	}
	if !strings.Contains(got, "\t0.220\t") {
		t.Fatalf("expected max_score 0.220 (3dp) in the line, got: %q", got)
	}
	tabs := strings.Count(strings.TrimRight(got, "\n"), "\t")
	if tabs != 3 { // timestamp \t query \t score \t note
		t.Fatalf("expected 3 tabs (4 fields), got %d in %q", tabs, got)
	}

	// Attempted + high score (>= gate) → not logged.
	app.maybeLogLearnCandidate("another substantive query about retrieval internals", true, 0.80)
	// Exactly at the gate → not logged (strict <).
	app.maybeLogLearnCandidate("yet another substantive query at the boundary", true, learnMaxScoreGate)
	// Retrieval skipped/absent → never a candidate.
	app.maybeLogLearnCandidate("a skipped substantive query about retrieval", false, 0.0)
	// Trivial query → not logged even with a zero score.
	app.maybeLogLearnCandidate("hi there", true, 0.0)

	if n := strings.Count(readLearnLog(t, home), "\n"); n != 1 {
		t.Fatalf("expected exactly 1 logged line, got %d", n)
	}
}

func TestMaybeLogAbsentHeaderWhileAttempted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	app := &App{}

	// Absent/empty X-Ilm-Grounding while X-Ilm-Retrieval==attempted → max 0.0 →
	// strongest hole signal → logged.
	maxScore := 0.0
	app.maybeLogLearnCandidate("explain the arrange retrieval scoring model", true, maxScore)
	if got := readLearnLog(t, home); !strings.Contains(got, "\t0.000\t") {
		t.Fatalf("expected logged line with 0.000 score, got: %q", got)
	}
}

func TestMaybeLogStripsMentionBlocks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	app := &App{}

	outgoing := "explain this retriever behavior for me\n\n--- @main.go (1.2 KB) ---\n```go\npackage main\n```"
	app.maybeLogLearnCandidate(outgoing, true, 0.1)

	got := readLearnLog(t, home)
	if !strings.Contains(got, "explain this retriever behavior for me") {
		t.Fatalf("typed query not logged: %q", got)
	}
	if strings.Contains(got, "package main") || strings.Contains(got, "@main.go") {
		t.Fatalf("injected block must not be logged: %q", got)
	}
}
