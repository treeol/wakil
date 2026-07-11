package proxy

import "testing"

// TestCostTrackerRecordCachedTokensAccumulate verifies the optional cachedTok
// argument to Record accumulates per-source into CostRow.CachedTok, the same
// way InputTok/OutputTok already do.
func TestCostTrackerRecordCachedTokensAccumulate(t *testing.T) {
	tr := NewCostTracker()
	tr.Record("inference·local", 1000, 100, 0.01, true, ConfModeled, 250)
	tr.Record("inference·local", 2000, 200, 0.02, true, ConfModeled, 500)

	_, rows := tr.Snapshot()
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].CachedTok != 750 {
		t.Errorf("CachedTok = %d, want 750 (250+500 accumulated)", rows[0].CachedTok)
	}
	if rows[0].InputTok != 3000 {
		t.Errorf("InputTok = %d, want 3000 (unaffected by the new param)", rows[0].InputTok)
	}
}

// TestCostTrackerRecordWithoutCachedTokensDefaultsToZero verifies every
// pre-existing 6-arg call shape (used throughout the codebase and its tests)
// keeps compiling and behaving identically — CachedTok stays 0 when the
// variadic argument is omitted.
func TestCostTrackerRecordWithoutCachedTokensDefaultsToZero(t *testing.T) {
	tr := NewCostTracker()
	tr.Record(CostSourceSearch, 0, 0, 0.05, true, ConfModeled) // no cachedTok arg at all

	_, rows := tr.Snapshot()
	if len(rows) != 1 || rows[0].CachedTok != 0 {
		t.Errorf("rows = %+v, want one row with CachedTok 0", rows)
	}
}
