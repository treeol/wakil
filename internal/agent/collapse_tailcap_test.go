package agent

import (
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// --- B1b: tail-answer cap ---------------------------------------------------
//
// The 20-turn tail keeps FULL answers (U + A + note) — in long sessions A
// is often multi-KB, so the tail dominates the context floor. The
// context_tail_answer_bytes cap truncates retained answers (rune-safe,
// archive pointer appended), dropping the floor to ~20 × (U + 0.5k).
// U, the note, and degraded pointer turns are never capped. 0 disables.

// TestTailAnswerCapTruncates: a tail turn with a multi-KB answer projects
// A capped at the configured bytes (marker included in the budget), with
// the archive path carried by the NOTE — not duplicated in the answer.
func TestTailAnswerCapTruncates(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextTailAnswerBytes = 300
	app.Cfg.ContextTailTurns = 20 // turn stays in the tail (no pointer degrade)

	// One collapsed turn with a large answer.
	conv := oneCollapsedTurn(1)
	for i := range conv {
		conv[i].Turn = 1
	}
	longAnswer := strings.Repeat("detailed ", 500) // 4.5k
	conv[len(conv)-1].Content = StrPtr(longAnswer)
	app.Conv = conv
	app.stateMu.Lock()
	app.collapsed = map[int]CollapseRecord{
		1: {Turn: 1, Archive: "/tmp/fake-archive-turn-1",
			Note: "[request 1 working set collapsed]\nfull record: /tmp/fake-archive-turn-1 (read_file with offset/limit)\n"},
	}
	app.stateMu.Unlock()

	proj := app.contextProjection()
	if len(proj) != 2 {
		t.Fatalf("want U+A (2 msgs), got %d", len(proj))
	}
	got := *proj[1].Content
	// Capped answer opens the content; the note (with the archive path)
	// follows it. The answer part must end with the marker.
	if !strings.Contains(got, "…") {
		t.Errorf("capped answer missing truncation marker:\n…%s", tailStr(got, 120))
	}
	// Recovery reference present exactly once — in the note, not the answer.
	if strings.Count(got, "/tmp/fake-archive-turn-1") != 1 {
		t.Errorf("archive path must appear exactly once (in the note):\n%s", got)
	}
	// Total projected A ≤ 300 (answer+marker) + "\n\n" join + note.
	maxLen := 300 + 2 + len("[request 1 working set collapsed]\nfull record: /tmp/fake-archive-turn-1 (read_file with offset/limit)\n")
	if len(got) > maxLen {
		t.Errorf("capped A + note still large: %d bytes (max %d)", len(got), maxLen)
	}
	// The original Conv must NOT be mutated (projection works on a copy).
	if !strings.Contains(*app.Conv[len(app.Conv)-1].Content, longAnswer) {
		t.Error("original Conv A was mutated by the projection cap")
	}
}

// TestTailAnswerCapZeroKeepsFull: 0 disables the cap — the full answer is
// kept (v1 tail behavior).
func TestTailAnswerCapZeroKeepsFull(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextTailAnswerBytes = 0 // disabled
	app.Cfg.ContextTailTurns = 20

	conv := oneCollapsedTurn(1)
	for i := range conv {
		conv[i].Turn = 1
	}
	longAnswer := strings.Repeat("detailed ", 500)
	conv[len(conv)-1].Content = StrPtr(longAnswer)
	app.Conv = conv
	app.stateMu.Lock()
	app.collapsed = map[int]CollapseRecord{
		1: {Turn: 1, Archive: "/tmp/fake-archive-turn-1", Note: "[request 1 working set collapsed]\n"},
	}
	app.stateMu.Unlock()

	proj := app.contextProjection()
	got := *proj[1].Content
	if !strings.Contains(got, strings.Repeat("detailed ", 500)) {
		t.Error("cap=0 must keep the full answer verbatim")
	}
	if strings.Contains(got, "full answer archived") {
		t.Error("cap=0 must not add an archive pointer to A")
	}
}

// TestTailAnswerCapSkipsShortAnswers: an answer already under the cap is
// passed through unmodified — no pointer, no truncation marker.
func TestTailAnswerCapSkipsShortAnswers(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextTailAnswerBytes = 500

	conv := oneCollapsedTurn(1)
	for i := range conv {
		conv[i].Turn = 1
	}
	app.Conv = conv
	app.stateMu.Lock()
	app.collapsed = map[int]CollapseRecord{
		1: {Turn: 1, Archive: "/tmp/fake-archive-turn-1", Note: "[request 1 working set collapsed]\n"},
	}
	app.stateMu.Unlock()

	proj := app.contextProjection()
	got := *proj[1].Content
	if !strings.Contains(got, "Here is the answer.") {
		t.Errorf("short answer must pass through verbatim, got: %q", got)
	}
	if strings.Contains(got, "full answer archived") {
		t.Error("short answer must not gain an archive pointer")
	}
}

// TestTailAnswerCapNotAppliedToDegradedOrKept: pointer-degraded turns and
// /keep-exempt turns never pass through the cap path.
func TestTailAnswerCapNotAppliedToDegradedOrKept(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextTailAnswerBytes = 100
	app.Cfg.ContextTailTurns = 1 // turn 1 degrades to a pointer line

	buildManyCollapsedTurns(t, app, 2) // turns 1..2; turn 1 > tail of 1 → pointer
	// /keep turn 2 — exempt turns bypass collapse entirely.
	app.stateMu.Lock()
	app.keep = map[int]bool{2: true}
	app.stateMu.Unlock()

	proj := app.contextProjection()

	pointers := 0
	for _, m := range proj {
		if strings.Contains(DerefStr(m.Content), "details archived") {
			pointers++
		}
	}
	if pointers != 1 {
		t.Fatalf("want 1 pointer line, got %d", pointers)
	}
	// The pointer line is not answer content and never carries the answer
	// cap suffix.
	for _, m := range proj {
		if strings.Contains(DerefStr(m.Content), "full answer archived") {
			t.Error("cap suffix must not appear on degraded pointer turns")
		}
	}
	// The kept turn is sent in full — its entire original answer verbatim,
	// never capped (kept turns don't consume tail slots and bypass the
	// collapsed-turn emission path).
	keptFull := false
	for _, m := range proj {
		if m.Role == "assistant" && strings.Contains(DerefStr(m.Content), "Here is the answer.") {
			keptFull = true
		}
	}
	if !keptFull {
		t.Error("kept turn's answer missing from projection")
	}
}

// TestTailAnswerCapUTF8Safe: truncation lands mid-CJK without splitting a
// rune. Cap 122 → budget 119 = 39×3 + 2: truncation lands 2 bytes into a
// 3-byte rune — a genuine mid-rune boundary (the old cap of 120 divided
// evenly and never tested the hard case).
func TestTailAnswerCapUTF8Safe(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextTailAnswerBytes = 122

	conv := oneCollapsedTurn(1)
	for i := range conv {
		conv[i].Turn = 1
	}
	conv[len(conv)-1].Content = StrPtr(strings.Repeat("答", 300)) // 900 bytes
	app.Conv = conv
	app.stateMu.Lock()
	app.collapsed = map[int]CollapseRecord{
		1: {Turn: 1, Archive: "/tmp/fake-archive-turn-1", Note: "[request 1 working set collapsed]\n"},
	}
	app.stateMu.Unlock()

	proj := app.contextProjection()
	got := *proj[1].Content
	if !utf8.ValidString(got) {
		t.Error("capped answer split a UTF-8 rune")
	}
}

// TestCapTailAnswerEdgeCases: direct unit tests of the helper's contract —
// small caps treated as disabled, no-grow guard, marker-in-budget, empty
// handling, exact fit, mid-rune and mixed-width truncation.
func TestCapTailAnswerEdgeCases(t *testing.T) {
	// Disabled / too-small caps → "" (no rewrite).
	for _, cap := range []int{0, -5, 1, 2, 3, 4} { // 4 = len("…")+1, below the +2 threshold
		if got := capTailAnswer(strings.Repeat("x", 9000), cap, "/arch"); got != "" {
			t.Errorf("cap %d: want no-op, got %q", cap, got)
		}
	}
	// Answer already fits → no rewrite.
	if got := capTailAnswer("short", 500, "/arch"); got != "" {
		t.Errorf("fitting answer rewritten: %q", got)
	}
	// Empty answer → no rewrite.
	if got := capTailAnswer("", 500, "/arch"); got != "" {
		t.Errorf("empty answer rewritten: %q", got)
	}
	// Exact fit → no rewrite (never grows the message).
	exact := strings.Repeat("y", 500)
	if got := capTailAnswer(exact, 500, "/arch"); got != "" {
		t.Errorf("exact fit rewritten: %q", got)
	}
	// One byte over → capped to at most capBytes, marker included.
	over := strings.Repeat("y", 501)
	got := capTailAnswer(over, 500, "/arch")
	if got == "" || len(got) > 500 {
		t.Errorf("one-over cap: want <=500 bytes, got %d (%q)", len(got), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("one-over cap missing marker: %q", got)
	}
	// No archive → still caps (the path lives in the note, not the answer).
	got = capTailAnswer(over, 500, "")
	if got == "" || !strings.HasSuffix(got, "…") {
		t.Errorf("empty archive: want capped+marker, got %q", got)
	}
	// Mid-rune boundary: cap 50 → budget 47 = 15×3 + 2 — 2 bytes into a
	// 3-byte CJK rune, the genuine hard case.
	cjk := strings.Repeat("答", 100) // 300 bytes
	got = capTailAnswer(cjk, 50, "/arch")
	if got == "" {
		t.Fatal("cjk not capped")
	}
	if len(got) > 50 {
		t.Errorf("cjk cap exceeded budget: %d bytes", len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("cjk cap split a rune: %q", got)
	}
	// Mixed-width content.
	mixed := strings.Repeat("a", 20) + strings.Repeat("答", 40) + strings.Repeat("b", 200)
	got = capTailAnswer(mixed, 60, "/arch")
	if got == "" || len(got) > 60 || !utf8.ValidString(got) {
		t.Errorf("mixed-width cap: len=%d valid=%v", len(got), utf8.ValidString(got))
	}
}

// TestTailAnswerCapMaterializePreservation: the compaction-fallback path
// (materializeCollapsed) installs the projection — capped answers — into
// the real conversation. The archive must hold the FULL answer (capping
// happens only at projection), so the truncated tail stays recoverable.
// After materialization, records are cleared: a further projection must not
// re-truncate or duplicate the note.
func TestTailAnswerCapMaterializePreservation(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextTailAnswerBytes = 200
	app.MemoryStore = testStore(t)

	// Real collapse so the archive exists on disk with the full turn.
	conv := oneCollapsedTurn(1)
	for i := range conv {
		conv[i].Turn = 1
	}
	longAnswer := strings.Repeat("final answer body ", 300) // 5.1k
	conv[len(conv)-1].Content = StrPtr(longAnswer)
	app.Conv = conv
	app.turnSeq = 1
	app.collapseFinishedTurns()

	app.stateMu.RLock()
	rec, ok := app.collapsed[1]
	app.stateMu.RUnlock()
	if !ok {
		t.Fatal("turn 1 should have collapsed")
	}

	// The archive (durable copy) contains the FULL answer — the cap only
	// affects what is sent, never what is stored.
	archBytes, err := os.ReadFile(rec.Archive)
	if err != nil {
		t.Fatalf("read archive: %v", err)
	}
	if !strings.Contains(string(archBytes), longAnswer) {
		t.Error("archive lost the full answer — capping must be projection-only")
	}

	// Materialize (compaction fallback): the projection replaces a.Conv.
	app.materializeCollapsed()

	app.stateMu.RLock()
	stillCollapsed := len(app.collapsed)
	app.stateMu.RUnlock()
	if stillCollapsed != 0 {
		t.Fatalf("materialize must clear collapse records, got %d", stillCollapsed)
	}
	// The materialized A is capped and carries the note; exactly one note.
	matA := DerefStr(app.Conv[len(app.Conv)-1].Content)
	if !strings.Contains(matA, "…") {
		t.Errorf("materialized A not capped: %.80s…", matA)
	}
	if strings.Count(matA, "working set collapsed") != 1 {
		t.Errorf("note duplicated after materialize: %d occurrences", strings.Count(matA, "working set collapsed"))
	}
	// A further projection (records cleared) is identity — no re-truncation.
	proj := app.contextProjection()
	projA := DerefStr(proj[len(proj)-1].Content)
	if projA != matA {
		t.Error("post-materialize projection re-truncated or altered the answer")
	}
}

func tailStr(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
