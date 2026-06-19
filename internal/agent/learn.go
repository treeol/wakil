package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"wakil/internal/tools"
)

// Phase 0 learn-candidate logging (detection only — no UI, no proposals, no
// extraction). When the proxy attempts retrieval for a substantive user query
// but the best chunk scores below the coverage gate, the query is silently
// appended to ~/.wakil/learn-candidates.log. Best-effort: any failure is
// swallowed and never interrupts the turn.

const learnLogNote = "retrieval attempted, low coverage"

// learnMaxScoreGate is the COLLECTION threshold: a turn is a learn candidate
// when the best retrieved chunk scores below this. Deliberately looser than the
// eval's zero-FP 0.35 so the boundary isn't missed; the proposal stage tightens
// it from real data. One-line knob.
const learnMaxScoreGate = 0.45

// maybeLogLearnCandidate logs the turn iff: retrieval was attempted
// (X-Ilm-Retrieval=="attempted"), the max chunk score is below the gate, and the
// query is substantive. rawQuery is the outgoing message; injected "@" blocks
// are stripped.
// maybeLogLearnCandidate logs the turn if it meets the learn-candidate criteria.
// Returns true when appendLearnCandidate was called (caller may use this to show
// the end-of-turn nudge).
func (a *App) maybeLogLearnCandidate(rawQuery string, attempted bool, maxScore float64) bool {
	if !attempted || maxScore >= learnMaxScoreGate {
		return false
	}
	q := tools.UserQueryText(rawQuery)
	if !isSubstantiveQuery(q) {
		return false
	}
	appendLearnCandidate(q, maxScore)
	return true
}

// isSubstantiveQuery filters out trivial inputs: blanks, slash commands,
// greetings/acks, and very short one-liners. Heuristic: ≥16 runes AND ≥3 words.
func isSubstantiveQuery(q string) bool {
	q = strings.TrimSpace(q)
	if q == "" || strings.HasPrefix(q, "/") {
		return false
	}
	return utf8.RuneCountInString(q) >= 16 && len(strings.Fields(q)) >= 3
}

// appendLearnCandidate writes one tab-separated line:
// timestamp, query, max_score (3dp), note.
func appendLearnCandidate(query string, maxScore float64) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	dir := filepath.Join(home, ".wakil")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "learn-candidates.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()

	oneLine := strings.Join(strings.Fields(query), " ") // collapse whitespace/newlines
	if len(oneLine) > 500 {
		oneLine = oneLine[:500] + "…"
	}
	fmt.Fprintf(f, "%s\t%s\t%.3f\t%s\n", time.Now().Format(time.RFC3339), oneLine, maxScore, learnLogNote)
}
