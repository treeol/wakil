package agent

import (
	"strings"
	"testing"
)

// --- B1b: tail-answer cap ---------------------------------------------------
//
// The 20-turn tail keeps FULL answers (U + A + note) — in long sessions A
// is often multi-KB, so the tail dominates the context floor. The
// context_tail_answer_bytes cap truncates retained answers (rune-safe,
// archive pointer appended), dropping the floor to ~20 × (U + 0.5k).
// U, the note, and degraded pointer turns are never capped. 0 disables.

// TestTailAnswerCapTruncates: a tail turn with a multi-KB answer projects
// A capped at the configured bytes, with the archive pointer appended.
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
		1: {Turn: 1, Archive: "/tmp/fake-archive-turn-1", Note: "[request 1 working set collapsed]\n"},
	}
	app.stateMu.Unlock()

	proj := app.contextProjection()
	if len(proj) != 2 {
		t.Fatalf("want U+A (2 msgs), got %d", len(proj))
	}
	got := *proj[1].Content
	if !strings.Contains(got, "[full answer archived: /tmp/fake-archive-turn-1 (read_file with offset/limit)]") {
		t.Errorf("capped answer missing archive pointer line:\n…%s", tailStr(got, 200))
	}
	// Cap: capped A (~300) + pointer line (~90) + note (~38). The original
	// answer was 4500 bytes — the projection must be an order smaller.
	if len(got) > 450 {
		t.Errorf("capped A + pointer + note still large: %d bytes", len(got))
	}
	if len(got) >= len(longAnswer) {
		t.Errorf("answer was not capped: %d bytes", len(got))
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
}

// TestTailAnswerCapUTF8Safe: truncation lands mid-CJK without splitting a
// rune.
func TestTailAnswerCapUTF8Safe(t *testing.T) {
	app := newTestApp("http://unused", newFakeExecutor(), func(_, _, _ string, _ bool) bool { return true })
	app.SetContextMode("long-session")
	app.Cfg.ContextTailAnswerBytes = 120

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
	if !utf8ValidStr(got) {
		t.Error("capped answer split a UTF-8 rune")
	}
}

func tailStr(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

func utf8ValidStr(s string) bool {
	return len(strings.ToValidUTF8(s, "")) == len(s)
}
