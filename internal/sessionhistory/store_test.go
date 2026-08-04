package sessionhistory

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// newTestStore opens a Store in a temp dir.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "sessionhistory", "test.db")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func input(chatID, ws string, turns []Turn) IndexInput {
	now := time.Unix(1700000000, 0)
	return IndexInput{
		ChatID:    chatID,
		Workspace: ws,
		Created:   now,
		Updated:   now,
		Turns:     turns,
		Tainted:   true,
	}
}

func TestIndexAndSearch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	err := s.Index(ctx, input("abc", "/ws/a", []Turn{
		{Ordinal: 0, Role: "user", Text: "how do I fix the io_uring sandbox"},
		{Ordinal: 0, Role: "assistant", Text: "set the syscall allowlist and remount"},
	}))
	if err != nil {
		t.Fatalf("index: %v", err)
	}

	res, err := s.Search(ctx, "io_uring sandbox", "/ws/a", "", 8)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("expected 1 result, got %d", len(res))
	}
	if res[0].ChatID != "abc" {
		t.Errorf("wrong chat_id: %s", res[0].ChatID)
	}
	if len(res[0].Turns) == 0 {
		t.Fatal("expected matched turns")
	}
	if res[0].Turns[0].Role != "user" {
		t.Errorf("expected first turn to be user role, got %s", res[0].Turns[0].Role)
	}
}

func TestSearchWorkspaceIsolation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Index(ctx, input("s1", "/ws/a", []Turn{{Ordinal: 0, Role: "user", Text: "io_uring setup details"}})); err != nil {
		t.Fatal(err)
	}
	if err := s.Index(ctx, input("s2", "/ws/b", []Turn{{Ordinal: 0, Role: "user", Text: "io_uring setup details"}})); err != nil {
		t.Fatal(err)
	}

	// Workspace A must not see workspace B's session.
	res, err := s.Search(ctx, "io_uring setup", "/ws/a", "", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].ChatID != "s1" {
		t.Fatalf("workspace isolation violated: %+v", res)
	}
}

func TestSearchEmptyWorkspaceFailClosed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Index(ctx, input("s1", "/ws/a", []Turn{{Ordinal: 0, Role: "user", Text: "some content"}})); err != nil {
		t.Fatal(err)
	}

	// Empty workspace must return nothing, never a global search.
	res, err := s.Search(ctx, "some content", "", "", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Fatalf("empty workspace must be fail-closed, got %d results", len(res))
	}
}

func TestIndexEmptyWorkspaceRejected(t *testing.T) {
	s := newTestStore(t)
	err := s.Index(context.Background(), input("s1", "", []Turn{{Ordinal: 0, Role: "user", Text: "x"}}))
	if err == nil {
		t.Fatal("expected error indexing with empty workspace")
	}
}

func TestCurrentSessionExcluded(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Index(ctx, input("current", "/ws/a", []Turn{{Ordinal: 0, Role: "user", Text: "io_uring setup"}})); err != nil {
		t.Fatal(err)
	}

	res, err := s.Search(ctx, "io_uring", "/ws/a", "current", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Fatalf("current session must be excluded, got %d results", len(res))
	}
}

func TestWholeSessionReplaceOnChange(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Index(ctx, input("s1", "/ws/a", []Turn{{Ordinal: 0, Role: "user", Text: "old text about io_uring"}})); err != nil {
		t.Fatal(err)
	}
	// Replace the whole session; the old turn content must be gone.
	if err := s.Index(ctx, input("s1", "/ws/a", []Turn{{Ordinal: 0, Role: "user", Text: "new text about networking"}})); err != nil {
		t.Fatal(err)
	}

	res, err := s.Search(ctx, "io_uring", "/ws/a", "", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Fatalf("old content must be replaced atomically, got %d results", len(res))
	}

	res, err = s.Search(ctx, "networking", "/ws/a", "", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("replacement content not found, got %d", len(res))
	}
}

func TestDeletePurges(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Index(ctx, input("s1", "/ws/a", []Turn{{Ordinal: 0, Role: "user", Text: "io_uring setup"}})); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	res, _ := s.Search(ctx, "io_uring", "/ws/a", "", 8)
	if len(res) != 0 {
		t.Fatalf("deleted session still searchable: %+v", res)
	}
}

