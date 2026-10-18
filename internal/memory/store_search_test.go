package memory

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ─── Relevance-ordered search (bm25) ─────────────────────────────────────

// TestSearchRelevanceOrder verifies that Search orders results by FTS5 bm25
// relevance (rank ascending) rather than recency alone: a highly relevant
// OLDER entry must outrank a marginally relevant NEWER one. Uses a
// controlled clock so created_at ordering is the reverse of relevance
// ordering — a recency-only implementation must fail this test.
func TestSearchRelevanceOrder(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(wsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "memory", "test.db")

	now := time.Now().UnixMilli()
	s, err := Open(dbPath, wsRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.nowFunc = func() int64 { return now }

	c := ctx(t)

	// OLDER entry (created first): dense in the query term — should rank
	// first despite being older.
	if _, err := s.PutActive(c, "old/relevant", "auth auth auth flow auth auth handler auth", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	oldCreated := now
	now += 60_000 // advance clock one minute

	// NEWER entry: mentions the query term once in passing.
	if _, err := s.PutActive(c, "recent/incidental", "we bumped the auth config, unrelated note", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if now <= oldCreated {
		t.Fatal("clock did not advance; test premise broken")
	}

	results, err := s.Search(c, "auth", "", false)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Key != "old/relevant" {
		t.Fatalf("expected old/relevant first (bm25 beats recency), got %s first", results[0].Key)
	}
	if results[1].Key != "recent/incidental" {
		t.Fatalf("expected recent/incidental second, got %s", results[1].Key)
	}
}

// TestSearchRelevanceTieBreakRecency verifies that when bm25 ranks tie
// exactly, the deterministic tie-break is created_at DESC (newest first).
func TestSearchRelevanceTieBreakRecency(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(wsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "memory", "test.db")

	now := time.Now().UnixMilli()
	s, err := Open(dbPath, wsRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.nowFunc = func() int64 { return now }

	c := ctx(t)

	// Two entries with identical bodies → identical bm25 rank.
	if _, err := s.PutActive(c, "a/first", "identical body", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	now += 1000 // advance clock so created_at differs
	if _, err := s.PutActive(c, "b/second", "identical body", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, ""); err != nil {
		t.Fatal(err)
	}

	results, err := s.Search(c, "identical", "", false)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Key != "b/second" {
		t.Fatalf("expected b/second (newer) first on rank tie, got %s", results[0].Key)
	}
}

// TestSearchFiltersBeforeLimit verifies that status/expiry/tier filters are
// applied BEFORE the LIMIT so filtered rows never consume the result cap.
// Excluded matches are made to OUTRANK the eligible match (denser term
// repetition = lower bm25) so a broken "limit FTS matches, then filter"
// implementation would drop the live entry and fail.
func TestSearchFiltersBeforeLimit(t *testing.T) {
	s, _ := newTestStore(t)
	c := ctx(t)

	// 25 expired entries, each denser in "auth" than the live entry.
	for i := 0; i < 25; i++ {
		expires := s.now() - 1 // already expired
		key := fmt.Sprintf("expired/auth-%02d", i)
		if _, err := s.PutActive(c, key, "auth auth auth auth auth auth auth auth", "note", TierMid, "main", "s1", TaintUnknown, &expires, nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	// One live entry that matches, but with a lower term density.
	if _, err := s.PutActive(c, "live/auth", "auth live note", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, ""); err != nil {
		t.Fatal(err)
	}

	results, err := s.Search(c, "auth", "", false)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected exactly 1 live result, got %d: %v", len(results), keysOf(results))
	}
	if results[0].Key != "live/auth" {
		t.Fatalf("expected live/auth, got %s", results[0].Key)
	}
	for _, r := range results {
		if r.Key == "live/auth" {
			continue
		}
		t.Errorf("expired entry %s leaked into results", r.Key)
	}
}

// TestSearchCapReturnsBest verifies that when eligible matches exceed
// searchCap, the cap returns the BEST ones by bm25, not an arbitrary subset.
// The densest entries are inserted FIRST (lowest ids) with the clock
// advancing, so created_at order and id order both OPPOSE relevance order —
// a "newest 20" or "first-scanned 20" selection fails this test.
func TestSearchCapReturnsBest(t *testing.T) {
	dir := t.TempDir()
	wsRoot := filepath.Join(dir, "workspace")
	if err := os.MkdirAll(wsRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "memory", "test.db")

	now := time.Now().UnixMilli()
	s, err := Open(dbPath, wsRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.nowFunc = func() int64 { return now }

	c := ctx(t)

	// 25 live entries: entry i (insert order) repeats "auth" (25-i) times,
	// so the FIRST-inserted entries are the densest (best bm25, lowest id,
	// oldest created_at). Relevance thus opposes both id and recency.
	for i := 0; i < 25; i++ {
		body := strings.Repeat("auth ", 25-i) + "tail"
		key := fmt.Sprintf("cap/auth-%02d", i)
		if _, err := s.PutActive(c, key, body, "note", TierMid, "main", "s1", TaintUnknown, nil, nil, ""); err != nil {
			t.Fatal(err)
		}
		now += 1000 // advance clock so created_at ordering opposes relevance
	}

	results, err := s.Search(c, "auth", "", false)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) != searchCap {
		t.Fatalf("expected exactly searchCap=%d results, got %d", searchCap, len(results))
	}
	// The top-20 must be the 20 densest entries (auth-00 .. auth-19).
	got := map[string]bool{}
	for _, r := range results {
		got[r.Key] = true
	}
	for i := 0; i < 20; i++ {
		want := fmt.Sprintf("cap/auth-%02d", i)
		if !got[want] {
			t.Errorf("dense entry %s missing from capped results; got %v", want, keysOf(results))
		}
	}
	for i := 20; i < 25; i++ {
		notWant := fmt.Sprintf("cap/auth-%02d", i)
		if got[notWant] {
			t.Errorf("sparse entry %s should have been excluded by cap; got %v", notWant, keysOf(results))
		}
	}
}

// keysOf is a test helper: the keys of a result slice, for failure messages.
func keysOf(entries []*Entry) []string {
	keys := make([]string, len(entries))
	for i, e := range entries {
		keys[i] = e.Key
	}
	return keys
}

// TestSearchRelevanceTieBreakID verifies the final id DESC tie-break: when
// bm25 rank AND created_at are both identical, the newer row (higher id,
// inserted later) wins. Frozen clock + identical bodies isolate the id term.
func TestSearchRelevanceTieBreakID(t *testing.T) {
	s, _ := newTestStore(t)
	c := ctx(t)

	if _, err := s.PutActive(c, "a/first", "same body words", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutActive(c, "b/second", "same body words", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, ""); err != nil {
		t.Fatal(err)
	}

	results, err := s.Search(c, "body", "", false)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	if results[0].Key != "b/second" || results[1].Key != "a/first" {
		t.Fatalf("expected [b/second a/first] (id DESC), got %v", keysOf(results))
	}
}

// TestSearchStillExcludesSuperseded verifies the rank join does not regress
// the active-status filter: superseded entries matching the query stay out.
func TestSearchStillExcludesSuperseded(t *testing.T) {
	s, _ := newTestStore(t)
	c := ctx(t)

	if _, err := s.PutActive(c, "sup/x", "gamma content", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	// Supersede with a different value.
	if _, err := s.PutActive(c, "sup/x", "delta content", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, ""); err != nil {
		t.Fatal(err)
	}

	results, err := s.Search(c, "gamma", "", false)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("expected 0 results for 'gamma' (superseded), got %d", len(results))
	}
	results, err = s.Search(c, "delta", "", false)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result for 'delta', got %d", len(results))
	}
}

// TestSearchIncludeProposed verifies includeProposed still works with the
// rank join — proposed entries are returned alongside active ones.
func TestSearchIncludeProposed(t *testing.T) {
	s, _ := newTestStore(t)
	c := ctx(t)

	if _, err := s.PutActive(c, "act/x", "epsilon content", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutProposed(c, "pro/x", "epsilon proposal", "decision", "sub-a", "s1", TaintUnknown, nil, ""); err != nil {
		t.Fatal(err)
	}

	results, err := s.Search(c, "epsilon", "", false)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 active-only result, got %d", len(results))
	}

	results, err = s.Search(c, "epsilon", "", true)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results with includeProposed, got %d", len(results))
	}
}

// TestSearchTierFilterWithRank verifies the tier filter composes with the
// rank join.
func TestSearchTierFilterWithRank(t *testing.T) {
	s, _ := newTestStore(t)
	c := ctx(t)

	if _, err := s.PutActive(c, "mid/x", "zeta content", "note", TierMid, "main", "s1", TaintUnknown, nil, nil, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutActive(c, "dur/x", "zeta content", "note", TierDurable, "main", "s1", TaintUnknown, nil, nil, ""); err != nil {
		t.Fatal(err)
	}

	results, err := s.Search(c, "zeta", TierDurable, false)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 durable result, got %d", len(results))
	}
	if results[0].Key != "dur/x" {
		t.Fatalf("expected dur/x, got %s", results[0].Key)
	}
}
