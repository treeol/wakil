package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/treeol/wakil/internal/proxy"
)

// Fix 1: in immediate mode a context_mark drop/stub mark takes effect only
// after a round of real tool calls has been recorded after the mark.
// Fix 2: thin stub notes for large rounds are rejected.

// isPruned reports whether round id is recorded as pruned.
func isPruned(app *App, id int) bool {
	app.stateMu.RLock()
	defer app.stateMu.RUnlock()
	return app.pruneRounds[id].Pruned || app.prunedRounds[id]
}

// loneMark appends an assistant message made only of one context_mark call,
// runs the handler and appends its result, like the live tool loop.
func loneMark(app *App, turn int, callID, args string) string {
	app.curTurn = turn
	call := proxy.ToolCall{ID: callID, Type: "function", Function: proxy.FunctionCall{Name: "context_mark", Arguments: args}}
	app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{call}})
	res := app.handleContextMark(context.Background(), call)
	app.appendConv(trMsg(callID, "context_mark", res))
	return res
}

// longNote returns a stub note of at least n characters built from facts.
func longNote(facts string, n int) string {
	var b strings.Builder
	b.WriteString(facts)
	for i := 0; b.Len() < n; i++ {
		b.WriteString("; fact " + itoa(i) + " at line " + itoa(10+i))
	}
	return b.String()
}

func markArgs(t *testing.T, drop []string, stub map[string]string) string {
	t.Helper()
	// Large drops now need drop_reason; these helpers must supply one.
	args := map[string]any{"drop": drop, "stub": stub}
	if len(drop) > 0 {
		reasons := map[string]string{}
		for _, id := range drop {
			reasons[id] = "searched and nothing needed"
		}
		args["drop_reason"] = reasons
	}
	b, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// (a) [context_mark] alone, then the final answer: nothing is pruned before
// the final answer is generated, and not at the request-boundary check either.
func TestMarkLoneThenFinalAnswerPrunesNothing(t *testing.T) {
	app := immediateApp(t)
	a, b := strings.Repeat("a", 6000), strings.Repeat("b", 6000)
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, a)
	appendRound(app, 1, "c2", "read_file", `{"path":"b.go"}`, b)
	before := app.requestMessages()
	res := loneMark(app, 1, "m1", markArgs(t, []string{"r1"}, map[string]string{"r2": longNote("b.go:10 handler", 220)}))
	if !strings.HasPrefix(res, "marked: drop r1 · stub r2") || !strings.HasSuffix(res, " · takes effect after three further tool steps") {
		t.Errorf("lone mark result = %q", res)
	}
	// The request that generates the final answer: pruneCheck, then build.
	app.pruneCheck()
	msgs := app.requestMessages()
	if got := toolResultOf(msgs, "c1"); !strings.HasPrefix(got, "[r1] "+a) {
		t.Errorf("r1 not sent in full for the final answer: %.60q", got)
	}
	if got := toolResultOf(msgs, "c2"); !strings.HasPrefix(got, "[r2] "+b) {
		t.Errorf("r2 not sent in full for the final answer: %.60q", got)
	}
	// A pending mark changes no bytes of the marked results.
	for _, id := range []string{"c1", "c2"} {
		if toolResultOf(msgs, id) != toolResultOf(before, id) {
			t.Errorf("%s bytes changed while the mark is pending", id)
		}
	}
	// Final answer, then the request-boundary check (finalizeTurn).
	app.appendConv(proxy.Message{Role: "assistant", Content: StrPtr("final answer")})
	app.pruneCheck()
	if isPruned(app, 1) || isPruned(app, 2) {
		t.Error("pending marks were applied at the end of the request")
	}
	// The marks are still recorded.
	if ratingOf(app, 1) != RatingDrop || ratingOf(app, 2) != RatingStub {
		t.Errorf("marks lost: r1=%q r2=%q", ratingOf(app, 1), ratingOf(app, 2))
	}
}

