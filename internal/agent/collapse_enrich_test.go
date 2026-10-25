package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/treeol/wakil/internal/proxy"
)

// --- AnswerExcerpt extraction ---

// The excerpt comes from the validated final assistant message, is fenced-
// block-trimmed, and byte-capped at answerExcerptBytes.
func TestExtractAnswerExcerpt(t *testing.T) {
	cases := []struct {
		name string
		msg  proxy.Message
		want string
	}{
		{"prose", proxy.Message{Role: "assistant", Content: StrPtr("Fixed the bug in foo.go.")}, "Fixed the bug in foo.go."},
		{"fenced", proxy.Message{Role: "assistant", Content: StrPtr("Summary text\n```go\nx := 1\n```")}, "Summary text"},
		{"code-only", proxy.Message{Role: "assistant", Content: StrPtr("```go\nx := 1\n```")}, ""},
		{"empty", proxy.Message{Role: "assistant", Content: StrPtr("")}, ""},
		{"nil content", proxy.Message{Role: "assistant"}, ""},
	}
	for _, tc := range cases {
		if got := extractAnswerExcerpt(tc.msg); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestExtractAnswerExcerptByteCap(t *testing.T) {
	// Multi-byte UTF-8: the cap is BYTES, rune-safe (never splits a rune).
	// Uses 3-byte runes (…) so the boundary can't fall on an even byte.
	text := strings.Repeat("…", 1000) // 3000 bytes
	got := extractAnswerExcerpt(proxy.Message{Role: "assistant", Content: StrPtr(text)})
	if len(got) > answerExcerptBytes {
		t.Errorf("excerpt exceeds byte cap: %d > %d", len(got), answerExcerptBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatal("byte cap split a rune")
	}
	// 400 bytes budget: 398 content + "…" marker = 399 runes max; must not be empty.
	if len(got) == 0 {
		t.Fatal("byte cap produced an empty excerpt")
	}
}

// Fully-suppressed bucket: when writes alone fill the budget, the read line
// reports its overflow instead of vanishing silently.
func TestNoteSuppressedReadBucket(t *testing.T) {
	var w []proxy.Message
	for i := 0; i < 12; i++ {
		w = append(w, tcMsg("w"+string(rune('a'+i)), "write_file",
			`{"path":"w`+string(rune('a'+i))+`.go"}`))
	}
	w = append(w, tcMsg("r1", "read_file", `{"path":"ra.go"}`))
	w = append(w, tcMsg("r2", "read_file", `{"path":"rb.go"}`))
	note := buildCollapseNote(1, w, "/tmp/arch")
	if !strings.Contains(note, "read: (+2 more — see archive)") {
		t.Errorf("suppressed read bucket must report its overflow:\n%s", note)
	}
	if !strings.Contains(note, "wa.go") {
		t.Error("oldest write was trimmed — should keep most recent 12")
	}
}

// fitNotePaths returns overflow counts with no shared state — a concurrent
// smoke check: run builders in parallel goroutines and verify every note's
// overflow counts are internally consistent (its own, not another call's).
func TestBuildCollapseNoteConcurrent(t *testing.T) {
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			nWrite := 3 + g // varies per goroutine
			nRead := 20     // constant overflow of 20-(12-nWrite)
			var w []proxy.Message
			for i := 0; i < nWrite; i++ {
				w = append(w, tcMsg("w"+itoa(i), "edit_file", `{"path":"w`+itoa(i)+`.go"}`))
			}
			for i := 0; i < nRead; i++ {
				w = append(w, tcMsg("r"+itoa(i), "read_file", `{"path":"r`+itoa(i)+`.go"}`))
			}
			note := buildCollapseNote(1, w, "/tmp/arch")
			wantHidden := nRead - (maxNotePaths - nWrite)
			want := fmt.Sprintf("(+%d more)", wantHidden)
			if !strings.Contains(note, want) {
				t.Errorf("g=%d: expected read overflow %q in note:\n%s", g, want, note)
			}
		}(g)
	}
	wg.Wait()
}

// --- collapseFinishedTurns sets AnswerExcerpt from the final answer ---

func TestCollapseSetsAnswerExcerpt(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Conv = []proxy.Message{
		{Role: "user", Content: StrPtr("fix it"), Turn: 1},
		tcMsg("tcA", "read_file", `{"path":"a.go"}`),
		trMsg("tcA", "read_file", bigContent),
		{Role: "assistant", Content: StrPtr("intermediate observation"), Turn: 1},
		// The INTERMEDIATE assistant text must never become the excerpt.
		tcMsg("tcB", "search_files", `{"pattern":"x","path":"."}`),
		trMsg("tcB", "search_files", bigContent),
		{Role: "assistant", Content: StrPtr("FINAL: fixed a.go by clamping the index."), Turn: 1},
	}
	app.turnSeq = 1

	app.collapseFinishedTurns()

	app.stateMu.RLock()
	rec, ok := app.collapsed[1]
	app.stateMu.RUnlock()
	if !ok {
		t.Fatal("turn 1 not collapsed")
	}
	if !strings.Contains(rec.AnswerExcerpt, "FINAL: fixed a.go") {
		t.Errorf("AnswerExcerpt should come from the final answer, got %q", rec.AnswerExcerpt)
	}
	if strings.Contains(rec.AnswerExcerpt, "intermediate") {
		t.Error("AnswerExcerpt must not capture intermediate observations")
	}
}

// Degraded pointers carry the answer excerpt — the tier-2 recall gap fix.
func TestDegradePointerCarriesAnswerExcerpt(t *testing.T) {
	rec := CollapseRecord{
		Turn:          7,
		Archive:       "/tmp/archive.txt",
		AnswerExcerpt: "Decided on Option B for the scheduler.",
	}
	s := degradePointerLine(rec, []proxy.Message{{Role: "user", Content: StrPtr("decide the scheduler")}})
	if !strings.Contains(s, "answer: Decided on Option B") {
		t.Errorf("degraded pointer should carry the answer excerpt, got:\n%s", s)
	}
	if !strings.Contains(s, "shell cannot see it") {
		t.Errorf("degraded pointer should carry the host-side read_file hint, got:\n%s", s)
	}
	// Auto-collapsed turns have no Summary — the excerpt is the only conclusion text.
	rec.Summary = ""
	s = degradePointerLine(rec, nil)
	if !strings.Contains(s, "Decided on Option B") {
		t.Error("pointer without Summary must still expose the answer excerpt")
	}
}

// Tail projection must NOT duplicate the answer into the note when A itself
// is present (the answer excerpt stays pointer/index-only by design).
func TestTailNoteDoesNotDuplicateAnswer(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Conv = oneCollapsedTurn(1)
	for i := range app.Conv {
		app.Conv[i].Turn = 1
	}
	app.turnSeq = 1

	app.collapseFinishedTurns()
	proj := app.contextProjection()
	if len(proj) != 2 {
		t.Fatalf("projection: want 2 msgs, got %d", len(proj))
	}
	aText := DerefStr(proj[1].Content)
	// Answer appears once (as A content), not again as an inlined excerpt line.
	if got := strings.Count(aText, "Here is the answer."); got != 1 {
		t.Errorf("answer should appear exactly once in the tail turn, got %d:\n%s", got, aText)
	}
}

// --- note paths: write targets vs read, shared budget ---

func TestNoteSplitsWriteTargetsAndReads(t *testing.T) {
	w := []proxy.Message{
		tcMsg("t1", "read_file", `{"path":"src/a.go"}`),
		tcMsg("t2", "edit_file", `{"path":"src/b.go"}`),
		tcMsg("t3", "write_file", `{"path":"out/c.txt"}`),
		tcMsg("t4", "move_file", `{"src":"old.md","dst":"new.md"}`),
		tcMsg("t5", "delete_file", `{"path":"tmp/x"}`),
	}
	note := buildCollapseNote(1, w, "/tmp/arch")
	if !strings.Contains(note, "write targets: src/b.go, out/c.txt, old.md, new.md, tmp/x") {
		t.Errorf("write targets line wrong:\n%s", note)
	}
	if strings.Contains(note, "src/b.go") && strings.Count(note, "src/b.go") != 1 {
		t.Error("write target duplicated across buckets")
	}
	// src and dst of move_file both appear (dst as write target, src too —
	// move_file is a write-class tool).
	if !strings.Contains(note, "old.md") || !strings.Contains(note, "new.md") {
		t.Error("move_file src/dst missing from write targets")
	}
	if !strings.Contains(note, "read: src/a.go") {
		t.Errorf("read line wrong:\n%s", note)
	}
}

func TestNoteSharedPathBudget(t *testing.T) {
	// 15 reads + 2 writes: shared budget 12 keeps BOTH writes (they fit
	// within the budget) and the 10 most recent reads, with overflow counts.
	var w []proxy.Message
	for i := 0; i < 15; i++ {
		w = append(w, tcMsg("r"+string(rune('a'+i)), "read_file",
			`{"path":"`+strings.Repeat("p", 1)+string(rune('a'+i))+`.go"}`))
	}
	w = append(w, tcMsg("w1", "edit_file", `{"path":"edit1.go"}`))
	w = append(w, tcMsg("w2", "write_file", `{"path":"wrote2.go"}`))
	note := buildCollapseNote(1, w, "/tmp/arch")
	if !strings.Contains(note, "edit1.go") || !strings.Contains(note, "wrote2.go") {
		t.Error("write targets crowded out of the shared budget")
	}
	if !strings.Contains(note, "(+5 more)") {
		t.Errorf("read overflow count missing:\n%s", note)
	}
}

func TestNoteCapKeepsArchivePath(t *testing.T) {
	// A very long archive path plus huge W overflows the cap — the archive
	// line must survive (recovery-reference exception).
	w := []proxy.Message{tcMsg("t1", "read_file", `{"path":"x"}`)}
	long := strings.Repeat("/verylongdir", 100) + "/arch.txt"
	note := buildCollapseNote(1, w, long)
	if !strings.Contains(note, long) {
		t.Error("archive path must never be truncated out of the note")
	}
}

// JSON persistence round trip: AnswerExcerpt survives save/load; a legacy
// record without the field loads to empty (forward compatibility), and
// unknown fields never break decoding.
func TestCollapseRecordPersistenceRoundTrip(t *testing.T) {
	rec := CollapseRecord{
		Turn:          3,
		Archive:       "/tmp/a.txt",
		Note:          "note",
		AnswerExcerpt: "Entschieden: Option B — UTF-8 süß ✔",
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back CollapseRecord
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.AnswerExcerpt != rec.AnswerExcerpt {
		t.Errorf("round trip lost AnswerExcerpt: got %q", back.AnswerExcerpt)
	}
	// Legacy record (no answer_excerpt field) → empty, no error.
	var legacy CollapseRecord
	if err := json.Unmarshal([]byte(`{"turn":1,"archive":"/tmp/b.txt"}`), &legacy); err != nil {
		t.Fatalf("legacy unmarshal: %v", err)
	}
	if legacy.AnswerExcerpt != "" || legacy.Turn != 1 {
		t.Errorf("legacy record mis-loaded: %+v", legacy)
	}
}

func TestIndexValueAnswerFallback(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	// Conv lacks the turn (materialized/rewound) — the record's excerpt
	// substitutes instead of losing the answer line entirely.
	rec := CollapseRecord{AnswerExcerpt: "the fallback conclusion"}
	v := app.indexValueForTurn(99, rec)
	if !strings.Contains(v, "answer: the fallback conclusion") {
		t.Errorf("index value should fall back to rec.AnswerExcerpt, got:\n%s", v)
	}
}