func TestDeleteThenReingest(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	in := input("s1", "/ws/a", []Turn{{Ordinal: 0, Role: "user", Text: "io_uring setup"}})
	in.Summary = "first summary"
	in.SummaryGenerated = true
	if err := s.Index(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	// Re-ingest after delete — both turn and summary FTS must work again.
	in2 := input("s1", "/ws/a", []Turn{{Ordinal: 0, Role: "user", Text: "io_uring setup again"}})
	in2.Summary = "second summary"
	in2.SummaryGenerated = true
	if err := s.Index(ctx, in2); err != nil {
		t.Fatal(err)
	}

	// Matching by turn text.
	res, _ := s.Search(ctx, "io_uring", "/ws/a", "", 8)
	if len(res) != 1 {
		t.Fatalf("expected 1 result after reingest, got %d", len(res))
	}
	// Matching by summary text.
	res, _ = s.Search(ctx, "second", "/ws/a", "", 8)
	if len(res) != 1 {
		t.Fatalf("expected summary match after reingest, got %d", len(res))
	}
}

func TestGetSummary(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	in := input("s1", "/ws/a", []Turn{{Ordinal: 0, Role: "user", Text: "hello"}})
	in.Summary = "generated summary text"
	in.SummaryGenerated = true
	if err := s.Index(ctx, in); err != nil {
		t.Fatal(err)
	}

	got, gen, err := s.GetSummary(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if !gen || got != "generated summary text" {
		t.Fatalf("unexpected (summary=%q gen=%v)", got, gen)
	}

	// Unknown session returns empty, no error.
	got, gen, err = s.GetSummary(ctx, "nope")
	if err != nil {
		t.Fatalf("unknown session should not error: %v", err)
	}
	if got != "" || gen {
		t.Fatalf("unknown session should return empty: (%q, %v)", got, gen)
	}
}

func TestSearchMatchesSummary(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Turn text does NOT contain the query; only the summary does.
	in := input("s1", "/ws/a", []Turn{{Ordinal: 0, Role: "user", Text: "fixed several things"}})
	in.Summary = "the io_uring sandbox allowlist was reconfigured"
	in.SummaryGenerated = true
	if err := s.Index(ctx, in); err != nil {
		t.Fatal(err)
	}

	res, err := s.Search(ctx, "sandbox allowlist", "/ws/a", "", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("expected summary match, got %d results", len(res))
	}
	foundSummary := false
	for _, t2 := range res[0].Turns {
		if t2.Role == "summary" {
			foundSummary = true
		}
	}
	if !foundSummary {
		t.Fatalf("expected a synthetic summary turn, got: %+v", res[0].Turns)
	}
}

func TestListMeta(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Index(ctx, input("s1", "/ws/a", []Turn{{Ordinal: 0, Role: "user", Text: "x"}})); err != nil {
		t.Fatal(err)
	}
	meta, err := s.ListMeta(ctx, "/ws/a")
	if err != nil {
		t.Fatal(err)
	}
	if len(meta) != 1 || meta[0].ChatID != "s1" {
		t.Fatalf("unexpected meta: %+v", meta)
	}
}

func TestNonASCIIQuery(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Index(ctx, input("s1", "/ws/a", []Turn{{Ordinal: 0, Role: "user", Text: "on a configuré le sandbox io_uring en français"}})); err != nil {
		t.Fatal(err)
	}

	res, err := s.Search(ctx, "configuré io_uring", "/ws/a", "", 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("non-ascii search failed, got %d results", len(res))
	}
}

func TestSanitizeQueryBasic(t *testing.T) {
	cases := []struct {
		in   string
		want []string // tokens that must be present (OR-joined), any single quoted token
	}{
		{"io_uring sandbox setup", []string{"io_uring", "sandbox", "setup"}},
	}
	for _, c := range cases {
		got := sanitizeQuery(c.in)
		if got == "" {
			t.Fatalf("sanitizeQuery(%q) = empty", c.in)
		}
		for _, tok := range c.want {
			if !contains(got, "\""+tok+"\"") {
				t.Errorf("sanitizeQuery(%q) missing token %q: %s", c.in, tok, got)
			}
		}
	}
}

func TestSanitizeQuerySingleChar(t *testing.T) {
	// Single-char tokens are dropped (noise), so a query of only "a b" is empty.
	if got := sanitizeQuery("a b"); got != "" {
		t.Errorf("expected empty for single-char tokens, got %q", got)
	}
}

func TestGetTurnsRange(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	err := s.Index(ctx, input("abc", "/ws/a", []Turn{
		{Ordinal: 0, Role: "user", Text: "request 0"},
		{Ordinal: 0, Role: "assistant", Text: "answer 0"},
		{Ordinal: 1, Role: "user", Text: "request 1"},
		{Ordinal: 1, Role: "assistant", Text: "answer 1"},
		{Ordinal: 2, Role: "user", Text: "request 2"},
		{Ordinal: 2, Role: "assistant", Text: "answer 2"},
	}))
	if err != nil {
		t.Fatalf("index: %v", err)
	}

	// Single ordinal.
	turns, err := s.GetTurns(ctx, "abc", "/ws/a", 1, 1)
	if err != nil {
		t.Fatalf("get turns: %v", err)
	}
	if len(turns) != 2 || turns[0].Ordinal != 1 {
		t.Errorf("single range 1..1 should return both turns of ordinal 1; got %+v", turns)
	}

	// Open-ended (whole session).
	turns, err = s.GetTurns(ctx, "abc", "/ws/a", -1, -1)
	if err != nil {
		t.Fatalf("whole session: %v", err)
	}
	if len(turns) != 6 {
		t.Errorf("whole session should return 6 turns; got %d", len(turns))
	}
	if turns[0].Ordinal != 0 || turns[len(turns)-1].Ordinal != 2 {
		t.Errorf("deterministic order expected; got first=%d last=%d", turns[0].Ordinal, turns[len(turns)-1].Ordinal)
	}
}

func TestGetTurnsWorkspaceScoped(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Index(ctx, input("s1", "/ws/a", []Turn{{Ordinal: 0, Role: "user", Text: "a-request"}})); err != nil {
		t.Fatal(err)
	}
	// Different workspace must not see it (indistinguishable from missing).
	turns, err := s.GetTurns(ctx, "s1", "/ws/b", -1, -1)
	if err != nil {
		t.Fatalf("wrong-workspace get: %v", err)
	}
	if len(turns) != 0 {
		t.Errorf("wrong workspace must return no turns; got %d", len(turns))
	}
	// Empty workspace is fail-closed.
	turns, err = s.GetTurns(ctx, "s1", "", -1, -1)
	if err != nil || len(turns) != 0 {
		t.Errorf("empty workspace should return nil, no error; got err=%v n=%d", err, len(turns))
	}
}

func TestGetTurnsInvalidRange(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Index(ctx, input("s1", "/ws/a", []Turn{{Ordinal: 0, Role: "user", Text: "r"}})); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetTurns(ctx, "s1", "/ws/a", 2, 1); err == nil {
		t.Error("reversed range should return an error (invalid input)")
	} else if !errors.Is(err, ErrInvalidRange) {
		t.Errorf("expected ErrInvalidRange, got %v", err)
	}
}

func TestGetTurnsOrderingMatchesInsertion(t *testing.T) {
	// GetTurns ORDER BY t.id (insertion order). Index inserts turns in the order
	// of the provided slice, so a slice with out-of-ordinal order must still
	// round-trip in insertion order.
	s := newTestStore(t)
	ctx := context.Background()
	// Deliberately non-ascending ordinals to prove the output follows insertion
	// (id) order, not ordinal sort.
	err := s.Index(ctx, input("s1", "/ws/a", []Turn{
		{Ordinal: 2, Role: "user", Text: "third"},
		{Ordinal: 0, Role: "user", Text: "first"},
		{Ordinal: 1, Role: "user", Text: "second"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	turns, err := s.GetTurns(ctx, "s1", "/ws/a", -1, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 3 {
		t.Fatalf("want 3 turns, got %d", len(turns))
	}
	want := []string{"third", "first", "second"}
	for i, w := range want {
		if turns[i].Text != w {
			t.Errorf("turns[%d] = %q, want %q (insertion order)", i, turns[i].Text, w)
		}
	}
}

func TestGetTurnsPartialOpenRanges(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if err := s.Index(ctx, input("s1", "/ws/a", []Turn{
		{Ordinal: 0, Role: "user", Text: "req0"},
		{Ordinal: 1, Role: "user", Text: "req1"},
		{Ordinal: 2, Role: "user", Text: "req2"},
	})); err != nil {
		t.Fatal(err)
	}
	// Open upper bound.
	turns, err := s.GetTurns(ctx, "s1", "/ws/a", 1, -1)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || turns[0].Ordinal != 1 {
		t.Errorf("open upper (1,..) should return ordinals 1+ ; got %+v", turns)
	}
	// Open lower bound.
	turns, err = s.GetTurns(ctx, "s1", "/ws/a", -1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 || turns[0].Ordinal != 0 {
		t.Errorf("open lower (..,1) should return ordinals 0-1 ; got %+v", turns)
	}
	// Out-of-range request returns empty (not error).
	turns, err = s.GetTurns(ctx, "s1", "/ws/a", 10, 20)
	if err != nil || len(turns) != 0 {
		t.Errorf("out-of-range ordinals should return empty, no error; got err=%v n=%d", err, len(turns))
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