// (b) [context_mark] alone, then real rounds: the marks take effect
// after three further real rounds, not after one or two.
func TestMarkLoneThenRealRoundPrunes(t *testing.T) {
	app := immediateApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, strings.Repeat("a", 6000))
	appendRound(app, 1, "c2", "read_file", `{"path":"b.go"}`, strings.Repeat("b", 6000))
	note := longNote("b.go:10 handler", 220)
	loneMark(app, 1, "m1", markArgs(t, []string{"r1"}, map[string]string{"r2": note}))
	app.pruneCheck()
	if isPruned(app, 1) || isPruned(app, 2) {
		t.Fatal("pruned before the next real tool call")
	}
	// One and two real rounds are not enough.
	appendRound(app, 1, "c3", "read_file", `{"path":"c.go"}`, "c")
	app.pruneCheck()
	if isPruned(app, 1) || isPruned(app, 2) {
		t.Fatal("pruned after only one further real round")
	}
	appendRound(app, 1, "c4", "read_file", `{"path":"d.go"}`, "d")
	app.pruneCheck()
	if isPruned(app, 1) || isPruned(app, 2) {
		t.Fatal("pruned after only two further real rounds")
	}
	// The third further real round releases the marks.
	appendRound(app, 1, "c5", "read_file", `{"path":"e.go"}`, "e")
	app.pruneCheck()
	msgs := app.requestMessages()
	if got := toolResultOf(msgs, "c1"); !strings.Contains(got, "[r1 pruned (drop") {
		t.Errorf("r1 not dropped after the third further real round: %.80q", got)
	}
	if got := toolResultOf(msgs, "c2"); !strings.Contains(got, "[r2 pruned: "+note+" — full result:") {
		t.Errorf("r2 not stubbed after the third further real round: %.80q", got)
	}
	assertPairing(t, msgs)
}

