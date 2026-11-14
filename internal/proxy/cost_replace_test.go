package proxy

import "testing"

// TestReplaceRowsReplacesNotMerges pins the /resume cost semantics
// (Group 3): resuming a session replaces the tracker, so old rows from the
// previous session are gone and the resumed session's rows are the whole
// state (no merge, no double-count).
func TestReplaceRowsReplacesNotMerges(t *testing.T) {
	tr := NewCostTracker()
	tr.Record("src-a", 100, 50, 0.30, true, "exact")
	tr.Record("src-b", 10, 10, 0.10, true, "exact")
	if total, _ := tr.Snapshot(); total != 0.40 {
		t.Fatalf("seeded total = %v, want 0.40", total)
	}

	sess := []CostRow{
		{Source: "src-a", Calls: 1, InputTok: 100, OutputTok: 50, CostUSD: 0.30, Priced: true, Confidence: "exact", Category: CategoryForSource("src-a")},
		{Source: "src-c", Calls: 2, InputTok: 200, OutputTok: 40, CostUSD: 0.25, Priced: true, Confidence: "exact", Category: CategoryForSource("src-c")},
	}
	tr.ReplaceRows(sess)

	total, rows := tr.Snapshot()
	if total != 0.55 {
		t.Fatalf("replaced total = %v, want 0.55 (src-a 0.30 + src-c 0.25; src-b must be gone)", total)
	}
	got := map[string]float64{}
	for _, r := range rows {
		got[r.Source] = r.CostUSD
	}
	if got["src-b"] != 0 || got["src-a"] != 0.30 || got["src-c"] != 0.25 {
		t.Fatalf("rows = %v, want src-a 0.30 src-c 0.25, src-b absent", got)
	}

	// Empty session rows replace with empty (legacy sessions predate rows).
	tr.ReplaceRows(nil)
	if total, rows := tr.Snapshot(); total != 0 || len(rows) != 0 {
		t.Fatalf("after ReplaceRows(nil): total=%v rows=%v, want empty", total, rows)
	}

	// Repeat calls don't double-count (idempotent replace).
	tr.ReplaceRows(sess)
	tr.ReplaceRows(sess)
	if total, _ := tr.Snapshot(); total != 0.55 {
		t.Fatalf("after double ReplaceRows total = %v, want 0.55", total)
	}
}