// (c) [context_mark, read_file] in one message: the read_file round counts
// as the first of three further real rounds, so the marks take effect after
// two more real rounds.
func TestMarkMixedMessagePrunesNextBuild(t *testing.T) {
	app := immediateApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, strings.Repeat("a", 6000))
	call := proxy.ToolCall{ID: "mx", Type: "function", Function: proxy.FunctionCall{Name: "context_mark", Arguments: `{"drop":["r1"],"drop_reason":{"r1":"searched and nothing needed"}}`}}
	app.appendConv(proxy.Message{Role: "assistant", ToolCalls: []proxy.ToolCall{
		call,
		{ID: "rf", Type: "function", Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"b.go"}`}},
	}})
	res := app.handleContextMark(context.Background(), call)
	if res != "marked: drop r1" {
		t.Errorf("mixed mark result = %q, want the plain text", res)
	}
	app.appendConv(trMsg("mx", "context_mark", res))
	app.appendConv(trMsg("rf", "read_file", strings.Repeat("b", 6000)))
	// The sibling round is the first of three further real rounds: not enough.
	app.pruneCheck()
	if isPruned(app, 1) {
		t.Fatal("pruned after the mixed message alone")
	}
	appendRound(app, 1, "c3", "read_file", `{"path":"c.go"}`, "c")
	app.pruneCheck()
	if isPruned(app, 1) {
		t.Fatal("pruned after only two further real rounds")
	}
	appendRound(app, 1, "c4", "read_file", `{"path":"d.go"}`, "d")
	app.pruneCheck()
	msgs := app.requestMessages()
	if got := toolResultOf(msgs, "c1"); !strings.Contains(got, "[r1 pruned (drop") {
		t.Errorf("r1 not pruned after the mixed message plus two more real rounds: %.80q", got)
	}
	if got := toolResultOf(msgs, "rf"); !strings.HasPrefix(got, "[r2] ") {
		t.Errorf("sibling round touched: %.40q", got)
	}
}

// (d) Marks still pending when the request ends stay recorded and apply under
// the same rule in the next request, also across a session save and resume.
func TestMarkPendingSurvivesRequestEndAndResume(t *testing.T) {
	app := immediateApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, strings.Repeat("a", 6000))
	loneMark(app, 1, "m1", `{"drop":["r1"],"drop_reason":{"r1":"searched and nothing needed"}}`)
	app.appendConv(proxy.Message{Role: "assistant", Content: StrPtr("final answer")})
	app.pruneCheck() // request boundary

	// Save and resume: the pending state travels with the round record.
	raw, err := json.Marshal(app.pruneRecordsSlice())
	if err != nil {
		t.Fatal(err)
	}
	var recs []RoundRecord
	if err := json.Unmarshal(raw, &recs); err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].MarkSeq != 1 || recs[0].Rating != RatingDrop || recs[0].Pruned {
		t.Fatalf("persisted record = %+v", recs)
	}
	app2 := immediateApp(t)
	app2.Conv = append([]proxy.Message(nil), app.Conv...)
	app2.RestoreTurnState(&Session{Conv: app2.Conv, PruneRecords: recs})

	// Next request: a new user prompt alone releases nothing.
	app2.curTurn = 2
	app2.appendConv(userMsgAt(2, "next task"))
	app2.pruneCheck()
	if isPruned(app2, 1) {
		t.Fatal("pending mark applied by a new request without a real tool call")
	}
	if got := toolResultOf(app2.requestMessages(), "c1"); !strings.HasPrefix(got, "[r1] aaaa") {
		t.Fatalf("r1 not in full at the start of the next request: %.40q", got)
	}
	// The third further real tool call of the next request releases it.
	appendRound(app2, 2, "c2", "read_file", `{"path":"b.go"}`, "b")
	appendRound(app2, 2, "c3", "read_file", `{"path":"c.go"}`, "c")
	app2.pruneCheck()
	if isPruned(app2, 1) {
		t.Fatal("pending mark applied after only two further real tool calls")
	}
	appendRound(app2, 2, "c4", "read_file", `{"path":"d.go"}`, "d")
	app2.pruneCheck()
	if got := toolResultOf(app2.requestMessages(), "c1"); !strings.Contains(got, "[r1 pruned (drop") {
		t.Errorf("pending mark not applied after three further real tool calls: %.80q", got)
	}
}

// (d, collapse) A pending mark on a round that collapse has replaced is left
// to collapse: nothing is pruned there.
func TestMarkPendingLeftToCollapse(t *testing.T) {
	app := immediateApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, strings.Repeat("a", 6000))
	loneMark(app, 1, "m1", `{"drop":["r1"],"drop_reason":{"r1":"searched and nothing needed"}}`)
	app.stateMu.Lock()
	app.collapsed = map[int]CollapseRecord{1: {Turn: 1}}
	app.stateMu.Unlock()
	appendRound(app, 2, "c2", "read_file", `{"path":"b.go"}`, "b")
	app.pruneCheck()
	if isPruned(app, 1) {
		t.Error("round of a collapsed turn was pruned")
	}
}

// The trigger does not apply a pending mark either (immediate mode), and
// on-trigger mode keeps its behaviour: marks apply at the trigger.
func TestMarkPendingHeldAtTriggerImmediateOnly(t *testing.T) {
	for _, mode := range []string{"immediate", "on-trigger"} {
		app := immediateApp(t)
		app.Cfg.ContextPruneMarks = mode
		big := strings.Repeat("t", 9000)
		for i := 1; i <= 8; i++ { // 72 KB: above the trigger (0.70 × 100000), below the emergency level
			appendRound(app, 1, "t"+itoa(i), "read_file", `{"path":"t`+itoa(i)+`.go"}`, big)
		}
		res := loneMark(app, 1, "m1", `{"drop":["r1","r2","r3","r4"],"drop_reason":{"r1":"searched and nothing needed","r2":"searched and nothing needed","r3":"searched and nothing needed","r4":"searched and nothing needed"}}`)
		app.pruneCheck()
		switch mode {
		case "immediate":
			if !strings.Contains(res, "takes effect after three further tool steps") {
				t.Errorf("immediate: result = %q", res)
			}
			for i := 1; i <= 8; i++ {
				if isPruned(app, i) {
					t.Errorf("immediate: r%d pruned at the trigger while marks are pending", i)
				}
			}
			// r1 was recorded first, so it never carried a stored line; while
			// its mark is pending its bytes are the full original.
			if toolResultOf(app.requestMessages(), "t1") != "[r1] "+big {
				t.Errorf("immediate: pending round is not sent in full: %.80q", toolResultOf(app.requestMessages(), "t1"))
			}
		case "on-trigger":
			if res != "marked: drop r1,r2,r3,r4" {
				t.Errorf("on-trigger: result = %q, want the plain text", res)
			}
			if !isPruned(app, 1) {
				t.Error("on-trigger: marked round not pruned at the trigger")
			}
		}
	}
}

// At the emergency level (≥ 90% of hard max) Wakil would stub unmarked rounds
// without a note. The rounds the model marked go first, pending or not, so no
// unmarked round is pruned while a marked one is held.
func TestMarkPendingReleasedBeforeEmergency(t *testing.T) {
	app := immediateApp(t)
	big := strings.Repeat("t", 9000)
	for i := 1; i <= 10; i++ { // 90 KB + overhead: at the emergency level
		appendRound(app, 1, "t"+itoa(i), "read_file", `{"path":"t`+itoa(i)+`.go"}`, big)
	}
	loneMark(app, 1, "m1", `{"drop":["r1","r2","r3","r4","r5"],"drop_reason":{"r1":"searched and nothing needed","r2":"searched and nothing needed","r3":"searched and nothing needed","r4":"searched and nothing needed","r5":"searched and nothing needed"}}`)
	app.pruneCheck()
	for i := 1; i <= 5; i++ {
		if !isPruned(app, i) || ratingOf(app, i) != RatingDrop {
			t.Errorf("r%d: pruned=%v rating=%q, want the model's drop applied", i, isPruned(app, i), ratingOf(app, i))
		}
	}
	for i := 6; i <= 10; i++ {
		if isPruned(app, i) || ratingOf(app, i) != RatingNone {
			t.Errorf("unmarked r%d: pruned=%v rating=%q, want untouched", i, isPruned(app, i), ratingOf(app, i))
		}
	}
}

// A round that was already rated (so it never carried a tool-result line) and
// is then marked with a lone context_mark keeps its bytes while the mark is
// pending: no line appears and vanishes again.
func TestMarkPendingKeepsBytesOfRatedRound(t *testing.T) {
	app := immediateApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, strings.Repeat("a", 6000))
	for i := 0; i < 3; i++ {
		appendRound(app, 1, "c"+string(rune('x'+i)), "read_file", `{"path":"x.go"}`, strings.Repeat("x", 6000))
	}
	appendRound(app, 1, "c2", "read_file", `{"path":"b.go"}`, strings.Repeat("b", 6000))
	loneMark(app, 1, "m0", `{"keep":["r1"]}`)
	before := toolResultOf(app.requestMessages(), "c1")
	if strings.Contains(before, "[context · unmarked") || strings.Contains(before, "[reread") {
		t.Fatal("precondition: a kept round carries no line")
	}
	loneMark(app, 1, "m1", `{"drop":["r1"],"drop_reason":{"r1":"searched and nothing needed"}}`)
	if got := toolResultOf(app.requestMessages(), "c1"); got != before {
		t.Errorf("pending mark changed the bytes of a rated round: tail %q", got[len(got)-60:])
	}
	// Re-marking while pending keeps the pre-mark state.
	loneMark(app, 1, "m2", markArgs(t, nil, map[string]string{"r1": longNote("a.go:3", 220)}))
	if got := toolResultOf(app.requestMessages(), "c1"); got != before {
		t.Error("second pending mark changed the bytes of a rated round")
	}
}

// A keep mark is never pending and carries no "takes effect" note.
func TestMarkKeepHasNoEffectNote(t *testing.T) {
	app := immediateApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, strings.Repeat("a", 6000))
	if res := loneMark(app, 1, "m1", `{"keep":["r1"]}`); res != "marked: keep r1" {
		t.Errorf("keep result = %q", res)
	}
}

/* Fix 2: thin stub notes. */

// multiFileRound appends one round reading four files, 38 KB in total.
func multiFileRound(app *App) {
	app.curTurn = 1
	files := []string{"internal/web/invite.go", "internal/web/export.go", "internal/web/authz.go", "internal/web/web.go"}
	var calls []proxy.ToolCall
	for i, f := range files {
		calls = append(calls, proxy.ToolCall{ID: "mf" + itoa(i), Type: "function", Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"` + f + `"}`}})
	}
	app.appendConv(proxy.Message{Role: "assistant", ToolCalls: calls})
	for i := range files {
		app.appendConv(trMsg("mf"+itoa(i), "read_file", strings.Repeat("s", 9728)))
	}
}

func TestStubNoteMinLen(t *testing.T) {
	for _, c := range []struct{ bytes, want int }{
		{4096, 200}, {12800, 200}, {38912, 608}, {76800, 1200}, {1 << 20, 1200},
	} {
		if got := markNoteMinLen(c.bytes); got != c.want {
			t.Errorf("markNoteMinLen(%d) = %d, want %d", c.bytes, got, c.want)
		}
	}
}

func TestStubNoteAccepted(t *testing.T) {
	app := immediateApp(t)
	multiFileRound(app)
	note := longNote("invite.go:40 token check; export.go:88 CSV writer; authz.go:17 four checks, MaxStateAge 30s is a constant at authz.go:9; web.go:120 routes", 608)
	res := loneMark(app, 1, "m1", markArgs(t, nil, map[string]string{"r1": note}))
	if res != "marked: stub r1 · takes effect after three further tool steps" {
		t.Errorf("result = %q", res)
	}
	app.stateMu.RLock()
	rec := app.pruneRounds[1]
	app.stateMu.RUnlock()
	if rec.Rating != RatingStub || rec.StubText != note {
		t.Errorf("accepted note not stored whole: rating %q, %d of %d chars", rec.Rating, len(rec.StubText), len(note))
	}
}

func TestStubNoteTooShortRejected(t *testing.T) {
	app := immediateApp(t)
	multiFileRound(app)
	before := toolResultOf(app.requestMessages(), "mf3")
	res := loneMark(app, 1, "m1", markArgs(t, nil, map[string]string{"r1": "authz 4 checks + MaxStateAge 30s"}))
	want := "r1 not stubbed: note too thin for 38 KB across 4 files (32 characters, at least 608 needed). A stub replaces the result; afterwards you only have your note. Write the facts you will still use with file:line, per file (missing: invite.go, export.go, authz.go, web.go), or drop it."
	if res != want {
		t.Errorf("result:\n got %q\nwant %q", res, want)
	}
	// The round stays unmarked and keeps its bytes, also after a real round.
	if ratingOf(app, 1) != RatingNone {
		t.Errorf("rejected round rated %q", ratingOf(app, 1))
	}
	appendRound(app, 1, "c2", "read_file", `{"path":"z.go"}`, "z")
	app.pruneCheck()
	if isPruned(app, 1) {
		t.Error("rejected round was pruned")
	}
	if toolResultOf(app.requestMessages(), "mf3") != before {
		t.Error("rejected round changed bytes")
	}
}

func TestStubNoteMissingFileRejected(t *testing.T) {
	app := immediateApp(t)
	multiFileRound(app)
	note := longNote("invite.go:40 token check; export.go:88 CSV writer; authz has four checks", 608)
	res := loneMark(app, 1, "m1", markArgs(t, nil, map[string]string{"r1": note}))
	want := "r1 not stubbed: note too thin for 38 KB across 4 files. A stub replaces the result; afterwards you only have your note. Write the facts you will still use with file:line, per file (missing: authz.go, web.go), or drop it."
	if res != want {
		t.Errorf("result:\n got %q\nwant %q", res, want)
	}
	if ratingOf(app, 1) != RatingNone {
		t.Errorf("rejected round rated %q", ratingOf(app, 1))
	}
}

func TestStubNoteMixedCallOneRejectedOneAccepted(t *testing.T) {
	app := immediateApp(t)
	multiFileRound(app)                                                                  // r1, 38 KB, 4 files
	appendRound(app, 1, "c2", "read_file", `{"path":"b.go"}`, strings.Repeat("b", 6000)) // r2
	appendRound(app, 1, "c3", "read_file", `{"path":"c.go"}`, strings.Repeat("c", 6000)) // r3
	good := longNote("b.go:10 handler", 220)
	res := loneMark(app, 1, "m1", markArgs(t, []string{"r3"}, map[string]string{"r1": "one line", "r2": good}))
	lines := strings.Split(res, "\n")
	if len(lines) != 2 || lines[0] != "marked: drop r3 · stub r2 · takes effect after three further tool steps" {
		t.Fatalf("result = %q", res)
	}
	if !strings.HasPrefix(lines[1], "r1 not stubbed: note too thin for 38 KB across 4 files (8 characters, at least 608 needed).") {
		t.Errorf("rejection line = %q", lines[1])
	}
	if ratingOf(app, 1) != RatingNone || ratingOf(app, 2) != RatingStub || ratingOf(app, 3) != RatingDrop {
		t.Errorf("ratings r1=%q r2=%q r3=%q", ratingOf(app, 1), ratingOf(app, 2), ratingOf(app, 3))
	}
	appendRound(app, 1, "c4", "read_file", `{"path":"d.go"}`, "d")
	appendRound(app, 1, "c5", "read_file", `{"path":"e.go"}`, "e")
	appendRound(app, 1, "c6", "read_file", `{"path":"f.go"}`, "f")
	app.pruneCheck()
	if isPruned(app, 1) || !isPruned(app, 2) || !isPruned(app, 3) {
		t.Errorf("pruned r1=%v r2=%v r3=%v, want false true true", isPruned(app, 1), isPruned(app, 2), isPruned(app, 3))
	}
}

func TestStubNoteSmallRoundAndDropUnaffected(t *testing.T) {
	app := immediateApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, strings.Repeat("a", 4000)) // r1 < 4 KB
	multiFileRound(app)                                                                  // r2
	res := loneMark(app, 1, "m1", markArgs(t, []string{"r2"}, map[string]string{"r1": "short"}))
	if res != "marked: drop r2 · stub r1 · takes effect after three further tool steps" {
		t.Errorf("result = %q", res)
	}
	if ratingOf(app, 1) != RatingStub || ratingOf(app, 2) != RatingDrop {
		t.Errorf("ratings r1=%q r2=%q", ratingOf(app, 1), ratingOf(app, 2))
	}
}

// The never-inflate rule still applies after the note check: an accepted
// note that is not shorter than the result does not replace it.
func TestStubNoteNeverInflateStillApplies(t *testing.T) {
	app := immediateApp(t)
	appendRound(app, 1, "c1", "read_file", `{"path":"a.go"}`, strings.Repeat("a", 300)) // r1, unchecked size
	loneMark(app, 1, "m1", markArgs(t, nil, map[string]string{"r1": longNote("a.go:1", 400)}))
	appendRound(app, 1, "c2", "read_file", `{"path":"b.go"}`, "b")
	app.pruneCheck()
	if isPruned(app, 1) {
		t.Error("stub longer than the result replaced it")
	}
}

// A long context_mark note on a round with several results is written into
// each of them, so never-inflate counts it once per result.
func TestStubNoteNeverInflateCountsEveryResult(t *testing.T) {
	app := immediateApp(t)
	app.curTurn = 1
	var calls []proxy.ToolCall
	for i := 0; i < 5; i++ {
		calls = append(calls, proxy.ToolCall{ID: "p" + itoa(i), Type: "function", Function: proxy.FunctionCall{Name: "read_file", Arguments: `{"path":"f` + itoa(i) + `.go"}`}})
	}
	app.appendConv(proxy.Message{Role: "assistant", ToolCalls: calls})
	for i := 0; i < 5; i++ {
		app.appendConv(trMsg("p"+itoa(i), "read_file", strings.Repeat("s", 1024)))
	}
	sizeBefore := TranscriptSize(app.requestMessages())
	note := longNote("f0.go:1 f1.go:2 f2.go:3 f3.go:4 f4.go:5", 1200) // 5 × (1200 + stub frame) > 5 KB
	res := loneMark(app, 1, "m1", markArgs(t, nil, map[string]string{"r1": note}))
	if !strings.HasPrefix(res, "marked: stub r1") {
		t.Fatalf("note not accepted: %q", res)
	}
	appendRound(app, 1, "c2", "read_file", `{"path":"z.go"}`, "z")
	app.pruneCheck()
	if isPruned(app, 1) {
		t.Errorf("stub replaced the round although it grows the request (before %d, after %d)", sizeBefore, TranscriptSize(app.requestMessages()))
	}
}

// The note is checked as it will be stored: a file name beyond the stored
// length does not count.
func TestStubNoteCheckedAfterTruncation(t *testing.T) {
	app := immediateApp(t)
	multiFileRound(app)
	note := longNote("invite.go:40; export.go:88; authz.go:17", markNoteMaxChars+50) + " web.go:120"
	res := loneMark(app, 1, "m1", markArgs(t, nil, map[string]string{"r1": note}))
	if !strings.HasPrefix(res, "r1 not stubbed:") || !strings.Contains(res, "(missing: web.go)") {
		t.Errorf("result = %q", res[:min(len(res), 200)])
	}
}

// A write round cannot be dropped, so its rejection does not offer drop.
func TestStubNoteWriteRoundRejectionOmitsDrop(t *testing.T) {
	app := immediateApp(t)
	appendRound(app, 1, "w1", "edit_file", `{"path":"a.go"}`, strings.Repeat("w", 6000))
	res := loneMark(app, 1, "m1", markArgs(t, nil, map[string]string{"r1": "edited"}))
	want := "r1 not stubbed: note too thin for 5 KB (6 characters, at least 200 needed). A stub replaces the result; afterwards you only have your note. Write the facts you will still use with file:line."
	if res != want {
		t.Errorf("result:\n got %q\nwant %q", res, want)
	}
}

// The three texts use the same terms and do not contradict each other.
// The cautions live only in the tool description; the system section and
// the user suffix stay two to three sentences of instruction each.
func TestPruneTextsCoherent(t *testing.T) {
	app := immediateApp(t)
	sec := app.pruneSystemSection()
	desc := contextMarkTool(true).Function.Description
	for name, text := range map[string]string{"system section": sec, "user suffix": pruneReminder} {
		for _, s := range []string{"[rN]", "re-sent in full on every step", "context_mark", "file:line"} {
			if !strings.Contains(text, s) {
				t.Errorf("%s lacks %q", name, s)
			}
		}
		if strings.Contains(text, "not a step to perform after every result") {
			t.Errorf("%s still carries the discouraging sentence", name)
		}
		if strings.Contains(text, "on the next step") || strings.Contains(text, "one-line") || strings.Contains(text, "right after") || strings.Contains(text, "as you go") {
			t.Errorf("%s still promises next-step effect, a one-line note, or marker-by-marker marking", name)
		}
	}
	for _, s := range []string{"drop", "stub", "keep", "only the note remains", "every fact you will still use", "per file", "file:line", "a note too thin for a large result is rejected", "costs more than keeping it", "Marking right before the final answer saves nothing"} {
		if !strings.Contains(desc, s) {
			t.Errorf("tool description lacks %q", s)
		}
	}
	if strings.Contains(desc, "not a step to perform after every result") {
		t.Error("tool description still carries the discouraging sentence")
	}
	// The sentence about when a mark takes effect follows the mode, in the
	// system section and in the tool description alike.
	for name, text := range map[string]string{"system section": sec, "tool description": desc} {
		if !strings.Contains(text, "A mark takes effect after three further tool steps") {
			t.Errorf("immediate %s: %s", name, text)
		}
	}
	app.Cfg.ContextPruneMarks = "on-trigger"
	for name, text := range map[string]string{"system section": app.pruneSystemSection(), "tool description": contextMarkTool(false).Function.Description} {
		if !strings.Contains(text, "A mark takes effect when the context fills up") || strings.Contains(text, "after three further tool steps") {
			t.Errorf("on-trigger %s: %s", name, text)
		}
	}
	// The section points large results at context_mark rather than offering
	// the one-line prev stub for them.
	if !strings.Contains(sec, "for large results use context_mark") {
		t.Errorf("section does not point large results at context_mark: %s", sec)
	}
	// The tool-result line is stored on the round record, fixed at recording.
	// The backlog line names its stale rounds in the same form the docs
	// quote: "r4 (~10k tokens)".
	for i := 1; i <= 3; i++ {
		appendRound(app, 1, "f"+itoa(i), "read_file", `{"path":"f.go"}`, strings.Repeat("f", 40000-i*3000))
	}
	appendRound(app, 1, "g1", "read_file", `{"path":"a.go"}`, strings.Repeat("g", 4000))
	appendRound(app, 1, "h1", "read_file", `{"path":"h.go"}`, strings.Repeat("h", 4000))
	appendRound(app, 1, "h2", "read_file", `{"path":"h.go"}`, strings.Repeat("h", 4000))
	// r6 is the newest; 3 real rounds older are r1..r3, largest first.
	got := toolResultOf(app.requestMessages(), "h2")
	if !strings.Contains(got, "r1 (~9k tokens)") || !strings.Contains(got, "[context · unmarked for 3+ steps:") {
		t.Errorf("backlog line: %q", got[:min(200, len(got))])
	}
}
